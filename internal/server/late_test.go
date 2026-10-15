package server

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// A finish or handoff replayed from an outbox carries park (protocol 8,
// design §18.6). After its claim's lease ran out it still applies if
// nobody took the issue, saying so (lapsed); if somebody did, it is kept
// as a comment, named in the result. The same request without park is
// fenced as it always was.
func TestLateFinishAndHandoff(t *testing.T) {
	t.Parallel()
	clk := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clk }
	advance := func(d time.Duration) { mu.Lock(); defer mu.Unlock(); clk = clk.Add(d) }
	s, _ := newServerClock(t, Limits{}, now)

	free := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "left alone"})
	taken := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "taken"})
	handed := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "handed"})
	for _, id := range []string{free.ID, taken.ID, handed.ID} {
		mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: id})
	}
	advance(20 * time.Minute) // past alice's 15-minute leases; the reaper is not running
	mustCall[proto.StartResult](t, s, bob, proto.OpStart, proto.StartArgs{ID: taken.ID})
	mustCall[proto.StartResult](t, s, bob, proto.OpStart, proto.StartArgs{ID: handed.ID})

	// Without park, as a live session sends them, the fence holds.
	live := proto.FinishArgs{ID: taken.ID, Epoch: 1, Reason: "done", Idem: "live-1"}
	if _, perr := call[proto.FinishResult](t, s, alice, proto.OpFinish, live); perr == nil ||
		perr.Code != proto.CodeForbidden && perr.Code != proto.CodeConflict {
		t.Errorf("live finish of an issue bob took = %+v, want it refused", perr)
	}
	if _, perr := call[proto.HandoffResult](t, s, alice, proto.OpHandoff, proto.HandoffArgs{ID: handed.ID, Epoch: 1, Note: "n", Release: true}); perr == nil ||
		perr.Code != proto.CodeForbidden && perr.Code != proto.CodeConflict {
		t.Errorf("live handoff of an issue bob took = %+v, want it refused", perr)
	}

	got := mustCall[proto.FinishResult](t, s, alice, proto.OpFinish,
		proto.FinishArgs{ID: free.ID, Epoch: 1, Reason: "done offline", Idem: "late-1", Park: true})
	if !got.Lapsed || got.Comment != "" {
		t.Errorf("replayed finish of an issue nobody took = %+v, want it applied, lapsed", got)
	}
	if sh := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: free.ID}); sh.Issue.Status != "closed" {
		t.Errorf("issue after a lapsed finish is %s, want closed", sh.Issue.Status)
	}

	got = mustCall[proto.FinishResult](t, s, alice, proto.OpFinish,
		proto.FinishArgs{ID: taken.ID, Epoch: 1, Reason: "done offline too", Idem: "late-2", Park: true,
			Discovered: []proto.Discovered{{Title: "found offline"}}})
	if got.Lapsed || got.Comment == "" || len(got.Created) != 1 {
		t.Errorf("replayed finish of an issue bob took = %+v, want it kept as a comment, its discovered issue created", got)
	}
	sh := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: taken.ID})
	if sh.Issue.Status != "in_progress" || sh.Claim == nil || sh.Claim.By != "bob" {
		t.Errorf("issue after a kept finish = %s, claim %+v; want bob's, in progress", sh.Issue.Status, sh.Claim)
	}
	inbox := mustCall[proto.InboxResult](t, s, bob, proto.OpInbox, proto.InboxArgs{})
	if !hasItem(inbox, "conflict", taken.ID, got.Comment) {
		t.Errorf("bob's inbox = %+v, want a conflict item naming comment %s", inbox.Items, got.Comment)
	}

	h := mustCall[proto.HandoffResult](t, s, alice, proto.OpHandoff,
		proto.HandoffArgs{ID: handed.ID, Epoch: 1, Note: "half done offline", Release: true, Idem: "late-3", Park: true})
	if h.Comment == "" || h.ID != handed.ID {
		t.Errorf("replayed handoff of an issue bob took = %+v, want it kept as a comment", h)
	}
	inbox = mustCall[proto.InboxResult](t, s, alice, proto.OpInbox, proto.InboxArgs{})
	if !hasItem(inbox, "conflict", handed.ID, h.Comment) {
		t.Errorf("alice's inbox = %+v, want a conflict item naming comment %s", inbox.Items, h.Comment)
	}
}

// hasItem reports whether an inbox page has an item of kind on issue
// whose body says what.
func hasItem(r proto.InboxResult, kind, issue, what string) bool {
	for _, it := range r.Items {
		if it.Kind == kind && it.Issue == issue && strings.Contains(it.Body, what) {
			return true
		}
	}
	return false
}
