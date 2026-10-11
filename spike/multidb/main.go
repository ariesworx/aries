// Command multidb is the stage 3a spike (docs/design/starfix.md §15.5): it
// measures what many projects on one server cost when each project has a
// Dolt database and a store of its own.
//
// Usage, with dolt at version.Dolt on PATH:
//
//	go run ./spike/multidb [-projects 1,5,20,50] [-issues 300]
//
// It starts a throwaway dolt sql-server, locked down as internal/dolttest
// does, and reports:
//
//   - grants: whether the least-privileged account can CREATE DATABASE,
//     bare, with a grant on a name pattern, and with a grant on the exact
//     name ahead of time;
//   - memory: Dolt's resident memory and connection count as stores are
//     opened, loaded, left idle and closed, and how long a store takes to
//     open the first time (migrating) and again (already migrated).
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/store"
)

var (
	projects = flag.String("projects", "1,5,20,50", "cumulative numbers of open stores to measure at")
	issues   = flag.Int("issues", 300, "issues created in each store")
	settle   = flag.Duration("settle", 5*time.Second, "pause before each memory reading")
)

func main() {
	flag.Parse()
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "multidb:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "multidb")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	srv, err := dolttest.Start(ctx, dir)
	if err != nil {
		return err
	}
	defer func() { _ = srv.Stop() }()
	v, _ := dolttest.Version(ctx)
	fmt.Printf("dolt %s\n\n", v)

	first, err := srv.NewDatabase(ctx)
	if err != nil {
		return err
	}
	if err := grants(ctx, srv, first); err != nil {
		return err
	}
	return memory(ctx, srv, dir, first)
}

// grants reports whether the least-privileged account may create a
// database under each grant root can give it short of one on *.*.
func grants(ctx context.Context, srv *dolttest.Server, userDSN string) error {
	root, err := sql.Open("mysql", srv.RootDSN(""))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	user, err := sql.Open("mysql", userDSN)
	if err != nil {
		return err
	}
	defer func() { _ = user.Close() }()
	account := "'" + dolttest.User + "'@'localhost'"
	var who string
	if err := user.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&who); err != nil {
		return err
	}
	try := func(label, grant, db string) error {
		if grant != "" {
			if _, err := root.ExecContext(ctx, grant); err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
		}
		_, err := user.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+db)
		res := "refused"
		if err == nil {
			// Allowed is only worth something if the database is there and
			// the account that made it can use it.
			var n int
			if err := root.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", db).Scan(&n); err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
			_, use := user.ExecContext(ctx, "CREATE TABLE "+db+".t (id INT PRIMARY KEY)")
			res = fmt.Sprintf("allowed; exists: %v; account uses it: %v", n == 1, use == nil)
		}
		fmt.Printf("  %-34s %s\n", label, res)
		return nil
	}
	fmt.Printf("grants: CREATE DATABASE as %s, the least-privileged account\n", who)
	for _, c := range [][3]string{
		{"no grant", "", "sfx_a"},
		{"grant on pattern `sfx\\_%`.*", "GRANT ALL ON `sfx\\_%`.* TO " + account, "sfx_b"},
		{"grant on pattern `sfx_%`.*", "GRANT ALL ON `sfx_%`.* TO " + account, "sfx_c"},
		{"grant on the exact name, ahead", "GRANT ALL ON sfx_d.* TO " + account, "sfx_d"},
	} {
		if err := try(c[0], c[1], c[2]); err != nil {
			return err
		}
	}
	// The same statement on a connection with no current database: the
	// check seems to read the rights on the connection's database, not on
	// the one being created.
	bare, err := sql.Open("mysql", userDSN[:strings.LastIndex(userDSN, "/")+1])
	if err != nil {
		return err
	}
	defer func() { _ = bare.Close() }()
	_, err = bare.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS sfx_f")
	fmt.Printf("  %-34s allowed: %v\n", "no grant, no current database", err == nil)
	// What works: root creates and grants. Twice, to show it can be rerun.
	for i := 0; i < 2; i++ {
		for _, q := range []string{"CREATE DATABASE IF NOT EXISTS sfx_e", "GRANT ALL ON sfx_e.* TO " + account} {
			if _, err := root.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("root creates, run %d: %w", i+1, err)
			}
		}
	}
	_, err = user.ExecContext(ctx, "CREATE TABLE sfx_e.t (id INT PRIMARY KEY)")
	fmt.Printf("  %-34s rerun ok; account uses it: %v\n", "root creates and grants, twice", err == nil)
	var global int
	if err := user.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.USER_PRIVILEGES").Scan(&global); err != nil {
		return err
	}
	fmt.Printf("  %-34s %d\n\n", "the account's rights on *.* after", global)
	return nil
}

// memory opens stores cumulatively to each count in -projects, loads each
// new one, and reads Dolt's resident memory along the way.
func memory(ctx context.Context, srv *dolttest.Server, dir, firstDSN string) error {
	root, err := sql.Open("mysql", srv.RootDSN(""))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	read := func(label string, n int) error {
		time.Sleep(*settle)
		rss, err := doltRSS(dir)
		if err != nil {
			return err
		}
		var conns int
		rows, err := root.QueryContext(ctx, "SHOW PROCESSLIST")
		if err != nil {
			return err
		}
		for rows.Next() {
			conns++
		}
		_ = rows.Close()
		fmt.Printf("  %-34s stores %3d  dolt rss %6d MiB  connections %4d\n", label, n, rss>>10, conns-1)
		return nil
	}

	fmt.Printf("memory: %d issues a store, each with a comment, a label and a claim\n", *issues)
	if err := read("server started", 0); err != nil {
		return err
	}
	var stores []*store.Store
	var dsns []string
	defer func() {
		for _, s := range stores {
			_ = s.Close()
		}
	}()
	next := firstDSN
	for _, f := range strings.Split(*projects, ",") {
		target, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return err
		}
		var opened, loaded time.Duration
		added := 0
		for len(stores) < target {
			dsn := next
			if dsn == "" {
				if dsn, err = srv.NewDatabase(ctx); err != nil {
					return err
				}
			}
			next = ""
			t := time.Now()
			s, err := store.Open(ctx, dsn, store.Options{})
			if err != nil {
				return err
			}
			opened += time.Since(t)
			stores, dsns = append(stores, s), append(dsns, dsn)
			t = time.Now()
			if err := load(ctx, s, *issues); err != nil {
				return err
			}
			loaded += time.Since(t)
			added++
		}
		if added > 0 {
			fmt.Printf("  opened %d: first open (migrating) %v a store; load %v a store, %v a write\n", added,
				(opened / time.Duration(added)).Round(time.Millisecond), (loaded / time.Duration(added)).Round(time.Millisecond),
				(loaded / time.Duration(added*(*issues)*4)).Round(time.Millisecond))
		}
		if err := read("open and loaded", len(stores)); err != nil {
			return err
		}
	}

	// Idle: do the open stores cost anything once the writes stop?
	time.Sleep(30 * time.Second)
	if err := read("idle 30s, all open", len(stores)); err != nil {
		return err
	}
	// Close all but one, as the idle close of §15.1 would.
	n := len(stores)
	for _, s := range stores[1:] {
		if err := s.Close(); err != nil {
			return err
		}
	}
	stores = stores[:1]
	if err := read("all but one closed", 1); err != nil {
		return err
	}
	time.Sleep(30 * time.Second)
	if err := read("30s later", 1); err != nil {
		return err
	}
	// Reopen: what a project's first request after an idle close waits for.
	var reopen time.Duration
	for _, dsn := range dsns[1:] {
		t := time.Now()
		s, err := store.Open(ctx, dsn, store.Options{})
		if err != nil {
			return err
		}
		reopen += time.Since(t)
		stores = append(stores, s)
	}
	if n > 1 {
		fmt.Printf("  reopened %d: %v a store\n", n-1, (reopen / time.Duration(n-1)).Round(time.Millisecond))
	}
	return read("all reopened", len(stores))
}

// load writes n issues to s, each with a comment, a label and a claim
// taken by starting it: four writes an issue.
func load(ctx context.Context, s *store.Store, n int) error {
	actor := store.Actor{Principal: "alice", Session: "spike", Machine: "localhost"}
	body := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 12)
	for i := 0; i < n; i++ {
		is, err := s.CreateIssue(ctx, actor, store.NewIssue{Title: fmt.Sprintf("Issue %d of the spike", i), Body: body})
		if err != nil {
			return err
		}
		if _, err := s.AddComment(ctx, actor, is.ID, body, ""); err != nil {
			return err
		}
		if err := s.AddLabel(ctx, actor, is.ID, "spike"); err != nil {
			return err
		}
		if _, _, err := s.StartIssue(ctx, actor, is.ID, time.Hour, false); err != nil {
			return err
		}
	}
	return s.Flush(ctx)
}

// doltRSS returns the resident memory, in KiB, of the dolt sql-server
// whose config file is under dir.
func doltRSS(dir string) (int64, error) {
	out, err := exec.Command("ps", "-axo", "rss=,command=").Output()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "sql-server") && strings.Contains(line, dir) {
			return strconv.ParseInt(strings.Fields(line)[0], 10, 64)
		}
	}
	return 0, fmt.Errorf("no dolt sql-server under %s in ps", dir)
}
