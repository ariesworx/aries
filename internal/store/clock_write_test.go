package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// waits is a Tracer that calls the func a test sets just before each wait
// for the writer, on the goroutine that is about to wait, with the wait's
// number since the func was set. A write is then past the settle it
// begins with and has not begun its transaction: where a write queues
// behind the writes ahead of it.
type waits struct {
	mu sync.Mutex
	n  int
	fn func(n int)
}

func (w *waits) on(fn func(n int)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n, w.fn = 0, fn
}

// count returns how many waits for the writer there have been since the
// func was last set, and starts the count again.
func (w *waits) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.n
	w.n = 0
	return n
}

func (w *waits) Begin(ctx context.Context, stage Stage, _ int) (context.Context, func(Outcome)) {
	if stage == StageWait {
		w.mu.Lock()
		w.n++
		n, fn := w.n, w.fn
		w.mu.Unlock()
		if fn != nil {
			fn(n)
		}
	}
	return ctx, untraced
}

// hold stops the next k writes at their wait for the writer. arrived
// returns once all k are there, and release lets them go; the test's end
// releases them if the test did not.
func (w *waits) hold(t *testing.T, k int) (arrived, release func()) {
	t.Helper()
	reached, gate := make(chan struct{}, k), make(chan struct{})
	w.on(func(n int) {
		if n <= k {
			reached <- struct{}{}
			<-gate
		}
	})
	release = sync.OnceFunc(func() { close(gate) })
	t.Cleanup(release)
	return func() {
		for range k {
			<-reached
		}
	}, release
}

// around returns the times of the events on target, and how many
// clock.step events the log holds before the first of them and from it
// on.
func around(evs []Event, target string) (at []time.Time, before, after int) {
	for _, e := range evs {
		switch {
		case e.Target == target:
			at = append(at, e.At)
		case e.Op != OpClockStep:
		case len(at) == 0:
			before++
		default:
			after++
		}
	}
	return at, before, after
}

// settledSteps sums, over the clock.step events of evs, the size of the
// steps settled and how far each extended the leases it found live.
func settledSteps(t *testing.T, evs []Event) (step, extended time.Duration) {
	t.Helper()
	for _, e := range evs {
		if e.Op != OpClockStep {
			continue
		}
		var cs clockStep
		if err := json.Unmarshal(e.After, &cs); err != nil {
			t.Fatalf("clock.step event %d: %v", e.Seq, err)
		}
		step += time.Duration(cs.Step) * time.Microsecond
		extended += time.Duration(cs.Extended) * time.Microsecond
	}
	return step, extended
}

// leaseOf returns when the claim on id runs out.
func leaseOf(t *testing.T, s *Store, id IssueID) time.Time {
	t.Helper()
	cl, err := s.ClaimOf(t.Context(), id)
	if err != nil || cl == nil {
		t.Fatalf("ClaimOf(%s) = %v, %v; want a claim", id, cl, err)
	}
	return cl.ExpiresAt
}

// A write settles the clock and then waits for the writer. Before this, a
// jump forward made while it waited left it dated on the time before the
// jump, with no clock.step ahead of it, though it ran after the jump: an
// effect could be dated before its cause in another store's log.
func TestWriteQueuedBeforeAJumpIsDatedAfterIt(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	w := &waits{}
	s, _ := steppedStore(t, "", c, Options{Tracer: w})
	ctx := t.Context()
	held := mustCreate(t, s, NewIssue{Title: "held"})
	_, cl, err := s.StartIssue(ctx, bob, held.ID, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.run(time.Minute)

	const jump = 12 * time.Minute
	w.on(func(n int) {
		if n == 1 {
			c.step(jump)
		}
	})
	queued, err := s.CreateIssue(ctx, alice, NewIssue{Title: "queued"})
	if err != nil {
		t.Fatalf("CreateIssue across a jump of %s = %v", jump, err)
	}

	at, before, after := around(eventTimes(t, s), string(queued.ID))
	if len(at) != 1 || !at[0].Equal(c.now()) || before != 1 || after != 0 {
		t.Errorf("a write that waited for the writer across a jump of %s is dated %s, with %d clock.step events before it and %d after; want the clock's %s, the time after the jump, and one before",
			jump, at, before, after, c.now())
	}
	if got, want := leaseOf(t, s, held.ID), cl.ExpiresAt.Add(jump); !got.Equal(want) {
		t.Errorf("lease after the jump = %s, want it extended once, to %s", got, want)
	}
	if st := s.ClockState(); st.Pending != 0 {
		t.Errorf("ClockState().Pending after the write = %s, want 0", st.Pending)
	}
}

// Two writes are both past their settle when the clock jumps. Whichever
// finds the step first settles it, and the other does not settle it
// again.
func TestTwoWritesQueuedBeforeAJumpSettleItOnce(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	w := &waits{}
	s, _ := steppedStore(t, "", c, Options{Tracer: w})
	ctx := t.Context()
	held := mustCreate(t, s, NewIssue{Title: "held"})
	_, cl, err := s.StartIssue(ctx, bob, held.ID, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.run(time.Minute)

	arrived, release := w.hold(t, 2)
	type created struct {
		id  IssueID
		err error
	}
	done := make(chan created, 2)
	for _, actor := range []Actor{alice, bob} {
		go func() {
			is, err := s.CreateIssue(ctx, actor, NewIssue{Title: "queued by " + actor.Principal})
			done <- created{is.ID, err}
		}()
	}
	arrived()
	const jump = 12 * time.Minute
	c.step(jump)
	release()
	var ids []IssueID
	for range 2 {
		r := <-done
		if r.err != nil {
			t.Fatalf("CreateIssue across a jump of %s = %v", jump, r.err)
		}
		ids = append(ids, r.id)
	}

	evs := eventTimes(t, s)
	for _, id := range ids {
		at, before, after := around(evs, string(id))
		if len(at) != 1 || !at[0].Equal(c.now()) || before != 1 || after != 0 {
			t.Errorf("%s, one of two writes that waited across a jump of %s, is dated %s, with %d clock.step events before it and %d after; want the clock's %s and one before",
				id, jump, at, before, after, c.now())
		}
	}
	if got, want := leaseOf(t, s, held.ID), cl.ExpiresAt.Add(jump); !got.Equal(want) {
		t.Errorf("lease after the jump = %s, want it extended once, to %s", got, want)
	}
}

// A write that loses to a concurrent transaction is run again from the
// start, and the clock may have jumped since its settle: the rerun is
// dated after the jump too.
func TestWriteRetriedAfterAJumpIsDatedAfterIt(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	o := &seen{}
	s, _ := steppedStore(t, "", c, Options{Observer: o})
	ctx := t.Context()
	held := mustCreate(t, s, NewIssue{Title: "held"})
	_, cl, err := s.StartIssue(ctx, bob, held.ID, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.run(time.Minute)
	o.take()

	const jump = 12 * time.Minute
	lost := false
	s.beforeCommit = func(context.Context) error {
		if lost {
			return nil
		}
		lost = true
		c.step(jump)
		return fmt.Errorf("%w: lost to a concurrent transaction", errRetry)
	}
	retried, err := s.CreateIssue(ctx, alice, NewIssue{Title: "retried"})
	if err != nil {
		t.Fatalf("CreateIssue, its first attempt lost and the clock jumped %s = %v", jump, err)
	}

	if _, n, _, _ := o.take(); n != 1 {
		t.Errorf("the write was retried %d times, want 1", n)
	}
	at, before, after := around(eventTimes(t, s), string(retried.ID))
	if len(at) != 1 || !at[0].Equal(c.now()) || before != 1 || after != 0 {
		t.Errorf("a write rerun after a jump of %s is dated %s, with %d clock.step events before it and %d after; want the clock's %s and one before",
			jump, at, before, after, c.now())
	}
	if got, want := leaseOf(t, s, held.ID), cl.ExpiresAt.Add(jump); !got.Equal(want) {
		t.Errorf("lease after the jump = %s, want it extended once, to %s", got, want)
	}
}

// The clock jumps a second time while the first jump is being settled
// for a write that found it. The write does not go on with a jump
// pending: it is run again, settles the second, and each jump is settled
// once.
func TestSecondJumpDuringTheSettle(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	w, o := &waits{}, &seen{}
	s, _ := steppedStore(t, "", c, Options{Tracer: w, Observer: o})
	ctx := t.Context()
	held := mustCreate(t, s, NewIssue{Title: "held"})
	_, cl, err := s.StartIssue(ctx, bob, held.ID, time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.run(time.Minute)
	o.take()

	const first, second = 12 * time.Minute, 7 * time.Minute
	w.on(func(n int) {
		switch n {
		case 1: // the write's own wait
			c.step(first)
		case 2: // the wait of the settle it then makes
			c.step(second)
		}
	})
	queued, err := s.CreateIssue(ctx, alice, NewIssue{Title: "queued"})
	if err != nil {
		t.Fatalf("CreateIssue across jumps of %s and %s = %v", first, second, err)
	}

	if _, n, _, _ := o.take(); n != 1 {
		t.Errorf("the write was retried %d times, want 1: once, for the jump made during the settle", n)
	}
	evs := eventTimes(t, s)
	at, before, after := around(evs, string(queued.ID))
	if len(at) != 1 || !at[0].Equal(c.now()) || before != 2 || after != 0 {
		t.Errorf("a write across jumps of %s and %s is dated %s, with %d clock.step events before it and %d after; want the clock's %s and two before",
			first, second, at, before, after, c.now())
	}
	if step, extended := settledSteps(t, evs); step != first+second || extended != first+second {
		t.Errorf("clock.step events settle %s and extend leases by %s, want %s for both", step, extended, first+second)
	}
	if got, want := leaseOf(t, s, held.ID), cl.ExpiresAt.Add(first+second); !got.Equal(want) {
		t.Errorf("lease after both jumps = %s, want it extended by each once, to %s", got, want)
	}
}

// A clock that jumps every time a write waits for the writer cannot spin
// the write: it is refused once its attempts run out, having written
// nothing on a time before a jump, and the jumps are still each settled
// once.
func TestClockThatKeepsJumpingRefusesTheWrite(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	w := &waits{}
	s, _ := steppedStore(t, "", c, Options{Tracer: w, MaxAttempts: 3})
	ctx := t.Context()
	held := mustCreate(t, s, NewIssue{Title: "held"})
	_, cl, err := s.StartIssue(ctx, bob, held.ID, 8*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	c.run(time.Minute)
	events := len(eventTimes(t, s))

	const jump = time.Minute
	var total time.Duration
	w.on(func(int) {
		c.step(jump)
		total += jump
	})
	_, err = s.CreateIssue(ctx, alice, NewIssue{Title: "refused"})
	w.on(nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("CreateIssue with the clock jumping at every wait for the writer = %v, want ErrConflict", err)
	}
	for _, e := range eventTimes(t, s)[events:] {
		if e.Op != OpClockStep {
			t.Errorf("the refused write left event %d (%s on %s), want only clock.step events", e.Seq, e.Op, e.Target)
		}
	}

	after, err := s.CreateIssue(ctx, alice, NewIssue{Title: "after"})
	if err != nil {
		t.Fatalf("CreateIssue once the clock is still = %v", err)
	}
	evs := eventTimes(t, s)
	if at, _, later := around(evs, string(after.ID)); len(at) != 1 || !at[0].Equal(c.now()) || later != 0 {
		t.Errorf("the next write is dated %s with %d clock.step events after it; want the clock's %s and none", at, later, c.now())
	}
	if step, extended := settledSteps(t, evs); step != total || extended != total {
		t.Errorf("clock.step events settle %s and extend leases by %s, want %s, the sum of the jumps, for both", step, extended, total)
	}
	if got, want := leaseOf(t, s, held.ID), cl.ExpiresAt.Add(total); !got.Equal(want) {
		t.Errorf("lease after jumps of %s in all = %s, want it extended by each once, to %s", total, got, want)
	}
}

// The control store has no leases, and dates its events as a project's
// store does: a write that waited for its writer across a jump is dated
// after it, behind one clock.step.
func TestControlWriteQueuedBeforeAJumpIsDatedAfterIt(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	w := &waits{}
	ctl := openControl(t, newDSN(t), ControlOptions{Clock: NewClock(ClockOptions{Read: c.read}), Tracer: w})
	ctx := t.Context()
	if _, err := ctl.AddPrincipal(ctx, dana, "alice", ""); err != nil {
		t.Fatal(err)
	}
	c.run(time.Minute)

	const jump = time.Hour
	w.on(func(n int) {
		if n == 1 {
			c.step(jump)
		}
	})
	if _, err := ctl.AddPrincipal(ctx, dana, "bob", ""); err != nil {
		t.Fatalf("AddPrincipal across a jump of %s = %v", jump, err)
	}

	evs, err := ctl.Events(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	at, before, after := around(evs, "bob")
	if len(at) != 1 || !at[0].Equal(c.now()) || before != 1 || after != 0 {
		t.Errorf("a control write that waited for the writer across a jump of %s is dated %s, with %d clock.step events before it and %d after; want the clock's %s and one before",
			jump, at, before, after, c.now())
	}
	if st := ctl.ClockState(); st.Pending != 0 {
		t.Errorf("ClockState().Pending after the write = %s, want 0", st.Pending)
	}
}
