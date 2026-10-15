package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A change that does not fit the issue's state is refused with a
// *StateError naming the reason and the issue, so that ariesd chooses
// the fix without reading the message (internal/server/errors.go). It
// still matches ErrInvalid, and its text is the message these refusals
// had as plain errors.
func TestStateErrors(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	open := func(t *testing.T) Issue {
		t.Helper()
		return mustCreate(t, s, NewIssue{})
	}
	closed := func(t *testing.T) Issue {
		t.Helper()
		is, err := s.CloseIssue(ctx, alice, open(t).ID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	tests := []struct {
		name   string
		run    func(t *testing.T) (IssueID, error)
		reason StateReason
		text   string // the message, with %s for the issue
	}{
		{"status change of a claimed issue", func(t *testing.T) (IssueID, error) {
			is, _, err := s.StartIssue(ctx, alice, open(t).ID, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Status: ptr(StatusBlocked)})
			return is.ID, err
		}, StateClaimed, "invalid input: issue %s is claimed by alice/sess-a; status and assignee change only through finish, close or a releasing handoff"},
		{"status change of a closed issue", func(t *testing.T) (IssueID, error) {
			is := closed(t)
			_, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Status: ptr(StatusOpen)})
			return is.ID, err
		}, StateClosed, "invalid input: issue %s is closed; reopen it first"},
		{"start of a closed issue", func(t *testing.T) (IssueID, error) {
			id := closed(t).ID
			_, _, err := s.StartIssue(ctx, alice, id, 0, false)
			return id, err
		}, StateClosed, "invalid input: issue %s is closed; reopen it first"},
		{"release of a closed issue", func(t *testing.T) (IssueID, error) {
			id := closed(t).ID
			_, err := s.HandoffIssue(ctx, alice, id, 0, HandoffNote{Note: "over to you"}, true, "", nil)
			return id, err
		}, StateClosed, "invalid input: issue %s is closed; reopen it first"},
		{"acceptance change on a closed issue", func(t *testing.T) (IssueID, error) {
			id := closed(t).ID
			_, err := s.Accept(ctx, alice, id, Acceptance{Tick: []int{1}})
			return id, err
		}, StateClosed, "invalid input: issue %s is closed; reopen it first"},
		{"close of a closed issue", func(t *testing.T) (IssueID, error) {
			id := closed(t).ID
			_, err := s.CloseIssue(ctx, alice, id, 0, "")
			return id, err
		}, StateAlreadyClosed, "invalid input: issue %s is already closed"},
		{"reopen of an open issue", func(t *testing.T) (IssueID, error) {
			id := open(t).ID
			_, err := s.ReopenIssue(ctx, alice, id, 0)
			return id, err
		}, StateNotClosed, "invalid input: issue %s is not closed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := tc.run(t)
			var se *StateError
			if !errors.As(err, &se) || se.Reason != tc.reason || se.ID != id {
				t.Fatalf("err = %v (%T), want a *StateError on %s with reason %q", err, err, id, tc.reason)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("errors.Is(%v, ErrInvalid) = false, want true", err)
			}
			if want := fmt.Sprintf(tc.text, id); err.Error() != want {
				t.Errorf("err.Error() = %q, want %q", err.Error(), want)
			}
		})
	}
}

// A write is rerun on Postgres's signs that a concurrent transaction won,
// a unique violation among them (two writers inserting one key), and on
// nothing else.
func TestRetryable(t *testing.T) {
	t.Parallel()
	pg := func(code string) error { return fmt.Errorf("insert: %w", &pgconn.PgError{Code: code}) }
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"serialization failure", pg("40001"), true},
		{"deadlock", pg("40P01"), true},
		{"unique violation", pg("23505"), true},
		{"store's own retry", fmt.Errorf("%w: moved", errRetry), true},
		{"insufficient privilege", pg("42501"), false},
		{"undefined column", pg("42703"), false},
		{"not found", ErrNotFound, false},
		{"conflict", ErrConflict, false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%s: %v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
	if !isDuplicate(pg("23505")) || isDuplicate(pg("40001")) {
		t.Error("isDuplicate does not tell a unique violation from another error")
	}
}

// Open reads the server's version and refuses one older than Postgres
// 16, naming the version and the fix.
func TestServerVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		num  int
		name string
		ok   bool
	}{
		{90624, "9.6.24", false},
		{150014, "15.14", false},
		{160000, "16.0", true},
		{170007, "17.7", true},
		{180006, "18.6", true},
	}
	for _, tc := range tests {
		err := serverVersion(tc.num, tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("serverVersion(%d, %q) = %v, want ok %v", tc.num, tc.name, err, tc.ok)
			continue
		}
		if err == nil {
			continue
		}
		ve, ok := errors.AsType[*ServerVersionError](err)
		if !ok || ve.Version != tc.name || !strings.Contains(err.Error(), "fix: upgrade it to Postgres 16") {
			t.Errorf("serverVersion(%d, %q) = %v, want a *ServerVersionError naming %s and the fix", tc.num, tc.name, err, tc.name)
		}
	}
}
