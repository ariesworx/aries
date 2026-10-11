package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
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

	// wmu serializes writes from the transaction's start until the key
	// index has taken its change, so the index changes in commit order:
	// a key added and then removed is never left in it.
	wmu sync.Mutex
	// keys answers AuthorizedKey from memory.
	keys keyIndex
}

// OpenControl connects to the control database named in dsn
// (go-sql-driver/mysql format), checks the account as [Open] does,
// applies pending migrations, loads the key index and starts the
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
		Admins: opts.Admins, Now: opts.Now, Logger: opts.Logger, AllowUnsafeAccount: opts.AllowUnsafeAccount,
		CommitInterval: opts.CommitInterval, MaxAttempts: opts.MaxAttempts, Readers: opts.Readers,
	}, controlSchema)
	if err != nil {
		return nil, err
	}
	c := &Control{s: s, lim: opts.Limits.withDefaults(), log: s.opts.Logger}
	if err := c.loadKeys(ctx); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return c, nil
}

// Close stops the committer, makes a final Dolt commit of pending writes
// and closes the connections. It is safe to call more than once; every
// call returns the first one's error.
func (c *Control) Close() error { return c.s.Close() }

// Flush makes a Dolt commit now if any write happened since the last one.
func (c *Control) Flush(ctx context.Context) error { return c.s.Flush(ctx) }

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
// with the control store's limits and the change the attempt makes to
// the key index.
type cwtx struct {
	*wtx
	lim ControlLimits
	// admins reports whether a principal is a server admin.
	admins func(string) bool
	// index, when set, is applied to the key index once the attempt
	// commits.
	index func(*keyIndex)
}

// write runs fn through the store's write loop as actor, so fn must be
// safe to rerun, and then applies the change fn made to the key index.
//
// A write that fails, rather than being refused, may have committed all
// the same (a connection lost during COMMIT does not say), so the index
// is then read again from the database: a removed key must not stay
// authorized because its removal's reply was lost.
func (c *Control) write(ctx context.Context, actor Actor, fn func(*cwtx) error) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var index func(*keyIndex)
	err := c.s.write(ctx, actor, func(w *wtx) error {
		index = nil
		cw := &cwtx{wtx: w, lim: c.lim, admins: c.IsServerAdmin}
		if err := fn(cw); err != nil {
			return err
		}
		index = cw.index
		return nil
	})
	switch {
	case err == nil:
		if index != nil {
			c.keys.mu.Lock()
			index(&c.keys)
			c.keys.mu.Unlock()
		}
	case !refusal(err):
		// The caller's ctx may be what failed the write; the reload gets
		// its own.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if lerr := c.loadKeys(rctx); lerr != nil {
			c.log.Error("reload of the key index failed; it may be stale until starfixd restarts", "err", lerr)
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

// keyIndex is every key and whether its principal is active, in memory.
// sshd asks about each key a stranger offers, before authentication, so
// the answer must cost no database read (design §15.2).
type keyIndex struct {
	mu sync.RWMutex
	// byFingerprint holds every key, of disabled principals too.
	byFingerprint map[string]indexedKey
	// active holds the principals whose state is active.
	active map[string]bool
}

// indexedKey is a key in the index: its principal and its text.
type indexedKey struct{ principal, key string }

// AuthorizedKey returns the principal a public key authenticates and the
// key in authorized_keys form (its type and base64, nothing else), for
// the fingerprint [golang.org/x/crypto/ssh.FingerprintSHA256] gives. It
// answers only for a key of an active principal: ok is false for a key
// that is unknown or removed, or whose principal is disabled.
//
// It reads memory only, never the database, so it is safe to call for
// every key offered before authentication. The index is loaded when the
// store opens and follows this store's writes; a change another process
// makes to the database is not seen until the next open.
func (c *Control) AuthorizedKey(fingerprint string) (principal, key string, ok bool) {
	c.keys.mu.RLock()
	defer c.keys.mu.RUnlock()
	k, found := c.keys.byFingerprint[fingerprint]
	if !found || !c.keys.active[k.principal] {
		return "", "", false
	}
	return k.principal, k.key, true
}

// loadKeys replaces the key index with what the database holds, read in
// one snapshot.
func (c *Control) loadKeys(ctx context.Context) error {
	q, end, err := c.s.beginRead(ctx)
	if err != nil {
		return err
	}
	defer end()
	active := map[string]bool{}
	names, err := queryStrings(ctx, q, `SELECT name FROM principals WHERE state = ?`, string(PrincipalActive))
	if err != nil {
		return fmt.Errorf("load key index: %w", err)
	}
	for _, n := range names {
		active[n] = true
	}
	rows, err := q.QueryContext(ctx, "SELECT fingerprint, principal, `key` FROM principal_keys")
	if err != nil {
		return fmt.Errorf("load key index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	keys := map[string]indexedKey{}
	for rows.Next() {
		var fp string
		var k indexedKey
		if err := rows.Scan(&fp, &k.principal, &k.key); err != nil {
			return fmt.Errorf("load key index: %w", err)
		}
		keys[fp] = k
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load key index: %w", err)
	}
	c.keys.mu.Lock()
	c.keys.byFingerprint, c.keys.active = keys, active
	c.keys.mu.Unlock()
	return nil
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
