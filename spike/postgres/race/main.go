// Command race measures Dolt and Postgres side by side for the Postgres
// spike (spike/postgres/RESULTS.md). It starts the server itself, the
// pinned Dolt through internal/dolttest and the pinned Postgres through
// spike/postgres/pgtest, each with durable settings, and stops it after.
//
// Usage:
//
//	go run ./race -backend dolt|postgres <experiment> [flags]
//
// Experiments:
//
//	claims       stage 0's claim race in raw SQL (spike/dolt/claims.go):
//	             -workers, -pick, -mode tx|auto|skiplocked, -issues, -limit
//	writes       stage 0's write mix in raw SQL (spike/dolt/commits.go),
//	             with Dolt committing once a second as ariesd does:
//	             -workers, -dur
//	storeclaims  the claim race through the store as ariesd runs it:
//	             every worker an agent calling StartIssue for the top
//	             ready issue on one store (one writer): -workers, -issues
//	storewrites  a write mix through the store: an update of an issue's
//	             title and priority, and half the time a comment: -workers,
//	             -dur
//	memory       memory per project: -projects stores opened one after
//	             another, each loaded with 300 issues, a comment, a label
//	             and a claim each, all left open; -layout database|schema
//	             (Postgres only)
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ariesworx/aries/internal/dolttest"
	"github.com/ariesworx/aries/spike/postgres/pgtest"
	"github.com/ariesworx/aries/spike/postgres/store"
)

var (
	backend = flag.String("backend", "postgres", "dolt or postgres")
	// pgsync is Postgres's wal_sync_method. On macOS Go's File.Sync, and so
	// Dolt's, is F_FULLFSYNC, which Postgres does only with
	// fsync_writethrough; its default there leaves writes in the drive's cache.
	pgsync = flag.String("pgsync", "fsync_writethrough", "Postgres wal_sync_method (\"\" for its default)")
)

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: race -backend dolt|postgres claims|writes|storeclaims|storewrites|memory [flags]")
		os.Exit(2)
	}
	ctx := context.Background()
	srv, err := start(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start:", err)
		os.Exit(1)
	}
	load, _ := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	fmt.Printf("# %s %s pgsync=%q load=%s\n", *backend, strings.Join(flag.Args(), " "), *pgsync, strings.TrimSpace(string(load)))
	args := flag.Args()[1:]
	switch flag.Arg(0) {
	case "claims":
		err = runClaims(ctx, srv, args)
	case "writes":
		err = runWrites(ctx, srv, args)
	case "storeclaims":
		err = runStoreClaims(ctx, srv, args)
	case "storewrites":
		err = runStoreWrites(ctx, srv, args)
	case "memory":
		err = runMemory(ctx, srv, args)
	default:
		err = fmt.Errorf("unknown experiment %q", flag.Arg(0))
	}
	if serr := srv.stop(); err == nil {
		err = serr
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// server is one running backend.
type server struct {
	pg   *pgtest.Server
	dolt *dolttest.Server
	dir  string
}

func start(ctx context.Context) (*server, error) {
	switch *backend {
	case "postgres":
		opts := pgtest.Options{Durable: true, Settings: map[string]string{}}
		if *pgsync != "" {
			opts.Settings["wal_sync_method"] = *pgsync
		}
		pg, err := pgtest.Start(ctx, opts)
		return &server{pg: pg}, err
	case "dolt":
		dir, err := os.MkdirTemp("", "ariesdolt-")
		if err != nil {
			return nil, err
		}
		d, err := dolttest.Start(ctx, dir)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		return &server{dolt: d, dir: dir}, nil
	}
	return nil, fmt.Errorf("backend %q", *backend)
}

func (s *server) stop() error {
	if s.pg != nil {
		return s.pg.Stop()
	}
	return errors.Join(s.dolt.Stop(), os.RemoveAll(s.dir))
}

func (s *server) pgOn() bool { return s.pg != nil }

// database makes a project database and returns the store's DSN for it.
func (s *server) database(ctx context.Context, layout string) (string, error) {
	if s.pg != nil {
		if layout == "schema" {
			return s.pg.Schema(ctx)
		}
		return s.pg.Database(ctx)
	}
	return s.dolt.NewDatabase(ctx)
}

// rawDB opens dsn with the plain driver, for the raw SQL experiments.
func (s *server) rawDB(dsn string, conns int) (*sql.DB, error) {
	drv := "pgx"
	if s.pg == nil {
		drv = "mysql"
		dsn += "?parseTime=true"
	}
	db, err := sql.Open(drv, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	return db, nil
}

// rss is the server's resident memory, in MiB, summed over its processes
// (which counts Postgres's shared memory once in each), and its process
// count.
func (s *server) rss() (float64, int) {
	if s.pg != nil {
		b, n, _ := s.pg.RSS()
		return float64(b) / (1 << 20), n
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(s.dolt.PID())).Output()
	if err != nil {
		return 0, 0
	}
	kb, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	return float64(kb) / 1024, 1
}

// dialect holds the statements that differ.
type dialect struct {
	ddl              []string
	now, lease       string
	upsertClaim      string
	ph               func(int) string
	retryable        func(error) bool
	commit           string // Dolt's commit, run once a second by writes
	partition        string
	commentDDL       []string
	eventInsertAfter string
}

func (s *server) dialect() dialect {
	if s.pg != nil {
		return dialect{
			ddl: []string{`CREATE TABLE issues (
  id VARCHAR(32) NOT NULL PRIMARY KEY, title VARCHAR(200) NOT NULL, status VARCHAR(16) NOT NULL,
  priority INT NOT NULL, assignee VARCHAR(96) NULL, rev BIGINT NOT NULL DEFAULT 1, updated_at TIMESTAMP(6) NOT NULL)`,
				`CREATE INDEX ready ON issues (status, priority, id)`,
				`CREATE TABLE claims (issue VARCHAR(32) NOT NULL PRIMARY KEY, holder VARCHAR(96) NOT NULL, epoch BIGINT NOT NULL, expires_at TIMESTAMP(6) NOT NULL)`},
			commentDDL: []string{`CREATE TABLE comments (id BIGSERIAL PRIMARY KEY, issue VARCHAR(32) NOT NULL, author VARCHAR(96) NOT NULL, body TEXT NOT NULL, at TIMESTAMP(6) NOT NULL)`,
				`CREATE INDEX by_issue ON comments (issue)`,
				`CREATE TABLE events (seq BIGSERIAL PRIMARY KEY, at TIMESTAMP(6) NOT NULL, actor VARCHAR(96) NOT NULL, op VARCHAR(32) NOT NULL, target VARCHAR(32) NOT NULL, after JSON NULL)`},
			now:         "now()",
			lease:       "now() + interval '15 minutes'",
			upsertClaim: "INSERT INTO claims (issue, holder, epoch, expires_at) VALUES ($1, $2, 1, now() + interval '15 minutes') ON CONFLICT (issue) DO UPDATE SET holder = EXCLUDED.holder, epoch = claims.epoch + 1, expires_at = EXCLUDED.expires_at",
			ph:          func(i int) string { return "$" + strconv.Itoa(i) },
			retryable: func(err error) bool {
				var pe *pgconn.PgError
				return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
			},
			partition:        " AND CAST(SUBSTRING(id, 4) AS BIGINT) %% %d = %d",
			eventInsertAfter: "json_build_object('rev', $3::bigint)",
		}
	}
	return dialect{
		ddl: []string{`CREATE TABLE issues (
  id VARCHAR(32) NOT NULL PRIMARY KEY, title VARCHAR(200) NOT NULL, status VARCHAR(16) NOT NULL,
  priority INT NOT NULL, assignee VARCHAR(96) NULL, rev BIGINT NOT NULL DEFAULT 1, updated_at DATETIME(6) NOT NULL,
  KEY ready (status, priority, id))`,
			`CREATE TABLE claims (issue VARCHAR(32) NOT NULL PRIMARY KEY, holder VARCHAR(96) NOT NULL, epoch BIGINT NOT NULL, expires_at DATETIME(6) NOT NULL)`},
		commentDDL: []string{`CREATE TABLE comments (id BIGINT AUTO_INCREMENT PRIMARY KEY, issue VARCHAR(32) NOT NULL, author VARCHAR(96) NOT NULL, body TEXT NOT NULL, at DATETIME(6) NOT NULL, KEY by_issue (issue))`,
			`CREATE TABLE events (seq BIGINT AUTO_INCREMENT PRIMARY KEY, at DATETIME(6) NOT NULL, actor VARCHAR(96) NOT NULL, op VARCHAR(32) NOT NULL, target VARCHAR(32) NOT NULL, after JSON NULL)`},
		now:         "NOW(6)",
		lease:       "NOW(6) + INTERVAL 15 MINUTE",
		upsertClaim: "INSERT INTO claims (issue, holder, epoch, expires_at) VALUES (?, ?, 1, NOW(6) + INTERVAL 15 MINUTE) ON DUPLICATE KEY UPDATE holder=VALUES(holder), epoch=epoch+1, expires_at=VALUES(expires_at)",
		ph:          func(int) string { return "?" },
		retryable: func(err error) bool {
			var me *mysql.MySQLError
			return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
		},
		commit:           "CALL DOLT_COMMIT('-Am', 'batch', '--skip-empty')",
		partition:        " AND CAST(SUBSTRING(id, 4) AS UNSIGNED) %% %d = %d",
		eventInsertAfter: "JSON_OBJECT('rev', ?)",
	}
}

func seed(ctx context.Context, db *sql.DB, d dialect, n int) error {
	for _, q := range d.ddl {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	for lo := 0; lo < n; lo += 500 {
		var b strings.Builder
		b.WriteString("INSERT INTO issues (id, title, status, priority, updated_at) VALUES ")
		for i := lo; i < min(lo+500, n); i++ {
			if i > lo {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "('sf-%06d','issue %d','open',%d,%s)", i, i, rand.IntN(5), d.now)
		}
		if _, err := db.ExecContext(ctx, b.String()); err != nil {
			return err
		}
	}
	if d.commit != "" {
		_, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'seed')")
		return err
	}
	return nil
}

type latencies []time.Duration

func (l latencies) pct(p float64) time.Duration {
	if len(l) == 0 {
		return 0
	}
	s := append(latencies(nil), l...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(p*float64(len(s)-1))]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }

func errClass(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code + " " + pe.Message
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		m := me.Message
		if len(m) > 60 {
			m = m[:60]
		}
		return fmt.Sprintf("%d %s", me.Number, m)
	}
	s := err.Error()
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

func printErrs(errs map[string]int) {
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  error x%d: %s\n", errs[k], k)
	}
}

// --- claims (raw SQL) ---

func runClaims(ctx context.Context, srv *server, args []string) error {
	fs := flag.NewFlagSet("claims", flag.ExitOnError)
	workers := fs.Int("workers", 20, "concurrent claimers, one connection each")
	n := fs.Int("issues", 2000, "open issues")
	pick := fs.Int("pick", 1, "pick at random among the top N; 0 partitions the queue")
	mode := fs.String("mode", "tx", "tx | auto | skiplocked (Postgres)")
	limit := fs.Duration("limit", 30*time.Second, "stop after")
	_ = fs.Parse(args)

	dsn, err := srv.database(ctx, "database")
	if err != nil {
		return err
	}
	db, err := srv.rawDB(dsn, *workers+2)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	d := srv.dialect()
	if err := seed(ctx, db, d, *n); err != nil {
		return err
	}
	type stats struct {
		won           []string
		attempts, cas int
		errs          map[string]int
		lat, att      latencies
	}
	deadline := time.Now().Add(*limit)
	all := make([]*stats, *workers)
	var wg sync.WaitGroup
	t0 := time.Now()
	for w := range *workers {
		st := &stats{errs: map[string]int{}}
		all[w] = st
		wg.Go(func() {
			conn, err := db.Conn(ctx)
			if err != nil {
				st.errs["conn"]++
				return
			}
			defer func() { _ = conn.Close() }()
			holder := fmt.Sprintf("worker-%02d/%d", w, rand.Uint32())
			part := ""
			if *pick == 0 {
				part = fmt.Sprintf(d.partition, *workers, w)
			}
			for time.Now().Before(deadline) {
				begin := time.Now()
				done := false
				for time.Now().Before(deadline) {
					a0 := time.Now()
					st.attempts++
					id, ok, empty, err := claimOnce(ctx, conn, d, holder, *mode, max(*pick, 1), part)
					st.att = append(st.att, time.Since(a0))
					if empty {
						return
					}
					if err != nil {
						st.errs[errClass(err)]++
						_, _ = conn.ExecContext(ctx, "ROLLBACK")
						continue
					}
					if !ok {
						st.cas++
						continue
					}
					st.won = append(st.won, id)
					st.lat = append(st.lat, time.Since(begin))
					done = true
					break
				}
				if !done {
					return
				}
			}
		})
	}
	wg.Wait()
	el := time.Since(t0)
	winner := map[string]int{}
	double, attempts, cas := 0, 0, 0
	var lat, att latencies
	errs := map[string]int{}
	for w, st := range all {
		for _, id := range st.won {
			if _, ok := winner[id]; ok {
				double++
			}
			winner[id] = w
		}
		attempts += st.attempts
		cas += st.cas
		lat = append(lat, st.lat...)
		att = append(att, st.att...)
		for k, v := range st.errs {
			errs[k] += v
		}
	}
	var assigned, mismatch int
	rows, err := db.QueryContext(ctx, "SELECT i.id, COALESCE(i.assignee, ''), COALESCE(c.holder, '') FROM issues i LEFT JOIN claims c ON c.issue = i.id")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, a, h string
		if err := rows.Scan(&id, &a, &h); err != nil {
			return err
		}
		w, ok := winner[id]
		if a != "" {
			assigned++
		}
		if ok != (a != "") || (ok && !strings.HasPrefix(a, fmt.Sprintf("worker-%02d/", w))) || (*mode != "auto" && a != h) {
			mismatch++
		}
	}
	_ = rows.Close()
	won := len(winner)
	fmt.Printf("backend=%s mode=%s workers=%d pick=%d issues=%d elapsed=%.1fs\n", *backend, *mode, *workers, *pick, *n, el.Seconds())
	fmt.Printf("claims/s=%.0f won=%d attempts/claim=%.2f failed=%.0f%% cas-lost=%d errors=%d p50=%s p99=%s max=%s (per attempt p50=%s p99=%s)\n",
		float64(won)/el.Seconds(), won, float64(attempts)/max(1, float64(won)), 100*float64(attempts-won)/max(1, float64(attempts)),
		cas, attempts-won-cas, ms(lat.pct(.5)), ms(lat.pct(.99)), ms(lat.pct(1)), ms(att.pct(.5)), ms(att.pct(.99)))
	fmt.Printf("verify: double=%d db-assigned=%d won=%d mismatch=%d\n", double, assigned, won, mismatch)
	printErrs(errs)
	return nil
}

func claimOnce(ctx context.Context, conn *sql.Conn, d dialect, holder, mode string, pick int, part string) (string, bool, bool, error) {
	cas := fmt.Sprintf("UPDATE issues SET assignee = %s, status = 'in_progress', rev = rev + 1, updated_at = %s WHERE id = %s AND rev = %s AND assignee IS NULL",
		d.ph(1), d.now, d.ph(2), d.ph(3))
	if mode == "skiplocked" {
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			return "", false, false, err
		}
		var id string
		var rev int64
		err := conn.QueryRowContext(ctx, "SELECT id, rev FROM issues WHERE status = 'open' AND assignee IS NULL"+part+
			" ORDER BY priority, id LIMIT 1 FOR UPDATE SKIP LOCKED").Scan(&id, &rev)
		if errors.Is(err, sql.ErrNoRows) {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
			return "", false, true, nil
		}
		if err != nil {
			return "", false, false, err
		}
		return finishClaim(ctx, conn, d, cas, holder, id, rev)
	}
	rows, err := conn.QueryContext(ctx, "SELECT id, rev FROM issues WHERE status = 'open' AND assignee IS NULL"+part+
		" ORDER BY priority, id LIMIT "+strconv.Itoa(pick))
	if err != nil {
		return "", false, false, err
	}
	type cand struct {
		id  string
		rev int64
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.rev); err != nil {
			_ = rows.Close()
			return "", false, false, err
		}
		cs = append(cs, c)
	}
	_ = rows.Close()
	if len(cs) == 0 {
		return "", false, true, nil
	}
	c := cs[rand.IntN(len(cs))]
	if mode == "auto" {
		res, err := conn.ExecContext(ctx, cas, holder, c.id, c.rev)
		if err != nil {
			return "", false, false, err
		}
		k, _ := res.RowsAffected()
		return c.id, k == 1, false, nil
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return "", false, false, err
	}
	return finishClaim(ctx, conn, d, cas, holder, c.id, c.rev)
}

func finishClaim(ctx context.Context, conn *sql.Conn, d dialect, cas, holder, id string, rev int64) (string, bool, bool, error) {
	res, err := conn.ExecContext(ctx, cas, holder, id, rev)
	if err != nil {
		return "", false, false, err
	}
	if k, _ := res.RowsAffected(); k != 1 {
		_, err := conn.ExecContext(ctx, "ROLLBACK")
		return id, false, false, err
	}
	if _, err := conn.ExecContext(ctx, d.upsertClaim, id, holder); err != nil {
		return "", false, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", false, false, err
	}
	return id, true, false, nil
}

// --- writes (raw SQL) ---

func runWrites(ctx context.Context, srv *server, args []string) error {
	fs := flag.NewFlagSet("writes", flag.ExitOnError)
	workers := fs.Int("workers", 8, "concurrent writers")
	dur := fs.Duration("dur", 15*time.Second, "run length")
	n := fs.Int("issues", 2000, "issues")
	_ = fs.Parse(args)
	dsn, err := srv.database(ctx, "database")
	if err != nil {
		return err
	}
	db, err := srv.rawDB(dsn, *workers+2)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	d := srv.dialect()
	if err := seed(ctx, db, d, *n); err != nil {
		return err
	}
	for _, q := range d.commentDDL {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	stop := make(chan struct{})
	var cwg sync.WaitGroup
	var commitLat latencies
	if d.commit != "" {
		cwg.Go(func() {
			conn, err := db.Conn(ctx)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
				}
				c0 := time.Now()
				_, _ = conn.ExecContext(ctx, d.commit)
				commitLat = append(commitLat, time.Since(c0))
			}
		})
	}
	var ok, conflicts atomic.Int64
	lats := make([]latencies, *workers)
	errs := make([]map[string]int, *workers)
	deadline := time.Now().Add(*dur)
	var wg sync.WaitGroup
	t0 := time.Now()
	for w := range *workers {
		errs[w] = map[string]int{}
		wg.Go(func() {
			conn, err := db.Conn(ctx)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			actor := fmt.Sprintf("writer-%02d", w)
			for time.Now().Before(deadline) {
				r0 := time.Now()
				err := writeOnce(ctx, conn, d, actor, *n)
				lats[w] = append(lats[w], time.Since(r0))
				if err != nil {
					_, _ = conn.ExecContext(ctx, "ROLLBACK")
					errs[w][errClass(err)]++
					if d.retryable(err) {
						conflicts.Add(1)
					}
					continue
				}
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	el := time.Since(t0)
	close(stop)
	cwg.Wait()
	var all latencies
	allErrs := map[string]int{}
	for w := range *workers {
		all = append(all, lats[w]...)
		for k, v := range errs[w] {
			allErrs[k] += v
		}
	}
	fmt.Printf("backend=%s workers=%d dur=%.1fs requests/s=%.0f ok=%d p50=%s p99=%s max=%s retryable-errors=%d\n",
		*backend, *workers, el.Seconds(), float64(ok.Load())/el.Seconds(), ok.Load(), ms(all.pct(.5)), ms(all.pct(.99)), ms(all.pct(1)), conflicts.Load())
	if len(commitLat) > 0 {
		fmt.Printf("  DOLT_COMMIT each second: p50=%s p99=%s\n", ms(commitLat.pct(.5)), ms(commitLat.pct(.99)))
	}
	printErrs(allErrs)
	return nil
}

var errCASLost = errors.New("cas lost")

func writeOnce(ctx context.Context, conn *sql.Conn, d dialect, actor string, n int) error {
	id := fmt.Sprintf("sf-%06d", rand.IntN(n))
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	var rev int64
	if err := conn.QueryRowContext(ctx, "SELECT rev FROM issues WHERE id = "+d.ph(1), id).Scan(&rev); err != nil {
		return err
	}
	res, err := conn.ExecContext(ctx, fmt.Sprintf("UPDATE issues SET priority = %s, title = %s, rev = rev + 1, updated_at = %s WHERE id = %s AND rev = %s",
		d.ph(1), d.ph(2), d.now, d.ph(3), d.ph(4)), rand.IntN(5), "edited by "+actor, id, rev)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k != 1 {
		return errCASLost
	}
	if rand.IntN(2) == 0 {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO comments (issue, author, body, at) VALUES (%s, %s, %s, %s)", d.ph(1), d.ph(2), d.ph(3), d.now),
			id, actor, "a comment of moderate length, about as long as an agent's note"); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO events (at, actor, op, target, after) VALUES (%s, %s, 'update', %s, %s)", d.now, d.ph(1), d.ph(2), d.eventInsertAfter),
		actor, id, rev+1); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return err
}

// --- through the store ---

func openStore(ctx context.Context, dsn string) (*store.Store, error) {
	return store.Open(ctx, dsn, store.Options{Prefix: "sf", Admins: []string{"dana"}, AllowUnsafeAccount: true})
}

func actor(i int) store.Actor {
	return store.Actor{Principal: fmt.Sprintf("agent%02d", i), Session: fmt.Sprintf("sess-%02d", i), Machine: "host"}
}

func runStoreClaims(ctx context.Context, srv *server, args []string) error {
	fs := flag.NewFlagSet("storeclaims", flag.ExitOnError)
	workers := fs.Int("workers", 20, "agents")
	n := fs.Int("issues", 1000, "open issues")
	_ = fs.Parse(args)
	dsn, err := srv.database(ctx, "database")
	if err != nil {
		return err
	}
	s, err := openStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	for i := range *n {
		p := store.Priority(rand.IntN(5))
		if _, err := s.CreateIssue(ctx, actor(99), store.NewIssue{Title: fmt.Sprintf("issue %d", i), Priority: &p}); err != nil {
			return err
		}
	}
	won := make([][]store.IssueID, *workers)
	lats := make([]latencies, *workers)
	errs := make([]map[string]int, *workers)
	var wg sync.WaitGroup
	t0 := time.Now()
	for w := range *workers {
		errs[w] = map[string]int{}
		wg.Go(func() {
			a := actor(w)
			for {
				r0 := time.Now()
				is, _, err := s.StartIssue(ctx, a, "", 15*time.Minute, false)
				if errors.Is(err, store.ErrNothingReady) {
					return
				}
				if err != nil {
					errs[w][errClass(err)]++
					continue
				}
				lats[w] = append(lats[w], time.Since(r0))
				won[w] = append(won[w], is.ID)
			}
		})
	}
	wg.Wait()
	el := time.Since(t0)
	seen := map[store.IssueID]int{}
	double, total := 0, 0
	var all latencies
	allErrs := map[string]int{}
	for w := range *workers {
		for _, id := range won[w] {
			if _, ok := seen[id]; ok {
				double++
			}
			seen[id] = w
			total++
		}
		all = append(all, lats[w]...)
		for k, v := range errs[w] {
			allErrs[k] += v
		}
	}
	mismatch := 0
	for id, w := range seen {
		c, err := s.ClaimOf(ctx, id)
		if err != nil || c == nil || c.Holder.Principal != actor(w).Principal || c.Epoch != 1 {
			mismatch++
		}
	}
	fmt.Printf("backend=%s storeclaims workers=%d issues=%d elapsed=%.1fs claims/s=%.0f won=%d p50=%s p99=%s max=%s double=%d claim-mismatch=%d\n",
		*backend, *workers, *n, el.Seconds(), float64(total)/el.Seconds(), total, ms(all.pct(.5)), ms(all.pct(.99)), ms(all.pct(1)), double, mismatch)
	printErrs(allErrs)
	return nil
}

func runStoreWrites(ctx context.Context, srv *server, args []string) error {
	fs := flag.NewFlagSet("storewrites", flag.ExitOnError)
	workers := fs.Int("workers", 8, "agents")
	dur := fs.Duration("dur", 15*time.Second, "run length")
	n := fs.Int("issues", 500, "issues")
	_ = fs.Parse(args)
	dsn, err := srv.database(ctx, "database")
	if err != nil {
		return err
	}
	s, err := openStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	ids := make([]store.IssueID, *n)
	for i := range *n {
		is, err := s.CreateIssue(ctx, actor(99), store.NewIssue{Title: fmt.Sprintf("issue %d", i)})
		if err != nil {
			return err
		}
		ids[i] = is.ID
	}
	lats := make([]latencies, *workers)
	errs := make([]map[string]int, *workers)
	var ok, conflicts atomic.Int64
	deadline := time.Now().Add(*dur)
	var wg sync.WaitGroup
	t0 := time.Now()
	for w := range *workers {
		errs[w] = map[string]int{}
		wg.Go(func() {
			a := actor(w)
			for time.Now().Before(deadline) {
				id := ids[rand.IntN(len(ids))]
				r0 := time.Now()
				title := fmt.Sprintf("edited by %s at %d", a.Principal, r0.UnixNano())
				p := store.Priority(rand.IntN(5))
				var err error
				for range 10 { // another agent may update it between the read and the write
					var cur store.Issue
					if cur, err = s.GetIssue(ctx, id); err != nil {
						break
					}
					if _, err = s.UpdateIssue(ctx, a, id, cur.Rev, store.IssuePatch{Title: &title, Priority: &p}); !errors.Is(err, store.ErrConflict) {
						break
					}
					conflicts.Add(1)
				}
				if err == nil && rand.IntN(2) == 0 {
					_, err = s.AddComment(ctx, a, id, "a comment of moderate length, about as long as an agent's note", "")
				}
				lats[w] = append(lats[w], time.Since(r0))
				if err != nil {
					errs[w][errClass(err)]++
					continue
				}
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	el := time.Since(t0)
	var all latencies
	allErrs := map[string]int{}
	for w := range *workers {
		all = append(all, lats[w]...)
		for k, v := range errs[w] {
			allErrs[k] += v
		}
	}
	fmt.Printf("backend=%s storewrites workers=%d dur=%.1fs requests/s=%.0f ok=%d p50=%s p99=%s max=%s cas-retries=%d\n",
		*backend, *workers, el.Seconds(), float64(ok.Load())/el.Seconds(), ok.Load(), ms(all.pct(.5)), ms(all.pct(.99)), ms(all.pct(1)), conflicts.Load())
	printErrs(allErrs)
	return nil
}

// --- memory ---

func runMemory(ctx context.Context, srv *server, args []string) error {
	fs := flag.NewFlagSet("memory", flag.ExitOnError)
	projects := fs.Int("projects", 50, "stores")
	layout := fs.String("layout", "database", "database | schema (Postgres)")
	per := fs.Int("issues", 300, "issues per store")
	readers := fs.Int("readers", 0, "the store's read pool (0: its default, 8)")
	_ = fs.Parse(args)
	report := func(label string) {
		time.Sleep(5 * time.Second)
		mib, procs := srv.rss()
		fmt.Printf("%-28s rss=%7.0f MiB footprint=%7.0f MiB processes=%d\n", label, mib, srv.footprint(), procs)
	}
	fmt.Printf("backend=%s layout=%s issues/store=%d\n", *backend, *layout, *per)
	report("server started")
	body := strings.Repeat("x", 540)
	var stores []*store.Store
	defer func() {
		for _, s := range stores {
			_ = s.Close()
		}
	}()
	marks := map[int]bool{1: true, 5: true, 10: true, 20: true, 50: true, 100: true, 200: true, 500: true}
	for p := 1; p <= *projects; p++ {
		dsn, err := srv.database(ctx, *layout)
		if err != nil {
			return err
		}
		s, err := store.Open(ctx, dsn, store.Options{Prefix: "sf", Admins: []string{"dana"}, AllowUnsafeAccount: true, Readers: *readers})
		if err != nil {
			return err
		}
		stores = append(stores, s)
		a := actor(1)
		for i := range *per {
			is, err := s.CreateIssue(ctx, a, store.NewIssue{Title: fmt.Sprintf("issue %d", i), Body: body})
			if err != nil {
				return err
			}
			if _, err := s.AddComment(ctx, a, is.ID, body, ""); err != nil {
				return err
			}
			if err := s.AddLabel(ctx, a, is.ID, "area", ""); err != nil {
				return err
			}
			if _, _, err := s.StartIssue(ctx, a, is.ID, 15*time.Minute, false); err != nil {
				return err
			}
		}
		// A read on each, so the read pool has opened a connection.
		if _, err := s.Ready(ctx, a, 10); err != nil {
			return err
		}
		if marks[p] || p == *projects {
			report(fmt.Sprintf("%d stores open", p))
		}
	}
	for _, s := range stores {
		_ = s.Close()
	}
	stores = nil
	report("all stores closed")
	return nil
}

// footprint is macOS's physical footprint of the server's processes
// together, in MiB, from footprint(1): memory a process dirtied, with
// what they share counted once. 0 where footprint is missing.
func (s *server) footprint() float64 {
	pids := []string{}
	if s.pg != nil {
		pids = append(pids, strconv.Itoa(s.pg.PID()))
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(s.pg.PID())).Output()
		pids = append(pids, strings.Fields(string(out))...)
	} else {
		pids = append(pids, strconv.Itoa(s.dolt.PID()))
	}
	args := []string{"-f", "bytes", "--noCategories"}
	for _, p := range pids {
		args = append(args, "-p", p)
	}
	out, err := exec.Command("footprint", args...).Output()
	if err != nil && len(out) == 0 {
		return 0
	}
	var total, single float64
	for line := range strings.SplitSeq(string(out), "\n") {
		if _, after, ok := strings.Cut(line, "Summary Footprint: "); ok {
			total, _ = strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(after), " B"), 64)
		}
		if _, after, ok := strings.Cut(line, "Footprint: "); ok && single == 0 {
			f := strings.Fields(after)
			if len(f) > 0 {
				single, _ = strconv.ParseFloat(f[0], 64)
			}
		}
	}
	if total == 0 {
		total = single
	}
	return total / (1 << 20)
}
