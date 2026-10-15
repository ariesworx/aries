package pgtest

import (
	"database/sql"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"
)

// patience is how long a test waits for something that must happen;
// nothing is asserted of it.
const patience = time.Minute

// newPool makes a pool of at most limit servers for one test, which
// closes it. It skips without Postgres, as startOrSkip does.
func newPool(t *testing.T, limit int) *Pool {
	t.Helper()
	if _, err := lookPostgres(); errors.Is(err, ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the pgtest tests")
	}
	p := NewPool("pgtest-pool-")
	p.limit = limit
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close = %v", err)
		}
	})
	return p
}

// take is p.take for a holder that must get a server.
func take(t *testing.T, p *Pool, holder string) *Server {
	t.Helper()
	s, err := p.take(holder)
	if err != nil {
		t.Fatalf("take(%q) = %v", holder, err)
	}
	return s
}

// servers reports how many servers p has started, and how many of them
// no one holds.
func servers(p *Pool) (started, idle int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.all), len(p.idle)
}

// A holder has one server however often it asks, and gives it up only
// when it has given it up as often.
func TestPoolOneServerToAHolder(t *testing.T) {
	t.Parallel()
	p := newPool(t, 2)
	first := take(t, p, "TestA")
	if again := take(t, p, "TestA"); again != first {
		t.Errorf("a second take by the holder returned another server")
	}
	_ = p.release("TestA")
	if started, idle := servers(p); started != 1 || idle != 0 {
		t.Errorf("after one release of two takes: %d servers started, %d idle; want 1 and 0, the holder still has it", started, idle)
	}
	_ = p.release("TestA")
	if started, idle := servers(p); started != 1 || idle != 1 {
		t.Errorf("after both releases: %d servers started, %d idle; want 1 and 1", started, idle)
	}
}

// Two holders at once have a server each, both of which answer, and a
// server given up goes to the next holder: none is started for it.
func TestPoolServersAreNotShared(t *testing.T) {
	t.Parallel()
	p := newPool(t, 2)
	a, b := take(t, p, "TestA"), take(t, p, "TestB")
	if a == b {
		t.Fatal("two holders at once were given the same server")
	}
	for name, s := range map[string]*Server{"TestA": a, "TestB": b} {
		dsn, err := s.NewDatabase(t.Context())
		if err != nil {
			t.Fatalf("NewDatabase on the server of %s = %v", name, err)
		}
		if err := s.DropDatabase(dsn); err != nil {
			t.Errorf("DropDatabase on the server of %s = %v", name, err)
		}
	}
	_ = p.release("TestA")
	if c := take(t, p, "TestC"); c != a {
		t.Errorf("the next holder was not given the server just given up")
	}
	if started, _ := servers(p); started != 2 {
		t.Errorf("%d servers started for three holders, two at a time; want 2", started)
	}
	_ = p.release("TestB")
	_ = p.release("TestC")
}

// At its limit the pool starts no more servers: a holder waits for one
// to be given up.
func TestPoolWaitsAtItsLimit(t *testing.T) {
	t.Parallel()
	p := newPool(t, 1)
	a := take(t, p, "TestA")
	got := make(chan *Server, 1)
	go func() {
		s, err := p.take("TestB")
		if err != nil {
			t.Errorf("take(TestB) = %v", err)
		}
		got <- s
	}()
	// Until the second holder is waiting, a release proves nothing.
	for end := time.Now().Add(patience); ; time.Sleep(time.Millisecond) {
		p.mu.Lock()
		waiting := p.waiting
		p.mu.Unlock()
		if waiting == 1 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the second holder never waited for a server: the pool started one past its limit, or take hangs")
		}
	}
	select {
	case <-got:
		t.Fatal("the second holder was given a server while the only one was held")
	default:
	}
	_ = p.release("TestA")
	select {
	case b := <-got:
		if b != a {
			t.Errorf("the waiting holder was given another server than the one given up")
		}
	case <-time.After(patience):
		t.Fatal("the waiting holder was not given the server that was given up")
	}
	if started, _ := servers(p); started != 1 {
		t.Errorf("%d servers started in a pool of at most 1", started)
	}
	_ = p.release("TestB")
}

// Server gives a test and its subtests one server, for as long as any of
// them runs.
func TestPoolServer(t *testing.T) {
	t.Parallel()
	p := newPool(t, 2)
	t.Run("a test", func(t *testing.T) {
		s, err := p.Server(t)
		if err != nil {
			t.Fatalf("Server = %v", err)
		}
		t.Run("its subtest", func(t *testing.T) {
			sub, err := p.Server(t)
			if err != nil {
				t.Fatalf("Server = %v", err)
			}
			if sub != s {
				t.Errorf("a subtest was given another server than its test's")
			}
		})
		if started, idle := servers(p); started != 1 || idle != 0 {
			t.Errorf("after the subtest ended: %d servers started, %d idle; want 1 and 0, the test still has it", started, idle)
		}
	})
	if started, idle := servers(p); started != 1 || idle != 1 {
		t.Errorf("after the test ended: %d servers started, %d idle; want 1 and 1", started, idle)
	}
}

// Database makes a database on the test's server that lasts as long as
// the test, and no longer.
func TestPoolDatabase(t *testing.T) {
	t.Parallel()
	p := newPool(t, 2)
	var dsn string
	var srv *Server
	t.Run("a test", func(t *testing.T) {
		var err error
		if srv, err = p.Server(t); err != nil {
			t.Fatalf("Server = %v", err)
		}
		if dsn, err = p.Database(t); err != nil {
			t.Fatalf("Database = %v", err)
		}
		if u, err := url.Parse(dsn); err != nil || u.Query().Get("host") != srv.dir {
			t.Errorf("Database = %q, which is not on the test's server (%s)", dsn, srv.dir)
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.ExecContext(t.Context(), "CREATE TABLE made (id INT PRIMARY KEY)"); err != nil {
			t.Errorf("a table in the test's database: %v", err)
		}
	})
	// DropDatabase fails on a database that is gone.
	if err := srv.DropDatabase(dsn); err == nil {
		t.Errorf("the database %s was still there after its test ended", dsn)
	}
}

// A database or a role a test leaves on its server is dropped when the
// test gives the server up: the next test finds a server that holds
// none.
func TestPoolDropsWhatATestLeaves(t *testing.T) {
	t.Parallel()
	p := newPool(t, 1)
	var srv *Server
	t.Run("a test", func(t *testing.T) {
		var err error
		if srv, err = p.Server(t); err != nil {
			t.Fatalf("Server = %v", err)
		}
		root, err := sql.Open("pgx", srv.RootDSN(superuser))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = root.Close() }()
		for _, q := range []string{"CREATE DATABASE left_behind", "CREATE DATABASE left_too", "CREATE ROLE left_role",
			"GRANT CONNECT ON DATABASE postgres TO left_role"} {
			if _, err := root.ExecContext(t.Context(), q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	})
	root, err := sql.Open("pgx", srv.RootDSN(superuser))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	rows, err := root.QueryContext(t.Context(), "SELECT datname FROM pg_database WHERE datname LIKE 'left%' UNION ALL SELECT rolname FROM pg_roles WHERE rolname LIKE 'left%'")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s is still on the server after the test that made it", name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// The server still serves: the account's own database was not dropped.
	if _, err := srv.NewDatabase(t.Context()); err != nil {
		t.Errorf("NewDatabase on the server after its databases were dropped = %v", err)
	}
}

// Warm starts the first server, so that no test is the one to start it.
func TestPoolWarm(t *testing.T) {
	t.Parallel()
	p := newPool(t, 2)
	p.Warm()
	if started, idle := servers(p); started != 1 || idle != 1 {
		t.Fatalf("after Warm: %d servers started, %d idle; want 1 and 1", started, idle)
	}
	take(t, p, "TestA")
	if started, _ := servers(p); started != 1 {
		t.Errorf("%d servers started for the first holder of a warm pool, want the 1 it had", started)
	}
	_ = p.release("TestA")
}

// Close stops every server and removes its directory, and a closed pool
// gives none.
func TestPoolClose(t *testing.T) {
	t.Parallel()
	p := newPool(t, 2)
	a, b := take(t, p, "TestA"), take(t, p, "TestB")
	_ = p.release("TestA")
	_ = p.release("TestB")
	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	for _, s := range []*Server{a, b} {
		if _, err := os.Stat(s.dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the directory of a closed pool's server is still there: %s (%v)", s.dir, err)
		}
		if err := s.cmd.Process.Signal(os.Interrupt); !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("postgres of a closed pool still runs in %s: signaling it = %v", s.dir, err)
		}
	}
	if _, err := p.take("TestC"); err == nil {
		t.Error("a closed pool gave a server")
	}
}

// Without Postgres the pool's error is Start's, so callers skip or fail by
// it as before.
func TestPoolWithoutPostgres(t *testing.T) {
	// Serial: sets the process's environment (t.Setenv).
	t.Setenv("PATH", t.TempDir())
	t.Setenv(RequireEnv, "")
	p := NewPool("pgtest-pool-")
	t.Cleanup(func() { _ = p.Close() })
	p.Warm()
	if _, err := p.Server(t); !errors.Is(err, ErrNoPostgres) {
		t.Errorf("Server without postgres on PATH = %v, want ErrNoPostgres", err)
	}
}
