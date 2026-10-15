package store

// Spike only (spike/postgres): the races of cas_test.go, work_test.go and
// memory_test.go without their barrier. The barrier holds the first
// writer's transaction open until the second reaches its commit, which
// Dolt allows (both run, the loser fails at COMMIT) and Postgres cannot
// (the second waits on the first's row lock, so it never arrives). Here
// several stores on one database race freely, as several starfixd
// processes on one database would, and every outcome is checked.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

const raceStores, raceWriters, raceRounds = 4, 8, 15

func raceOpen(t *testing.T) []*Store {
	t.Helper()
	dsn := newDSN(t)
	ss := make([]*Store, raceStores)
	for i := range ss {
		ss[i] = openStore(t, dsn, Options{MaxAttempts: 50})
	}
	return ss
}

func raceActor(w int) Actor {
	return Actor{Principal: fmt.Sprintf("agent%d", w), Session: fmt.Sprintf("sess-%d", w), Machine: "m"}
}

// Every writer starts the same issue: one takes it, the rest are told it
// is held, and its claim names the one.
func TestSpikeStartRaceManyStores(t *testing.T) {
	t.Parallel()
	ss := raceOpen(t)
	for round := range raceRounds {
		is := mustCreate(t, ss[0], NewIssue{Title: fmt.Sprintf("contested %d", round)})
		errs := make([]error, raceWriters)
		var wg sync.WaitGroup
		for w := range raceWriters {
			wg.Go(func() { _, _, errs[w] = ss[w%raceStores].StartIssue(t.Context(), raceActor(w), is.ID, 0, false) })
		}
		wg.Wait()
		wins := 0
		for w, err := range errs {
			var held *HeldError
			switch {
			case err == nil:
				wins++
			case errors.As(err, &held):
			default:
				t.Errorf("round %d writer %d: %v", round, w, err)
			}
		}
		got, err := ss[0].GetIssue(t.Context(), is.ID)
		if err != nil {
			t.Fatal(err)
		}
		c, err := ss[0].ClaimOf(t.Context(), is.ID)
		if wins != 1 || got.Rev != 2 || err != nil || c == nil || c.Epoch != 1 || c.Holder.Principal != got.Assignee {
			t.Errorf("round %d: %d winners, rev %d, claim %+v (%v); want 1 winner, rev 2, epoch 1 held by %s", round, wins, got.Rev, c, err, got.Assignee)
		}
	}
	assertGapless(t, ss[0])
}

// Every writer updates the same issue from the same rev, each to its own
// title or priority: exactly one change applies, with one event.
func TestSpikeUpdateRaceManyStores(t *testing.T) {
	t.Parallel()
	ss := raceOpen(t)
	for round := range raceRounds {
		is := mustCreate(t, ss[0], NewIssue{Title: fmt.Sprintf("start %d", round)})
		before := lastSeq(t, ss[0])
		errs := make([]error, raceWriters)
		var wg sync.WaitGroup
		for w := range raceWriters {
			wg.Go(func() {
				p := IssuePatch{}
				if w%2 == 0 {
					p.Title = ptr(fmt.Sprintf("title by %d", w))
				} else {
					p.Priority = prio(Priority(w % 4))
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
		if got.Rev != 2 || lastSeq(t, ss[0]) != before+1 {
			t.Errorf("round %d: rev %d, %d events; want rev 2 and 1 event", round, got.Rev, lastSeq(t, ss[0])-before)
		}
	}
	assertGapless(t, ss[0])
}

// Writers on different issues through different stores all land, and the
// event log stays gapless: they contend only for the log's next seq.
func TestSpikeDisjointWritesManyStores(t *testing.T) {
	t.Parallel()
	ss := raceOpen(t)
	var ids []IssueID
	for i := range raceWriters {
		ids = append(ids, mustCreate(t, ss[0], NewIssue{Title: fmt.Sprintf("own %d", i)}).ID)
	}
	before := lastSeq(t, ss[0])
	errs := make([]error, raceWriters)
	var wg sync.WaitGroup
	for w := range raceWriters {
		wg.Go(func() {
			for i := range raceRounds {
				if _, err := ss[w%raceStores].AddComment(t.Context(), raceActor(w), ids[w], fmt.Sprintf("note %d", i), ""); err != nil {
					errs[w] = err
					return
				}
			}
		})
	}
	wg.Wait()
	for w, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", w, err)
		}
	}
	if n := lastSeq(t, ss[0]) - before; n != raceWriters*raceRounds {
		t.Errorf("%d events, want %d", n, raceWriters*raceRounds)
	}
	assertGapless(t, ss[0])
}
