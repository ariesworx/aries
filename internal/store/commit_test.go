package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A write whose context ends while its COMMIT is in flight does not know
// whether Postgres applied it: pgx gives up on the connection without
// telling Postgres, which finishes the commit. The store must say so,
// with ErrCommitUnknown, rather than return an error a caller would take
// for a refusal (aries-eyomn7gz: the soak saw an update its client was
// told failed, in the event log).
func TestCommitCutOffIsUnknown(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	// A deferred constraint trigger runs at COMMIT, so it holds the
	// commit open long enough for the context to end inside it. When pgx
	// lets go it also sends a cancel request, which a commit that is
	// quick (as in the soak) has already outrun; the trigger ignores the
	// cancel to stand for that commit, so that this one applies too.
	for _, q := range []string{
		`CREATE FUNCTION slow_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   BEGIN PERFORM pg_sleep(1); EXCEPTION WHEN query_canceled THEN NULL; END;
		   RETURN NULL;
		 END $$`,
		`CREATE CONSTRAINT TRIGGER slow_commit AFTER INSERT ON issues
		 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
		 WHEN (NEW.title = 'cut off') EXECUTE FUNCTION slow_commit()`,
	} {
		if _, err := s.w.ExecContext(ctx, q); err != nil {
			t.Fatalf("plant the slow commit: %v", err)
		}
	}

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.beforeCommit = func(context.Context) error {
		// Ends the context once COMMIT has been sent, not before it:
		// database/sql refuses to commit a transaction whose context
		// has already ended, which would be a plain refusal.
		time.AfterFunc(200*time.Millisecond, cancel)
		return nil
	}
	_, err := s.CreateIssue(wctx, alice, NewIssue{Title: "cut off"})
	s.beforeCommit = nil
	if !errors.Is(err, ErrCommitUnknown) {
		t.Fatalf("CreateIssue with its context ended during COMMIT = %v, want ErrCommitUnknown", err)
	}
	if errors.Is(err, ErrConflict) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrBusy) {
		t.Errorf("CreateIssue's error %v also reads as a refusal", err)
	}

	// Why it is unknown: Postgres finished the commit after pgx let go.
	var n int
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := s.w.QueryRowContext(ctx, `SELECT count(*) FROM issues WHERE title = 'cut off'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n != 1 {
		t.Errorf("issues titled %q after the cut-off commit = %d, want 1: the commit applied though its caller was told it failed", "cut off", n)
	}

	// The store still writes: the connection pgx gave up on is not reused.
	if _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "after"}); err != nil {
		t.Errorf("CreateIssue after a cut-off commit = %v", err)
	}
}

// A commit Postgres refused with an error of its own is known to have
// failed, and is not ErrCommitUnknown.
func TestCommitRefusedByPostgresIsKnown(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	for _, q := range []string{
		`CREATE FUNCTION refuse_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'refused at commit' USING ERRCODE = 'check_violation'; END $$`,
		`CREATE CONSTRAINT TRIGGER refuse_commit AFTER INSERT ON issues
		 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
		 WHEN (NEW.title = 'refused') EXECUTE FUNCTION refuse_commit()`,
	} {
		if _, err := s.w.ExecContext(ctx, q); err != nil {
			t.Fatalf("plant the refusing commit: %v", err)
		}
	}
	_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "refused"})
	if err == nil {
		t.Fatal("CreateIssue whose commit Postgres refused = nil")
	}
	if errors.Is(err, ErrCommitUnknown) {
		t.Errorf("CreateIssue whose commit Postgres refused = %v, want an error that is not ErrCommitUnknown", err)
	}
}
