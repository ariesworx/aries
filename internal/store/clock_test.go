package store

import (
	"encoding/json"
	"errors"
	"sync"
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

// A clock built from Now alone, with times that carry no monotonic
// reading, never shows a step: there is nothing to compare the wall
// clock with.
func TestClockWithoutMonotonicReadings(t *testing.T) {
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

// A clock set back and then put right is two steps. The leases already
// had the time the store ran slow for, so the forward step gives them
// only what the jump takes beyond that: nothing, when it only undoes the
// step back.
func TestBackThenForwardStepGivesNothingTwice(t *testing.T) {
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

// Across a restart there is no monotonic clock to compare with, so a
// lease is measured by the wall clock: a daemon that comes back after
// the lease ran out ends the claim.
func TestLeaseAcrossRestartIsByTheWallClock(t *testing.T) {
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

// A client's own clock decides nothing but the time it claims for a
// usage record, and that is bounded ahead of the server's time. Behind
// it is not: records are sent late, and the daily cap counts them by
// when the server stored them.
func TestClientAhead(t *testing.T) {
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
				At: c.now().Add(tc.at), Granularity: GranularityRequest, Input: &n}})
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

// Windows counted back from now do not reopen when the clock is set
// back: the daily cap on usage records.
func TestWindowsDoNotReopenOnBackwardStep(t *testing.T) {
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{Limits: Limits{UsagePerDay: 2}})
	ctx := t.Context()
	n := int64(1)
	rec := func(id string) []UsageRecord {
		return []UsageRecord{{RequestID: id, Model: "m", Harness: "h", At: c.now().Add(-time.Minute), Granularity: GranularityRequest, Input: &n}}
	}
	for _, id := range []string{"r1", "r2"} {
		if _, err := s.AddUsage(ctx, alice, rec(id)); err != nil {
			t.Fatal(err)
		}
	}
	for _, back := range []time.Duration{-time.Hour, -48 * time.Hour} {
		c.step(back)
		if _, err := s.AddUsage(ctx, alice, rec("more"+back.String())); !errors.Is(err, ErrBusy) {
			t.Errorf("AddUsage past the daily cap, with the clock set back %s = %v, want ErrBusy", back, err)
		}
	}
}
