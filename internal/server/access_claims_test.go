package server

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
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
	t.Parallel()
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
	t.Parallel()
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
	released(t, r, ids[0], "bob", "ariesd", "access", "membership removed")
	released(t, r, ids[1], "carol", "ariesd", "access", "principal disabled")
	if got := claimOf(r, ids[2]); got != "erin" {
		t.Errorf("erin, who still has access, lost her claim: %s is held by %q", ids[2], got)
	}
	if !strings.Contains(m.log.String(), "issue="+ids[0]+` principal=bob session=s-b epoch=1 by=ariesd reason="membership removed"`) {
		t.Errorf("the log lacks the release of bob's claim:\n%s", m.log.String())
	}

	// An archive is the exception (decision 24): nobody can connect to
	// an archived project, so its store would not open again until it
	// was activated. The archive opens a closed store, which ends the
	// claims as every opening does, under the server's name, and closes
	// it again. They are gone before the project is activated.
	r.hangUp()
	ck.advance(2 * time.Minute)
	m.CloseIdleStores()
	opened = m.opened(projB)
	// The result counts the claims the admin's own release ended.
	if arch := mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projB}); arch.Released != 0 {
		t.Errorf("project.archive with the store closed released %d claims as the admin's, want 0: the opening ends them as the server's", arch.Released)
	}
	m.stores.closeWhere("test", func(*entry) bool { return false }) // waits for the store to close
	if got := m.opened(projB) - opened; got != 1 {
		t.Errorf("the archive opened the closed store %d times, want once", got)
	}
	if n := m.OpenStores(); n != 1 {
		t.Errorf("%d stores open after the archive, want only a's", n)
	}
	st := m.inStore(t, "proj-b")
	if cl, err := st.ClaimOf(t.Context(), store.IssueID(ids[2])); err != nil || cl != nil {
		t.Errorf("after the archive, with the project still archived, the claim on %s is %+v (err %v), want none", ids[2], cl, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	mustAsk[proto.ProjectResult](d, proto.OpProjectActivate, proto.ProjectArgs{Project: projB})
	released(t, mustDialTo(t, m.Server, dana, projB), ids[2], "erin", "ariesd", "access", "project archived")
}

// TestSweep checks the reaper tick's sweep: it ends the claims of
// holders without access in the stores that are open, and no others.
func TestSweep(t *testing.T) {
	t.Parallel()
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
	released(t, d, bobs, "bob", "ariesd", "access", "membership removed")
	if got := claimOf(d, carols); got != "carol" {
		t.Errorf("the sweep ended carol's claim: held by %q", got)
	}
	// A server of one project has nothing to sweep.
	newServer(t).Sweep(t.Context())
}

// asIs opens a project's database without migrating it, as a daemon
// opens an archived project's at its start.
func (m *multi) asIs() func(context.Context, store.Project) (Store, error) {
	return ProjectOpener(m.dsn, m.ctl, store.Options{CommitInterval: -1, NoMigrate: true}, m.tel)
}

// inStore opens the database of the project named name directly, as the
// next open will find it, whatever the project's state.
func (m *multi) inStore(t *testing.T, name string) *store.Store {
	t.Helper()
	p, ok := m.ctl.ProjectNamed(name)
	if !ok {
		t.Fatalf("no project %s", name)
	}
	st, err := m.asIs()(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return realStore(st)
}

// historyOps returns the events of issue id as "OP by PRINCIPAL/SESSION".
func historyOps(t *testing.T, st *store.Store, id string) []string {
	t.Helper()
	evs, err := st.History(t.Context(), store.IssueID(id))
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, e := range evs {
		ops = append(ops, string(e.Op)+" by "+e.Actor.Principal+"/"+e.Actor.Session)
	}
	return ops
}

// TestReleaseArchived pins decision 24: before it listens, a daemon ends
// every claim in every archived project, as the server's own release for
// "project archived", so that a daemon that stopped between an archive's
// control write and its release leaves no claim behind once it is back.
func TestReleaseArchived(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	a, c, e := mustDialTo(t, m.Server, alice, projA), mustDialTo(t, m.Server, carol, projB), mustDialTo(t, m.Server, erin, projB)
	alices, carols, erins := held(a, "alice's"), held(c, "carol's"), held(e, "erin's")

	// With nothing archived there is nothing to end, and no store is opened.
	opens := 0
	open := func(ctx context.Context, p store.Project) (Store, error) {
		opens++
		return m.asIs()(ctx, p)
	}
	if err := m.ReleaseArchived(t.Context(), open); err != nil {
		t.Fatal(err)
	}
	if opens != 0 {
		t.Errorf("ReleaseArchived with no archived project opened %d stores, want 0", opens)
	}

	// The daemon stops after the archive's control write and before its
	// release: the archive is in the control store alone, and the stores
	// close with the claims in place.
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projB); err != nil {
		t.Fatal(err)
	}
	// An archived project whose database cannot be opened is logged and
	// does not stop the others. It sorts before proj-b.
	const projC = "00000000-0000-4000-8000-00000000000c"
	if _, err := m.ctl.AddProject(t.Context(), dana, store.NewProject{ID: projC, Name: "proj-0", Prefix: "pc",
		Account: "internal", DBName: "never_granted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projC); err != nil {
		t.Fatal(err)
	}
	c.hangUp()
	e.hangUp()
	m.stores.closeAll()

	if err := m.ReleaseArchived(t.Context(), open); err != nil {
		t.Fatal(err)
	}
	if opens != 2 {
		t.Errorf("ReleaseArchived opened %d stores, want 2: one for each archived project, none for an active one", opens)
	}
	if !strings.Contains(m.log.String(), `msg="end the claims of an archived project at start" event=access.sweep_failed project=`+projC) {
		t.Errorf("no log line for the archived project that could not be opened:\n%s", m.log.String())
	}
	st := m.inStore(t, "proj-b")
	for id, who := range map[string]string{carols: "carol", erins: "erin"} {
		cl, err := st.ClaimOf(t.Context(), store.IssueID(id))
		if err != nil {
			t.Fatal(err)
		}
		if cl != nil {
			t.Errorf("%s is still claimed by %s in the archived project after ReleaseArchived", id, cl.Holder.Principal)
		}
		want := []string{"issue.create by " + who + "/s-" + who[:1], "claim.take by " + who + "/s-" + who[:1], "issue.update by " + who + "/s-" + who[:1],
			"claim.release by ariesd/access", "issue.update by ariesd/access"}
		if got := historyOps(t, st, id); !slices.Equal(got, want) {
			t.Errorf("the history of %s:\n%v\nwant\n%v", id, got, want)
		}
	}
	if !strings.Contains(m.log.String(), "issue="+carols+` principal=carol session=s-c epoch=1 by=ariesd reason="project archived"`) {
		t.Errorf("the log lacks the release of carol's claim:\n%s", m.log.String())
	}
	if got := claimOf(a, alices); got != "alice" {
		t.Errorf("the claim in an active project is held by %q after ReleaseArchived, want alice", got)
	}

	// It is safe to repeat: a second run ends nothing and records
	// nothing, and does not open the store of a project with no claim.
	// The one that could not be read is tried again.
	if err := m.ReleaseArchived(t.Context(), open); err != nil {
		t.Fatal(err)
	}
	if opens != 3 {
		t.Errorf("after a second ReleaseArchived %d stores were opened in all, want 3: the unreadable project's again, and not proj-b's", opens)
	}
	want := []string{"issue.create by carol/s-c", "claim.take by carol/s-c", "issue.update by carol/s-c",
		"claim.release by ariesd/access", "issue.update by ariesd/access"}
	if got := historyOps(t, st, carols); !slices.Equal(got, want) {
		t.Errorf("after a second ReleaseArchived the history of %s is\n%v\nwant\n%v", carols, got, want)
	}

	// The holder is told, where she reads it once the project is back.
	d := mustDialTo(t, m.Server, dana, projA)
	mustAsk[proto.ProjectResult](d, proto.OpProjectActivate, proto.ProjectArgs{Project: projB})
	released(t, mustDialTo(t, m.Server, dana, projB), carols, "carol", "ariesd", "access", "project archived")
	in := mustAsk[proto.InboxResult](mustDialTo(t, m.Server, carol, projB), proto.OpInbox, proto.InboxArgs{})
	if len(in.Items) != 1 || in.Items[0].Kind != "claim.lost" || in.Items[0].Issue != carols ||
		!strings.Contains(in.Items[0].Body, "project archived by ariesd") {
		t.Errorf("carol's inbox = %+v; want one claim.lost for %s saying project archived by ariesd", in.Items, carols)
	}

	// A server of one project has nothing archived.
	if err := newServer(t).ReleaseArchived(t.Context(), open); err != nil {
		t.Errorf("ReleaseArchived on a server of one project: %v", err)
	}
}

// TestReleaseArchivedDoesNotMigrate pins the other half of decision 24:
// the database of an archived project that an older ariesd last
// migrated is left exactly as it is, claims included. Activating the
// project migrates it and ends them, as before.
func TestReleaseArchivedDoesNotMigrate(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	e := mustDialTo(t, m.Server, erin, projB)
	erins := held(e, "erin's")
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projB); err != nil {
		t.Fatal(err)
	}
	e.hangUp()
	m.stores.closeAll()

	// One migration behind, as an older binary left it. Deleting the
	// record leaves the tables, so the claim can still be read.
	p, _ := m.ctl.ProjectNamed("proj-b")
	dsn, err := DatabaseDSN(m.dsn, p.DBName)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	version := func() (n int) {
		t.Helper()
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	last := version()
	if _, err := db.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE version = ?`, last); err != nil {
		t.Fatal(err)
	}

	if err := m.ReleaseArchived(t.Context(), m.asIs()); err != nil {
		t.Fatal(err)
	}
	if got := version(); got != last-1 {
		t.Errorf("ReleaseArchived left the archived project's database at version %d, want %d: it must not migrate", got, last-1)
	}
	var claims int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM claims WHERE principal IS NOT NULL`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 1 {
		t.Errorf("%d claims in the database ReleaseArchived must not touch, want erin's 1 (%s)", claims, erins)
	}
	if !strings.Contains(m.log.String(), `msg="archived project not opened at start: its database awaits this ariesd's migrations, and any claim in it ends when the project is activated" event=project.not_opened project=`+projB) {
		t.Errorf("no log line saying the archived project was left alone:\n%s", m.log.String())
	}
}

// TestArchivesTogetherLeaveOneProject sends the archive of each of a
// server's two projects at once. Each alone is allowed, and both would
// leave no active project: nobody could connect again, and the daemon
// would not start. Whatever the order, one is refused.
func TestArchivesTogetherLeaveOneProject(t *testing.T) {
	t.Parallel()
	for range 5 {
		m := adminWorld(t, Limits{})
		conns := []*conn{mustDialTo(t, m.Server, dana, projA), mustDialTo(t, m.Server, dana, projB)}
		errs := make([]*proto.Error, 2)
		var wg sync.WaitGroup
		for i, id := range []string{projA, projB} {
			wg.Go(func() {
				_, errs[i] = ask[proto.ProjectResult](conns[i], proto.OpProjectArchive, proto.ProjectArgs{Project: id})
			})
		}
		wg.Wait()
		if got := m.ctl.ActiveProjects(); got != 1 {
			t.Fatalf("after two archives sent together the server has %d active projects, want 1 (answers: %v, %v)", got, errs[0], errs[1])
		}
		refused := 0
		for _, e := range errs {
			if e != nil {
				refused++
				if e.Code != proto.CodeInvalid || !strings.Contains(e.Message, "only active project") {
					t.Errorf("the archive that lost = %+v, want invalid: the server's only active project", e)
				}
			}
		}
		if refused != 1 {
			t.Errorf("%d of the two archives were refused, want exactly 1", refused)
		}
	}
}
