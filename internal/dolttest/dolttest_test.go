package dolttest

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/version"
)

// shortDir makes a directory for a server, removed when t ends. Not
// t.TempDir, whose path has the test's name in it: with the server's
// socket it comes within a few bytes of the 104 a unix socket's path may
// be on macOS, and passes them under a temporary directory with one more
// component (scripts/leftovers.sh).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sfd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startOrSkip starts a server for this package's own tests. It skips without
// dolt unless STARFIX_REQUIRE_DOLT is set, as every Dolt-backed package does.
func startOrSkip(t *testing.T) *Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s, err := Start(ctx, shortDir(t))
	if errors.Is(err, ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the dolttest tests")
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

func TestStartWithoutDolt(t *testing.T) {
	// Serial: sets the process's environment (t.Setenv).
	tests := []struct {
		name    string
		require string
		skip    bool // the error is ErrNoDolt, so callers skip
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
				t.Fatal("Start without dolt on PATH succeeded")
			}
			if got := errors.Is(err, ErrNoDolt); got != tc.skip {
				t.Errorf("Start with %s=%q: errors.Is(%v, ErrNoDolt) = %v, want %v", RequireEnv, tc.require, err, got, tc.skip)
			}
			if !tc.skip && !strings.Contains(err.Error(), RequireEnv) {
				t.Errorf("Start with %s=%q: error %q does not name %s", RequireEnv, tc.require, err, RequireEnv)
			}
		})
	}
}

// TestDoltVersion checks that the Dolt the tests run against is the one
// this release names (version.Dolt, and CI's DOLT_VERSION).
func TestDoltVersion(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("dolt"); err != nil {
		if required() {
			t.Fatalf("%s is set but dolt is not on PATH", RequireEnv)
		}
		t.Skip("dolt is not on PATH")
	}
	got, err := Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != version.Dolt {
		t.Errorf("Version() = %q, want version.Dolt %q; scripts/install-dolt.sh installs that release and prints the directory to put first on PATH", got, version.Dolt)
	}
}

// Asking dolt its version writes nothing in the home directory of whoever
// runs the tests: dolt makes ~/.dolt for any command, and looks for a
// newer release.
func TestVersionLeavesTheHomeDirectoryAlone(t *testing.T) {
	// Serial: sets the process's environment (t.Setenv).
	if _, err := lookDolt(); errors.Is(err, ErrNoDolt) {
		t.Skip("dolt is not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, err := Version(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("Version() left %s in the home directory", e.Name())
	}
}

// TestStartPortTaken reproduces T-8: the port Start picks is taken by
// another Dolt before this one binds it. Start must not mistake the other
// server for its own; it retries on another port.
func TestStartPortTaken(t *testing.T) {
	// Serial: replaces the package's pickPort.
	a := startOrSkip(t)
	if _, err := a.NewDatabase(t.Context()); err != nil {
		t.Fatal(err)
	}
	ports := []int{a.Port}
	pickPort = func() (int, error) {
		if len(ports) > 0 {
			p := ports[0]
			ports = ports[1:]
			return p, nil
		}
		return freePort()
	}
	t.Cleanup(func() { pickPort = freePort })
	b := startOrSkip(t)
	if b.Port == a.Port {
		t.Fatalf("second server reports the first one's port %d", a.Port)
	}
	dsn, err := b.NewDatabase(t.Context())
	if err != nil {
		t.Fatalf("NewDatabase on the second server: %v", err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("ping the second server's database: %v", err)
	}
}

// TestStopLeavesNothingRunning checks that nothing dolt started still runs
// once Stop returns, so the caller can remove the directory. As it exits,
// dolt starts a detached "dolt send-metrics" unless DOLT_DISABLE_EVENT_FLUSH
// is set, and that process wrote under the server's directory while
// TempDir's cleanup was removing it.
func TestStopLeavesNothingRunning(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("listing processes needs /proc")
	}
	dir := shortDir(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s, err := Start(ctx, dir)
	if errors.Is(err, ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the dolttest tests")
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	left, err := running(filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("after Stop, still running with the server's DOLT_ROOT_PATH: %q", left)
	}
}

// running returns the pid and command line of each process whose
// environment sets DOLT_ROOT_PATH to root. It reads /proc, so Linux only.
func running(root string) ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	want := "\x00DOLT_ROOT_PATH=" + root + "\x00"
	var found []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a process
		}
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !strings.Contains("\x00"+string(env)+"\x00", want) {
			continue // exited, another user's, or not this server's
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		found = append(found, e.Name()+" "+strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " ")))
	}
	return found, nil
}

// TestRestartKeepsData checks that Restart brings up a new dolt process on
// the same data, and that a DSN handed out before it still works after.
func TestRestartKeepsData(t *testing.T) {
	t.Parallel()
	s := startOrSkip(t)
	ctx := t.Context()
	dsn, err := s.NewDatabase(ctx)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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

// TestStartWithOptions checks that Metrics turns on Dolt's Prometheus
// endpoint and that Env reaches the server, by reading back a Go memory
// limit set in it, before and after a restart; and that Start alone turns
// the endpoint off.
func TestStartWithOptions(t *testing.T) {
	t.Parallel()
	if plain := startOrSkip(t); plain.MetricsURL() != "" {
		t.Errorf("Start: MetricsURL() = %q, want none", plain.MetricsURL())
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s, err := StartWith(ctx, shortDir(t), Options{Metrics: true, Env: []string{"GOMEMLIMIT=3GiB"}})
	if err != nil {
		t.Fatalf("StartWith: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	want := "go_gc_gomemlimit_bytes " + strconv.FormatFloat(3<<30, 'e', -1, 64)
	check := func(when string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.MetricsURL(), nil)
		if err != nil {
			t.Fatalf("%s: MetricsURL() = %q: %v", when, s.MetricsURL(), err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: GET %s: %v", when, s.MetricsURL(), err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "\n"+want+"\n") {
			t.Errorf("%s: GET %s has no line %q, so GOMEMLIMIT did not reach dolt", when, s.MetricsURL(), want)
		}
	}
	check("after StartWith")
	if err := s.Restart(ctx); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	check("after Restart")
}

// TestLockedDown checks that the test server is configured as
// docs/server.md says a production one must be: NewDatabase's DSN logs in as
// User, not root, and secure_file_priv names a directory.
func TestLockedDown(t *testing.T) {
	t.Parallel()
	s := startOrSkip(t)
	dsn, err := s.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var user string
	var priv sql.NullString
	if err := db.QueryRowContext(t.Context(), "SELECT CURRENT_USER(), @@secure_file_priv").Scan(&user, &priv); err != nil {
		t.Fatal(err)
	}
	if user != User+"@localhost" {
		t.Errorf("NewDatabase DSN logs in as %q, want %s@localhost", user, User)
	}
	if !priv.Valid || priv.String == "" {
		t.Errorf("secure_file_priv = %q (valid %v), want a directory", priv.String, priv.Valid)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM information_schema.USER_PRIVILEGES WHERE GRANTEE = ?",
		"'"+User+"'@'localhost'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%s holds %d global privileges, want none", User, n)
	}
}

// TestDropDatabase checks that a dropped database is gone from the
// server, that a connection a test left open does not keep it, that its
// name is not given out again, and that the database beside it stays.
func TestDropDatabase(t *testing.T) {
	t.Parallel()
	s := startOrSkip(t)
	ctx := t.Context()
	root, err := sql.Open("mysql", s.RootDSN(""))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	exists := func(dsn string) bool {
		t.Helper()
		var n int
		name := dsn[strings.LastIndex(dsn, "/")+1:]
		if err := root.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", name).Scan(&n); err != nil {
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
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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
	for _, bad := range []string{"", "no-database", s.RootDSN(""), "u@unix(/x)/a`; DROP DATABASE b"} {
		if err := s.DropDatabase(bad); err == nil {
			t.Errorf("DropDatabase(%q) = nil, want an error: it names no database", bad)
		}
	}
}
