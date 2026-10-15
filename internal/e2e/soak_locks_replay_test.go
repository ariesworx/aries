package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// lockOf is a lock event's holder state: who, at epoch, until expires.
func (b *logBuilder) lockOf(who sessKey, epoch int64, expires time.Duration, capacity int) string {
	return fmt.Sprintf(`{"holder": {"principal": %q, "session": %q}, "epoch": %d, "expires_at": %s, "capacity": %d}`,
		who.principal, who.session, epoch, b.ts(expires), capacity)
}

// How late a lapsed hold was ended leaves out a jump of the clock
// settled after its lease ran out, and only such a jump.
func TestLockLateness(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	lr := &lockReplay{jumps: []clockJump{
		{seq: 5, from: at(time.Minute), to: at(21 * time.Minute)},       // before the lease ran out: extended it
		{seq: 9, from: at(31 * time.Minute), to: at(56 * time.Minute)},  // after: the hold had all its time
		{seq: 20, from: at(70 * time.Minute), to: at(90 * time.Minute)}, // after the end
	}}
	lt := lockLate{expires: at(30 * time.Minute), at: at(56*time.Minute + 10*time.Second), seq: 12}
	if got, want := lr.lateness(lt), 70*time.Second; got != want {
		t.Errorf("lateness(%+v) = %s, want %s: 26m10s less the 25m jump after the lease", lt, got, want)
	}
}

// The lock replay finds each way a lock's log can go wrong, and nothing
// in a clean one.
func TestLockReplayCatches(t *testing.T) {
	t.Parallel()
	alice, bob, carol := sessKey{"alice", "s1"}, sessKey{"bob", "s1"}, sessKey{"carol", "s1"}
	const mutex, pool = "lock:train/main", "lock:pool"
	tests := []struct {
		name string
		log  func(b *logBuilder) *logBuilder
		want string // a substring of the first violation; empty: none
	}{
		{
			name: "a clean story",
			log: func(b *logBuilder) *logBuilder {
				b.add(time.Minute, alice, "lock.acquire", mutex, "", b.lockOf(alice, 1, 2*time.Minute, 1))
				b.add(time.Minute, bob, "lock.wait", mutex, "", `{"position": 1, "capacity": 1}`)
				b.add(time.Minute, alice, "lock.release", mutex, b.lockOf(alice, 1, 2*time.Minute, 0), "")
				b.add(0, alice, "lock.grant", mutex, b.lockOf(bob, 0, 0, 0), b.lockOf(bob, 2, 4*time.Minute, 1))
				b.add(2*time.Minute, carol, "lock.expire", mutex, b.lockOf(bob, 2, 4*time.Minute, 0), "")
				return b.add(0, carol, "lock.acquire", mutex, "", b.lockOf(carol, 3, 7*time.Minute, 1))
			},
		},
		{
			name: "a semaphore granted past its capacity",
			log: func(b *logBuilder) *logBuilder {
				b.add(time.Minute, alice, "lock.acquire", pool, "", b.lockOf(alice, 1, time.Hour, 2))
				b.add(time.Minute, bob, "lock.acquire", pool, "", b.lockOf(bob, 2, time.Hour, 2))
				return b.add(time.Minute, carol, "lock.acquire", pool, "", b.lockOf(carol, 3, time.Hour, 2))
			},
			want: "with 2 holders already, at capacity 2",
		},
		{
			name: "a grant out of the queue's order",
			log: func(b *logBuilder) *logBuilder {
				b.add(time.Minute, alice, "lock.acquire", mutex, "", b.lockOf(alice, 1, time.Hour, 1))
				b.add(time.Minute, bob, "lock.wait", mutex, "", `{"capacity": 1}`)
				b.add(time.Minute, carol, "lock.wait", mutex, "", `{"capacity": 1}`)
				b.add(time.Minute, alice, "lock.release", mutex, b.lockOf(alice, 1, time.Hour, 0), "")
				return b.add(0, alice, "lock.grant", mutex, b.lockOf(carol, 0, 0, 0), b.lockOf(carol, 2, time.Hour, 1))
			},
			want: "grants train/main to carol/s1, but the first live waiter is [bob/s1 carol/s1]",
		},
		{
			name: "an epoch that does not rise",
			log: func(b *logBuilder) *logBuilder {
				b.add(time.Minute, alice, "lock.acquire", mutex, "", b.lockOf(alice, 1, time.Hour, 1))
				b.add(time.Minute, alice, "lock.release", mutex, b.lockOf(alice, 1, time.Hour, 0), "")
				return b.add(time.Minute, bob, "lock.acquire", pool, "", b.lockOf(bob, 1, time.Hour, 3))
			},
			want: "epoch 1 after epoch 1",
		},
		{
			name: "a hold ended before its lease",
			log: func(b *logBuilder) *logBuilder {
				b.add(time.Minute, alice, "lock.acquire", mutex, "", b.lockOf(alice, 1, time.Hour, 1))
				return b.add(time.Minute, reaper, "lock.expire", mutex, b.lockOf(alice, 1, time.Hour, 0), "")
			},
			want: "before its lease ran out",
		},
		{
			name: "a free slot while someone waits",
			log: func(b *logBuilder) *logBuilder {
				b.add(time.Minute, alice, "lock.acquire", mutex, "", b.lockOf(alice, 1, 2*time.Minute, 1))
				b.add(time.Minute, bob, "lock.wait", mutex, "", `{"capacity": 1}`)
				return b.add(time.Minute, reaper, "lock.expire", mutex, b.lockOf(alice, 1, 2*time.Minute, 0), "")
			},
			want: "with 0 of 1 holders while [bob/s1] wait",
		},
		{
			name: "a release that leaves the queue waiting, and an acquire past it",
			log: func(b *logBuilder) *logBuilder {
				b.add(time.Minute, alice, "lock.acquire", pool, "", b.lockOf(alice, 1, time.Hour, 1))
				b.add(time.Minute, bob, "lock.wait", pool, "", `{"capacity": 1}`)
				b.add(time.Minute, alice, "lock.release", pool, b.lockOf(alice, 1, time.Hour, 0), "")
				return b.add(0, carol, "lock.acquire", pool, "", b.lockOf(carol, 2, time.Hour, 1))
			},
			want: "leave pool with 0 of 1 holders while [bob/s1] wait",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			r := newReplay(func(_, format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }, []string{"dave"})
			r.feed(tc.log(newLog()).evs)
			r.flush()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("replay found %q, want no violation", got)
			case tc.want != "" && (len(got) == 0 || !strings.Contains(got[0], tc.want)):
				t.Errorf("replay found %q, want first a violation containing %q", got, tc.want)
			}
		})
	}
}
