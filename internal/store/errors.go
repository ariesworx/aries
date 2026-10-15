package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ariesworx/aries/internal/proto"
)

// Errors returned by the store, always wrapped with context. Test with
// [errors.Is].
//
// ariesd's error mapping (internal/server/errors.go) chooses the next
// step by error, or by a typed error such as [*StateError], never by
// message. It shows the messages, trimmed, and its tests pin what it
// shows.
var (
	// ErrNotFound means the issue, or another target, does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means the row changed since the caller's revision, or
	// a concurrent write kept winning until retries ran out.
	ErrConflict = errors.New("conflict")
	// ErrExists means an issue with that ID already exists.
	ErrExists = errors.New("already exists")
	// ErrCycle means the change would make a cycle of blocking edges or
	// parent links.
	ErrCycle = errors.New("dependency cycle")
	// ErrInvalid means the input failed validation, or the change does
	// not fit the issue's state, such as a status change on a closed
	// issue.
	ErrInvalid = errors.New("invalid input")
	// ErrNothingReady means StartIssue was asked for the top ready issue
	// and none is ready. It wraps ErrNotFound.
	ErrNothingReady = fmt.Errorf("nothing is ready to start: %w", ErrNotFound)
	// ErrSchemaTooNew means a newer ariesd migrated the database.
	ErrSchemaTooNew = errors.New("database schema is newer than this binary")
	// ErrSchemaOld means this binary has migrations the database lacks,
	// and was asked not to apply them (Options.NoMigrate).
	ErrSchemaOld = errors.New("database schema is older than this binary")
	// ErrForbidden means the actor may not make this change: another
	// principal holds the issue, or the change is for admins.
	ErrForbidden = errors.New("forbidden")
	// ErrBusy means the actor reached a cap on how much it may add in a
	// time window, such as its usage records a day; it may retry later.
	ErrBusy = errors.New("busy")

	// errRetry marks a failure that rerunning the write resolves.
	errRetry = errors.New("retry")
)

// The SQLSTATEs the store acts on.
const (
	codeSerialization = "40001" // serialization_failure
	codeDeadlock      = "40P01" // deadlock_detected
	codeUnique        = "23505" // unique_violation
	codePrivilege     = "42501" // insufficient_privilege
)

// sqlState returns the SQLSTATE of the Postgres error err wraps, or "".
func sqlState(err error) string {
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pe.Code
	}
	return ""
}

// retryable reports whether rerunning the write may succeed: err is
// errRetry, or Postgres's sign that a concurrent transaction won.
//
// That is a serialization failure, a deadlock, or a unique violation:
// two writers inserted one key, such as an issue's first claim row or
// the event log's next seq, and the second fails once the first commits.
// The rerun reads the row the winner wrote. Where a duplicate key means
// something to the caller (an issue id, an idempotency key, a memory's
// key), the write has checked for it in its transaction and refused
// with its own error, so the duplicate only reaches here when the row
// was written between that check and the insert; the rerun's check then
// finds it and refuses as it should.
func retryable(err error) bool {
	if errors.Is(err, errRetry) {
		return true
	}
	switch sqlState(err) {
	case codeSerialization, codeDeadlock, codeUnique:
		return true
	}
	return false
}

// isDuplicate reports whether err is a unique violation.
func isDuplicate(err error) bool { return sqlState(err) == codeUnique }

// HeldError refuses a start of an issue held under a live claim: by
// another principal, or (Own) by another session of the actor's own
// principal, which a start may take over only when asked to. It wraps
// ErrConflict.
type HeldError struct {
	ID IssueID
	By string
	// Own is set when By is the actor's principal; Session is the
	// session holding it then.
	Own     bool
	Session string
}

func (e *HeldError) Error() string {
	if e.Own {
		return fmt.Sprintf("issue %s is held by your session %s", e.ID, e.Session)
	}
	return fmt.Sprintf("issue %s is in progress by %s", e.ID, e.By)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *HeldError) Unwrap() error { return ErrConflict }

// IdemError refuses an idempotency key the principal already used for a
// different request, or used longer ago than the store's window
// (Expired): the request it was used for was applied then, at Used, and
// its result is no longer given for it. It wraps ErrConflict.
type IdemError struct {
	Key     string
	Expired bool
	// Used and Window are an expired key's: when its request was
	// applied, and the window it is past.
	Used   time.Time
	Window time.Duration
}

func (e *IdemError) Error() string {
	if e.Expired {
		return fmt.Sprintf("idempotency key %q was used at %s, longer ago than the %s a key's result is kept: its request was applied then",
			e.Key, e.Used.UTC().Format(time.RFC3339), proto.Span(e.Window))
	}
	return fmt.Sprintf("idempotency key %q was already used for a different request", e.Key)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *IdemError) Unwrap() error { return ErrConflict }

// StaleEpochError refuses a finish or release that names a claim epoch
// other than the current one: the issue was taken again since. It wraps
// ErrConflict.
type StaleEpochError struct {
	ID      IssueID
	Epoch   int64
	Current int64
	// By is the current holder, if the issue is held.
	By Actor
}

func (e *StaleEpochError) Error() string {
	return fmt.Sprintf("claim epoch %d on %s is stale (now %d)", e.Epoch, e.ID, e.Current)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *StaleEpochError) Unwrap() error { return ErrConflict }

// ForbiddenError refuses a change the actor may not make. Either Holder,
// another principal, holds the issue under a live claim until Until, and
// only the holder or an admin may change it; or Action is for admins only
// (for example "close --force" on ID, or "prices set", which has no ID).
// It wraps ErrForbidden.
type ForbiddenError struct {
	ID     IssueID
	Holder Actor
	Until  time.Time
	Action string
}

func (e *ForbiddenError) Error() string {
	switch {
	case e.Action != "" && e.ID == "":
		return e.Action + " is for admins"
	case e.Action != "":
		return fmt.Sprintf("%s on %s is for admins", e.Action, e.ID)
	}
	return fmt.Sprintf("issue %s is held by %s/%s until %s; only the holder or an admin may change it",
		e.ID, e.Holder.Principal, e.Holder.Session, e.Until.UTC().Format(time.RFC3339))
}

// Unwrap makes errors.Is(err, ErrForbidden) hold.
func (e *ForbiddenError) Unwrap() error { return ErrForbidden }

// StateError refuses a change that does not fit the issue's state, for
// the Reason given. It wraps ErrInvalid, and its text starts with
// ErrInvalid's.
type StateError struct {
	ID     IssueID
	Reason StateReason
	// Holder holds the issue's claim, when Reason is StateClaimed.
	Holder Actor
}

// StateReason says why a [*StateError] refused a change.
type StateReason string

// The reasons a [*StateError] gives.
const (
	// StateClaimed: a status or assignee change to an issue under a live
	// claim, which changes them only through finish, close or a releasing
	// handoff.
	StateClaimed StateReason = "claimed"
	// StateClosed: a change that needs the issue open, such as a status
	// change, a start, a release or an acceptance change.
	StateClosed StateReason = "closed"
	// StateAlreadyClosed: a close or finish of a closed issue.
	StateAlreadyClosed StateReason = "already closed"
	// StateNotClosed: a reopen of an issue that is not closed.
	StateNotClosed StateReason = "not closed"
)

func (e *StateError) Error() string {
	switch e.Reason {
	case StateClaimed:
		return fmt.Sprintf("%v: issue %s is claimed by %s/%s; %s", ErrInvalid, e.ID, e.Holder.Principal, e.Holder.Session, errClaimedFields)
	case StateClosed:
		return fmt.Sprintf("%v: issue %s is closed; reopen it first", ErrInvalid, e.ID)
	case StateAlreadyClosed:
		return fmt.Sprintf("%v: issue %s is already closed", ErrInvalid, e.ID)
	}
	return fmt.Sprintf("%v: issue %s is not closed", ErrInvalid, e.ID)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *StateError) Unwrap() error { return ErrInvalid }
