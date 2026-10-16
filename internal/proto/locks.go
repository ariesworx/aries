package proto

import "time"

// Locks (design §20.5; stage 6, slice G2; protocol 8). A lock is a name
// in the project held by up to its capacity of holders, each under a
// lease and an epoch that fences it. No MCP tool takes one (decision
// 70); `aries lock` and the protocol do.
const (
	OpLockAcquire = "lock.acquire" // LockAcquireArgs → LockResult
	OpLockRenew   = "lock.renew"   // LockRenewArgs → LockResult
	OpLockRelease = "lock.release" // LockReleaseArgs → LockReleaseResult
	OpLockLeave   = "lock.leave"   // LockLeaveArgs → LockLeaveResult
	OpLocks       = "locks"        // LocksArgs → LocksResult
)

// LockEpochEnv is the variable `aries lock run` gives the command it runs:
// the hold's epoch, for a resource that checks it.
const LockEpochEnv = "ARIES_LOCK_EPOCH"

// LockAcquireArgs takes lock Name: at once when fewer than its capacity
// hold it and nobody waits, else with Wait a place in its queue, else
// refused busy. Capacity is set by the lock's first acquire, 1 when
// omitted; a later one naming another is refused. Lease is the hold's,
// as a duration (15m when omitted, 1m to 24h).
type LockAcquireArgs struct {
	Name     string `json:"name"`
	Capacity int    `json:"capacity,omitempty"`
	Lease    string `json:"lease,omitempty"`
	Wait     bool   `json:"wait,omitempty"`
}

// LockRenewArgs renews the caller's hold of Name at Epoch, for Lease from
// now (the acquire's when omitted), or with Epoch 0 its place in the
// queue; a place granted meanwhile is answered as the hold it became.
type LockRenewArgs struct {
	Name  string `json:"name"`
	Epoch int64  `json:"epoch,omitempty"`
	Lease string `json:"lease,omitempty"`
}

// LockReleaseArgs releases the caller's hold of Name at Epoch.
type LockReleaseArgs struct {
	Name  string `json:"name"`
	Epoch int64  `json:"epoch"`
}

// LockLeaveArgs gives up the caller's place in Name's queue.
type LockLeaveArgs struct {
	Name string `json:"name"`
}

// LocksArgs lists lock Name, or every lock in use when it is empty.
type LocksArgs struct {
	Name string `json:"name,omitempty"`
}

// LockResult is where the caller stands with a lock: Epoch and ExpiresAt
// when it holds it, else Position in the queue, from 1, and WaitExpires,
// when the place lapses unless renewed.
type LockResult struct {
	Name        string    `json:"name"`
	Capacity    int       `json:"capacity"`
	Held        bool      `json:"held"`
	Epoch       int64     `json:"epoch,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	Position    int       `json:"position,omitempty"`
	WaitExpires time.Time `json:"wait_expires,omitzero"`
}

// LockReleaseResult says a hold was released: Epoch is the hold's.
type LockReleaseResult struct {
	Name  string `json:"name"`
	Epoch int64  `json:"epoch"`
}

// LockLeaveResult says whether a place was given up; when the caller
// holds the lock instead, Epoch is its hold's.
type LockLeaveResult struct {
	Name  string `json:"name"`
	Left  bool   `json:"left"`
	Epoch int64  `json:"epoch,omitempty"`
}

// LockHolder is one hold of a lock, or one place in its queue: who, and
// until when. Epoch is a hold's, and Position a place's.
type LockHolder struct {
	By        string    `json:"by"`
	Session   string    `json:"session"`
	Machine   string    `json:"machine"`
	Epoch     int64     `json:"epoch,omitempty"`
	Position  int       `json:"position,omitempty"`
	Since     time.Time `json:"since"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Lock is a lock in use: its live holds, oldest grant first, and its live
// places, in the order they will be served.
type Lock struct {
	Name     string       `json:"name"`
	Capacity int          `json:"capacity"`
	Holders  []LockHolder `json:"holders"`
	Waiters  []LockHolder `json:"waiters,omitempty"`
}

// LocksResult lists locks in use, by name, and the server's time.
type LocksResult struct {
	Locks []Lock    `json:"locks"`
	Now   time.Time `json:"now"`
}
