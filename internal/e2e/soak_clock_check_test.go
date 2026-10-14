package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestJumpFaults checks the soak's check of the claims reaped after the
// clock jumped forward, against logs as a store writes them. The first
// case is the order a loaded machine gave a run (seed
// 264949260793903814, a jump of 22 minutes): the store settled the jump
// 34 seconds of its own time after the clock jumped, by when a lease
// with 33 seconds left had run out.
func TestJumpFaults(t *testing.T) {
	t.Parallel()
	const d = 22 * time.Minute
	at := func(clock string) time.Time {
		t.Helper()
		v, err := time.Parse("15:04:05.000000", clock)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	settle := func(seq int64, when time.Time, extended time.Duration) sevent {
		return sevent{seq: seq, at: when, actor: sessKey{"starfixd", "clock"}, op: "clock.step", target: "clock",
			before: fmt.Appendf(nil, `{"at": %q}`, when.Format(time.RFC3339Nano)),
			after: fmt.Appendf(nil, `{"at": %q, "claims": 3, "step_us": %d, "extended_us": %d}`,
				when.Add(extended).Format(time.RFC3339Nano), extended.Microseconds(), extended.Microseconds())}
	}
	expire := func(seq int64, when time.Time, issue string, epoch int64, lease time.Time) sevent {
		return sevent{seq: seq, at: when, actor: reaper, op: "claim.expire", target: issue,
			before: fmt.Appendf(nil, `{"epoch": %d, "holder": {"principal": "erin", "session": "lib-erin-4g1"}, "expires_at": %q}`,
				epoch, lease.Format(time.RFC3339Nano))}
	}
	var (
		jumped   = at("22:48:46.646815") // the clock then reads 23:10:46
		lease    = at("22:49:20.229800") // 33 seconds left at the jump
		settled  = at("22:49:21.162300") // the store's time when it settled the jump
		reaped   = at("23:11:32.194320") // the first reap after
		unjumped = at("23:11:34.000000").Add(-d)
		live     = settled.Add(time.Second) // a lease the settle found live
	)
	if !lease.After(jumped) || !settled.After(lease) || !unjumped.After(settled) || !reaped.After(unjumped) {
		t.Fatal("the cases need a lease live at the jump that ran out before the store settled it")
	}
	tests := []struct {
		name   string
		lease  time.Time // of sb-1 at epoch 2, before the jump
		events []sevent
		slow   bool     // the clock was behind the store's time at the jump
		want   []string // a part of each fault's message, in order
	}{
		{name: "a lease that ran out before the store settled the jump is reaped as it was", lease: lease,
			events: []sevent{settle(3038, settled, d), expire(3049, reaped, "sb-1", 2, lease)}},
		{name: "a lease that ran out before any settle is reaped on the store's own time", lease: lease,
			events: []sevent{expire(3037, settled, "sb-1", 2, lease), settle(3038, settled, d)}},
		{name: "a lease live at the settle and extended by the jump runs out later", lease: live,
			events: []sevent{settle(3038, settled, d), expire(3049, reaped, "sb-1", 2, live.Add(d))}},
		{name: "a lease live at the settle and not extended", lease: live,
			events: []sevent{settle(3038, settled, d), expire(3049, reaped, "sb-1", 2, live)},
			want:   []string{"event 3049: the reaper ended erin/lib-erin-4g1's claim (epoch 2) with its lease at 22:49:22.162300"}},
		{name: "a lease extended by less than the jump", lease: live,
			events: []sevent{settle(3038, settled, d-time.Minute), expire(3049, reaped, "sb-1", 2, live.Add(d-time.Minute))},
			want:   []string{"event 3049: the reaper ended"}},
		{name: "a lease live at two settles is owed both", lease: live,
			events: []sevent{settle(3030, settled.Add(-time.Second), 5*time.Minute), settle(3038, settled, d), expire(3049, reaped, "sb-1", 2, live.Add(d))},
			want:   []string{"event 3049: the reaper ended"}},
		{name: "a reap on the jumped time with the jump not settled", lease: lease,
			events: []sevent{expire(3037, reaped, "sb-1", 2, lease)},
			want:   []string{"event 3037 (claim.expire on sb-1) is at 23:11:32.194320"}},
		{name: "a settle on the jumped time", lease: lease,
			events: []sevent{settle(3038, reaped, d), expire(3049, reaped.Add(time.Second), "sb-1", 2, lease)},
			want:   []string{"event 3038 (clock.step on clock) is at 23:11:32.194320"}},
		// A store whose time ran slow moves by less than the jump, and says
		// by how much in its clock.step.
		{name: "a store that ran slow extends a live lease by what its settle says", lease: live, slow: true,
			events: []sevent{settle(3038, settled, d-5*time.Minute), expire(3049, reaped, "sb-1", 2, live.Add(d-5*time.Minute))}},
		{name: "a store that ran slow and did not extend a live lease by what its settle says", lease: live, slow: true,
			events: []sevent{settle(3038, settled, d-5*time.Minute), expire(3049, reaped, "sb-1", 2, live.Add(time.Minute))},
			want:   []string{"event 3049: the reaper ended"}},
		{name: "a store that ran slow is not held to a time the run cannot know", lease: lease, slow: true,
			events: []sevent{expire(3037, reaped, "sb-1", 2, lease), settle(3038, reaped, 0)}},
		{name: "another claim's lease, and a later epoch of the same issue", lease: live,
			events: []sevent{settle(3038, settled, d), expire(3049, reaped, "sb-2", 2, live), expire(3051, reaped, "sb-1", 3, live)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unjumped := unjumped
			if tc.slow {
				unjumped = time.Time{}
			}
			got := jumpFaults(d, map[leased]time.Time{{"sb-1", 2}: tc.lease}, tc.events, unjumped)
			ok := len(got) == len(tc.want)
			for i := 0; ok && i < len(got); i++ {
				ok = strings.Contains(got[i].msg, tc.want[i])
			}
			if !ok {
				t.Errorf("jumpFaults(%s, sb-1's lease to %s, events, %s) = %q, want %d faults holding %q",
					d, stamp(tc.lease), stamp(unjumped), got, len(tc.want), tc.want)
			}
		})
	}
}
