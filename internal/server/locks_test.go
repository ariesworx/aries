package server

import (
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// The lock ops round trip: an acquire granted at once, a place in the
// queue, the list, a renewal of the place, a release that grants the
// queue, the waiter learning of it by renewing, a leave by a holder, and
// a release.
func TestLockOps(t *testing.T) {
	t.Parallel()
	ck := newClock()
	s, _ := newServerClock(t, Limits{}, ck.now)
	got := mustCall[proto.LockResult](t, s, alice, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy/prod", Lease: "30m"})
	if !got.Held || got.Epoch != 1 || got.Capacity != 1 || !got.ExpiresAt.Equal(ck.now().Add(30*time.Minute).UTC().Truncate(time.Microsecond)) {
		t.Fatalf("lock.acquire = %+v, want epoch 1 for 30m", got)
	}
	place := mustCall[proto.LockResult](t, s, bob, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy/prod", Wait: true})
	if place.Held || place.Position != 1 || place.WaitExpires.IsZero() {
		t.Fatalf("lock.acquire --wait = %+v, want position 1", place)
	}
	ls := mustCall[proto.LocksResult](t, s, carol, proto.OpLocks, proto.LocksArgs{})
	if len(ls.Locks) != 1 || len(ls.Locks[0].Holders) != 1 || ls.Locks[0].Holders[0].By != "alice" || ls.Locks[0].Holders[0].Epoch != 1 ||
		len(ls.Locks[0].Waiters) != 1 || ls.Locks[0].Waiters[0].By != "bob" || ls.Locks[0].Waiters[0].Position != 1 || ls.Now.IsZero() {
		t.Errorf("locks = %+v, want deploy/prod held by alice at epoch 1, bob waiting first", ls)
	}
	if one := mustCall[proto.LocksResult](t, s, carol, proto.OpLocks, proto.LocksArgs{Name: "other"}); len(one.Locks) != 0 {
		t.Errorf("locks other = %+v, want none", one)
	}
	ck.advance(time.Minute)
	if r := mustCall[proto.LockResult](t, s, bob, proto.OpLockRenew, proto.LockRenewArgs{Name: "deploy/prod"}); r.Held || r.Position != 1 {
		t.Errorf("lock.renew of bob's place = %+v, want position 1", r)
	}
	if r := mustCall[proto.LockReleaseResult](t, s, alice, proto.OpLockRelease, proto.LockReleaseArgs{Name: "deploy/prod", Epoch: 1}); r.Epoch != 1 {
		t.Errorf("lock.release = %+v, want epoch 1 released", r)
	}
	r := mustCall[proto.LockResult](t, s, bob, proto.OpLockRenew, proto.LockRenewArgs{Name: "deploy/prod"})
	if !r.Held || r.Epoch != 2 {
		t.Errorf("lock.renew of bob's place once alice released = %+v, want his hold at epoch 2", r)
	}
	if l := mustCall[proto.LockLeaveResult](t, s, bob, proto.OpLockLeave, proto.LockLeaveArgs{Name: "deploy/prod"}); l.Left || l.Epoch != 2 {
		t.Errorf("lock.leave by the holder = %+v, want not left, epoch 2", l)
	}
	mustCall[proto.LockReleaseResult](t, s, bob, proto.OpLockRelease, proto.LockReleaseArgs{Name: "deploy/prod", Epoch: 2})
	in := mustCall[proto.InboxResult](t, s, bob, proto.OpInbox, proto.InboxArgs{})
	if len(in.Items) != 1 || in.Items[0].Kind != "lock.granted" || !strings.Contains(in.Items[0].Body, "deploy/prod") {
		t.Errorf("bob's inbox = %+v, want one lock.granted", in.Items)
	}
}

// Every refusal of a lock op says what to do next (rule 11).
func TestLockRefusals(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{Limits: store.Limits{Locks: 2, LockWaiters: 1, LockWaits: 1}})
	mustCall[proto.LockResult](t, s, alice, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy"})
	mustCall[proto.LockResult](t, s, bob, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy", Wait: true})
	mustCall[proto.LockResult](t, s, alice, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "api", Capacity: 2})
	for _, tc := range []struct {
		name     string
		by       store.Actor
		op       string
		args     any
		code     proto.Code
		msg, fix string
	}{
		{"held", carol, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy"}, proto.CodeBusy, "held by alice", "--wait"},
		{"a bad name", alice, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "Deploy Prod"}, proto.CodeInvalid, "lock name", "lowercase"},
		{"a bad lease", alice, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "x", Lease: "soon"}, proto.CodeInvalid, "lease", "1m to 24h"},
		{"another capacity", carol, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "api", Capacity: 3}, proto.CodeInvalid, "capacity 2", "--capacity"},
		{"a capacity past the limit", carol, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "x", Capacity: 65}, proto.CodeInvalid, "lock_capacity", "lock_capacity"},
		{"a third lock", carol, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "third"}, proto.CodeBusy, "locks", "aries lock list"},
		{"a full queue", carol, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy", Wait: true}, proto.CodeBusy, "lock_waiters", "lock_waiters"},
		{"a stale epoch", alice, proto.OpLockRelease, proto.LockReleaseArgs{Name: "deploy", Epoch: 9}, proto.CodeConflict, "epoch 9", "ARIES_LOCK_EPOCH"},
		{"someone else's epoch", bob, proto.OpLockRenew, proto.LockRenewArgs{Name: "deploy", Epoch: 1}, proto.CodeConflict, "held by alice", "stop"},
		{"no place", carol, proto.OpLockRenew, proto.LockRenewArgs{Name: "deploy"}, proto.CodeConflict, "no place", "acquire"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := call[proto.LockResult](t, s, tc.by, tc.op, tc.args)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Errorf("%s %+v = %+v; want %s naming %q, with a fix holding %q", tc.op, tc.args, perr, tc.code, tc.msg, tc.fix)
			}
		})
	}
	// bob's place in deploy's queue is the most a principal may have,
	// from any of its sessions.
	mustCall[proto.LockResult](t, s, carol, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "api"})
	_, perr := call[proto.LockResult](t, s, store.Actor{Principal: "bob", Session: "other", Machine: "m"}, proto.OpLockAcquire,
		proto.LockAcquireArgs{Name: "api", Wait: true})
	if perr == nil || perr.Code != proto.CodeBusy || !strings.Contains(perr.Message+perr.Fix, "lock_waits_per_principal") {
		t.Errorf("a second place for bob = %+v, want busy naming lock_waits_per_principal", perr)
	}
}

// The reaper's tick ends lapsed holds and places and grants the queue,
// logging each end without the lock's name leaving the journal.
func TestReapEndsLocks(t *testing.T) {
	t.Parallel()
	ck := newClock()
	m := newMulti(t, Limits{}, ck)
	m.project(t, projA, "alice", "bob")
	a, b := mustDialTo(t, m.Server, alice, projA), mustDialTo(t, m.Server, bob, projA)
	mustAsk[proto.LockResult](a, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy", Lease: "1m"})
	mustAsk[proto.LockResult](b, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy", Wait: true})
	ck.advance(61 * time.Second)
	m.Reap(t.Context())
	ls := mustAsk[proto.LocksResult](a, proto.OpLocks, proto.LocksArgs{})
	if len(ls.Locks) != 1 || len(ls.Locks[0].Holders) != 1 || ls.Locks[0].Holders[0].By != "bob" || ls.Locks[0].Holders[0].Epoch != 2 {
		t.Errorf("locks after the reaper = %+v, want bob holding epoch 2", ls.Locks)
	}
	if !strings.Contains(m.log.String(), `event=lock.expired lock=deploy principal=alice session=s-a epoch=1 waiting=false`) {
		t.Errorf("the log lacks the end of alice's hold:\n%s", m.log.String())
	}
}

// Losing access ends one's holds and places (decision 21): in the
// stores open at the change, as the admin's, with the holder told, and
// the slot granted to the next waiter; and in an archived project at the
// daemon's start.
func TestAccessEndsLocks(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projA)
	a, b, c := mustDialTo(t, m.Server, alice, projA), mustDialTo(t, m.Server, bob, projA), mustDialTo(t, m.Server, carol, projA)
	mustAsk[proto.LockResult](b, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy"})
	mustAsk[proto.LockResult](a, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy", Wait: true})
	mustAsk[proto.LockResult](c, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "api"})
	mustAsk[proto.LockResult](b, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "api", Wait: true})

	mustAsk[proto.MemberRemoveResult](d, proto.OpMemberRemove, proto.MemberArgs{Name: "bob"})
	ls := mustAsk[proto.LocksResult](d, proto.OpLocks, proto.LocksArgs{})
	if len(ls.Locks) != 2 || ls.Locks[0].Name != "api" || len(ls.Locks[0].Waiters) != 0 ||
		ls.Locks[1].Name != "deploy" || len(ls.Locks[1].Holders) != 1 || ls.Locks[1].Holders[0].By != "alice" {
		t.Errorf("locks after bob's removal = %+v, want api with no queue and deploy held by alice", ls.Locks)
	}
	if !strings.Contains(m.log.String(), `event=lock.released lock=deploy principal=bob session=s-b epoch=1 waiting=false by=dana reason="membership removed"`) {
		t.Errorf("the log lacks the end of bob's hold:\n%s", m.log.String())
	}
	mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "bob"})
	in := mustAsk[proto.InboxResult](mustDialTo(t, m.Server, bob, projA), proto.OpInbox, proto.InboxArgs{})
	if len(in.Items) != 1 || in.Items[0].Kind != "lock.lost" || !strings.Contains(in.Items[0].Body, "membership removed by dana") {
		t.Errorf("bob's inbox = %+v, want one lock.lost saying why", in.Items)
	}

	// An archive the daemon did not finish: the next start ends what is
	// held in the archived project, which held no claim.
	e := mustDialTo(t, m.Server, erin, projB)
	mustAsk[proto.LockResult](e, proto.OpLockAcquire, proto.LockAcquireArgs{Name: "deploy"})
	e.hangUp()
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projB); err != nil {
		t.Fatal(err)
	}
	m.stores.closeAll()
	if err := m.ReleaseArchived(t.Context(), m.asIs()); err != nil {
		t.Fatal(err)
	}
	st := m.inStore(t, "proj-b")
	if got, err := st.Locks(t.Context(), ""); err != nil || len(got) != 0 {
		t.Errorf("locks of the archived project after the start = %+v, %v; want none", got, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}
