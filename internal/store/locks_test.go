package store

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func mustAcquire(t *testing.T, s *Store, a Actor, in LockAcquire) LockState {
	t.Helper()
	st, err := s.AcquireLock(t.Context(), a, in)
	if err != nil {
		t.Fatalf("AcquireLock(%+v) by %s: %v", in, a.Principal, err)
	}
	return st
}

// lockOps returns the ops of the events on lock name, oldest first.
func lockOps(t *testing.T, s *Store, name string) []Op {
	t.Helper()
	evs, err := s.events(t.Context(), `WHERE target = ? ORDER BY seq`, LockTarget(name))
	if err != nil {
		t.Fatal(err)
	}
	var out []Op
	for _, e := range evs {
		out = append(out, e.Op)
	}
	return out
}

// lockOf reads lock name, or fails when nobody holds or waits for it.
func lockOf(t *testing.T, s *Store, name string) Lock {
	t.Helper()
	ls, err := s.Locks(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 {
		t.Fatalf("Locks(%q) = %+v, want the one lock", name, ls)
	}
	return ls[0]
}

func holderPrincipals(l Lock) []string {
	var out []string
	for _, h := range l.Holders {
		out = append(out, h.Holder.Principal)
	}
	return out
}

func waiterPrincipals(l Lock) []string {
	var out []string
	for _, w := range l.Waiters {
		out = append(out, w.Holder.Principal)
	}
	return out
}

// A mutex is granted to one holder at a time, under an epoch that rises
// with each grant and fences a renewal or release that names another;
// a second acquirer that does not wait is refused, told who holds it and
// until when.
func TestLockGrantsAndFences(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()

	first := mustAcquire(t, s, alice, LockAcquire{Name: "deploy/prod"})
	if first.Held == nil || first.Held.Epoch != 1 || !first.Held.ExpiresAt.Equal(clk.now().Add(DefaultLease)) || first.Capacity != 1 {
		t.Fatalf("AcquireLock(deploy/prod) = %+v, want epoch 1 to now+15m, capacity 1", first)
	}
	_, err := s.AcquireLock(ctx, bob, LockAcquire{Name: "deploy/prod"})
	busy, ok := errors.AsType[*LockBusyError](err)
	if !ok || !errors.Is(err, ErrBusy) || len(busy.Holders) != 1 || busy.Holders[0].Holder != alice ||
		!busy.Holders[0].ExpiresAt.Equal(first.Held.ExpiresAt) {
		t.Fatalf("AcquireLock by bob while alice holds = %v, want a LockBusyError naming alice until %s", err, first.Held.ExpiresAt)
	}
	for _, epoch := range []int64{0, 2} {
		err := s.ReleaseLock(ctx, alice, "deploy/prod", epoch)
		if stale, ok := errors.AsType[*LockEpochError](err); !ok || !errors.Is(err, ErrConflict) || stale.Epoch != epoch {
			t.Errorf("ReleaseLock(epoch %d) of alice's epoch 1 = %v, want a LockEpochError (conflict)", epoch, err)
		}
	}
	if err := s.ReleaseLock(ctx, bob, "deploy/prod", 1); !errors.Is(err, ErrConflict) {
		t.Errorf("ReleaseLock by bob of alice's epoch 1 = %v, want conflict: bob holds no epoch 1", err)
	}
	if err := s.ReleaseLock(ctx, alice, "deploy/prod", 1); err != nil {
		t.Fatalf("ReleaseLock by alice of epoch 1: %v", err)
	}
	if ls, err := s.Locks(ctx, ""); err != nil || len(ls) != 0 {
		t.Errorf("Locks after the last release = %+v, %v; want none: an idle lock is gone", ls, err)
	}
	// The row went with the last holder, and the epoch still rises.
	second := mustAcquire(t, s, bob, LockAcquire{Name: "deploy/prod"})
	if second.Held == nil || second.Held.Epoch != 2 {
		t.Fatalf("AcquireLock after a release = %+v, want epoch 2", second)
	}
	if err := s.ReleaseLock(ctx, alice, "deploy/prod", 1); !errors.Is(err, ErrConflict) {
		t.Errorf("ReleaseLock of a released epoch = %v, want conflict", err)
	}
	if got, want := lockOps(t, s, "deploy/prod"), []Op{OpLockAcquire, OpLockRelease, OpLockAcquire}; !slices.Equal(got, want) {
		t.Errorf("events on lock:deploy/prod = %v, want %v", got, want)
	}
}

// Acquiring a lock one already holds, or waits for, is no new grant: a
// retried acquire whose answer was lost gets the hold or the place it
// made, and holds no second slot of a semaphore.
func TestLockAcquireAgain(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	first := mustAcquire(t, s, alice, LockAcquire{Name: "staging", Capacity: 2})
	clk.add(time.Minute)
	again := mustAcquire(t, s, alice, LockAcquire{Name: "staging"})
	if again.Held == nil || again.Held.Epoch != first.Held.Epoch || !again.Held.ExpiresAt.Equal(clk.now().Add(DefaultLease)) {
		t.Errorf("AcquireLock again = %+v, want epoch %d extended to now+15m", again, first.Held.Epoch)
	}
	if l := lockOf(t, s, "staging"); len(l.Holders) != 1 {
		t.Errorf("holders after acquiring twice = %+v, want one", l.Holders)
	}
	mustAcquire(t, s, bob, LockAcquire{Name: "staging"})
	w1 := mustAcquire(t, s, carol, LockAcquire{Name: "staging", Wait: true})
	w2 := mustAcquire(t, s, carol, LockAcquire{Name: "staging", Wait: true})
	if w1.Position != 1 || w2.Position != 1 || w2.Held != nil {
		t.Errorf("AcquireLock --wait twice = %+v then %+v, want position 1 both times", w1, w2)
	}
	if got, want := lockOps(t, s, "staging"), []Op{OpLockAcquire, OpLockAcquire, OpLockWait}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// A lock's capacity is fixed by its first acquire; within the limit, a
// semaphore grants that many holds and no more.
func TestLockCapacity(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	for _, a := range []Actor{alice, bob, carol} {
		mustAcquire(t, s, a, LockAcquire{Name: "api", Capacity: 3})
	}
	if _, err := s.AcquireLock(ctx, erin, LockAcquire{Name: "api", Capacity: 3}); !errors.Is(err, ErrBusy) {
		t.Errorf("a fourth acquire of a lock of capacity 3 = %v, want busy", err)
	}
	if _, err := s.AcquireLock(ctx, erin, LockAcquire{Name: "api", Capacity: 4}); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), "capacity 3") {
		t.Errorf("an acquire naming capacity 4 of a lock of 3 = %v, want invalid naming capacity 3", err)
	}
	l := lockOf(t, s, "api")
	if l.Capacity != 3 || !slices.Equal(holderPrincipals(l), []string{"alice", "bob", "carol"}) {
		t.Errorf("Locks(api) = %+v, want capacity 3 held by alice, bob and carol", l)
	}
	for _, tc := range []struct {
		name     string
		capacity int
	}{
		{"negative", -1},
		{"past the limit", DefaultLimits.LockCapacity + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.AcquireLock(ctx, alice, LockAcquire{Name: "other", Capacity: tc.capacity}); !errors.Is(err, ErrInvalid) {
				t.Errorf("AcquireLock(capacity %d) = %v, want invalid", tc.capacity, err)
			}
		})
	}
}

// A lock name is a short path of lowercase words, which as an event's
// target, lock:NAME, never reads as an issue id.
func TestLockNames(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"deploy", true},
		{"merge/main", true},
		{"0-api.rate_limit", true},
		{strings.Repeat("a", MaxLockName), true},
		{strings.Repeat("a", MaxLockName+1), false},
		{"", false},
		{"Deploy", false},
		{"-deploy", false},
		{"/deploy", false},
		{"deploy prod", false},
		{"lock:x", false},
		{"dé", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AcquireLock(t.Context(), alice, LockAcquire{Name: tc.name})
			if tc.ok != (err == nil) || !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Errorf("AcquireLock(%q) = %v, want ok %v (else invalid)", tc.name, err, tc.ok)
			}
			if tc.ok && (IssueID(LockTarget(tc.name)).Validate() == nil || len(LockTarget(tc.name)) > 64) {
				t.Errorf("LockTarget(%q) = %q, which reads as an issue id or is longer than an event's target", tc.name, LockTarget(tc.name))
			}
		})
	}
}

// A hold's lease is one a claim may have: a minute to a day.
func TestLockLeaseBounds(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	for _, tc := range []struct {
		lease time.Duration
		ok    bool
	}{
		{0, true},
		{MinLease, true},
		{MaxClaimLease, true},
		{MinLease - time.Second, false},
		{MaxClaimLease + time.Second, false},
		{-time.Minute, false},
	} {
		t.Run(tc.lease.String(), func(t *testing.T) {
			name := "l" + strings.ReplaceAll(tc.lease.String(), "-", "n")
			st, err := s.AcquireLock(t.Context(), alice, LockAcquire{Name: name, Lease: tc.lease})
			if tc.ok != (err == nil) || !tc.ok && !errors.Is(err, ErrInvalid) {
				t.Fatalf("AcquireLock(lease %s) = %v, want ok %v (else invalid)", tc.lease, err, tc.ok)
			}
			if want := clk.now().Add(cmpOr(tc.lease, DefaultLease)); tc.ok && !st.Held.ExpiresAt.Equal(want) {
				t.Errorf("AcquireLock(lease %s) expires %s, want %s", tc.lease, st.Held.ExpiresAt, want)
			}
		})
	}
}

// Waiters are served in the order they joined, skipping any whose place
// lapsed; a release grants the next in the same write, and tells it with
// a lock.granted item in its session.
func TestLockFIFO(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	bobWatch := s.Watch("bob", bob.Session)
	defer bobWatch.Close()

	held := mustAcquire(t, s, alice, LockAcquire{Name: "merge/main"})
	for i, a := range []Actor{bob, carol, erin} {
		st := mustAcquire(t, s, a, LockAcquire{Name: "merge/main", Wait: true, Lease: time.Hour})
		if st.Held != nil || st.Position != i+1 || !st.WaitExpires.Equal(clk.now().Add(WaiterLease)) {
			t.Fatalf("AcquireLock --wait by %s = %+v, want position %d until now+2m", a.Principal, st, i+1)
		}
	}
	// carol stops renewing; bob and erin keep their places.
	clk.add(90 * time.Second)
	for _, a := range []Actor{bob, erin} {
		if _, err := s.RenewLock(ctx, a, "merge/main", 0, 0); err != nil {
			t.Fatalf("RenewLock of %s's place: %v", a.Principal, err)
		}
	}
	clk.add(time.Minute)
	if l := lockOf(t, s, "merge/main"); !slices.Equal(waiterPrincipals(l), []string{"bob", "erin"}) {
		t.Errorf("waiters once carol's place lapsed = %v, want bob then erin", waiterPrincipals(l))
	}
	if err := s.ReleaseLock(ctx, alice, "merge/main", held.Held.Epoch); err != nil {
		t.Fatal(err)
	}
	l := lockOf(t, s, "merge/main")
	if len(l.Holders) != 1 || l.Holders[0].Holder != bob || l.Holders[0].Epoch != 2 ||
		!l.Holders[0].ExpiresAt.Equal(clk.now().Add(time.Hour)) {
		t.Fatalf("holders after alice's release = %+v, want bob at epoch 2 for the hour he asked", l.Holders)
	}
	items, _, _ := bobWatch.Take()
	if len(items) != 1 || items[0].Kind != InboxLockGranted || items[0].Session != bob.Session || !strings.Contains(items[0].Body, "merge/main") ||
		!strings.Contains(items[0].Body, "epoch 2") {
		t.Errorf("bob's pushed items = %+v, want one lock.granted for merge/main at epoch 2 in his session", items)
	}
	// bob learns of it by renewing his place, as aries lock run does.
	st, err := s.RenewLock(ctx, bob, "merge/main", 0, 0)
	if err != nil || st.Held == nil || st.Held.Epoch != 2 {
		t.Errorf("RenewLock of bob's place once granted = %+v, %v; want his hold at epoch 2", st, err)
	}
	if err := s.ReleaseLock(ctx, bob, "merge/main", 2); err != nil {
		t.Fatal(err)
	}
	if l := lockOf(t, s, "merge/main"); len(l.Holders) != 1 || l.Holders[0].Holder != erin || l.Holders[0].Epoch != 3 {
		t.Errorf("holders after bob's release = %+v, want erin at epoch 3", l.Holders)
	}
	want := []Op{OpLockAcquire, OpLockWait, OpLockWait, OpLockWait, OpLockLeave, OpLockRelease, OpLockGrant, OpLockRelease, OpLockGrant}
	if got := lockOps(t, s, "merge/main"); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// A hold whose lease ran out holds nothing, before the reaper too: an
// acquire finds the lock free, and the holder's renewal is refused.
func TestLapsedHoldHoldsNothing(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	old := mustAcquire(t, s, alice, LockAcquire{Name: "deploy", Lease: time.Minute})
	clk.add(2 * time.Minute)
	st := mustAcquire(t, s, bob, LockAcquire{Name: "deploy"})
	if st.Held == nil || st.Held.Epoch != 2 {
		t.Fatalf("AcquireLock of a lapsed lock = %+v, want epoch 2", st)
	}
	_, err := s.RenewLock(ctx, alice, "deploy", old.Held.Epoch, 0)
	if stale, ok := errors.AsType[*LockEpochError](err); !ok || stale.Epoch != 1 {
		t.Errorf("RenewLock of a lapsed hold = %v, want a LockEpochError for epoch 1", err)
	}
	if got, want := lockOps(t, s, "deploy"), []Op{OpLockAcquire, OpLockExpire, OpLockAcquire}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// A lapsed hold frees its slot for the queue first: whoever waited gets
// it, and an acquire that does not wait is still refused.
func TestLapsedHoldGoesToTheQueue(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	mustAcquire(t, s, alice, LockAcquire{Name: "deploy", Lease: time.Minute})
	mustAcquire(t, s, bob, LockAcquire{Name: "deploy", Wait: true})
	clk.add(90 * time.Second)
	if _, err := s.AcquireLock(t.Context(), carol, LockAcquire{Name: "deploy"}); !errors.Is(err, ErrBusy) {
		t.Errorf("AcquireLock by carol once alice's hold lapsed = %v, want busy: bob waited first", err)
	}
	st, err := s.RenewLock(t.Context(), bob, "deploy", 0, 0)
	if err != nil || st.Held == nil || st.Held.Epoch != 2 {
		t.Errorf("RenewLock of bob's place once alice's hold lapsed = %+v, %v; want his hold at epoch 2", st, err)
	}
}

// Renewing a hold extends it, quietly; renewing a place in the queue
// keeps it, quietly; renewing what one neither holds nor waits for is a
// conflict.
func TestLockRenew(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	h := mustAcquire(t, s, alice, LockAcquire{Name: "deploy", Lease: 10 * time.Minute})
	mustAcquire(t, s, bob, LockAcquire{Name: "deploy", Wait: true})
	seq := lastSeq(t, s)
	clk.add(time.Minute)
	got, err := s.RenewLock(ctx, bob, "deploy", 0, 0)
	if err != nil || got.Position != 1 || !got.WaitExpires.Equal(clk.now().Add(WaiterLease)) {
		t.Errorf("RenewLock of bob's place = %+v, %v; want position 1 until now+2m", got, err)
	}
	clk.add(time.Minute)
	got, err = s.RenewLock(ctx, alice, "deploy", h.Held.Epoch, 0)
	if err != nil || got.Held == nil || !got.Held.ExpiresAt.Equal(clk.now().Add(10*time.Minute)) {
		t.Errorf("RenewLock of the hold = %+v, %v; want it extended by its own 10m from now", got, err)
	}
	got, err = s.RenewLock(ctx, alice, "deploy", h.Held.Epoch, time.Hour)
	if err != nil || got.Held == nil || !got.Held.ExpiresAt.Equal(clk.now().Add(time.Hour)) {
		t.Errorf("RenewLock for an hour = %+v, %v; want now+1h", got, err)
	}
	if n := lastSeq(t, s); n != seq {
		t.Errorf("renewals recorded %d events, want none", n-seq)
	}
	if _, err := s.RenewLock(ctx, carol, "deploy", 0, 0); !errors.Is(err, ErrConflict) {
		t.Errorf("RenewLock by carol, who neither holds nor waits = %v, want conflict", err)
	}
	if _, err := s.RenewLock(ctx, alice, "deploy", h.Held.Epoch, 2*MaxClaimLease); !errors.Is(err, ErrInvalid) {
		t.Errorf("RenewLock past the longest lease = %v, want invalid", err)
	}
}

// Leaving the queue ends one's place; leaving a lock one holds, or never
// waited for, changes nothing and says which.
func TestLockLeave(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	mustAcquire(t, s, alice, LockAcquire{Name: "deploy"})
	mustAcquire(t, s, bob, LockAcquire{Name: "deploy", Wait: true})
	left, err := s.LeaveLock(ctx, bob, "deploy")
	if err != nil || !left.Left {
		t.Errorf("LeaveLock by bob = %+v, %v; want left", left, err)
	}
	if l := lockOf(t, s, "deploy"); len(l.Waiters) != 0 {
		t.Errorf("waiters after bob left = %+v, want none", l.Waiters)
	}
	seq := lastSeq(t, s)
	held, err := s.LeaveLock(ctx, alice, "deploy")
	if err != nil || held.Left || held.Held == nil || held.Held.Epoch != 1 {
		t.Errorf("LeaveLock by the holder = %+v, %v; want her hold at epoch 1, not left", held, err)
	}
	none, err := s.LeaveLock(ctx, carol, "deploy")
	if err != nil || none.Left || none.Held != nil {
		t.Errorf("LeaveLock by carol = %+v, %v; want nothing", none, err)
	}
	if n := lastSeq(t, s); n != seq {
		t.Errorf("leaves that changed nothing recorded %d events", n-seq)
	}
}

// The reaper ends lapsed holds, drops lapsed places and grants the
// next live waiter, telling the holder it lost the lock and the waiter
// it has it.
func TestReapLocks(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	aliceWatch, bobWatch := s.Watch("alice", alice.Session), s.Watch("bob", bob.Session)
	defer aliceWatch.Close()
	defer bobWatch.Close()
	mustAcquire(t, s, alice, LockAcquire{Name: "deploy", Lease: time.Minute})
	mustAcquire(t, s, carol, LockAcquire{Name: "deploy", Wait: true})
	clk.add(90 * time.Second)
	mustAcquire(t, s, bob, LockAcquire{Name: "deploy", Wait: true}) // ends alice's hold and grants carol
	if h := lockOf(t, s, "deploy").Holders; len(h) != 1 || h[0].Holder != carol || h[0].Epoch != 2 {
		t.Fatalf("holders once alice's lapsed = %+v, want carol at epoch 2", h)
	}
	aliceWatch.Take()
	mustAcquire(t, s, alice, LockAcquire{Name: "idle", Lease: time.Minute})
	mustAcquire(t, s, erin, LockAcquire{Name: "idle", Wait: true})
	clk.add(14 * time.Minute) // carol's hold runs to 16m30s; bob's and erin's places lapsed

	ends, err := s.ReapLocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []LockEnd{{Name: "deploy", Holder: bob, Waiting: true}, {Name: "idle", Holder: alice, Epoch: 3}, {Name: "idle", Holder: erin, Waiting: true}}
	if !slices.Equal(ends, want) {
		t.Errorf("ReapLocks = %+v, want %+v", ends, want)
	}
	if ls, _ := s.Locks(ctx, "idle"); len(ls) != 0 {
		t.Errorf("Locks(idle) after the reaper = %+v, want none", ls)
	}
	clk.add(2 * time.Minute)
	if ends, err := s.ReapLocks(ctx); err != nil || !slices.Equal(ends, []LockEnd{{Name: "deploy", Holder: carol, Epoch: 2}}) {
		t.Fatalf("ReapLocks once carol's hold lapsed = %+v, %v; want her hold", ends, err)
	}
	items, _, _ := aliceWatch.Take()
	if len(items) != 1 || items[0].Kind != InboxLockLost || !strings.Contains(items[0].Body, "idle") {
		t.Errorf("alice's items = %+v, want one lock.lost for idle", items)
	}
	if items, _, _ := bobWatch.Take(); len(items) != 0 {
		t.Errorf("bob's items = %+v, want none: his place lapsed before his turn", items)
	}
	if got, want := lockOps(t, s, "idle"), []Op{OpLockAcquire, OpLockWait, OpLockExpire, OpLockLeave}; !slices.Equal(got, want) {
		t.Errorf("events on idle = %v, want %v", got, want)
	}
	if ends, err := s.ReapLocks(ctx); err != nil || len(ends) != 0 {
		t.Errorf("ReapLocks with nothing lapsed = %+v, %v; want nothing", ends, err)
	}
}

// The reaper grants the next live waiter once a hold lapses.
func TestReapLocksGrants(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	mustAcquire(t, s, alice, LockAcquire{Name: "deploy", Lease: time.Minute})
	mustAcquire(t, s, bob, LockAcquire{Name: "deploy", Wait: true})
	clk.add(61 * time.Second)
	if _, err := s.ReapLocks(t.Context()); err != nil {
		t.Fatal(err)
	}
	l := lockOf(t, s, "deploy")
	if len(l.Holders) != 1 || l.Holders[0].Holder != bob || l.Holders[0].Epoch != 2 || len(l.Waiters) != 0 {
		t.Errorf("lock after the reaper = %+v, want bob holding epoch 2", l)
	}
	if got, want := lockOps(t, s, "deploy"), []Op{OpLockAcquire, OpLockWait, OpLockExpire, OpLockGrant}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

// Losing access ends one's holds and places, each with its event, tells
// the holder's session it lost the lock, and grants the next waiter.
func TestEndLocks(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	aliceWatch := s.Watch("alice", alice.Session)
	defer aliceWatch.Close()
	mustAcquire(t, s, alice, LockAcquire{Name: "deploy"})
	mustAcquire(t, s, bob, LockAcquire{Name: "deploy", Wait: true})
	mustAcquire(t, s, bob, LockAcquire{Name: "other"})
	mustAcquire(t, s, alice, LockAcquire{Name: "other", Wait: true})
	ends, err := s.EndLocks(ctx, Actor{Principal: "dana", Session: "admin", Machine: "m"}, func(p string) string {
		if p == "alice" {
			return "membership removed"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ends) != 2 {
		t.Errorf("EndLocks = %+v, want alice's hold of deploy and her place for other", ends)
	}
	if l := lockOf(t, s, "deploy"); len(l.Holders) != 1 || l.Holders[0].Holder != bob {
		t.Errorf("deploy after alice lost access = %+v, want bob holding it", l)
	}
	if l := lockOf(t, s, "other"); len(l.Waiters) != 0 {
		t.Errorf("other's queue after alice lost access = %+v, want empty", l.Waiters)
	}
	items, _, _ := aliceWatch.Take()
	if len(items) != 1 || items[0].Kind != InboxLockLost || !strings.Contains(items[0].Body, "membership removed") {
		t.Errorf("alice's items = %+v, want one lock.lost saying why", items)
	}
	evs, err := s.events(ctx, `WHERE target = ? ORDER BY seq`, LockTarget("deploy"))
	if err != nil {
		t.Fatal(err)
	}
	var after struct{ Reason string }
	if n := len(evs); n < 2 || evs[n-2].Op != OpLockRelease || json.Unmarshal(evs[n-2].After, &after) != nil || after.Reason != "membership removed" ||
		evs[n-1].Op != OpLockGrant {
		t.Errorf("deploy's last events = %+v, want lock.release for membership removed, then lock.grant", evs)
	}
}

// A forward step of the clock is added to every live hold and place in a
// queue, as to a claim's lease, so none lapses for it.
func TestForwardStepExtendsLocks(t *testing.T) {
	t.Parallel()
	c := newTwoClocks()
	s, _ := steppedStore(t, "", c, Options{})
	ctx := t.Context()
	h, err := s.AcquireLock(ctx, alice, LockAcquire{Name: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLock(ctx, bob, LockAcquire{Name: "deploy", Wait: true}); err != nil {
		t.Fatal(err)
	}
	c.run(time.Minute)
	c.step(time.Hour)
	// The reaper's tick settles the step with its first write.
	if err := s.SettleClock(ctx); err != nil {
		t.Fatal(err)
	}
	if ends, err := s.ReapLocks(ctx); err != nil || len(ends) != 0 {
		t.Fatalf("ReapLocks after the jump = %+v, %v; want nothing ended for it", ends, err)
	}
	l := lockOf(t, s, "deploy")
	if len(l.Holders) != 1 || !l.Holders[0].ExpiresAt.Equal(h.Held.ExpiresAt.Add(time.Hour)) || len(l.Waiters) != 1 {
		t.Fatalf("lock after a jump of an hour = %+v, want alice's hold to %s and bob still waiting", l, h.Held.ExpiresAt.Add(time.Hour))
	}
	var cs struct{ Locks int }
	if ev, _, n := lastClockStep(t, s); n != 1 || json.Unmarshal(ev.After, &cs) != nil || cs.Locks != 2 {
		t.Errorf("clock.step: %d events, the last %s; want one extending 2 locks' holds and places", n, ev.After)
	}
}

// Lock events are not issue events: a board's event watch is not pushed
// them.
func TestLockEventsAreNotPushedToBoards(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	we := s.WatchEvents("bob", "sess-x")
	defer we.Close()
	st := mustAcquire(t, s, alice, LockAcquire{Name: "deploy"})
	if err := s.ReleaseLock(t.Context(), alice, "deploy", st.Held.Epoch); err != nil {
		t.Fatal(err)
	}
	select {
	case <-we.Ready():
		_, evs, _ := we.Take()
		t.Errorf("an event watch was pushed %+v", evs)
	default:
	}
}

// The caps on locks (rule 18): names in use in the project, places in one
// lock's queue, and places one principal holds in all queues.
func TestLockLimits(t *testing.T) {
	t.Parallel()
	lim := Limits{Locks: 2, LockWaiters: 2, LockWaits: 2, LockCapacity: 3}
	s := openStore(t, newDSN(t), Options{Limits: lim})
	ctx := t.Context()
	mustAcquire(t, s, alice, LockAcquire{Name: "a"})
	mustAcquire(t, s, alice, LockAcquire{Name: "b"})
	mustAcquire(t, s, bob, LockAcquire{Name: "a", Wait: true})
	mustAcquire(t, s, carol, LockAcquire{Name: "a", Wait: true})
	mustAcquire(t, s, bob, LockAcquire{Name: "b", Wait: true})
	for _, tc := range []struct {
		name  string
		actor Actor
		in    LockAcquire
		limit string
		want  error
	}{
		{"a third name", alice, LockAcquire{Name: "c"}, "locks", ErrBusy},
		{"a third waiter", erin, LockAcquire{Name: "a", Wait: true}, "lock_waiters", ErrBusy},
		{"a capacity past the limit", alice, LockAcquire{Name: "a", Capacity: 4}, "lock_capacity", ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.AcquireLock(ctx, tc.actor, tc.in)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.limit) {
				t.Errorf("AcquireLock(%+v) by %s = %v, want %v naming %s", tc.in, tc.actor.Principal, err, tc.want, tc.limit)
			}
		})
	}
	// bob's two places are the most one principal may have.
	if err := s.ReleaseLock(ctx, alice, "b", 2); err != nil {
		t.Fatal(err)
	}
	mustAcquire(t, s, carol, LockAcquire{Name: "b", Wait: true})
	if _, err := s.AcquireLock(ctx, Actor{Principal: "carol", Session: "two", Machine: "m"}, LockAcquire{Name: "b", Wait: true}); !errors.Is(err, ErrBusy) ||
		!strings.Contains(err.Error(), "lock_waits_per_principal") {
		t.Errorf("a third place for carol = %v, want busy naming lock_waits_per_principal", err)
	}
}

// The count ariesd makes of an archived project, to know whether to
// open it at start and end what is held there, counts holds and places
// in a lock's queue beside claims: losing access ends them too.
func TestHeldClaimsCountsLocks(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	s := openStore(t, dsn, Options{})
	c := newControl(t)
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	held := func() int {
		t.Helper()
		n, err := c.HeldClaims(t.Context(), cfg.DBName)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := held(); n != 0 {
		t.Fatalf("HeldClaims of a new project = %d, want 0", n)
	}
	mustAcquire(t, s, alice, LockAcquire{Name: "deploy"})
	mustAcquire(t, s, bob, LockAcquire{Name: "deploy", Wait: true})
	if n := held(); n != 2 {
		t.Errorf("HeldClaims with a hold and a place = %d, want 2", n)
	}
}

// Locks lists live holds and places only, in grant and queue order, and
// no lock that has neither.
func TestLocksRead(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	mustAcquire(t, s, alice, LockAcquire{Name: "b", Lease: time.Minute})
	mustAcquire(t, s, bob, LockAcquire{Name: "a", Capacity: 2})
	mustAcquire(t, s, carol, LockAcquire{Name: "a"})
	mustAcquire(t, s, erin, LockAcquire{Name: "a", Wait: true})
	clk.add(2 * time.Minute)
	ls, err := s.Locks(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0].Name != "a" || ls[0].Capacity != 2 || !slices.Equal(holderPrincipals(ls[0]), []string{"bob", "carol"}) ||
		len(ls[0].Waiters) != 0 {
		t.Errorf("Locks() = %+v, want only a, held by bob and carol: erin's place and alice's hold of b lapsed", ls)
	}
	if ls, err := s.Locks(t.Context(), "nothing"); err != nil || len(ls) != 0 {
		t.Errorf("Locks(nothing) = %+v, %v; want none", ls, err)
	}
	if _, err := s.Locks(t.Context(), "Bad Name"); !errors.Is(err, ErrInvalid) {
		t.Errorf("Locks(Bad Name) = %v, want invalid", err)
	}
}
