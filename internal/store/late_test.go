package store

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A finish or handoff written while the server could not be reached is
// replayed from the outbox, maybe after its claim's lease ran out
// (design §18.3, §18.6). It applies if nobody took the issue since,
// with a claim.lapsed event that says the lease had lapsed; otherwise
// its note is kept as a comment, a finish's discovered issues are still
// created, and the replaying principal and the new holder are told.

// lateCase is one history an issue may have had while alice's session
// was cut off, after she took it at epoch 1 with a 15-minute lease.
type lateCase struct {
	name string
	// after builds the history, from the clock at alice's start.
	after func(t *testing.T, s *Store, clk *clock, id IssueID)
	// epoch is the epoch the replayed write carries.
	epoch int64
	// lapsed: it applies, and says the lease had lapsed; applied without
	// it: it applies as any finish would. Otherwise it is kept.
	lapsed, applied bool
	// why is in the kept comment and the items.
	why string
	// told is the session, besides alice's principal, told it was kept.
	told Actor
}

// lateCases are the histories of a held issue, alice's at epoch 1.
var lateCases = []lateCase{
	{name: "held still", epoch: 1, applied: true, after: func(*testing.T, *Store, *clock, IssueID) {}},
	{name: "lapsed, not reaped", epoch: 1, lapsed: true, after: func(_ *testing.T, _ *Store, clk *clock, _ IssueID) {
		clk.add(20 * time.Minute)
	}},
	{name: "lapsed and reaped", epoch: 1, lapsed: true, after: func(t *testing.T, s *Store, clk *clock, _ IssueID) {
		clk.add(20 * time.Minute)
		if _, err := s.ReapClaims(t.Context()); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "taken by another principal", epoch: 1, why: "taken since", told: bob, after: func(t *testing.T, s *Store, clk *clock, id IssueID) {
		clk.add(20 * time.Minute)
		startAs(t, s, bob, id, false)
	}},
	{name: "taken over by another session of hers", epoch: 1, why: "taken since", told: alice2, after: func(t *testing.T, s *Store, _ *clock, id IssueID) {
		startAs(t, s, alice2, id, true)
	}},
	{name: "taken and let go", epoch: 1, why: "taken since", after: func(t *testing.T, s *Store, clk *clock, id IssueID) {
		clk.add(20 * time.Minute)
		startAs(t, s, bob, id, false)
		if _, err := s.HandoffIssue(t.Context(), bob, id, 2, HandoffNote{Note: "not mine"}, true, "", nil); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "closed since", epoch: 1, why: "closed since", after: func(t *testing.T, s *Store, clk *clock, id IssueID) {
		clk.add(20 * time.Minute)
		if _, err := s.CloseIssue(t.Context(), bob, id, 0, "duplicate"); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "no epoch, another principal holds it", why: "holds it", told: bob, after: func(t *testing.T, s *Store, clk *clock, id IssueID) {
		clk.add(20 * time.Minute)
		startAs(t, s, bob, id, false)
	}},
}

func startAs(t *testing.T, s *Store, a Actor, id IssueID, take bool) Claim {
	t.Helper()
	_, c, err := s.StartIssue(t.Context(), a, id, 0, take)
	if err != nil {
		t.Fatalf("StartIssue(%s) by %s/%s: %v", id, a.Principal, a.Session, err)
	}
	return c
}

func TestReplayFinish(t *testing.T) {
	t.Parallel()
	for _, tc := range lateCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, clk := clockStore(t)
			ctx := t.Context()
			is := mustCreate(t, s, NewIssue{Title: "work"})
			startAs(t, s, alice, is.ID, false)
			tc.after(t, s, clk, is.ID)
			seq := lastSeq(t, s)
			f := Finish{Reason: "done offline", Handoff: HandoffNote{Note: "watch the cache", HandoffFields: HandoffFields{State: HandoffDone, To: "carol"}},
				Discovered: []NewIssue{{Title: "found offline"}}, IdempotencyKey: "late-1"}
			got, err := s.ReplayFinish(ctx, alice, is.ID, tc.epoch, f)
			if err != nil {
				t.Fatalf("ReplayFinish = %v, want it applied or kept", err)
			}
			after, err := s.GetIssue(ctx, is.ID)
			if err != nil {
				t.Fatal(err)
			}
			ops := opsSince(t, s, seq)
			if len(got.IDs) != 1 {
				t.Fatalf("ReplayFinish created %v, want the one discovered issue", got.IDs)
			}
			if deps, err := s.Deps(ctx, got.IDs[0]); err != nil || len(deps) != 1 || deps[0].To != is.ID || deps[0].Type != DepDiscoveredFrom {
				t.Errorf("Deps(%s) = %+v, %v, want it discovered from %s", got.IDs[0], deps, err, is.ID)
			}
			if tc.applied || tc.lapsed {
				if got.Comment != "" || got.Lapsed != tc.lapsed || after.Status != StatusClosed {
					t.Errorf("ReplayFinish = lapsed %t, comment %q, issue %s; want it applied, lapsed %t", got.Lapsed, got.Comment, after.Status, tc.lapsed)
				}
				if has := slices.Contains(ops, OpClaimLapsed); has != tc.lapsed {
					t.Errorf("events of the replay = %v; want claim.lapsed %t", ops, tc.lapsed)
				}
				return
			}
			if got.Comment == "" || got.Lapsed {
				t.Fatalf("ReplayFinish = lapsed %t, comment %q, want it kept as a comment", got.Lapsed, got.Comment)
			}
			if after.Status == StatusClosed && tc.why != "closed since" || after.CloseReason == f.Reason {
				t.Errorf("issue after a kept finish is %s (%q), want it as it was", after.Status, after.CloseReason)
			}
			cs, err := s.Comments(ctx, is.ID)
			if err != nil {
				t.Fatal(err)
			}
			kept := cs[len(cs)-1]
			if kept.ID != got.Comment || kept.Kind != CommentPlain || kept.Author != "alice" {
				t.Fatalf("last comment = %+v, want plain comment %s by alice", kept, got.Comment)
			}
			made := "made at epoch 1"
			if tc.epoch == 0 {
				made = "made with no epoch"
			}
			for _, want := range []string{tc.why, f.Reason, f.Handoff.Note, made} {
				if !strings.Contains(kept.Body, want) {
					t.Errorf("kept comment %q does not say %q", kept.Body, want)
				}
			}
			if slices.Contains(ops, OpIssueClose) || slices.Contains(ops, OpClaimLapsed) || !slices.Contains(ops, OpIssueKept) {
				t.Errorf("events of a kept finish = %v, want issue.kept and no close", ops)
			}
			if h, err := s.LastHandoff(ctx, is.ID); err != nil || h != nil && h.Body == f.Handoff.Note {
				t.Errorf("LastHandoff after a kept finish = %+v, %v; want the note kept as a comment, not a handoff", h, err)
			}
			assertTold(t, s, alice, is.ID, tc.why)
			if tc.told != (Actor{}) {
				assertTold(t, s, tc.told, is.ID, "kept")
			}
			if page, err := s.Inbox(ctx, Actor{Principal: "carol", Session: "s-c", Machine: "m"}, true, 0); err != nil || len(page.Items) != 0 {
				t.Errorf("carol's inbox = %+v, %v; the handoff to her was not made, so want nothing", page.Items, err)
			}
		})
	}
}

func TestReplayHandoff(t *testing.T) {
	t.Parallel()
	for _, tc := range lateCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, clk := clockStore(t)
			ctx := t.Context()
			is := mustCreate(t, s, NewIssue{Title: "work"})
			startAs(t, s, alice, is.ID, false)
			tc.after(t, s, clk, is.ID)
			seq := lastSeq(t, s)
			h := HandoffNote{Note: "half done offline", HandoffFields: HandoffFields{State: HandoffPartial}}
			got, err := s.ReplayHandoff(ctx, alice, is.ID, tc.epoch, h, true, "late-h", nil)
			if err != nil {
				t.Fatalf("ReplayHandoff = %v, want it applied or kept", err)
			}
			ops := opsSince(t, s, seq)
			last, err := s.LastHandoff(ctx, is.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.applied || tc.lapsed {
				if got.Comment != "" || got.Lapsed != tc.lapsed || last == nil || last.Body != h.Note {
					t.Errorf("ReplayHandoff = lapsed %t, comment %q, last handoff %+v; want it applied, lapsed %t", got.Lapsed, got.Comment, last, tc.lapsed)
				}
				if c, err := s.ClaimOf(ctx, is.ID); err != nil || c != nil {
					t.Errorf("ClaimOf after a releasing handoff = %+v, %v, want none", c, err)
				}
				if has := slices.Contains(ops, OpClaimLapsed); has != tc.lapsed {
					t.Errorf("events of the replay = %v; want claim.lapsed %t", ops, tc.lapsed)
				}
				return
			}
			if got.Comment == "" || last != nil && last.Body == h.Note {
				t.Fatalf("ReplayHandoff = comment %q, last handoff %+v; want it kept as a comment", got.Comment, last)
			}
			if !slices.Contains(ops, OpIssueKept) || slices.Contains(ops, OpClaimRelease) {
				t.Errorf("events of a kept handoff = %v, want issue.kept and no release", ops)
			}
			assertTold(t, s, alice, is.ID, tc.why)
			if tc.told != (Actor{}) {
				assertTold(t, s, tc.told, is.ID, "kept")
			}
		})
	}
}

// A handoff that keeps the claim and names no epoch is kept only when
// another principal holds the issue: otherwise it applies as a live one
// would, closed issue or not.
func TestReplayHandoffWithoutRelease(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	startAs(t, s, alice, is.ID, false)
	clk.add(20 * time.Minute)
	got, err := s.ReplayHandoff(ctx, alice, is.ID, 0, HandoffNote{Note: "notes, lapsed"}, false, "", nil)
	if err != nil || got.Comment != "" {
		t.Fatalf("ReplayHandoff of a note after the lease lapsed = %+v, %v, want it applied", got, err)
	}
	startAs(t, s, bob, is.ID, false)
	got, err = s.ReplayHandoff(ctx, alice, is.ID, 0, HandoffNote{Note: "notes, taken"}, false, "", nil)
	if err != nil || got.Comment == "" {
		t.Fatalf("ReplayHandoff of a note bob holds = %+v, %v, want it kept as a comment", got, err)
	}
}

// The fence is unchanged for a write that is not a replay: a live
// finish or handoff with a stale epoch, or on an issue another principal
// holds, is refused as before, and nothing is written.
func TestLiveFinishStillFenced(t *testing.T) {
	t.Parallel()
	for _, tc := range lateCases {
		if tc.applied || tc.lapsed {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, clk := clockStore(t)
			ctx := t.Context()
			is := mustCreate(t, s, NewIssue{Title: "work"})
			startAs(t, s, alice, is.ID, false)
			tc.after(t, s, clk, is.ID)
			seq := lastSeq(t, s)
			_, _, err := s.FinishIssue(ctx, alice, is.ID, tc.epoch, Finish{Reason: "live", Discovered: []NewIssue{{Title: "x"}}})
			var stale *StaleEpochError
			var forbidden *ForbiddenError
			var state *StateError
			if !errors.As(err, &stale) && !errors.As(err, &forbidden) && !errors.As(err, &state) {
				t.Errorf("live FinishIssue = %v, want it refused as stale, forbidden or closed", err)
			}
			_, err = s.HandoffIssue(ctx, alice, is.ID, tc.epoch, HandoffNote{Note: "live"}, true, "", nil)
			if !errors.As(err, &stale) && !errors.As(err, &forbidden) && !errors.As(err, &state) {
				t.Errorf("live HandoffIssue = %v, want it refused as stale, forbidden or closed", err)
			}
			if got := lastSeq(t, s); got != seq {
				t.Errorf("refused live writes recorded %d events, want none", got-seq)
			}
		})
	}
}

// A live finish after the lease lapsed, with nobody taking the issue
// since, applied before and still does; now its history says the lease
// had lapsed.
func TestLiveFinishAfterLapseIsMarked(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	c := startAs(t, s, alice, is.ID, false)
	clk.add(20 * time.Minute)
	seq := lastSeq(t, s)
	if _, _, err := s.FinishIssue(ctx, alice, is.ID, 1, Finish{Reason: "late but free"}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.History(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	var lapsed *Event
	for i := range evs {
		if evs[i].Seq > seq && evs[i].Op == OpClaimLapsed {
			lapsed = &evs[i]
		}
	}
	if lapsed == nil {
		t.Fatalf("history after a finish past the lease = %v, want a claim.lapsed event", opsSince(t, s, seq))
	}
	var b struct {
		Holder    Actor     `json:"holder"`
		Epoch     int64     `json:"epoch"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(lapsed.Before, &b); err != nil {
		t.Fatal(err)
	}
	if b.Holder != alice || b.Epoch != 1 || !b.ExpiresAt.Equal(c.ExpiresAt) {
		t.Errorf("claim.lapsed before = %+v, want the claim that lapsed, %+v", b, c)
	}
}

// A lease lapsed when the claim's holder had it until its lease ran out,
// and not when it let the claim go itself. A finish that names no epoch,
// as `sfx finish` without --epoch sends, was made under the claim of
// its own principal, if that is the one that lapsed.
func TestLapsedOnlyWhenTheLeaseRanOut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		epoch  int64
		before func(t *testing.T, s *Store, clk *clock, id IssueID)
		lapsed bool
	}{
		{name: "no epoch, hers lapsed", lapsed: true, before: func(_ *testing.T, _ *Store, clk *clock, _ IssueID) {
			clk.add(20 * time.Minute)
		}},
		{name: "no epoch, hers reaped", lapsed: true, before: func(t *testing.T, s *Store, clk *clock, _ IssueID) {
			clk.add(20 * time.Minute)
			if _, err := s.ReapClaims(t.Context()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no epoch, held still", before: func(*testing.T, *Store, *clock, IssueID) {}},
		{name: "epoch, released by her handoff", epoch: 1, before: func(t *testing.T, s *Store, _ *clock, id IssueID) {
			if _, err := s.HandoffIssue(t.Context(), alice, id, 1, HandoffNote{Note: "pausing"}, true, "", nil); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no epoch, bob's lapsed and reaped", before: func(t *testing.T, s *Store, clk *clock, id IssueID) {
			if _, err := s.HandoffIssue(t.Context(), alice, id, 1, HandoffNote{Note: "yours"}, true, "", nil); err != nil {
				t.Fatal(err)
			}
			startAs(t, s, bob, id, false)
			clk.add(20 * time.Minute)
			if _, err := s.ReapClaims(t.Context()); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, clk := clockStore(t)
			is := mustCreate(t, s, NewIssue{Title: "work"})
			startAs(t, s, alice, is.ID, false)
			tc.before(t, s, clk, is.ID)
			got, err := s.ReplayFinish(t.Context(), alice, is.ID, tc.epoch, Finish{Reason: "done"})
			if err != nil || got.Comment != "" || got.Lapsed != tc.lapsed {
				t.Errorf("ReplayFinish(epoch %d) = lapsed %t, comment %q, %v; want it applied, lapsed %t", tc.epoch, got.Lapsed, got.Comment, err, tc.lapsed)
			}
		})
	}
}

// A replayed finish sent again with its key, as an outbox does when the
// answer was lost, gives back the first result and writes nothing more:
// kept once, or applied once, also when the first attempt was live.
func TestReplayFinishOnce(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	kept := mustCreate(t, s, NewIssue{Title: "kept"})
	live := mustCreate(t, s, NewIssue{Title: "live"})
	startAs(t, s, alice, kept.ID, false)
	startAs(t, s, alice, live.ID, false)
	clk.add(20 * time.Minute)
	startAs(t, s, bob, kept.ID, false)
	f := Finish{Reason: "offline", Discovered: []NewIssue{{Title: "found"}}, IdempotencyKey: "once-1"}
	first, err := s.ReplayFinish(ctx, alice, kept.ID, 1, f)
	if err != nil || first.Comment == "" {
		t.Fatalf("ReplayFinish = %+v, %v, want it kept", first, err)
	}
	seq := lastSeq(t, s)
	again, err := s.ReplayFinish(ctx, alice, kept.ID, 1, f)
	if err != nil || again.Comment != first.Comment || !slices.Equal(again.IDs, first.IDs) {
		t.Errorf("ReplayFinish again = %+v, %v, want the first result %+v", again, err, first)
	}
	if got := lastSeq(t, s); got != seq {
		t.Errorf("a repeated replay recorded %d events, want none", got-seq)
	}

	g := Finish{Reason: "answer lost", IdempotencyKey: "once-2"}
	if _, _, err := s.FinishIssue(ctx, alice, live.ID, 1, g); err != nil {
		t.Fatal(err)
	}
	seq = lastSeq(t, s)
	got, err := s.ReplayFinish(ctx, alice, live.ID, 1, g)
	if err != nil || got.Comment != "" || got.Status != StatusClosed {
		t.Errorf("ReplayFinish of a finish applied live = %+v, %v, want the live result", got, err)
	}
	if n := lastSeq(t, s); n != seq {
		t.Errorf("replaying a finish applied live recorded %d events, want none", n-seq)
	}
}

// assertTold checks that a's inbox has a conflict item on id that says
// want.
func assertTold(t *testing.T, s *Store, a Actor, id IssueID, want string) {
	t.Helper()
	page, err := s.Inbox(t.Context(), a, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		if it.Kind == InboxConflict && it.Issue == id && strings.Contains(it.Body, want) {
			return
		}
	}
	t.Errorf("inbox of %s/%s = %+v, want a conflict item on %s saying %q", a.Principal, a.Session, page.Items, id, want)
}
