package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ariesworx/aries/internal/logs"
)

// DefaultPrefix starts the ids of a store given no prefix. An issue keeps
// its id for good, so this is never changed once a release has made one.
const DefaultPrefix = "ar"

// Options configure a Store. The zero value is usable.
type Options struct {
	// Prefix is used for IDs the store generates. Default [DefaultPrefix].
	Prefix string
	// MaxAttempts bounds retries of a write that loses to a concurrent
	// transaction. Default 5.
	MaxAttempts int
	// Readers caps the connections the store's reads hold at once, of
	// its DB's pool. Default [DefaultReaders].
	Readers int
	// Now is the server clock. Default time.Now.
	Now func() time.Time
	// Clock, when set, is the server clock in place of Now: the one
	// [Clock] a process shares among its stores, so that each learns of
	// the clock's steps (clock.go). Unset, the store makes its own from
	// Now.
	Clock *Clock
	// ClientAhead is how far past the store's time a time a client
	// supplies may be. Default [DefaultClientAhead], at most
	// [MaxClientAhead].
	ClientAhead time.Duration
	// IdemWindow is how long an idempotency key gives back its request's
	// result (idem.go). Default [DefaultIdemWindow]; one that is not
	// positive is refused.
	IdemWindow time.Duration
	// AwayMax is the longest lease a renewal of every session's claims
	// (`aries away`) may ask for. Default [MaxLease]; one that is negative,
	// or under [MinLease], is refused.
	AwayMax time.Duration
	// Logger receives the warning that AllowUnsafeAccount let an unsafe
	// account through. Default discards.
	Logger *slog.Logger
	// Admins are the principals who may change issues others hold and
	// force a close. None may be a [Reserved] name.
	Admins []string
	// IsAdmin, when set, is asked as well as Admins, on every write and
	// admin-only request, so its answer may change while the store is
	// open: a multi-project ariesd answers from the control store's
	// project admins ([Control.Access]). It must be fast, read no
	// database, and be safe for concurrent use.
	IsAdmin func(principal string) bool
	// ListAdmins, when set, lists the principals IsAdmin answers for:
	// the project's admins first, then the server's, each once. The
	// store tells them of a gate that names no approvers, which they pass
	// (decision 68), before those in Admins. Like IsAdmin it must be
	// fast, read no database, and be safe for concurrent use.
	ListAdmins func() []string
	// AllowUnsafeAccount opens the store even when the database role is a
	// superuser or holds rights beyond its database (account.go). Off,
	// Open refuses such a role with an [*UnsafeAccountError]; ariesd turns
	// it on only for --dev --allow-unsafe-account.
	AllowUnsafeAccount bool
	// Limits bound requests and per-principal rows. Zero fields take
	// [DefaultLimits].
	Limits Limits
	// Observer, when set, is told of each write, retry and change of a
	// claim or a lock, for the daemon's metrics. See [Observer].
	Observer Observer
	// Tracer, when set, is told where a write's time goes, under the
	// caller's context, for the daemon's traces. See [Tracer].
	Tracer Tracer
	// NoMigrate opens the database only as it is: one that this binary's
	// migrations have not all been applied to is refused with
	// [ErrSchemaOld], and nothing in it is changed. ariesd sets it for
	// the database of an archived project, which is migrated when the
	// project is activated and not before.
	NoMigrate bool
	// ReadOnly opens the schema to read it beside the process that
	// writes it: it takes no writer's lock, migrates nothing (as
	// NoMigrate does), and refuses every write with [ErrReadOnly].
	// `ariesd export-bd` and the clock check of `ariesd upgrade` open a
	// running daemon's schemas so.
	ReadOnly bool
}

// Store is the server-side issue store. It is safe for concurrent use.
// [Open] returns one, and [Store.Close] releases it.
type Store struct {
	// w is the single writer and r the readers, each taking connections
	// from pool, the DB the store was opened from, in schema. own is set
	// when the store has pool to itself and closes it; fenced while it
	// holds schema's writer's lock (fence.go).
	w, r   *sql.DB
	pool   *DB
	schema string
	own    bool
	fenced bool
	opts   Options

	closeOnce sync.Once
	closeErr  error

	// watch holds the open inbox watches; publish feeds them after each
	// commit that wrote inbox items.
	watch watchers
	// similar caches closed titles for SimilarClosed (similar.go).
	similar similarCache
	// stats counts the writes that committed (stats.go).
	stats writeStats
	// clock is the server clock, and tm the store's own time, which the
	// clock cannot move backward (clock.go). hasClaims is false for the
	// control database, which has no leases to settle; settling
	// serializes SettleClock. clockName names the schema to the clock,
	// which remembers what each has settled: its server's address, its
	// database and its name.
	clock     *Clock
	tm        storeTime
	hasClaims bool
	settling  sync.Mutex
	clockName string
	// migrated is how many migrations Open applied.
	migrated int

	// beforeCommit, when set by tests, runs inside every write transaction
	// just before COMMIT.
	beforeCommit func(context.Context) error
	// beforeRetry, when set by tests, runs between a write's attempts,
	// once the attempt that lost has rolled back and holds no lock: where
	// a test makes the concurrent write the rerun must see.
	beforeRetry func(context.Context) error
	// wrapRead, when set by tests, wraps the querier beginRead returns, so
	// a test can act between a read's queries.
	wrapRead func(querier) querier
}

// Open connects to the Postgres schema named in dsn (a postgres:// URL,
// dsn.go), checks the server's version and the role, takes the schema's
// writer's lock and applies pending migrations. The store has a [DB] of
// its own, with a connection for its writer and one for each of its
// readers, which closes with it: what a command that opens one store
// needs. A daemon opens each project's from its one DB instead
// ([DB.Open]). ctx bounds only the opening; the caller must Close the
// store.
//
// Open refuses, with [ErrInvalid], a dsn that is not a postgres:// URL or
// names no database, a schema that does not exist, and an invalid
// prefix, admin or limit in opts. It refuses a server older than
// Postgres [version.PostgresMin] with a [*ServerVersionError], an unsafe
// role with an [*UnsafeAccountError] unless opts.AllowUnsafeAccount is
// set, a schema the role may not use with an error [SchemaDenied]
// recognizes, a schema another process writes with a [*ServedError], a
// schema a newer binary migrated with [ErrSchemaTooNew], and, with
// opts.NoMigrate or opts.ReadOnly, one with migrations pending with
// [ErrSchemaOld].
func Open(ctx context.Context, dsn string, opts Options) (*Store, error) {
	return openOwn(ctx, dsn, max(opts.Readers, DefaultReaders)+1, func(db *DB, schema string) (*Store, error) {
		return db.open(ctx, schema, opts, projectSchema, false)
	})
}

// openOwn opens a DB of conns connections on dsn's database and, with
// it, the store open makes of dsn's schema, which closes the DB when it
// closes.
func openOwn(ctx context.Context, dsn string, conns int, open func(*DB, string) (*Store, error)) (*Store, error) {
	d, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := OpenDB(ctx, dsn, DBOptions{Conns: conns})
	if err != nil {
		return nil, err
	}
	s, err := open(db, d.Schema)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	s.own = true
	return s, nil
}

// Open opens the store of the project in schema, on db's connections, as
// the package's Open does for the schema of a DSN, and refuses what that
// refuses. Every store opened from db shares its pool, and its writer's
// lock is db's: to every other process, two stores of one schema on one
// db are one writer, so it is the caller's to open one store of a
// project at a time. The caller must Close the store before db.
func (db *DB) Open(ctx context.Context, schema string, opts Options) (*Store, error) {
	return db.open(ctx, schema, opts, projectSchema, false)
}

// DefaultReaders is Options.Readers' default: the most connections one
// store's reads hold at once.
const DefaultReaders = 8

// open opens a store of schema on db, which sc describes: a project's,
// or the control store's, whose store ([Control]) wraps the Store it
// returns for its writer and event log. create makes the schema when it
// does not exist, which only the operator's `ariesd control init` asks.
func (db *DB) open(ctx context.Context, schema string, opts Options, sc schema, create bool) (*Store, error) {
	if schema == "" {
		return nil, fmt.Errorf("%w: no schema named", ErrInvalid)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Prefix == "" {
		opts.Prefix = DefaultPrefix
	}
	if !prefixPattern.MatchString(opts.Prefix) {
		return nil, fmt.Errorf("%w: prefix %q", ErrInvalid, opts.Prefix)
	}
	if err := checkAdmins(opts.Admins); err != nil {
		return nil, err
	}
	if err := opts.Limits.Validate(); err != nil {
		return nil, err
	}
	opts.Limits = opts.Limits.withDefaults()
	if opts.ClientAhead < 0 || opts.ClientAhead > MaxClientAhead {
		return nil, fmt.Errorf("%w: client times may be up to %s ahead, not %s", ErrInvalid, MaxClientAhead, opts.ClientAhead)
	}
	if opts.ClientAhead == 0 {
		opts.ClientAhead = DefaultClientAhead
	}
	if opts.IdemWindow < 0 {
		return nil, fmt.Errorf("%w: an idempotency window must be more than zero, not %s", ErrInvalid, opts.IdemWindow)
	}
	if opts.IdemWindow == 0 {
		opts.IdemWindow = DefaultIdemWindow
	}
	if opts.AwayMax != 0 && opts.AwayMax < MinLease {
		return nil, fmt.Errorf("%w: away may reach at least %s, not %s", ErrInvalid, MinLease, opts.AwayMax)
	}
	if opts.AwayMax == 0 {
		opts.AwayMax = MaxLease
	}
	opts.Admins = slices.Clone(opts.Admins)
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.Readers <= 0 {
		opts.Readers = DefaultReaders
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.NoMigrate = opts.NoMigrate || opts.ReadOnly

	s := &Store{
		// One writer: writes are serialized in this process, so the
		// event sequence is gapless without contention (stage 0: one
		// writer ran at 233 req/s, within 15% of the best pool).
		w:    db.sqlDB(schema, 1),
		r:    db.sqlDB(schema, opts.Readers),
		opts: opts,
		pool: db, schema: schema,
		// Every writer holds its schema's lock but the operator's
		// `ariesd control init`, which writes the control store beside
		// the daemon (fence.go); create is set for that one alone.
		fenced: !opts.ReadOnly && !create,

		clock: opts.Clock, hasClaims: sc.table == projectSchema.table,
		clockName: db.addr + "/" + schema,
	}
	if s.clock == nil {
		s.clock = NewClock(ClockOptions{Now: opts.Now})
	}
	d := db.dsn
	d.Schema = schema
	if err := s.prepare(ctx, d, sc, create); err != nil {
		return nil, errors.Join(err, s.close())
	}
	return s, nil
}

// prepare checks the server and the role, finds the schema d names,
// takes its writer's lock, migrates it and reads the store's time, for
// open. A store whose lock it cannot take is left unfenced, with
// nothing to give back.
func (s *Store) prepare(ctx context.Context, d DSN, sc schema, create bool) error {
	if err := checkServer(ctx, s.w); err != nil {
		return err
	}
	if err := checkAccount(ctx, s.w); err != nil {
		ua, ok := errors.AsType[*UnsafeAccountError](err)
		if !s.opts.AllowUnsafeAccount || !ok {
			return err
		}
		s.opts.Logger.Warn("database role is unsafe; allowed for development", "event", logs.StoreUnsafeAccount,
			"account", ua.User, "problems", strings.Join(ua.Problems, "; "))
	}
	if err := checkSchema(ctx, s.w, d, create); err != nil {
		return err
	}
	if s.fenced {
		if err := s.pool.fence.take(ctx, s.schema); err != nil {
			s.fenced = false
			return err
		}
	}
	mctx, migrated := s.begin(ctx, StageMigrate, 0)
	n, err := migrate(mctx, s.w, s.now(), sc, !s.opts.NoMigrate)
	migrated(outcomeOf(err))
	if err != nil {
		return err
	}
	s.migrated = n
	return s.initTime(ctx)
}

// Migrated returns how many migrations opening the store applied: zero
// for a database that was already at this binary's schema.
func (s *Store) Migrated() int { return s.migrated }

// Ping checks that the database answers, on a connection of the read
// pool. It is what a health check asks: cheap, and it reads no table.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.r.PingContext(ctx); err != nil {
		return fmt.Errorf("ping the database: %w", err)
	}
	return nil
}

// Close closes the connections. It is safe to call more than once; every
// call returns the first one's error.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}

// close closes the store's database/sql pools, gives its writer's lock
// up and, when the store has its DB to itself, closes that.
func (s *Store) close() error {
	err := errors.Join(s.w.Close(), s.r.Close())
	if s.fenced {
		s.pool.fence.release(s.schema)
	}
	if s.own {
		err = errors.Join(err, s.pool.Close())
	}
	return err
}

// Now reads the store's time, in UTC to the microsecond: the server
// clock (Options.Clock, or Options.Now), which the store keeps from going
// backward and from jumping forward unsettled (clock.go).
func (s *Store) Now() time.Time { return s.now() }

// Prefix returns what the issue ids the store makes start with.
func (s *Store) Prefix() string { return s.opts.Prefix }

func (s *Store) now() time.Time { return s.readTime().at }

// beginRead starts a read of several queries on one read-only
// transaction, so that they see one snapshot: a write that commits while
// the read runs shows in none of them. It returns the transaction to run
// the queries on and a func that ends it, to call once they are done.
func (s *Store) beginRead(ctx context.Context) (querier, func(), error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("begin read: %w", err)
	}
	var q querier = tx
	if s.wrapRead != nil {
		q = s.wrapRead(q)
	}
	return q, func() { _ = tx.Rollback() }, nil // read-only: nothing to keep
}

// querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// wtx is one attempt at a write transaction. Every attempt gets a fresh
// one, so none of a failed attempt's events, inbox items or idempotency
// stamp carries into the rerun.
//
// The transaction's connection serves one result set at a time: read
// rows to the end and close them before running the next statement.
type wtx struct {
	tx    *sql.Tx
	actor Actor
	// admin is set when actor's principal is one of the store's admins.
	admin bool
	// now is the attempt's server time, used for every timestamp it
	// writes.
	now time.Time
	// mutated is set once a statement changed a row; writeOnce then
	// requires an event unless quiet is set.
	mutated bool
	events  int
	// quiet allows a mutation with no event, for bookkeeping that is not
	// history: a lease renewal, a registry touch, an inbox ack, a prune.
	quiet bool
	// touched is set once a statement run by touch changed a row: a
	// change the store counts as a write, but that leaves mutated, and so
	// the event check, alone.
	touched bool
	// inbox are the items this attempt wrote, pushed once it commits.
	inbox []InboxItem
	// firstSeq is the seq flush gave the first pending event.
	firstSeq int64
	// pending are the events recorded, written by flush.
	pending []pendingEvent
	// idem is the operation's idempotency stamp, when it has a key, and
	// window how long a key gives back its result (Options.IdemWindow).
	idem   *idemStamp
	window time.Duration
	// lim are the store's limits.
	lim Limits
	// closedChanged is set when the attempt closed or reopened an issue,
	// or changed a closed one, so the similar-title cache is stale once
	// it commits.
	closedChanged bool
	// claims are the changes of a claim the attempt made, told to the
	// store's observer once it commits (observe.go).
	claims []claimChange
	// locks are the changes of a lock the attempt made, told to the
	// store's observer once it commits.
	locks []lockChange
	// gates are the changes of a gate the attempt made, for the
	// observer once it commits.
	gates []gateChange
	// usage are the tokens the attempt added, for the observer once it
	// commits.
	usage []UsageTokens
}

// exec runs a mutating statement.
func (w *wtx) exec(ctx context.Context, q string, args ...any) (int64, error) {
	n, err := w.run(ctx, q, args...)
	if n > 0 {
		w.mutated = true
	}
	return n, err
}

// touch runs a statement that is bookkeeping within a write that may
// also make history, such as refreshing the time of an issue's paths
// during a finish. Unlike setting quiet, it exempts only its own change
// from the event check: anything else the write changes still needs an
// event.
func (w *wtx) touch(ctx context.Context, q string, args ...any) error {
	n, err := w.run(ctx, q, args...)
	if n > 0 {
		w.touched = true
	}
	return err
}

// run executes q for exec and touch, and returns how many rows it
// changed.
func (w *wtx) run(ctx context.Context, q string, args ...any) (int64, error) {
	res, err := w.tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// event records an event for op on target, with the before and after
// states as JSON (nil is NULL). Every change that is history records at
// least one. Events are buffered and written in order, with the next
// gapless sequence numbers, once the operation's closure returns (flush),
// so the last can carry the operation's idempotency stamp and result.
func (w *wtx) event(_ context.Context, op Op, target string, before, after any) error {
	b, err := jsonOrNull(before)
	if err != nil {
		return err
	}
	a, err := jsonOrNull(after)
	if err != nil {
		return err
	}
	w.pending = append(w.pending, pendingEvent{op: op, target: target, before: b, after: a})
	w.events++
	return nil
}

// revEvent is event for the one event of a write that changed issue id's
// row: it records rev, the issue's rev after the write, with it, which
// is how a replayed update finds the state at the rev it was based on
// (conflicts.go). before and after are the changed fields, as diff gives
// them, or nil before a create.
func (w *wtx) revEvent(ctx context.Context, op Op, id IssueID, rev Rev, before, after any) error {
	if err := w.event(ctx, op, string(id), before, after); err != nil {
		return err
	}
	w.pending[len(w.pending)-1].rev = rev
	return nil
}

// issueEvent records op for a write that took an issue from before to
// after, with the fields it changed and extra added to the after state,
// and closes the open conflicts on those fields: a later change to a
// field decides it (design §18.7).
func (w *wtx) issueEvent(ctx context.Context, op Op, before, after Issue, extra map[string]any) error {
	b, a := diff(before, after)
	maps.Copy(a, extra)
	if err := w.revEvent(ctx, op, after.ID, after.Rev, b, a); err != nil {
		return err
	}
	return supersede(ctx, w, after.ID, b)
}

// pendingEvent is an event recorded but not yet written. rev is set on
// the event of a change to an issue's row (revEvent).
type pendingEvent struct {
	op            Op
	target        string
	before, after any
	rev           Rev
}

// flush writes the buffered events with the next sequence numbers. The
// last carries the idempotency stamp, if the operation has one.
func (w *wtx) flush(ctx context.Context) error {
	if len(w.pending) == 0 {
		if w.idem != nil && w.idem.result != nil {
			return errors.New("store: idempotent operation recorded no event")
		}
		return nil
	}
	seq, err := lastEventSeq(ctx, w.tx)
	if err != nil {
		return fmt.Errorf("next event seq: %w", err)
	}
	for i, e := range w.pending {
		seq++
		var key, args, result any
		if st := w.idem; st != nil && i == len(w.pending)-1 {
			if st.result == nil {
				return errors.New("store: idempotent operation set no result")
			}
			key, args, result = st.key, st.args, string(st.result)
		}
		q, vals := insertEvent, []any{seq, w.now, w.actor.Principal, w.actor.Session, w.actor.Machine, string(e.op),
			e.target, e.before, e.after, key, args, result}
		if e.rev > 0 {
			// Only a project's database has the column: the control
			// database's events change no issue.
			q, vals = insertRevEvent, append(vals, int64(e.rev))
		}
		if _, err := w.exec(ctx, q, vals...); err != nil {
			if key != nil && isDuplicate(err) {
				// A concurrent request with the same key committed first;
				// the rerun replays it.
				return fmt.Errorf("%w: idempotency key %q: %w", errRetry, w.idem.key, err)
			}
			return fmt.Errorf("insert event: %w", err)
		}
	}
	w.firstSeq = seq - int64(len(w.pending)) + 1
	return nil
}

// The statements that write an event, without and with the issue's rev.
const (
	insertEvent = `INSERT INTO events
  (seq, at, principal, session, machine, op, target, before_state, after_state, idem_key, idem_args, idem_result)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`
	insertRevEvent = `INSERT INTO events
  (seq, at, principal, session, machine, op, target, before_state, after_state, idem_key, idem_args, idem_result, rev)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
)

// issueEvents returns the events flush wrote on issues, without their
// states, for the event watches.
func (w *wtx) issueEvents() []Event {
	var evs []Event
	for i, e := range w.pending {
		if IssueID(e.target).Validate() == nil {
			evs = append(evs, Event{Seq: w.firstSeq + int64(i), At: w.now, Actor: w.actor, Op: e.op, Target: e.target})
		}
	}
	return evs
}

// lastEventSeq returns the highest event seq, or 0.
func lastEventSeq(ctx context.Context, q querier) (int64, error) {
	return lastKey(ctx, q, `SELECT seq FROM events ORDER BY seq DESC LIMIT 1`)
}

// lastKey runs query, which selects one integer in descending order with
// LIMIT 1, and returns it, or 0 when the table is empty.
func lastKey(ctx context.Context, q querier, query string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, query).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// sumInt returns SQL for the exact sum of the integer expression expr, as
// a BIGINT. Postgres sums a BIGINT as a NUMERIC, which is exact; a sum
// past the BIGINT range fails the cast rather than being clamped to it.
func sumInt(expr string) string {
	return "CAST(SUM(" + expr + ") AS BIGINT)"
}

// jsonOrNull encodes an event state as compacted JSON text, or returns
// nil, which is NULL, for a nil state.
func jsonOrNull(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode event state: %w", err)
	}
	if b, err = compactState(b); err != nil {
		return nil, err
	}
	return string(b), nil
}

// Event text bounds (S-10). An event keeps the before and after of what
// changed, and the log keeps every one, so without a bound an edit loop
// over four 64 KiB fields would grow the log by 256 KiB a write. A string
// longer than EventTextMax is kept as its first EventTextKeep bytes, its
// length and its SHA-256: enough to tell what changed and to check a
// copy, while the text itself lives in the issue or comment row.
const (
	EventTextMax  = 8 << 10
	EventTextKeep = 512
)

// compactState shortens every string in the JSON document b longer than
// EventTextMax, keeping it a string so readers decode it as before.
func compactState(b []byte) ([]byte, error) {
	if len(b) <= EventTextMax {
		return b, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("compact event state: %w", err)
	}
	changed := false
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if len(x) > EventTextMax {
				changed = true
				return elide(x)
			}
		case map[string]any:
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		}
		return v
	}
	v = walk(v)
	if !changed {
		return b, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("compact event state: %w", err)
	}
	return out, nil
}

// elide is s's first EventTextKeep bytes, on a rune boundary, then its
// length and SHA-256.
func elide(s string) string {
	n := EventTextKeep
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%s… [%d bytes, sha256:%s]", s[:n], len(s), hex.EncodeToString(sum[:]))
}

// write runs fn in a transaction on the writer connection as actor,
// retrying when a concurrent transaction wins (see retryable). It
// refuses an invalid actor with ErrInvalid before trying. fn must be safe
// to rerun: each attempt starts from a fresh wtx, and fn must set every
// result it hands back afresh. Between attempts write waits a random,
// growing backoff; when ctx ends first, or attempts reach
// Options.MaxAttempts, it gives up, the latter with ErrConflict.
//
// It settles a forward step of the clock before it waits for the writer,
// so that leases get the step as soon as a write knows of it, and each
// attempt looks again once its transaction has begun (writeOnce), since
// the clock may step while the write waits.
func (s *Store) write(ctx context.Context, actor Actor, fn func(*wtx) error) error {
	if err := actor.validate(); err != nil {
		return err
	}
	if err := s.SettleClock(ctx); err != nil {
		return err
	}
	return s.writeRetry(ctx, actor, false, fn)
}

// writeRetry is write for an actor already checked, without settling the
// clock first. settle is set for SettleClock's own write, which is dated
// on the time before the step it settles.
func (s *Store) writeRetry(ctx context.Context, actor Actor, settle bool, fn func(*wtx) error) error {
	start := time.Now()
	for attempt := 1; ; attempt++ {
		actx, attempted := s.begin(ctx, StageAttempt, attempt)
		changed, err := s.writeOnce(actx, actor, settle, fn)
		attempted(outcomeOf(err))
		if err == nil {
			if changed {
				took := time.Since(start)
				s.stats.record(time.Now(), took)
				if o := s.opts.Observer; o != nil {
					o.Wrote(took)
				}
			}
			return nil
		}
		if !retryable(err) {
			return err
		}
		if attempt >= s.opts.MaxAttempts {
			return fmt.Errorf("%w: gave up after %d attempts: %w", ErrConflict, attempt, err)
		}
		if o := s.opts.Observer; o != nil {
			o.Retried()
		}
		if s.beforeRetry != nil {
			if err := s.beforeRetry(ctx); err != nil {
				return err
			}
		}
		d := time.Duration(rand.Int64N(int64(5*time.Millisecond) << min(attempt, 5))) //nolint:gosec // jitter, not security
		select {
		case <-ctx.Done():
			return fmt.Errorf("write retry: %w", ctx.Err())
		case <-time.After(d):
		}
	}
}

// beginWrite waits for the writer, begins a transaction on it and then
// reads the store's time: the attempt's time, and whether a forward step
// of the clock is pending now that the wait is over.
func (s *Store) beginWrite(ctx context.Context) (*sql.Tx, view, error) {
	if s.opts.ReadOnly {
		return nil, view{}, ErrReadOnly
	}
	// The writer is one connection at a time, so this is where a write
	// waits for the writes ahead of it.
	_, waited := s.begin(ctx, StageWait, 0)
	tx, err := s.w.BeginTx(ctx, nil)
	waited(outcomeOf(err))
	if err != nil {
		return nil, view{}, fmt.Errorf("begin: %w", err)
	}
	if s.fenced {
		if err := s.pool.fence.verify(ctx, tx, s.schema); err != nil {
			return nil, view{}, errors.Join(err, rollback(tx))
		}
	}
	return tx, s.readTime(), nil
}

// writeOnce makes one attempt: it runs fn in a new transaction, refuses a
// mutation that recorded no event unless the attempt is quiet, writes the
// buffered events and commits. Only once the commit succeeds does it
// invalidate the similar-title cache and push inbox
// items and issue events, so a rolled-back attempt leaves no trace.
//
// No attempt goes on with a forward step of the clock pending, but the
// settle's own (settle). A write settles the clock before it waits for
// the writer, and the clock may step while it waits, or between two
// attempts; dated on the time before the step, it would put what it
// records before what caused it in another store's log. So the attempt
// gives the writer up, settles, and begins again on the time after the
// step. If the clock has stepped yet again by then, the attempt ends as
// one that lost to a concurrent transaction does, and write's bound on
// attempts is the bound on this: a clock that steps at every attempt gets
// the write refused, and never an event dated before a step.
func (s *Store) writeOnce(ctx context.Context, actor Actor, settle bool, fn func(*wtx) error) (changed bool, err error) {
	tx, v, err := s.beginWrite(ctx)
	if err != nil {
		return false, err
	}
	if v.pending > 0 && !settle {
		// Rolled back first: the settle is a write, and waits for the
		// writer this attempt holds.
		if err := rollback(tx); err != nil {
			return false, err
		}
		if err := s.SettleClock(ctx); err != nil {
			return false, err
		}
		if tx, v, err = s.beginWrite(ctx); err != nil {
			return false, err
		}
		if v.pending > 0 {
			return false, errors.Join(fmt.Errorf("%w: the clock stepped forward again while a step was settled", errRetry), rollback(tx))
		}
	}
	w := &wtx{tx: tx, actor: actor, admin: s.IsAdmin(actor.Principal), now: v.at, lim: s.opts.Limits, window: s.opts.IdemWindow}
	if err := fn(w); err != nil {
		return false, errors.Join(err, rollback(tx))
	}
	if w.mutated && w.events == 0 && !w.quiet {
		return false, errors.Join(errors.New("store: mutation without an event"), rollback(tx))
	}
	if err := w.flush(ctx); err != nil {
		return false, errors.Join(err, rollback(tx))
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(ctx); err != nil {
			return false, errors.Join(err, rollback(tx))
		}
	}
	if err := tx.Commit(); err != nil {
		return false, commitErr(err)
	}
	changed = w.mutated || w.touched
	if w.closedChanged {
		s.similar.invalidate()
	}
	if o := s.opts.Observer; o != nil {
		for _, c := range w.claims {
			o.ClaimChanged(c.change, c.reason)
		}
		for _, c := range w.locks {
			o.LockChanged(c.change, c.waited)
		}
		for _, c := range w.gates {
			o.GateChanged(ctx, actor, c.gate, c.change)
		}
		if len(w.usage) > 0 {
			o.UsageAdded(ctx, actor, w.usage)
		}
	}
	// Counted after the commit: every watch subscribed before it is
	// counted, and one subscribed later is not owed this write's events.
	var evs []Event
	if s.watch.events.Load() > 0 {
		evs = w.issueEvents()
	}
	if len(w.inbox) > 0 || len(evs) > 0 {
		s.publish(w.inbox, evs)
	}
	return changed, nil
}

// commitErr says what a failed COMMIT means. An error Postgres answered
// with (a SQLSTATE) is a commit that did not happen, and a serialization
// failure among them is retried as any is. Any other error, a context
// that ended or a connection that broke, leaves the outcome unknown:
// pgx gives up on the connection without telling Postgres, which may
// finish the commit all the same. That is ErrCommitUnknown, never
// retried, since the write may stand. database/sql also refuses to send
// COMMIT for a context that has already ended, which is known not to
// have applied; it returns the same context error, so it is reported as
// unknown too, which is never wrong: unknown allows that it did not.
func commitErr(err error) error {
	if sqlState(err) != "" {
		return fmt.Errorf("commit: %w", err)
	}
	return fmt.Errorf("%w: %w", ErrCommitUnknown, err)
}

// rollback rolls tx back; a transaction already done is not an error.
func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}
