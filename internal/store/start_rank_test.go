package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// onQuery is a querier that runs hook before its first query.
type onQuery struct {
	querier
	once func()
}

func (q onQuery) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.once()
	return q.querier.QueryContext(ctx, query, args...)
}

func (q onQuery) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	q.once()
	return q.querier.QueryRowContext(ctx, query, args...)
}

// Start with no id ranks the ready issues on a read, not under the
// writer: another write of the same store goes ahead while it ranks.
func TestStartRanksOnARead(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	top := mustCreate(t, s, NewIssue{Title: "top", Priority: prio(P0)})
	mustCreate(t, s, NewIssue{Title: "next", Priority: prio(P2)})
	var during error
	ran := false
	s.wrapRead = func(q querier) querier {
		return onQuery{querier: q, once: func() {
			if ran {
				return
			}
			ran = true
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second) // a guard: the writer is free, or this fails
			defer cancel()
			_, during = s.CreateIssue(wctx, bob, NewIssue{Title: "made while start ranked", Priority: prio(P4)})
		}}
	}
	is, _, err := s.StartIssue(ctx, alice, "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ran || during != nil {
		t.Errorf("a write while StartIssue(\"\") ranked: ran %v, %v; want it to run and succeed", ran, during)
	}
	if is.ID != top.ID {
		t.Errorf("StartIssue(\"\") took %s (%s), want %s, the top ready issue", is.ID, is.Title, top.ID)
	}
}

// readyNow agrees with Ready's query on every issue: open, not a
// template, and neither blocked nor deferred, on itself or an ancestor.
func TestReadyNowAgreesWithReady(t *testing.T) {
	t.Parallel()
	p := newSummaryProject(t)
	s, ctx := p.s, t.Context()
	now := s.Now()
	future := now.Add(time.Hour)
	var extra []IssueID
	mk := func(in NewIssue) IssueID {
		t.Helper()
		id := mustCreate(t, s, in).ID
		extra = append(extra, id)
		return id
	}
	deferred := mk(NewIssue{Title: "deferred parent", DeferUntil: &future})
	mk(NewIssue{Title: "child of deferred", ParentID: mk(NewIssue{Title: "middle", ParentID: deferred})})
	closedMiddle := mk(NewIssue{Title: "closed middle", ParentID: p.blocked["epic"]})
	mk(NewIssue{Title: "under a closed middle", ParentID: closedMiddle})
	if _, err := s.CloseIssue(ctx, alice, closedMiddle, 0, ""); err != nil {
		t.Fatal(err)
	}
	target := mk(NewIssue{Title: "conditional target"})
	cond := mk(NewIssue{Title: "conditionally blocked"})
	related := mk(NewIssue{Title: "related only"})
	waits := mk(NewIssue{Title: "waits for"})
	// A closed issue's blockers hold back nothing, not even its children.
	closedBlocked := mk(NewIssue{Title: "closed, with an open blocker"})
	mk(NewIssue{Title: "under a closed issue with an open blocker", ParentID: closedBlocked})
	for _, e := range []struct {
		from, to IssueID
		typ      DepType
	}{{cond, target, DepConditionalBlocks}, {related, target, DepRelated}, {waits, target, DepWaitsFor},
		{closedBlocked, target, DepBlocks}} {
		if err := s.AddDep(ctx, alice, e.from, e.to, e.typ); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CloseIssue(ctx, alice, closedBlocked, 0, ""); err != nil {
		t.Fatal(err)
	}
	ready, err := readyIDList(ctx, s.r, now, 500)
	if err != nil {
		t.Fatal(err)
	}
	ids := extra
	for _, is := range p.all {
		ids = append(ids, is.ID)
	}
	for _, id := range ids {
		got, err := readyNow(ctx, s.r, id, now)
		if err != nil {
			t.Fatal(err)
		}
		if want := slices.Contains(ready, id); got != want {
			is, _ := s.GetIssue(ctx, id)
			t.Errorf("readyNow(%s, %q) = %v, want %v as Ready's query has it", id, is.Title, got, want)
		}
	}
	if got, err := readyNow(ctx, s.r, "tst-nosuch1", now); got || err != nil {
		t.Errorf("readyNow(an issue that is not there) = %v, %v; want false", got, err)
	}
}

// Start with no id takes what Ready lists first, issue after issue, with
// ties, paths that overlap a held issue's, and a held issue that is open.
func TestStartTakesWhatReadyListsFirst(t *testing.T) {
	t.Parallel()
	frozen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := openStore(t, newDSN(t), Options{Now: func() time.Time { return frozen }})
	ctx := t.Context()
	held := mustCreate(t, s, NewIssue{Title: "held", Paths: []string{"a/"}})
	mustStart(t, s, alice, held.ID)
	for i := range 8 {
		in := NewIssue{Title: "work", Priority: prio(Priority(i % 3))}
		if i%3 == 0 {
			in.Paths = []string{"a/x.go"}
		}
		mustCreate(t, s, in)
	}
	for {
		want, err := s.Ready(ctx, bob, 1)
		if err != nil {
			t.Fatal(err)
		}
		is, _, err := s.StartIssue(ctx, bob, "", 0, false)
		if len(want) == 0 {
			if !errors.Is(err, ErrNothingReady) {
				t.Fatalf("StartIssue(\"\") with nothing ready = %s, %v; want ErrNothingReady", is.ID, err)
			}
			return
		}
		if err != nil || is.ID != want[0].ID {
			t.Fatalf("StartIssue(\"\") = %s, %v; want %s, which Ready lists first", is.ID, err, want[0].ID)
		}
	}
}

// When other sessions take the issues start ranked on its read before its
// write begins, the write takes the first of them still ready, and when
// none is, ranks under the writer, where no one can get ahead of it:
// either way the issue left first, in one write.
func TestStartTakesTheFirstStillReady(t *testing.T) {
	t.Parallel()
	for _, lost := range []int{0, 1, startCandidates - 1, startCandidates, startCandidates + 2} {
		t.Run(fmt.Sprintf("lost=%d", lost), func(t *testing.T) {
			dsn := newDSN(t)
			w := &waits{}
			s := openStore(t, dsn, Options{Tracer: w})
			other := openStore(t, dsn, Options{})
			ctx := t.Context()
			for range startCandidates + 6 {
				mustCreate(t, s, NewIssue{Title: "work"})
			}
			want := readyAfter(t, s, lost)
			var taken []IssueID
			// Carol takes the first lost issues just before bob's write
			// waits for the writer, after his read has ranked them.
			w.on(func(int) {
				for range lost {
					is, _, err := other.StartIssue(ctx, carol, "", 0, false)
					if err != nil {
						t.Errorf("carol's start: %v", err)
					}
					taken = append(taken, is.ID)
				}
			})
			is, _, err := s.StartIssue(ctx, bob, "", 0, false)
			if err != nil {
				t.Fatal(err)
			}
			if n := w.count(); n != 1 {
				t.Errorf("bob's start waited for the writer %d times, want once", n)
			}
			if slices.Contains(taken, is.ID) || is.ID != want {
				t.Errorf("bob's start took %s after carol took %v; want %s", is.ID, taken, want)
			}
		})
	}
}

// readyAfter returns the issue Ready lists at position n, counting from
// 0: the first left after n others are taken in Ready's order.
func readyAfter(t *testing.T, s *Store, n int) IssueID {
	t.Helper()
	rs, err := s.Ready(t.Context(), bob, n+1)
	if err != nil || len(rs) != n+1 {
		t.Fatalf("Ready(%d) = %d issues, %v", n+1, len(rs), err)
	}
	return rs[n].ID
}
