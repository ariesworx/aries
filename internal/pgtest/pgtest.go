// Package pgtest starts a throwaway Postgres for tests.
//
// [Start] initializes a data directory in a directory the caller owns
// and runs a [Server] on it, which the caller should [Server.Stop]. The
// server listens on a unix socket in that directory alone, never on a
// TCP port, and its superuser, which only this package and tests of the
// account check use ([Server.RootDSN]), signs in without a password: the
// directory is the user's own (mode 0700), so no one else can reach the
// socket. [Server.NewDatabase] makes an empty database for each test,
// owned by [User], a role that may log in and do nothing else (no
// superuser, no CREATEDB, no CREATEROLE), as ariesd's role is on a
// server; the test drops it when it ends ([Server.DropDatabase]).
// Without postgres and initdb on PATH, Start returns [ErrNoPostgres] and
// callers skip, unless [RequireEnv] makes that a failure. NewDatabase is
// safe for concurrent use.
//
// The server runs with fsync, synchronous_commit and full_page_writes
// off: a test's data need not survive a crash of the machine, and
// without them a write waits for no disk. So nothing a test measures of
// a write's time says anything of a durable one.
//
// A package's tests take their servers from a [Pool], which gives each
// test one to itself while it runs, so that the tests can run in
// parallel and a test may stop, freeze or restart its server, and drops
// what a test left on it.
//
// A server does not outlive the process that started it. A test binary
// that is killed (a go test timeout, a canceled run, a stopped agent)
// runs no deferred Stop. So on Linux and macOS each launch has a watcher
// (watch_unix.go): a /bin/sh in a session of its own, reading a pipe that
// only the starting process holds the other end of. When that process is
// gone, however it went, the pipe closes; the watcher then asks the
// server for an immediate shutdown, which ends every backend it forked
// and frees its System V semaphores and shared memory, kills the
// server's process group if it has not gone within 10 seconds, frees
// the semaphore sets a killed server left (macOS; sema_darwin.go),
// removes from the directory what this package and Postgres put there,
// removes the directory if that emptied it, and exits. Stop, or the
// server exiting by itself, ends the watcher first, and then the
// directory is left to the caller. Nothing looks for the directories of
// earlier runs: that would mean guessing which are in use. On Windows
// the package builds but there is no watcher, and Start fails: the tests
// that need Postgres skip there.
package pgtest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // driver "pgx", for the readiness check and the superuser's statements
)

// ErrNoPostgres means postgres or initdb is not on PATH; callers skip
// their tests. Start never returns it when RequireEnv is set.
var ErrNoPostgres = errors.New("postgres and initdb not found on PATH")

// RequireEnv names the environment variable that makes a missing
// Postgres a failure rather than a skip. CI sets it to 1, so a test that
// needs Postgres can never pass by skipping.
const RequireEnv = "ARIES_REQUIRE_POSTGRES"

// required reports whether RequireEnv is set to anything but 0.
func required() bool {
	v := os.Getenv(RequireEnv)
	return v != "" && v != "0"
}

// User is the locked-down role NewDatabase's DSNs log in as, and that
// owns each database it makes.
const User = "aries"

// superuser is the role initdb makes, which only this package and the
// tests of the account check log in as.
const superuser = "postgres"

// port names the server's socket (.s.PGSQL.5432). The server listens on
// no TCP port, and each has a directory of its own, so it is the same for
// every one.
const port = 5432

// Server is a running Postgres, reached through its unix socket.
type Server struct {
	dir  string
	bin  string
	cmd  *exec.Cmd
	exit chan error // receives cmd.Wait's result, once, when the server exits
	// stopped is set once Stop has seen this launch's server exit.
	stopped bool
	n       atomic.Int64 // databases made, for unique names
}

// lookPostgres finds the directory holding postgres and initdb, from
// postgres on PATH, or reports why there is none.
func lookPostgres() (string, error) {
	bin, err := exec.LookPath("postgres")
	if err == nil {
		dir := filepath.Dir(bin)
		if _, err := os.Stat(filepath.Join(dir, "initdb")); err == nil {
			return dir, nil
		}
	}
	if required() {
		return "", fmt.Errorf("pgtest: %s is set but postgres and initdb are not on PATH: scripts/install-postgres.sh installs the Postgres release in internal/version.Postgres and prints the directory to add", RequireEnv)
	}
	return "", ErrNoPostgres
}

// Version returns the release of the postgres on PATH, such as "18.6".
func Version(ctx context.Context) (string, error) {
	bin, err := lookPostgres()
	if err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, filepath.Join(bin, "postgres"), "--version").Output() //nolint:gosec // postgres from PATH
	if err != nil {
		return "", fmt.Errorf("pgtest: postgres --version: %w", err)
	}
	return parseVersion(string(out))
}

// parseVersion reads the release from what postgres --version prints:
// "postgres (PostgreSQL) 18.6", which a distribution's build follows with
// its own words, such as "(Ubuntu 18.6-1.pgdg24.04+1)".
func parseVersion(out string) (string, error) {
	f := strings.Fields(out)
	if len(f) < 3 || f[0] != "postgres" || f[1] != "(PostgreSQL)" {
		return "", fmt.Errorf("pgtest: unexpected postgres --version output %q", strings.TrimSpace(out))
	}
	return f[2], nil
}

// Start initializes a data directory under dir, starts Postgres on it
// and waits until it accepts connections. ctx bounds only the startup:
// if it ends first, the server is stopped. The caller should Stop the
// server. One that is not stopped runs until the process that started it
// is gone, and is then killed, and what it had in dir removed (the
// package comment says how).
//
// dir's path must leave room for the socket's name in a unix socket's
// path, at most 103 bytes on macOS: a directory of os.MkdirTemp's, not a
// t.TempDir named for a long test.
func Start(ctx context.Context, dir string) (*Server, error) {
	bin, err := lookPostgres()
	if err != nil {
		return nil, err
	}
	if os.Geteuid() == 0 {
		return nil, errors.New("pgtest: Postgres refuses to run as root; run the tests as another user")
	}
	if n := len(dir) + len("/.s.PGSQL.") + len(strconv.Itoa(port)); n > 103 {
		return nil, fmt.Errorf("pgtest: %s is too long for a unix socket's path in it (%d bytes, at most 103)", dir, n)
	}
	// The socket is reached by whoever can reach the directory, and the
	// superuser signs in without a password.
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory, which its owner must be able to enter
		return nil, fmt.Errorf("pgtest: %w", err)
	}
	s := &Server{dir: dir, bin: bin}
	initdb := exec.CommandContext(ctx, filepath.Join(bin, "initdb"), "-D", s.data(), "-U", superuser, "-A", "trust", //nolint:gosec // initdb from PATH
		"--locale=C", "-E", "UTF8", "--no-sync", "--no-instructions")
	if out, err := initdb.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("pgtest: initdb: %w: %s", err, out)
	}
	if err := s.launch(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// data is the server's data directory.
func (s *Server) data() string { return filepath.Join(s.dir, "data") }

// Restart stops the server, unless it is stopped already, and starts
// Postgres again on the same data and socket, so the DSNs handed out
// before still work. ctx bounds only the startup.
func (s *Server) Restart(ctx context.Context) error {
	if err := s.Stop(); err != nil {
		return err
	}
	return s.launch(ctx)
}

// PID is the process id of the postmaster. Each process it starts, a
// backend for each connection among them, is a session of its own, and
// exits when the postmaster is gone; [Server.Signal] reaches them all.
// It changes at each Restart.
func (s *Server) PID() int { return s.cmd.Process.Pid }

// settings are the server's, as -c NAME=VALUE: the socket in its own
// directory and no TCP port, room for the connections of a test that
// opens many stores (a writer and up to eight readers each), and no
// waiting for the disk.
var settings = []string{
	"listen_addresses=",
	"unix_socket_permissions=0700",
	"max_connections=300",
	"fsync=off",
	"synchronous_commit=off",
	"full_page_writes=off",
	"logging_collector=off",
}

// launching runs as each launch begins; tests replace it.
var launching = func() {}

// launch starts Postgres on the data directory and waits up to 30
// seconds for the superuser to connect, then makes User if it is not
// there. If it fails, the server is stopped.
func (s *Server) launch(ctx context.Context) error {
	launching()
	// Appended to, so a restart keeps what the server logged before it.
	logf, err := os.OpenFile(filepath.Join(s.dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the caller's directory
	if err != nil {
		return fmt.Errorf("pgtest: %w", err)
	}
	args := []string{"-D", s.data(), "-k", s.dir, "-p", strconv.Itoa(port)}
	for _, kv := range settings {
		args = append(args, "-c", kv)
	}
	// Not CommandContext: the server runs until Stop, not until ctx ends.
	cmd := exec.Command(filepath.Join(s.bin, "postgres"), args...) //nolint:gosec // postgres from PATH
	cmd.Stdout = logf
	cmd.Stderr = logf
	ownGroup(cmd)
	// The watcher first, so the server never runs without one.
	w, err := startWatcher(s.dir)
	if err != nil {
		_ = logf.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		w.dismiss()
		_ = logf.Close()
		return fmt.Errorf("pgtest: start postgres: %w", err)
	}
	exit := make(chan error, 1)
	s.cmd, s.exit, s.stopped = cmd, exit, false
	watched := w.watch(cmd.Process.Pid)
	// The watcher goes before exit is sent, so nothing of this launch
	// runs once Stop has returned. A server that exits frees its
	// semaphores and shared memory, and one that was killed leaves
	// them: they are freed before the watcher goes, so that one or the
	// other frees them.
	pid, data := cmd.Process.Pid, s.data()
	go func() {
		err := cmd.Wait()
		freeSemaphores(data, pid)
		_ = removeSegment(data)
		w.dismiss()
		_ = logf.Close()
		exit <- err
	}()
	if watched != nil {
		return errors.Join(watched, s.Stop())
	}

	db, err := sql.Open("pgx", s.RootDSN(superuser))
	if err != nil {
		return errors.Join(fmt.Errorf("pgtest: %w", err), s.Stop())
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := db.PingContext(ctx); err == nil {
			// The server made all its semaphores before it took a
			// connection.
			if err := s.recordSemaphores(pid); err != nil {
				return errors.Join(err, s.Stop())
			}
			// Restart finds the role made at the first launch.
			if _, err := db.ExecContext(ctx, `DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+User+`') THEN CREATE ROLE `+User+` LOGIN; END IF;
END $$`); err != nil {
				return errors.Join(fmt.Errorf("pgtest: create role: %w", err), s.Stop())
			}
			return nil
		}
		select {
		case err := <-exit:
			return fmt.Errorf("pgtest: postgres exited early (see %s/server.log): %v", s.dir, err)
		case <-ctx.Done():
			return errors.Join(ctx.Err(), s.Stop())
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.Join(errors.New("pgtest: server not ready after 30s"), s.Stop())
		}
	}
}

// dsn is the DSN of role on database db, over the socket.
func (s *Server) dsn(role, db string) string {
	q := url.Values{"host": {s.dir}, "port": {strconv.Itoa(port)}, "sslmode": {"disable"}}
	return (&url.URL{Scheme: "postgres", User: url.User(role), Path: "/" + db, RawQuery: q.Encode()}).String()
}

// RootDSN is the DSN of the superuser on database db: what tests of the
// store's account check open to show that an unsafe role is refused, and
// what a test uses to make another role.
func (s *Server) RootDSN(db string) string { return s.dsn(superuser, db) }

// DSN is the DSN of User on database db.
func (s *Server) DSN(db string) string { return s.dsn(User, db) }

// NewDatabase creates an empty database with a unique name, owned by
// User, and returns the DSN of User on it. As its owner, User may create
// schemas in it, as ariesd's role does in its database, and nothing
// beyond it.
func (s *Server) NewDatabase(ctx context.Context) (string, error) {
	name := fmt.Sprintf("t%d", s.n.Add(1))
	db, err := sql.Open("pgx", s.RootDSN(superuser))
	if err != nil {
		return "", fmt.Errorf("pgtest: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+name+" OWNER "+User); err != nil {
		return "", fmt.Errorf("pgtest: create database: %w", err)
	}
	return s.DSN(name), nil
}

// dropTimeout bounds DropDatabase: it is there so that a server that
// does not answer fails a cleanup instead of hanging it.
const dropTimeout = time.Minute

// DropDatabase drops the database a DSN of NewDatabase's names, whatever
// connections to it are still open. A test calls it from a cleanup, once
// it has closed what it opened on the database, which is why it takes no
// context: a test's own has ended by then.
func (s *Server) DropDatabase(dsn string) error {
	u, err := url.Parse(dsn)
	name := ""
	if err == nil {
		name = strings.TrimPrefix(u.Path, "/")
	}
	if err != nil || u.Scheme != "postgres" || !plainName(name) {
		return fmt.Errorf("pgtest: drop database: %q does not name a plain database", dsn)
	}
	db, err := sql.Open("pgx", s.RootDSN(superuser))
	if err != nil {
		return fmt.Errorf("pgtest: %w", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), dropTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("pgtest: drop database %s: %w", name, err)
	}
	return nil
}

// plainName reports whether name is lowercase letters, digits and
// underscores, and so safe to write into a statement unquoted.
func plainName(name string) bool {
	return name != "" && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

// dropAll drops every database on the server but Postgres's own, and
// every role but the superuser and User. Only a [Pool] calls it, on a
// server no test holds.
func (s *Server) dropAll() error {
	db, err := sql.Open("pgx", s.RootDSN(superuser))
	if err != nil {
		return fmt.Errorf("pgtest: %w", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), dropTimeout)
	defer cancel()
	list := func(query string) ([]string, error) {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, err
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return nil, err
			}
			names = append(names, name)
		}
		return names, errors.Join(rows.Err(), rows.Close())
	}
	dbs, err := list(`SELECT datname FROM pg_database WHERE NOT datistemplate AND datname <> 'postgres'`)
	if err != nil {
		return fmt.Errorf("pgtest: list databases: %w", err)
	}
	roles, err := list(`SELECT rolname FROM pg_roles WHERE rolname NOT LIKE 'pg\_%' AND rolname NOT IN ('` + superuser + `', '` + User + `')`)
	if err != nil {
		return fmt.Errorf("pgtest: list roles: %w", err)
	}
	var errs []error
	for _, name := range dbs {
		if _, err := db.ExecContext(ctx, `DROP DATABASE "`+strings.ReplaceAll(name, `"`, `""`)+`" WITH (FORCE)`); err != nil {
			errs = append(errs, fmt.Errorf("pgtest: drop database %s: %w", name, err))
		}
	}
	// A role a test made owns nothing once its databases are gone, but
	// may hold rights in the postgres database.
	for _, name := range roles {
		quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
		if _, err := db.ExecContext(ctx, `DROP OWNED BY `+quoted); err != nil {
			errs = append(errs, fmt.Errorf("pgtest: drop what role %s owns: %w", name, err))
			continue
		}
		if _, err := db.ExecContext(ctx, `DROP ROLE `+quoted); err != nil {
			errs = append(errs, fmt.Errorf("pgtest: drop role %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Stop asks the server to shut down fast and waits for it to exit,
// asking for an immediate shutdown after 10 seconds and killing its
// process group after 10 more. Once it returns, nothing the server or
// its launch started still runs, the watcher included, so the caller
// can remove its directory. Stopping a server that is stopped does
// nothing.
func (s *Server) Stop() error {
	if s.cmd == nil || s.cmd.Process == nil || s.stopped {
		return nil
	}
	// SIGINT is Postgres's fast shutdown: it ends the sessions and stops.
	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case <-s.exit:
		s.stopped = true
		return nil
	case <-time.After(10 * time.Second):
	}
	// The server frees its semaphores and shared memory in an immediate
	// shutdown too, and in none if it is killed.
	quitGroup(s.cmd)
	select {
	case <-s.exit:
		s.stopped = true
		return nil
	case <-time.After(10 * time.Second):
	}
	if err := killGroup(s.cmd); err != nil {
		return fmt.Errorf("pgtest: kill: %w", err)
	}
	// What the killed server left was freed before exit was sent.
	<-s.exit
	s.stopped = true
	return nil
}
