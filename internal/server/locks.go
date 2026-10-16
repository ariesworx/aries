package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// Locks (design §20.5; stage 6, slice G2). The rules are the store's
// (store/locks.go); here they are put on the wire, and each refusal is
// given its next step (rule 11). A lock's name is a person's text: it is
// on no span and no metric, and stays in the journal (logs.KindLocal).

func lockAcquire(ctx context.Context, pr *served, a store.Actor, in proto.LockAcquireArgs) (any, *proto.Error) {
	if perr := lockName(proto.OpLockAcquire, in.Name); perr != nil {
		return nil, perr
	}
	d, perr := lease(proto.OpLockAcquire, in.Lease)
	if perr != nil {
		return nil, perr
	}
	st, err := pr.store.AcquireLock(ctx, a, store.LockAcquire{Name: in.Name, Capacity: in.Capacity, Lease: d, Wait: in.Wait})
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpLockAcquire, in.Name, 0, err)
	}
	return wireLockState(st), nil
}

func lockRenew(ctx context.Context, pr *served, a store.Actor, in proto.LockRenewArgs) (any, *proto.Error) {
	if perr := lockName(proto.OpLockRenew, in.Name); perr != nil {
		return nil, perr
	}
	var d time.Duration
	if in.Lease != "" {
		var perr *proto.Error
		if d, perr = lease(proto.OpLockRenew, in.Lease); perr != nil {
			return nil, perr
		}
	}
	st, err := pr.store.RenewLock(ctx, a, in.Name, in.Epoch, d)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpLockRenew, in.Name, 0, err)
	}
	return wireLockState(st), nil
}

func lockRelease(ctx context.Context, pr *served, a store.Actor, in proto.LockReleaseArgs) (any, *proto.Error) {
	if perr := lockName(proto.OpLockRelease, in.Name); perr != nil {
		return nil, perr
	}
	if err := pr.store.ReleaseLock(ctx, a, in.Name, in.Epoch); err != nil {
		return nil, pr.mapErr(ctx, proto.OpLockRelease, in.Name, 0, err)
	}
	return proto.LockReleaseResult(in), nil
}

func lockLeave(ctx context.Context, pr *served, a store.Actor, in proto.LockLeaveArgs) (any, *proto.Error) {
	if perr := lockName(proto.OpLockLeave, in.Name); perr != nil {
		return nil, perr
	}
	l, err := pr.store.LeaveLock(ctx, a, in.Name)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpLockLeave, in.Name, 0, err)
	}
	out := proto.LockLeaveResult{Name: in.Name, Left: l.Left}
	if l.Held != nil {
		out.Epoch = l.Held.Epoch
	}
	return out, nil
}

func locks(ctx context.Context, pr *served, _ store.Actor, in proto.LocksArgs) (any, *proto.Error) {
	if in.Name != "" {
		if perr := lockName(proto.OpLocks, in.Name); perr != nil {
			return nil, perr
		}
	}
	ls, err := pr.store.Locks(ctx, in.Name)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpLocks, in.Name, 0, err)
	}
	out := proto.LocksResult{Locks: make([]proto.Lock, 0, len(ls)), Now: pr.store.Now().UTC()}
	for _, l := range ls {
		w := proto.Lock{Name: l.Name, Capacity: l.Capacity, Holders: wireHolds(l.Holders)}
		for _, p := range l.Waiters {
			w.Waiters = append(w.Waiters, proto.LockHolder{By: p.Holder.Principal, Session: p.Holder.Session, Machine: p.Holder.Machine,
				Position: p.Position, Since: p.JoinedAt.UTC(), ExpiresAt: p.ExpiresAt.UTC()})
		}
		out.Locks = append(out.Locks, w)
	}
	return out, nil
}

func wireHolds(hs []store.LockHold) []proto.LockHolder {
	out := make([]proto.LockHolder, 0, len(hs))
	for _, h := range hs {
		out = append(out, proto.LockHolder{By: h.Holder.Principal, Session: h.Holder.Session, Machine: h.Holder.Machine,
			Epoch: h.Epoch, Since: h.GrantedAt.UTC(), ExpiresAt: h.ExpiresAt.UTC()})
	}
	return out
}

func wireLockState(st store.LockState) proto.LockResult {
	out := proto.LockResult{Name: st.Name, Capacity: st.Capacity, Position: st.Position, WaitExpires: st.WaitExpires.UTC()}
	if st.Held != nil {
		out.Held, out.Epoch, out.ExpiresAt = true, st.Held.Epoch, st.Held.ExpiresAt.UTC()
	}
	return out
}

// holdersText names who holds a lock and until when, for a refusal.
func holdersText(hs []store.LockHold) string {
	if len(hs) == 0 {
		return "nobody"
	}
	parts := make([]string, 0, len(hs))
	for _, h := range hs {
		parts = append(parts, fmt.Sprintf("%s (session %s, epoch %d) until %s", h.Holder.Principal, h.Holder.Session, h.Epoch,
			h.ExpiresAt.UTC().Format(time.RFC3339)))
	}
	return strings.Join(parts, ", ")
}

// lockErr words a refusal of a lock op, or returns nil for an error that
// is none.
func lockErr(err error) *proto.Error {
	if e, ok := errors.AsType[*store.LockBusyError](err); ok {
		return lockBusyErr(e)
	}
	if e, ok := errors.AsType[*store.LockEpochError](err); ok {
		return lockEpochErr(e)
	}
	if e, ok := errors.AsType[*store.LockLimitError](err); ok {
		return lockLimitErr(e)
	}
	if e, ok := errors.AsType[*store.LockCapacityError](err); ok {
		return lockCapacityErr(e)
	}
	return nil
}

// lockBusyErr refuses an acquire of a full lock, naming its holders and
// when their leases end.
func lockBusyErr(e *store.LockBusyError) *proto.Error {
	msg := fmt.Sprintf("lock %s is held by %s", e.Name, holdersText(e.Holders))
	if e.Waiters > 0 {
		msg += fmt.Sprintf("; %d waiting", e.Waiters)
	}
	return proto.Errf(proto.CodeBusy,
		fmt.Sprintf("wait for it in its queue: `aries lock acquire %s --wait`, or `aries lock run %s --wait -- CMD`; or retry once a lease ends", e.Name, e.Name),
		msg)
}

// lockEpochErr refuses a renewal or release of a hold, or a place, the
// caller does not have.
func lockEpochErr(e *store.LockEpochError) *proto.Error {
	held := ""
	if len(e.Holders) > 0 {
		held = "; held by " + holdersText(e.Holders)
	}
	if e.Place {
		return proto.Errf(proto.CodeConflict,
			fmt.Sprintf("acquire it again to wait (`aries lock acquire %s --wait`): a place lapses 2m after its last renewal", e.Name),
			fmt.Sprintf("lock %s: no place in its queue is yours%s", e.Name, held))
	}
	return proto.Errf(proto.CodeConflict,
		fmt.Sprintf("stop what the lock guards: your hold ended, and a resource that checks %s refuses epoch %d; acquire it again if it is still needed",
			proto.LockEpochEnv, e.Epoch),
		fmt.Sprintf("lock %s: no hold at epoch %d is yours%s", e.Name, e.Epoch, held))
}

// lockCapacityErr refuses a capacity that is not the lock's, or past the
// limit.
func lockCapacityErr(e *store.LockCapacityError) *proto.Error {
	msg := strings.TrimPrefix(e.Error(), store.ErrInvalid.Error()+": ")
	if e.Capacity == 0 {
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("give --capacity from 1 to %d, or ask the server admin to raise holders_per_lock under limits: in ariesd's config", e.Max), msg)
	}
	return proto.Errf(proto.CodeInvalid,
		fmt.Sprintf("leave --capacity out, or give --capacity %d: a lock keeps the capacity its first acquire gave it until nobody holds or waits for it", e.Capacity),
		msg)
}

// lockName refuses a name no lock may have, before the store is asked.
func lockName(op, name string) *proto.Error {
	if store.ValidLockName(name) {
		return nil
	}
	return proto.Errf(proto.CodeInvalid,
		fmt.Sprintf("name a lock with a lowercase letter or digit, then lowercase letters, digits and . _ / -, at most %d in all, such as deploy/prod; `aries %s -h` shows the usage",
			store.MaxLockName, command(op)),
		fmt.Sprintf("lock name %q is not one a lock may have", name))
}

// lockLimitErr refuses an acquire past one of the lock limits.
func lockLimitErr(e *store.LockLimitError) *proto.Error {
	fix := fmt.Sprintf("ask the server admin to raise %s under limits: in ariesd's config", e.Limit)
	switch e.Limit {
	case "locks_per_project":
		fix = "release the locks nobody needs (`aries lock list` shows them, and a lock nobody holds or waits for is gone), or " + fix
	case "waiters_per_lock":
		fix = "retry later without --wait, or " + fix
	case "lock_waits_per_principal":
		fix = "leave a queue you no longer need (`aries lock leave NAME`), or " + fix
	}
	return proto.Errf(proto.CodeBusy, fix, strings.TrimPrefix(e.Error(), store.ErrBusy.Error()+": "))
}

// reapLocks ends the project's lapsed holds and places now, grants the
// queues, and logs each end.
func (pr *served) reapLocks(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	ends, err := pr.store.ReapLocks(rctx)
	if err != nil {
		if ctx.Err() == nil {
			pr.log.ErrorContext(ctx, "reap locks", "event", logs.ReaperFailed, "err", err)
		}
		return
	}
	for _, e := range ends {
		pr.log.InfoContext(ctx, "lock expired", "event", logs.LockExpired, "lock", e.Name, "principal", e.Holder.Principal,
			"session", e.Holder.Session, "epoch", e.Epoch, "waiting", e.Waiting)
	}
}

// endLocks ends the holds and places in pr that why gives a reason for,
// as by, logs each, and returns how many.
func (pr *served) endLocks(ctx context.Context, by store.Actor, why func(holder string) string) int {
	ectx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	ends, err := pr.store.EndLocks(ectx, by, why)
	if err != nil {
		if ctx.Err() == nil {
			pr.log.ErrorContext(ctx, "end the locks of holders without access", "event", logs.SweepFailed, "err", err)
		}
		return 0
	}
	for _, e := range ends {
		pr.log.InfoContext(ctx, "lock released: access ended", "event", logs.LockReleased, "lock", e.Name, "principal", e.Holder.Principal,
			"session", e.Holder.Session, "epoch", e.Epoch, "waiting", e.Waiting, "by", by.Principal, "reason", why(e.Holder.Principal))
	}
	return len(ends)
}
