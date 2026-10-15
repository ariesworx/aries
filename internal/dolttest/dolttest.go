// Package dolttest starts a throwaway dolt sql-server for tests.
//
// [Start] launches a [Server] in a directory the caller owns, usually
// t.TempDir(), and the caller should [Server.Stop] it. Tests reach it
// through its unix socket: [Server.NewDatabase] makes an empty database
// for each test, with the DSN of the least-privileged account [User],
// which the test drops when it ends ([Server.DropDatabase]), and
// [Server.RootDSN] gives Dolt's superuser, for tests of the store's
// account check. [Server.Grant] grants User a database by name before it
// exists. Without dolt on PATH, Start returns [ErrNoDolt] and
// callers skip, unless [RequireEnv] makes that a failure. NewDatabase is
// safe for concurrent use.
//
// A package's tests take their servers from a [Pool], which gives each
// test one to itself while it runs, so that the tests can run in
// parallel, and drops what a test left on it.
//
// Tests of Dolt's metrics also use [StartWith], for its metrics
// endpoint and extra environment; tests and measurements use
// [Server.Restart], for a new dolt process on the same data, and
// [Server.PID].
//
// A server does not outlive the process that started it. A test binary
// that is killed (a go test timeout, a canceled run, a stopped agent)
// runs no deferred Stop, and its dolt, a child of no one, once ran on for
// hours. So on Linux and macOS each launch has a watcher (watch_unix.go):
// a /bin/sh in a session of its own, reading a pipe that only the
// starting process holds the other end of. When that process is gone,
// however it went, the pipe closes; the watcher then kills dolt's process
// group, removes from the directory what this package and dolt put there,
// removes the directory if that emptied it, and exits. Stop, or dolt
// exiting by itself, ends the watcher first, and then the directory is
// left to the caller. Nothing looks for the directories of earlier runs:
// that would mean guessing which are in use. On Windows the package
// builds but there is no watcher.
package dolttest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql" // driver for the readiness check
)

// ErrNoDolt means dolt is not on PATH; callers skip their tests. Start
// never returns it when RequireEnv is set.
var ErrNoDolt = errors.New("dolt not found on PATH")

// RequireEnv names the environment variable that makes a missing dolt a
// failure rather than a skip. CI sets it to 1, so a Dolt-backed test can
// never pass by skipping.
const RequireEnv = "ARIES_REQUIRE_DOLT"

// required reports whether RequireEnv is set to anything but 0.
func required() bool {
	v := os.Getenv(RequireEnv)
	return v != "" && v != "0"
}

// Server is a running dolt sql-server on a loopback port. Connections go
// through its unix socket, which only this server can have created.
//
// It is configured the way docs/server.md locks a server's Dolt down:
// secure_file_priv names a directory that does not exist, and NewDatabase
// hands out the DSN of User, an account with rights on that database
// only, never root's, so the tests run through the same account check
// (store.Options.AllowUnsafeAccount) as a production server.
type Server struct {
	// Port is the loopback TCP port the server listens on. Connections
	// use the socket instead.
	Port    int
	metrics int // the metrics endpoint's loopback port; 0 for none
	socket  string
	dir     string
	bin     string
	env     []string
	opts    Options
	cmd     *exec.Cmd
	exit    chan error   // receives cmd.Wait's result, once, when dolt exits
	n       atomic.Int64 // databases made, for unique names
}

// Options are what [StartWith] adds to [Start]. The zero value adds nothing.
type Options struct {
	// Env is added to the server's environment, each entry "KEY=value":
	// GOMEMLIMIT, say, to run Dolt under a Go memory limit.
	Env []string
	// Metrics turns on Dolt's Prometheus endpoint, on a loopback port of
	// its own, at [Server.MetricsURL].
	Metrics bool
}

// pickPort chooses the TCP port for the next launch; tests replace it.
var pickPort = freePort

// lookDolt finds dolt on PATH, or reports why there is none.
func lookDolt() (string, error) {
	bin, err := exec.LookPath("dolt")
	if err == nil {
		return bin, nil
	}
	if required() {
		return "", fmt.Errorf("dolttest: %s is set but dolt is not on PATH: scripts/install-dolt.sh installs the Dolt release in internal/version.Dolt and prints the directory to add", RequireEnv)
	}
	return "", ErrNoDolt
}

// Version returns the version of the dolt on PATH, such as "2.4.2".
//
// Dolt keeps its own files under its root, by default the home directory
// (~/.dolt), and makes them for any command, this one included. So it is
// given a root that is removed afterward: a test run writes nothing in
// the home directory of whoever runs it.
func Version(ctx context.Context) (string, error) {
	bin, err := lookDolt()
	if err != nil {
		return "", err
	}
	root, err := os.MkdirTemp("", "dolttest-version-")
	if err != nil {
		return "", fmt.Errorf("dolttest: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	cmd := exec.CommandContext(ctx, bin, "version") //nolint:gosec // fixed args, dolt from PATH
	cmd.Env = append(os.Environ(), "DOLT_ROOT_PATH="+root, "DOLT_DISABLE_EVENT_FLUSH=1")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("dolttest: dolt version: %w", err)
	}
	// The first line is "dolt version X.Y.Z"; later lines may warn about
	// a newer release.
	line, _, _ := strings.Cut(string(out), "\n")
	v, ok := strings.CutPrefix(strings.TrimSpace(line), "dolt version ")
	if !ok || v == "" {
		return "", fmt.Errorf("dolttest: unexpected dolt version output %q", line)
	}
	return v, nil
}

// Start runs dolt sql-server with its data and config under dir, on a free
// port, and waits until it accepts connections, trying up to three ports.
// ctx bounds only the startup: if it ends first, the server is stopped.
// The caller should Stop the server. One that is not stopped runs until
// the process that started it is gone, and is then killed, and what it
// had in dir removed (the package comment says how).
func Start(ctx context.Context, dir string) (*Server, error) {
	return StartWith(ctx, dir, Options{})
}

// StartWith is Start with opts.
func StartWith(ctx context.Context, dir string, opts Options) (*Server, error) {
	bin, err := lookDolt()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "home")
	for _, d := range []string{root, filepath.Join(dir, "data")} {
		if err := os.MkdirAll(d, 0o700); err != nil { //nolint:gosec // under the directory the caller chose for the server
			return nil, fmt.Errorf("dolttest: %w", err)
		}
	}
	// DOLT_DISABLE_EVENT_FLUSH keeps dolt from starting a detached "dolt
	// send-metrics" as each command exits, to send its usage events to
	// DoltHub. That process outlived Stop and wrote under root while the
	// caller was removing dir.
	env := append(os.Environ(), "DOLT_ROOT_PATH="+root, "DOLT_DISABLE_EVENT_FLUSH=1")
	for _, kv := range [][2]string{{"user.name", "aries-test"}, {"user.email", "test@example.com"}} {
		c := exec.CommandContext(ctx, bin, "config", "--global", "--add", kv[0], kv[1]) //nolint:gosec // fixed args, dolt from PATH
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("dolttest: dolt config: %w: %s", err, out)
		}
	}
	// The port is free when picked but not held, so another process can
	// take it before dolt binds it (T-8). Dolt then exits, and the next
	// attempt picks another port. Readiness is checked over this launch's
	// own socket, so another server on the port is never mistaken for it.
	s := &Server{dir: dir, bin: bin, env: append(env, opts.Env...), opts: opts}
	var lastErr error
	for i := range 3 {
		s.socket = filepath.Join(dir, fmt.Sprintf("dolt%d.sock", i))
		if lastErr = s.launch(ctx); lastErr == nil {
			return s, nil
		}
	}
	return nil, lastErr
}

// Restart stops the server and starts dolt again on the same data, with
// the same options and socket, so the DSNs handed out before still work.
// The ports change. ctx bounds only the startup.
func (s *Server) Restart(ctx context.Context) error {
	if err := s.Stop(); err != nil {
		return err
	}
	var lastErr error
	for range 3 {
		// A dolt that was killed leaves its socket behind.
		if err := os.Remove(s.socket); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("dolttest: %w", err)
		}
		if lastErr = s.launch(ctx); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// PID is the process id of the running dolt sql-server. It changes at
// each Restart.
func (s *Server) PID() int { return s.cmd.Process.Pid }

// MetricsURL is the URL of Dolt's Prometheus endpoint, or empty when
// [Options.Metrics] was not set. It changes at each Restart.
func (s *Server) MetricsURL() string {
	if s.metrics == 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/metrics", s.metrics)
}

// User is the least-privileged account NewDatabase's DSNs log in as.
const User = "aries"

// config is the sql-server configuration: loopback and a unix socket,
// and secure_file_priv set to a directory that does not exist, so
// LOAD_FILE and INTO OUTFILE reach no file even for an account with FILE.
const config = `log_level: info
data_dir: %q
cfg_dir: %q
listener:
  host: 127.0.0.1
  port: %d
  socket: %q
system_variables:
  secure_file_priv: %q
`

// metricsConfig turns on the Prometheus endpoint, which has no
// authentication, on loopback.
const metricsConfig = `metrics:
  host: 127.0.0.1
  port: %d
`

// launch starts dolt on s.socket and on ports picked now, waits up to 30
// seconds for root to connect over the socket, and creates User. If it
// fails, the server is stopped.
func (s *Server) launch(ctx context.Context) error {
	port, err := pickPort()
	if err != nil {
		return err
	}
	body := fmt.Sprintf(config, filepath.Join(s.dir, "data"), filepath.Join(s.dir, "doltcfg"), port, s.socket, filepath.Join(s.dir, "no-such-dir"))
	metrics := 0
	if s.opts.Metrics {
		if metrics, err = pickPort(); err != nil {
			return err
		}
		body += fmt.Sprintf(metricsConfig, metrics)
	}
	// Appended to, so a restart keeps what the server logged before it.
	logf, err := os.OpenFile(filepath.Join(s.dir, "server.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // test temp dir
	if err != nil {
		return fmt.Errorf("dolttest: %w", err)
	}
	cfg := filepath.Join(s.dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		_ = logf.Close()
		return fmt.Errorf("dolttest: %w", err)
	}
	// Not CommandContext: the server runs until Stop, not until ctx ends.
	cmd := exec.Command(s.bin, "sql-server", "--config", cfg) //nolint:gosec // dolt from PATH
	cmd.Env = s.env
	cmd.Stdout = logf
	cmd.Stderr = logf
	ownGroup(cmd)
	// The watcher first, so dolt never runs without one.
	w, err := startWatcher(s.dir)
	if err != nil {
		_ = logf.Close()
		return err
	}
	if err := cmd.Start(); err != nil {
		w.dismiss()
		_ = logf.Close()
		return fmt.Errorf("dolttest: start dolt: %w", err)
	}
	exit := make(chan error, 1)
	s.Port, s.metrics, s.cmd, s.exit = port, metrics, cmd, exit
	watched := w.watch(cmd.Process.Pid)
	// The watcher goes before exit is sent, so nothing of this launch
	// runs once Stop has returned.
	go func() { err := cmd.Wait(); w.dismiss(); _ = logf.Close(); exit <- err }()
	if watched != nil {
		return errors.Join(watched, s.Stop())
	}

	db, err := sql.Open("mysql", s.rootDSN(""))
	if err != nil {
		return errors.Join(fmt.Errorf("dolttest: %w", err), s.Stop())
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := db.PingContext(ctx); err == nil {
			if _, err := db.ExecContext(ctx, "CREATE USER IF NOT EXISTS '"+User+"'@'localhost'"); err != nil {
				return errors.Join(fmt.Errorf("dolttest: create user: %w", err), s.Stop())
			}
			return nil
		}
		select {
		case err := <-exit:
			return fmt.Errorf("dolttest: dolt exited early (see %s/server.log): %v", s.dir, err)
		case <-ctx.Done():
			return errors.Join(ctx.Err(), s.Stop())
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return errors.Join(errors.New("dolttest: server not ready after 30s"), s.Stop())
		}
	}
}

// freePort returns a loopback TCP port that was free a moment ago;
// nothing holds it.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("dolttest: free port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) rootDSN(db string) string {
	return fmt.Sprintf("root@unix(%s)/%s", s.socket, db)
}

// RootDSN is the DSN of root on database db (empty for none): Dolt's
// default superuser, which tests use to show that an unsafe account is
// refused.
func (s *Server) RootDSN(db string) string { return s.rootDSN(db) }

// NewDatabase creates an empty database with a unique name and returns the
// DSN of User on it, who has every right on that database and none
// elsewhere.
func (s *Server) NewDatabase(ctx context.Context) (string, error) {
	name := fmt.Sprintf("t%d", s.n.Add(1))
	db, err := sql.Open("mysql", s.rootDSN(""))
	if err != nil {
		return "", fmt.Errorf("dolttest: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		return "", fmt.Errorf("dolttest: create database: %w", err)
	}
	if _, err := db.ExecContext(ctx, "GRANT ALL ON "+name+".* TO '"+User+"'@'localhost'"); err != nil {
		return "", fmt.Errorf("dolttest: grant: %w", err)
	}
	return fmt.Sprintf("%s@unix(%s)/%s", User, s.socket, name), nil
}

// dropTimeout bounds DropDatabase, which takes a moment: it is there so
// that a Dolt that does not answer fails a cleanup instead of hanging it.
const dropTimeout = time.Minute

// DropDatabase drops the database a DSN of NewDatabase's names, whatever
// connections to it are still open. A test calls it from a cleanup, once
// it has closed what it opened on the database, which is why it takes no
// context: a test's own has ended by then.
//
// Dolt keeps every database of a server in memory, about 14 MB for one
// that holds Aries's schema, and takes longer to open each new one the
// more it holds: a package that makes a database for each of several
// hundred tests and drops none ends with a Dolt of several gigabytes, and
// spends most of its time in it.
func (s *Server) DropDatabase(dsn string) error {
	_, name, ok := strings.Cut(dsn, ")/")
	if !ok || name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
		return fmt.Errorf("dolttest: drop database: %q does not end in a plain database name", dsn)
	}
	db, err := sql.Open("mysql", s.rootDSN(""))
	if err != nil {
		return fmt.Errorf("dolttest: %w", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), dropTimeout)
	defer cancel()
	if _, err := db.ExecContext(ctx, "DROP DATABASE "+name); err != nil {
		return fmt.Errorf("dolttest: drop database %s: %w", name, err)
	}
	return nil
}

// dropAll drops every database on the server but Dolt's own. Only a
// [Pool] calls it, on a server no test holds.
func (s *Server) dropAll() error {
	db, err := sql.Open("mysql", s.rootDSN(""))
	if err != nil {
		return fmt.Errorf("dolttest: %w", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), dropTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, "SELECT schema_name FROM information_schema.schemata WHERE schema_name NOT IN ('information_schema', 'mysql')")
	if err != nil {
		return fmt.Errorf("dolttest: list databases: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("dolttest: list databases: %w", err)
		}
		names = append(names, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("dolttest: list databases: %w", err)
	}
	var errs []error
	for _, name := range names {
		if strings.Contains(name, "`") {
			errs = append(errs, fmt.Errorf("dolttest: drop database: %q cannot be quoted", name))
			continue
		}
		if _, err := db.ExecContext(ctx, "DROP DATABASE `"+name+"`"); err != nil {
			errs = append(errs, fmt.Errorf("dolttest: drop database %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Grant gives User every right on each database named, whether or not it
// exists yet: Dolt honors a grant on an exact name made beforehand, which
// is how an operator lets ariesd create a project's database later
// (design §15.5, 1). Each name must be lowercase letters, digits and
// underscores, since it is written into the statement.
func (s *Server) Grant(ctx context.Context, names ...string) error {
	db, err := sql.Open("mysql", s.rootDSN(""))
	if err != nil {
		return fmt.Errorf("dolttest: %w", err)
	}
	defer func() { _ = db.Close() }()
	for _, name := range names {
		if strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" || name == "" {
			return fmt.Errorf("dolttest: grant: %q is not a plain database name", name)
		}
		if _, err := db.ExecContext(ctx, "GRANT ALL ON "+name+".* TO '"+User+"'@'localhost'"); err != nil {
			return fmt.Errorf("dolttest: grant: %w", err)
		}
	}
	return nil
}

// Stop interrupts the server and waits for it to exit, killing it and its
// process group after 10 seconds. Once it returns, nothing the server or
// its launch started still runs, the watcher included, so the caller can
// remove its directory. Call it once: a second call waits 10 seconds and
// fails.
func (s *Server) Stop() error {
	if s.cmd.Process == nil {
		return nil
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case <-s.exit:
		return nil
	case <-time.After(10 * time.Second):
		if err := killGroup(s.cmd); err != nil {
			return fmt.Errorf("dolttest: kill: %w", err)
		}
		<-s.exit
		return nil
	}
}
