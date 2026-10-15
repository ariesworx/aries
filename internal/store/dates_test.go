package store

import (
	"slices"
	"testing"
	"time"
)

func itemIDs(items []DigestItem) []IssueID {
	var ids []IssueID
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	return ids
}

// A maintenance window: an issue deferred to a date is out of Ready until
// then, and once the date passes Dates lists it as newly ready to a reader
// whose window began before; an issue past its due date is overdue, and
// one due within the window is due soon. Each is computed from the
// store's time when read, and nothing is written when a date passes.
func TestDates(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	s, clk := clockStoreAt(t, start)
	ctx := t.Context()
	upgrade := mustCreate(t, s, NewIssue{Title: "upgrade the database", DeferUntil: ptr(start.Add(2 * time.Hour)),
		DueAt: ptr(start.Add(4 * time.Hour)), Assignee: "bob"})
	report := mustCreate(t, s, NewIssue{Title: "quarterly report", DueAt: ptr(start.Add(30 * time.Hour))})
	blocker := mustCreate(t, s, NewIssue{Title: "blocker"})
	waiting := mustCreate(t, s, NewIssue{Title: "deferred but blocked", DeferUntil: ptr(start.Add(time.Hour))})
	if err := s.AddDep(ctx, alice, waiting.ID, blocker.ID, DepBlocks, ""); err != nil {
		t.Fatal(err)
	}
	done := mustCreate(t, s, NewIssue{Title: "done long ago", DueAt: ptr(start.Add(time.Hour))})
	if _, err := s.CloseIssue(ctx, alice, done.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	seq := lastSeq(t, s)

	check := func(at string, since time.Time, ready, soon, overdue []IssueID) {
		t.Helper()
		d, err := s.Dates(ctx, DatesFilter{Since: since, DueSoon: 24 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if got := itemIDs(d.Ready.Items); !slices.Equal(got, ready) {
			t.Errorf("%s: Dates ready = %v, want %v", at, got, ready)
		}
		if got := itemIDs(d.DueSoon.Items); !slices.Equal(got, soon) {
			t.Errorf("%s: Dates due soon = %v, want %v", at, got, soon)
		}
		if got := itemIDs(d.Overdue.Items); !slices.Equal(got, overdue) {
			t.Errorf("%s: Dates overdue = %v, want %v", at, got, overdue)
		}
	}
	if got := readyIDs(t, s); slices.Contains(got, upgrade.ID) {
		t.Errorf("Ready before the window = %v, want %s left out", got, upgrade.ID)
	}
	check("at the start", start, nil, []IssueID{upgrade.ID}, nil)

	clk.add(3 * time.Hour)
	if got := readyIDs(t, s); !slices.Contains(got, upgrade.ID) {
		t.Errorf("Ready in the window = %v, want %s", got, upgrade.ID)
	}
	check("in the window", start, []IssueID{upgrade.ID}, []IssueID{upgrade.ID}, nil)
	check("asked after the date passed", start.Add(150*time.Minute), nil, []IssueID{upgrade.ID}, nil)

	clk.add(4 * time.Hour) // start+7h: upgrade overdue, the report due within a day
	check("past the due date", start, []IssueID{upgrade.ID}, []IssueID{report.ID}, []IssueID{upgrade.ID})

	if got := lastSeq(t, s); got != seq {
		t.Errorf("dates passing wrote events %d to %d, want none", seq, got)
	}
	d, err := s.Dates(ctx, DatesFilter{Since: start, DueSoon: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if it := d.Overdue.Items[0]; !it.At.Equal(start.Add(4*time.Hour)) || it.By != "bob" {
		t.Errorf("overdue item = %+v, want its due date and assignee", it)
	}
	dg, err := s.Digest(ctx, DigestFilter{Since: start, DueSoon: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(itemIDs(dg.Ready.Items), []IssueID{upgrade.ID}) || dg.Overdue.Total != 1 || dg.DueSoon.Total != 1 {
		t.Errorf("Digest dates = ready %v, due soon %d, overdue %d; want %s, 1 and 1",
			itemIDs(dg.Ready.Items), dg.DueSoon.Total, dg.Overdue.Total, upgrade.ID)
	}
}

// update sets and clears both dates; a zero time clears one.
func TestUpdateDates(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	s, _ := clockStoreAt(t, start)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x"})
	due, later := start.Add(48*time.Hour), start.Add(24*time.Hour)
	is, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{DueAt: &due, DeferUntil: &later})
	if err != nil {
		t.Fatal(err)
	}
	if is.DueAt == nil || !is.DueAt.Equal(due) || is.DeferUntil == nil || !is.DeferUntil.Equal(later) {
		t.Fatalf("UpdateIssue(dates) = due %v, defer %v; want %v and %v", is.DueAt, is.DeferUntil, due, later)
	}
	is, err = s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{DueAt: &time.Time{}, DeferUntil: &time.Time{}})
	if err != nil {
		t.Fatal(err)
	}
	if is.DueAt != nil || is.DeferUntil != nil {
		t.Errorf("UpdateIssue(clear dates) = due %v, defer %v; want both cleared", is.DueAt, is.DeferUntil)
	}
}
