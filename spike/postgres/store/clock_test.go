package store

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// twoClocks is a machine's two clocks for tests: the wall clock, which
// can be set, and the monotonic clock, which only runs.
type twoClocks struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func newTwoClocks() *twoClocks {
	return &twoClocks{wall: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
}

func (c *twoClocks) read() (time.Time, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall, c.mono
}

// run lets d pass on both clocks.
func (c *twoClocks) run(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall, c.mono = c.wall.Add(d), c.mono+d
}

// step sets the wall clock by d, forward or back: an operator's date
// command, a time service's correction, or, forward, a machine resumed
// after d suspended.
func (c *twoClocks) step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
}

func (c *twoClocks) now() time.Time { w, _ := c.read(); return w }

// steppedStore opens a store on dsn (a fresh database if empty) whose
// clock is c.
func steppedStore(t *testing.T, dsn string, c *twoClocks, opts Options) (*Store, *Clock) {
	t.Helper()
	if dsn == "" {
		dsn = newDSN(t)
	}
	opts.Clock = NewClock(ClockOptions{Read: c.read})
	return openStore(t, dsn, opts), opts.Clock
}

func TestClockSteps(t *testing.T) {
	t.Parallel()
	type move struct {
		run, step time.Duration
	}
	tests := []struct {
		name              string
		moves             []move
		forward, backward int
		lastBy            time.Duration
	}{
		{name: "running", moves: []move{{run: time.Hour}, {run: 30 * 24 * time.Hour}}},
		{name: "under the tolerance either way", moves: []move{{run: time.Second, step: 4 * time.Second}, {run: time.Second, step: -5 * time.Second}}},
		{name: "forward", moves: []move{{run: time.Second, step: 6 * time.Second}}, forward: 1, lastBy: 6 * time.Second},
		// Before this, steps each under the tolerance went unseen however
		// many there were: 200 of 4.9s would have run out every lease.
		{name: "small steps made quickly add up to one", moves: []move{{step: 2 * time.Second}, {step: 2 * time.Second},
			{step: 2 * time.Second}, {step: 2 * time.Second}}, forward: 1, lastBy: 6 * time.Second},
		{name: "small steps back add up too", moves: []move{{step: -3 * time.Second}, {step: -3 * time.Second}}, backward: 1, lastBy: -6 * time.Second},
		{name: "a second an hour is forgiven", moves: []move{{run: time.Hour, step: time.Second}, {run: time.Hour, step: time.Second}, {run: time.Hour, step: time.Second},
			{run: time.Hour, step: time.Second}, {run: time.Hour, step: time.Second}, {run: time.Hour, step: time.Second}, {run: time.Hour, step: time.Second}}},
		{name: "backward", moves: []move{{run: time.Minute, step: -time.Hour}}, backward: 1, lastBy: -time.Hour},
		{name: "suspended overnight", moves: []move{{step: 9 * time.Hour}, {run: time.Minute}}, forward: 1, lastBy: 9 * time.Hour},
		{name: "back and forward again", moves: []move{{step: -time.Hour}, {run: time.Minute, step: time.Hour}}, forward: 1, backward: 1, lastBy: time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTwoClocks()
			clk := NewClock(ClockOptions{Read: c.read})
			clk.Now()
			for _, m := range tc.moves {
				c.run(m.run)
				c.step(m.step)
				clk.Now()
			}
			got := clk.Steps()
			if got.Forward != tc.forward || got.Backward != tc.backward || got.LastBy != tc.lastBy {
				t.Errorf("Steps() after %v = %d forward, %d backward, last by %s; want %d, %d, %s",
					tc.moves, got.Forward, got.Backward, got.LastBy, tc.forward, tc.backward, tc.lastBy)
			}
		})
	}
}

// A store's time is to the microsecond, as its columns are, whatever
// the clocks read: a time with more would be pushed to a watch as one
// instant and stored as another. The soak found this.
func TestStoreTimeIsToTheMicrosecond(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{})
	for _, move := range []func(){
		func() { c.run(1234567 * time.Nanosecond) },
		func() { c.run(1234567 * time.Nanosecond); c.step(20 * time.Minute) }, // held back until settled
		func() { c.run(333 * time.Nanosecond); c.step(-time.Hour) },
		func() { c.run(7777777 * time.Nanosecond) }, // running slow
	} {
		move()
		if now := s.Now(); now.Nanosecond()%1000 != 0 {
			t.Errorf("Now() = %s, finer than a microsecond", now.Format(time.RFC3339Nano))
		}
	}
}

// The stores of one server share one time. Each database has its own
// latest event; after a restart with the clock behind them, a store that
// went on from its own would be minutes apart from its neighbor, and an
// event in the control store could no longer be placed against one in a
// project. The soak found this: a claim released for a lost membership
// was dated before the control event that removed it.
func TestStoresOfOneServerShareOneTime(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	quiet, busy := newDSN(t), newDSN(t)
	clk := NewClock(ClockOptions{Read: c.read})
	a := openStore(t, quiet, Options{Clock: clk})
	mustCreate(t, a, NewIssue{Title: "the quiet database's last event"})
	c.run(10 * time.Minute)
	b := openStore(t, busy, Options{Clock: clk})
	mustCreate(t, b, NewIssue{Title: "the busy database's last event"})
	last := b.Now()
	for _, s := range []*Store{a, b} {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}

	c.step(-time.Hour) // the machine restarts with its clock an hour behind
	clk = NewClock(ClockOptions{Read: c.read})
	a = openStore(t, quiet, Options{Clock: clk}) // opened first, as the control store is
	b = openStore(t, busy, Options{Clock: clk})
	if sa, sb := a.ClockState(), b.ClockState(); !sa.Mark.Before(sb.Mark) || sb.Mark.After(last) {
		t.Fatalf("marks %s and %s, want the quiet database's 10 minutes before the busy one's", sa.Mark, sb.Mark)
	}
	for range 3 {
		c.run(time.Minute)
		na, nb := a.Now(), b.Now()
		if d := nb.Sub(na); d < 0 || d > time.Millisecond {
			t.Errorf("the two stores' times are %s and %s, %s apart; want one time", na, nb, d)
		}
		if na.Before(last) {
			t.Errorf("the quiet store's time is %s, before the server's latest event at %s", na, last)
		}
	}
	// What the server does next in the quiet database is dated after
	// what it last did in the busy one.
	ev := mustCreate(t, a, NewIssue{Title: "after the restart"})
	if !ev.CreatedAt.After(last) {
		t.Errorf("an event in the quiet database after the restart is at %s, not after the busy database's last at %s", ev.CreatedAt, last)
	}
}

// One store settles a jump and gives out times after it; the clock is
// then set back before its neighbor has written anything. The neighbor
// follows the first, to keep one time, and its leases are given the jump
// that makes it do so: no claim ends for it.
func TestNeighborFollowsASettledJumpWithItsLeases(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	clk := NewClock(ClockOptions{Read: c.read})
	a := openStore(t, newDSN(t), Options{Clock: clk})
	b := openStore(t, newDSN(t), Options{Clock: clk})
	ctx := t.Context()
	is := mustCreate(t, b, NewIssue{})
	_, cl, err := b.StartIssue(ctx, alice, is.ID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	c.step(time.Hour)
	mustCreate(t, a, NewIssue{Title: "a writes after the jump, and settles it"})
	c.step(-time.Hour)
	if reaped, err := b.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("ReapClaims in the neighbor = %+v, %v; want no claim ended", reaped, err)
	}
	if na, nb := a.Now(), b.Now(); nb.Before(na.Add(-time.Millisecond)) || nb.After(na.Add(time.Millisecond)) {
		t.Errorf("the two stores' times are %s and %s; want one time", na, nb)
	}
	got, err := b.ClaimOf(ctx, is.ID)
	if err != nil || got == nil || !got.ExpiresAt.Equal(cl.ExpiresAt.Add(time.Hour)) {
		t.Errorf("the neighbor's claim = %+v, %v; want it live, its lease an hour later at %s", got, err, cl.ExpiresAt.Add(time.Hour))
	}
}

// A clock built from Now alone, with times that carry no monotonic
// reading, never shows a step: there is nothing to compare the wall
// clock with.
func TestClockWithoutMonotonicReadings(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	clk := NewClock(ClockOptions{Now: func() time.Time { return at }})
	clk.Now()
	at = at.Add(48 * time.Hour)
	clk.Now()
	at = at.Add(-72 * time.Hour)
	if got := clk.Steps(); got.Forward != 0 || got.Backward != 0 {
		t.Errorf("Steps() of a clock without monotonic readings = %+v, want none", got)
	}
}

// eventTimes fails the test if any event of s has a time before its
// predecessor's, and returns the events.
func eventTimes(t *testing.T, s *Store) []Event {
	t.Helper()
	evs, err := s.Events(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].At.Before(evs[i-1].At) {
			t.Errorf("event %d (%s) is at %s, before event %d at %s", evs[i].Seq, evs[i].Op,
				evs[i].At.Format(time.RFC3339Nano), evs[i-1].Seq, evs[i-1].At.Format(time.RFC3339Nano))
		}
	}
	return evs
}

// Before this, an event written after the clock was set back had a time
// before its predecessor's, in a running store and across a restart.
func TestEventTimeNeverGoesBackward(t *testing.T) {
	t.Parallel()
	for _, back := range []time.Duration{time.Second, 30 * time.Second, time.Hour, 30 * 24 * time.Hour} {
		t.Run(back.String(), func(t *testing.T) {
			c := newTwoClocks()
			dsn := newDSN(t)
			s, _ := steppedStore(t, dsn, c, Options{})
			mustCreate(t, s, NewIssue{Title: "before"})
			c.run(time.Minute)
			mustCreate(t, s, NewIssue{Title: "a minute later"})

			c.step(-back)
			mustCreate(t, s, NewIssue{Title: "after the clock was set back"})
			if now, wall := s.Now(), c.now(); now.Before(wall) || !now.After(wall) && back > time.Minute {
				t.Errorf("Now() = %s with the clock at %s, set back %s: want the store's time ahead of the clock", now, wall, back)
			}
			c.run(3 * time.Second)
			mustCreate(t, s, NewIssue{Title: "three seconds after that"})
			evs := eventTimes(t, s)
			if n := len(evs); !evs[n-1].At.After(evs[n-2].At) {
				t.Errorf("an event written three seconds after another, with the clock behind, is at %s, not after %s: the store's time stood still",
					evs[n-1].At.Format(time.RFC3339Nano), evs[n-2].At.Format(time.RFC3339Nano))
			}

			// Across a close and reopen, as a restart with the clock
			// still behind, and with a clock that knows of no step.
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s2, _ := steppedStore(t, dsn, c, Options{})
			st := s2.ClockState()
			if back > time.Minute && (st.Behind < back-2*time.Minute || st.Behind > back) {
				t.Errorf("ClockState().Behind after reopening with the clock set back %s = %s", back, st.Behind)
			}
			mustCreate(t, s2, NewIssue{Title: "after the restart"})
			eventTimes(t, s2)
		})
	}
}

// While the clock is behind, the store's time runs at half speed, so the
// clock catches up after twice the gap and the two then agree again.
func TestStoreTimeCatchesUp(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{})
	mustCreate(t, s, NewIssue{})
	start := s.Now()
	c.step(-time.Hour)
	for _, tc := range []struct {
		run    time.Duration
		behind time.Duration
	}{
		{0, time.Hour}, {30 * time.Minute, 45 * time.Minute}, {time.Hour, 15 * time.Minute}, {30 * time.Minute, 0}, {time.Hour, 0},
	} {
		c.run(tc.run)
		st := s.ClockState()
		if st.Behind != tc.behind {
			t.Errorf("ClockState().Behind %s after the clock was set back an hour = %s, want %s", c.now().Sub(start)+time.Hour, st.Behind, tc.behind)
		}
		if tc.behind == 0 && !st.Now.Equal(c.now()) {
			t.Errorf("Now() = %s once the clock (%s) has caught up, want the clock's time", st.Now, c.now())
		}
	}
}

// A lease promises a duration. Set back, the clock lengthens a lease by
// at most how far it was set back, and never shortens one; before, a
// clock set back an hour kept a 15-minute lease alive for 75.
func TestLeaseAcrossBackwardStep(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	c.run(5 * time.Minute)
	s.Now() // a daemon reads its clock on every request and every tick
	c.step(-time.Hour)
	live := func() bool {
		t.Helper()
		cl, err := s.ClaimOf(ctx, is.ID)
		if err != nil {
			t.Fatal(err)
		}
		return cl != nil
	}
	// 10 minutes of the lease were left; at half speed they take 20.
	c.run(9 * time.Minute)
	if !live() {
		t.Error("a 15m lease ended 14 minutes in, after the clock was set back: a step back shortened it")
	}
	c.run(12 * time.Minute)
	if live() {
		t.Error("a 15m lease is still live 26 minutes in, with the clock set back an hour: want it lapsed by 25")
	}
	// Bob takes the lapsed claim under a new epoch, and alice's old one
	// is refused: fencing is by epoch, not by the clock.
	_, cl, err := s.StartIssue(ctx, bob, is.ID, 0, false)
	if err != nil || cl.Epoch != 2 {
		t.Fatalf("StartIssue(bob) of a lapsed claim = epoch %d, %v; want epoch 2", cl.Epoch, err)
	}
	if _, _, err := s.FinishIssue(ctx, alice, is.ID, 1, Finish{}); !isStale(err) && !errors.Is(err, ErrForbidden) {
		t.Errorf("FinishIssue(alice, epoch 1) after bob took epoch 2 = %v, want a refusal", err)
	}
}

func isStale(err error) bool {
	var se *StaleEpochError
	return errors.As(err, &se)
}

// Fencing is by epoch, and no step of the clock weakens it: while a
// claim is live nobody else takes the issue, once another has taken it
// the old holder's epoch is refused everywhere, and the epoch only
// rises. The steps are tried at every point of a takeover.
func TestFencingAcrossClockSteps(t *testing.T) {
	t.Parallel()
	steps := []struct {
		name string
		by   []time.Duration // the clock is set by each in turn, a reading between
	}{
		{"none", nil},
		{"back an hour", []time.Duration{-time.Hour}},
		{"back a year", []time.Duration{-365 * 24 * time.Hour}},
		{"forward an hour", []time.Duration{time.Hour}},
		{"forward a year", []time.Duration{365 * 24 * time.Hour}},
		{"back and forward", []time.Duration{-time.Hour, time.Hour}},
		{"forward and back", []time.Duration{time.Hour, -time.Hour}},
	}
	for _, when := range []string{"while alice holds it", "after bob took it"} {
		for _, st := range steps {
			t.Run(when+", "+st.name, func(t *testing.T) {
				c := newTwoClocks()
				s, _ := steppedStore(t, "", c, Options{})
				ctx := t.Context()
				step := func() {
					for _, d := range st.by {
						c.step(d)
						s.Now()
					}
				}
				is := mustCreate(t, s, NewIssue{})
				rev := func() Rev {
					t.Helper()
					cur, err := s.GetIssue(ctx, is.ID)
					if err != nil {
						t.Fatal(err)
					}
					return cur.Rev
				}
				_, ca, err := s.StartIssue(ctx, alice, is.ID, 0, false)
				if err != nil {
					t.Fatal(err)
				}
				c.run(time.Minute)
				s.Now()
				if when == "while alice holds it" {
					step()
				}
				// A minute into a 15m lease, whatever the clock now says,
				// the claim is alice's and nobody else's.
				if _, cb, err := s.StartIssue(ctx, bob, is.ID, 0, false); err == nil {
					t.Fatalf("StartIssue(bob) a minute into alice's lease = epoch %d, no error: two holders", cb.Epoch)
				}
				if _, err := s.UpdateIssue(ctx, bob, is.ID, rev(), IssuePatch{Title: ptr("bob's")}); !errors.Is(err, ErrForbidden) {
					t.Errorf("UpdateIssue(bob) of the issue alice holds = %v, want ErrForbidden", err)
				}
				// Alice vanishes. However long the lease then lasts, it
				// ends, and bob takes the issue under the next epoch.
				var cb Claim
				for i := 0; ; i++ {
					c.run(5 * time.Minute)
					if _, err := s.ReapClaims(ctx); err != nil {
						t.Fatal(err)
					}
					if _, cb, err = s.StartIssue(ctx, bob, is.ID, time.Hour, false); err == nil {
						break
					}
					if i > 12 {
						t.Fatalf("an hour on, alice's 15m lease still holds the issue: %v", err)
					}
				}
				if cb.Epoch != ca.Epoch+1 || cb.Holder != bob {
					t.Fatalf("bob's claim = %+v, want epoch %d", cb, ca.Epoch+1)
				}
				if when == "after bob took it" {
					step()
				}
				// The stale holder writes nothing, whatever the clock says.
				if _, _, err := s.FinishIssue(ctx, alice, is.ID, ca.Epoch, Finish{}); err == nil {
					t.Error("FinishIssue(alice, her old epoch) after bob took the issue succeeded")
				}
				if _, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{}); !errors.Is(err, ErrForbidden) {
					t.Errorf("FinishIssue(alice, no epoch) after bob took the issue = %v, want ErrForbidden", err)
				}
				if _, err := s.UpdateIssue(ctx, alice, is.ID, rev(), IssuePatch{Title: ptr("alice's")}); !errors.Is(err, ErrForbidden) {
					t.Errorf("UpdateIssue(alice) after bob took the issue = %v, want ErrForbidden", err)
				}
				if held, err := s.RenewClaims(ctx, alice, DefaultLease, false, nil); err != nil || len(held) != 0 {
					t.Errorf("RenewClaims(alice) after bob took the issue = %+v, %v; want none", held, err)
				}
				if _, c2, err := s.StartIssue(ctx, alice, is.ID, 0, false); err == nil {
					t.Errorf("StartIssue(alice) while bob's hour-long lease is a few minutes old = epoch %d, no error: two holders", c2.Epoch)
				}
				got, err := s.ClaimOf(ctx, is.ID)
				if err != nil || got == nil || got.Holder != bob || got.Epoch != cb.Epoch {
					t.Errorf("ClaimOf after it all = %+v, %v; want bob's, epoch %d", got, err, cb.Epoch)
				}
				// And bob's own epoch still works.
				if _, _, err := s.FinishIssue(ctx, bob, is.ID, cb.Epoch, Finish{}); err != nil {
					t.Errorf("FinishIssue(bob, his epoch) = %v", err)
				}
				eventTimes(t, s)
			})
		}
	}
}

// clockStep is a clock.step event's after state.
type clockStep struct {
	At       time.Time `json:"at"`
	Step     int64     `json:"step_us"`
	Extended int64     `json:"extended_us"`
	Claims   int       `json:"claims"`
}

func lastClockStep(t *testing.T, s *Store) (Event, clockStep, int) {
	t.Helper()
	var last Event
	var n int
	for _, e := range eventTimes(t, s) {
		if e.Op == OpClockStep {
			last, n = e, n+1
		}
	}
	var cs clockStep
	if n > 0 {
		if err := json.Unmarshal(last.After, &cs); err != nil {
			t.Fatal(err)
		}
	}
	return last, cs, n
}

// Before this, a clock that jumped forward by more than the leases had
// left ended every claim in the next reap. Now a jump the monotonic
// clock did not see is added to every live lease, with one event, and
// no claim ends for it.
func TestForwardStepEndsNoClaim(t *testing.T) {
	t.Parallel()
	for _, jump := range []time.Duration{6 * time.Second, 20 * time.Minute, 9 * time.Hour, 400 * 24 * time.Hour} {
		t.Run(jump.String(), func(t *testing.T) {
			c := newTwoClocks()
			s, clk := steppedStore(t, "", c, Options{})
			ctx := t.Context()
			a, b, lapsed := mustCreate(t, s, NewIssue{Title: "a"}), mustCreate(t, s, NewIssue{Title: "b"}), mustCreate(t, s, NewIssue{Title: "lapsed"})
			if _, _, err := s.StartIssue(ctx, alice, lapsed.ID, time.Minute, false); err != nil {
				t.Fatal(err)
			}
			c.run(2 * time.Minute) // lapsed's lease has run out, and nothing reaped it
			_, ca, err := s.StartIssue(ctx, alice, a.ID, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			_, cb, err := s.StartIssue(ctx, bob, b.ID, 8*time.Hour, false)
			if err != nil {
				t.Fatal(err)
			}
			c.run(5 * time.Minute)

			c.step(jump)
			// Until the step is settled the store's time has not jumped,
			// so no read sees a lapsed claim either.
			if cl, err := s.ClaimOf(ctx, a.ID); err != nil || cl == nil {
				t.Fatalf("ClaimOf(a) right after the clock jumped %s = %v, %v; want alice's claim", jump, cl, err)
			}
			reaped, err := s.ReapClaims(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(reaped) != 1 || reaped[0].Issue != lapsed.ID {
				t.Fatalf("ReapClaims after the clock jumped %s ended %+v; want only the claim that had lapsed before", jump, reaped)
			}
			if got := clk.Steps(); got.Forward != 1 || got.LastBy != jump {
				t.Errorf("Steps() = %+v, want one forward step of %s", got, jump)
			}
			if now := s.Now(); !now.Equal(c.now()) {
				t.Errorf("Now() = %s after settling, want the clock's %s", now, c.now())
			}
			for _, want := range []Claim{ca, cb} {
				got, err := s.ClaimOf(ctx, want.Issue)
				if err != nil || got == nil {
					t.Fatalf("ClaimOf(%s) after the jump = %v, %v", want.Issue, got, err)
				}
				if got.Epoch != want.Epoch || !got.ExpiresAt.Equal(want.ExpiresAt.Add(jump)) {
					t.Errorf("claim on %s after a jump of %s: epoch %d, expires %s; want epoch %d, expires %s",
						want.Issue, jump, got.Epoch, got.ExpiresAt, want.Epoch, want.ExpiresAt.Add(jump))
				}
			}
			ev, cs, n := lastClockStep(t, s)
			if n != 1 || ev.Actor != ClockActor || ev.Target != ClockTarget || cs.Claims != 2 ||
				time.Duration(cs.Extended)*time.Microsecond != jump || !cs.At.Equal(c.now()) {
				t.Errorf("clock.step events: %d, the last by %+v on %q with %+v; want one, by the server, extending 2 claims by %s", n, ev.Actor, ev.Target, cs, jump)
			}

			// The leases then run out as promised: a's 15m had 10 left.
			c.run(9 * time.Minute)
			if reaped, _ := s.ReapClaims(ctx); len(reaped) != 0 {
				t.Errorf("ReapClaims 14 minutes into a 15m lease = %+v, want none", reaped)
			}
			c.run(2 * time.Minute)
			if reaped, _ := s.ReapClaims(ctx); len(reaped) != 1 || reaped[0].Issue != a.ID {
				t.Errorf("ReapClaims 16 minutes into a 15m lease = %+v, want a's claim", reaped)
			}
			// A second settle finds nothing to do.
			if err := s.SettleClock(ctx); err != nil {
				t.Fatal(err)
			}
			if _, _, n := lastClockStep(t, s); n != 1 {
				t.Errorf("%d clock.step events after one step, want 1", n)
			}
		})
	}
}

// A store settles a jump with its first write after it, which on a busy
// server may be a while later; until then its time runs on as if the
// clock had not moved. A lease that runs out in between had all of its
// time, so the settle leaves it and the reaper ends it as it was, while
// a lease still live is extended. The soak once took this for a claim
// reaped for the jump (seed 264949260793903814).
func TestLeaseThatRunsOutBeforeAJumpIsSettled(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{})
	ctx := t.Context()
	short, long := mustCreate(t, s, NewIssue{Title: "short"}), mustCreate(t, s, NewIssue{Title: "long"})
	_, cs, err := s.StartIssue(ctx, alice, short.ID, time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	_, cl, err := s.StartIssue(ctx, bob, long.ID, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.run(27 * time.Second)
	const jump = 22 * time.Minute
	c.step(jump) // short has 33 seconds left
	c.run(34 * time.Second)
	if now, want := s.Now(), cs.ExpiresAt.Add(time.Second); !now.Equal(want) {
		t.Fatalf("Now() 34s after a jump not yet settled = %s, want %s: the store's time as if the clock had not moved", now, want)
	}

	reaped, err := s.ReapClaims(ctx) // the first write since the jump
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0].Issue != short.ID || !reaped[0].ExpiresAt.Equal(cs.ExpiresAt) {
		t.Errorf("ReapClaims 61s into a 1m lease, across a jump of %s = %+v; want short's claim ended with its lease as granted, %s", jump, reaped, cs.ExpiresAt)
	}
	if got, err := s.ClaimOf(ctx, long.ID); err != nil || got == nil || !got.ExpiresAt.Equal(cl.ExpiresAt.Add(jump)) {
		t.Errorf("ClaimOf(long) = %+v, %v; want its lease extended by %s to %s", got, err, jump, cl.ExpiresAt.Add(jump))
	}
	ev, step, n := lastClockStep(t, s)
	if n != 1 || step.Claims != 1 || !ev.At.Equal(cs.ExpiresAt.Add(time.Second)) {
		t.Errorf("clock.step events: %d, the last at %s with %+v; want one, a second after short's lease ran out, extending 1 claim", n, ev.At, step)
	}
}

// A clock set back and then put right is two steps. The leases already
// had the time the store ran slow for, so the forward step gives them
// only what the jump takes beyond that: nothing, when it only undoes the
// step back.
func TestBackThenForwardStepGivesNothingTwice(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	_, cl, err := s.StartIssue(ctx, alice, is.ID, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.step(-time.Hour)
	s.Now()
	c.run(10 * time.Minute) // the store's time advances 5 minutes
	s.Now()
	c.step(time.Hour) // the clock is right again: 10 minutes after the claim
	if _, err := s.ReapClaims(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimOf(ctx, is.ID)
	if err != nil || got == nil {
		t.Fatalf("ClaimOf = %v, %v", got, err)
	}
	if !got.ExpiresAt.Equal(cl.ExpiresAt) {
		t.Errorf("lease after the clock was set back an hour and put right ten minutes later expires %s, want %s as granted",
			got.ExpiresAt, cl.ExpiresAt)
	}
	if now := s.Now(); !now.Equal(c.now()) {
		t.Errorf("Now() = %s, want the clock's %s", now, c.now())
	}
	if _, cs, n := lastClockStep(t, s); n != 1 || cs.Extended != 0 || cs.Claims != 0 {
		t.Errorf("clock.step events: %d, the last %+v; want one that extended nothing", n, cs)
	}
}

// A store that was closed when the clock jumped, as an idle project's
// is, settles the step with its first write after it opens, so its
// leases are not ended by the reap at opening either.
func TestForwardStepWhileStoreClosed(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	dsn := newDSN(t)
	clk := NewClock(ClockOptions{Read: c.read})
	s := openStore(t, dsn, Options{Clock: clk})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	_, cl, err := s.StartIssue(ctx, alice, is.ID, 8*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	c.run(time.Hour)
	clk.Now() // the daemon reads its clock on every tick
	c.step(12 * time.Hour)
	clk.Now()
	c.run(time.Minute)

	s = openStore(t, dsn, Options{Clock: clk})
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("ReapClaims at opening, after a 12h jump while the store was closed = %+v, %v; want none", reaped, err)
	}
	got, err := s.ClaimOf(ctx, is.ID)
	if err != nil || got == nil || !got.ExpiresAt.Equal(cl.ExpiresAt.Add(12*time.Hour)) {
		t.Errorf("ClaimOf after reopening = %+v, %v; want the lease extended by 12h to %s", got, err, cl.ExpiresAt.Add(12*time.Hour))
	}
	// Closed and opened again, the step is not settled twice.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, dsn, Options{Clock: clk})
	if _, err := s.ReapClaims(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, n := lastClockStep(t, s); n != 1 {
		t.Errorf("%d clock.step events after one step and two openings, want 1", n)
	}
}

// The clock is set back, so the store's time runs ahead of it, and the
// store's last event is dated after the clock's readings; the store then
// closes, as an idle project's does, and the clock jumps forward. The
// jump came after that event, whatever the wall clock read at the time:
// the store settles it at its next opening, and no claim ends for it.
func TestForwardStepWhileStoreClosedAndClockBehind(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	dsn := newDSN(t)
	clk := NewClock(ClockOptions{Read: c.read})
	s := openStore(t, dsn, Options{Clock: clk})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	_, cl, err := s.StartIssue(ctx, alice, is.ID, 8*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.step(-time.Minute)
	// The store's time does not go back, so this event is dated a minute
	// after what the clock reads.
	if _, err := s.AddComment(ctx, alice, is.ID, "still on it", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	c.run(10 * time.Second)
	clk.Now() // the daemon reads its clock on every tick
	const jump = 12 * time.Hour
	c.step(jump)
	clk.Now()
	c.run(time.Second)

	s = openStore(t, dsn, Options{Clock: clk})
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("ReapClaims at opening, after a %s jump while the store was closed and the clock a minute behind it = %+v, %v; want none", jump, reaped, err)
	}
	// The store's time moves by the jump less the minute the clock was
	// behind it; the closed store ran no slower than the clock meanwhile.
	got, err := s.ClaimOf(ctx, is.ID)
	if want := cl.ExpiresAt.Add(jump - time.Minute); err != nil || got == nil || got.ExpiresAt.Before(want) || got.ExpiresAt.After(cl.ExpiresAt.Add(jump)) {
		t.Errorf("ClaimOf after reopening = %+v, %v; want the lease extended to between %s and %s", got, err, want, cl.ExpiresAt.Add(jump))
	}
	if _, cs, n := lastClockStep(t, s); n != 1 || cs.Claims != 1 {
		t.Errorf("clock.step events: %d, the last %+v; want one that extended the claim", n, cs)
	}
}

// Two jumps while the store is closed are settled by one clock.step,
// dated on the store's time, which is before the clock's own reading at
// the second jump. If nothing else is written before the store closes
// again, that event is the database's latest, and its date must not make
// the next opening take the second jump for one not settled.
func TestTwoForwardStepsWhileStoreClosedAreSettledOnce(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	dsn := newDSN(t)
	clk := NewClock(ClockOptions{Read: c.read})
	s := openStore(t, dsn, Options{Clock: clk})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	_, cl, err := s.StartIssue(ctx, alice, is.ID, 8*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	const first, second = 2 * time.Hour, 3 * time.Hour
	for _, jump := range []time.Duration{first, second} {
		c.run(time.Minute)
		clk.Now() // the daemon reads its clock on every tick
		c.step(jump)
		clk.Now()
	}
	c.run(time.Minute)

	for opening := 1; opening <= 2; opening++ {
		s = openStore(t, dsn, Options{Clock: clk})
		if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
			t.Fatalf("opening %d: ReapClaims = %+v, %v; want none", opening, reaped, err)
		}
		if now := s.Now(); !now.Equal(c.now()) {
			t.Errorf("opening %d: Now() = %s after a write, want the clock's %s", opening, now, c.now())
		}
		got, err := s.ClaimOf(ctx, is.ID)
		if want := cl.ExpiresAt.Add(first + second); err != nil || got == nil || !got.ExpiresAt.Equal(want) {
			t.Errorf("opening %d: ClaimOf = %+v, %v; want the lease extended once by both jumps, to %s", opening, got, err, want)
		}
		if _, _, n := lastClockStep(t, s); n != 1 {
			t.Errorf("opening %d: %d clock.step events after two jumps settled together, want 1", opening, n)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		c.run(time.Minute)
	}
}

// A process that opens a database for the first time has settled none of
// the jumps its clock has seen for it, whatever the database's latest
// event is dated: the claims in it were made before the process first
// read its clock. Before this, the dates decided, and a history dated
// ahead of the clock when it jumped (the clock had been set back, or had
// run fast) made the jump look settled: the claims ended at opening.
func TestForwardStepBeforeFirstOpen(t *testing.T) {
	t.Parallel()
	const jump, back = 12 * time.Hour, time.Minute
	tests := []struct {
		name string
		// fast: the clock ran a minute fast under the process that wrote
		// the history, and was corrected before this one started. setBack:
		// it was set back a minute after this one started.
		fast, setBack bool
		// least is the least the lease is extended by; the most is the
		// jump.
		least time.Duration
	}{
		{name: "history behind the clock", least: jump},
		{name: "the clock set back after the process started", setBack: true, least: jump - back},
		{name: "the clock ran fast under the process before", fast: true, least: jump - back},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTwoClocks()
			dsn := newDSN(t)
			ctx := t.Context()
			s, _ := steppedStore(t, dsn, c, Options{}) // the process before
			is := mustCreate(t, s, NewIssue{})
			_, cl, err := s.StartIssue(ctx, alice, is.ID, 8*time.Hour, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if tc.fast {
				c.step(-back)
			}

			clk := NewClock(ClockOptions{Read: c.read}) // this process starts
			clk.Now()
			if tc.setBack {
				c.step(-back)
				clk.Now()
			}
			c.run(10 * time.Second)
			clk.Now() // the daemon reads its clock on every tick
			c.step(jump)
			clk.Now()
			c.run(time.Second)

			s = openStore(t, dsn, Options{Clock: clk})
			if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
				t.Fatalf("ReapClaims at the process's first opening, after a %s jump = %+v, %v; want none", jump, reaped, err)
			}
			got, err := s.ClaimOf(ctx, is.ID)
			if err != nil || got == nil || got.ExpiresAt.Before(cl.ExpiresAt.Add(tc.least)) || got.ExpiresAt.After(cl.ExpiresAt.Add(jump)) {
				t.Errorf("ClaimOf after the first opening = %+v, %v; want the lease extended to between %s and %s",
					got, err, cl.ExpiresAt.Add(tc.least), cl.ExpiresAt.Add(jump))
			}
			if _, cs, n := lastClockStep(t, s); n != 1 || cs.Claims != 1 || time.Duration(cs.Step)*time.Microsecond != jump {
				t.Errorf("clock.step events: %d, the last %+v; want one, of %s, that extended the claim", n, cs, jump)
			}
			// Opened again, it goes on from what it settled.
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = openStore(t, dsn, Options{Clock: clk})
			if _, err := s.ReapClaims(ctx); err != nil {
				t.Fatal(err)
			}
			if _, _, n := lastClockStep(t, s); n != 1 {
				t.Errorf("%d clock.step events after one jump and two openings, want 1", n)
			}
		})
	}
}

// A database with no history has no claim to extend, so a process that
// opens it first after a jump has nothing to settle: its log does not
// begin with a clock.step for a jump made before it held anything.
func TestFirstOpenOfAnEmptyDatabaseSettlesNothing(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	clk := NewClock(ClockOptions{Read: c.read})
	clk.Now()
	c.step(time.Hour)
	clk.Now()
	c.run(time.Second)
	s := openStore(t, newDSN(t), Options{Clock: clk})
	if st := s.ClockState(); st.Pending != 0 || !st.Now.Equal(c.now()) {
		t.Errorf("ClockState() of a new database opened after a jump = %+v; want the clock's time %s and nothing pending", st, c.now())
	}
	mustCreate(t, s, NewIssue{})
	if _, _, n := lastClockStep(t, s); n != 0 {
		t.Errorf("%d clock.step events in a database created after the jump, want none", n)
	}
}

// What a first opening cannot know is whether another process wrote the
// database after the jump: its claims then need no extension, and get
// one. The store's time never goes before the latest event, so they get
// at most the time since that event, however large the jump.
func TestFirstOpenExtendsByNoMoreThanTheTimeSinceTheLatestEvent(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	dsn := newDSN(t)
	ctx := t.Context()
	clk := NewClock(ClockOptions{Read: c.read})
	clk.Now()
	c.step(12 * time.Hour)
	clk.Now()

	other, _ := steppedStore(t, dsn, c, Options{}) // another process, after the jump
	is := mustCreate(t, other, NewIssue{})
	_, cl, err := other.StartIssue(ctx, alice, is.ID, 8*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	const since = 5 * time.Minute
	c.run(since)
	clk.Now()

	s := openStore(t, dsn, Options{Clock: clk})
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("ReapClaims at the first opening = %+v, %v; want none", reaped, err)
	}
	got, err := s.ClaimOf(ctx, is.ID)
	if err != nil || got == nil || got.ExpiresAt.Before(cl.ExpiresAt) || got.ExpiresAt.After(cl.ExpiresAt.Add(since)) {
		t.Errorf("ClaimOf after the first opening, %s after the claim was made = %+v, %v; want the lease between %s and %s",
			since, got, err, cl.ExpiresAt, cl.ExpiresAt.Add(since))
	}
	if now := s.Now(); !now.Equal(c.now()) {
		t.Errorf("Now() = %s after a write, want the clock's %s", now, c.now())
	}
}

// The clock steps while a store is opening, between the opening's reading
// of the clock and the next thing it does. The step is the store's to
// settle once: before this, the opening read the clock and its steps
// under two holds of the clock's lock, counted a step seen between them
// twice, and the leases were extended by twice the jump.
func TestForwardStepWhileStoreOpensIsSettledOnce(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	// reading is closed by the first reading of the clock made once armed
	// is set, after the reading is taken.
	var armed atomic.Bool
	reading := make(chan struct{})
	clk := NewClock(ClockOptions{Read: func() (time.Time, time.Duration) {
		wall, mono := c.read()
		if armed.CompareAndSwap(true, false) {
			close(reading)
		}
		return wall, mono
	}})
	s := openStore(t, newDSN(t), Options{Clock: clk})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	_, cl, err := s.StartIssue(ctx, alice, is.ID, 8*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.run(time.Hour)
	clk.Now()

	// The store reads the clock as Open does, for a database the process
	// has not had open. Nothing else reads this clock meanwhile: openStore
	// starts no committer. Holding the store's time stops the opening just
	// after its reading, which no sleep could do on a loaded machine.
	clk.mu.Lock()
	delete(clk.settled, s.db)
	clk.mu.Unlock()
	s.tm.mu.Lock()
	armed.Store(true)
	opened := make(chan error, 1)
	go func() { opened <- s.initTime(ctx) }()
	<-reading
	const jump = 10 * time.Minute
	c.step(jump)
	clk.Now() // waits for the opening's reading to end, and sees the step
	s.tm.mu.Unlock()
	if err := <-opened; err != nil {
		t.Fatal(err)
	}

	if st := s.ClockState(); st.Pending != jump {
		t.Errorf("ClockState().Pending after a jump of %s made while the store opened = %s, want %s", jump, st.Pending, jump)
	}
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("ReapClaims after the opening = %+v, %v; want none", reaped, err)
	}
	got, err := s.ClaimOf(ctx, is.ID)
	if err != nil || got == nil || !got.ExpiresAt.Equal(cl.ExpiresAt.Add(jump)) {
		t.Errorf("ClaimOf after a jump of %s made while the store opened = %+v, %v; want the lease extended once, to %s", jump, got, err, cl.ExpiresAt.Add(jump))
	}
	if _, cs, n := lastClockStep(t, s); n != 1 || time.Duration(cs.Step)*time.Microsecond != jump || time.Duration(cs.Extended)*time.Microsecond != jump {
		t.Errorf("clock.step events: %d, the last %+v; want one, of %s, extending by %s", n, cs, jump, jump)
	}
}

// Across a restart there is no monotonic clock to compare with, so a
// lease is measured by the wall clock: a daemon that comes back after
// the lease ran out ends the claim.
func TestLeaseAcrossRestartIsByTheWallClock(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	dsn := newDSN(t)
	s, _ := steppedStore(t, dsn, c, Options{})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	c.step(time.Hour) // down for an hour, or the clock was set while down: the store cannot tell
	s, _ = steppedStore(t, dsn, c, Options{})
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 1 {
		t.Errorf("ReapClaims after a restart an hour later = %+v, %v; want the claim ended", reaped, err)
	}
}

// The control store has no leases; it records the step all the same.
func TestControlSettlesClockStep(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	ctl := openControl(t, newDSN(t), ControlOptions{Clock: NewClock(ClockOptions{Read: c.read})})
	ctl.Now()
	c.step(time.Hour)
	if st := ctl.ClockState(); st.Pending != time.Hour {
		t.Errorf("ClockState().Pending after a jump of 1h = %s, want 1h", st.Pending)
	}
	if err := ctl.SettleClock(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := ctl.ClockState(); st.Pending != 0 || !st.Now.Equal(c.now()) {
		t.Errorf("ClockState() after settling = %+v, want the clock's time and nothing pending", st)
	}
}

// The control store opens as any store does. A jump made before the
// process first opened it is recorded in its log once, though its history
// is dated ahead of the clock at the jump; before this it followed the
// jump with nothing in its log to say so.
func TestControlFirstOpenRecordsAJumpBeforeIt(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	dsn := newDSN(t)
	ctx := t.Context()
	ctl := openControl(t, dsn, ControlOptions{Clock: NewClock(ClockOptions{Read: c.read})}) // the process before
	if _, err := ctl.AddPrincipal(ctx, dana, "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := ctl.Close(); err != nil {
		t.Fatal(err)
	}

	clk := NewClock(ClockOptions{Read: c.read}) // this process starts
	clk.Now()
	c.step(-time.Minute)
	clk.Now()
	c.run(10 * time.Second)
	clk.Now()
	c.step(time.Hour)
	clk.Now()
	c.run(time.Second)

	steps := func(ctl *Control) (n int) {
		t.Helper()
		evs, err := ctl.Events(ctx, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Op == OpClockStep {
				n++
			}
		}
		return n
	}
	for opening := 1; opening <= 2; opening++ {
		ctl = openControl(t, dsn, ControlOptions{Clock: clk})
		if st := ctl.ClockState(); opening == 1 && st.Pending != time.Hour {
			t.Errorf("ClockState().Pending at the process's first opening, after a jump of 1h = %s, want 1h", st.Pending)
		}
		if _, err := ctl.AddPrincipal(ctx, dana, []string{"bob", "carol"}[opening-1], ""); err != nil {
			t.Fatal(err)
		}
		if st := ctl.ClockState(); st.Pending != 0 || !st.Now.Equal(c.now()) {
			t.Errorf("opening %d: ClockState() after a write = %+v; want the clock's time %s and nothing pending", opening, st, c.now())
		}
		if n := steps(ctl); n != 1 {
			t.Errorf("opening %d: %d clock.step events in the control log after one jump, want 1", opening, n)
		}
		if err := ctl.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// A client's own clock decides nothing but the time it claims for a
// usage record, and that is bounded ahead of the server's time. Behind
// it is not: records are sent late, and the daily cap counts them by
// when the server stored them.
func TestClientAhead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ahead   time.Duration // Options.ClientAhead
		at      time.Duration // the record's time, from the server's
		refused bool
	}{
		{name: "default, now", at: 0},
		{name: "default, four minutes ahead", at: 4 * time.Minute},
		{name: "default, six minutes ahead", at: 6 * time.Minute, refused: true},
		{name: "default, a year behind", at: -365 * 24 * time.Hour},
		{name: "an hour allowed, half an hour ahead", ahead: time.Hour, at: 30 * time.Minute},
		{name: "an hour allowed, two hours ahead", ahead: time.Hour, at: 2 * time.Hour, refused: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
			s := openStore(t, newDSN(t), Options{Now: c.now, ClientAhead: tc.ahead})
			n := int64(1)
			_, err := s.AddUsage(t.Context(), alice, []UsageRecord{{RequestID: "r", Model: "m", Harness: "h",
				At: c.now().Add(tc.at), Granularity: GranularityRequest, Input: &n}}, "")
			if got := errors.Is(err, ErrInvalid); got != tc.refused || (err != nil && !got) {
				t.Errorf("AddUsage(at the server's time %+v, Options.ClientAhead %s) = %v, want refused %v", tc.at, tc.ahead, err, tc.refused)
			}
		})
	}
	for _, bad := range []time.Duration{-time.Second, MaxClientAhead + time.Second} {
		if _, err := Open(t.Context(), newDSN(t), Options{ClientAhead: bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("Open(ClientAhead %s) = %v, want ErrInvalid", bad, err)
		}
	}
}

// A write that checks a client's time against now settles a forward step
// first: a record made after the machine resumed is not "in the future"
// because the store had not yet followed the clock.
func TestClientTimeAfterForwardStep(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{})
	c.step(9 * time.Hour)
	n := int64(1)
	if _, err := s.AddUsage(ctx, alice, []UsageRecord{{RequestID: "r", Model: "m", Harness: "h", At: c.now(),
		Granularity: GranularityRequest, Input: &n}}, ""); err != nil {
		t.Errorf("AddUsage(a record at the clock's time) first after a 9h jump = %v, want it stored", err)
	}
	c.step(48 * time.Hour)
	if _, err := s.LogHours(ctx, alice, NewHours{Issue: is.ID, Duration: time.Hour, On: startDay(c.now())}); err != nil {
		t.Errorf("LogHours(on the clock's today) first after a 2d jump = %v, want it logged", err)
	}
}

// Windows counted back from now do not reopen when the clock is set
// back: the daily cap on usage records.
func TestWindowsDoNotReopenOnBackwardStep(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{Limits: Limits{UsagePerDay: 2}})
	ctx := t.Context()
	n := int64(1)
	rec := func(id string) []UsageRecord {
		return []UsageRecord{{RequestID: id, Model: "m", Harness: "h", At: c.now().Add(-time.Minute), Granularity: GranularityRequest, Input: &n}}
	}
	for _, id := range []string{"r1", "r2"} {
		if _, err := s.AddUsage(ctx, alice, rec(id), ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, back := range []time.Duration{-time.Hour, -48 * time.Hour} {
		c.step(back)
		if _, err := s.AddUsage(ctx, alice, rec("more"+back.String()), ""); !errors.Is(err, ErrBusy) {
			t.Errorf("AddUsage past the daily cap, with the clock set back %s = %v, want ErrBusy", back, err)
		}
	}
}
