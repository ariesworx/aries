package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/ariesworx/starfix/internal/server"
)

// The soak's clock chaos (design §14, decision 37). While the sessions
// work, the machine's wall clock is set forward by more than any lease,
// as a suspended machine's is when it resumes, and set back, as a wrong
// correction does; and the daemon is restarted with its clock set back
// past the tolerance, which it must refuse until the clock is corrected
// or the history accepted for that start. Through all of it the
// invariants the soak already checks must hold, none loosened: no epoch
// to two sessions, no write by a stale holder, a claim.expire only once
// its lease ran out, an event of its own for every claim that ends, and
// event time that never decreases in a log, which only now is put to the
// test. One is added: a claim with lease left when the clock jumps
// forward is not reaped for it.

// clockStats counts what the clock chaos did, for the report.
type clockStats struct {
	forward, back, backAtStart         atomic.Int64
	refused, corrected, accepted       atomic.Int64
	leasesChecked, forwardNotChecked   atomic.Int64
	stepEvents, leasesExtendedInEvents atomic.Int64
}

// step sets the wall clock by d, and keeps lead, how far a store's time
// may be ahead of it: a store that was in step keeps its time when the
// clock goes back, and runs on at half speed until the clock has caught
// up; a jump forward closes that gap first.
func (c *vclock) step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.elapsed()
	cur := c.leadAt(m)
	c.offset += d
	c.lead, c.leadFrom = max(0, cur-d), m
}

// leadAt is lead at monotonic time m. c.mu is held.
func (c *vclock) leadAt(m time.Duration) time.Duration {
	return max(0, c.lead-(m-c.leadFrom)/2)
}

// ahead returns how far a store's time may now be ahead of the wall
// clock.
func (c *vclock) ahead() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leadAt(c.elapsed())
}

// stepClock sets the clock forward or back, as the seed chooses.
func (s *soak) stepClock(ctx context.Context, rng *rand.Rand) {
	if rng.IntN(3) > 0 {
		s.stepForward(ctx, rng)
		return
	}
	// Small steps back, each well past the step tolerance; the large ones
	// are made at a restart, where the daemon must refuse to start.
	if s.clock.ahead() < 30*time.Second {
		s.clock.step(-time.Duration(10+rng.IntN(35)) * time.Second)
		s.clockStats.back.Add(1)
	}
}

// leased is a claim with lease left.
type leased struct {
	issue string
	epoch int64
}

// stepForward sets the clock forward by more than most leases have left
// (5 to 30 minutes, when the client library's leases are 1 to 3), then
// checks, in each project, that no claim with lease left at the jump was
// reaped for it: the claim.expire of any such claim in the moments after
// must name a lease extended by the whole step.
func (s *soak) stepForward(ctx context.Context, rng *rand.Rand) {
	d := time.Duration(5+rng.IntN(26)) * time.Minute
	const margin = 30 * time.Second // on the store's clock: live when the clock jumps, which is at once
	check := s.clock.ahead() == 0   // a store running slow gives less than the whole step
	type before struct {
		seq    int64
		leases map[leased]time.Time
	}
	held := map[*sproj]before{}
	if check {
		for _, p := range s.projs {
			b := before{leases: map[leased]time.Time{}}
			if err := p.db.QueryRowContext(ctx, `SELECT seq FROM events ORDER BY seq DESC LIMIT 1`).Scan(&b.seq); err != nil {
				continue // no events yet, or the run is ending
			}
			rows, err := p.db.QueryContext(ctx, `SELECT issue_id, epoch, expires_at FROM claims WHERE principal IS NOT NULL AND expires_at > ?`,
				s.clock.now().Add(margin))
			if err != nil {
				continue
			}
			for rows.Next() {
				var l leased
				var exp time.Time
				if err := rows.Scan(&l.issue, &l.epoch, &exp); err == nil {
					b.leases[l] = exp.UTC()
				}
			}
			_ = rows.Close()
			held[p] = b
		}
	}

	s.clock.step(d)
	s.clockStats.forward.Add(1)
	// The control store has no reaper to write to it; the daemon settles
	// it on its next tick. Settled at once, its time and the run's agree
	// again before the next access change.
	s.w.daemonMu.Lock()
	ctl := s.w.ctl
	s.w.daemonMu.Unlock()
	if err := ctl.SettleClock(ctx); err != nil && ctx.Err() == nil {
		s.mon.fail("", "settle the control store after the clock jumped %s: %v", d, err)
	}
	if !check {
		s.clockStats.forwardNotChecked.Add(1)
		return
	}
	// Several reaps later, on any machine.
	pause(ctx, 400*time.Millisecond)
	for p, b := range held {
		rows, err := p.db.QueryContext(context.WithoutCancel(ctx), `SELECT seq, target, before_state FROM events WHERE seq > ? AND op = 'claim.expire' ORDER BY seq`, b.seq)
		if err != nil {
			s.mon.fail("", "read the claims reaped after the clock jumped: %v", err)
			continue
		}
		for rows.Next() {
			var seq int64
			var target string
			var state []byte
			var c claimState
			if err := rows.Scan(&seq, &target, &state); err != nil || json.Unmarshal(state, &c) != nil {
				continue
			}
			was, ok := b.leases[leased{target, c.Epoch}]
			if ok && c.ExpiresAt.Before(was.Add(d-time.Second)) {
				s.mon.fail(target, "event %d: the reaper ended %s/%s's claim (epoch %d) with its lease at %s, after the clock jumped forward %s; the lease ran to %s before the jump, so want it extended to %s or later",
					seq, c.Holder.Principal, c.Holder.Session, c.Epoch, stamp(c.ExpiresAt), d, stamp(was), stamp(was.Add(d)))
			}
		}
		_ = rows.Close()
		s.clockStats.leasesChecked.Add(int64(len(b.leases)))
	}
}

// restartDaemon stops the daemon and starts another, as an operator's
// restart does. When the stores are in step with the clock, the clock is
// first set back by more than the tolerance while nothing runs, as on a
// machine restored from a snapshot or booted with a dead clock battery:
// the daemon must then refuse to start (clock_behind: stop, the
// default). Whenever a start is refused for the clock, the seed chooses
// the way on: the clock is corrected, or the history is accepted for one
// start (--accept-clock-behind).
func (s *soak) restartDaemon(rng *rand.Rand) error {
	w := s.w
	w.daemonMu.Lock()
	defer w.daemonMu.Unlock()
	w.stopDaemon()
	w.stopDaemon = func() {} // until a new daemon starts, nothing runs to stop
	// Far more than the two minutes of tolerance, and than the time on the
	// store's clock that opening the databases takes on a loaded machine.
	var back time.Duration
	if s.clock.ahead() < 30*time.Second {
		back = time.Duration(10+rng.IntN(6)) * time.Minute
		s.clock.step(-back)
		s.clockStats.backAtStart.Add(1)
	}
	err := w.startDaemon()
	behind, refused := errors.AsType[*server.ClockBehindError](err)
	switch {
	case err == nil && back > 0:
		return fmt.Errorf("the daemon started with its clock set back %s, behind its own history: want it to refuse (clock_behind: stop)", back)
	case !refused:
		return err
	}
	s.clockStats.refused.Add(1)
	if rng.IntN(2) == 0 {
		s.clock.step(behind.History.Sub(behind.Clock) + time.Second)
		s.clockStats.corrected.Add(1)
	} else {
		w.acceptHistory = behind.History.Add(2 * time.Second)
		defer func() { w.acceptHistory = time.Time{} }()
		s.clockStats.accepted.Add(1)
	}
	if err := w.startDaemon(); err != nil {
		return fmt.Errorf("start the daemon after its refusal for the clock (%v): %w", behind, err)
	}
	return nil
}

// clockStep applies a clock.step event of a project's log: the daemon
// settling a forward step. It is the server's own, on no issue.
func (r *replay) clockStep(e sevent) {
	var a struct {
		Extended int64 `json:"extended_us"`
		Claims   int   `json:"claims"`
	}
	_ = json.Unmarshal(e.after, &a)
	if e.actor != (sessKey{"starfixd", "clock"}) || e.target != "clock" {
		r.fail(e.target, "event %d: clock.step by %s on %q, not by the server on its clock", e.seq, e.actor, e.target)
	}
	if a.Extended < 0 || (a.Extended == 0 && a.Claims != 0) {
		r.fail(e.target, "event %d: clock.step extends %d leases by %dµs", e.seq, a.Claims, a.Extended)
	}
	r.stepped += a.Claims
}

// checkClock checks what the logs say of the clock chaos against what
// was done, once the run is over.
func (s *soak) checkClock() {
	cs := &s.clockStats
	for _, p := range s.projs {
		p.rep.mu.Lock()
		cs.stepEvents.Add(int64(p.rep.ops["clock.step"]))
		cs.leasesExtendedInEvents.Add(int64(p.rep.stepped))
		p.rep.mu.Unlock()
	}
	s.ctl.mu.Lock()
	defer s.ctl.mu.Unlock()
	// The control store is always open, and each jump was settled in it
	// at once: one event a jump.
	if got, want := int64(s.ctl.ops["clock.step"]), cs.forward.Load(); got != want {
		s.mon.fail("", "the control log has %d clock.step events after %d jumps forward of the clock", got, want)
	}
	if cs.forward.Load() > cs.forwardNotChecked.Load() && cs.leasesChecked.Load() > 0 && cs.leasesExtendedInEvents.Load() == 0 {
		s.mon.fail("", "%d claims had lease left when the clock jumped forward, and no clock.step event of a project extended any", cs.leasesChecked.Load())
	}
}
