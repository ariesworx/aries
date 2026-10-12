package server

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// held starts a new issue as c's principal, with an hour's lease, and
// returns its id.
func held(c *conn, title string) string {
	c.t.Helper()
	is := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: title})
	mustAsk[proto.StartResult](c, proto.OpStart, proto.StartArgs{ID: is.ID, Lease: "1h"})
	return is.ID
}

// claimOf returns who holds issue id, as reader sees it, or "".
func claimOf(reader *conn, id string) string {
	reader.t.Helper()
	sh := mustAsk[proto.ShowResult](reader, proto.OpShow, proto.ShowArgs{ID: id})
	if sh.Claim == nil {
		return ""
	}
	return sh.Claim.By
}

// released checks that issue id's claim, which holder held, ended as
// decision 21 says: a claim.release event by by, giving reason, then
// the issue back to open, and the issue ready for the next person.
func released(t *testing.T, reader *conn, id, holder, by, bySession, reason string) {
	t.Helper()
	sh := mustAsk[proto.ShowResult](reader, proto.OpShow, proto.ShowArgs{ID: id})
	if sh.Claim != nil || sh.Issue.Status != "open" || sh.Issue.Assignee != "" {
		t.Errorf("%s: claim %+v, status %s, assignee %q; want no claim, open and unassigned", id, sh.Claim, sh.Issue.Status, sh.Issue.Assignee)
	}
	h := mustAsk[proto.HistoryResult](reader, proto.OpHistory, proto.PageArgs{ID: id})
	var rel *proto.Event
	var after []string
	for i, e := range h.Events {
		if e.Op == string(store.OpClaimRelease) {
			rel = &h.Events[i]
			after = nil
			continue
		}
		if rel != nil {
			after = append(after, e.Op)
		}
	}
	switch {
	case rel == nil:
		t.Errorf("%s has no claim.release event: %+v", id, h.Events)
	case rel.Principal != by || rel.Session != bySession || !strings.Contains(string(rel.Before), `"principal":"`+holder+`"`) ||
		!strings.Contains(string(rel.After), `"reason":"`+reason+`"`):
		t.Errorf("%s: claim.release by %s/%s, before %s, after %s; want by %s/%s, the holder %s and the reason %q",
			id, rel.Principal, rel.Session, rel.Before, rel.After, by, bySession, holder, reason)
	case !slices.Equal(after, []string{string(store.OpIssueUpdate)}):
		t.Errorf("%s: after its claim.release come %v, want the issue's return to open", id, after)
	}
	ready := mustAsk[proto.ListResult](reader, proto.OpReady, proto.LimitArgs{})
	if !slices.ContainsFunc(ready.Issues, func(s proto.Summary) bool { return s.ID == id }) {
		t.Errorf("%s is not ready after its claim ended: %+v", id, ready.Issues)
	}
}

// TestAccessEndsClaims pins decision 21: a membership removed, a
// principal disabled and a project archived each end the claims the
// lost access held, and only those; a demotion ends none.
func TestAccessEndsClaims(t *testing.T) {
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projA)
	dB := mustDialTo(t, m.Server, dana, projB)
	a, b, c := mustDialTo(t, m.Server, alice, projA), mustDialTo(t, m.Server, bob, projA), mustDialTo(t, m.Server, carol, projA)
	cB, e := mustDialTo(t, m.Server, carol, projB), mustDialTo(t, m.Server, erin, projB)
	alices, bobs, carols := held(a, "alice's"), held(b, "bob's"), held(c, "carol's in a")
	carolsB, erins := held(cB, "carol's in b"), held(e, "erin's in b")

	// A demotion: alice is still a member, and keeps her claim.
	mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "alice", Role: "member"})
	if got := claimOf(d, alices); got != "alice" {
		t.Errorf("after alice's demotion her issue is held by %q, want alice", got)
	}

	// A membership removed: bob's claim in the project ends, by the admin.
	rm := mustAsk[proto.MemberRemoveResult](d, proto.OpMemberRemove, proto.MemberArgs{Name: "bob"})
	if rm.Released != 1 {
		t.Errorf("member.remove bob released %d claims, want 1", rm.Released)
	}
	released(t, d, bobs, "bob", "dana", dana.Session, "membership removed")
	for id, who := range map[string]string{alices: "alice", carols: "carol"} {
		if got := claimOf(d, id); got != who {
			t.Errorf("after bob's removal %s is held by %q, want %s", id, got, who)
		}
	}
	// bob is told, where he will read it if he is let back in.
	mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "bob"})
	in := mustAsk[proto.InboxResult](mustDialTo(t, m.Server, bob, projA), proto.OpInbox, proto.InboxArgs{})
	if len(in.Items) != 1 || in.Items[0].Kind != "claim.lost" || in.Items[0].Issue != bobs ||
		!strings.Contains(in.Items[0].Body, "membership removed by dana") {
		t.Errorf("bob's inbox = %+v; want one claim.lost for %s saying membership removed by dana", in.Items, bobs)
	}

	// A principal disabled: carol's claims end in every project.
	dis := mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "carol"})
	if dis.Released != 2 {
		t.Errorf("principal.disable carol released %d claims, want 2: one in each project", dis.Released)
	}
	released(t, d, carols, "carol", "dana", dana.Session, "principal disabled")
	released(t, dB, carolsB, "carol", "dana", dana.Session, "principal disabled")
	if got := claimOf(dB, erins); got != "erin" {
		t.Errorf("after carol was disabled erin's issue is held by %q, want erin", got)
	}

	// A project archived: every claim in it ends. The project is read
	// again once it is activated.
	arch := mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projB})
	if arch.Released != 1 {
		t.Errorf("project.archive released %d claims, want 1: erin's", arch.Released)
	}
	mustAsk[proto.ProjectResult](d, proto.OpProjectActivate, proto.ProjectArgs{Project: projB})
	released(t, mustDialTo(t, m.Server, dana, projB), erins, "erin", "dana", dana.Session, "project archived")
	if got := claimOf(d, alices); got != "alice" {
		t.Errorf("after another project was archived alice's issue is held by %q, want alice", got)
	}
}

// TestAccessEndsClaimsOfAClosedStore is the case where the project's
// store is not open when the access ends: nothing can be released then,
// so the claims end when the store next opens, before its first
// request, under the server's own name.
func TestAccessEndsClaimsOfAClosedStore(t *testing.T) {
	ck := newClock()
	m := newMulti(t, Limits{StoreIdle: Duration(time.Minute)}, ck)
	m.project(t, projA, "alice")
	m.project(t, projB, "bob", "carol", "erin")
	d := mustDialTo(t, m.Server, dana, projA)

	// bob, carol and erin each hold an issue in b for a day, and leave.
	var ids []string
	for _, who := range []store.Actor{bob, carol, erin} {
		c := mustDialTo(t, m.Server, who, projB)
		is := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: who.Principal + "'s"})
		mustAsk[proto.StartResult](c, proto.OpStart, proto.StartArgs{ID: is.ID, Lease: "24h"})
		ids = append(ids, is.ID)
		c.hangUp()
	}
	ck.advance(2 * time.Minute)
	m.CloseIdleStores()
	if n := m.OpenStores(); n != 1 {
		t.Fatalf("%d stores open, want only a's: b's must be closed for this test", n)
	}
	opened := m.opened(projB)

	// bob's membership ends and carol is disabled while b's store is
	// closed. Nothing is released, and the store is not opened for it.
	if rm := mustAsk[proto.MemberRemoveResult](d, proto.OpMemberRemove, proto.MemberArgs{Name: "bob", Project: projB}); rm.Released != 0 {
		t.Errorf("member.remove with the project's store closed released %d claims, want 0", rm.Released)
	}
	if dis := mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "carol"}); dis.Released != 0 {
		t.Errorf("principal.disable with the project's store closed released %d claims, want 0", dis.Released)
	}
	if got := m.opened(projB); got != opened {
		t.Errorf("ending access opened b's store %d times, want none", got-opened)
	}

	// The next connection opens the store, which ends their claims first.
	r := mustDialTo(t, m.Server, dana, projB)
	released(t, r, ids[0], "bob", "starfixd", "access", "membership removed")
	released(t, r, ids[1], "carol", "starfixd", "access", "principal disabled")
	if got := claimOf(r, ids[2]); got != "erin" {
		t.Errorf("erin, who still has access, lost her claim: %s is held by %q", ids[2], got)
	}
	if !strings.Contains(m.log.String(), "issue="+ids[0]+` principal=bob session=s-b epoch=1 by=starfixd reason="membership removed"`) {
		t.Errorf("the log lacks the release of bob's claim:\n%s", m.log.String())
	}

	// Archived while its store is closed, a project's claims end when it
	// is activated, which opens it.
	r.hangUp()
	ck.advance(2 * time.Minute)
	m.CloseIdleStores()
	if arch := mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projB}); arch.Released != 0 {
		t.Errorf("project.archive with the store closed released %d claims, want 0", arch.Released)
	}
	mustAsk[proto.ProjectResult](d, proto.OpProjectActivate, proto.ProjectArgs{Project: projB})
	released(t, mustDialTo(t, m.Server, dana, projB), ids[2], "erin", "starfixd", "access", "project archived")
}

// TestSweep checks the reaper tick's sweep: it ends the claims of
// holders without access in the stores that are open, and no others.
func TestSweep(t *testing.T) {
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projA)
	b, c := mustDialTo(t, m.Server, bob, projA), mustDialTo(t, m.Server, carol, projA)
	bobs, carols := held(b, "bob's"), held(c, "carol's")
	m.Sweep(t.Context())
	if bh, ch := claimOf(d, bobs), claimOf(d, carols); bh != "bob" || ch != "carol" {
		t.Fatalf("a sweep with everyone's access in place left the claims with %q and %q", bh, ch)
	}
	// The membership ends in the control store alone, as it does when the
	// daemon stops between the change and the release.
	if _, err := m.ctl.RemoveMember(t.Context(), dana, "bob", projA); err != nil {
		t.Fatal(err)
	}
	m.Sweep(t.Context())
	released(t, d, bobs, "bob", "starfixd", "access", "membership removed")
	if got := claimOf(d, carols); got != "carol" {
		t.Errorf("the sweep ended carol's claim: held by %q", got)
	}
	// A server of one project has nothing to sweep.
	newServer(t).Sweep(t.Context())
}
