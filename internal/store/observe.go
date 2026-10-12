package store

import "time"

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
}

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
