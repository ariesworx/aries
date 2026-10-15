package store

import (
	"context"
	"time"
)

// Observer is told what a store's writer did, as it does it, so that the
// daemon can measure a store without the store knowing how (design
// §16.4): starfixd's metrics are built on it. Every method is called on
// the goroutine that did the work, after the work is done and outside
// any transaction, so an implementation must return at once, read no
// database, and be safe for concurrent use.
type Observer interface {
	// Wrote reports a write that changed a row and committed: took is
	// its latency as [WriteStats] measures it, from the request reaching
	// the store to its commit, with the wait for the single writer and
	// any retry.
	Wrote(took time.Duration)
	// Retried reports one attempt of a write that lost to a concurrent
	// transaction and is tried again.
	Retried()
	// Committed reports a Dolt commit of pending writes: how long it
	// took, and its error if it failed and the writes stay pending.
	Committed(took time.Duration, err error)
	// ClaimChanged reports a change of a claim that committed. reason is
	// why a claim was released, when its claim.release event gives a
	// reason ([Store.EndClaims]), and empty for every other change.
	ClaimChanged(change ClaimChange, reason string)
	// LockChanged reports a change of a lock's hold or queue that
	// committed. waited is, for LockGranted, how long the place waited
	// on the store's clock, and zero otherwise.
	LockChanged(change LockChange, waited time.Duration)
}

// Tracer is told where a write's time goes, as it goes, under the
// context of the request that asked for it, so that the daemon can
// trace a store without the store knowing how (design §16.4): starfixd's
// spans below a request's are built on it. It is told of nothing an
// [Observer] measures, and changes none of those figures.
type Tracer interface {
	// Begin marks the start of stage, under ctx. It returns the context
	// the stage's work runs under, which a stage begun inside it is
	// given, and a func the store calls once, when the stage ends, with
	// how it ended. attempt numbers a write's attempts from 1, and is 0
	// for every other stage.
	//
	// Both are called on the goroutine doing the work, with the writer
	// held for part of it, so an implementation must return at once,
	// read no database, and be safe for concurrent use. It is given
	// nothing of what is written, and no error: an error's text may
	// quote a row.
	Begin(ctx context.Context, stage Stage, attempt int) (context.Context, func(Outcome))
}

// Stage is a part of the store's work a [Tracer] is told of.
type Stage string

// The stages.
const (
	// StageAttempt is one attempt of a write, from the wait for the
	// writer to its commit or rollback. A write that loses to a
	// concurrent transaction is attempted again, as a new stage.
	StageAttempt Stage = "write.attempt"
	// StageWait is, within an attempt, the wait for the store's one
	// writer connection and the start of its transaction.
	StageWait Stage = "write.wait"
	// StageCommit is a Dolt commit of pending writes.
	StageCommit Stage = "commit"
	// StageMigrate is, within an open, the check of the schema and the
	// migrations it applies.
	StageMigrate Stage = "migrate"
)

// Outcome is how a [Stage] ended.
type Outcome string

// The outcomes. OutcomeRetry is an attempt that lost to a concurrent
// transaction. OutcomeError is every other error, which for an attempt
// is most often the request's own refusal.
const (
	OutcomeOK    Outcome = "ok"
	OutcomeRetry Outcome = "retry"
	OutcomeError Outcome = "error"
)

// outcomeOf is how a stage that returned err ended.
func outcomeOf(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeOK
	case retryable(err):
		return OutcomeRetry
	}
	return OutcomeError
}

// begin tells the store's tracer, if it has one, that stage starts.
func (s *Store) begin(ctx context.Context, stage Stage, attempt int) (context.Context, func(Outcome)) {
	if s.opts.Tracer == nil {
		return ctx, untraced
	}
	return s.opts.Tracer.Begin(ctx, stage, attempt)
}

// untraced ends a stage nobody was told of.
func untraced(Outcome) {}

// ClaimChange is what happened to a claim, as an [Observer] is told.
type ClaimChange string

// The changes of a claim. They are the store's events (OpClaimTake,
// OpClaimRelease, OpClaimExpire) and one that is not an event:
// ClaimRenewed is a lease that a renew extended. A session that starts
// an issue it already holds extends its lease too, and that is neither
// an event nor reported.
const (
	ClaimTaken    ClaimChange = "take"
	ClaimRenewed  ClaimChange = "renew"
	ClaimReleased ClaimChange = "release"
	ClaimExpired  ClaimChange = "expire"
)

// claimChange is one change of a claim an attempt made, reported once
// the attempt commits.
type claimChange struct {
	change ClaimChange
	reason string
}

// claimed notes a change of a claim for the store's observer.
func (w *wtx) claimed(change ClaimChange, reason string) {
	w.claims = append(w.claims, claimChange{change, reason})
}

// LockChange is what happened to a lock's hold or queue, as an
// [Observer] is told. They are the lock events, and LockRenewed, a hold
// or a place a renewal or an acquire again extended, which is not one.
type LockChange string

// The changes of a lock.
const (
	LockAcquired  LockChange = "acquire"
	LockWaited    LockChange = "wait"
	LockGranted   LockChange = "grant"
	LockRenewed   LockChange = "renew"
	LockReleased  LockChange = "release"
	LockExpired   LockChange = "expire"
	LockWithdrawn LockChange = "leave"
)

// lockChange is one change of a lock an attempt made, reported once the
// attempt commits.
type lockChange struct {
	change LockChange
	waited time.Duration
}

// locked notes a change of a lock for the store's observer.
func (w *wtx) locked(change LockChange, waited time.Duration) {
	w.locks = append(w.locks, lockChange{change, waited})
}
