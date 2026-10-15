package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/spike/postgres/pgtest"
)

// dolts are the package's Dolt servers: a test has one to itself while it
// runs, so the tests can run in parallel (dolttest.Pool).
var dolts = dolttest.NewPool("starfix-dolt-", dolttest.Options{})

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// backend is STARFIX_BACKEND: "" or "dolt", "postgres" (a database per
// test) or "postgres-schema" (a schema per test in one database).
var backend = os.Getenv("STARFIX_BACKEND")

func onPostgres() bool { return strings.HasPrefix(backend, "postgres") }

// pg is the Postgres server when the backend is one.
var pg *pgtest.Server

func run(m *testing.M) int {
	if onPostgres() {
		var err error
		if pg, err = pgtest.Start(context.Background(), pgtest.Options{}); err != nil {
			panic(err)
		}
		defer func() { _ = pg.Stop() }()
		return m.Run()
	}
	defer func() { _ = dolts.Close() }()
	dolts.Warm()
	return m.Run()
}

// doltOf returns the Dolt server t's test holds, or skips when dolt is
// missing.
func doltOf(t testing.TB) *dolttest.Server {
	t.Helper()
	if onPostgres() {
		t.Skip("spike: the test drives the Dolt server itself")
	}
	srv, err := dolts.Server(t)
	if errors.Is(err, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the store integration tests")
	}
	if err != nil {
		t.Fatalf("dolt sql-server: %v", err)
	}
	return srv
}

// newDSN returns a fresh database on the test's Dolt server, or skips
// when dolt is missing. The database is dropped when the test ends, after
// the cleanups of what the test opened on it: Dolt holds every database
// in memory and slows with each (dolttest.Server.DropDatabase).
func newDSN(t testing.TB) string {
	t.Helper()
	if onPostgres() {
		make := pg.Database
		if backend == "postgres-schema" {
			make = pg.Schema
		}
		dsn, err := make(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pg.Drop(context.Background(), dsn) })
		return dsn
	}
	doltOf(t)
	dsn, err := dolts.Database(t)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

func openStore(t testing.TB, dsn string, opts Options) *Store {
	t.Helper()
	if opts.CommitInterval == 0 {
		opts.CommitInterval = -1
	}
	if opts.Prefix == "" {
		opts.Prefix = "tst"
	}
	if opts.Admins == nil {
		opts.Admins = []string{"dana"}
	}
	s, err := Open(t.Context(), dsn, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func newStore(t *testing.T) *Store {
	t.Helper()
	return openStore(t, newDSN(t), Options{})
}

var (
	alice = Actor{Principal: "alice", Session: "sess-a", Machine: "laptop-a"}
	bob   = Actor{Principal: "bob", Session: "sess-b", Machine: "laptop-b"}
)

func mustCreate(t *testing.T, s *Store, in NewIssue) Issue {
	t.Helper()
	if in.Title == "" {
		in.Title = "issue"
	}
	is, err := s.CreateIssue(t.Context(), alice, in)
	if err != nil {
		t.Fatalf("create %q: %v", in.Title, err)
	}
	return is
}

func prio(p Priority) *Priority { return &p }
func ptr[T any](v T) *T         { return &v }

func lastSeq(t *testing.T, s *Store) int64 {
	t.Helper()
	n, err := lastEventSeq(t.Context(), s.r)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// barrier holds the first k write transactions that reach it until all k
// have, so they overlap. Later transactions (retries) pass straight through.
type barrier struct {
	k       int32
	arrived atomic.Int32
	wg      sync.WaitGroup
}

func newBarrier(k int) *barrier {
	b := &barrier{k: int32(k)} //nolint:gosec // small test constant
	b.wg.Add(k)
	return b
}

func (b *barrier) hook(context.Context) error {
	if b.arrived.Add(1) > b.k {
		return nil
	}
	b.wg.Done()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("barrier: the other transaction never arrived")
	}
}
