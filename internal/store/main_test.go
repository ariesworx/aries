package store

import (
	"errors"
	"os"
	"sync"
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

// testDBs are the DBs openStore opened, one for each test database, as a
// daemon has one for its database: the stores a test opens on one
// database share its pool and its writers' locks, so that the test can
// race several stores of one schema as one process's rivals. Each is
// closed when the test that opened it ends, after its stores.
var testDBs = struct {
	sync.Mutex
	m map[string]*DB
}{m: map[string]*DB{}}

// testDB returns the DB of dsn's database for t, opening it if no test
// has it open.
func testDB(t testing.TB, dsn string) *DB {
	t.Helper()
	testDBs.Lock()
	defer testDBs.Unlock()
	if db := testDBs.m[dsn]; db != nil {
		return db
	}
	db, err := OpenDB(t.Context(), dsn, DBOptions{})
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	testDBs.m[dsn] = db
	t.Cleanup(func() {
		testDBs.Lock()
		delete(testDBs.m, dsn)
		testDBs.Unlock()
		if err := db.Close(); err != nil {
			t.Errorf("close the DB: %v", err)
		}
	})
	return db
}

func openStore(t testing.TB, dsn string, opts Options) *Store {
	t.Helper()
	if opts.Prefix == "" {
		opts.Prefix = "tst"
	}
	if opts.Admins == nil {
		opts.Admins = []string{"dana"}
	}
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	s, err := testDB(t, dsn).Open(t.Context(), d.Schema, opts)
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
