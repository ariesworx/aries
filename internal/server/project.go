package server

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// served is one project the daemon serves and what its requests need:
// its store, its default account and its log. A server of one project
// (Config.Store) has one for its whole life; a server of many
// (Config.Control) has one for each store its registry holds open.
type served struct {
	// id is the project's UUID, and schema the schema that holds it,
	// which a server of one project is not told.
	id, schema string
	store      Store
	account    string
	// log is the daemon's logger, naming the project when the server
	// holds many.
	log *slog.Logger
	// timeout bounds each reap, prune and presence update
	// (Config.RequestTimeout).
	timeout time.Duration
	// used is when the project last answered a request, or was opened if
	// it has answered none, in Unix nanoseconds on the server's clock:
	// what `aries admin status` shows as last used.
	used atomic.Int64
	// tel counts the refusals at the store's caps (observeLimit); nil
	// counts none.
	tel *telemetry.Telemetry
	// conflictAge is how long a conflict is open before digest lists it
	// (Config.ConflictAge).
	conflictAge time.Duration
	// dueSoon is how far ahead a due date makes an issue due soon
	// (Config.DueSoon).
	dueSoon time.Duration
}

// reap ends the project's expired claims, and its locks' lapsed holds
// and places, now, and logs each.
func (pr *served) reap(ctx context.Context) {
	defer pr.reapLocks(ctx)
	rctx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	cs, err := pr.store.ReapClaims(rctx)
	if err != nil {
		if ctx.Err() == nil {
			pr.log.ErrorContext(ctx, "reap claims", "event", logs.ReaperFailed, "err", err)
		}
		return
	}
	for _, c := range cs {
		pr.log.InfoContext(ctx, "claim expired", "event", logs.ClaimExpired, "issue", c.Issue, "principal", c.Holder.Principal,
			"session", c.Holder.Session, "epoch", c.Epoch)
	}
}

// prune deletes the project's registry rows and read inbox items older
// than lim keeps them (S-6, S-7), and logs what it removed.
func (pr *served) prune(ctx context.Context, lim Limits) {
	pctx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	n, err := pr.store.Prune(pctx, time.Duration(lim.AgentKeep), time.Duration(lim.InboxKeep))
	if err != nil {
		if ctx.Err() == nil {
			pr.log.ErrorContext(ctx, "prune", "event", logs.ReaperPruneFailed, "err", err)
		}
		return
	}
	if n != (store.Pruned{}) {
		pr.log.InfoContext(ctx, "pruned", "event", logs.ReaperPruned, "agents", n.Agents, "inbox", n.Inbox)
	}
}

// ProjectOpener returns a Config.Open for a daemon whose database db
// holds its projects' schemas: it opens a project's store on the
// project's schema, from db's one pool, with opts, the project's prefix,
// and admins and members that follow ctl as it changes: an admin is a
// server admin, or an active principal whose role in the project is
// admin (design §15.3). Each store is measured and traced by tel
// ([StoreObserver], [StoreTracer]), which may be nil.
func ProjectOpener(db *store.DB, ctl Index, opts store.Options, tel *telemetry.Telemetry) func(context.Context, store.Project) (Store, error) {
	return func(ctx context.Context, p store.Project) (Store, error) {
		o := opts
		o.Prefix = p.Prefix
		o.IsAdmin = func(principal string) bool { return ctl.Access(principal, p.ID).Role == store.RoleAdmin }
		o.ListAdmins = func() []string { return ctl.ProjectAdmins(p.ID) }
		o.IsMember = func(principal string) bool { return ctl.Access(principal, p.ID).Role != "" }
		o.Observer = StoreObserver(tel, o.Logger, p.ID)
		o.Tracer = StoreTracer(tel, p.ID)
		st, err := db.Open(ctx, p.Schema, o)
		if err != nil {
			return nil, err // not a nil *store.Store in a Store that is not nil
		}
		return st, nil
	}
}
