// Package pgtest starts a throwaway Postgres for the spike's tests and
// measurements, as internal/dolttest starts Dolt: the pinned release from
// the user's cache directory (pgtest.Version; install.sh beside this file
// fetches and verifies it), on loopback, in a temporary data directory,
// with a watcher that kills it and removes its files if the process that
// started it dies first. Nothing is installed system-wide.
//
// The server has a superuser, postgres, which only this package uses, and
// a role, starfix, that may log in and owns what it is given: a database
// per project (Database) or a schema per project in one database
// (Schema), the two layouts the spike compares.
package pgtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx"
)

// Version is the pinned Postgres release.
const Version = "18.6"

// ErrNoPostgres means the pinned release is not installed.
var ErrNoPostgres = errors.New("postgres " + Version + " not found: run spike/postgres/install.sh")

// Bin returns the directory of the pinned release's programs:
// $STARFIX_POSTGRES_BIN, or ~/.cache/starfix/postgres-VERSION/bin.
func Bin() (string, error) {
	dir := os.Getenv("STARFIX_POSTGRES_BIN")
	if dir == "" {
		cache := os.Getenv("XDG_CACHE_HOME")
		if cache == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			cache = filepath.Join(home, ".cache")
		}
		dir = filepath.Join(cache, "starfix", "postgres-"+Version, "bin")
	}
	out, err := exec.Command(filepath.Join(dir, "postgres"), "--version").Output() //nolint:gosec // the pinned binary
	if err != nil {
		return "", ErrNoPostgres
	}
	if !strings.Contains(string(out), " "+Version) {
		return "", fmt.Errorf("pgtest: %s is %q, want %s", dir, strings.TrimSpace(string(out)), Version)
	}
	return dir, nil
}

// Options are settings for the server, as -c name=value. Durable leaves
// fsync and synchronous_commit at Postgres's defaults; without it they
// are off, which is what a test wants and a measurement must not have.
type Options struct {
	Durable  bool
	Settings map[string]string
}

// Server is a running Postgres.
type Server struct {
	Port int
	Dir  string

	cmd   *exec.Cmd
	watch *watcher
	admin *sql.DB

	mu   sync.Mutex
	made []string // databases and schemas made, to drop
}

// Start initializes a data directory under a new temporary directory and
// starts Postgres on it.
func Start(ctx context.Context, opts Options) (*Server, error) {
	bin, err := Bin()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "sfxpg-")
	if err != nil {
		return nil, err
	}
	s := &Server{Dir: dir}
	data := filepath.Join(dir, "data")
	initdb := exec.CommandContext(ctx, filepath.Join(bin, "initdb"), "-D", data, "-U", "postgres", "-A", "trust", //nolint:gosec // the pinned binary
		"--locale=C", "-E", "UTF8", "--no-sync")
	if out, err := initdb.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("pgtest: initdb: %w: %s", err, out)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.Port = ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	args := []string{"-D", data, "-p", strconv.Itoa(s.Port), "-c", "listen_addresses=127.0.0.1",
		"-c", "unix_socket_directories=", "-c", "max_connections=1000", "-c", "logging_collector=off"}
	if !opts.Durable {
		args = append(args, "-c", "fsync=off", "-c", "synchronous_commit=off", "-c", "full_page_writes=off")
	}
	for k, v := range opts.Settings {
		args = append(args, "-c", k+"="+v)
	}
	if s.watch, err = startWatcher(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.cmd = exec.Command(filepath.Join(bin, "postgres"), args...) //nolint:gosec // the pinned binary
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	log, err := os.Create(filepath.Join(dir, "server.log"))
	if err != nil {
		s.watch.dismiss()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	s.cmd.Stdout, s.cmd.Stderr = log, log
	if err := s.cmd.Start(); err != nil {
		_ = log.Close()
		s.watch.dismiss()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	_ = log.Close()
	if err := s.watch.watch(s.cmd.Process.Pid); err != nil {
		return nil, errors.Join(err, s.Stop())
	}
	if s.admin, err = sql.Open("pgx", s.dsn("postgres", "postgres", "")); err != nil {
		return nil, errors.Join(err, s.Stop())
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err = s.admin.PingContext(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(filepath.Join(dir, "server.log"))
			return nil, errors.Join(fmt.Errorf("pgtest: postgres did not start: %w: %s", err, b), s.Stop())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := s.admin.ExecContext(ctx, `CREATE ROLE starfix LOGIN`); err != nil {
		return nil, errors.Join(err, s.Stop())
	}
	return s, nil
}

func (s *Server) dsn(user, db, schema string) string {
	d := fmt.Sprintf("postgres://%s@127.0.0.1:%d/%s?sslmode=disable", user, s.Port, db)
	if user == "starfix" {
		// A test that holds one writer open while another writes would
		// wait forever on Postgres's row locks; let it fail instead.
		d += "&lock_timeout=5s"
	}
	if schema != "" {
		d += "&search_path=" + schema
	}
	return d
}

// Admin returns the superuser's connection to the postgres database.
func (s *Server) Admin() *sql.DB { return s.admin }

// AdminDSN returns the superuser's DSN for db.
func (s *Server) AdminDSN(db string) string { return s.dsn("postgres", db, "") }

// PID is the postmaster's process id.
func (s *Server) PID() int { return s.cmd.Process.Pid }

func name(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// Database creates a database that starfix owns and returns starfix's
// DSN for it.
func (s *Server) Database(ctx context.Context) (string, error) {
	db := name("sfx_")
	if err := s.createDatabase(ctx, db); err != nil {
		return "", err
	}
	return s.dsn("starfix", db, ""), nil
}

var createMu sync.Mutex // CREATE DATABASE copies template1, one at a time

func (s *Server) createDatabase(ctx context.Context, db string) error {
	createMu.Lock()
	defer createMu.Unlock()
	if _, err := s.admin.ExecContext(ctx, `CREATE DATABASE `+db+` OWNER starfix`); err != nil {
		return fmt.Errorf("pgtest: create database: %w", err)
	}
	s.mu.Lock()
	s.made = append(s.made, "d:"+db)
	s.mu.Unlock()
	return nil
}

// SharedDB is the database that holds the schemas Schema makes.
const SharedDB = "starfix"

// Schema creates a schema in the shared database, owned by starfix, and
// returns starfix's DSN for it, whose search_path is that schema.
func (s *Server) Schema(ctx context.Context) (string, error) {
	if err := s.shared(ctx); err != nil {
		return "", err
	}
	sc := name("sfx_")
	db, err := sql.Open("pgx", s.dsn("postgres", SharedDB, ""))
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA `+sc+` AUTHORIZATION starfix`); err != nil {
		return "", fmt.Errorf("pgtest: create schema: %w", err)
	}
	s.mu.Lock()
	s.made = append(s.made, "s:"+sc)
	s.mu.Unlock()
	return s.dsn("starfix", SharedDB, sc), nil
}

var sharedOnce sync.Map // *Server -> error

func (s *Server) shared(ctx context.Context) error {
	v, _ := sharedOnce.LoadOrStore(s, new(sync.Once))
	var err error
	v.(*sync.Once).Do(func() {
		err = s.createDatabase(ctx, SharedDB)
		if err == nil {
			// The public schema is not the starfix role's to use.
			var db *sql.DB
			if db, err = sql.Open("pgx", s.dsn("postgres", SharedDB, "")); err == nil {
				_, err = db.ExecContext(ctx, `REVOKE ALL ON SCHEMA public FROM PUBLIC`)
				_ = db.Close()
			}
		}
	})
	return err
}

// Drop drops a database or schema a DSN of Database or Schema names.
func (s *Server) Drop(ctx context.Context, dsn string) error {
	if i := strings.Index(dsn, "search_path="); i >= 0 {
		db, err := sql.Open("pgx", s.dsn("postgres", SharedDB, ""))
		if err != nil {
			return err
		}
		defer func() { _ = db.Close() }()
		_, err = db.ExecContext(ctx, `DROP SCHEMA `+dsn[i+len("search_path="):]+` CASCADE`)
		return err
	}
	u := dsn[strings.LastIndexByte(dsn, '/')+1:]
	u, _, _ = strings.Cut(u, "?")
	_, err := s.admin.ExecContext(ctx, `DROP DATABASE `+u+` WITH (FORCE)`)
	return err
}

// Stop stops the server and removes its directory.
func (s *Server) Stop() error {
	if s.admin != nil {
		_ = s.admin.Close()
	}
	var err error
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(syscall.SIGINT) // fast shutdown
		done := make(chan error, 1)
		go func() { done <- s.cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
	if s.watch != nil {
		s.watch.dismiss()
	}
	return errors.Join(err, os.RemoveAll(s.Dir))
}

// RSS returns the resident set size, in bytes, of the postmaster and
// every backend it started, from ps.
func (s *Server) RSS() (int64, int, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,rss=").Output()
	if err != nil {
		return 0, 0, err
	}
	pid := s.PID()
	var total int64
	n := 0
	for line := range bytes.SplitSeq(out, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) != 3 {
			continue
		}
		p, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		if p == pid || pp == pid {
			kb, _ := strconv.ParseInt(f[2], 10, 64)
			total += kb << 10
			n++
		}
	}
	return total, n, nil
}

const watchScript = `trap '' INT TERM HUP QUIT
read -r pid || exit 0
case $pid in '' | *[!0-9]* | 0 | 1) exit 0 ;; esac
if read -r stopped; then exit 0; fi
kill -s KILL -- "-$pid" 2>/dev/null
n=0
while kill -s 0 -- "-$pid" 2>/dev/null && [ "$n" -lt 100 ]; do
	n=$((n + 1))
	sleep 0.1 2>/dev/null || sleep 1
done
rm -rf -- "$1"
exit 0
`

// watcher is dolttest's: a shell reading the server's pid from a pipe,
// which kills the server's group and removes its directory when the pipe
// closes without "stopped", that is when this process died.
type watcher struct {
	cmd *exec.Cmd
	in  io.WriteCloser
}

func startWatcher(dir string) (*watcher, error) {
	cmd := exec.Command("/bin/sh", "-c", watchScript, "sh", dir) //nolint:gosec // a constant script; dir is an argument
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &watcher{cmd: cmd, in: in}, nil
}

func (w *watcher) watch(pid int) error {
	_, err := io.WriteString(w.in, strconv.Itoa(pid)+"\n")
	return err
}

func (w *watcher) dismiss() {
	_, _ = io.WriteString(w.in, "\nstopped\n")
	_ = w.in.Close()
	_ = w.cmd.Wait()
}
