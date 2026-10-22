package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Observer is told what a store's writer did, as it does it, so that the
// daemon can measure a store without the store knowing how (design
// §16.4): ariesd's metrics are built on it. Every method is called on
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
	// ClaimChanged reports a change of a claim that committed. reason is
	// why a claim was released, when its claim.release event gives a
	// reason ([Store.EndClaims]), and empty for every other change.
	ClaimChanged(change ClaimChange, reason string)
	// LockChanged reports a change of a lock's hold or queue that
	// committed. waited is, for LockGranted, how long the place waited
	// on the store's clock, and zero otherwise.
	LockChanged(change LockChange, waited time.Duration)
	// GateChanged reports a change of a gate that committed, made by
	// actor, with the gate as the change left it, under the context of
	// the request that made it: a report or a decision that changed
	// nothing is not one.
	GateChanged(ctx context.Context, actor Actor, g Gate, change GateChange)
	// UsageAdded reports the tokens a usage write that committed added,
	// summed for each model, under the context of the request that made
	// it. A record the store already had adds nothing, and a write
	// that stored no row is not reported. The model is [UsageOther]
	// unless the model has a price in the store, so its values are
	// bounded by the table and not by what clients report.
	UsageAdded(ctx context.Context, actor Actor, u []UsageTokens)
	// IssueCycled reports a close that committed of an issue of the
	// given type that had been started, with how long it took from the
	// issue's first start since it was last opened to the close, on the
	// store's clock. A close of an issue never started is not reported.
	IssueCycled(issueType IssueType, took time.Duration)
	// IssueQueued reports an issue's first start that committed, with how
	// long it waited, on the store's clock, from its creation, or from
	// its defer_until if that is later. When it became ready is not
	// used: ready is derived and no event records it.
	IssueQueued(issueType IssueType, took time.Duration)
	// IssueLanded reports a close by the closing pass of a required
	// check that committed, with how long it was from the issue's last
	// handoff with state done to the close, on the store's clock. An
	// issue with no such handoff is not reported.
	IssueLanded(issueType IssueType, took time.Duration)
	// GateWaited reports a gate's pass or waiver that committed, with
	// how long the gate was open on the store's clock, from its add or
	// its last rearm.
	GateWaited(kind GateKind, took time.Duration)
}

// Tracer is told where a write's time goes, as it goes, under the
// context of the request that asked for it, so that the daemon can
// trace a store without the store knowing how (design §16.4): ariesd's
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

// GateChange is what happened to a gate, as an [Observer] is told: its
// gate events (OpGateAdd, OpGatePass, OpGateFail, OpGateWaive,
// OpGateRearm). A pass is a person's or a CI job's; a failure, a CI
// job's report of its check.
type GateChange string

// The changes of a gate.
const (
	GateChangeAdded   GateChange = "added"
	GateChangePassed  GateChange = "passed"
	GateChangeFailed  GateChange = "failed"
	GateChangeWaived  GateChange = "waived"
	GateChangeRearmed GateChange = "rearmed"
)

// gateChange is one change of a gate an attempt made, reported once the
// attempt commits.
type gateChange struct {
	gate   Gate
	change GateChange
}

// gated notes a change of gate g for the store's observer.
func (w *wtx) gated(g Gate, change GateChange) {
	w.gates = append(w.gates, gateChange{g, change})
}

// UsageOther is the model of a usage record whose model has no price in
// the store, as an [Observer] is told of it. No reported model can be
// this, which starts with a letter or digit (proto.UsageModel).
const UsageOther = "(other)"

// UsageTokens are the tokens of a usage write's new rows for one model,
// as an [Observer] is told of them. A count that a record did not
// report adds nothing. CacheWrite includes the part written for an
// hour, which CacheWrite1h counts alone.
type UsageTokens struct {
	Model                                string
	Input, Output, CacheWrite, CacheRead int64
	// CacheWrite1h is the part of CacheWrite written for an hour.
	CacheWrite1h int64
}

// cycle is one start-to-close time an attempt found, reported once the
// attempt commits.
type cycle struct {
	typ  IssueType
	took time.Duration
}

// cycled notes, for the store's observer, how long the issue just closed
// took from its first start since its last reopening: the first
// claim.take after the latest issue.reopen, which the events_target
// index serves. An issue closed without ever being started has none and
// notes nothing.
func (w *wtx) cycled(ctx context.Context, is Issue) error {
	var start sql.NullTime
	err := w.tx.QueryRowContext(ctx, `SELECT min(at) FROM events WHERE target = $1 AND op = $2
  AND seq > COALESCE((SELECT max(seq) FROM events WHERE target = $1 AND op = $3), 0)`,
		string(is.ID), string(OpClaimTake), string(OpIssueReopen)).Scan(&start)
	if err != nil {
		return fmt.Errorf("start of an issue: %w", err)
	}
	if start.Valid {
		w.cycles = append(w.cycles, cycle{is.Type, max(w.now.Sub(start.Time), 0)})
	}
	return nil
}

// stage is one wait of an issue or a gate an attempt found, reported once
// the attempt commits: kind is "queue", "landing" or "gate", and typ is
// the issue's type, or for a gate its kind.
type stage struct {
	kind string
	typ  string
	took time.Duration
}

// queued notes, for the store's observer, how long issue id waited for
// its first start, when the take being written is its first: from its
// creation, or its defer_until if later, to now. It must run before the
// take's event is written. A later take notes nothing.
func (w *wtx) queued(ctx context.Context, id IssueID) error {
	var typ IssueType
	var created time.Time
	var deferred sql.NullTime
	var taken int
	err := w.tx.QueryRowContext(ctx, `SELECT i.type, i.created_at, i.defer_until,
  (SELECT count(*) FROM events WHERE target = $1 AND op = $2) FROM issues i WHERE i.id = $1`,
		string(id), string(OpClaimTake)).Scan(&typ, &created, &deferred, &taken)
	if err != nil {
		return fmt.Errorf("queue of an issue: %w", err)
	}
	if taken > 0 {
		return nil
	}
	if deferred.Valid && deferred.Time.After(created) {
		created = deferred.Time
	}
	w.stages = append(w.stages, stage{"queue", string(typ), max(w.now.Sub(created), 0)})
	return nil
}

// landedAfter notes, for the store's observer, how long the issue the
// closing pass just closed had waited since its last handoff with state
// done.
func (w *wtx) landedAfter(ctx context.Context, is Issue) error {
	var at sql.NullTime
	err := w.tx.QueryRowContext(ctx, `SELECT max(c.created_at) FROM handoffs h JOIN comments c ON c.id = h.comment_id
  WHERE h.issue_id = $1 AND h.state = $2`, string(is.ID), string(HandoffDone)).Scan(&at)
	if err != nil {
		return fmt.Errorf("last handoff of an issue: %w", err)
	}
	if at.Valid {
		w.stages = append(w.stages, stage{"landing", string(is.Type), max(w.now.Sub(at.Time), 0)})
	}
	return nil
}

// gateWaited notes, for the store's observer, how long gate g was open
// when it was passed or waived: since its add or its last rearm, which
// the events of the gate's issue give.
func (w *wtx) gateWaited(ctx context.Context, g Gate) error {
	var at sql.NullTime
	err := w.tx.QueryRowContext(ctx, `SELECT max(at) FROM events WHERE target = $1 AND op IN ($2, $3)`,
		string(g.ID), string(OpGateAdd), string(OpGateRearm)).Scan(&at)
	if err != nil {
		return fmt.Errorf("start of a gate's wait: %w", err)
	}
	if at.Valid {
		w.stages = append(w.stages, stage{"gate", string(g.Kind), max(w.now.Sub(at.Time), 0)})
	}
	return nil
}

// StatusCounts returns how many issues the project has in each status,
// from one grouped count that the status index serves: at most one row a
// status, whatever the number of issues, and no text.
func (s *Store) StatusCounts(ctx context.Context) (map[Status]int, error) {
	out := map[Status]int{}
	err := scanAll(ctx, s.r, "issue counts", `SELECT status, count(*) FROM issues GROUP BY status`, nil, func(rs *sql.Rows) error {
		var st Status
		var n int
		if err := rs.Scan(&st, &n); err != nil {
			return err
		}
		out[st] = n
		return nil
	})
	return out, err
}
