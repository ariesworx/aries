package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestUpdateSameRevOneWinner: many concurrent updates from the same rev,
// through one store. Exactly one wins; the rest get ErrConflict.
func TestUpdateSameRevOneWinner(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "start"})

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			p := IssuePatch{Title: ptr(fmt.Sprintf("writer %d", i))}
			if i%2 == 1 {
				p = IssuePatch{Priority: prio(P0)}
			}
			_, errs[i] = s.UpdateIssue(t.Context(), alice, is.ID, is.Rev, p)
		})
	}
	wg.Wait()
	assertOneWinner(t, errs)
	got, err := s.GetIssue(t.Context(), is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rev != 2 {
		t.Errorf("rev = %d, want 2", got.Rev)
	}
	assertUpdateEvents(t, s, is.ID, 1)
}

// Several stores on one database stand for several processes, such as an
// admin tool beside ariesd: they race freely, with no barrier, and every
// outcome is checked. A barrier that held one writer's transaction open
// until another reached its commit tested the retry under an optimistic
// database; under Postgres's row locks the second writer waits for the
// first and never reaches the barrier.
const raceStores, raceWriters, raceRounds = 4, 8, 15

// raceOpen opens raceStores stores on one new database.
func raceOpen(t *testing.T) []*Store {
	t.Helper()
	dsn := newDSN(t)
	ss := make([]*Store, raceStores)
	for i := range ss {
		ss[i] = openStore(t, dsn, Options{MaxAttempts: 50})
	}
	return ss
}

// raceActor is writer w's actor: a principal and session of its own.
func raceActor(w int) Actor {
	return Actor{Principal: fmt.Sprintf("agent%d", w), Session: fmt.Sprintf("sess-%d", w), Machine: "m"}
}

// TestUpdateRaceManyStores is the stage 0 bug, through several stores:
// writers read the same rev and change different columns of one issue.
// No change is merged into another silently: exactly one applies, with
// one event, and the rest get ErrConflict.
func TestUpdateRaceManyStores(t *testing.T) {
	t.Parallel()
	ss := raceOpen(t)
	for round := range raceRounds {
		is := mustCreate(t, ss[0], NewIssue{Title: fmt.Sprintf("start %d", round), Priority: prio(P2)})
		before := lastSeq(t, ss[0])
		errs := make([]error, raceWriters)
		var wg sync.WaitGroup
		for w := range raceWriters {
			wg.Go(func() {
				p := IssuePatch{Title: ptr(fmt.Sprintf("title by %d", w))}
				if w%2 == 1 {
					p = IssuePatch{Priority: prio(P0)}
				}
				_, errs[w] = ss[w%raceStores].UpdateIssue(t.Context(), raceActor(w), is.ID, is.Rev, p)
			})
		}
		wg.Wait()
		assertOneWinner(t, errs)
		got, err := ss[0].GetIssue(t.Context(), is.ID)
		if err != nil {
			t.Fatal(err)
		}
		titleChanged, prioChanged := got.Title != is.Title, got.Priority != P2
		if titleChanged == prioChanged {
			t.Errorf("round %d: exactly one change must apply: title %q, priority %d", round, got.Title, got.Priority)
		}
		if got.Rev != 2 || lastSeq(t, ss[0]) != before+1 {
			t.Errorf("round %d: rev %d, %d events; want rev 2 and 1 event", round, got.Rev, lastSeq(t, ss[0])-before)
		}
	}
	assertGapless(t, ss[0])
}

// TestCASNeedsNoWriteID pins the Postgres behavior the store depends on
// in place of Dolt's write_id: an UPDATE that names the rev it read is an
// exact compare-and-swap. Of two transactions that read rev 1 and change
// different columns of the row, the second, at the store's repeatable
// read, fails with a serialization failure the store retries; at read
// committed it matches no row. Either way the first's change is not
// overwritten and the second's is not merged into it.
func TestCASNeedsNoWriteID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		isolation sql.IsolationLevel
		retry     bool // the second's UPDATE fails as retryable; else it matches no row
	}{
		{"repeatable read", sql.LevelRepeatableRead, true},
		{"read committed", sql.LevelReadCommitted, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newDSN(t)
			s := openStore(t, dsn, Options{})
			is := mustCreate(t, s, NewIssue{Title: "start", Priority: prio(P2)})
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ctx := t.Context()
			opts := &sql.TxOptions{Isolation: tc.isolation}
			tx1, err := db.BeginTx(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx1.Rollback() }()
			tx2, err := db.BeginTx(ctx, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx2.Rollback() }()
			// Both read rev 1 before either writes.
			for i, tx := range []*sql.Tx{tx1, tx2} {
				var rev int64
				if err := tx.QueryRowContext(ctx, `SELECT rev FROM issues WHERE id = $1`, string(is.ID)).Scan(&rev); err != nil || rev != 1 {
					t.Fatalf("tx%d read rev %d, %v; want 1", i+1, rev, err)
				}
			}
			update := func(tx *sql.Tx, set string) (int64, error) {
				res, err := tx.ExecContext(ctx, `UPDATE issues SET `+set+`, rev = rev + 1 WHERE id = $1 AND rev = 1`, string(is.ID)) //nolint:gosec // constant test cases
				if err != nil {
					return 0, err
				}
				return res.RowsAffected()
			}
			if n, err := update(tx1, "title = 'from tx1'"); err != nil || n != 1 {
				t.Fatalf("tx1's CAS = %d rows, %v; want 1", n, err)
			}
			if err := tx1.Commit(); err != nil {
				t.Fatalf("tx1 commit: %v", err)
			}
			n, err := update(tx2, "priority = 0")
			switch {
			case tc.retry && (err == nil || !retryable(err)):
				t.Errorf("tx2's CAS after tx1 committed = %d rows, %v; want a serialization failure the store retries", n, err)
			case !tc.retry && (err != nil || n != 0):
				t.Errorf("tx2's CAS after tx1 committed = %d rows, %v; want no row matched", n, err)
			}
			_ = tx2.Rollback()
			got, err := s.GetIssue(ctx, is.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != "from tx1" || got.Priority != P2 || got.Rev != 2 {
				t.Errorf("issue after both = title %q, priority %d, rev %d; want tx1's title alone, priority 2, rev 2", got.Title, got.Priority, got.Rev)
			}
		})
	}
}

func assertOneWinner(t *testing.T, errs []error) {
	t.Helper()
	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrConflict):
		default:
			t.Errorf("writer %d: unexpected error %v", i, err)
		}
	}
	if wins != 1 {
		t.Errorf("%d writers won from the same rev, want exactly 1", wins)
	}
}

func assertUpdateEvents(t *testing.T, s *Store, id IssueID, want int) {
	t.Helper()
	h, err := s.History(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range h {
		if e.Op == OpIssueUpdate {
			n++
		}
	}
	if n != want {
		t.Errorf("%d update events, want %d", n, want)
	}
}

func assertGapless(t *testing.T, s *Store) {
	t.Helper()
	evs, err := s.Events(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: sequence has a gap", i, e.Seq)
		}
	}
}
