package server

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ariesworx/starfix/internal/store"
	"github.com/go-sql-driver/mysql"
)

// served is one project the daemon serves and what its requests need:
// its store, its default account and its log. A server of one project
// (Config.Store) has one for its whole life; a server of many
// (Config.Control) has one for each store its registry holds open.
type served struct {
	// id is the project's UUID.
	id      string
	store   *store.Store
	account string
	// log is the daemon's logger, naming the project when the server
	// holds many.
	log *slog.Logger
	// timeout bounds each reap, prune and presence update
	// (Config.RequestTimeout).
	timeout time.Duration
	// used is when the project last answered a request, or was opened if
	// it has answered none, in Unix nanoseconds on the server's clock:
	// what `sfx admin status` shows as last used.
	used atomic.Int64
}

// reap ends the project's expired claims now and logs each.
func (pr *served) reap(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	cs, err := pr.store.ReapClaims(rctx)
	if err != nil {
		if ctx.Err() == nil {
			pr.log.Error("reap claims", "err", err)
		}
		return
	}
	for _, c := range cs {
		pr.log.Info("claim expired", "issue", c.Issue, "principal", c.Holder.Principal,
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
			pr.log.Error("prune", "err", err)
		}
		return
	}
	if n != (store.Pruned{}) {
		pr.log.Info("pruned", "agents", n.Agents, "inbox", n.Inbox)
	}
}

// DatabaseDSN returns dsn with its database replaced by name: another
// database on the same Dolt server, as the same account. A server of
// many projects reaches its control database and each project's this
// way, from the one dsn it is configured with. name must be a
// [store.ValidDBName], since it comes from settings or from the control
// store and never from a client.
func DatabaseDSN(dsn, name string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	if !store.ValidDBName(name) {
		return "", fmt.Errorf("database name %q must be a lowercase letter, then lowercase letters, digits and underscores, at most 64", name)
	}
	cfg.DBName = name
	return cfg.FormatDSN(), nil
}

// ProjectOpener returns a Config.Open for a daemon configured with dsn:
// it opens a project's store on the project's own database
// ([DatabaseDSN]), with opts, the project's prefix, and admins that
// follow ctl as it changes: a server admin, or an active principal whose
// role in the project is admin (design §15.3).
func ProjectOpener(dsn string, ctl *store.Control, opts store.Options) func(context.Context, store.Project) (*store.Store, error) {
	return func(ctx context.Context, p store.Project) (*store.Store, error) {
		d, err := DatabaseDSN(dsn, p.DBName)
		if err != nil {
			return nil, err
		}
		o := opts
		o.Prefix = p.Prefix
		o.IsAdmin = func(principal string) bool { return ctl.Access(principal, p.ID).Role == store.RoleAdmin }
		return store.Open(ctx, d, o)
	}
}

// ProjectDBs is the block of database names the operator granted
// starfixd's account for new projects (design §15.5, 1, decision 16):
// Prefix followed by a number of three digits, from 001 to Count, so
// {starfix_p, 100} is starfix_p001 to starfix_p100. Dolt honors a grant
// only on an exact name, so the names are fixed and listed, not matched.
// The zero value is no block: `sfx admin project add` is then refused.
type ProjectDBs struct {
	// Prefix starts each name: a lowercase letter, then lowercase
	// letters, digits and underscores.
	Prefix string `yaml:"prefix"`
	// Count is how many names the block holds, at most MaxProjectDBs.
	Count int `yaml:"count"`
}

// MaxProjectDBs is the most names a block may hold: what three digits
// number.
const MaxProjectDBs = 999

// Name returns the block's i-th name, counting from 1.
func (b ProjectDBs) Name(i int) string { return fmt.Sprintf("%s%03d", b.Prefix, i) }

// Validate refuses a block whose names would not be database names, or
// that holds no name or more than MaxProjectDBs. The zero value is
// valid.
func (b ProjectDBs) Validate() error {
	switch {
	case b == ProjectDBs{}:
		return nil
	case !store.ValidDBName(b.Name(MaxProjectDBs)):
		return fmt.Errorf("project_dbs prefix %q does not start a database name; fix: set it to a lowercase letter, then lowercase letters, digits and underscores, at most 61, such as starfix_p", b.Prefix)
	case b.Count < 1 || b.Count > MaxProjectDBs:
		return fmt.Errorf("project_dbs count %d must be 1 to %d; fix: set it to how many names you granted starfixd's account", b.Count, MaxProjectDBs)
	}
	return nil
}

// holds reports whether name is one of the block's names.
func (b ProjectDBs) holds(name string) bool {
	num, ok := strings.CutPrefix(name, b.Prefix)
	if !ok || b.Prefix == "" || len(num) != 3 {
		return false
	}
	n, err := strconv.Atoi(num)
	return err == nil && n >= 1 && n <= b.Count
}

// free returns the block's lowest name that is not in used, or false
// when every name is.
func (b ProjectDBs) free(used map[string]bool) (string, bool) {
	for i := 1; i <= b.Count; i++ {
		if n := b.Name(i); !used[n] {
			return n, true
		}
	}
	return "", false
}
