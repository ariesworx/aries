package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/ariesworx/aries/internal/server"
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
// forward is not reaped for it, which is to say that the store's time
// follows the jump only once the store has settled it, and that the
// settle extends every lease it finds live (jumpFaults).

// clockStats counts what the clock chaos did, for the report.
type clockStats struct {
	forward, back, backAtStart         atomic.Int64
	refused, corrected, accepted       atomic.Int64
	leasesChecked, forwardSlow         atomic.Int64
	stepEvents, leasesExtendedInEvents atomic.Int64
	// slowStarts counts the restarts with the clock set back that took so
	// long that the clock was within the tolerance of the history again
	// by the end, so that a daemon which started cannot be faulted.
	slowStarts atomic.Int64
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
// reaped for it (jumpFaults): the claim.expire of any such claim in the
// moments after must name a lease extended by the whole step, unless the
// lease ran out, on the store's time, before the store settled the jump.
// When the clock was behind the stores' time, which then ran slow, a
// store's time moves by less than the step and its leases are owed less,
// by an amount only the store knows: each claim is then held to what the
// clock.step events of its log say they gave it, and no more.
func (s *soak) stepForward(ctx context.Context, rng *rand.Rand) {
	d := time.Duration(5+rng.IntN(26)) * time.Minute
	// The run's reckoning is never behind the stores' time, so when it
	// says the clock is not behind them, it is not.
	whole := s.clock.ahead() == 0
	type before struct {
		seq    int64
		leases map[leased]time.Time
	}
	held := map[*sproj]before{}
	for _, p := range s.projs {
		b := before{leases: map[leased]time.Time{}}
		if err := p.db.QueryRowContext(ctx, `SELECT seq FROM events ORDER BY seq DESC LIMIT 1`).Scan(&b.seq); err != nil {
			continue // no events yet, or the run is ending
		}
		// Live by the clock, a moment before it jumps. How long a lease
		// then has left does not matter: a store may take seconds of
		// its own time to settle the jump, more on a loaded machine,
		// and what the log shows of it is checked, not how soon.
		rows, err := p.db.QueryContext(ctx, `SELECT issue_id, epoch, expires_at FROM claims WHERE principal IS NOT NULL AND expires_at > $1`,
			s.clock.now())
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
	if !whole {
		s.clockStats.forwardSlow.Add(1)
	}
	// Several reaps later, on any machine.
	pause(ctx, 400*time.Millisecond)
	for p, b := range held {
		evs, err := readEvents(context.WithoutCancel(ctx), p.db, b.seq)
		if err != nil {
			s.mon.fail("", "read the claims reaped after the clock jumped: %v", err)
			continue
		}
		// Read after the events were, so no earlier than any of them.
		var unjumped time.Time
		if whole {
			unjumped = s.clock.now().Add(-d)
		}
		for _, v := range jumpFaults(d, b.leases, evs, unjumped) {
			s.mon.fail(v.issue, "%s", v.msg)
		}
		s.clockStats.leasesChecked.Add(int64(len(b.leases)))
	}
}

// jumpFaults returns what a project's log says the store did wrong when
// the clock jumped forward by d. leases are the claims that were live
// before the jump, each with its lease then; events are the log's since;
// and unjumped is the clock less d, read after the events were: a store
// that has not settled the jump has given out no time after it. A zero
// unjumped says that the clock was behind the store's time at the jump,
// so that neither that time nor how much of d a lease is owed is known
// outside the store; only the second rule below is then checked.
//
// It reckons on the store's time, as the store does, and not on how long
// anything took. A store settles the jump with its first write after it,
// and until then its time runs on as if the clock had not moved, so a
// lease may run out between the jump and the settle: its holder had all
// of it, and the reaper ends it as it was. The log must show:
//
//   - no event after unjumped until a clock.step: the store's time
//     follows the clock only once the jump is settled;
//   - for each claim reaped, a lease extended by every clock.step that
//     found it live: one dated before the lease's end as it then was;
//   - and by the whole jump, if every clock.step before the reap found
//     it live and the reap is dated after unjumped.
func jumpFaults(d time.Duration, leases map[leased]time.Time, events []sevent, unjumped time.Time) []violation {
	type settle struct {
		at time.Time // the store's time when it settled
		by time.Duration
	}
	var out []violation
	var settles []settle
	whole := !unjumped.IsZero()
	followed := false // reported once: every later event is after unjumped too
	for _, e := range events {
		if whole && len(settles) == 0 && !followed && e.at.After(unjumped) {
			followed = true
			issue := ""
			if e.op == "claim.expire" {
				issue = e.target
			}
			out = append(out, violation{issue: issue, msg: fmt.Sprintf("event %d (%s on %s) is at %s, and a store that has not settled the clock's jump forward of %s is at %s or earlier: its time followed the clock with no clock.step before it",
				e.seq, e.op, e.target, stamp(e.at), d, stamp(unjumped))})
		}
		switch e.op {
		case "clock.step":
			var a struct {
				Extended int64 `json:"extended_us"`
			}
			_ = json.Unmarshal(e.after, &a)
			settles = append(settles, settle{at: e.at, by: time.Duration(a.Extended) * time.Microsecond})
		case "claim.expire":
			var c claimState
			if json.Unmarshal(e.before, &c) != nil {
				continue
			}
			was, ok := leases[leased{e.target, c.Epoch}]
			if !ok {
				continue
			}
			// A renewal in between only makes the lease later.
			want, always := was, len(settles) > 0
			for _, s := range settles {
				if want.After(s.at) {
					want = want.Add(s.by)
				} else {
					always = false
				}
			}
			if all := was.Add(d - time.Second); whole && always && e.at.After(unjumped) && want.Before(all) {
				want = all
			}
			if c.ExpiresAt.Before(want) {
				out = append(out, violation{issue: e.target, msg: fmt.Sprintf("event %d: the reaper ended %s/%s's claim (epoch %d) with its lease at %s, after the clock jumped forward %s; the lease ran to %s before the jump and was live when the store settled it, so want it extended to %s or later",
					e.seq, c.Holder.Principal, c.Holder.Session, c.Epoch, stamp(c.ExpiresAt), d, stamp(was), stamp(want))})
			}
		}
	}
	return out
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
	began := time.Now()
	s.restartMark()
	w.stopDaemon()
	w.stopDaemon = func() {} // until a new daemon starts, nothing runs to stop
	// Far more than the two minutes of tolerance, and most often than the
	// time on the store's clock that stopping the daemon and opening the
	// databases take, which the clock makes up before the daemon reads it.
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
		// The daemon compares its clock with its history as it opens the
		// databases, and the clock had been gaining on the history since
		// the last event was written: on a machine loaded enough, stopping
		// and starting take longer than the clock was set back by, less the
		// tolerance, and a daemon that starts is right. How far behind the
		// clock was when the daemon looked is not known here, but it has
		// only gained since: if it is past the tolerance now, it was then.
		if st := w.ctl.ClockState(); st.Behind > server.DefaultBehindTolerance {
			return fmt.Errorf("the daemon started with its clock set back %s and still %s behind its own history, %s after it was stopped: want it to refuse (clock_behind: stop)",
				back, st.Behind.Round(time.Second), time.Since(began).Round(time.Millisecond))
		}
		s.clockStats.slowStarts.Add(1)
		return nil
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
		Extended int64     `json:"extended_us"`
		Claims   int       `json:"claims"`
		At       time.Time `json:"at"`
	}
	var b struct {
		At time.Time `json:"at"`
	}
	_ = json.Unmarshal(e.after, &a)
	_ = json.Unmarshal(e.before, &b)
	r.locks.jumps = append(r.locks.jumps, clockJump{seq: e.seq, from: b.At, to: a.At})
	if e.actor != (sessKey{"ariesd", "clock"}) || e.target != "clock" {
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
	if cs.forward.Load() > cs.forwardSlow.Load() && cs.leasesChecked.Load() > 0 && cs.leasesExtendedInEvents.Load() == 0 {
		s.mon.fail("", "%d claims had lease left when the clock jumped forward, and no clock.step event of a project extended any", cs.leasesChecked.Load())
	}
}
