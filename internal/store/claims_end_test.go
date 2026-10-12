package store

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEndClaims pins decision 21 in the store: the claims of the holders
// chosen end, each with its own claim.release naming the actor and the
// reason, the holder's session is told, and the issue is ready again.
// Other claims are left alone.
func TestEndClaims(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	issue := func(title string, by Actor, lease time.Duration) IssueID {
		t.Helper()
		is := mustCreate(t, s, NewIssue{Title: title})
		if _, _, err := s.StartIssue(ctx, by, is.ID, lease, false); err != nil {
			t.Fatalf("start %s: %v", title, err)
		}
		return is.ID
	}
	a1 := issue("alice's", alice, time.Hour)
	a2 := issue("alice's, in another session", alice2, time.Hour)
	lapsed := issue("alice's, lapsed and not yet reaped", alice, time.Minute)
	b := issue("bob's", bob, time.Hour)
	free := mustCreate(t, s, NewIssue{Title: "nobody's"}).ID
	clk.add(2 * time.Minute)

	// A sweep that finds nobody to end records nothing.
	seq := lastSeq(t, s)
	if ended, err := s.EndClaims(ctx, dana, func(string) string { return "" }); err != nil || len(ended) != 0 {
		t.Fatalf("EndClaims choosing no one = %+v, %v; want none", ended, err)
	}
	if got := lastSeq(t, s); got != seq {
		t.Errorf("EndClaims choosing no one recorded %d events", got-seq)
	}

	ended, err := s.EndClaims(ctx, dana, func(p string) string {
		if p == "alice" {
			return "membership removed"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("EndClaims(alice): %v", err)
	}
	var got []IssueID
	for _, c := range ended {
		if c.Holder.Principal != "alice" {
			t.Errorf("ended a claim of %s on %s", c.Holder.Principal, c.Issue)
		}
		got = append(got, c.Issue)
	}
	want := []IssueID{a1, a2, lapsed}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("EndClaims(alice) ended %v, want %v", got, want)
	}

	for _, id := range want {
		if c, err := s.ClaimOf(ctx, id); err != nil || c != nil {
			t.Errorf("ClaimOf(%s) = %+v, %v; want none", id, c, err)
		}
		is, err := s.GetIssue(ctx, id)
		if err != nil || is.Status != StatusOpen || is.Assignee != "" {
			t.Errorf("issue %s = %s, assignee %q, %v; want open and unassigned", id, is.Status, is.Assignee, err)
		}
		evs, err := s.History(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var ops []string
		for _, e := range evs[len(evs)-2:] {
			ops = append(ops, string(e.Op)+" by "+e.Actor.Principal)
		}
		if w := []string{"claim.release by dana", "issue.update by dana"}; !slices.Equal(ops, w) {
			t.Errorf("last events of %s = %v, want %v", id, ops, w)
		}
		rel := evs[len(evs)-2]
		if !strings.Contains(string(rel.Before), `"principal":"alice"`) || !strings.Contains(string(rel.Before), `"epoch":1`) ||
			!strings.Contains(string(rel.After), `"reason":"membership removed"`) {
			t.Errorf("claim.release of %s: before %s, after %s; want the holder, the epoch and the reason", id, rel.Before, rel.After)
		}
		for _, e := range evs {
			if e.Op == OpAdminOverride {
				t.Errorf("issue %s has an admin.override event: ending access is no change to the issue by an admin", id)
			}
		}
	}

	// bob's claim, and his issue, are as they were.
	if c, err := s.ClaimOf(ctx, b); err != nil || c == nil || c.Holder != bob {
		t.Errorf("ClaimOf(bob's) = %+v, %v; want bob's claim", c, err)
	}
	if is, err := s.GetIssue(ctx, b); err != nil || is.Status != StatusInProgress || is.Assignee != "bob" {
		t.Errorf("bob's issue = %s, %q, %v; want in progress with bob", is.Status, is.Assignee, err)
	}

	// Each session that held one is told, and only it.
	for _, tc := range []struct {
		reader Actor
		issues []IssueID
	}{{alice, []IssueID{a1, lapsed}}, {alice2, []IssueID{a2}}, {bob, nil}} {
		page, err := s.Inbox(ctx, tc.reader, false, 0)
		if err != nil {
			t.Fatal(err)
		}
		var told []IssueID
		for _, it := range page.Items {
			if it.Kind != InboxClaimLost || !strings.Contains(it.Body, "membership removed by dana") || !strings.Contains(it.Body, "stop work on it") {
				t.Errorf("%s/%s got %s item %q; want claim.lost saying why and by whom", tc.reader.Principal, tc.reader.Session, it.Kind, it.Body)
			}
			told = append(told, it.Issue)
		}
		slices.Sort(told)
		wantTold := slices.Sorted(slices.Values(tc.issues))
		if !slices.Equal(told, wantTold) {
			t.Errorf("%s/%s was told of %v, want %v", tc.reader.Principal, tc.reader.Session, told, wantTold)
		}
	}

	// The issues are ready for the next person, with the one never held.
	ready, err := s.Ready(ctx, bob, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []IssueID
	for _, r := range ready {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	if w := slices.Sorted(slices.Values([]IssueID{a1, a2, lapsed, free})); !slices.Equal(ids, w) {
		t.Errorf("Ready = %v, want %v", ids, w)
	}
	if _, c, err := s.StartIssue(ctx, bob, a1, 0, false); err != nil || c.Epoch != 2 {
		t.Errorf("bob starts the released issue: %+v, %v; want epoch 2", c, err)
	}

	// Run again, it finds nothing left to end.
	if ended, err := s.EndClaims(ctx, dana, func(string) string { return "membership removed" }); err != nil || len(ended) != 2 {
		t.Errorf("EndClaims of everyone = %d claims, %v; want bob's two", len(ended), err)
	}
}
