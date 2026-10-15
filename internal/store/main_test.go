package store

import (
	"errors"
	"os"
	"testing"

	"github.com/ariesworx/aries/internal/pgtest"
)

// pgs are the package's Postgres servers: a test has one to itself while
// it runs, so the tests can run in parallel (pgtest.Pool).
var pgs = pgtest.NewPool("aries-store-")

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	defer func() { _ = pgs.Close() }()
	pgs.Warm()
	return m.Run()
}

// pgOf returns the Postgres server t's test holds, or skips when
// Postgres is missing.
func pgOf(t testing.TB) *pgtest.Server {
	t.Helper()
	srv, err := pgs.Server(t)
	if errors.Is(err, pgtest.ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the store integration tests")
	}
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	return srv
}

// newDSN returns a fresh database on the test's Postgres server, owned by
// the locked-down role its DSN names, or skips when Postgres is missing.
// The database is dropped when the test ends, after the cleanups of what
// the test opened on it.
func newDSN(t testing.TB) string {
	t.Helper()
	pgOf(t)
	dsn, err := pgs.Database(t)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

func openStore(t testing.TB, dsn string, opts Options) *Store {
	t.Helper()
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
