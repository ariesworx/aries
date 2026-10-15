package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// Locks (design §20.5; decision 70). A lock is a name in the project,
// held by up to its capacity of holders at once, each a (principal,
// session, machine) under a lease, as a claim is. It is tied to no issue,
// and holding one changes no issue: a merge train, a deploy or a shared
// resource takes one around its critical section, through `sfx lock run`
// or the protocol. No agent tool takes one.
//
// Each grant takes the project's next epoch, so a lock's epochs only
// rise, and the epoch is the fencing token: a renewal or release that
// names another is refused. An acquire that finds the lock full either
// joins its queue or is refused; the queue is served first in, first
// out, among the places whose lease is live: a place is kept by renewing
// it, within WaiterLease, so a waiter that died never holds the queue up.
//
// A hold or a place whose lease ran out holds nothing, before the reaper
// too: every write to a lock first ends what lapsed (lock.expire for a
// hold, lock.leave for a place) and grants the free slots to the queue
// (lock.grant), so a grant never finds more live holders than capacity,
// and never skips a live waiter. The reaper does the same for every lock
// with something lapsed (ReapLocks), and losing access ends a principal's
// holds and places (EndLocks). A lock's row goes once nobody holds or
// waits for it.

// Lock bounds.
const (
	// WaiterLease is how long a place in a queue lasts unless renewed;
	// `sfx lock run` renews it every 30 seconds.
	WaiterLease = 2 * time.Minute
	// MaxLockName is the longest name a lock may have: an event's target
	// is "lock:" and the name, in 64 bytes.
	MaxLockName = 64 - len(lockTargetPrefix)
	// maxLockCapacity bounds the lock_capacity limit.
	maxLockCapacity = 1000
)

const lockTargetPrefix = "lock:"

// lockNamePattern is what a lock's name may be: a short path of
// lowercase words.
var lockNamePattern = lazyregexp.New(`^[a-z0-9][a-z0-9._/-]{0,58}$`)

// ValidLockName reports whether name may name a lock.
func ValidLockName(name string) bool { return lockNamePattern.MatchString(name) }

// LockTarget is the target of lock name's events, which no issue id can
// be, so a board's watch of issue events is not pushed them.
func LockTarget(name string) string { return lockTargetPrefix + name }

// checkLockName refuses a name no lock may have, with ErrInvalid.
func checkLockName(name string) error {
	if !ValidLockName(name) {
		return fmt.Errorf("%w: lock name %q: a lowercase letter or digit, then up to %d of lowercase letters, digits and . _ / -",
			ErrInvalid, name, MaxLockName-1)
	}
	return nil
}

// The lock events and inbox kinds. lock.acquire is a grant made at once,
// to the acquirer; lock.wait a place in the queue; lock.grant a grant to
// the first waiter, in whoever's write freed the slot; lock.release a
// hold ended by its holder, or with a reason in its after state by lost
// access; lock.expire a hold whose lease ran out; lock.leave a place
// given up, or with a reason in its after state ended by its lease or
// lost access. Renewals are not events.
const (
	OpLockAcquire Op = "lock.acquire"
	OpLockWait    Op = "lock.wait"
	OpLockGrant   Op = "lock.grant"
	OpLockRelease Op = "lock.release"
	OpLockExpire  Op = "lock.expire"
	OpLockLeave   Op = "lock.leave"

	// InboxLockGranted tells a waiting session the lock is its now.
	InboxLockGranted InboxKind = "lock.granted"
	// InboxLockLost tells a holding session its hold ended without its
	// asking: the lease ran out, or its principal lost access.
	InboxLockLost InboxKind = "lock.lost"
)

// LockLapsed is the reason in the after state of a lock.leave that the
// place's lease ended.
const LockLapsed = "lease expired"

// LockHold is a hold of a lock.
type LockHold struct {
	Holder    Actor     `json:"holder"`
	Epoch     int64     `json:"epoch"`
	GrantedAt time.Time `json:"granted_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// LockPlace is a place in a lock's queue; Position counts from 1.
type LockPlace struct {
	Holder    Actor     `json:"holder"`
	Position  int       `json:"position"`
	JoinedAt  time.Time `json:"joined_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Lock is a lock as read: its live holds, oldest grant first, and its
// live places, in the order they will be served.
type Lock struct {
	Name     string
	Capacity int
	Holders  []LockHold
	Waiters  []LockPlace
}

// LockAcquire is the input to AcquireLock. Capacity 0 is the lock's
// capacity, or 1 for a lock nobody holds or waits for; Lease 0 is
// DefaultLease.
type LockAcquire struct {
	Name     string
	Capacity int
	Lease    time.Duration
	Wait     bool
}

// LockState is where the caller stands with a lock: Held is its hold,
// when it has one; else Position is its place in the queue and
// WaitExpires when that lapses unless renewed.
type LockState struct {
	Name        string
	Capacity    int
	Held        *LockHold
	Position    int
	WaitExpires time.Time
}

// LockLeft is what LeaveLock did: Left is set when it gave up the
// caller's place, and Held when the caller holds the lock instead.
type LockLeft struct {
	Left bool
	Held *LockHold
}

// LockEnd is a hold, or a place when Waiting, that the reaper or lost
// access ended.
type LockEnd struct {
	Name    string
	Holder  Actor
	Epoch   int64
	Waiting bool
}

// LockBusyError refuses an acquire that does not wait of a lock that is
// full, or has a queue: Holders hold it now, and Waiters wait. It wraps
// ErrBusy.
type LockBusyError struct {
	Name     string
	Capacity int
	Holders  []LockHold
	Waiters  int
}

func (e *LockBusyError) Error() string {
	return fmt.Sprintf("%v: lock %s is held by %d of %d, with %d waiting", ErrBusy, e.Name, len(e.Holders), e.Capacity, e.Waiters)
}

// Unwrap makes errors.Is(err, ErrBusy) hold.
func (e *LockBusyError) Unwrap() error { return ErrBusy }

// LockEpochError refuses a renewal or a release of a hold the caller's
// principal does not have at Epoch, which ended or was never its, or,
// with Place, a renewal of a place in the queue the caller does not
// have. Holders hold the lock now. It wraps ErrConflict.
type LockEpochError struct {
	Name    string
	Epoch   int64
	Place   bool
	Holders []LockHold
}

func (e *LockEpochError) Error() string {
	if e.Place {
		return fmt.Sprintf("lock %s: no place in its queue is yours", e.Name)
	}
	return fmt.Sprintf("lock %s: no hold at epoch %d is yours", e.Name, e.Epoch)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *LockEpochError) Unwrap() error { return ErrConflict }

// LockLimitError refuses an acquire past Limit (locks, lock_waiters or
// lock_waits_per_principal), which is Max. It wraps ErrBusy.
type LockLimitError struct {
	Name  string
	Limit string
	Max   int
}

func (e *LockLimitError) Error() string {
	switch e.Limit {
	case "locks":
		return fmt.Sprintf("%v: the project has %d locks in use, its limit (locks)", ErrBusy, e.Max)
	case "lock_waiters":
		return fmt.Sprintf("%v: lock %s has %d waiting, its limit (lock_waiters)", ErrBusy, e.Name, e.Max)
	}
	return fmt.Sprintf("%v: you wait for %d locks already, the limit (lock_waits_per_principal)", ErrBusy, e.Max)
}

// Unwrap makes errors.Is(err, ErrBusy) hold.
func (e *LockLimitError) Unwrap() error { return ErrBusy }

// LockCapacityError refuses an acquire whose capacity is not the lock's
// (Capacity, which its first acquire set), or with Capacity 0 one past
// the lock_capacity limit, which is Max. It wraps ErrInvalid.
type LockCapacityError struct {
	Name     string
	Asked    int
	Capacity int
	Max      int
}

func (e *LockCapacityError) Error() string {
	if e.Capacity == 0 {
		return fmt.Sprintf("%v: capacity %d: a lock holds 1 to %d at once (lock_capacity)", ErrInvalid, e.Asked, e.Max)
	}
	return fmt.Sprintf("%v: lock %s has capacity %d, which its first acquire set, not %d", ErrInvalid, e.Name, e.Capacity, e.Asked)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *LockCapacityError) Unwrap() error { return ErrInvalid }

// holdRow is a hold as stored, with the lease its acquire asked for.
type holdRow struct {
	LockHold
	lease time.Duration
}

// placeRow is a place as stored.
type placeRow struct {
	holder          Actor
	seq             int64
	joined, expires time.Time
	lease           time.Duration
}

// lockView is a lock in a write: its row, if it has one, and its holds
// and places, oldest first.
type lockView struct {
	name     string
	capacity int
	exists   bool
	holds    []holdRow
	places   []placeRow
}

// heldBy returns the hold of actor, or -1.
func (v *lockView) heldBy(a Actor) int {
	return slices.IndexFunc(v.holds, func(h holdRow) bool { return h.Holder == a })
}

// holdOf returns the hold of principal at epoch, or -1.
func (v *lockView) holdOf(principal string, epoch int64) int {
	return slices.IndexFunc(v.holds, func(h holdRow) bool { return h.Holder.Principal == principal && h.Epoch == epoch })
}

// placeOf returns the place of actor in the queue, or -1.
func (v *lockView) placeOf(a Actor) int {
	return slices.IndexFunc(v.places, func(p placeRow) bool { return p.holder == a })
}

func (v *lockView) heldNow() []LockHold {
	out := make([]LockHold, len(v.holds))
	for i, h := range v.holds {
		out[i] = h.LockHold
	}
	return out
}

// openLock reads lock name in w and brings it up to w's time: it ends
// the holds and places that lapsed, and grants the free slots to the
// queue. It returns the lock and the holds and places it ended; a lock
// that then has none is gone, and the view says it does not exist.
func (w *wtx) openLock(ctx context.Context, name string) (*lockView, []LockEnd, error) {
	v := &lockView{name: name}
	err := w.tx.QueryRowContext(ctx, `SELECT capacity FROM locks WHERE name = ?`, name).Scan(&v.capacity)
	if errors.Is(err, sql.ErrNoRows) {
		return v, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read lock %s: %w", name, err)
	}
	v.exists = true
	holds, err := loadNamedHolds(ctx, w.tx, `name = ?`, name)
	if err != nil {
		return nil, nil, err
	}
	for _, h := range holds {
		v.holds = append(v.holds, h.holdRow)
	}
	places, err := loadNamedPlaces(ctx, w.tx, `name = ?`, name)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range places {
		v.places = append(v.places, p.placeRow)
	}
	var ends []LockEnd
	live := v.holds[:0]
	for _, h := range v.holds {
		if h.ExpiresAt.After(w.now) {
			live = append(live, h)
			continue
		}
		if err := w.endHold(ctx, name, h, OpLockExpire, ""); err != nil {
			return nil, nil, err
		}
		ends = append(ends, LockEnd{Name: name, Holder: h.Holder, Epoch: h.Epoch})
	}
	v.holds = live
	waiting := v.places[:0]
	for _, p := range v.places {
		if p.expires.After(w.now) {
			waiting = append(waiting, p)
			continue
		}
		if err := w.endPlace(ctx, name, p, LockLapsed); err != nil {
			return nil, nil, err
		}
		ends = append(ends, LockEnd{Name: name, Holder: p.holder, Waiting: true})
	}
	v.places = waiting
	if err := w.grantNext(ctx, v); err != nil {
		return nil, nil, err
	}
	return v, ends, w.dropIfIdle(ctx, v)
}

// dropIfIdle deletes v's row once nobody holds or waits for it. What
// emptied it recorded the event.
func (w *wtx) dropIfIdle(ctx context.Context, v *lockView) error {
	if !v.exists || len(v.holds) > 0 || len(v.places) > 0 {
		return nil
	}
	if _, err := w.exec(ctx, `DELETE FROM locks WHERE name = ?`, v.name); err != nil {
		return fmt.Errorf("drop lock %s: %w", v.name, err)
	}
	v.exists = false
	return nil
}

// endHold deletes hold h of lock name and records op: lock.expire for a
// lapsed hold, lock.release otherwise, with reason in its after state
// when the holder did not ask. The holder is told of an end it did not
// ask for (lock.lost).
func (w *wtx) endHold(ctx context.Context, name string, h holdRow, op Op, reason string) error {
	if _, err := w.exec(ctx, `DELETE FROM lock_holders WHERE name = ? AND epoch = ?`, name, h.Epoch); err != nil {
		return fmt.Errorf("end hold of %s: %w", name, err)
	}
	var after any // nil, not a nil map, so the state is NULL
	if reason != "" {
		after = map[string]string{"reason": reason}
	}
	if err := w.event(ctx, op, LockTarget(name), map[string]any{"holder": h.Holder, "epoch": h.Epoch, "expires_at": h.ExpiresAt}, after); err != nil {
		return err
	}
	change := LockReleased
	switch {
	case op == OpLockExpire:
		change = LockExpired
		reason = "lease expired"
	case reason != "":
		reason += " by " + w.actor.Principal
	}
	w.locked(change, 0)
	if reason == "" {
		return nil
	}
	return w.notify(ctx, InboxItem{To: h.Holder.Principal, Session: h.Holder.Session, Kind: InboxLockLost,
		Body: fmt.Sprintf("lock %s lost: %s (epoch %d); stop what it guards", name, reason, h.Epoch)})
}

// endPlace deletes place p in lock name's queue and records lock.leave,
// with reason in its after state when the waiter did not ask.
func (w *wtx) endPlace(ctx context.Context, name string, p placeRow, reason string) error {
	if _, err := w.exec(ctx, `DELETE FROM lock_waiters WHERE name = ? AND seq = ?`, name, p.seq); err != nil {
		return fmt.Errorf("end place in %s: %w", name, err)
	}
	var after any // nil, not a nil map, so the state is NULL
	if reason != "" {
		after = map[string]string{"reason": reason}
	}
	w.locked(LockWithdrawn, 0)
	return w.event(ctx, OpLockLeave, LockTarget(name), map[string]any{"holder": p.holder, "joined_at": p.joined, "expires_at": p.expires}, after)
}

// grantNext grants v's free slots to its queue, first in first out, and
// tells each new holder's session.
func (w *wtx) grantNext(ctx context.Context, v *lockView) error {
	for len(v.holds) < v.capacity && len(v.places) > 0 {
		p := v.places[0]
		v.places = v.places[1:]
		if _, err := w.exec(ctx, `DELETE FROM lock_waiters WHERE name = ? AND seq = ?`, v.name, p.seq); err != nil {
			return fmt.Errorf("grant %s: %w", v.name, err)
		}
		h, err := w.grant(ctx, v, p.holder, p.lease, &p)
		if err != nil {
			return err
		}
		if err := w.notify(ctx, InboxItem{To: p.holder.Principal, Session: p.holder.Session, Kind: InboxLockGranted,
			Body: fmt.Sprintf("lock %s granted (epoch %d, until %s)", v.name, h.Epoch, h.ExpiresAt.Format(time.RFC3339))}); err != nil {
			return err
		}
	}
	return nil
}

// grant gives holder a hold of v for lease under the project's next
// epoch, and records lock.acquire, or lock.grant naming the place it was
// granted from.
func (w *wtx) grant(ctx context.Context, v *lockView, holder Actor, lease time.Duration, from *placeRow) (LockHold, error) {
	epoch, err := nextLockEpoch(ctx, w)
	if err != nil {
		return LockHold{}, err
	}
	h := holdRow{LockHold: LockHold{Holder: holder, Epoch: epoch, GrantedAt: w.now, ExpiresAt: w.now.Add(lease)}, lease: lease}
	if _, err := w.exec(ctx, `INSERT INTO lock_holders
  (name, epoch, principal, session, machine, granted_at, expires_at, lease_us, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, v.name, epoch, holder.Principal, holder.Session, holder.Machine, h.GrantedAt, h.ExpiresAt,
		lease.Microseconds(), randomInt63()); err != nil {
		return LockHold{}, fmt.Errorf("grant %s: %w", v.name, err)
	}
	v.holds = append(v.holds, h)
	op, change, waited := OpLockAcquire, LockAcquired, time.Duration(0)
	var before any // nil, not a nil map, so the state is NULL
	if from != nil {
		op, change, waited = OpLockGrant, LockGranted, w.now.Sub(from.joined)
		before = map[string]any{"holder": holder, "joined_at": from.joined}
	}
	w.locked(change, waited)
	return h.LockHold, w.event(ctx, op, LockTarget(v.name), before,
		map[string]any{"holder": holder, "epoch": epoch, "expires_at": h.ExpiresAt, "capacity": v.capacity})
}

// nextLockEpoch takes the project's next lock epoch.
func nextLockEpoch(ctx context.Context, w *wtx) (int64, error) {
	var e int64
	err := w.tx.QueryRowContext(ctx, `SELECT epoch FROM lock_epochs WHERE id = 1`).Scan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := w.exec(ctx, `INSERT INTO lock_epochs (id, epoch, write_id) VALUES (1, 1, ?)`, randomInt63()); err != nil {
			return 0, fmt.Errorf("first lock epoch: %w", err)
		}
		return 1, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read the lock epoch: %w", err)
	}
	if _, err := w.exec(ctx, `UPDATE lock_epochs SET epoch = ?, write_id = ? WHERE id = 1`, e+1, randomInt63()); err != nil {
		return 0, fmt.Errorf("next lock epoch: %w", err)
	}
	return e + 1, nil
}

// extendHold sets hold i of v to expire at exp, a renewal.
func (w *wtx) extendHold(ctx context.Context, v *lockView, i int, exp time.Time) error {
	if _, err := w.exec(ctx, `UPDATE lock_holders SET expires_at = ?, write_id = ? WHERE name = ? AND epoch = ?`,
		exp, randomInt63(), v.name, v.holds[i].Epoch); err != nil {
		return fmt.Errorf("renew hold of %s: %w", v.name, err)
	}
	v.holds[i].ExpiresAt = exp
	w.locked(LockRenewed, 0)
	return nil
}

// extendPlace sets place i of v to expire at exp, a renewal.
func (w *wtx) extendPlace(ctx context.Context, v *lockView, i int, exp time.Time) error {
	if _, err := w.exec(ctx, `UPDATE lock_waiters SET expires_at = ?, write_id = ? WHERE name = ? AND seq = ?`,
		exp, randomInt63(), v.name, v.places[i].seq); err != nil {
		return fmt.Errorf("renew place in %s: %w", v.name, err)
	}
	v.places[i].expires = exp
	w.locked(LockRenewed, 0)
	return nil
}

// stateOf is where actor stands with v: its hold, else its place.
func (v *lockView) stateOf(a Actor) LockState {
	st := LockState{Name: v.name, Capacity: v.capacity}
	if i := v.heldBy(a); i >= 0 {
		h := v.holds[i].LockHold
		st.Held = &h
	} else if i := v.placeOf(a); i >= 0 {
		st.Position, st.WaitExpires = i+1, v.places[i].expires
	}
	return st
}

// AcquireLock takes lock in.Name for actor (design §20.5). The lock is
// made by the first acquire, with in.Capacity (1 when 0), and a later one
// naming another capacity is refused with ErrInvalid. It is granted at
// once when fewer than its capacity hold it and nobody waits, for
// in.Lease (DefaultLease when 0, MinLease to MaxClaimLease), and the
// state returned has Held. Else, with in.Wait, actor joins the queue, and
// the state has its Position; without, it is refused with a
// [*LockBusyError]. An actor that holds the lock, or waits for it,
// already gets that hold, extended to in.Lease from now if that is
// later, or that place, renewed: a retry of an acquire whose answer was
// lost takes nothing twice. Past a limit it is refused with a
// [*LockLimitError], and a capacity past lock_capacity with ErrInvalid.
func (s *Store) AcquireLock(ctx context.Context, actor Actor, in LockAcquire) (LockState, error) {
	if err := checkLockName(in.Name); err != nil {
		return LockState{}, err
	}
	if in.Capacity < 0 || in.Capacity > s.opts.Limits.LockCapacity {
		return LockState{}, &LockCapacityError{Name: in.Name, Asked: in.Capacity, Max: s.opts.Limits.LockCapacity}
	}
	lease := cmp.Or(in.Lease, DefaultLease)
	if err := checkLease(lease, MaxClaimLease); err != nil {
		return LockState{}, err
	}
	var out LockState
	var refused error
	err := s.write(ctx, actor, func(w *wtx) error {
		out, refused = LockState{}, nil
		v, _, err := w.openLock(ctx, in.Name)
		if err != nil {
			return err
		}
		switch {
		case !v.exists:
			var n int
			if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM locks`).Scan(&n); err != nil {
				return fmt.Errorf("count locks: %w", err)
			}
			if n >= w.lim.Locks {
				refused = &LockLimitError{Name: in.Name, Limit: "locks", Max: w.lim.Locks}
				return nil
			}
			v.capacity, v.exists = cmp.Or(in.Capacity, 1), true
			if _, err := w.exec(ctx, `INSERT INTO locks (name, capacity, created_at, created_by, rev, write_id) VALUES (?, ?, ?, ?, 1, ?)`,
				in.Name, v.capacity, w.now, w.actor.Principal, randomInt63()); err != nil {
				return fmt.Errorf("make lock %s: %w", in.Name, err)
			}
		case in.Capacity != 0 && in.Capacity != v.capacity:
			refused = &LockCapacityError{Name: in.Name, Asked: in.Capacity, Capacity: v.capacity, Max: w.lim.LockCapacity}
			return nil
		}
		if i := v.heldBy(w.actor); i >= 0 {
			if exp := w.now.Add(lease); exp.After(v.holds[i].ExpiresAt) {
				if err := w.extendHold(ctx, v, i, exp); err != nil {
					return err
				}
			}
			w.quiet = true
			out = v.stateOf(w.actor)
			return nil
		}
		if i := v.placeOf(w.actor); i >= 0 {
			if err := w.extendPlace(ctx, v, i, w.now.Add(WaiterLease)); err != nil {
				return err
			}
			w.quiet = true
			out = v.stateOf(w.actor)
			return nil
		}
		if len(v.holds) < v.capacity && len(v.places) == 0 {
			if _, err := w.grant(ctx, v, w.actor, lease, nil); err != nil {
				return err
			}
			out = v.stateOf(w.actor)
			return nil
		}
		if !in.Wait {
			refused = &LockBusyError{Name: in.Name, Capacity: v.capacity, Holders: v.heldNow(), Waiters: len(v.places)}
			return nil
		}
		if len(v.places) >= w.lim.LockWaiters {
			refused = &LockLimitError{Name: in.Name, Limit: "lock_waiters", Max: w.lim.LockWaiters}
			return nil
		}
		var mine int
		if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lock_waiters WHERE principal = ? AND expires_at > ?`,
			w.actor.Principal, w.now).Scan(&mine); err != nil {
			return fmt.Errorf("count places: %w", err)
		}
		if mine >= w.lim.LockWaits {
			refused = &LockLimitError{Name: in.Name, Limit: "lock_waits_per_principal", Max: w.lim.LockWaits}
			return nil
		}
		// The queue's places are numbered from its last, so a later
		// place always sorts after an earlier one.
		var seq int64
		if n := len(v.places); n > 0 {
			seq = v.places[n-1].seq
		}
		p := placeRow{holder: w.actor, seq: seq + 1, joined: w.now, expires: w.now.Add(WaiterLease), lease: lease}
		if _, err := w.exec(ctx, `INSERT INTO lock_waiters
  (name, principal, session, machine, seq, joined_at, expires_at, lease_us, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.Name, p.holder.Principal, p.holder.Session, p.holder.Machine, p.seq, p.joined, p.expires,
			lease.Microseconds(), randomInt63()); err != nil {
			return fmt.Errorf("join the queue of %s: %w", in.Name, err)
		}
		v.places = append(v.places, p)
		w.locked(LockWaited, 0)
		out = v.stateOf(w.actor)
		return w.event(ctx, OpLockWait, LockTarget(in.Name), nil,
			map[string]any{"holder": w.actor, "position": out.Position, "expires_at": p.expires, "capacity": v.capacity})
	})
	if err != nil {
		return LockState{}, err
	}
	if refused != nil {
		return LockState{}, refused
	}
	return out, nil
}

// RenewLock renews actor's hold of lock name at epoch, for lease from now
// (the lease its acquire asked for when 0), or with epoch 0 actor's place
// in the queue, for WaiterLease. Renewals are not events. A hold is never
// shortened. A place that was granted meanwhile is renewed as the hold it
// became, and returned as Held: that is how a waiter learns of its grant
// without the push. A hold of another principal's, or one that ended or
// lapsed, is refused with a [*LockEpochError]; so is a place actor does
// not have (Place set).
func (s *Store) RenewLock(ctx context.Context, actor Actor, name string, epoch int64, lease time.Duration) (LockState, error) {
	if err := checkLockName(name); err != nil {
		return LockState{}, err
	}
	if epoch < 0 {
		return LockState{}, fmt.Errorf("%w: epoch %d", ErrInvalid, epoch)
	}
	if lease != 0 {
		if err := checkLease(lease, MaxClaimLease); err != nil {
			return LockState{}, err
		}
	}
	var out LockState
	var refused error
	err := s.write(ctx, actor, func(w *wtx) error {
		out, refused = LockState{}, nil
		v, _, err := w.openLock(ctx, name)
		if err != nil {
			return err
		}
		w.quiet = true
		i := v.holdOf(w.actor.Principal, epoch)
		if epoch == 0 {
			if p := v.placeOf(w.actor); p >= 0 {
				if err := w.extendPlace(ctx, v, p, w.now.Add(WaiterLease)); err != nil {
					return err
				}
				out = v.stateOf(w.actor)
				return nil
			}
			i = v.heldBy(w.actor)
		}
		if i < 0 {
			refused = &LockEpochError{Name: name, Epoch: epoch, Place: epoch == 0, Holders: v.heldNow()}
			return nil
		}
		if exp := w.now.Add(cmp.Or(lease, v.holds[i].lease)); exp.After(v.holds[i].ExpiresAt) {
			if err := w.extendHold(ctx, v, i, exp); err != nil {
				return err
			}
		}
		h := v.holds[i].LockHold
		out = LockState{Name: name, Capacity: v.capacity, Held: &h}
		return nil
	})
	if err != nil {
		return LockState{}, err
	}
	if refused != nil {
		return LockState{}, refused
	}
	return out, nil
}

// ReleaseLock ends actor's principal's hold of lock name at epoch, and
// grants the freed slot to the next live waiter in the same write. A
// hold that is not that principal's, or that ended or lapsed, is refused
// with a [*LockEpochError].
func (s *Store) ReleaseLock(ctx context.Context, actor Actor, name string, epoch int64) error {
	if err := checkLockName(name); err != nil {
		return err
	}
	var refused error
	err := s.write(ctx, actor, func(w *wtx) error {
		refused = nil
		v, _, err := w.openLock(ctx, name)
		if err != nil {
			return err
		}
		i := v.holdOf(w.actor.Principal, epoch)
		if epoch <= 0 || i < 0 {
			refused = &LockEpochError{Name: name, Epoch: epoch, Holders: v.heldNow()}
			return nil
		}
		if err := w.endHold(ctx, name, v.holds[i], OpLockRelease, ""); err != nil {
			return err
		}
		v.holds = slices.Delete(v.holds, i, i+1)
		if err := w.grantNext(ctx, v); err != nil {
			return err
		}
		return w.dropIfIdle(ctx, v)
	})
	if err != nil {
		return err
	}
	return refused
}

// LeaveLock gives up actor's place in lock name's queue. When actor holds
// the lock instead, or neither, it changes nothing and says which.
func (s *Store) LeaveLock(ctx context.Context, actor Actor, name string) (LockLeft, error) {
	if err := checkLockName(name); err != nil {
		return LockLeft{}, err
	}
	var out LockLeft
	err := s.write(ctx, actor, func(w *wtx) error {
		out = LockLeft{}
		v, _, err := w.openLock(ctx, name)
		if err != nil {
			return err
		}
		if i := v.placeOf(w.actor); i >= 0 {
			if err := w.endPlace(ctx, name, v.places[i], ""); err != nil {
				return err
			}
			v.places = slices.Delete(v.places, i, i+1)
			out.Left = true
			return w.dropIfIdle(ctx, v)
		}
		out.Held = v.stateOf(w.actor).Held
		return nil
	})
	return out, err
}

// Locks returns lock name, or with "" every lock, by name: the live holds
// and places of each, and no lock that has neither.
func (s *Store) Locks(ctx context.Context, name string) ([]Lock, error) {
	where, args := `TRUE`, []any{}
	if name != "" {
		if err := checkLockName(name); err != nil {
			return nil, err
		}
		where, args = `name = ?`, []any{name}
	}
	now := s.now()
	q, done, err := s.beginRead(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := q.QueryContext(ctx, `SELECT name, capacity FROM locks WHERE `+where+` ORDER BY name`, args...) //nolint:gosec // where is a constant
	if err != nil {
		return nil, fmt.Errorf("locks: %w", err)
	}
	var out []Lock
	at := map[string]int{}
	for rows.Next() {
		var l Lock
		if err := rows.Scan(&l.Name, &l.Capacity); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("locks: %w", err)
		}
		at[l.Name] = len(out)
		out = append(out, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("locks: %w", err)
	}
	live := append([]any{now}, args...)
	holds, err := loadNamedHolds(ctx, q, `expires_at > ? AND `+where, live...)
	if err != nil {
		return nil, err
	}
	for _, h := range holds {
		if i, ok := at[h.name]; ok {
			out[i].Holders = append(out[i].Holders, h.LockHold)
		}
	}
	places, err := loadNamedPlaces(ctx, q, `expires_at > ? AND `+where, live...)
	if err != nil {
		return nil, err
	}
	for _, p := range places {
		if i, ok := at[p.name]; ok {
			l := &out[i]
			l.Waiters = append(l.Waiters, LockPlace{Holder: p.holder, Position: len(l.Waiters) + 1, JoinedAt: p.joined, ExpiresAt: p.expires})
		}
	}
	return slices.DeleteFunc(out, func(l Lock) bool { return len(l.Holders) == 0 && len(l.Waiters) == 0 }), nil
}

// namedHold and namedPlace are a hold and a place with their lock's name,
// for reads across locks.
type (
	namedHold struct {
		name string
		holdRow
	}
	namedPlace struct {
		name string
		placeRow
	}
)

func loadNamedHolds(ctx context.Context, q querier, where string, args ...any) ([]namedHold, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, epoch, principal, session, machine, granted_at, expires_at, lease_us
  FROM lock_holders WHERE `+where+` ORDER BY name, epoch`, args...) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("lock holders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []namedHold
	for rows.Next() {
		var h namedHold
		var us int64
		if err := rows.Scan(&h.name, &h.Epoch, &h.Holder.Principal, &h.Holder.Session, &h.Holder.Machine, &h.GrantedAt, &h.ExpiresAt, &us); err != nil {
			return nil, fmt.Errorf("lock holders: %w", err)
		}
		h.GrantedAt, h.ExpiresAt, h.lease = h.GrantedAt.UTC(), h.ExpiresAt.UTC(), time.Duration(us)*time.Microsecond
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock holders: %w", err)
	}
	return out, nil
}

func loadNamedPlaces(ctx context.Context, q querier, where string, args ...any) ([]namedPlace, error) {
	rows, err := q.QueryContext(ctx, `SELECT name, seq, principal, session, machine, joined_at, expires_at, lease_us
  FROM lock_waiters WHERE `+where+` ORDER BY name, seq`, args...) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("lock waiters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []namedPlace
	for rows.Next() {
		var p namedPlace
		var us int64
		if err := rows.Scan(&p.name, &p.seq, &p.holder.Principal, &p.holder.Session, &p.holder.Machine, &p.joined, &p.expires, &us); err != nil {
			return nil, fmt.Errorf("lock waiters: %w", err)
		}
		p.joined, p.expires, p.lease = p.joined.UTC(), p.expires.UTC(), time.Duration(us)*time.Microsecond
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock waiters: %w", err)
	}
	return out, nil
}

// distinct reads the distinct strings that query, a constant query
// selecting one string column with placeholders for args, returns, sorted.
func distinct(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read locks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("read locks: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read locks: %w", err)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// ReapLocks ends, in every lock, the holds whose lease ran out
// (lock.expire, and lock.lost to the holder's session) and the places
// whose lease ran out (lock.leave), and grants the slots freed to the
// next live waiters (lock.grant, and lock.granted to each). It returns
// what it ended. The reaper runs it on its tick.
func (s *Store) ReapLocks(ctx context.Context) ([]LockEnd, error) {
	const lapsed = `SELECT name FROM lock_holders WHERE expires_at <= ?
  UNION SELECT name FROM lock_waiters WHERE expires_at <= ?`
	// Most ticks find nothing lapsed, and that is a read: it does not
	// wait for the writer. What lapses after it is the next tick's.
	now := s.now()
	if names, err := distinct(ctx, s.r, lapsed, now, now); err != nil || len(names) == 0 {
		return nil, err
	}
	var out []LockEnd
	err := s.write(ctx, ReaperActor, func(w *wtx) error {
		out = nil
		names, err := distinct(ctx, w.tx, lapsed, w.now, w.now)
		if err != nil {
			return err
		}
		for _, name := range names {
			_, ends, err := w.openLock(ctx, name)
			if err != nil {
				return err
			}
			out = append(out, ends...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// EndLocks ends the holds and places of the principals who lost their
// access to the project (decision 21): why is asked for each principal
// that holds or waits for a lock, and a reason it returns ends that
// principal's live holds and places, as actor; the lapsed ones end as the
// reaper ends them. A hold ends with a lock.release whose after state
// gives the reason, and a lock.lost item for the session that held it; a
// place with a lock.leave that gives it. The slots freed go to the next
// live waiters. It returns what it ended for a reason. why runs inside
// the write, which may be retried, so it must be fast, read no database
// and give the same answer again.
//
// It is for the daemon, as EndClaims is.
func (s *Store) EndLocks(ctx context.Context, actor Actor, why func(principal string) string) ([]LockEnd, error) {
	// Most sweeps find nobody to end, and a read says so without the
	// writer. A hold or place taken after it, by a request already under
	// way when the access ended, the next sweep ends.
	who, err := distinct(ctx, s.r, `SELECT principal FROM lock_holders UNION SELECT principal FROM lock_waiters`)
	if err != nil {
		return nil, err
	}
	if !slices.ContainsFunc(who, func(p string) bool { return why(p) != "" }) {
		return nil, nil
	}
	var out []LockEnd
	err = s.write(ctx, actor, func(w *wtx) error {
		out = nil
		holds, err := loadNamedHolds(ctx, w.tx, `TRUE`)
		if err != nil {
			return err
		}
		places, err := loadNamedPlaces(ctx, w.tx, `TRUE`)
		if err != nil {
			return err
		}
		var names []string
		for _, h := range holds {
			if why(h.Holder.Principal) != "" {
				names = append(names, h.name)
			}
		}
		for _, p := range places {
			if why(p.holder.Principal) != "" {
				names = append(names, p.name)
			}
		}
		slices.Sort(names)
		for _, name := range slices.Compact(names) {
			v, _, err := w.openLock(ctx, name)
			if err != nil {
				return err
			}
			kept := v.holds[:0]
			for _, h := range v.holds {
				reason := why(h.Holder.Principal)
				if reason == "" {
					kept = append(kept, h)
					continue
				}
				if err := w.endHold(ctx, name, h, OpLockRelease, reason); err != nil {
					return err
				}
				out = append(out, LockEnd{Name: name, Holder: h.Holder, Epoch: h.Epoch})
			}
			v.holds = kept
			waiting := v.places[:0]
			for _, p := range v.places {
				reason := why(p.holder.Principal)
				if reason == "" {
					waiting = append(waiting, p)
					continue
				}
				if err := w.endPlace(ctx, name, p, reason); err != nil {
					return err
				}
				out = append(out, LockEnd{Name: name, Holder: p.holder, Waiting: true})
			}
			v.places = waiting
			if err := w.grantNext(ctx, v); err != nil {
				return err
			}
			if err := w.dropIfIdle(ctx, v); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// extendLocks adds d to every hold and place live at w's time, for a
// settled forward step of the clock, and returns how many it extended.
func extendLocks(ctx context.Context, w *wtx, d time.Duration) (int, error) {
	holds, err := loadNamedHolds(ctx, w.tx, `expires_at > ?`, w.now)
	if err != nil {
		return 0, err
	}
	for _, h := range holds {
		if _, err := w.exec(ctx, `UPDATE lock_holders SET expires_at = ?, write_id = ? WHERE name = ? AND epoch = ?`,
			h.ExpiresAt.Add(d), randomInt63(), h.name, h.Epoch); err != nil {
			return 0, fmt.Errorf("extend hold of %s: %w", h.name, err)
		}
	}
	places, err := loadNamedPlaces(ctx, w.tx, `expires_at > ?`, w.now)
	if err != nil {
		return 0, err
	}
	for _, p := range places {
		if _, err := w.exec(ctx, `UPDATE lock_waiters SET expires_at = ?, write_id = ? WHERE name = ? AND seq = ?`,
			p.expires.Add(d), randomInt63(), p.name, p.seq); err != nil {
			return 0, fmt.Errorf("extend place in %s: %w", p.name, err)
		}
	}
	return len(holds) + len(places), nil
}
