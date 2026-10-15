package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/version"
)

// shortDir makes a directory for a server, removed when t ends. Not
// t.TempDir, whose path has the test's name in it: with the server's
// socket it can pass the 103 bytes a unix socket's path may be on macOS,
// under a temporary directory with one more component
// (scripts/leftovers.sh).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sfd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startOrSkip starts a server for this package's own tests. It skips
// without Postgres unless ARIES_REQUIRE_POSTGRES is set, as every package
// whose tests need it does.
func startOrSkip(t *testing.T) *Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s, err := Start(ctx, shortDir(t))
	if errors.Is(err, ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the pgtest tests")
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

// open opens dsn, closed when t ends.
func open(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestStartWithoutPostgres(t *testing.T) {
	// Serial: sets the process's environment (t.Setenv).
	tests := []struct {
		name    string
		require string
		skip    bool // the error is ErrNoPostgres, so callers skip
	}{
		{name: "not required", require: "", skip: true},
		{name: "required", require: "1", skip: false},
		{name: "explicitly not required", require: "0", skip: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			t.Setenv(RequireEnv, tc.require)
			_, err := Start(t.Context(), t.TempDir())
			if err == nil {
				t.Fatal("Start without postgres on PATH succeeded")
			}
			if got := errors.Is(err, ErrNoPostgres); got != tc.skip {
				t.Errorf("Start with %s=%q: errors.Is(%v, ErrNoPostgres) = %v, want %v", RequireEnv, tc.require, err, got, tc.skip)
			}
			if !tc.skip && !strings.Contains(err.Error(), RequireEnv) {
				t.Errorf("Start with %s=%q: error %q does not name %s", RequireEnv, tc.require, err, RequireEnv)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		out, want string
		ok        bool
	}{
		{"postgres (PostgreSQL) 18.6\n", "18.6", true},
		{"postgres (PostgreSQL) 18.6 (Ubuntu 18.6-1.pgdg24.04+1)\n", "18.6", true},
		{"postgres (PostgreSQL) 16.15 (Debian 16.15-1.pgdg13+1)", "16.15", true},
		{"postgres (PostgreSQL)", "", false},
		{"dolt version 2.4.2", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, err := parseVersion(tc.out)
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("parseVersion(%q) = %q, %v; want %q and ok %v", tc.out, got, err, tc.want, tc.ok)
		}
	}
}

// TestPostgresVersion checks that the Postgres the tests run against is
// of the major version this release names (version.PostgresMajor, and
// CI's POSTGRES_MAJOR). The minor is not held: CI installs the newest of
// the major version from the PostgreSQL project's apt repository.
func TestPostgresVersion(t *testing.T) {
	t.Parallel()
	got, err := Version(t.Context())
	if errors.Is(err, ErrNoPostgres) {
		t.Skip("postgres is not on PATH")
	}
	if err != nil {
		t.Fatal(err)
	}
	if major, _, _ := strings.Cut(got, "."); major != version.PostgresMajor {
		t.Errorf("Version() = %q, want Postgres %s (version.PostgresMajor); scripts/install-postgres.sh installs version.Postgres %s and prints the directory to put first on PATH",
			got, version.PostgresMajor, version.Postgres)
	}
}

// A directory whose path leaves no room for the socket's is refused
// before anything is started in it.
func TestStartRefusesALongDirectory(t *testing.T) {
	t.Parallel()
	if _, err := lookPostgres(); errors.Is(err, ErrNoPostgres) {
		t.Skip("postgres is not on PATH")
	}
	dir := filepath.Join(shortDir(t), strings.Repeat("d", 100))
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if s, err := Start(t.Context(), dir); err == nil {
		_ = s.Stop()
		t.Fatalf("Start(%s) succeeded, want the path refused as too long for a socket", dir)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("Start of a refused directory left %v in it", entries)
	}
}

// TestRestartKeepsData checks that Restart brings up a new postmaster on
// the same data, and that a DSN handed out before it still works after.
func TestRestartKeepsData(t *testing.T) {
	t.Parallel()
	s := startOrSkip(t)
	ctx := t.Context()
	dsn, err := s.NewDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db := open(t, dsn)
	for _, q := range []string{"CREATE TABLE kept (id INT PRIMARY KEY)", "INSERT INTO kept VALUES (7)"} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	before := s.PID()
	if before <= 0 {
		t.Fatalf("PID() = %d before Restart, want a process id", before)
	}
	if err := s.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := s.PID(); got == before || got <= 0 {
		t.Errorf("PID() = %d after Restart, want a new process, not %d", got, before)
	}
	var id int
	if err := db.QueryRowContext(ctx, "SELECT id FROM kept").Scan(&id); err != nil {
		t.Fatalf("read the row written before Restart, through the DSN from before: %v", err)
	}
	if id != 7 {
		t.Errorf("row after Restart = %d, want 7", id)
	}
	// A database made after the restart must not reuse a name from before.
	if _, err := s.NewDatabase(ctx); err != nil {
		t.Errorf("NewDatabase after Restart: %v", err)
	}
}

// TestLockedDown checks that NewDatabase's DSN logs in as User, a role
// with none of the rights the store refuses (internal/store, account.go),
// that owns its database and so may make schemas in it, as ariesd's role
// does; and that the server listens on no TCP port.
func TestLockedDown(t *testing.T) {
	t.Parallel()
	s := startOrSkip(t)
	dsn, err := s.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	db := open(t, dsn)
	var user string
	var super, createRole, createDB, replication, bypassRLS, owner bool
	if err := db.QueryRowContext(t.Context(), `SELECT r.rolname, r.rolsuper, r.rolcreaterole, r.rolcreatedb, r.rolreplication, r.rolbypassrls,
  d.datdba = r.oid
FROM pg_roles r, pg_database d WHERE r.rolname = current_user AND d.datname = current_database()`).
		Scan(&user, &super, &createRole, &createDB, &replication, &bypassRLS, &owner); err != nil {
		t.Fatal(err)
	}
	if user != User {
		t.Errorf("NewDatabase DSN logs in as %q, want %s", user, User)
	}
	if super || createRole || createDB || replication || bypassRLS {
		t.Errorf("%s: superuser %v, createrole %v, createdb %v, replication %v, bypassrls %v; want none", user, super, createRole, createDB, replication, bypassRLS)
	}
	if !owner {
		t.Errorf("%s does not own the database NewDatabase made", user)
	}
	var members int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.member
  WHERE r.rolname = current_user`).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if members != 0 {
		t.Errorf("%s is a member of %d roles, want none", user, members)
	}
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA made"); err != nil {
		t.Errorf("CREATE SCHEMA as %s in its own database: %v", user, err)
	}
	var listen string
	if err := db.QueryRowContext(t.Context(), "SHOW listen_addresses").Scan(&listen); err != nil {
		t.Fatal(err)
	}
	if listen != "" {
		t.Errorf("listen_addresses = %q, want none: the server is reached through its socket alone", listen)
	}
}

// TestDropDatabase checks that a dropped database is gone from the
// server, that a connection a test left open does not keep it, that its
// name is not given out again, and that the database beside it stays.
func TestDropDatabase(t *testing.T) {
	t.Parallel()
	s := startOrSkip(t)
	ctx := t.Context()
	root := open(t, s.RootDSN(superuser))
	exists := func(dsn string) bool {
		t.Helper()
		var n int
		name := strings.TrimPrefix(dsn[strings.LastIndex(dsn, "/"):strings.Index(dsn, "?")], "/")
		if err := root.QueryRowContext(ctx, "SELECT COUNT(*) FROM pg_database WHERE datname = $1", name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	kept, err := s.NewDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := s.NewDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db := open(t, dsn)
	if _, err := db.ExecContext(ctx, "CREATE TABLE held (id INT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if !exists(dsn) {
		t.Fatalf("the database of %s is not listed before it is dropped, so this test would see nothing", dsn)
	}

	if err := s.DropDatabase(dsn); err != nil {
		t.Fatalf("DropDatabase(%s), with a connection to it open = %v", dsn, err)
	}
	if exists(dsn) {
		t.Errorf("the database of %s is still there after DropDatabase", dsn)
	}
	if !exists(kept) {
		t.Errorf("DropDatabase(%s) dropped the database of %s too", dsn, kept)
	}
	if next, err := s.NewDatabase(ctx); err != nil || next == dsn {
		t.Errorf("NewDatabase after DropDatabase = %q, %v; want a new name, not %q again", next, err, dsn)
	}
	if err := s.DropDatabase(dsn); err == nil {
		t.Errorf("DropDatabase(%s) a second time = nil, want an error: nothing was dropped", dsn)
	}
	for _, bad := range []string{"", "no-database", "mysql://aries@/t1", s.DSN("t1; DROP DATABASE t2")} {
		if err := s.DropDatabase(bad); err == nil {
			t.Errorf("DropDatabase(%q) = nil, want an error: it names no plain database", bad)
		}
	}
}
