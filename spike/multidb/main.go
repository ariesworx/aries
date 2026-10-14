// Command multidb is the stage 3a spike (docs/design/starfix.md §15.5): it
// measures what many projects on one server cost when each project has a
// Dolt database and a store of its own.
//
// Usage, with dolt at version.Dolt on PATH:
//
//	go run ./spike/multidb -exp grants
//	go run ./spike/multidb -exp table      [-projects 1,5,20,50] [-issues 300]
//	go run ./spike/multidb -exp restart    [-stores 5] [-issues 300] [-every 0] [-budget 30m] [-gc]
//	go run ./spike/multidb -exp limit      [-stores 10] [-issues 300] [-limits 0,2048,1024] [-repeat 2]
//	go run ./spike/multidb -exp concurrent [-concurrency 1,2,5,10,20] [-issues 300] [-repeat 3]
//
// Each starts a throwaway dolt sql-server, locked down as internal/dolttest
// does, with its metrics endpoint on, and reports:
//
//   - grants: whether the least-privileged account can CREATE DATABASE,
//     bare, with a grant on a name pattern, and with a grant on the exact
//     name ahead of time.
//   - table: Dolt's memory and connection count as stores are opened,
//     loaded one after another, left idle and closed, and how long a store
//     takes to open the first time (migrating) and again.
//   - restart: -stores stores of -issues issues are loaded, Dolt is
//     restarted on the same data, and its memory is read at rest, after each
//     store is opened again, after reads (ready, list, blocked, and show
//     with comments of a sample of issues) and after a few new writes. With
//     -stores 1 and a large -issues it is the measurement of a large
//     project; -every prints memory as the load goes, and -budget stops the
//     load, not the run, when it has taken that long. -gc then runs Dolt's
//     own garbage collection (CALL DOLT_GC()) in each database, restarts
//     again and reads memory once more.
//   - limit: the same load into a fresh server under each Go memory limit
//     in -limits (MiB; 0 is none), with the latency of each write, Dolt's
//     CPU and its collector's cycles.
//   - concurrent: for each number in -concurrency, that many stores on a
//     fresh server loaded at the same time, one writer each, with the
//     latency of each write, the throughput and Dolt's CPU.
//
// Memory is read three ways and printed side by side (mem.go): the
// process's resident set from the operating system (on Linux split into
// anonymous and file-backed pages, from /proc), the container's cgroup
// where there is one, and Dolt's own metrics. -deadline bounds a whole run,
// and -doltlog keeps Dolt's log, which otherwise goes with its directory.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/store"
)

var (
	exp         = flag.String("exp", "table", "experiment: grants, table, restart, limit or concurrent")
	projects    = flag.String("projects", "1,5,20,50", "table: cumulative numbers of open stores to measure at")
	issues      = flag.Int("issues", 300, "issues created in each store")
	settle      = flag.Duration("settle", 5*time.Second, "pause before each memory reading")
	stores      = flag.Int("stores", 5, "restart, limit: stores loaded, one after another")
	every       = flag.Int("every", 0, "restart: print memory every this many issues of the load (0: never)")
	gc          = flag.Bool("gc", false, "restart: then run Dolt's garbage collection in each database and restart again")
	budget      = flag.Duration("budget", 30*time.Minute, "restart: stop loading, and go on with what is written, after this long")
	limits      = flag.String("limits", "0", "limit: Go memory limits for Dolt, in MiB (0: none)")
	concurrency = flag.String("concurrency", "1,2,5,10,20", "concurrent: numbers of stores loaded at the same time")
	repeat      = flag.Int("repeat", 1, "limit, concurrent: runs of each setting")
	deadline    = flag.Duration("deadline", 2*time.Hour, "give the whole run up after this long")
	keepLog     = flag.String("doltlog", "", "copy Dolt's log to this file when its server stops (the last server's, for limit and concurrent)")
	tmp         = flag.String("tmp", "", "directory for Dolt's data (default: the system's temporary directory)")
)

func main() {
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), *deadline)
	defer cancel()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "multidb:", err)
		if cg, ok := readCgroup(); ok {
			// A Dolt the kernel killed shows here, not in the error above.
			fmt.Fprintf(os.Stderr, "multidb: cgroup at exit: %d MiB of %s, oom kills %d\n", cg.current>>20, mib(cg.max), cg.oomKills)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	v, err := dolttest.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%s  dolt %s  %s/%s  %d CPUs  %s\n", time.Now().UTC().Format(time.RFC3339), v, runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	fmt.Print(system())
	fmt.Printf("flags: %s\n\n", strings.Join(os.Args[1:], " "))
	switch *exp {
	case "grants":
		return withServer(ctx, "", func(srv *dolttest.Server, _ string) error {
			first, err := srv.NewDatabase(ctx)
			if err != nil {
				return err
			}
			return grants(ctx, srv, first)
		})
	case "table":
		return withServer(ctx, "", func(srv *dolttest.Server, dir string) error { return table(ctx, srv, dir) })
	case "restart":
		return withServer(ctx, "", func(srv *dolttest.Server, dir string) error { return restart(ctx, srv, dir) })
	case "limit":
		return limit(ctx)
	case "concurrent":
		return concurrent(ctx)
	}
	return fmt.Errorf("unknown -exp %q", *exp)
}

// withServer runs f against a fresh Dolt in a directory of its own, under
// a Go memory limit of memlimit ("" for none), and removes both after.
func withServer(ctx context.Context, memlimit string, f func(srv *dolttest.Server, dir string) error) error {
	dir, err := os.MkdirTemp(*tmp, "multidb")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	opts := dolttest.Options{Metrics: true}
	if memlimit != "" {
		opts.Env = []string{"GOMEMLIMIT=" + memlimit}
	}
	srv, err := dolttest.StartWith(ctx, dir, opts)
	if err != nil {
		return err
	}
	defer func() { _ = srv.Stop() }()
	defer doltLog(dir)
	return f(srv, dir)
}

// doltLog prints what Dolt's log says of its own garbage collection, which
// it starts by itself (behavior.auto_gc_behavior) and which shows in the
// readings as data on disk shrinking: the first twenty such lines.
func doltLog(dir string) {
	b, err := os.ReadFile(filepath.Join(dir, "server.log"))
	if err != nil {
		return
	}
	if *keepLog != "" {
		if err := os.WriteFile(*keepLog, b, 0o600); err != nil {
			fmt.Printf("  dolt's log: not kept: %v\n", err)
		}
	}
	n := 0
	for line := range strings.SplitSeq(string(b), "\n") {
		if l := strings.ToLower(line); strings.Contains(l, "gc") || strings.Contains(l, "garbage") {
			if n++; n <= 20 {
				fmt.Printf("  dolt's log: %s\n", line)
			}
		}
	}
	fmt.Printf("  dolt's log: %d lines about garbage collection\n", n)
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

// table opens stores cumulatively to each count in -projects, loads each
// new one, and reads Dolt's memory along the way: the first spike's table.
func table(ctx context.Context, srv *dolttest.Server, dir string) error {
	fmt.Printf("table: %d issues a store, each with a comment, a label and a claim\n", *issues)
	if err := read(ctx, srv, dir, "server started", 0); err != nil {
		return err
	}
	var open []*store.Store
	var dsns []string
	defer func() {
		for _, s := range open {
			_ = s.Close()
		}
	}()
	for _, f := range strings.Split(*projects, ",") {
		target, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return err
		}
		var opened time.Duration
		var lat []time.Duration
		added := 0
		for len(open) < target {
			dsn, err := srv.NewDatabase(ctx)
			if err != nil {
				return err
			}
			t := time.Now()
			s, err := store.Open(ctx, dsn, store.Options{})
			if err != nil {
				return err
			}
			opened += time.Since(t)
			open, dsns = append(open, s), append(dsns, dsn)
			if _, err := load(ctx, s, *issues, &lat, nil); err != nil {
				return err
			}
			added++
		}
		if added > 0 {
			fmt.Printf("  opened %d: first open (migrating) %v a store; writes %s\n", added,
				(opened / time.Duration(added)).Round(time.Millisecond), summary(lat))
		}
		if err := read(ctx, srv, dir, "open and loaded", len(open)); err != nil {
			return err
		}
	}

	// Idle: do the open stores cost anything once the writes stop?
	if err := pause(ctx, 30*time.Second); err != nil {
		return err
	}
	if err := read(ctx, srv, dir, "idle 30s, all open", len(open)); err != nil {
		return err
	}
	// Close all but one, as the idle close of §15.1 would.
	n := len(open)
	for _, s := range open[1:] {
		if err := s.Close(); err != nil {
			return err
		}
	}
	open = open[:1]
	if err := read(ctx, srv, dir, "all but one closed", 1); err != nil {
		return err
	}
	if err := pause(ctx, 30*time.Second); err != nil {
		return err
	}
	if err := read(ctx, srv, dir, "30s later", 1); err != nil {
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
		open = append(open, s)
	}
	if n > 1 {
		fmt.Printf("  reopened %d: %v a store\n", n-1, (reopen / time.Duration(n-1)).Round(time.Millisecond))
	}
	return read(ctx, srv, dir, "all reopened", len(open))
}

// restart loads -stores stores, restarts Dolt on the same data, and reads
// its memory as the stores are opened, read from and written to again.
func restart(ctx context.Context, srv *dolttest.Server, dir string) error {
	fmt.Printf("restart: %d stores of %d issues, each with a comment, a label and a claim\n", *stores, *issues)
	if err := read(ctx, srv, dir, "server started", 0); err != nil {
		return err
	}
	var open []*store.Store
	closeAll := func() error {
		for _, s := range open {
			if err := s.Close(); err != nil {
				return err
			}
		}
		open = nil
		return nil
	}
	defer func() { _ = closeAll() }()

	var dsns []string
	var sample [][]store.IssueID // of each store, some fifty issues spread over the load
	var lat []time.Duration
	stop := time.Now().Add(*budget)
	written := 0
	for range *stores {
		dsn, err := srv.NewDatabase(ctx)
		if err != nil {
			return err
		}
		s, err := store.Open(ctx, dsn, store.Options{})
		if err != nil {
			return err
		}
		open, dsns = append(open, s), append(dsns, dsn)
		base := written
		ids, err := load(ctx, s, *issues, &lat, func(i int) bool {
			if *every > 0 && i > 0 && i%*every == 0 {
				m := measure(ctx, srv, dir)
				fmt.Printf("  %7d issues  %s  writes so far %s\n", base+i, m, summary(lat))
			}
			return time.Now().Before(stop)
		})
		if err != nil {
			return err
		}
		written += len(ids)
		var some []store.IssueID
		for i := 0; i < len(ids); i += max(1, len(ids)/50) {
			some = append(some, ids[i])
		}
		sample = append(sample, some)
		if len(ids) < *issues {
			fmt.Printf("  the load passed its budget of %v and was stopped: figures below are for what was written\n", *budget)
			break
		}
	}
	fmt.Printf("  loaded %d issues in %d stores; writes %s\n", written, len(open), summary(lat))
	if err := read(ctx, srv, dir, "open and loaded", len(open)); err != nil {
		return err
	}
	if err := pause(ctx, 30*time.Second); err != nil {
		return err
	}
	if err := read(ctx, srv, dir, "idle 30s", len(open)); err != nil {
		return err
	}
	if err := closeAll(); err != nil {
		return err
	}
	if err := read(ctx, srv, dir, "all stores closed", 0); err != nil {
		return err
	}

	t := time.Now()
	if err := srv.Restart(ctx); err != nil {
		return err
	}
	fmt.Printf("  dolt restarted on the same data in %v\n", time.Since(t).Round(time.Millisecond))
	if err := read(ctx, srv, dir, "restarted, at rest", 0); err != nil {
		return err
	}
	t = time.Now()
	for _, dsn := range dsns {
		s, err := store.Open(ctx, dsn, store.Options{})
		if err != nil {
			return err
		}
		open = append(open, s)
	}
	fmt.Printf("  reopened %d: %v a store\n", len(open), (time.Since(t) / time.Duration(len(open))).Round(time.Millisecond))
	if err := read(ctx, srv, dir, "every store open again", len(open)); err != nil {
		return err
	}

	// Reading, as agents do it: what is ready, a page of the list, what is
	// blocked, and a sample of issues shown with their comments. Three
	// rounds, so the second and third show what the first left warm.
	actor := store.Actor{Principal: "bob", Session: "spike", Machine: "localhost"}
	for round := 1; round <= 3; round++ {
		t := time.Now()
		reads := 0
		for i, s := range open {
			if _, err := s.Ready(ctx, actor, 50); err != nil {
				return err
			}
			if _, err := s.List(ctx, store.Filter{}); err != nil {
				return err
			}
			if _, err := s.Blocked(ctx, 50); err != nil {
				return err
			}
			reads += 3
			for _, id := range sample[i] {
				if _, err := s.GetIssue(ctx, id); err != nil {
					return err
				}
				if _, err := s.Comments(ctx, id); err != nil {
					return err
				}
				reads += 2
			}
		}
		fmt.Printf("  read round %d: %d reads, %v each\n", round, reads, (time.Since(t) / time.Duration(reads)).Round(10*time.Microsecond))
		if round != 2 {
			if err := read(ctx, srv, dir, fmt.Sprintf("after %d round(s) of reads", round), len(open)); err != nil {
				return err
			}
		}
	}

	// A few new writes: ten issues a store, forty writes.
	lat = nil
	for _, s := range open {
		if _, err := load(ctx, s, 10, &lat, nil); err != nil {
			return err
		}
	}
	fmt.Printf("  wrote 10 more issues a store; writes %s\n", summary(lat))
	if err := read(ctx, srv, dir, "after 10 more issues a store", len(open)); err != nil {
		return err
	}
	if err := pause(ctx, 30*time.Second); err != nil {
		return err
	}
	if err := read(ctx, srv, dir, "30s later", len(open)); err != nil {
		return err
	}
	if !*gc {
		return nil
	}

	// Dolt's own garbage collection of its storage, in each database, as
	// root, with the stores closed; then a second restart on what is left.
	if err := closeAll(); err != nil {
		return err
	}
	t = time.Now()
	for _, dsn := range dsns {
		root, err := sql.Open("mysql", srv.RootDSN(dsn[strings.LastIndex(dsn, "/")+1:]))
		if err != nil {
			return err
		}
		_, err = root.ExecContext(ctx, "CALL DOLT_GC()")
		_ = root.Close()
		if err != nil {
			return fmt.Errorf("dolt_gc: %w", err)
		}
	}
	fmt.Printf("  CALL DOLT_GC() in %d databases: %v each\n", len(dsns), (time.Since(t) / time.Duration(len(dsns))).Round(time.Millisecond))
	if err := read(ctx, srv, dir, "after dolt_gc, stores closed", 0); err != nil {
		return err
	}
	if err := srv.Restart(ctx); err != nil {
		return err
	}
	if err := read(ctx, srv, dir, "restarted again, at rest", 0); err != nil {
		return err
	}
	for _, dsn := range dsns {
		s, err := store.Open(ctx, dsn, store.Options{})
		if err != nil {
			return err
		}
		open = append(open, s)
	}
	lat = nil
	for i, s := range open {
		if _, err := s.List(ctx, store.Filter{}); err != nil {
			return err
		}
		for _, id := range sample[i] {
			if _, err := s.GetIssue(ctx, id); err != nil {
				return err
			}
		}
		if _, err := load(ctx, s, 10, &lat, nil); err != nil {
			return err
		}
	}
	fmt.Printf("  reopened, read and wrote 10 more issues a store; writes %s\n", summary(lat))
	return read(ctx, srv, dir, "open, read and written again", len(open))
}

// limit loads the same data into a fresh Dolt under each memory limit in
// -limits, -repeat times each, and reports memory, write latency and CPU.
func limit(ctx context.Context) error {
	fmt.Printf("limit: %d stores of %d issues, loaded one after another, under each Go memory limit\n", *stores, *issues)
	for _, f := range strings.Split(*limits, ",") {
		mibs, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return err
		}
		name, env := "none", ""
		if mibs > 0 {
			name, env = fmt.Sprintf("%d MiB", mibs), fmt.Sprintf("%dMiB", mibs)
		}
		for r := 1; r <= *repeat; r++ {
			err := withServer(ctx, env, func(srv *dolttest.Server, dir string) error {
				var open []*store.Store
				defer func() {
					for _, s := range open {
						_ = s.Close()
					}
				}()
				for range *stores {
					dsn, err := srv.NewDatabase(ctx)
					if err != nil {
						return err
					}
					s, err := store.Open(ctx, dsn, store.Options{})
					if err != nil {
						return err
					}
					open = append(open, s)
				}
				before := measure(ctx, srv, dir)
				var lat []time.Duration
				t := time.Now()
				for _, s := range open {
					if _, err := load(ctx, s, *issues, &lat, nil); err != nil {
						return err
					}
				}
				wall := time.Since(t)
				after := measure(ctx, srv, dir)
				fmt.Printf("  limit %-9s run %d: writes %s; wall %v; %s; load %s\n", name, r, summary(lat), wall.Round(time.Second), work(before, after, len(lat), wall), loadavg())
				return read(ctx, srv, dir, "limit "+name+", loaded", len(open))
			})
			if err != nil {
				return fmt.Errorf("limit %s, run %d: %w", name, r, err)
			}
		}
	}
	return nil
}

// concurrent loads n stores at the same time on a fresh Dolt, for each n
// in -concurrency, -repeat times each. Every store has its one writer, so
// what it shows is whether writers in different databases slow each other.
func concurrent(ctx context.Context) error {
	fmt.Printf("concurrent: stores of %d issues, loaded at the same time, one writer each\n", *issues)
	for _, f := range strings.Split(*concurrency, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil {
			return err
		}
		for r := 1; r <= *repeat; r++ {
			err := withServer(ctx, "", func(srv *dolttest.Server, dir string) error {
				var open []*store.Store
				defer func() {
					for _, s := range open {
						_ = s.Close()
					}
				}()
				for range n {
					dsn, err := srv.NewDatabase(ctx)
					if err != nil {
						return err
					}
					s, err := store.Open(ctx, dsn, store.Options{})
					if err != nil {
						return err
					}
					open = append(open, s)
				}
				before := measure(ctx, srv, dir)
				lats := make([][]time.Duration, n)
				errs := make([]error, n)
				var wg sync.WaitGroup
				t := time.Now()
				for i, s := range open {
					wg.Go(func() { _, errs[i] = load(ctx, s, *issues, &lats[i], nil) })
				}
				wg.Wait()
				wall := time.Since(t)
				after := measure(ctx, srv, dir)
				for _, err := range errs {
					if err != nil {
						return err
					}
				}
				all := slices.Concat(lats...)
				// The slowest store's median says whether one writer was
				// starved while the rest ran.
				var slowest time.Duration
				for _, l := range lats {
					slices.Sort(l)
					slowest = max(slowest, l[len(l)/2])
				}
				fmt.Printf("  stores %2d run %d: writes %s, slowest store's p50 %v; wall %v; %.0f writes/s; %s; load %s\n", n, r, summary(all),
					slowest.Round(10*time.Microsecond), wall.Round(100*time.Millisecond), float64(len(all))/wall.Seconds(), work(before, after, len(all), wall), loadavg())
				return read(ctx, srv, dir, fmt.Sprintf("%d loaded at once", n), n)
			})
			if err != nil {
				return fmt.Errorf("%d stores, run %d: %w", n, r, err)
			}
		}
	}
	return nil
}

// load writes n issues to s, each with a comment, a label and a claim
// taken by starting it: four writes an issue. It appends each write's
// latency to lat and returns the issues' ids. more, if not nil, is called
// before each issue with the number written, and stops the load early by
// returning false.
func load(ctx context.Context, s *store.Store, n int, lat *[]time.Duration, more func(written int) bool) ([]store.IssueID, error) {
	actor := store.Actor{Principal: "alice", Session: "spike", Machine: "localhost"}
	body := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 12)
	timed := func(f func() error) error {
		t := time.Now()
		err := f()
		*lat = append(*lat, time.Since(t))
		return err
	}
	ids := make([]store.IssueID, 0, n)
	for i := range n {
		if more != nil && !more(i) {
			break
		}
		var is store.Issue
		err := timed(func() (err error) {
			is, err = s.CreateIssue(ctx, actor, store.NewIssue{Title: fmt.Sprintf("Issue %d of the spike", i), Body: body})
			return err
		})
		if err != nil {
			return ids, err
		}
		for _, f := range []func() error{
			func() error { _, err := s.AddComment(ctx, actor, is.ID, body, ""); return err },
			func() error { return s.AddLabel(ctx, actor, is.ID, "spike", "") },
			func() error { _, _, err := s.StartIssue(ctx, actor, is.ID, time.Hour, false); return err },
		} {
			if err := timed(f); err != nil {
				return ids, err
			}
		}
		ids = append(ids, is.ID)
	}
	return ids, s.Flush(ctx)
}

// summary is the count, median, 99th percentile, mean and maximum of lat.
func summary(lat []time.Duration) string {
	if len(lat) == 0 {
		return "none"
	}
	l := slices.Clone(lat)
	slices.Sort(l)
	var sum time.Duration
	for _, d := range l {
		sum += d
	}
	r := func(d time.Duration) time.Duration { return d.Round(10 * time.Microsecond) }
	return fmt.Sprintf("n %d p50 %v p99 %v mean %v max %v", len(l), r(l[len(l)/2]), r(l[len(l)*99/100]), r(sum/time.Duration(len(l))), r(l[len(l)-1]))
}

// pause waits for d, or less if ctx ends.
func pause(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// read prints a memory reading of srv after the settle pause, with the
// number of open stores and Dolt's count of connections.
func read(ctx context.Context, srv *dolttest.Server, dir, label string, n int) error {
	if err := pause(ctx, *settle); err != nil {
		return err
	}
	root, err := sql.Open("mysql", srv.RootDSN(""))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	rows, err := root.QueryContext(ctx, "SHOW PROCESSLIST")
	if err != nil {
		return err
	}
	conns := -1 // not this connection
	for rows.Next() {
		conns++
	}
	if err := rows.Close(); err != nil {
		return err
	}
	fmt.Printf("  %-30s stores %3d  connections %3d  %s\n", label, n, conns, measure(ctx, srv, dir))
	return nil
}
