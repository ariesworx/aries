package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// summaryProject is a project made to catch a summary read that differs
// from the full one: every issue has the same created_at, so ordering
// falls to the id; texts are long; and there are closed, deferred,
// template, in-progress, assigned, parented and labeled issues, and
// issues blocked directly and through one or two blocked ancestors.
type summaryProject struct {
	s       *Store
	all     []Issue
	blocked map[string]IssueID
}

func newSummaryProject(t *testing.T) summaryProject {
	t.Helper()
	frozen := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := openStore(t, newDSN(t), Options{Now: func() time.Time { return frozen }})
	ctx := t.Context()
	long := strings.Repeat("A long line of the issue's text, and more of it. ", 200) // about 10 KiB
	future, past := frozen.Add(time.Hour), frozen.Add(-time.Hour)
	p := summaryProject{s: s, blocked: map[string]IssueID{}}
	mk := func(in NewIssue) IssueID {
		t.Helper()
		is := mustCreate(t, s, in)
		p.all = append(p.all, is)
		return is.ID
	}
	dep := func(from, to IssueID) {
		t.Helper()
		if err := s.AddDep(ctx, alice, from, to, DepBlocks, ""); err != nil {
			t.Fatalf("AddDep(%s, %s): %v", from, to, err)
		}
	}
	for i := range 12 {
		mk(NewIssue{Title: fmt.Sprintf("plain %d", i), Priority: prio(Priority(i % 3)), Body: long, Design: long,
			Acceptance: "- [ ] it works\n" + long, Notes: long, Labels: []string{"area-" + fmt.Sprint(i%2)}})
	}
	mk(NewIssue{Title: "template", Template: true})
	mk(NewIssue{Title: "deferred", Status: StatusDeferred})
	mk(NewIssue{Title: "defer future", DeferUntil: &future})
	mk(NewIssue{Title: "defer past", DeferUntil: &past, Assignee: "bob", Owner: "carol"})
	mk(NewIssue{Title: "in progress", Status: StatusInProgress, Assignee: "alice"})
	closed := mk(NewIssue{Title: "closed", Body: long})
	if _, err := s.CloseIssue(ctx, alice, closed, 0, "done"); err != nil {
		t.Fatal(err)
	}
	blocker := mk(NewIssue{Title: "blocker", Priority: prio(P0)})
	other := mk(NewIssue{Title: "other blocker", Priority: prio(P4)})
	epic := mk(NewIssue{Title: "blocked epic", Type: TypeEpic, Priority: prio(P1)})
	dep(epic, blocker)
	feature := mk(NewIssue{Title: "blocked feature", Type: TypeFeature, ParentID: epic, Priority: prio(P1)})
	dep(feature, other)
	for i := range 4 {
		p.blocked[fmt.Sprintf("leaf %d", i)] = mk(NewIssue{Title: fmt.Sprintf("leaf %d", i), ParentID: feature,
			Priority: prio(Priority(i % 2)), Body: long})
	}
	p.blocked["self and ancestors"] = mk(NewIssue{Title: "self and ancestors", ParentID: feature, Priority: prio(P2)})
	dep(p.blocked["self and ancestors"], blocker)
	tmpl := mk(NewIssue{Title: "blocked template", Template: true, Priority: prio(P2)})
	dep(tmpl, other)
	for i := range 6 {
		id := mk(NewIssue{Title: fmt.Sprintf("direct %d", i), Priority: prio(Priority(i % 3)), Body: long})
		dep(id, blocker)
		if i%2 == 0 {
			dep(id, other)
		}
	}
	gone := mk(NewIssue{Title: "closed and blocked"})
	dep(gone, blocker)
	if _, err := s.CloseIssue(ctx, alice, gone, 0, ""); err != nil {
		t.Fatal(err)
	}
	p.blocked["epic"], p.blocked["feature"] = epic, feature
	return p
}

func summaryOf(is Issue) Summary {
	return Summary{ID: is.ID, Title: is.Title, Status: is.Status, Priority: is.Priority}
}

func TestListSummariesMatchList(t *testing.T) {
	t.Parallel()
	p := newSummaryProject(t)
	ctx := t.Context()
	cases := []struct {
		name string
		f    Filter
	}{
		{"all", Filter{}},
		{"open", Filter{Statuses: []Status{StatusOpen}}},
		{"closed and deferred", Filter{Statuses: []Status{StatusClosed, StatusDeferred}}},
		{"epics and features", Filter{Types: []IssueType{TypeEpic, TypeFeature}}},
		{"priorities", Filter{Priorities: []Priority{P0, P2}}},
		{"assignee", Filter{Assignee: "bob"}},
		{"parent", Filter{ParentID: p.blocked["feature"]}},
		{"label", Filter{Labels: []string{"area-1"}}},
		{"no match", Filter{Labels: []string{"none"}}},
	}
	for _, tc := range cases {
		for _, limit := range []int{0, 1, 2, 7, 500} {
			t.Run(fmt.Sprintf("%s/limit=%d", tc.name, limit), func(t *testing.T) {
				f := tc.f
				f.Limit = limit
				for page := 0; ; page++ {
					full, err := p.s.List(ctx, f)
					if err != nil {
						t.Fatalf("List(%+v): %v", f, err)
					}
					got, err := p.s.ListSummaries(ctx, f)
					if err != nil {
						t.Fatalf("ListSummaries(%+v): %v", f, err)
					}
					want := make([]Summary, len(full.Issues))
					for i, is := range full.Issues {
						want[i] = summaryOf(is)
					}
					if !slices.Equal(got.Issues, want) || got.Next != full.Next {
						t.Fatalf("page %d: ListSummaries(%+v) = %v, next %q; want %v, next %q", page, f, got.Issues, got.Next,
							want, full.Next)
					}
					if full.Next == "" {
						return
					}
					f.Cursor = full.Next
				}
			})
		}
	}
}

func TestReadySummariesMatchIssues(t *testing.T) {
	t.Parallel()
	p := newSummaryProject(t)
	ctx := t.Context()
	for _, limit := range []int{1, 3, 10, 500} {
		rs, err := p.s.Ready(ctx, alice, limit)
		if err != nil {
			t.Fatal(err)
		}
		if limit == 500 && len(rs) < 10 {
			t.Fatalf("Ready(500) = %d issues, want the fixture's 10 or more", len(rs))
		}
		for _, r := range rs {
			is, err := p.s.GetIssue(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if r.Summary != summaryOf(is) {
				t.Errorf("Ready(%d) has %+v, want %+v", limit, r.Summary, summaryOf(is))
			}
		}
	}
}

func TestBlockedLimitIsAPrefix(t *testing.T) {
	t.Parallel()
	p := newSummaryProject(t)
	ctx := t.Context()
	all, err := p.s.Blocked(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	// Ordered by priority, then created_at, which is the same for all,
	// then id; via names the blocked ancestor when the issue is not
	// blocked itself.
	var ids []IssueID
	for _, b := range all {
		ids = append(ids, b.ID)
		is, err := p.s.GetIssue(ctx, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if b.Summary != summaryOf(is) {
			t.Errorf("Blocked has %+v, want %+v", b.Summary, summaryOf(is))
		}
	}
	if len(all) != 14 {
		t.Fatalf("Blocked(500) = %d issues %v, want 14", len(all), ids)
	}
	if !slices.IsSortedFunc(all, func(a, b BlockedIssue) int {
		if a.Priority != b.Priority {
			return int(a.Priority - b.Priority)
		}
		return strings.Compare(string(a.ID), string(b.ID))
	}) {
		t.Errorf("Blocked(500) = %v, not in priority and id order", ids)
	}
	vias := map[IssueID]IssueID{}
	for _, b := range all {
		vias[b.ID] = b.Via
	}
	// A leaf under two blocked ancestors is blocked through the nearer,
	// as the recursive query reaches it first.
	want := map[string]IssueID{"epic": "", "feature": "", "self and ancestors": ""}
	for i := range 4 {
		want[fmt.Sprintf("leaf %d", i)] = p.blocked["feature"]
	}
	for name, want := range want {
		if got := vias[p.blocked[name]]; got != want {
			t.Errorf("Blocked: %s via %q, want %q", name, got, want)
		}
	}
	for limit := range len(all) + 2 {
		got, err := p.s.Blocked(ctx, limit)
		if err != nil {
			t.Fatal(err)
		}
		n := limit
		if n == 0 {
			n = 50 // the default
		}
		want := all[:min(n, len(all))]
		if !equalBlocked(got, want) {
			t.Errorf("Blocked(%d) = %v, want %v", limit, got, want)
		}
	}
}

// The digest's blocked section names the same blockers as Blocked: an
// issue's own, or those of the ancestor it is blocked through.
func TestDigestBlockersMatchBlocked(t *testing.T) {
	t.Parallel()
	p := newSummaryProject(t)
	ctx := t.Context()
	all, err := p.s.Blocked(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	by := map[IssueID][]IssueID{}
	for _, b := range all {
		by[b.ID] = b.BlockedBy
	}
	d, err := p.s.Digest(ctx, DigestFilter{Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if d.Blocked.Total != len(all) || len(d.Blocked.Items) != DigestItems {
		t.Fatalf("Digest's blocked section has %d of %d, want %d of %d", len(d.Blocked.Items), d.Blocked.Total,
			DigestItems, len(all))
	}
	for _, it := range d.Blocked.Items {
		if !slices.Equal(it.BlockedBy, by[it.ID]) || len(it.BlockedBy) == 0 {
			t.Errorf("Digest: %s (%s) blocked by %v, want %v as Blocked has it", it.ID, it.Title, it.BlockedBy, by[it.ID])
		}
	}
}

func equalBlocked(a, b []BlockedIssue) bool {
	return slices.EqualFunc(a, b, func(x, y BlockedIssue) bool {
		return x.Summary == y.Summary && x.Via == y.Via && slices.Equal(x.BlockedBy, y.BlockedBy)
	})
}
