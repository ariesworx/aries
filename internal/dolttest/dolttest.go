// Package dolttest starts a throwaway dolt sql-server for tests.
//
// [Start] launches a [Server] in a directory the caller owns, usually
// t.TempDir(), and the caller must [Server.Stop] it. Tests reach it
// through its unix socket: [Server.NewDatabase] makes an empty database
// for each test, with the DSN of the least-privileged account [User], and
// [Server.RootDSN] gives Dolt's superuser, for tests of the store's
// account check. [Server.Grant] grants User a database by name before it
// exists. Without dolt on PATH, Start returns [ErrNoDolt] and
// callers skip, unless [RequireEnv] makes that a failure. NewDatabase is
// safe for concurrent use.
//
// Measurements (spike/multidb) also use [StartWith], for Dolt's metrics
// endpoint and extra environment, [Server.Restart], for a new dolt process
// on the same data, and [Server.PID].
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
const RequireEnv = "STARFIX_REQUIRE_DOLT"

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
func Version(ctx context.Context) (string, error) {
	bin, err := lookDolt()
	if err != nil {
		return "", err
	}
	out, err := exec.CommandContext(ctx, bin, "version").Output() //nolint:gosec // fixed args, dolt from PATH
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
// ctx bounds only the startup. The caller must Stop the server, which
// otherwise outlives the test process.
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
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("dolttest: %w", err)
		}
	}
	// DOLT_DISABLE_EVENT_FLUSH keeps dolt from starting a detached "dolt
	// send-metrics" as each command exits, to send its usage events to
	// DoltHub. That process outlived Stop and wrote under root while the
	// caller was removing dir.
	env := append(os.Environ(), "DOLT_ROOT_PATH="+root, "DOLT_DISABLE_EVENT_FLUSH=1")
	for _, kv := range [][2]string{{"user.name", "starfix-test"}, {"user.email", "test@example.com"}} {
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
const User = "starfix"

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
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return fmt.Errorf("dolttest: start dolt: %w", err)
	}
	exit := make(chan error, 1)
	s.Port, s.metrics, s.cmd, s.exit = port, metrics, cmd, exit
	go func() { exit <- cmd.Wait(); _ = logf.Close() }()

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

// Grant gives User every right on each database named, whether or not it
// exists yet: Dolt honors a grant on an exact name made beforehand, which
// is how an operator lets starfixd create a project's database later
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

// Stop interrupts the server and waits for it to exit, killing it after
// 10 seconds. Once it returns, nothing the server started still runs, so
// the caller can remove its directory. Call it once: a second call waits
// 10 seconds and fails.
func (s *Server) Stop() error {
	if s.cmd.Process == nil {
		return nil
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case <-s.exit:
		return nil
	case <-time.After(10 * time.Second):
		if err := s.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("dolttest: kill: %w", err)
		}
		<-s.exit
		return nil
	}
}
