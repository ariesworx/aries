package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// Locks in the soak: the gate of stage 6's slice G2 (design §20.10).
// Sessions contend for two locks of the first project, a mutex and a
// semaphore of three, through everything the run does to every session:
// answers dropped after the server applied the request, every connection
// dropped, daemon restarts, the clock set forward and back, and access
// ended and restored. A session waits in a queue or is refused, holds for
// a while, renewing, and releases; or it dies holding or waiting, or
// stalls past its lease. From the project's own event log the replay
// checks, transaction by transaction, that no grant ever finds the lock
// at its capacity, that grants follow the order of the live waiters,
// that epochs only rise, that no hold or place is ended before its lease,
// and that no slot is left free while someone waits; the tables are held
// to the replay at every snapshot; and every lapsed hold is ended within
// the bound of soakLockLate of its lease, so that a dead holder's lock
// passes on, unless a daemon restart fell in between.

// soakLocks are the locks the run contends for, and how many sessions
// contend for each.
var soakLocks = []struct {
	name     string
	capacity int
	sessions int
}{
	{"train/main", 1, 4},
	{"pool", 3, 5},
}

// soakLockLease is the hold's lease the sessions ask for, on the store's
// clock: half a second of real time at the run's speed.
const soakLockLease = time.Minute

// soakLockLate is how long after its lease a hold may still stand, in
// real time: two ticks of the reaper, and room for the tick's wait for
// the one writer of a busy project on a loaded machine. A run of four
// minutes once saw two seconds, for a tick of a tenth of one. In a
// server, whose tick is 30 seconds, it is the tick that counts.
func (s *soak) soakLockLate() time.Duration {
	return 2*100*time.Millisecond + 3*time.Second
}

// lockStats counts what the lock sessions did.
type lockStats struct {
	mu sync.Mutex
	// told maps a lock and an epoch to the session the server said holds
	// it: no epoch is ever told to two.
	told map[string]sessKey
	// held, waited, refused, vanished, stalled and lost count the holds
	// begun, the places taken, the acquires refused busy, the sessions
	// that died, the stalls past a lease, and the holds a session learned
	// it had lost.
	held, waited, refused, vanished, stalled, lost int
}

// lockSession contends for one lock.
type lockSession struct {
	*session
	name     string
	capacity int
	conn     *mcpserver.RepoConn
	ln       *line
	conns    int
}

// lockSlots runs the lock sessions until ctx ends: each slot starts a new
// session when the one before it dies.
func (s *soak) lockSlots(ctx context.Context) {
	var in []string
	for _, p := range s.cfg.principals {
		if s.user(p, s.a) != nil {
			in = append(in, p)
		}
	}
	var wg sync.WaitGroup
	n := 0
	for _, l := range soakLocks {
		for i := range l.sessions {
			p, seed := in[n%len(in)], s.cfg.seed+uint64(9000+n)
			n++
			wg.Go(func() {
				rng := rand.New(rand.NewPCG(seed, seed^0x51ed)) //nolint:gosec // a reproducible workload, not security
				for gen := 0; ctx.Err() == nil; gen++ {
					tag := fmt.Sprintf("%s-%s-%dg%d", p, strings.ReplaceAll(l.name, "/", "."), i, gen)
					ls := &lockSession{session: &session{s: s, p: s.a, tag: tag, u: s.user(p, s.a), key: sessKey{p, "lock-" + tag},
						rng: rand.New(rand.NewPCG(rng.Uint64(), rng.Uint64()))}, name: l.name, capacity: l.capacity} //nolint:gosec // reproducible
					ls.run(ctx)
				}
			})
		}
	}
	wg.Wait()
}

// connect dials until it has a watched connection or ctx ends.
func (ls *lockSession) connect(ctx context.Context) bool {
	for ctx.Err() == nil {
		ls.conns++
		ln := ls.inbox.newLine(ls.key, ls.conns, ls.s.mon, ls.p)
		dctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		known, start := ls.s.ctlKnown.Load(), ls.s.clock.now()
		c, err := mcpserver.DialRepo(dctx, ls.u.repo, ls.options(ln))
		cancel()
		ls.s.dialed(ls.session, known, start, c, err)
		if err != nil {
			pause(ctx, time.Duration(20+ls.rng.IntN(60))*time.Millisecond+refusedPause(err))
			continue
		}
		ln.watching(false)
		if out, _ := ls.s.do(ls.session, c, proto.OpWatch, proto.WatchArgs{}, &proto.WatchResult{}); out == acked {
			ln.watched(ls.s.clock.now())
		}
		if c.Err() != nil {
			_ = c.Close()
			continue
		}
		ls.conn, ls.ln = c, ln
		return true
	}
	return false
}

// drop closes the connection.
func (ls *lockSession) drop() {
	if ls.conn == nil {
		return
	}
	ls.ln.broke()
	_ = ls.conn.Close()
	ls.conn, ls.ln = nil, nil
}

// call sends one request, reconnecting first if the connection is gone,
// and sends it again when the connection failed under it: every lock op
// is safe to repeat. An acquire or a renewal repeated gets the hold or
// the place the first made; a release repeated is refused, the hold
// being gone.
func (ls *lockSession) call(ctx context.Context, op string, args, res any) (outcome, *proto.Error) {
	for attempt := 0; ; attempt++ {
		if ls.conn == nil || ls.conn.Err() != nil {
			ls.drop()
			if !ls.connect(ctx) {
				return lost, nil
			}
		}
		out, perr := ls.s.do(ls.session, ls.conn, op, args, res)
		if out != lost {
			return out, perr
		}
		ls.drop()
		if attempt >= 4 || ctx.Err() != nil {
			return lost, nil
		}
		ls.s.mon.retried(op)
	}
}

// told notes that the server said ls holds its lock at epoch, and fails
// the run if it said so of another session before.
func (ls *lockSession) told(epoch int64) {
	st := &ls.s.locks
	st.mu.Lock()
	defer st.mu.Unlock()
	k := fmt.Sprintf("%s@%d", ls.name, epoch)
	if st.told == nil {
		st.told = map[string]sessKey{}
	}
	if prev, ok := st.told[k]; ok && prev != ls.key {
		ls.s.mon.fail("", "lock %s epoch %d was told to %s and to %s", ls.name, epoch, prev, ls.key)
	}
	st.told[k] = ls.key
}

func (ls *lockSession) count(f func(*lockStats)) {
	ls.s.locks.mu.Lock()
	defer ls.s.locks.mu.Unlock()
	f(&ls.s.locks)
}

// run contends for the lock until ctx ends or the session dies.
func (ls *lockSession) run(ctx context.Context) {
	defer ls.drop()
	s := ls.s
	holdRenew := s.clock.real(soakLockLease) / 3
	placeRenew := s.clock.real(soakPlaceLease) / 4
	for ctx.Err() == nil {
		wait := ls.chance(0.7)
		var r proto.LockResult
		out, perr := ls.call(ctx, proto.OpLockAcquire, proto.LockAcquireArgs{Name: ls.name, Capacity: ls.capacity, Lease: "1m", Wait: wait}, &r)
		switch {
		case out == refused && perr.Code == proto.CodeBusy:
			ls.count(func(st *lockStats) { st.refused++ })
			pause(ctx, time.Duration(ls.rng.IntN(int(holdRenew)+1)))
			continue
		case out != acked:
			pause(ctx, 50*time.Millisecond)
			continue
		}
		if !r.Held {
			ls.count(func(st *lockStats) { st.waited++ })
		}
		for !r.Held && ctx.Err() == nil {
			pause(ctx, placeRenew)
			switch {
			case ls.chance(0.01):
				ls.count(func(st *lockStats) { st.vanished++ }) // a waiter dies: its place lapses
				return
			case ls.chance(0.02):
				ls.call(ctx, proto.OpLockLeave, proto.LockLeaveArgs{Name: ls.name}, &proto.LockLeaveResult{})
			}
			out, perr = ls.call(ctx, proto.OpLockRenew, proto.LockRenewArgs{Name: ls.name}, &r)
			if out == refused && perr.Code == proto.CodeConflict {
				break // the place lapsed, or was left: acquire again
			}
		}
		if !r.Held {
			continue
		}
		ls.told(r.Epoch)
		ls.count(func(st *lockStats) { st.held++ })
		epoch := r.Epoch
		end := time.Now().Add(time.Duration(ls.rng.Int64N(int64(3 * s.clock.real(soakLockLease)))))
		kept := true
		for time.Now().Before(end) && ctx.Err() == nil && kept {
			pause(ctx, holdRenew)
			switch {
			case ls.chance(0.02):
				ls.count(func(st *lockStats) { st.vanished++ }) // the holder dies holding: the lease ends it
				return
			case ls.chance(0.02):
				ls.count(func(st *lockStats) { st.stalled++ })
				pause(ctx, 2*s.clock.real(soakLockLease))
			}
			out, perr = ls.call(ctx, proto.OpLockRenew, proto.LockRenewArgs{Name: ls.name, Epoch: epoch}, &r)
			if out == refused && perr.Code == proto.CodeConflict {
				kept = false
				ls.count(func(st *lockStats) { st.lost++ })
			}
		}
		if kept {
			ls.call(ctx, proto.OpLockRelease, proto.LockReleaseArgs{Name: ls.name, Epoch: epoch}, &proto.LockReleaseResult{})
		}
		pause(ctx, time.Duration(ls.rng.IntN(int(s.cfg.think)+1)))
	}
}

// soakPlaceLease is a place's lease in a queue, as the store has it.
const soakPlaceLease = 2 * time.Minute

// rLockHold is a hold as the replay knows it.
type rLockHold struct {
	holder  sessKey
	granted time.Time
}

// rLock is a lock as the replay rebuilds it.
type rLock struct {
	capacity int
	holds    map[int64]rLockHold
	queue    []sessKey
}

// lockLate is a hold that lapsed, ended by the event at seq: how long
// after its lease ran out.
type lockLate struct {
	name     string
	epoch    int64
	expires  time.Time
	at       time.Time
	seq      int64
	granted  bool // the end granted the lock to a waiter in the same write
	restarts bool // a daemon restart fell between the lease's end and the event
}

// lockReplay rebuilds a project's locks from its event log.
type lockReplay struct {
	locks map[string]*rLock
	// epoch is the project's last epoch; every grant takes the next.
	epoch int64
	// late are the lapsed holds, as they were ended.
	late []lockLate
	// ops counts the lock events by op, and peak the most holders each
	// lock had at once.
	ops  map[string]int
	peak map[string]int
	// lost are the holds and places ended for lost access.
	lost int
	// jumps are the project's settled forward steps of the clock.
	jumps []clockJump
}

// clockJump is a settled forward step of the clock: event seq moved the
// store's time from from to to.
type clockJump struct {
	seq      int64
	from, to time.Time
}

// lateness is how long after its lease a lapsed hold was ended, less the
// jumps of the clock settled in between: a lease that ran out before a
// jump had all of its time, and is ended on the time after the jump,
// which no reaper's delay made.
func (lr *lockReplay) lateness(lt lockLate) time.Duration {
	d := lt.at.Sub(lt.expires)
	for _, j := range lr.jumps {
		if j.seq < lt.seq && !j.from.Before(lt.expires) {
			d -= j.to.Sub(j.from)
		}
	}
	return d
}

func newLockReplay() *lockReplay {
	return &lockReplay{locks: map[string]*rLock{}, ops: map[string]int{}, peak: map[string]int{}}
}

// lockState is the part of a lock event's states the replay reads.
type lockState struct {
	Holder struct {
		Principal string `json:"principal"`
		Session   string `json:"session"`
	} `json:"holder"`
	Epoch     int64     `json:"epoch"`
	ExpiresAt time.Time `json:"expires_at"`
	Capacity  int       `json:"capacity"`
	Reason    string    `json:"reason"`
}

func (st lockState) who() sessKey { return sessKey{st.Holder.Principal, st.Holder.Session} }

// applyLock applies one lock event of the transaction tx.
func (r *replay) applyLock(e sevent) {
	lr := r.locks
	lr.ops[e.op]++
	name := strings.TrimPrefix(e.target, "lock:")
	if name == e.target {
		r.fail("", "event %d (%s) targets %q, not lock:NAME", e.seq, e.op, e.target)
		return
	}
	var before, after lockState
	_ = json.Unmarshal(e.before, &before)
	_ = json.Unmarshal(e.after, &after)
	l := lr.locks[name]
	if l == nil {
		l = &rLock{holds: map[int64]rLockHold{}}
		lr.locks[name] = l
	}
	if after.Capacity > 0 {
		if l.capacity != 0 && l.capacity != after.Capacity {
			r.fail("", "event %d: lock %s has capacity %d, the log said %d", e.seq, name, after.Capacity, l.capacity)
		}
		l.capacity = after.Capacity
	}
	grant := func(who sessKey) {
		if after.Epoch != lr.epoch+1 {
			r.fail("", "event %d grants %s epoch %d after epoch %d: epochs rise by one in a project", e.seq, name, after.Epoch, lr.epoch)
		}
		lr.epoch = max(lr.epoch, after.Epoch)
		if len(l.holds) >= l.capacity {
			r.fail("", "event %d grants %s to %s with %d holders already, at capacity %d", e.seq, name, who, len(l.holds), l.capacity)
		}
		l.holds[after.Epoch] = rLockHold{holder: who, granted: e.at}
		lr.peak[name] = max(lr.peak[name], len(l.holds))
	}
	switch e.op {
	case "lock.acquire":
		if len(l.queue) > 0 {
			r.fail("", "event %d: %s acquired %s at once while %v waited", e.seq, after.who(), name, l.queue)
		}
		if after.who() != e.actor {
			r.fail("", "event %d: %s acquired %s for %s", e.seq, e.actor, name, after.who())
		}
		grant(after.who())
	case "lock.wait":
		if slices.Contains(l.queue, e.actor) {
			r.fail("", "event %d: %s joined %s's queue twice", e.seq, e.actor, name)
		}
		l.queue = append(l.queue, e.actor)
	case "lock.grant":
		who := before.who()
		if len(l.queue) == 0 || l.queue[0] != who {
			r.fail("", "event %d grants %s to %s, but the first live waiter is %v", e.seq, name, who, l.queue)
		}
		l.queue = slices.DeleteFunc(l.queue, func(k sessKey) bool { return k == who })
		grant(who)
	case "lock.release", "lock.expire":
		h, ok := l.holds[before.Epoch]
		switch {
		case !ok:
			r.fail("", "event %d ends %s epoch %d, which nobody held", e.seq, name, before.Epoch)
		case h.holder != before.who():
			r.fail("", "event %d ends %s epoch %d of %s, which %s held", e.seq, name, before.Epoch, before.who(), h.holder)
		}
		if e.op == "lock.expire" {
			if before.ExpiresAt.After(e.at) {
				r.fail("", "event %d ended %s epoch %d at %s, before its lease ran out at %s", e.seq, name, before.Epoch, stamp(e.at), stamp(before.ExpiresAt))
			}
			lr.late = append(lr.late, lockLate{name: name, epoch: before.Epoch, expires: before.ExpiresAt, at: e.at, seq: e.seq})
		}
		if after.Reason != "" {
			lr.lost++
		}
		delete(l.holds, before.Epoch)
	case "lock.leave":
		who := before.who()
		if !slices.Contains(l.queue, who) {
			r.fail("", "event %d: %s left %s's queue, where it had no place", e.seq, who, name)
		}
		if after.Reason == "lease expired" && before.ExpiresAt.After(e.at) {
			r.fail("", "event %d dropped %s's place in %s at %s, before it lapsed at %s", e.seq, who, name, stamp(e.at), stamp(before.ExpiresAt))
		}
		if after.Reason != "" && after.Reason != "lease expired" {
			lr.lost++
		}
		l.queue = slices.DeleteFunc(l.queue, func(k sessKey) bool { return k == who })
	default:
		r.fail("", "event %d has op %q, which the soak does not expect", e.seq, e.op)
	}
}

// endLocks checks, once a transaction is applied, each lock it touched:
// no slot is left free while someone waits, and a lapsed hold ended in
// it gave its slot to the first waiter. A lock nobody holds or waits for
// is gone, and its capacity with it.
func (r *replay) endLocks(tx []sevent) {
	lr := r.locks
	touched := map[string]bool{}
	for _, e := range tx {
		if strings.HasPrefix(e.op, "lock.") {
			touched[strings.TrimPrefix(e.target, "lock:")] = true
		}
	}
	for name := range touched {
		l := lr.locks[name]
		if l == nil {
			continue
		}
		if len(l.holds) < l.capacity && len(l.queue) > 0 {
			r.fail("", "events up to %d leave %s with %d of %d holders while %v wait", tx[len(tx)-1].seq, name, len(l.holds), l.capacity, l.queue)
		}
		for i := range lr.late {
			if lt := &lr.late[i]; lt.name == name && lt.seq >= tx[0].seq {
				lt.granted = slices.ContainsFunc(tx, func(e sevent) bool { return e.op == "lock.grant" && e.target == "lock:"+name })
			}
		}
		if len(l.holds) == 0 && len(l.queue) == 0 {
			delete(lr.locks, name)
		}
	}
}

// compareLocks holds the lock tables to the replay.
func (r *replay) compareLocks(tb tables) {
	want := map[string]string{}
	for name, l := range r.locks.locks {
		var parts []string
		for _, ep := range slices.Sorted(maps.Keys(l.holds)) {
			parts = append(parts, fmt.Sprintf("%d:%s", ep, l.holds[ep].holder))
		}
		for _, k := range l.queue {
			parts = append(parts, "w:"+k.String())
		}
		want[name] = strings.Join(parts, " ")
	}
	for name, got := range tb.locks {
		if want[name] != got {
			r.fail("", "lock %s: the tables hold %q, the event log rebuilds %q", name, got, want[name])
		}
	}
	for name, w := range want {
		if _, ok := tb.locks[name]; !ok {
			r.fail("", "lock %s: the event log rebuilds %q, the tables hold nothing", name, w)
		}
	}
}

// restartMark notes, for each project, the last event of its log as the
// daemon stops for a restart.
func (s *soak) restartMark() {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	for _, p := range s.projs {
		var seq int64
		if err := p.db.QueryRowContext(ctx, `SELECT seq FROM events ORDER BY seq DESC LIMIT 1`).Scan(&seq); err == nil {
			p.marks = append(p.marks, seq)
		}
	}
}

// awaitLocksEnded waits, once the sessions have stopped, until no lock
// is held or waited for: the holds and places left behind lapse within
// their leases, and the reaper ends them.
func (s *soak) awaitLocksEnded(ctx context.Context) {
	for _, p := range s.projs {
		for {
			var n int
			err := p.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM lock_holders) + (SELECT COUNT(*) FROM lock_waiters)`).Scan(&n)
			if err == nil && n == 0 {
				break
			}
			if ctx.Err() != nil {
				s.t.Errorf("project %s still has %d holds and places of locks at rest (%v)", p.name, n, err)
				break
			}
			pause(ctx, 50*time.Millisecond)
		}
	}
}

// checkLocks: once the run is at rest, every lapsed hold was ended within
// soakLockLate of its lease, unless a daemon restart fell in between; no
// lock is held or waited for, all having lapsed or been released; and
// the replay's counts are reported.
func (s *soak) checkLocks(ctx context.Context, p *sproj) {
	lr := p.rep.locks
	bound := time.Duration(float64(s.soakLockLate()) * s.cfg.speed)
	// A restart: the daemon stopped after event m, at t0, and its
	// successor wrote event m+1 at t1. A hold whose lease ran out from a
	// bound before t0 to t1, and that a later event ended, waited out the
	// restart, when no reaper ran.
	type restart struct {
		m      int64
		t0, t1 time.Time
	}
	var restarts []restart
	for _, m := range p.marks {
		var t0, t1 time.Time
		if err := p.db.QueryRowContext(ctx, `SELECT at FROM events WHERE seq = ?`, m).Scan(&t0); err != nil {
			s.t.Errorf("read event %d: %v", m, err)
			continue
		}
		if err := p.db.QueryRowContext(ctx, `SELECT at FROM events WHERE seq = ?`, m+1).Scan(&t1); err != nil {
			continue // nothing was written after the restart
		}
		restarts = append(restarts, restart{m, t0.UTC(), t1.UTC()})
	}
	for i := range lr.late {
		lt := &lr.late[i]
		for _, rs := range restarts {
			if lt.seq > rs.m && !lt.expires.Before(rs.t0.Add(-bound)) && !lt.expires.After(rs.t1) {
				lt.restarts = true
			}
		}
		if late := lr.lateness(*lt); late > bound && !lt.restarts {
			s.mon.fail("", "lock %s epoch %d lapsed at %s and was ended %s later (event %d), past %s (%s of real time)",
				lt.name, lt.epoch, stamp(lt.expires), late, lt.seq, bound, s.soakLockLate())
		}
	}
	for name, l := range lr.locks {
		s.mon.fail("", "lock %s is still held or waited for at rest: %d holders, queue %v", name, len(l.holds), l.queue)
	}
}

// reportLocks says what the lock sessions and the log did.
func (s *soak) reportLocks(b *bytes.Buffer) {
	lr := s.a.rep.locks
	st := &s.locks
	st.mu.Lock()
	defer st.mu.Unlock()
	var lates []time.Duration
	excused := 0
	for _, lt := range lr.late {
		if lt.restarts {
			excused++
			continue
		}
		lates = append(lates, time.Duration(float64(lr.lateness(lt))/s.cfg.speed))
	}
	slices.Sort(lates)
	q := func(f float64) time.Duration {
		if len(lates) == 0 {
			return 0
		}
		return lates[min(len(lates)-1, int(f*float64(len(lates))))].Round(time.Millisecond)
	}
	fmt.Fprintf(b, "  locks: %d holds begun, %d places taken, %d acquires refused busy, %d sessions died, %d stalls, %d holds found lost; log: %d acquire, %d wait, %d grant, %d release (%d for lost access), %d expire, %d leave; peak holders %v; lapsed holds ended after their lease (real time) p50 %s, p99 %s, max %s of %d, %d across a restart\n",
		st.held, st.waited, st.refused, st.vanished, st.stalled, st.lost,
		lr.ops["lock.acquire"], lr.ops["lock.wait"], lr.ops["lock.grant"], lr.ops["lock.release"], lr.lost, lr.ops["lock.expire"], lr.ops["lock.leave"],
		lr.peak, q(0.5), q(0.99), q(1), len(lates), excused)
}
