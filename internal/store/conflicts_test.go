package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// merge is MergeIssue that fails the test on an error.
func merge(t *testing.T, s *Store, a Actor, id IssueID, base Rev, p IssuePatch) Merged {
	t.Helper()
	m, err := s.MergeIssue(t.Context(), a, id, base, p)
	if err != nil {
		t.Fatalf("MergeIssue(%s, base %d) = %v", id, base, err)
	}
	return m
}

func mustUpdate(t *testing.T, s *Store, a Actor, id IssueID, p IssuePatch) Issue {
	t.Helper()
	is, err := s.GetIssue(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.UpdateIssue(t.Context(), a, id, is.Rev, p)
	if err != nil {
		t.Fatalf("update %s: %v", id, err)
	}
	return out
}

func openConflicts(t *testing.T, s *Store, id IssueID) []Conflict {
	t.Helper()
	cs, _, err := s.Conflicts(t.Context(), ConflictFilter{Issue: id})
	if err != nil {
		t.Fatalf("Conflicts(%s) = %v", id, err)
	}
	return cs
}

// TestRowEventsCarryTheirRev: every write that changes an issue's row
// records the rev it left on its event, which is what a merge reads the
// state at a rev from.
func TestRowEventsCarryTheirRev(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "revs"})
	mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("renamed")})
	if _, _, err := s.StartIssue(t.Context(), alice, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandoffIssue(t.Context(), alice, is.ID, 0, HandoffNote{Note: "over to you"}, true, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseIssue(t.Context(), alice, is.ID, 0, "done"); err != nil {
		t.Fatal(err)
	}
	last, err := s.ReopenIssue(t.Context(), alice, is.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.r.QueryContext(t.Context(), `SELECT rev FROM events WHERE target = $1 AND rev IS NOT NULL ORDER BY seq`, string(is.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []Rev
	for rows.Next() {
		var r Rev
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if want := []Rev{1, 2, 3, 4, 5, 6}; !slices.Equal(got, want) || last.Rev != 6 {
		t.Errorf("revs on the events = %v (issue at rev %d); want %v: one for each write of the row", got, last.Rev, want)
	}
}

// TestConflictsPage: open conflicts are read a page at a time, oldest
// first, saying when there are more.
func TestConflictsPage(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	var want []int64
	for _, title := range []string{"one", "two", "three"} {
		is := mustCreate(t, s, NewIssue{Title: title})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr(title + ", bob's")})
		want = append(want, merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr(title + ", alice's")}).Parked[0].Conflict)
	}
	page, more, err := s.Conflicts(t.Context(), ConflictFilter{Limit: 2})
	if err != nil || !more || len(page) != 2 || page[0].ID != want[0] || page[1].ID != want[1] {
		t.Fatalf("Conflicts(limit 2) = %v, more %v, %v; want %v and more", page, more, err, want[:2])
	}
	page, more, err = s.Conflicts(t.Context(), ConflictFilter{Limit: 2, After: page[1].ID})
	if err != nil || more || len(page) != 1 || page[0].ID != want[2] {
		t.Errorf("Conflicts(after %d) = %v, more %v, %v; want %d alone", want[1], page, more, err, want[2])
	}
}

func eventsOf(t *testing.T, s *Store, id IssueID, op Op) []Event {
	t.Helper()
	evs, err := s.History(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, e := range evs {
		if e.Op == op {
			out = append(out, e)
		}
	}
	return out
}

// TestMergeRules holds MergeIssue, a replayed update compared field by
// field against the state at its base rev (design §18.7), to each rule of
// §8's table as kept.
func TestMergeRules(t *testing.T) {
	t.Parallel()
	s := newStore(t)

	t.Run("different fields both apply", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "one", Body: "b"})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("bob's title")})
		if err := s.AddLabel(t.Context(), bob, is.ID, "infra", ""); err != nil {
			t.Fatal(err)
		}
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Body: ptr("alice's body"), Priority: prio(P0)})
		if len(m.Parked) != 0 || len(m.Dropped) != 0 || m.Comment != "" {
			t.Errorf("merge of other fields = parked %v, dropped %v, comment %q; want all applied", m.Parked, m.Dropped, m.Comment)
		}
		if m.Title != "bob's title" || m.Body != "alice's body" || m.Priority != P0 || !slices.Equal(m.Labels, []string{"infra"}) {
			t.Errorf("issue = title %q, body %q, priority %d, labels %v; want both changes and the label", m.Title, m.Body, m.Priority, m.Labels)
		}
	})

	t.Run("the same scalar field parks", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "two"})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("bob's title")})
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("alice's title"), Body: ptr("alice's body")})
		if len(m.Parked) != 1 || m.Parked[0].Field != "title" || m.Parked[0].Conflict == 0 {
			t.Fatalf("parked = %+v, want title", m.Parked)
		}
		if m.Title != "bob's title" || m.Body != "alice's body" {
			t.Errorf("issue = title %q, body %q; want the server's title kept and the body applied", m.Title, m.Body)
		}
		cs := openConflicts(t, s, is.ID)
		if len(cs) != 1 {
			t.Fatalf("open conflicts = %+v, want 1", cs)
		}
		c := cs[0]
		if c.ID != m.Parked[0].Conflict || c.Field != "title" || c.Base != "two" || c.Mine != "alice's title" ||
			c.Theirs != "bob's title" || c.Author != "alice" || c.TheirsBy != "bob" || c.BaseRev != is.Rev {
			t.Errorf("conflict = %+v; want title, base two, mine alice's, theirs bob's, by alice against bob, base rev %d", c, is.Rev)
		}
		if n := len(eventsOf(t, s, is.ID, OpConflictPark)); n != 1 {
			t.Errorf("conflict.park events = %d, want 1", n)
		}
	})

	t.Run("long text parks whole", func(t *testing.T) {
		base := "line one\nline two\nline three\n"
		is := mustCreate(t, s, NewIssue{Title: "three", Body: base})
		theirs := "line one, edited\nline two\nline three\n"
		mustUpdate(t, s, bob, is.ID, IssuePatch{Body: ptr(theirs)})
		mine := "line one\nline two\nline three, edited\n"
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Body: ptr(mine)})
		if len(m.Parked) != 1 || m.Parked[0].Field != "body" {
			t.Fatalf("parked = %+v, want body, though the hunks do not overlap", m.Parked)
		}
		c := openConflicts(t, s, is.ID)[0]
		if c.Base != base || c.Mine != mine || c.Theirs != theirs || m.Body != theirs {
			t.Errorf("conflict = base %q mine %q theirs %q, issue body %q; want each whole, the server's kept", c.Base, c.Mine, c.Theirs, m.Body)
		}
	})

	t.Run("status, the server wins", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "four"})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Status: ptr(StatusBlocked)})
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Status: ptr(StatusDeferred), Title: ptr("alice's title")})
		if !slices.Equal(m.Dropped, []string{"status"}) || len(m.Parked) != 0 {
			t.Errorf("merge = dropped %v, parked %v; want status dropped", m.Dropped, m.Parked)
		}
		if m.Status != StatusBlocked || m.Title != "alice's title" {
			t.Errorf("issue = status %s, title %q; want blocked kept and the title applied", m.Status, m.Title)
		}
		if cs := openConflicts(t, s, is.ID); len(cs) != 0 {
			t.Errorf("open conflicts = %+v, want none", cs)
		}
	})

	t.Run("claim, the server wins", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "five"})
		if _, _, err := s.StartIssue(t.Context(), alice2, is.ID, time.Hour, false); err != nil {
			t.Fatal(err)
		}
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Assignee: ptr("carol"), Notes: ptr("n")})
		if !slices.Equal(m.Dropped, []string{"assignee"}) || m.Assignee != "alice" || m.Notes != "n" {
			t.Errorf("merge = dropped %v, assignee %q, notes %q; want the assignee dropped and the notes applied", m.Dropped, m.Assignee, m.Notes)
		}
	})

	t.Run("close wins over edit, the edit kept as a comment", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "six"})
		if _, err := s.CloseIssue(t.Context(), bob, is.ID, 0, "done"); err != nil {
			t.Fatal(err)
		}
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("alice's title"), Body: ptr("alice's body")})
		if m.Comment == "" || len(m.Parked) != 0 || m.Title != "six" || m.Status != StatusClosed {
			t.Fatalf("merge = %+v; want nothing applied and a comment", m)
		}
		page, err := s.CommentsPage(t.Context(), is.ID, "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Comments) != 1 || page.Comments[0].ID != m.Comment ||
			!strings.Contains(page.Comments[0].Body, "alice's title") || !strings.Contains(page.Comments[0].Body, "alice's body") {
			t.Errorf("comments = %+v; want one that keeps the edit", page.Comments)
		}
	})

	t.Run("the same change twice is no conflict", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "seven"})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("agreed")})
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("agreed")})
		if len(m.Parked)+len(m.Dropped) != 0 || m.Title != "agreed" {
			t.Errorf("merge = %+v; want nothing parked", m)
		}
	})

	t.Run("a change undone since is no conflict", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "eight"})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("briefly")})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("eight")})
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("alice's title")})
		if len(m.Parked) != 0 || m.Title != "alice's title" {
			t.Errorf("merge = parked %v, title %q; want it applied", m.Parked, m.Title)
		}
	})

	t.Run("at the current rev it is an update", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "nine"})
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("alice's title")})
		if m.Title != "alice's title" || m.Rev != is.Rev+1 {
			t.Errorf("merge = title %q rev %d; want applied at rev %d", m.Title, m.Rev, is.Rev+1)
		}
	})

	t.Run("a rev ahead of the issue is a conflict", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "ten"})
		if _, err := s.MergeIssue(t.Context(), alice, is.ID, is.Rev+5, IssuePatch{Title: ptr("x")}); !errors.Is(err, ErrConflict) {
			t.Errorf("MergeIssue at a rev ahead = %v, want ErrConflict", err)
		}
	})

	t.Run("a live update at a stale rev is still refused", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "eleven"})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("bob's")})
		if _, err := s.UpdateIssue(t.Context(), alice, is.ID, is.Rev, IssuePatch{Body: ptr("x")}); !errors.Is(err, ErrConflict) {
			t.Errorf("UpdateIssue at a stale rev = %v, want ErrConflict", err)
		}
	})

	t.Run("dates merge as fields: one nobody changed applies, one changed parks", func(t *testing.T) {
		day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
		is := mustCreate(t, s, NewIssue{Title: "dated", DueAt: ptr(day)})
		mustUpdate(t, s, bob, is.ID, IssuePatch{DueAt: ptr(day.Add(24 * time.Hour))})
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{DueAt: ptr(day.Add(48 * time.Hour)), DeferUntil: ptr(day)})
		if len(m.Parked) != 1 || m.Parked[0].Field != "due_at" {
			t.Fatalf("parked = %+v, want due_at", m.Parked)
		}
		if m.DueAt == nil || !m.DueAt.Equal(day.Add(24*time.Hour)) || m.DeferUntil == nil || !m.DeferUntil.Equal(day) {
			t.Errorf("issue = due %v, defer %v; want bob's due date kept and the defer applied", m.DueAt, m.DeferUntil)
		}
		c := openConflicts(t, s, is.ID)[0]
		if c.Base != "2026-11-02T00:00:00Z" || c.Mine != "2026-11-04T00:00:00Z" || c.Theirs != "2026-11-03T00:00:00Z" {
			t.Errorf("conflict = base %q mine %q theirs %q; want the three dates", c.Base, c.Mine, c.Theirs)
		}
	})

	t.Run("another principal's claim refuses it", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "twelve"})
		if _, _, err := s.StartIssue(t.Context(), bob, is.ID, time.Hour, false); err != nil {
			t.Fatal(err)
		}
		_, err := s.MergeIssue(t.Context(), alice, is.ID, is.Rev, IssuePatch{Title: ptr("x")})
		if _, ok := errors.AsType[*ForbiddenError](err); !ok {
			t.Errorf("MergeIssue of an issue bob holds = %v, want *ForbiddenError", err)
		}
	})
}

// TestMergeTellsAuthorAndHolder: a parked field is told to its author,
// whichever session replays it, and to the claim holder when that is
// another session.
func TestMergeTellsAuthorAndHolder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "told"})
	if _, _, err := s.StartIssue(t.Context(), alice2, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, s, alice2, is.ID, IssuePatch{Title: ptr("holder's title")})
	merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("replayed title")})

	for _, a := range []Actor{alice, alice2} {
		page, err := s.Inbox(t.Context(), a, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, it := range page.Items {
			found = found || it.Kind == InboxConflict && it.Issue == is.ID
		}
		if !found {
			t.Errorf("inbox of %s/%s = %+v; want a conflict item for %s", a.Principal, a.Session, page.Items, is.ID)
		}
	}
	n, err := s.OpenConflicts(t.Context(), "alice")
	if err != nil || n != 1 {
		t.Errorf("OpenConflicts(alice) = %d, %v; want 1", n, err)
	}
}

// TestConflictItemsToSelfAreNotRateLimited: a replay of many updates that
// park tells their author of each, past notices_per_minute, which bounds
// what one principal sends another.
func TestConflictItemsToSelfAreNotRateLimited(t *testing.T) {
	t.Parallel()
	s := openStore(t, newDSN(t), Options{Limits: Limits{Notices: 1}})
	for _, title := range []string{"one", "two", "three"} {
		is := mustCreate(t, s, NewIssue{Title: title})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr(title + ", bob's")})
		merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr(title + ", alice's")})
	}
	page, err := s.Inbox(t.Context(), alice, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range page.Items {
		if it.Kind == InboxConflict {
			n++
		}
	}
	if n != 3 {
		t.Errorf("alice has %d conflict items, want 3: %+v", n, page.Items)
	}
}

// TestResolveConflict: each choice, as a guarded change to the issue
// with a conflict.resolve event.
func TestResolveConflict(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	park := func(t *testing.T) (IssueID, int64) {
		t.Helper()
		is := mustCreate(t, s, NewIssue{Title: "base"})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("theirs")})
		m := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("mine")})
		return is.ID, m.Parked[0].Conflict
	}
	tests := []struct {
		name    string
		choice  Choice
		text    string
		propose *Choice
		want    string
	}{
		{"mine", ChoiceMine, "", nil, "mine"},
		{"theirs", ChoiceTheirs, "", nil, "theirs"},
		{"new text", ChoiceText, "both, merged", nil, "both, merged"},
		{"accept a proposal of mine", ChoiceAccept, "", ptr(ChoiceMine), "mine"},
		{"accept a proposal of text", ChoiceAccept, "", ptr(ChoiceText), "proposed text"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, cid := park(t)
			if tc.propose != nil {
				text := ""
				if *tc.propose == ChoiceText {
					text = "proposed text"
				}
				if _, err := s.ProposeResolution(t.Context(), bob, cid, *tc.propose, text, "it reads better"); err != nil {
					t.Fatal(err)
				}
			}
			is, err := s.ResolveConflict(t.Context(), bob, cid, tc.choice, tc.text)
			if err != nil {
				t.Fatalf("ResolveConflict(%d, %s) = %v", cid, tc.choice, err)
			}
			if is.Title != tc.want {
				t.Errorf("title after %s = %q, want %q", tc.choice, is.Title, tc.want)
			}
			if cs := openConflicts(t, s, id); len(cs) != 0 {
				t.Errorf("open conflicts after resolving = %+v", cs)
			}
			c, err := s.GetConflict(t.Context(), cid)
			if err != nil || c.ResolvedAt == nil || c.ResolvedBy != "bob" {
				t.Errorf("GetConflict(%d) = %+v, %v; want resolved by bob", cid, c, err)
			}
			if n := len(eventsOf(t, s, id, OpConflictResolve)); n != 1 {
				t.Errorf("conflict.resolve events = %d, want 1", n)
			}
			if _, err := s.ResolveConflict(t.Context(), bob, cid, ChoiceMine, ""); !errors.Is(err, ErrInvalid) {
				t.Errorf("resolving it again = %v, want ErrInvalid", err)
			}
		})
	}

	t.Run("accept with no proposal", func(t *testing.T) {
		_, cid := park(t)
		if _, err := s.ResolveConflict(t.Context(), bob, cid, ChoiceAccept, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("accept with no proposal = %v, want ErrInvalid", err)
		}
	})

	t.Run("text that the field does not take", func(t *testing.T) {
		_, cid := park(t)
		if _, err := s.ResolveConflict(t.Context(), bob, cid, ChoiceText, "  "); !errors.Is(err, ErrInvalid) {
			t.Errorf("a blank title = %v, want ErrInvalid", err)
		}
	})

	t.Run("guarded by the claim", func(t *testing.T) {
		id, cid := park(t)
		if _, _, err := s.StartIssue(t.Context(), alice, id, time.Hour, false); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ResolveConflict(t.Context(), bob, cid, ChoiceTheirs, ""); err == nil {
			t.Error("bob resolved a conflict on an issue alice holds")
		} else if _, ok := errors.AsType[*ForbiddenError](err); !ok {
			t.Errorf("bob's resolve = %v, want *ForbiddenError", err)
		}
		if _, err := s.ProposeResolution(t.Context(), bob, cid, ChoiceTheirs, "", ""); err == nil {
			t.Error("bob proposed on an issue alice holds")
		}
		dana := Actor{Principal: "dana", Session: "s-d", Machine: "m"}
		if _, err := s.ResolveConflict(t.Context(), dana, cid, ChoiceTheirs, ""); err != nil {
			t.Errorf("an admin's resolve = %v, want it allowed", err)
		}
	})
}

// TestProposeResolution: a proposal is kept with its conflict, a later
// one replaces it, each with a conflict.propose event, and none changes
// the issue.
func TestProposeResolution(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "base"})
	mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("theirs")})
	cid := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("mine")}).Parked[0].Conflict

	if _, err := s.ProposeResolution(t.Context(), bob, cid, ChoiceMine, "", "first"); err != nil {
		t.Fatal(err)
	}
	c, err := s.ProposeResolution(t.Context(), alice, cid, ChoiceText, "merged title", "second")
	if err != nil {
		t.Fatal(err)
	}
	if p := c.Proposal; p == nil || p.Choice != ChoiceText || p.Text != "merged title" || p.Why != "second" || p.By != "alice" {
		t.Errorf("proposal = %+v; want alice's text, which replaced bob's", c.Proposal)
	}
	if got, _ := s.GetIssue(t.Context(), is.ID); got.Title != "theirs" {
		t.Errorf("title after proposals = %q, want it unchanged", got.Title)
	}
	if n := len(eventsOf(t, s, is.ID, OpConflictPropose)); n != 2 {
		t.Errorf("conflict.propose events = %d, want 2", n)
	}
	for _, tc := range []struct {
		name   string
		choice Choice
		text   string
	}{
		{"text without text", ChoiceText, ""},
		{"mine with text", ChoiceMine, "x"},
		{"accept", ChoiceAccept, ""},
		{"unknown", Choice("both"), ""},
	} {
		if _, err := s.ProposeResolution(t.Context(), bob, cid, tc.choice, tc.text, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("propose %s = %v, want ErrInvalid", tc.name, err)
		}
	}
}

// TestLaterChangeClosesConflict: a change to a field with an open
// conflict closes it, with a note in the issue's history.
func TestLaterChangeClosesConflict(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "base"})
	mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("theirs")})
	cid := merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr("mine"), Body: ptr("b")}).Parked[0].Conflict

	mustUpdate(t, s, bob, is.ID, IssuePatch{Body: ptr("another field")})
	if cs := openConflicts(t, s, is.ID); len(cs) != 1 {
		t.Fatalf("a change to another field closed the conflict: %+v", cs)
	}
	mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("later")})
	if cs := openConflicts(t, s, is.ID); len(cs) != 0 {
		t.Errorf("open conflicts after a later change of the field = %+v", cs)
	}
	c, err := s.GetConflict(t.Context(), cid)
	if err != nil || c.Resolution != ResolvedSuperseded || c.ResolvedBy != "bob" {
		t.Errorf("conflict = %+v, %v; want superseded by bob", c, err)
	}
	evs := eventsOf(t, s, is.ID, OpConflictResolve)
	if len(evs) != 1 || !strings.Contains(string(evs[0].After), `"superseded"`) {
		t.Errorf("conflict.resolve events = %+v; want one that says superseded", evs)
	}
}

// TestConflictCaps: open conflicts for each issue and for each
// principal are capped; past the cap the update is refused, nothing of it
// applied, and not parked.
func TestConflictCaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		lim   Limits
		limit string
		other bool // the second conflict is on another issue
	}{
		{"per issue", Limits{ConflictsPerIssue: 1}, "conflicts_per_issue", false},
		{"per principal", Limits{ConflictsPerPrincipal: 1}, "conflicts_per_principal", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, newDSN(t), Options{Limits: tc.lim})
			a := mustCreate(t, s, NewIssue{Title: "a", Body: "b"})
			mustUpdate(t, s, bob, a.ID, IssuePatch{Title: ptr("theirs"), Body: ptr("theirs")})
			merge(t, s, alice, a.ID, a.Rev, IssuePatch{Title: ptr("mine")})

			target := a
			if tc.other {
				target = mustCreate(t, s, NewIssue{Title: "c"})
				mustUpdate(t, s, bob, target.ID, IssuePatch{Body: ptr("theirs")})
			}
			_, err := s.MergeIssue(t.Context(), alice, target.ID, target.Rev, IssuePatch{Body: ptr("mine"), Priority: prio(P0)})
			lim, ok := errors.AsType[*ConflictLimitError](err)
			if !ok || lim.Limit != tc.limit || !errors.Is(err, ErrInvalid) {
				t.Fatalf("MergeIssue past the cap = %v; want *ConflictLimitError for %s", err, tc.limit)
			}
			got, _ := s.GetIssue(t.Context(), target.ID)
			if got.Priority == P0 {
				t.Error("the refused update applied its priority")
			}
			if n := len(openConflicts(t, s, target.ID)); n != btoi(!tc.other) {
				t.Errorf("open conflicts on %s = %d after the refusal", target.ID, n)
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestMergeIdempotent: the same key replays the first result and parks
// nothing more.
func TestMergeIdempotent(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "base"})
	mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("theirs")})
	p := IssuePatch{Title: ptr("mine"), IdempotencyKey: "cli-merge-1"}
	first := merge(t, s, alice, is.ID, is.Rev, p)
	again := merge(t, s, alice, is.ID, is.Rev, p)
	if len(again.Parked) != 1 || again.Parked[0] != first.Parked[0] || again.Rev != first.Rev {
		t.Errorf("replayed merge = %+v, want the first result %+v", again, first)
	}
	if n := len(openConflicts(t, s, is.ID)); n != 1 {
		t.Errorf("open conflicts = %d, want 1", n)
	}
}

// TestMergeRerun (rule 8): a merge that loses to a concurrent write is
// run again from the start, on the state the winner left, and keeps
// nothing of the attempt that lost.
func TestMergeRerun(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	s1 := openStore(t, dsn, Options{})
	s2 := openStore(t, dsn, Options{})
	is := mustCreate(t, s1, NewIssue{Title: "base", Body: "body"})
	mustUpdate(t, s1, bob, is.ID, IssuePatch{Title: ptr("theirs")})

	// The first attempt loses, and the winner's write lands before the
	// rerun.
	var once bool
	s1.beforeCommit = func(context.Context) error {
		if once {
			return nil
		}
		once = true
		return errRetry
	}
	s1.beforeRetry = func(ctx context.Context) error {
		_, err := s2.UpdateIssue(ctx, bob, is.ID, is.Rev+1, IssuePatch{Body: ptr("bob's body")})
		return err
	}
	m := merge(t, s1, alice, is.ID, is.Rev, IssuePatch{Title: ptr("mine"), Body: ptr("alice's body")})
	if !once {
		t.Fatal("the concurrent write did not run")
	}
	var fields []string
	for _, p := range m.Parked {
		fields = append(fields, p.Field)
	}
	slices.Sort(fields)
	if !slices.Equal(fields, []string{"body", "title"}) {
		t.Errorf("parked = %v; want body too, which the winner changed", fields)
	}
	if n := len(openConflicts(t, s1, is.ID)); n != 2 {
		t.Errorf("open conflicts = %d, want 2 and none from the lost attempt", n)
	}
	if n := len(eventsOf(t, s1, is.ID, OpConflictPark)); n != 2 {
		t.Errorf("conflict.park events = %d, want 2", n)
	}
}

// TestMergeBeforeHistory: a base rev from before the events carried
// revs cannot be compared, and is refused as a stale update is.
func TestMergeBeforeHistory(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "old"})
	mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr("theirs")})
	if _, err := s.w.ExecContext(t.Context(), `UPDATE events SET rev = NULL WHERE target = $1`, string(is.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MergeIssue(t.Context(), alice, is.ID, is.Rev, IssuePatch{Body: ptr("x")}); !errors.Is(err, ErrConflict) {
		t.Errorf("MergeIssue against unstamped history = %v, want ErrConflict", err)
	}
}

// TestDigestListsOldConflicts: digest lists the conflicts open longer
// than its ConflictAge, oldest first, and leaves out newer and resolved
// ones.
func TestDigestListsOldConflicts(t *testing.T) {
	t.Parallel()
	clk := &clock{t: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	s := openStore(t, newDSN(t), Options{Now: clk.now})
	parkTitle := func(title string) (IssueID, int64) {
		t.Helper()
		is := mustCreate(t, s, NewIssue{Title: title})
		mustUpdate(t, s, bob, is.ID, IssuePatch{Title: ptr(title + ", bob's")})
		return is.ID, merge(t, s, alice, is.ID, is.Rev, IssuePatch{Title: ptr(title + ", alice's")}).Parked[0].Conflict
	}
	old, _ := parkTitle("old")
	_, resolved := parkTitle("resolved")
	if _, err := s.ResolveConflict(t.Context(), bob, resolved, ChoiceTheirs, ""); err != nil {
		t.Fatal(err)
	}
	clk.add(30 * time.Hour)
	parkTitle("new")

	d, err := s.Digest(t.Context(), DigestFilter{Window: 48 * time.Hour, ConflictAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(d.Conflicts); got != string(old)+" /1" {
		t.Errorf("digest's conflicts = %s; want only %s, open past a day", got, old)
	}
	if it := d.Conflicts.Items[0]; it.By != "alice" || !strings.Contains(it.Note, "title") {
		t.Errorf("conflict item = %+v; want alice's, naming the field", it)
	}
}

// TestConflictAdvice: the recommendation is computed when read, from who
// holds the claim and who made each side.
func TestConflictAdvice(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "base"})
	if _, _, err := s.StartIssue(t.Context(), alice2, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, s, alice2, is.ID, IssuePatch{Title: ptr("holder's")})
	if _, err := s.HandoffIssue(t.Context(), alice2, is.ID, 0, HandoffNote{Note: "pausing"}, true, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartIssue(t.Context(), bob, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.GetIssue(t.Context(), is.ID)
	if _, err := s.UpdateIssue(t.Context(), bob, is.ID, cur.Rev, IssuePatch{Title: ptr("bob holds it")}); err != nil {
		t.Fatal(err)
	}
	c := openConflicts(t, s, is.ID)
	if len(c) != 0 {
		t.Fatalf("unexpected conflicts %+v", c)
	}
	// dana, an admin, replays a title written at the start: it parks
	// against bob, who holds the issue.
	dana := Actor{Principal: "dana", Session: "s-d", Machine: "m"}
	merge(t, s, dana, is.ID, is.Rev, IssuePatch{Title: ptr("dana's")})
	c = openConflicts(t, s, is.ID)
	if len(c) != 1 || !strings.Contains(c[0].Advice, "take theirs") {
		t.Errorf("advice = %q; want theirs, from the claim holder", c[0].Advice)
	}
}
