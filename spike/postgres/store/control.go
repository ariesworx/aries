package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ariesworx/starfix/internal/logs"
)

// The control store (design §15). One server serves many projects, each
// in a Dolt database of its own with a [Store] of its own. What belongs
// to the server rather than to a project lives in one more database, the
// control database: the projects, the principals, their public SSH keys,
// and which principal may reach which project, as a member or an admin.
//
// Control is a Store over that database: it has the same single writer,
// retry loop, write_id check, event log and Dolt committer, and other
// migrations (controlmigrations). Each of its writes is authorized inside
// its transaction (control_authz.go) and appends an event, so every
// grant, revocation and key change is history.

// ControlOptions configure a [Control]. The zero value is usable, and
// names no server admin, so nothing can be written.
type ControlOptions struct {
	// Admins are the server admins: the principals who may make every
	// change. Project admins, a role in the store, may make some
	// (control_authz.go). None may be a [Reserved] name. They come from
	// starfixd's config file, so nothing over the protocol makes one.
	Admins []string
	// Limits bound the rows the store holds. Zero fields take
	// [DefaultControlLimits].
	Limits ControlLimits
	// Now is the server clock. Default time.Now.
	Now func() time.Time
	// Clock, when set, is the server clock in place of Now, as
	// [Options.Clock] is.
	Clock *Clock
	// Logger receives background committer errors, a failed reload of
	// the key index, and the warning that AllowUnsafeAccount let an
	// unsafe account through. Default discards.
	Logger *slog.Logger
	// AllowUnsafeAccount opens the store even when the database account
	// is unsafe, as [Options.AllowUnsafeAccount] does.
	AllowUnsafeAccount bool
	// CommitInterval is how often pending writes become a Dolt commit.
	// Default 1s; negative disables the background committer (call
	// Flush).
	CommitInterval time.Duration
	// MaxAttempts bounds retries of a write that loses to a concurrent
	// transaction. Default 5.
	MaxAttempts int
	// Readers caps the read pool. Default 8.
	Readers int
	// Tracer, when set, is told where a write's time goes, as
	// [Options.Tracer] is.
	Tracer Tracer
	// Operator marks a store opened by a command the operator runs on
	// the server (`starfixd control init`), not by the daemon: its writes
	// by [OperatorPrincipal] are allowed as a server admin's are. The
	// daemon never sets it, so no connection gains anything from the
	// name, which is reserved besides.
	Operator bool
}

// Control is the server's control store: projects, principals, their
// keys and memberships. It is safe for concurrent use. [OpenControl]
// returns one, and [Control.Close] releases it.
//
// Writes name their [Actor] and are authorized in the store: a refusal
// is an [*AccessError]. Reads are not guarded; the caller decides who may
// list what.
type Control struct {
	// s is a Store on the control database, used for its writer, read
	// pool, committer and event log. None of its issue methods apply.
	s   *Store
	lim ControlLimits
	log *slog.Logger
	// operator is ControlOptions.Operator.
	operator bool

	// wmu serializes writes from the transaction's start until the index
	// has taken its change, so the index changes in commit order: a key
	// added and then removed is never left in it.
	wmu sync.Mutex
	// ix answers AuthorizedKey and Access from memory.
	ix index
	// elsewhere, when set, names the principal of a key the store holds
	// no row for (KeysElsewhere).
	elsewhere atomic.Pointer[func(fingerprint string) (principal string, ok bool)]
}

// KeysElsewhere tells the store of keys that belong to a principal
// though it holds no row for them: owner returns the principal of the
// key with a fingerprint, and false for a key it does not know. A key
// belongs to one principal, so [Control.AddKey] and
// [Control.AddPrincipal] then refuse such a key for any other principal,
// with the [*TakenError] of a key another principal has, after the
// checks of who may add one.
//
// starfixd gives it the keys of the authorized_keys file its operator
// maintains by hand. sshd reads the keys file written from this store
// before that file (design §14, decision 26), so a key of that file
// added here under another name would sign its owner in as someone
// else. The operator's `starfixd control init`, which imports that file,
// sets none.
//
// owner runs inside a write, which may be retried: it must be fast, safe
// for concurrent use, and give the same answer again.
func (c *Control) KeysElsewhere(owner func(fingerprint string) (principal string, ok bool)) {
	c.elsewhere.Store(&owner)
}

// OpenControl connects to the control database named in dsn
// (go-sql-driver/mysql format), checks the account as [Open] does,
// applies pending migrations, loads the index and starts the
// committer. ctx bounds only the opening; the caller must Close the
// store.
//
// OpenControl refuses, with [ErrInvalid], a dsn that names no database,
// an invalid admin or limit in opts, and a database that holds a project.
// It refuses an unsafe account with an [*UnsafeAccountError] unless
// opts.AllowUnsafeAccount is set, and a database a newer binary migrated
// with [ErrSchemaTooNew].
func OpenControl(ctx context.Context, dsn string, opts ControlOptions) (*Control, error) {
	if err := opts.Limits.Validate(); err != nil {
		return nil, err
	}
	s, err := open(ctx, dsn, Options{
		Admins: opts.Admins, Now: opts.Now, Clock: opts.Clock, Logger: opts.Logger, AllowUnsafeAccount: opts.AllowUnsafeAccount,
		CommitInterval: opts.CommitInterval, MaxAttempts: opts.MaxAttempts, Readers: opts.Readers, Tracer: opts.Tracer,
	}, controlSchema)
	if err != nil {
		return nil, err
	}
	c := &Control{s: s, lim: opts.Limits.withDefaults(), log: s.opts.Logger, operator: opts.Operator}
	if err := c.load(ctx); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return c, nil
}

// Close stops the committer, makes a final Dolt commit of pending writes
// and closes the connections. It is safe to call more than once; every
// call returns the first one's error.
func (c *Control) Close() error { return c.s.Close() }

// Ping checks that Dolt answers, as [Store.Ping] does.
func (c *Control) Ping(ctx context.Context) error { return c.s.Ping(ctx) }

// Flush makes a Dolt commit now if any write happened since the last one.
func (c *Control) Flush(ctx context.Context) error { return c.s.Flush(ctx) }

// BackupTag commits everything written to the control database so far
// and tags the commit name, as [Store.BackupTag] does for a project's.
func (c *Control) BackupTag(ctx context.Context, name string) (string, error) {
	return c.s.BackupTag(ctx, name)
}

// Now reads the server clock (ControlOptions.Now), in UTC to the
// microsecond.
func (c *Control) Now() time.Time { return c.s.now() }

// Limits returns the limits in force.
func (c *Control) Limits() ControlLimits { return c.lim }

// Events returns up to limit events of the control database with seq
// greater than after, in order; limit 0 means 100, and it is capped at
// 1,000. A target is a project id, a principal name, a key fingerprint,
// or PROJECT/PRINCIPAL for a membership.
func (c *Control) Events(ctx context.Context, after int64, limit int) ([]Event, error) {
	return c.s.Events(ctx, after, limit)
}

// cwtx is one attempt at a control write: the store's write transaction,
// with the control store's limits and the changes the attempt makes to
// the index.
type cwtx struct {
	*wtx
	lim ControlLimits
	// admins reports whether the config file names a principal a server
	// admin (Control.ConfigAdmin): what protects it as a target.
	admins func(string) bool
	// configAdmins are the principals the config file names server
	// admins.
	configAdmins []string
	// index holds the changes to apply to the index, in order, once the
	// attempt commits.
	index []func(*index)
	// elsewhere is Control.elsewhere, or nil.
	elsewhere func(fingerprint string) (principal string, ok bool)
}

// indexed records a change for the index to take when the attempt
// commits.
func (w *cwtx) indexed(fn func(*index)) { w.index = append(w.index, fn) }

// write runs fn through the store's write loop as actor, so fn must be
// safe to rerun, and then applies the changes fn made to the index.
//
// In the operator's store (ControlOptions.Operator) the operator's
// principal writes as a server admin does: the equivalent of
// [Store.ImportIssue]'s exemption, for `starfixd control init`.
//
// A write that fails, rather than being refused, may have committed all
// the same (a connection lost during COMMIT does not say), so the index
// is then read again from the database: a removed key must not stay
// authorized because its removal's reply was lost.
func (c *Control) write(ctx context.Context, actor Actor, fn func(*cwtx) error) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var changes []func(*index)
	err := c.s.write(ctx, actor, func(w *wtx) error {
		changes = nil
		switch {
		case c.operator && actor.Principal == OperatorPrincipal:
			w.admin = true
		case w.admin:
			// Named in the config file. Disabled wins (decision 20),
			// read here so that it holds when the write commits.
			switch p, err := readPrincipal(ctx, w.tx, actor.Principal); {
			case errors.Is(err, ErrNotFound):
			case err != nil:
				return err
			default:
				w.admin = p.State == PrincipalActive
			}
		}
		cw := &cwtx{wtx: w, lim: c.lim, admins: c.ConfigAdmin, configAdmins: c.s.opts.Admins}
		if fn := c.elsewhere.Load(); fn != nil {
			cw.elsewhere = *fn
		}
		if err := fn(cw); err != nil {
			return err
		}
		changes = cw.index
		return nil
	})
	switch {
	case err == nil:
		if len(changes) > 0 {
			c.ix.mu.Lock()
			for _, change := range changes {
				change(&c.ix)
			}
			c.ix.mu.Unlock()
		}
	case !refusal(err):
		// The caller's ctx may be what failed the write; the reload gets
		// its own.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if lerr := c.load(rctx); lerr != nil {
			c.log.ErrorContext(ctx, "reload of the control index failed; it may be stale until starfixd restarts", "event", logs.ControlIndexStale, "err", lerr)
		}
	}
	return err
}

// refusal reports whether err is the store refusing a request, which
// wrote nothing, rather than a failure.
func refusal(err error) bool {
	for _, e := range []error{ErrNotFound, ErrConflict, ErrExists, ErrInvalid, ErrForbidden, ErrBusy} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// index is what the daemon asks the control store on every handshake
// and every issue write, in memory: the keys, the active principals, the
// projects and the roles. A handshake checks the key that signed in, and
// a project's store asks who its admins are on each write, so neither
// answer may cost a database read (design §15.2, §15.3). The file of
// keys the daemon writes for sshd is built from it too.
type index struct {
	mu sync.RWMutex
	// byFingerprint holds every key, of disabled principals too.
	byFingerprint map[string]indexedKey
	// active holds the principals whose state is active, and disabled
	// those whose state is disabled. A name in neither has no row.
	active, disabled map[string]bool
	// projects holds every project, in every state, by id.
	projects map[string]Project
	// roles holds every membership, of disabled principals and of
	// projects in every state too.
	roles map[membership]Role
}

// membership names one principal in one project.
type membership struct{ principal, project string }

// indexedKey is a key in the index: its principal and its text.
type indexedKey struct{ principal, key string }

// AuthorizedKey returns the principal a public key authenticates and the
// key in authorized_keys form (its type and base64, nothing else), for
// the fingerprint [golang.org/x/crypto/ssh.FingerprintSHA256] gives. It
// answers only for a key of an active principal: ok is false for a key
// that is unknown or removed, or whose principal is disabled.
//
// It reads memory only, never the database, so every handshake can ask
// it. The index is loaded when the
// store opens and follows this store's writes; a change another process
// makes to the database is not seen until [Control.Reload] or the next
// open.
func (c *Control) AuthorizedKey(fingerprint string) (principal, key string, ok bool) {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	k, found := c.ix.byFingerprint[fingerprint]
	if !found || !c.ix.active[k.principal] {
		return "", "", false
	}
	return k.principal, k.key, true
}

// AuthorizedKeys returns every key [Control.AuthorizedKey] answers for,
// the keys of the active principals, by principal and then fingerprint:
// what the daemon writes sshd's keys file from. Only Fingerprint,
// Principal and Key are set. It reads memory only, as AuthorizedKey
// does.
func (c *Control) AuthorizedKeys() []PrincipalKey {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	var out []PrincipalKey
	for fp, k := range c.ix.byFingerprint {
		if c.ix.active[k.principal] {
			out = append(out, PrincipalKey{Fingerprint: fp, Principal: k.principal, Key: k.key})
		}
	}
	slices.SortFunc(out, func(a, b PrincipalKey) int {
		return cmp.Or(strings.Compare(a.Principal, b.Principal), strings.Compare(a.Fingerprint, b.Fingerprint))
	})
	return out
}

// Access is what a principal may do in a project, as [Control.Access]
// reads it from memory.
type Access struct {
	// Project is the project, when Active.
	Project Project
	// Active reports whether the project exists and is active. A project
	// that is unknown, still being created or archived is served to no
	// one.
	Active bool
	// Role is what the principal is in the project: RoleAdmin for a
	// server admin and for an active principal whose role there is
	// admin, RoleMember for an active member, and empty for anyone else.
	// A disabled principal has none, a server admin included (decision
	// 20). It does not depend on Active.
	Role Role
	// Disabled reports whether the principal has a row that is disabled.
	Disabled bool
}

// Access reports whether project is active and what principal is in it:
// the one check of a handshake, and of a project store's admin rights
// ([Options.IsAdmin]). It reads memory only, as [Control.AuthorizedKey]
// does, and follows this store's writes the same way.
func (c *Control) Access(principal, project string) Access {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	var a Access
	if p, ok := c.ix.projects[project]; ok && p.State == ProjectActive {
		a.Project, a.Active = p, true
	}
	switch {
	case c.ix.disabled[principal]:
		a.Disabled = true
	case c.ConfigAdmin(principal):
		a.Role = RoleAdmin
	case c.ix.active[principal]:
		a.Role = c.ix.roles[membership{principal, project}]
	}
	return a
}

// Standing reports what the server knows of principal whatever project
// it asks for: known, when it has a row or the config file names it a
// server admin, and disabled, when its row is disabled. A principal that
// is not known, or is disabled, is let into no project, so the daemon
// can refuse its connection before a hello names one. It reads memory
// only, as [Control.Access] does.
func (c *Control) Standing(principal string) (known, disabled bool) {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	if c.ix.disabled[principal] {
		return true, true
	}
	return c.ix.active[principal] || c.ConfigAdmin(principal), false
}

// ActiveProjects returns how many projects are active, from memory.
func (c *Control) ActiveProjects() int {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	n := 0
	for _, p := range c.ix.projects {
		if p.State == ProjectActive {
			n++
		}
	}
	return n
}

// Reload reads the index again from the database, so that it holds what
// another process wrote: `starfixd control init` run beside the daemon.
// It waits for a write in progress, so no change of this store's is
// lost.
func (c *Control) Reload(ctx context.Context) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.load(ctx)
}

// load replaces the index with what the database holds, read in one
// snapshot. The caller holds wmu, or is OpenControl.
func (c *Control) load(ctx context.Context) error {
	q, end, err := c.s.beginRead(ctx)
	if err != nil {
		return err
	}
	defer end()
	active, disabled := map[string]bool{}, map[string]bool{}
	for state, set := range map[PrincipalState]map[string]bool{PrincipalActive: active, PrincipalDisabled: disabled} {
		names, err := queryStrings(ctx, q, `SELECT name FROM principals WHERE state = ?`, string(state))
		if err != nil {
			return fmt.Errorf("load index: %w", err)
		}
		for _, n := range names {
			set[n] = true
		}
	}
	rows, err := q.QueryContext(ctx, "SELECT fingerprint, principal, `key` FROM principal_keys")
	if err != nil {
		return fmt.Errorf("load index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	keys := map[string]indexedKey{}
	for rows.Next() {
		var fp string
		var k indexedKey
		if err := rows.Scan(&fp, &k.principal, &k.key); err != nil {
			return fmt.Errorf("load index: %w", err)
		}
		keys[fp] = k
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load index: %w", err)
	}
	projects, roles, err := loadProjects(ctx, q)
	if err != nil {
		return fmt.Errorf("load index: %w", err)
	}
	c.ix.mu.Lock()
	c.ix.byFingerprint, c.ix.active, c.ix.disabled, c.ix.projects, c.ix.roles = keys, active, disabled, projects, roles
	c.ix.mu.Unlock()
	return nil
}

// loadProjects reads every project and every membership for the index.
func loadProjects(ctx context.Context, q querier) (map[string]Project, map[membership]Role, error) {
	projects := map[string]Project{}
	prows, err := q.QueryContext(ctx, projectSelect)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = prows.Close() }()
	for prows.Next() {
		p, err := scanProject(prows)
		if err != nil {
			return nil, nil, err
		}
		projects[p.ID] = p
	}
	if err := prows.Err(); err != nil {
		return nil, nil, err
	}
	roles := map[membership]Role{}
	mrows, err := q.QueryContext(ctx, `SELECT principal, project, role FROM members`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = mrows.Close() }()
	for mrows.Next() {
		var m membership
		var r Role
		if err := mrows.Scan(&m.principal, &m.project, &r); err != nil {
			return nil, nil, err
		}
		roles[m] = r
	}
	return projects, roles, mrows.Err()
}

// queryStrings runs query, which selects one string column, and returns
// its rows.
func queryStrings(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
