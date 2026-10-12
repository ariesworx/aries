package server

import (
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

func TestResolveSettingsClock(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		body string
		env  string // STARFIXD_CLOCK_BEHIND
		want Settings
		err  []string // what the refusal must name
	}{
		{name: "defaults", want: Settings{ClockBehind: ClockStop, ClockBehindTolerance: Tolerance(time.Minute),
			ClockStepTolerance: Tolerance(5 * time.Second), ClockClientTolerance: Tolerance(5 * time.Minute)}},
		{name: "warn", body: "clock_behind: warn\n", want: Settings{ClockBehind: ClockWarn, ClockBehindTolerance: Tolerance(time.Minute),
			ClockStepTolerance: Tolerance(5 * time.Second), ClockClientTolerance: Tolerance(5 * time.Minute)}},
		{name: "the environment wins", body: "clock_behind: warn\n", env: "stop", want: Settings{ClockBehind: ClockStop, ClockBehindTolerance: Tolerance(time.Minute),
			ClockStepTolerance: Tolerance(5 * time.Second), ClockClientTolerance: Tolerance(5 * time.Minute)}},
		{name: "tolerances", body: "clock_behind_tolerance: 10s\nclock_step_tolerance: 30s\nclock_client_tolerance: 1h\n",
			want: Settings{ClockBehind: ClockStop, ClockBehindTolerance: Tolerance(10 * time.Second),
				ClockStepTolerance: Tolerance(30 * time.Second), ClockClientTolerance: Tolerance(time.Hour)}},
		{name: "the widest", body: "clock_behind_tolerance: 24h\nclock_step_tolerance: 10m\nclock_client_tolerance: 1d\n",
			want: Settings{ClockBehind: ClockStop, ClockBehindTolerance: Tolerance(24 * time.Hour),
				ClockStepTolerance: Tolerance(10 * time.Minute), ClockClientTolerance: Tolerance(24 * time.Hour)}},
		{name: "an unknown value", body: "clock_behind: halt\n", err: []string{"clock_behind", `"halt"`, "fix:", "stop", "warn"}},
		{name: "an unknown value in the environment", env: "off", err: []string{"clock_behind", `"off"`, "fix:", EnvClockBehind}},
		{name: "a tolerance of zero", body: "clock_step_tolerance: 0s\n", err: []string{"clock tolerance", "more than zero", "fix:"}},
		{name: "a step tolerance too small", body: "clock_step_tolerance: 500ms\n", err: []string{"clock_step_tolerance"}},
		{name: "a step tolerance that turns the check off", body: "clock_step_tolerance: 1h\n", err: []string{"clock_step_tolerance", "1s to 10m", "fix:", "5s"}},
		{name: "a behind tolerance that turns the check off", body: "clock_behind_tolerance: 30d\n", err: []string{"clock_behind_tolerance", "24h", "fix:"}},
		{name: "a client tolerance too wide", body: "clock_client_tolerance: 2d\n", err: []string{"clock_client_tolerance", "fix:"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := ResolveSettings(Settings{}, path, func(k string) string {
				if k == EnvClockBehind {
					return tc.env
				}
				return ""
			})
			if tc.err != nil {
				if err == nil {
					t.Fatalf("ResolveSettings(%q, %s=%q) = no error, want one naming %q", tc.body, EnvClockBehind, tc.env, tc.err)
				}
				for _, w := range tc.err {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("ResolveSettings(%q, %s=%q) = %v, want it to name %q", tc.body, EnvClockBehind, tc.env, err, w)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSettings(%q, %s=%q) = %v", tc.body, EnvClockBehind, tc.env, err)
			}
			w := tc.want
			if got.ClockBehind != w.ClockBehind || got.ClockBehindTolerance != w.ClockBehindTolerance ||
				got.ClockStepTolerance != w.ClockStepTolerance || got.ClockClientTolerance != w.ClockClientTolerance {
				t.Errorf("ResolveSettings(%q, %s=%q) = clock_behind %s, tolerances %s, %s, %s; want %s, %s, %s, %s", tc.body, EnvClockBehind, tc.env,
					got.ClockBehind, time.Duration(got.ClockBehindTolerance), time.Duration(got.ClockStepTolerance), time.Duration(got.ClockClientTolerance),
					w.ClockBehind, time.Duration(w.ClockBehindTolerance), time.Duration(w.ClockStepTolerance), time.Duration(w.ClockClientTolerance))
			}
		})
	}
}

// machine is a machine's two clocks: the wall clock, which can be set,
// and the monotonic clock, which only runs.
type machine struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func newMachine() *machine {
	return &machine{wall: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *machine) read() (time.Time, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall, c.mono
}

func (c *machine) now() time.Time { w, _ := c.read(); return w }

// run lets d pass on both clocks; set moves the wall clock alone.
func (c *machine) run(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall, c.mono = c.wall.Add(d), c.mono+d
}

func (c *machine) set(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
}

// newDatabase returns the DSN of a fresh database, or skips without
// dolt.
func newDatabase(t *testing.T) string {
	t.Helper()
	if errors.Is(doltErr, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the server tests")
	}
	if doltErr != nil {
		t.Fatalf("dolt sql-server: %v", doltErr)
	}
	dsn, err := dolt.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// earlierWork writes one event to the database at dsn, at the time of a
// clock, as a daemon that ran there before did.
func earlierWork(t *testing.T, dsn string, clk *store.Clock) {
	t.Helper()
	st, err := store.Open(t.Context(), dsn, store.Options{Prefix: "sf", CommitInterval: -1, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateIssue(t.Context(), alice, store.NewIssue{Title: "earlier work"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// A daemon starts, or refuses to, by how its clock stands against the
// history in its database.
func TestStartClockBehind(t *testing.T) {
	tests := []struct {
		name    string
		mode    string        // Config.ClockBehind; empty is the default
		back    time.Duration // how far the clock is set back between the history and the start
		accept  time.Duration // Config.AcceptHistory, from the history's time; 0 is unset
		refused bool
		logged  bool // the start logs `clock behind history`
	}{
		{name: "the clock was not set"},
		{name: "ahead of history, a restart a day later", back: -24 * time.Hour},
		{name: "30s behind is within the tolerance", back: 30 * time.Second},
		{name: "2m behind is refused by default", back: 2 * time.Minute, refused: true},
		{name: "a year behind is refused by default", back: 365 * 24 * time.Hour, refused: true},
		{name: "stop", mode: ClockStop, back: 3 * time.Hour, refused: true},
		{name: "stop, accepted for this history", mode: ClockStop, back: 3 * time.Hour, accept: 2 * time.Second, logged: true},
		{name: "stop, accepted for an earlier history only", mode: ClockStop, back: 3 * time.Hour, accept: -time.Minute, refused: true},
		{name: "warn", mode: ClockWarn, back: 3 * time.Hour, logged: true},
		{name: "warn, within the tolerance", mode: ClockWarn, back: 30 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newDatabase(t)
			c := newMachine()
			earlierWork(t, dsn, store.NewClock(store.ClockOptions{Read: c.read}))
			was := c.now()
			c.run(time.Second) // the daemon stops, and the machine restarts
			c.set(-tc.back)

			clk := store.NewClock(store.ClockOptions{Read: c.read}) // a new process
			st, err := store.Open(t.Context(), dsn, store.Options{Prefix: "sf", CommitInterval: -1, Clock: clk})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			log := &syncBuffer{}
			cfg := Config{Store: st, Project: project, PeerCheck: func(net.Conn) error { return nil }, Clock: clk,
				ClockBehind: tc.mode, ReapInterval: -1, Logger: slog.New(slog.NewTextHandler(log, nil))}
			if tc.accept != 0 {
				cfg.AcceptHistory = was.Add(tc.accept)
			}
			s, err := New(cfg)
			behind, isBehind := errors.AsType[*ClockBehindError](err)
			if isBehind != tc.refused || (err != nil && !isBehind) {
				t.Fatalf("New with the clock set back %s, clock_behind %q = %v; want refused %v", tc.back, tc.mode, err, tc.refused)
			}
			if tc.refused {
				// The refusal names both times, the difference, whose
				// history, and each way on.
				msg := err.Error()
				for _, w := range []string{c.now().Format(time.RFC3339), was.Format(time.RFC3339), "project " + project, "fix:",
					"timedatectl", "--accept-clock-behind " + was.Add(2*time.Second).Format(time.RFC3339), "clock_behind: warn"} {
					if !strings.Contains(msg, w) {
						t.Errorf("the refusal lacks %q:\n%s", w, msg)
					}
				}
				if got := behind.History.Sub(behind.Clock); got < tc.back-2*time.Second || got > tc.back {
					t.Errorf("the refusal has the clock %s behind, want about %s", got, tc.back)
				}
				return
			}
			if got := strings.Contains(log.String(), `msg="clock behind history"`); got != tc.logged {
				t.Errorf("the start's log has `clock behind history`: %v, want %v:\n%s", got, tc.logged, log)
			}
			if tc.logged && !strings.Contains(log.String(), "level=ERROR") {
				t.Errorf("the warning is not at error level:\n%s", log)
			}
			// Started, the server's time is never before its history.
			if now := s.single.store.Now(); now.Before(was) {
				t.Errorf("the store's time is %s, before its latest event at %s", now, was)
			}
		})
	}
}

// A database with no history has nothing for the clock to be behind,
// whatever the clock says.
func TestStartFreshDatabaseWithAnyClock(t *testing.T) {
	c := newMachine()
	c.set(-30 * 365 * 24 * time.Hour) // a dead battery: the clock reads 1996
	clk := store.NewClock(store.ClockOptions{Read: c.read})
	st, err := store.Open(t.Context(), newDatabase(t), store.Options{Prefix: "sf", CommitInterval: -1, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := New(Config{Store: st, Project: project, Clock: clk, ReapInterval: -1}); err != nil {
		t.Errorf("New on a database with no events, the clock in 1996 = %v, want a server", err)
	}
}

// While the clock is behind, a running daemon says so again and again,
// in its log and in its status, and says when it is over. It does not
// stop: a daemon that exits when its clock is set back hands whoever can
// set it a way to take the server down.
func TestClockBehindWhileRunning(t *testing.T) {
	for _, mode := range []string{ClockStop, ClockWarn} {
		t.Run(mode, func(t *testing.T) {
			dsn := newDatabase(t)
			c := newMachine()
			clk := store.NewClock(store.ClockOptions{Read: c.read})
			st, err := store.Open(t.Context(), dsn, store.Options{Prefix: "sf", CommitInterval: -1, Clock: clk})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			log := &syncBuffer{}
			s, err := New(Config{Store: st, Project: project, PeerCheck: func(net.Conn) error { return nil }, Clock: clk,
				ClockBehind: mode, ReapInterval: -1, Logger: slog.New(slog.NewTextHandler(log, nil))})
			if err != nil {
				t.Fatal(err)
			}
			s.now = c.now
			ctx := t.Context()
			mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
			count := func(msg string) int { return strings.Count(log.String(), `msg="`+msg+`"`) }

			s.CheckClock(ctx)
			if n := count("clock behind history") + count("clock stepped"); n != 0 {
				t.Fatalf("a clock that only ran logged %d clock lines:\n%s", n, log)
			}
			c.run(time.Minute)
			s.CheckClock(ctx)
			c.set(-time.Hour)
			s.CheckClock(ctx)
			if count("clock stepped") != 1 || !strings.Contains(log.String(), "direction=backward") || count("clock behind history") != 1 {
				t.Fatalf("after the clock was set back an hour, want one `clock stepped` backward and one `clock behind history`:\n%s", log)
			}
			wantRestart := "it will start again as it is"
			if mode == ClockStop {
				wantRestart = "THIS SERVER WILL NOT START AGAIN"
			}
			if !strings.Contains(log.String(), wantRestart) {
				t.Errorf("clock_behind %s: the warning does not say %q:\n%s", mode, wantRestart, log)
			}
			// It serves, and its events stay in order.
			mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "more work"})
			evs, err := st.Events(ctx, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i < len(evs); i++ {
				if evs[i].At.Before(evs[i-1].At) {
					t.Errorf("event %d is at %s, before event %d at %s", evs[i].Seq, evs[i].At, evs[i-1].Seq, evs[i-1].At)
				}
			}
			// Not a line on every tick, but one every ten minutes.
			for range 5 {
				c.run(time.Minute)
				s.CheckClock(ctx)
			}
			if n := count("clock behind history"); n != 1 {
				t.Errorf("%d `clock behind history` lines in the first 5 minutes, want 1", n)
			}
			c.run(6 * time.Minute)
			s.CheckClock(ctx)
			if n := count("clock behind history"); n != 2 {
				t.Errorf("%d `clock behind history` lines after 11 minutes, want 2", n)
			}
			if b := s.clockStatus().Behind; b == nil || b.Log != "project "+project || b.RefusesStart != (mode == ClockStop) ||
				b.Seconds < 50*60 || b.Seconds > 3600 {
				t.Errorf("clockStatus().Behind = %+v, want this project's history about 55 minutes ahead", b)
			}
			if cs := s.clockStatus(); cs.StepsBackward != 1 || cs.StepsForward != 0 || cs.LastStepSeconds != -3600 {
				t.Errorf("clockStatus() = %+v, want one step back of an hour", cs)
			}
			// At half speed the hour is made up in two.
			c.run(2 * time.Hour)
			s.CheckClock(ctx)
			if count("clock caught up with history") != 1 || s.clockStatus().Behind != nil {
				t.Errorf("two hours on, want one `clock caught up with history` and a status that is no longer behind:\n%s", log)
			}
			if !st.Now().Equal(c.now()) {
				t.Errorf("the store's time is %s and the clock's %s, want them equal again", st.Now(), c.now())
			}
		})
	}
}

// A server of many projects checks its clock against every history it
// will write to: the control store's and each active project's, which
// it opens at start anyway.
func TestOpenProjectsClockBehind(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	a := m.project(t, projA, "alice")
	b := m.project(t, projB, "alice")
	// Project b's database was written by a machine whose clock ran a
	// week ahead.
	ahead := time.Now().Add(7 * 24 * time.Hour)
	dsn, err := DatabaseDSN(m.dsn, b.DBName)
	if err != nil {
		t.Fatal(err)
	}
	earlierWork(t, dsn, store.NewClock(store.ClockOptions{Now: func() time.Time { return ahead }}))

	err = m.OpenProjects(t.Context())
	behind, ok := errors.AsType[*ClockBehindError](err)
	if !ok {
		t.Fatalf("OpenProjects with project b's history a week ahead = %v, want a *ClockBehindError", err)
	}
	if !strings.Contains(behind.Log, b.DBName) || strings.Contains(behind.Log, a.DBName) || behind.More != 0 {
		t.Errorf("the refusal names %q and %d more, want project b's database %s alone", behind.Log, behind.More, b.DBName)
	}

	m.cfg.ClockBehind = ClockWarn
	if err := m.OpenProjects(t.Context()); err != nil {
		t.Fatalf("OpenProjects with clock_behind: warn = %v", err)
	}
	if !strings.Contains(m.log.String(), `msg="clock behind history"`) || !strings.Contains(m.log.String(), b.DBName) {
		t.Errorf("the log lacks `clock behind history` naming %s:\n%s", b.DBName, m.log)
	}
	// Status says so to a server admin, with the database.
	d := mustDialTo(t, m.Server, dana, projB)
	m.CheckClock(t.Context())
	res := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
	if c := res.Server.Clock; c == nil || c.Behind == nil || !strings.Contains(c.Behind.Log, b.DBName) || c.Behind.RefusesStart ||
		c.Behind.Seconds < 6*24*3600 {
		t.Errorf("status's clock = %+v, want it behind project b's history by about a week", res.Server.Clock)
	}

	// Accepted for one start, under stop.
	m.cfg.ClockBehind, m.cfg.AcceptHistory = ClockStop, ahead.Add(time.Second)
	if err := m.OpenProjects(t.Context()); err != nil {
		t.Errorf("OpenProjects with the history accepted = %v", err)
	}
}

// The limits that count time in memory run on the monotonic clock in a
// daemon (time.Now's reading), so a step of the wall clock does not move
// them. Given wall-clock times alone, as here, a step back refills no
// bucket and a jump forward fills one only to its burst.
func TestBucketsAcrossClockSteps(t *testing.T) {
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	b := buckets{rate: 2, burst: 3}
	for i, st := range []struct {
		at time.Duration
		ok bool
	}{
		{0, true}, {0, true}, {0, true}, {0, false},
		{-time.Hour, false},                       // set back: no token for it
		{-time.Hour + 500*time.Millisecond, true}, // and the refill goes on from there
		{-time.Hour + 500*time.Millisecond, false},
		{365 * 24 * time.Hour, true}, {365 * 24 * time.Hour, true}, {365 * 24 * time.Hour, true}, // a jump forward: the burst, no more
		{365 * 24 * time.Hour, false},
	} {
		if ok, _ := b.take("alice", t0.Add(st.at)); ok != st.ok {
			t.Errorf("step %d: take(alice, %+v) = %v, want %v", i, st.at, ok, st.ok)
		}
	}
}

// A project store is idle by the registry's clock. Set back, the clock
// closes nothing early; set forward, it closes stores with no
// connection, which the next connection opens again.
func TestRegistryIdleAcrossClockSteps(t *testing.T) {
	ck := newClock()
	m := newMulti(t, Limits{StoreIdle: Duration(10 * time.Minute)}, ck)
	m.project(t, projA, "alice")
	c := mustDialTo(t, m.Server, alice, projA)
	c.hangUp()
	ck.advance(-time.Hour)
	m.CloseIdleStores()
	if n := m.OpenStores(); n != 1 {
		t.Errorf("%d stores open after the clock was set back an hour, want 1: nothing closes early", n)
	}
	ck.advance(time.Hour + 11*time.Minute)
	m.CloseIdleStores()
	if n := m.OpenStores(); n != 0 {
		t.Errorf("%d stores open 11 minutes after the last connection, want 0", n)
	}
	if w, _ := dialTo(t, m.Server, alice, projA); w.Err != nil {
		t.Errorf("a connection after the idle close = %v, want the store opened again", w.Err)
	}
}
