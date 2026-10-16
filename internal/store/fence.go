package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// ServedError refuses a project's schema whose writer's lock another
// process holds: another ariesd on the same database, or an operator's
// command such as `ariesd import-bd` while ariesd serves the project.
// Its message ends with the fix.
type ServedError struct {
	Schema string
}

func (e *ServedError) Error() string {
	return fmt.Sprintf("schema %s is served by another process on this database, which holds its writer's lock: another ariesd, or an operator's command writing it; fix: stop that process first, or give each ariesd a database or a control_schema: of its own", e.Schema)
}

// ErrReadOnly refuses a write to a store opened with Options.ReadOnly.
var ErrReadOnly = fmt.Errorf("%w: the store was opened to read only", ErrInvalid)

// writerKey is the advisory lock that makes a process schema's writer.
func writerKey(schema string) int64 { return lockKey("aries writer\x00" + schema) }

// barrierKey is the advisory lock every write to schema holds for its
// transaction.
func barrierKey(schema string) int64 { return lockKey("aries write\x00" + schema) }

// lockKey derives a 64-bit advisory lock key from name.
func lockKey(name string) int64 {
	sum := sha256.Sum256([]byte(name))
	return int64(binary.BigEndian.Uint64(sum[:8])) //nolint:gosec // a key, any bits will do
}

// The writer's lock (decision 79, slice P2; spike/postgres/RESULTS.md,
// 4.1). Rule 5 gives each project's schema one writer. Within a process
// the daemon's registry keeps to it; between processes on one database,
// a session advisory lock on a key derived from the schema does: a DB
// takes it before a store of the schema migrates or writes anything,
// holds it while the store is open, and gives it up when the store
// closes. Postgres gives it up by itself when the session ends, so a
// process that dies, or loses its connection, frees the schema for
// another.
//
// The locks are a session's, and a store's writes take a connection from
// the pool for each transaction, so the DB holds every lock on one
// connection of its own, the fence, and each write proves in its
// transaction that the fence still holds its schema's lock, in one
// statement:
//
//  1. It takes a transaction lock on the schema's barrier key, which
//     Postgres holds until the transaction ends.
//  2. It tries to take, shared, the fence's own key: a random one the
//     fence takes with each connection, which no other session ever
//     does. Refused, the fence's session is alive, and holds every
//     lock it took, since only the end of the session gives one up
//     unasked. Granted, the session is gone, and its locks with it.
//
// A process that takes a schema over from one whose fence died takes the
// barrier with its first write, and so waits for the write the other had
// in flight, which had passed step 2 before the fence died. Every later
// write of the other fails step 2. So two processes never write a schema
// at once, not even for one transaction. (Reading pg_locks for the
// fence's locks instead cost 60µs a write, measured, against 10µs for
// this.)
//
// A write that finds its lock gone, because the fence's connection died,
// connects again and takes back the locks of every schema the DB serves,
// then tries again; one that another process has taken meanwhile is
// refused with a [*ServedError], and so is every write after, until the
// store is closed. The store's reads go on: they need no lock.
//
// The control store's writer takes its schema's lock as a project's does,
// so a second daemon on the same control store is refused at start. The
// operator's `ariesd control init` takes none: it writes the control
// store beside a running daemon, as docs/server.md says it may, and the
// daemon's index follows its event log. Stores opened to read only take
// none either (Options.ReadOnly).

// FenceApplication is the application_name of the connection that holds
// a DB's writers' locks, by which pg_stat_activity shows it to an
// operator looking for who serves a schema.
const FenceApplication = "ariesd writer locks"

// fence holds the writers' locks of a DB's schemas on one connection.
// Its methods are safe for concurrent use.
type fence struct {
	cfg *pgx.ConnConfig

	mu sync.Mutex
	// conn holds the locks, and own is the key it holds as its own; nil
	// until the first take.
	conn *pgx.Conn
	own  int64
	// held counts the stores of each schema that hold its lock; lost
	// names those whose lock another process took while the fence was
	// gone.
	held map[string]int
	lost map[string]bool
}

func newFence(cfg *pgx.ConnConfig) *fence {
	cfg.RuntimeParams["application_name"] = FenceApplication
	return &fence{cfg: cfg, held: map[string]int{}, lost: map[string]bool{}}
}

// take makes the DB schema's writer, or refuses with a [*ServedError]
// when another process is. A schema the DB already writes is counted
// once more, and given up when the last of its stores releases it.
func (f *fence) take(ctx context.Context, schema string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[schema] > 0 && !f.lost[schema] {
		f.held[schema]++
		return nil
	}
	if err := f.connect(ctx); err != nil {
		return err
	}
	ok, err := f.try(ctx, schema)
	if err != nil {
		return err
	}
	if !ok {
		return &ServedError{Schema: schema}
	}
	f.held[schema]++
	delete(f.lost, schema)
	return nil
}

// try takes schema's lock on the fence's connection, reporting whether
// it could. f.mu is held.
func (f *fence) try(ctx context.Context, schema string) (bool, error) {
	var ok bool
	if err := f.conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, writerKey(schema)).Scan(&ok); err != nil {
		return false, fmt.Errorf("take the writer's lock of schema %s: %w", schema, err)
	}
	return ok, nil
}

// connect makes the fence's connection when it has none, or when the one
// it has no longer answers, and takes back on it the lock of every
// schema the DB holds, noting in f.lost those another process took
// meanwhile. f.mu is held.
func (f *fence) connect(ctx context.Context) error {
	if f.conn != nil && !f.conn.IsClosed() && f.conn.Ping(ctx) == nil {
		return nil
	}
	if f.conn != nil {
		_ = f.conn.Close(ctx) // gone already; its locks went with it
		f.conn = nil
	}
	conn, err := pgx.ConnectConfig(ctx, f.cfg)
	if err != nil {
		return fmt.Errorf("connect for the writers' locks: %w", err)
	}
	own := lockKey(rand.Text())
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, own); err != nil {
		return errors.Join(fmt.Errorf("take the writers' locks' own key: %w", err), conn.Close(ctx))
	}
	f.conn, f.own = conn, own
	for schema := range f.held {
		ok, err := f.try(ctx, schema)
		if err != nil {
			// The connection must hold every lock the DB has, or be
			// none: a write checks only that it is alive.
			f.conn = nil
			return errors.Join(err, conn.Close(ctx))
		}
		if ok {
			delete(f.lost, schema)
		} else {
			f.lost[schema] = true
		}
	}
	return nil
}

// release gives up one store's hold of schema's lock, and the lock with
// the last.
func (f *fence) release(schema string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.held[schema]--; f.held[schema] > 0 {
		return
	}
	delete(f.held, schema)
	lost := f.lost[schema]
	delete(f.lost, schema)
	if lost || f.conn == nil || f.conn.IsClosed() {
		return
	}
	// A lock that cannot be given up goes with the connection, which
	// is closed so: another process must not wait on a schema nobody
	// here serves.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := f.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, writerKey(schema)); err != nil {
		_ = f.conn.Close(ctx)
		f.conn = nil
	}
}

// verify checks, in tx, a write's transaction, that the DB is still
// schema's writer, as the comment above says. A lock whose connection
// died it takes back and asks for the write to be tried again; one
// another process took it refuses with a [*ServedError].
func (f *fence) verify(ctx context.Context, tx *sql.Tx, schema string) error {
	f.mu.Lock()
	own, lost := f.own, f.lost[schema]
	f.mu.Unlock()
	if lost {
		return &ServedError{Schema: schema}
	}
	// CASE evaluates its condition first, so the barrier is taken before
	// the fence is looked at. pg_advisory_xact_lock returns void, which
	// is not NULL.
	var alive bool
	if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN pg_advisory_xact_lock($1) IS NULL THEN NULL
  ELSE NOT pg_try_advisory_xact_lock_shared($2) END`, barrierKey(schema), own).Scan(&alive); err != nil {
		return fmt.Errorf("check the writer's lock of schema %s: %w", schema, err)
	}
	if alive {
		return nil
	}
	// The fence's connection died, and its locks with it. Its write is
	// rolled back first by the caller; this takes the locks back.
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.connect(ctx); err != nil {
		return err
	}
	if f.lost[schema] {
		return &ServedError{Schema: schema}
	}
	return fmt.Errorf("%w: the writer's lock of schema %s was taken back", errRetry, schema)
}

// close gives up every lock the fence holds, with its connection.
func (f *fence) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conn == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := f.conn.Close(ctx)
	f.conn = nil
	if err != nil {
		return fmt.Errorf("close the writers' locks' connection: %w", err)
	}
	return nil
}
