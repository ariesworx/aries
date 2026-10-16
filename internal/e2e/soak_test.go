package e2e

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// TestSoak is the gate of stages 3 and 3a (design §13): many agent
// sessions, across several principals, work the backlogs of two projects
// on one server at once through the real stack, under churn and failure,
// and every stage 3 guarantee must hold throughout, in each project from
// its own event log. Sessions vanish without finishing, connections drop
// between a request being applied and its answer arriving, and the daemon
// restarts mid-run. The store's clock runs fast, so leases lapse and the
// reaper ends them many times over.
//
// Meanwhile an admin ends and restores access (soak_access_test.go):
// memberships, principals, keys, a project admin's role and a whole
// project, and a membership of a third, quiet project while its store is
// closed. Whoever loses access must lose their connections and their
// claims, and nothing of one project may show through another.
//
// Beside them, sessions contend for a mutex and a semaphore
// (soak_locks_test.go), the gate of stage 6's slice G2: no grant past a
// lock's capacity, grants in the order of the live waiters, epochs that
// only rise, and a dead holder's lock passed on soon after its lease.
//
// A short run is part of `go test`. Set ARIES_SOAK to a duration, such
// as 10m, for a long one, and ARIES_SOAK_SEED to replay a run's choices;
// the seed is printed at the start and with any failure.
// ARIES_SOAK_TELEMETRY turns the daemon's telemetry on for the run
// (soak_telemetry_test.go): check holds its instruments and spans to
// the event logs, and refused and hang give it a collector that is not
// there. ARIES_SOAK_PARTITION=1 cuts agents off from the server for
// minutes at a time, the gate of stage 5 (soak_partition_test.go). See
// CONTRIBUTING.md, "The soak test".
func TestSoak(t *testing.T) {
	// Serial: counts the process's goroutines and connections (checkLeaks); it is concurrent inside.
	cfg, err := soakConfigFrom(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("soak: seed %d, %s of work, %d principals × %d sessions in 2 projects, store clock ×%g; replay with ARIES_SOAK_SEED=%d",
		cfg.seed, cfg.duration, len(cfg.principals), cfg.sessions, cfg.speed, cfg.seed)
	if cfg.telemetry != telOff {
		t.Logf("soak: telemetry on (ARIES_SOAK_TELEMETRY=%s)", cfg.telemetry)
	}
	if cfg.partition {
		t.Logf("soak: partition mode on (ARIES_SOAK_PARTITION): agents are cut off from the server for %s to %s at a time", cutMin, cutMax)
	}
	s := newSoak(t, cfg)
	s.run()
	s.verify()
	s.checkTelemetry()
	s.checkDeadCollector()
	s.report()
	s.explain()
}

// soakConfig shapes one run.
type soakConfig struct {
	seed       uint64
	duration   time.Duration // how long the sessions work
	principals []string
	// admin is the server admin who works beside the others, and root
	// the one who manages access while they do.
	admin, root string
	// sessions per principal; the first of each is an agent running `aries
	// mcp`, the others use the client library directly.
	sessions int
	// speed is how many times faster than real time the store's clock
	// runs.
	speed float64
	// lease is the range of leases the client library sessions ask for,
	// on the store's clock; `aries mcp` asks for its own 15m.
	leaseMin, leaseMax time.Duration
	// vanish is the chance, each step, that a session dies without a word.
	vanish float64
	// stall is the chance, each step, that a session stops past its
	// lease and then carries on with the claims it believes it holds.
	stall float64
	// think is the most a session pauses between steps, in real time.
	think time.Duration
	// restarts is how many times the daemon restarts, evenly spread.
	restarts int
	// limits are the daemon's limits, low enough that the workload meets
	// each cap.
	limits server.Limits
	// target is the backlog of unclosed issues the workload keeps near.
	target int
	// telemetry is what the daemon's telemetry does in the run: telOff,
	// telCheck, telRefused or telHang.
	telemetry string
	// partition cuts agents off from the server now and then, as they
	// go on working (soak_partition_test.go).
	partition bool
}

// soakConfigFrom reads ARIES_SOAK (a duration; empty runs the short
// soak), ARIES_SOAK_SEED (empty picks one) and ARIES_SOAK_TELEMETRY
// (check, refused or hang; empty leaves telemetry off).
func soakConfigFrom(getenv func(string) string) (soakConfig, error) {
	cfg := soakConfig{
		seed:     rand.Uint64(), //nolint:gosec // a seed to print and replay, not a secret
		duration: 12 * time.Second, principals: []string{"alice", "bob", "carol", "dave", "erin"},
		admin: "dave", root: "root", sessions: 5, speed: 120, leaseMin: time.Minute, leaseMax: 3 * time.Minute,
		vanish: 0.004, stall: 0.004, think: 20 * time.Millisecond, restarts: 1, target: 120,
		limits: server.Limits{
			Limits: store.Limits{Labels: 6, AcceptanceItems: 5, Deps: 6, Sessions: 10, InboxUnread: 40,
				UsageRecords: 20, UsagePerDay: 4000, Paths: 12},
			WriteRate: 100, WriteBurst: 100, ControlWriteRate: 100, ControlWriteBurst: 100,
			// Not met: every principal of the run connects from loopback,
			// whose one bucket of refused sign-ins stands for one
			// machine's. A removed key's sessions, still trying, would
			// empty it and shut out every other principal, the probe
			// that checks the key is refused among them. TestHTTPSLimits
			// holds the bucket to its limit.
			AuthFailureRate: 100_000, AuthFailureBurst: 100_000,
			// A store with no connection closes soon, so the quiet
			// project's does, and every store does once the run is over.
			StoreIdle: server.Duration(300 * time.Millisecond),
		},
	}
	if v := getenv("ARIES_SOAK"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return soakConfig{}, fmt.Errorf("ARIES_SOAK=%q: want a duration such as 10m", v)
		}
		cfg.duration = d
		cfg.restarts = max(1, int(d/(2*time.Minute)))
	}
	if v := getenv("ARIES_SOAK_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return soakConfig{}, fmt.Errorf("ARIES_SOAK_SEED=%q: want an unsigned integer", v)
		}
		cfg.seed = n
	}
	var err error
	if cfg.partition, err = partitionFrom(getenv("ARIES_SOAK_PARTITION")); err != nil {
		return soakConfig{}, err
	}
	switch v := getenv("ARIES_SOAK_TELEMETRY"); v {
	case telOff, telCheck, telRefused, telHang:
		cfg.telemetry = v
	default:
		return soakConfig{}, fmt.Errorf("ARIES_SOAK_TELEMETRY=%q: want %s, %s or %s, or nothing for no telemetry", v, telCheck, telRefused, telHang)
	}
	return cfg, nil
}

// vclock is the machine's clocks as the daemon reads them: they run
// speed times faster than real time from when the run starts, so a
// one-minute lease lapses in half a second. The wall clock can be set
// (step, in soak_clock_test.go); the monotonic clock only runs.
type vclock struct {
	base  time.Time
	start time.Time // real, with its monotonic reading
	speed float64

	mu sync.Mutex
	// offset is what the wall clock has been set by, in all; lead is how
	// far a store's time may be ahead of the wall clock, as of leadFrom
	// on the monotonic clock.
	offset, lead, leadFrom time.Duration
}

// elapsed is the monotonic clock: the time since the run started.
func (c *vclock) elapsed() time.Duration {
	return time.Duration(float64(time.Since(c.start)) * c.speed)
}

// read is both clocks from one reading, for the daemon's store.Clock.
func (c *vclock) read() (time.Time, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.elapsed()
	return c.base.Add(m + c.offset).UTC().Truncate(time.Microsecond), m
}

// now is the time of the daemon's stores, as the run reckons it: the
// wall clock, or, while that is behind what the stores had reached when
// it was set back, where their time has run on to since.
func (c *vclock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.elapsed()
	return c.base.Add(m + c.offset + c.leadAt(m)).UTC().Truncate(time.Microsecond)
}

// real is how long d on the store's clock takes in real time.
func (c *vclock) real(d time.Duration) time.Duration { return time.Duration(float64(d) / c.speed) }

// sproj is one project of the run: its database as the checker reads
// it, the replay of its own event log, and what the workload knows of
// it.
type sproj struct {
	id, name, prefix string
	db               *sql.DB // the checker's own read-only use of the project's database
	rep              *replay
	pool             pool
	holding          holding
	// foreign matches an issue id of another project of the run.
	foreign *regexp.Regexp
	// marks are the last event of the project's log at each daemon
	// restart (soak_locks_test.go).
	marks []int64
}

// soak is one run.
type soak struct {
	t     *testing.T
	cfg   soakConfig
	clock *vclock
	w     *world
	// a and b are the two projects the sessions work in, and c the quiet
	// one, which has a connection only when the admin gives it one;
	// projs is all three.
	a, b, c *sproj
	projs   []*sproj
	// cdb is the checker's read-only use of the control database, and
	// ctl the replay of its event log.
	cdb  *sql.DB
	ctl  *ctlReplay
	mon  *monitor
	logs *logCounter
	// tel is the daemon's telemetry, which is off unless the run asks
	// for it (soak_telemetry_test.go).
	tel *soakTel
	// users holds each principal's repository for each project it may
	// reach, all with the principal's one key; storeKey is the
	// fingerprint of a key that only the control store holds.
	users    map[string]map[*sproj]*user
	storeKey map[string]string
	eve      map[*sproj]*user // reads for the checks, and does nothing else
	// open holds the sessions' connections, for the access checks.
	open connSet
	// ctlKnown is the last event of the control log that the server has
	// answered an admin for (answered).
	ctlKnown atomic.Int64
	acc      accessStats
	// shaken counts the times the chaos was about to close connections
	// that no admin's change closed: a drop of them all, and a restart.
	shaken atomic.Int64
	// restarting counts the daemon restarts begun.
	restarting atomic.Int64

	mu    sync.Mutex // guards live and every
	live  []worker   // the sessions still at work when the run ended
	every []worker   // every session the run started
	stats struct {
		drops, dropAlls, restarts, stalls, rewatches     atomic.Int64
		heapPeak                                         atomic.Uint64
		goroutinePeak                                    atomic.Int64
		snapshots                                        atomic.Int64
		windowsChecked, windowsResynced                  atomic.Int64
		eventWindowsChecked, eventsChecked, eventsPushed atomic.Int64
		claimsChecked                                    atomic.Int64
	}
	// clockStats counts the clock chaos (soak_clock_test.go).
	clockStats clockStats
	// part is the partition mode's, nil when it is off
	// (soak_partition_test.go).
	part *partition
	// locks counts what the lock sessions did (soak_locks_test.go).
	locks    lockStats
	baseline int                        // goroutines before any session started
	connBase map[string]map[uint64]bool // connGoroutines then
	started  time.Time
	ended    time.Duration // real length of the work
}

// user returns principal's repository for project p.
func (s *soak) user(principal string, p *sproj) *user { return s.users[principal][p] }

// projectOf returns the project whose prefix an issue id has.
func (s *soak) projectOf(id string) *sproj {
	for _, p := range s.projs {
		if strings.HasPrefix(id, p.prefix+"-") {
			return p
		}
	}
	return nil
}

func newSoak(t *testing.T, cfg soakConfig) *soak {
	clock := &vclock{base: time.Now().UTC().Truncate(time.Second), start: time.Now(), speed: cfg.speed}
	s := &soak{t: t, cfg: cfg, clock: clock, mon: newMonitor(), logs: &logCounter{}, users: map[string]map[*sproj]*user{},
		storeKey: map[string]string{}, eve: map[*sproj]*user{}}
	s.logs.audit = cfg.telemetry == telCheck
	if cfg.partition {
		s.part = &partition{}
	}
	// Before the world, so that it stops after the daemon has.
	s.tel = newSoakTel(t, cfg.telemetry, slog.New(s.logs))
	// A span may carry no principal's name, and no session id, which
	// every session of the run starts with lib- or mcp-.
	names := append([]string{cfg.admin, cfg.root, "eve"}, cfg.principals...)
	s.tel.people = func(v string) bool {
		return slices.Contains(names, v) || strings.HasPrefix(v, "lib-") || strings.HasPrefix(v, "mcp-") || strings.HasPrefix(v, "lock-")
	}
	s.w = newWorld(t, daemonOpts{now: clock.now, clock: clock.read, reap: 100 * time.Millisecond,
		admins: []string{cfg.admin, cfg.root}, limits: cfg.limits, logger: slog.New(s.logs), telemetry: s.tel.tel,
		many: true, keys: true})
	// Before anything connects: the stores the setup opens close again.
	s.baseline, s.connBase = runtime.NumGoroutine(), connGoroutines()
	s.setup()
	return s
}

// openCheckerDB opens the checker's own connection to the store's
// schema, which it only reads. Its transactions run at repeatable read,
// as the store's do, so that a snapshot reads the event log and the
// tables at one moment: at read committed, each statement would see the
// writes committed before it began.
func openCheckerDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("default_transaction_isolation", "repeatable read")
	u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20") // pgx reads a + as itself
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// run starts every session, the chaos and the sampler, lets them work for
// the configured time, then brings the run to rest: sessions stop, their
// claims lapse and are reaped, and every live watch is checked.
func (s *soak) run() {
	t := s.t
	ctx, stop := context.WithTimeout(context.Background(), s.cfg.duration)
	defer stop()
	s.started = time.Now()
	var wg sync.WaitGroup
	for i, name := range s.cfg.principals {
		// A principal's sessions take its projects in turn.
		var in []*sproj
		for _, p := range []*sproj{s.a, s.b} {
			if s.user(name, p) != nil {
				in = append(in, p)
			}
		}
		for slot := range s.cfg.sessions {
			seed := s.cfg.seed + uint64(i*100+slot)
			p := in[slot%len(in)]
			wg.Go(func() { s.slot(ctx, name, p, slot, seed) })
		}
	}
	wg.Go(func() { s.lockSlots(ctx) })
	var side sync.WaitGroup
	side.Go(func() { s.chaos(ctx) })
	side.Go(func() { s.sample(ctx) })
	side.Go(func() { s.conduct(ctx) })
	wg.Wait()
	side.Wait()
	s.ended = time.Since(s.started)
	s.w.dropReplyTo("")

	drain, stopDrain := context.WithTimeout(context.Background(), 15*time.Minute)
	s.drain(drain)
	stopDrain()
	settle, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, a := range s.agents() {
		a.settle(settle)
	}
	for _, p := range s.projs {
		s.checkClaimsOp(settle, p)
		s.awaitReaped(p)
	}
	s.awaitLocksEnded(settle)
	for _, a := range s.agents() {
		if ln, c := a.liveLine(); ln != nil {
			s.awaitWindow(a.base(), ln, c, bound{})
		}
	}
	if err := s.snapshot(); err != nil {
		t.Errorf("final snapshot: %v", err)
	}
}

// slot keeps one session of principal p at work in project proj: when it
// vanishes, a new session, with a new id, takes its place.
func (s *soak) slot(ctx context.Context, p string, proj *sproj, slot int, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // a reproducible workload, not security
	for gen := 0; ctx.Err() == nil; gen++ {
		tag := fmt.Sprintf("%s-%dg%d", p, slot, gen)
		sub := rand.New(rand.NewPCG(rng.Uint64(), rng.Uint64())) //nolint:gosec // a reproducible workload, not security
		base := &session{s: s, p: proj, tag: tag, u: s.user(p, proj), rng: sub,
			lease: s.cfg.leaseMin + time.Duration(rng.Int64N(int64(s.cfg.leaseMax-s.cfg.leaseMin)+1)).Truncate(time.Minute)}
		var a interface {
			worker
			run(context.Context) bool
		}
		if slot == 0 {
			base.key = sessKey{p, "mcp-" + tag}
			base.lease = 15 * time.Minute
			base.renewEvery = s.clock.real(base.lease) / 8
			ms := &mcpSession{session: base, held: map[string]int{}}
			if err := ms.open(); err != nil {
				s.mon.fail("", "%s: start aries mcp: %v", base.key, err)
				return
			}
			a = ms
		} else {
			base.key = sessKey{p, "lib-" + tag}
			base.renewEvery = s.clock.real(base.lease) / 4
			base.events = slot == 1 || slot == 2 // one board in each of the principal's projects
			a = &libSession{session: base, held: map[string]heldIssue{}, revs: map[string]int64{}}
		}
		s.mu.Lock()
		s.every = append(s.every, a)
		s.mu.Unlock()
		if a.run(ctx) {
			s.mu.Lock()
			s.live = append(s.live, a)
			s.mu.Unlock()
			return
		}
	}
}

func (s *soak) agents() []worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.live)
}

// createWeight steers creates so p's backlog stays near its target.
func (s *soak) createWeight(p *sproj) int {
	open := 0
	p.rep.mu.Lock()
	open = p.rep.openCount
	p.rep.mu.Unlock()
	switch {
	case open < s.cfg.target/2:
		return 16
	case open < s.cfg.target:
		return 8
	}
	return 1
}

// chaos drops answers after the server applied the request, drops every
// connection now and then, and restarts the daemon at even intervals.
func (s *soak) chaos(ctx context.Context) {
	rng := rand.New(rand.NewPCG(s.cfg.seed, 7)) //nolint:gosec // reproducible chaos, not security
	ops := []string{proto.OpCreate, proto.OpComment, proto.OpFinish, proto.OpHandoff, proto.OpStart, proto.OpRenew,
		proto.OpUsage, proto.OpUpdate, proto.OpLabelAdd, proto.OpDepAdd, proto.OpAccept, proto.OpClose,
		proto.OpWatch, proto.OpInbox, proto.OpAck, proto.OpReady, proto.OpShow,
		proto.OpLockAcquire, proto.OpLockRenew, proto.OpLockRelease, proto.OpLockLeave,
		// What the admin sends too: an access change whose answer is lost
		// was made all the same.
		proto.OpMemberRemove, proto.OpMemberSet, proto.OpPrincipalDisable, proto.OpPrincipalEnable, proto.OpKeyRemove,
		proto.OpKeyAdd, proto.OpProjectArchive, proto.OpProjectActivate, proto.OpStatus}
	every := s.cfg.duration / time.Duration(s.cfg.restarts+1)
	nextRestart := time.Now().Add(every)
	nextDropAll := time.Now().Add(time.Duration(1000+rng.IntN(2000)) * time.Millisecond)
	nextStep := time.Now().Add(time.Duration(1000+rng.IntN(2000)) * time.Millisecond)
	for ctx.Err() == nil {
		pause(ctx, time.Duration(100+rng.IntN(300))*time.Millisecond)
		if ctx.Err() != nil {
			return
		}
		s.w.dropReplyTo(ops[rng.IntN(len(ops))])
		s.stats.drops.Add(1)
		if time.Now().After(nextDropAll) {
			s.shaken.Add(1)
			s.w.dropAll()
			s.stats.dropAlls.Add(1)
			nextDropAll = time.Now().Add(time.Duration(1000+rng.IntN(2000)) * time.Millisecond)
		}
		if int(s.stats.restarts.Load()) < s.cfg.restarts && time.Now().After(nextRestart) {
			s.shaken.Add(1)
			s.restarting.Add(1)
			if err := s.restartDaemon(rng); err != nil {
				s.mon.fail("", "restart the daemon: %v", err)
				return
			}
			s.stats.restarts.Add(1)
			nextRestart = time.Now().Add(every)
		}
		if time.Now().After(nextStep) && ctx.Err() == nil {
			s.stepClock(ctx, rng)
			nextStep = time.Now().Add(time.Duration(1500+rng.IntN(2500)) * time.Millisecond)
		}
	}
}

// sample watches memory and goroutines, and every few seconds compares
// the replayed event log with the tables in one snapshot.
func (s *soak) sample(ctx context.Context) {
	last := time.Now()
	for ctx.Err() == nil {
		pause(ctx, 500*time.Millisecond)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > s.stats.heapPeak.Load() {
			s.stats.heapPeak.Store(ms.HeapAlloc)
		}
		if n := int64(runtime.NumGoroutine()); n > s.stats.goroutinePeak.Load() {
			s.stats.goroutinePeak.Store(n)
		}
		s.tel.drain()
		if time.Since(last) >= 3*time.Second && ctx.Err() == nil {
			last = time.Now()
			if err := s.snapshot(); err != nil {
				s.t.Logf("soak: snapshot: %v", err)
			}
		}
	}
}

// logCounter is a slog handler that counts the daemon's warnings and
// errors by line, for the report. When audit is set, before the daemon
// starts, it also keeps the lines of the audit stream, which are at
// info (soak_telemetry_test.go).
type logCounter struct {
	audit bool

	mu     sync.Mutex
	n      int
	lines  map[string]int
	audits []auditLine
}

func (l *logCounter) Enabled(_ context.Context, lv slog.Level) bool {
	return lv >= slog.LevelWarn || l.audit && lv == slog.LevelInfo
}

func (l *logCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Level < slog.LevelWarn {
		l.keepAudit(r)
		return nil
	}
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	if l.lines == nil {
		l.lines = map[string]int{}
	}
	l.lines[b.String()]++
	return nil
}

func (l *logCounter) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// top returns the most frequent lines, at most n, with their counts.
func (l *logCounter) top(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := slices.SortedFunc(maps.Keys(l.lines), func(a, b string) int {
		return cmp.Or(cmp.Compare(l.lines[b], l.lines[a]), cmp.Compare(a, b))
	})
	var out []string
	for _, k := range keys[:min(n, len(keys))] {
		out = append(out, fmt.Sprintf("%5d× %s", l.lines[k], k))
	}
	return out
}
func (l *logCounter) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logCounter) WithGroup(string) slog.Handler      { return l }

// report logs throughput and latency per op, and what the run did. They
// are reported, not asserted: only a hang fails the run.
func (s *soak) report() {
	calls := s.mon.calls()
	type opStat struct {
		n, acked, refused, lost int
		took                    []time.Duration
	}
	byOp := map[string]*opStat{}
	for _, c := range calls {
		st := byOp[c.op]
		if st == nil {
			st = &opStat{}
			byOp[c.op] = st
		}
		st.n++
		st.took = append(st.took, c.took)
		switch c.out {
		case acked:
			st.acked++
		case refused:
			st.refused++
		case lost:
			st.lost++
		}
	}
	var b bytes.Buffer
	secs := s.ended.Seconds()
	fmt.Fprintf(&b, "soak: seed %d: %d requests in %.1fs (%.0f/s)\n", s.cfg.seed, len(calls), secs, float64(len(calls))/secs)
	fmt.Fprintf(&b, "  %-10s %7s %7s %7s %6s %8s %8s %8s\n", "op", "count", "acked", "refused", "lost", "p50", "p99", "max")
	for _, op := range slices.Sorted(maps.Keys(byOp)) {
		st := byOp[op]
		slices.Sort(st.took)
		q := func(f float64) time.Duration { return st.took[min(len(st.took)-1, int(f*float64(len(st.took))))] }
		fmt.Fprintf(&b, "  %-10s %7d %7d %7d %6d %8s %8s %8s\n", op, st.n, st.acked, st.refused, st.lost,
			q(0.5).Round(100*time.Microsecond), q(0.99).Round(100*time.Microsecond), st.took[len(st.took)-1].Round(time.Millisecond))
	}
	codes := map[string]int{}
	for _, c := range calls {
		if c.out == refused && c.err != nil {
			codes[c.op+" "+string(c.err.Code)]++
		}
	}
	b.WriteString("  refusals:")
	for _, k := range slices.Sorted(maps.Keys(codes)) {
		fmt.Fprintf(&b, " %s %d;", k, codes[k])
	}
	b.WriteString("\n")
	for _, p := range s.projs {
		r := p.rep
		r.mu.Lock()
		n := 0
		for _, c := range calls {
			if c.p == p {
				n++
			}
		}
		fmt.Fprintf(&b, "  project %s: %d requests; events %d in %d transactions; issues %d; claims taken %d, reaped %d, released %d (%d for lost access); takeovers of a live claim %d; admin overrides by a project admin %d\n",
			p.name, n, r.seq, r.txs, len(r.issues), r.ops["claim.take"], r.ops["claim.expire"], r.ops["claim.release"], len(r.lost), len(r.overs), len(r.overrides))
		r.mu.Unlock()
	}
	s.reportAccess(&b)
	s.reportPartition(&b, calls)
	s.reportLocks(&b)
	m := s.mon
	m.mu.Lock()
	fmt.Fprintf(&b, "  chaos: %d dropped answers, %d connection drops, %d daemon restarts; %d sessions vanished, %d stalled; %d dial failures; retries %v\n",
		s.stats.drops.Load(), s.stats.dropAlls.Load(), s.stats.restarts.Load(), m.vanishes, s.stats.stalls.Load(), m.dialFails, m.retries)
	m.mu.Unlock()
	cs := &s.clockStats
	fmt.Fprintf(&b, "  clock: set forward %d times (5 to 30 minutes; %d claims with lease left checked, none reaped for it; %d clock.step events in the projects' logs extended %d leases; %d of the jumps while a store ran slow, where a lease is held to what its log says it was given), set back %d times while running and %d at a restart; %d starts refused for a clock behind history (%d then corrected, %d accepted for one start), %d too slow for the clock to be behind still\n",
		cs.forward.Load(), cs.leasesChecked.Load(), cs.stepEvents.Load(), cs.leasesExtendedInEvents.Load(), cs.forwardSlow.Load(),
		cs.back.Load(), cs.backAtStart.Load(), cs.refused.Load(), cs.corrected.Load(), cs.accepted.Load(), cs.slowStarts.Load())
	fmt.Fprintf(&b, "  watches checked %d (%d for events, %d events owed; %d events pushed in all), %d ended by a resync, %d watched again; claims checked %d; snapshots compared %d; peak heap %d MiB, peak goroutines %d; daemon warnings %d\n",
		s.stats.windowsChecked.Load(), s.stats.eventWindowsChecked.Load(), s.stats.eventsChecked.Load(), s.stats.eventsPushed.Load(), s.stats.windowsResynced.Load(),
		s.stats.rewatches.Load(), s.stats.claimsChecked.Load(), s.stats.snapshots.Load(), s.stats.heapPeak.Load()>>20, s.stats.goroutinePeak.Load(), s.logs.count())
	for _, l := range s.logs.top(12) {
		fmt.Fprintf(&b, "    %s\n", l)
	}
	s.t.Log(b.String())
}
