package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/logs"
)

// The server's clock is relied on and not trusted to be right (design
// §14, decision 37). Three things follow from one rule each:
//
//   - Time in a log never goes backward. A store's time is the later of
//     the clock and the store's high-water mark, which starts as the time
//     of the database's latest event and then follows every time the store
//     gives out; a process's stores share the highest of theirs, so the
//     logs of one server keep one time. While the clock is behind the
//     mark the store's time goes on from the mark at half speed, until
//     the clock has caught up with it. So a clock set back cannot put an event before its
//     predecessor or reopen a window that is counted from now, and
//     lengthens a lease by at most how far it was set back.
//   - A step is detected. A [Clock] compares how far the wall clock moved
//     between two readings with how far the monotonic clock did. A
//     difference beyond its tolerance is a step: someone or something set
//     the clock, or the machine was suspended.
//   - A jump forward does not by itself end claims. A lease promises a
//     duration, and a clock set forward takes that duration away from
//     every claim at once. Until a store has settled a forward step its
//     time stays where the monotonic clock puts it; settling, which the
//     next write does first, adds the step to every lease that was live
//     and records one clock.step event, and only then does the store's
//     time follow the clock.
//   - No write that begins after a jump is dated before it. A write looks
//     for a step once more when it holds the writer, and settles it
//     before it goes on (writeOnce), so in each log the events before a
//     clock.step are those whose transactions had begun when the clock
//     jumped, and an event one store records because of another's is
//     never dated before it.
//
// Fencing does not rest on any of this: an epoch rises with each new
// holder whatever the clock says, and a stale epoch is refused.

// DefaultStepTolerance is how far elapsed wall-clock time and elapsed
// monotonic time may differ, between two readings or summed over
// several (driftLeak), before a [Clock] calls it a step. A healthy time service slews a running clock and does not
// step it, and the two readings of one time.Now differ by microseconds
// on a loaded machine, so seconds of difference is never drift; a leap
// second, and the sub-second steps systemd-timesyncd makes, stay under
// it.
const DefaultStepTolerance = 5 * time.Second

// OpClockStep is the event a store records when it settles a forward
// step of the clock: its before state is the store's time before, its
// after state the time after, how far each live lease was extended and
// how many were. Its target is [ClockTarget].
const OpClockStep Op = "clock.step"

// ClockTarget is the target of clock.step events, which no issue id can
// be.
const ClockTarget = "clock"

// ClockActor is who a settled clock step is recorded as.
var ClockActor = Actor{Principal: "ariesd", Session: "clock", Machine: "server"}

// ClockOptions configure a [Clock]. The zero value reads time.Now.
type ClockOptions struct {
	// Now is the wall clock. Default time.Now. Elapsed monotonic time is
	// taken from its readings, so a clock that returns times without one
	// (a test's fixed time) never shows a step.
	Now func() time.Time
	// Read, when set, replaces Now: it returns the wall clock and the
	// monotonic time elapsed since any fixed moment, from one reading.
	// Tests, and clocks that run faster than real time, set it.
	Read func() (wall time.Time, elapsed time.Duration)
	// StepTolerance is the difference between elapsed wall-clock and
	// monotonic time that counts as a step. Default
	// [DefaultStepTolerance].
	StepTolerance time.Duration
}

// Clock is a process's clock, shared by every store the process opens
// ([Options.Clock]), so that each store learns of a step whichever
// noticed it, and one opened later learns of the steps it has not
// settled. It is safe for concurrent use.
type Clock struct {
	read func() (time.Time, time.Duration)
	tol  time.Duration

	mu sync.Mutex
	// started is set by the first reading; lastWall and lastMono are the
	// latest one.
	started  bool
	lastWall time.Time
	lastMono time.Duration
	// drift is how much further the wall clock has moved than the
	// monotonic clock, over the readings since the last step, less what
	// driftLeak has forgiven. Past the tolerance either way it is a step.
	drift time.Duration
	// forward is the sum of the forward steps seen.
	forward time.Duration
	steps   ClockSteps
	// settled is, for each database a store of the process has had open
	// (Store.db), how much of forward that store had given its leases
	// when it last settled, or opened (open). A store that opens the
	// database again goes on from there: the process knows what it
	// settled, and the times in the database's log cannot say, since a
	// store's time may be ahead of the clock that dates a step. It has
	// one entry for each database, so the limit on a server's projects
	// bounds it.
	settled map[string]time.Duration
	// floor is the latest time any store of the process has given out.
	// No store's time is behind it, but for a forward step the store has
	// not settled, so the logs of one server share one time, and an
	// event in one database can be placed against an event in another.
	floor time.Time
}

// driftLeak is how much monotonic time forgives one unit of drift: a
// second an hour. A leap second and a time service's sub-second
// corrections are forgotten, so they never add up to a step over the
// months a daemon runs; small steps made quickly to stay under the
// tolerance do add up, and are caught as one.
const driftLeak = 3600

// ClockSteps counts the steps a [Clock] has seen since it was made.
type ClockSteps struct {
	// Forward and Backward count the steps in each direction.
	Forward, Backward int
	// LastAt is the clock's time just after the latest step, and LastBy
	// its size: positive forward, negative backward. Zero when there has
	// been none.
	LastAt time.Time
	LastBy time.Duration
}

// NewClock returns a Clock for opts.
func NewClock(opts ClockOptions) *Clock {
	c := &Clock{read: opts.Read, tol: opts.StepTolerance}
	if c.tol <= 0 {
		c.tol = DefaultStepTolerance
	}
	if c.read == nil {
		now := opts.Now
		if now == nil {
			now = time.Now
		}
		base := now()
		// Sub uses the monotonic readings when both times have one, and
		// the wall clock otherwise, which then never disagrees with
		// itself.
		c.read = func() (time.Time, time.Duration) { t := now(); return t, t.Sub(base) }
	}
	return c
}

// reading is one reading of a Clock.
type reading struct {
	// wall is the wall clock, in UTC to the microsecond; mono the
	// monotonic time elapsed.
	wall time.Time
	mono time.Duration
	// forward is the sum of the forward steps seen up to this reading.
	forward time.Duration
	// floor is Clock.floor.
	floor time.Time
}

// now reads the clock, and counts a step when the wall clock has moved
// further than the monotonic clock did, or less far, by more than the
// tolerance: between two readings, or over several.
func (c *Clock) now() reading {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readLocked()
}

// readLocked is now for a caller that holds c.mu.
func (c *Clock) readLocked() reading {
	wall, mono := c.read()
	wall = wall.UTC().Truncate(time.Microsecond)
	if c.started {
		elapsed := mono - c.lastMono
		// What was carried from earlier readings leaks away; what this
		// reading adds counts in full, so one step is measured exactly.
		if leak := max(0, elapsed) / driftLeak; c.drift > 0 {
			c.drift = max(0, c.drift-leak)
		} else {
			c.drift = min(0, c.drift+leak)
		}
		c.drift += wall.Sub(c.lastWall) - elapsed
		// To the microsecond, as a store's time is.
		switch by := c.drift.Truncate(time.Microsecond); {
		case by > c.tol:
			c.drift = 0
			c.forward += by
			c.steps.Forward++
			c.steps.LastAt, c.steps.LastBy = wall, by
		case by < -c.tol:
			c.drift = 0
			c.steps.Backward++
			c.steps.LastAt, c.steps.LastBy = wall, by
		}
	}
	c.started, c.lastWall, c.lastMono = true, wall, mono
	return reading{wall: wall, mono: mono, forward: c.forward, floor: c.floor}
}

// raise moves the floor up to t.
func (c *Clock) raise(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.floor) {
		c.floor = t
	}
}

// Now reads the wall clock, in UTC to the microsecond. A store's own
// time ([Store.Now]) may be later: it never goes backward.
func (c *Clock) Now() time.Time { return c.now().wall }

// Steps reads the clock and returns the steps seen so far.
func (c *Clock) Steps() ClockSteps {
	c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.steps
}

// open reads the clock for a store that is opening db, and returns with
// the reading how much of the clock's forward steps that store starts
// from as settled. The reading, the answer and the record of it are made
// under one hold of the lock, so a step another goroutine sees meanwhile
// is wholly before the opening or wholly after it, and is pending once.
//
// A store that opens a database again goes on from what the last one
// settled. For a database the process has not had open, the answer does
// not come from the times in its log, which are a store's and may be
// ahead of the clock that dates a step. It comes from what the process
// knows: it has written nothing to the database, so whatever claims the
// database holds were made before the clock's first reading, and every
// step seen since came after them and took its size from their leases.
// None is settled. A database with no history holds no claim, and has
// nothing to settle.
//
// What that cannot see is a database another process wrote after a step:
// a restored one, or one an operator's command filled. Its claims are
// then given a step they did not lose, but no more of it than the time
// since the database's latest event, which the store's time never goes
// before: a lease may live that much too long, and none ends early.
func (c *Clock) open(db string, history bool) (reading, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.readLocked()
	d, ok := c.settled[db]
	if !ok && !history {
		d = r.forward
	}
	if c.settled == nil {
		c.settled = map[string]time.Duration{}
	}
	c.settled[db] = d
	return r, d
}

// setSettled records that the store of db, which open recorded, has
// settled d of the forward steps.
func (c *Clock) setSettled(db string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settled[db] = d
}

// behindRate is how many times slower than the monotonic clock a store's
// time runs while the clock is behind it: a clock an hour behind is
// caught up with in two.
const behindRate = 2

// storeTime is a store's own time: the clock's, kept from going backward
// by the high-water mark, and kept from jumping forward until the store
// has settled the step.
type storeTime struct {
	mu sync.Mutex
	// high is the latest time the store gave out, or its database's
	// latest event's if that is later.
	high time.Time
	// mark is the time of the latest event the database held at opening.
	mark time.Time
	// settled is how much of the clock's forward steps the store's leases
	// have been given.
	settled time.Duration
	// held is set while the clock is behind high, since is the clock's
	// time when that began, and slack how much less the store's time has
	// advanced since than the monotonic clock: time its leases were
	// given for nothing, which a later jump forward takes back before
	// any is added.
	held     bool
	since    time.Time
	slack    time.Duration
	lastMono time.Duration
}

// view is one reading of a store's time.
type view struct {
	// at is the store's time; wall is the clock's.
	at, wall time.Time
	// forward is the clock's sum of forward steps, and pending the part
	// the store has not settled.
	forward, pending time.Duration
	// slack is storeTime.slack, and floor the clock's floor.
	slack time.Duration
	floor time.Time
}

// readTime reads the store's time.
func (s *Store) readTime() view {
	r := s.clock.now()
	t := &s.tm
	t.mu.Lock()
	defer t.mu.Unlock()
	v := view{wall: r.wall, forward: r.forward, pending: max(0, r.forward-t.settled)}
	v.at = r.wall.Add(-v.pending)
	// Where the store's time would be had the clock been behind it since
	// the last reading: on from the mark at half the monotonic clock's
	// rate.
	elapsed := max(0, r.mono-t.lastMono)
	slow := t.high.Add((elapsed / behindRate).Truncate(time.Microsecond))
	// Nor behind where another store of the server already is.
	if f := r.floor.Add(-v.pending); f.After(slow) {
		slow = f
	}
	if v.at.Before(slow) {
		// The clock is behind what the server has already given out. The
		// store's time goes on at that rate, so the clock gains on it,
		// leases still run out and events still get times of their own;
		// what it advances less than the monotonic clock is slack.
		if !t.held {
			t.held, t.since = true, r.wall
		}
		t.slack += max(0, elapsed-slow.Sub(t.high))
		t.high, v.at = slow, slow
	} else {
		t.high = v.at
		if v.pending == 0 {
			t.held, t.slack = false, 0
		}
	}
	t.lastMono, v.slack = r.mono, t.slack
	// Only a time given out raises the floor: a jump this store has not
	// settled may yet be undone by a step back.
	s.clock.raise(v.at)
	v.floor = r.floor
	return v
}

// ClockState is how a store's time stands against the clock.
type ClockState struct {
	// Now is the store's time and Clock the wall clock's. They are equal
	// unless the clock is behind the store's history, or a forward step
	// is not settled yet.
	Now, Clock time.Time
	// Behind is how far the clock is behind the store's time, which runs
	// at half speed until the clock has made that up. Zero when it is
	// not behind.
	Behind time.Duration
	// Since is the clock's time when the store first found it behind.
	Since time.Time
	// Pending is the size of the forward steps the store has not settled
	// yet; its next write settles them.
	Pending time.Duration
	// Mark is the time of the latest event the database held when the
	// store opened; zero if it held none.
	Mark time.Time
}

// ClockState reads the store's time and the clock.
func (s *Store) ClockState() ClockState {
	v := s.readTime()
	out := ClockState{Now: v.at, Clock: v.wall, Pending: v.pending}
	s.tm.mu.Lock()
	defer s.tm.mu.Unlock()
	out.Mark = s.tm.mark
	if v.pending == 0 && v.at.After(v.wall) {
		out.Behind, out.Since = v.at.Sub(v.wall), s.tm.since
	}
	return out
}

// ClockState reads the control store's time and the clock.
func (c *Control) ClockState() ClockState { return c.s.ClockState() }

// SettleClock settles any forward step of the clock, as the control
// store's next write would.
func (c *Control) SettleClock(ctx context.Context) error { return c.s.SettleClock(ctx) }

// initTime starts the store's time at its database's high-water mark,
// the time of its latest event, and counts as settled the forward steps
// that the store which last had the database open in this process had
// settled: a store that was closed when the clock stepped settles the
// step with its first write. For a database the process has not had
// open, that is none of the steps its clock has seen ([Clock.open]).
func (s *Store) initTime(ctx context.Context) error {
	var mark sql.NullTime
	err := s.r.QueryRowContext(ctx, `SELECT at FROM events ORDER BY seq DESC LIMIT 1`).Scan(&mark)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read the latest event's time: %w", err)
	}
	r, settled := s.clock.open(s.db, mark.Valid)
	t := &s.tm
	t.mu.Lock()
	defer t.mu.Unlock()
	t.settled = settled
	if mark.Valid {
		m := mark.Time.UTC()
		t.high, t.mark = m, m
		s.clock.raise(m)
	}
	t.lastMono = r.mono
	return nil
}

// SettleClock settles the forward steps of the clock that the store has
// not settled, which every write does first, and again once it holds the
// writer if the clock stepped while it waited. In one transaction it adds
// to each lease that is live on the store's time the part of the jump
// its holder did not have, and records a clock.step event; the store's
// time then follows the clock. A claim whose lease had run out before
// the step is left to the reaper.
//
// The jump is how far the store's time moves, which is less than the
// step when the clock had been behind the store's history. Of that, what
// the store's time had fallen behind the monotonic clock while it ran
// slow was already given to every lease, and is not given again.
func (s *Store) SettleClock(ctx context.Context) error {
	// A reading of its own, so that the write that first sees a step is
	// the one that settles it.
	if s.clock.now().forward == s.settledForward() {
		return nil
	}
	s.settling.Lock()
	defer s.settling.Unlock()
	v := s.readTime()
	if v.pending <= 0 {
		return nil
	}
	// Where the store's time goes once settled: to the clock, or to
	// where another store of the server already is, if that is later.
	after := v.at
	for _, t := range []time.Time{v.wall, v.floor} {
		if t.After(after) {
			after = t
		}
	}
	jump := after.Sub(v.at)
	// To the microsecond, as the column that holds a lease is.
	extend := max(0, jump-v.slack).Truncate(time.Microsecond)
	var n int
	err := s.writeRetry(ctx, ClockActor, true, func(w *wtx) error {
		n = 0
		if s.hasClaims && extend > 0 {
			live, err := s.liveClaims(ctx, w)
			if err != nil {
				return err
			}
			for _, c := range live {
				row, err := loadClaim(ctx, w.tx, c.Issue)
				if err != nil {
					return err
				}
				row.ExpiresAt = row.ExpiresAt.Add(extend)
				if err := writeClaim(ctx, w, row); err != nil {
					return err
				}
				n++
			}
		}
		return w.event(ctx, OpClockStep, ClockTarget, map[string]any{"at": w.now},
			map[string]any{"at": after, "step_us": v.pending.Microseconds(), "extended_us": extend.Microseconds(), "claims": n})
	})
	if err != nil {
		return fmt.Errorf("settle a clock step: %w", err)
	}
	t := &s.tm
	t.mu.Lock()
	t.settled = v.forward
	t.slack = max(0, t.slack-jump)
	t.mu.Unlock()
	s.clock.setSettled(s.db, v.forward)
	s.opts.Logger.Warn("clock step settled", "event", logs.ClockStepSettled, "step", v.pending.String(), "from", v.at, "to", after,
		"extended", extend.String(), "claims", n)
	return nil
}

// settledNow settles any forward step of the clock and then reads the
// store's time, for a write that checks a client's time against now
// before its transaction begins.
func (s *Store) settledNow(ctx context.Context) (time.Time, error) {
	if err := s.SettleClock(ctx); err != nil {
		return time.Time{}, err
	}
	return s.now(), nil
}

// liveClaims reads the claims that are live at w's time.
func (s *Store) liveClaims(ctx context.Context, w *wtx) ([]Claim, error) {
	return s.claims(ctx, w.tx, `principal IS NOT NULL AND expires_at > ?`, w.now)
}

// settledForward returns storeTime.settled.
func (s *Store) settledForward() time.Duration {
	s.tm.mu.Lock()
	defer s.tm.mu.Unlock()
	return s.tm.settled
}
