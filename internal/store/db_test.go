package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The server's one pool and its writer's locks (db.go): stores in many
// schemas share one DB's connections, and a project's schema has one
// writer however many processes share the database.

// openDB opens a DB on dsn's database for t, closed when t ends, after
// the stores t opened on it.
func openDB(t testing.TB, dsn string, opts DBOptions) *DB {
	t.Helper()
	db, err := OpenDB(t.Context(), dsn, opts)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the DB: %v", err)
		}
	})
	return db
}

// openIn opens a store of schema on db for t, closed when t ends.
func openIn(t testing.TB, db *DB, schema string, opts Options) *Store {
	t.Helper()
	if opts.Prefix == "" {
		opts.Prefix = "tst"
	}
	s, err := db.Open(t.Context(), schema, opts)
	if err != nil {
		t.Fatalf("Open(%s): %v", schema, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close %s: %v", schema, err)
		}
	})
	return s
}

// makeSchema makes schema in dsn's database, as `aries admin project
// add` does before it opens a project's store.
func makeSchema(t testing.TB, dsn, schema string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+quoteIdent(schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
}

// backends counts the connections to dsn's database other than the
// asker's own.
func backends(t testing.TB, dsn string) int {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pg_stat_activity
  WHERE datname = current_database() AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStoresShareOnePool(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	const projects, conns = 6, 2
	db := openDB(t, dsn, DBOptions{Conns: conns})
	stores := make([]*Store, projects)
	for i := range stores {
		schema := fmt.Sprintf("p%d", i)
		makeSchema(t, dsn, schema)
		stores[i] = openIn(t, db, schema, Options{Prefix: fmt.Sprintf("p%d", i)})
	}
	// Every store writes and reads at once, through the one pool.
	var wg sync.WaitGroup
	errs := make(chan error, projects*10)
	for i, s := range stores {
		wg.Go(func() {
			for j := range 5 {
				if _, err := s.CreateIssue(t.Context(), alice, NewIssue{Title: fmt.Sprintf("p%d issue %d", i, j)}); err != nil {
					errs <- err
				}
				if _, err := s.List(t.Context(), Filter{}); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a store's request through the shared pool: %v", err)
	}
	// Each schema holds its own project's issues and no other's.
	for i, s := range stores {
		page, err := s.List(t.Context(), Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Issues) != 5 {
			t.Errorf("project p%d lists %d issues, want its own 5", i, len(page.Issues))
		}
		for _, is := range page.Issues {
			if want := fmt.Sprintf("p%d-", i); len(is.ID) < len(want) || string(is.ID[:len(want)]) != want {
				t.Errorf("project p%d lists %s, which is another project's", i, is.ID)
			}
		}
	}
	// The pool's connections, and the one that holds the writers'
	// locks: no more however many stores are open.
	if n := backends(t, dsn); n > conns+1 {
		t.Errorf("%d stores hold %d connections, want at most %d (the pool and the lock holder)", projects, n, conns+1)
	}
}

// A store's write that waits for a connection while every one of a pool
// of one is in use by reads finishes once they do: writes and reads of
// many schemas share the pool without a deadlock.
func TestPoolOfOneServesEveryStore(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	db := openDB(t, dsn, DBOptions{Conns: 1})
	makeSchema(t, dsn, "a")
	makeSchema(t, dsn, "b")
	a := openIn(t, db, "a", Options{})
	b := openIn(t, db, "b", Options{})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range []*Store{a, b, a, b} {
		wg.Go(func() {
			for range 5 {
				if _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "t"}); err != nil {
					t.Errorf("create: %v", err)
					return
				}
				if _, err := s.Ready(ctx, alice, 10); err != nil {
					t.Errorf("ready: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
}

func TestWriterLockRefusesASecondDB(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	first, second := openDB(t, dsn, DBOptions{}), openDB(t, dsn, DBOptions{})
	makeSchema(t, dsn, "web")
	s, err := first.Open(t.Context(), "web", Options{Prefix: "tst"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = second.Open(t.Context(), "web", Options{Prefix: "tst"})
	if se, ok := errors.AsType[*ServedError](err); !ok || se.Schema != "web" {
		t.Fatalf("second DB's Open(web) while the first serves it = %v, want a *ServedError for web", err)
	}
	// Another schema is free.
	makeSchema(t, dsn, "api")
	openIn(t, second, "api", Options{})
	// Closing the first's store gives the schema up.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	openIn(t, second, "web", Options{})
}

// Two stores of one schema on one DB are one process's: the lock is the
// process's, and the registry keeps a daemon to one store a project. The
// lock is held until the last of them closes.
func TestWriterLockIsTheDBs(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	db, other := openDB(t, dsn, DBOptions{}), openDB(t, dsn, DBOptions{})
	makeSchema(t, dsn, "web")
	one, err := db.Open(t.Context(), "web", Options{Prefix: "tst"})
	if err != nil {
		t.Fatal(err)
	}
	two := openIn(t, db, "web", Options{})
	if err := one.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(t.Context(), "web", Options{Prefix: "tst"}); err == nil {
		t.Fatal("another DB opened web while a store of it was still open")
	}
	mustCreate(t, two, NewIssue{})
}

// lockParts is how pg_locks shows an advisory lock taken on one bigint
// key: its high half as classid and its low half as objid, with objsubid
// 1.
func lockParts(key int64) (hi, lo int64) {
	return int64(uint64(key) >> 32), int64(uint64(key) & 0xffffffff) //nolint:gosec // halves of a key
}

// killLockHolder ends the connection that holds schema's writer's lock,
// as a lost network or a restarted Postgres would.
func killLockHolder(t *testing.T, dsn, schema string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	hi, lo := lockParts(writerKey(schema))
	var ok bool
	if err := db.QueryRowContext(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_locks
  WHERE locktype = 'advisory' AND granted AND classid = $1 AND objid = $2 AND objsubid = 1`, hi, lo).Scan(&ok); err != nil || !ok {
		t.Fatalf("end the holder of %s's lock: %v, %v", schema, ok, err)
	}
	// The lock goes when the backend has gone.
	waitFor(t, dsn, `SELECT NOT EXISTS (SELECT 1 FROM pg_locks
  WHERE locktype = 'advisory' AND classid = $1 AND objid = $2 AND objsubid = 1)`, hi, lo)
}

// waitFor polls Postgres until query, which selects one boolean, is
// true: for what another process does, which nothing here can wait on.
func waitFor(t *testing.T, dsn, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for range 1000 {
		var ok bool
		if err := db.QueryRowContext(t.Context(), query, args...).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("still false after 10s: %s", query)
}

func TestWriterLockTakenBackAfterItsConnectionDies(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	db := openDB(t, dsn, DBOptions{})
	makeSchema(t, dsn, "web")
	s := openIn(t, db, "web", Options{})
	killLockHolder(t, dsn, "web")
	// Nobody took it meanwhile: the next write takes it back and goes
	// on.
	mustCreate(t, s, NewIssue{Title: "after the lock's connection died"})
	if _, err := openDB(t, dsn, DBOptions{}).Open(t.Context(), "web", Options{Prefix: "tst"}); err == nil {
		t.Fatal("another DB opened web after the first took its lock back")
	}
}

func TestWriterLockLostToAnotherDBRefusesWrites(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	first, second := openDB(t, dsn, DBOptions{}), openDB(t, dsn, DBOptions{})
	makeSchema(t, dsn, "web")
	s := openIn(t, first, "web", Options{})
	mustCreate(t, s, NewIssue{})
	killLockHolder(t, dsn, "web")
	// The second takes the schema while the first's lock is gone.
	rival := openIn(t, second, "web", Options{})
	_, err := s.CreateIssue(t.Context(), alice, NewIssue{Title: "from the first"})
	if se, ok := errors.AsType[*ServedError](err); !ok || se.Schema != "web" {
		t.Fatalf("the first's write after it lost web's lock = %v, want a *ServedError for web", err)
	}
	mustCreate(t, rival, NewIssue{Title: "from the second"})
	// The first's reads still answer.
	if _, err := s.List(t.Context(), Filter{}); err != nil {
		t.Errorf("the first's read after it lost the lock: %v", err)
	}
}

// A write that had checked the lock when the lock's connection died
// commits before the store that takes the schema over writes anything:
// the two never write at once.
func TestWriterTakingOverWaitsForTheWriteInFlight(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	first, second := openDB(t, dsn, DBOptions{}), openDB(t, dsn, DBOptions{})
	makeSchema(t, dsn, "web")
	s := openIn(t, first, "web", Options{})
	inFlight, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.beforeCommit = func(context.Context) error {
		once.Do(func() { close(inFlight); <-resume })
		return nil
	}
	var firstSeq int64
	var wg sync.WaitGroup
	wg.Go(func() {
		is, err := s.CreateIssue(t.Context(), alice, NewIssue{Title: "in flight"})
		if err != nil {
			t.Errorf("the write in flight: %v", err)
			return
		}
		evs, err := s.Events(t.Context(), 0, 100)
		if err != nil {
			t.Error(err)
			return
		}
		for _, e := range evs {
			if e.Target == string(is.ID) {
				firstSeq = e.Seq
			}
		}
	})
	<-inFlight
	killLockHolder(t, dsn, "web")
	rival := openIn(t, second, "web", Options{})
	done := make(chan Issue)
	wg.Go(func() {
		is, err := rival.CreateIssue(t.Context(), bob, NewIssue{Title: "after the take-over"})
		if err != nil {
			t.Errorf("the second's write: %v", err)
		}
		done <- is
	})
	// The second's write waits on the first's, in Postgres's lock table.
	hi, lo := lockParts(barrierKey("web"))
	waitFor(t, dsn, `SELECT EXISTS (SELECT 1 FROM pg_locks
  WHERE locktype = 'advisory' AND NOT granted AND classid = $1 AND objid = $2 AND objsubid = 1)`, hi, lo)
	select {
	case <-done:
		t.Fatal("the second wrote while the first's write was still in flight")
	default:
	}
	close(resume)
	<-done
	wg.Wait()
	evs, err := rival.Events(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(evs); n != 2 || evs[0].Seq != firstSeq {
		t.Errorf("events after the take-over = %+v, want the first's (seq %d) and then the second's", evs, firstSeq)
	}
}

func TestReadOnlyStoreTakesNoLock(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	first, second := openDB(t, dsn, DBOptions{}), openDB(t, dsn, DBOptions{})
	makeSchema(t, dsn, "web")
	s := openIn(t, first, "web", Options{})
	is := mustCreate(t, s, NewIssue{Title: "seen by a reader"})
	ro := openIn(t, second, "web", Options{ReadOnly: true})
	if got, err := ro.GetIssue(t.Context(), is.ID); err != nil || got.Title != is.Title {
		t.Errorf("read-only GetIssue(%s) = %+v, %v, want the issue", is.ID, got, err)
	}
	if _, err := ro.CreateIssue(t.Context(), alice, NewIssue{Title: "x"}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("read-only CreateIssue = %v, want ErrReadOnly", err)
	}
}

// endBackends ends every connection to dsn's database but the asker's
// and the writers' locks', as a restart of Postgres ends the pool's,
// and waits until they are gone.
func endBackends(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	const others = `datname = current_database() AND pid <> pg_backend_pid() AND application_name <> $1`
	_, err = db.ExecContext(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE `+others,
		FenceApplication)
	_ = db.Close()
	if err != nil {
		t.Fatalf("end the pool's connections: %v", err)
	}
	waitFor(t, dsn, `SELECT NOT EXISTS (SELECT 1 FROM pg_stat_activity WHERE `+others+`)`, FenceApplication)
}

// A connection Postgres ended while it was idle in the pool, as a
// restart ends them all, is not handed to a store: the first reads and
// writes after it succeed, rather than failing with SQLSTATE 57P01. The
// pool pings a connection idle for more than a second already, so this
// is about those used just before.
func TestPoolHandsOutNoConnectionPostgresEnded(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	db := openDB(t, dsn, DBOptions{Conns: 4})
	makeSchema(t, dsn, "web")
	s := openIn(t, db, "web", Options{})
	is := mustCreate(t, s, NewIssue{Title: "before the restart"})
	// Every connection the pool may hold is in it, idle.
	var held []interface{ Release() }
	for range 4 {
		c, err := db.pool.Acquire(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		c.Release()
	}
	endBackends(t, dsn)
	ctx := t.Context()
	if err := s.TouchAgent(ctx, alice, "claude-code"); err != nil {
		t.Errorf("TouchAgent after Postgres ended the pool's connections: %v", err)
	}
	if _, err := s.GetIssue(ctx, is.ID); err != nil {
		t.Errorf("GetIssue(%s) after Postgres ended the pool's connections: %v", is.ID, err)
	}
	if _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "after the restart"}); err != nil {
		t.Errorf("CreateIssue after Postgres ended the pool's connections: %v", err)
	}
	if _, err := s.List(ctx, Filter{}); err != nil {
		t.Errorf("List after Postgres ended the pool's connections: %v", err)
	}
}
