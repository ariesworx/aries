package server

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// byName indexes a status report's projects.
func byName(res proto.StatusResult) map[string]proto.ProjectStatus {
	out := map[string]proto.ProjectStatus{}
	for _, p := range res.Projects {
		out[p.Name] = p
	}
	return out
}

// TestStatus reports the server and its projects to a server admin, a
// project admin's own projects to them, and nothing to anyone else. It
// opens no store to do so.
func TestStatus(t *testing.T) {
	t.Parallel()
	ck := newClock()
	m := newMulti(t, Limits{OpenStores: 8, Conns: 50}, ck)
	m.project(t, projA, "alice=admin", "bob", "carol")
	m.project(t, projB, "erin=admin", "carol")
	m.project(t, projC, "erin=admin")
	for _, write := range []func() error{
		func() error { _, err := m.ctl.ArchiveProject(t.Context(), dana, projC); return err },
		func() error { _, err := m.ctl.AddPrincipal(t.Context(), dana, "frank", ""); return err },
		func() error { _, err := m.ctl.DisablePrincipal(t.Context(), dana, "frank"); return err },
		func() error { _, err := m.ctl.AddKey(t.Context(), dana, "carol", keyLine(t, "carol")); return err },
	} {
		if err := write(); err != nil {
			t.Fatal(err)
		}
	}
	d := mustDialTo(t, m.Server, dana, projA)
	byFile := mustDialTo(t, m.Server, carol, projA)
	// A session of its own, so that its handshake writes a registry row.
	_, byKey, _ := dialKey(t, m.Server, store.Actor{Principal: "carol", Session: "s-c2", Machine: "laptop-c"}, projA, fingerprintOf(t, "carol"))
	b := mustDialTo(t, m.Server, bob, projA)
	for _, c := range []*conn{byFile, byKey, b} {
		if !c.alive() {
			t.Fatal("a connection to project a is not open")
		}
	}
	mustAsk[proto.CreateResult](b, proto.OpCreate, proto.CreateArgs{Title: "one"})
	mustAsk[proto.CreateResult](b, proto.OpCreate, proto.CreateArgs{Title: "two"})
	ck.advance(90 * time.Minute)

	t.Run("server admin", func(t *testing.T) {
		res := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
		sv := res.Server
		if sv == nil {
			t.Fatal("status for a server admin has no server part")
		}
		if sv.Version != "v0.2.0" || sv.ProtoMin != proto.ProtoMin || sv.ProtoMax != proto.ProtoMax {
			t.Errorf("status names %s, protocol %d to %d; want v0.2.0, %d to %d", sv.Version, sv.ProtoMin, sv.ProtoMax, proto.ProtoMin, proto.ProtoMax)
		}
		if sv.OpenStores != 1 || sv.OpenStoresMax != 8 || sv.Conns != 4 || sv.ConnsMax != 50 {
			t.Errorf("status = %d/%d stores, %d/%d connections; want 1/8 and 4/50", sv.OpenStores, sv.OpenStoresMax, sv.Conns, sv.ConnsMax)
		}
		// The file was written at the start, before carol had her key.
		if k := sv.KeysFile; k == nil || k.Path != m.keysFile || k.Keys != 0 || k.Failed || k.WrittenAt == nil {
			t.Errorf("status of the keys file = %+v; want %s, written with no key yet", k, m.keysFile)
		}
		if sv.UptimeSeconds != 90*60 {
			t.Errorf("uptime %ds, want 90 minutes on the server's clock", sv.UptimeSeconds)
		}
		if sv.HeapBytes == 0 || sv.SysBytes < sv.HeapBytes || sv.Goroutines < 4 {
			t.Errorf("status = heap %d, sys %d, %d goroutines; want ariesd's own, from the runtime", sv.HeapBytes, sv.SysBytes, sv.Goroutines)
		}
		if want := map[string]int{"active": 2, "archived": 1}; !maps.Equal(sv.Projects, want) {
			t.Errorf("projects by state = %v, want %v", sv.Projects, want)
		}
		if want := map[string]int{"active": 4, "disabled": 1}; !maps.Equal(sv.Principals, want) {
			t.Errorf("principals by state = %v, want %v", sv.Principals, want)
		}

		ps := byName(res)
		if len(res.Projects) != 3 || res.More != 0 || res.Projects[0].Name != "proj-a" || res.Projects[2].Name != "proj-c" {
			t.Fatalf("status lists %+v and %d more; want proj-a, proj-b and proj-c, by name", res.Projects, res.More)
		}
		a := ps["proj-a"]
		if a.ID != projA || a.State != "active" || !a.Open || a.Conns != 4 || a.FileConns != 3 {
			t.Errorf("proj-a = %+v; want it active and open, with 4 connections, 3 of them by the authorized_keys file", a)
		}
		if a.Sessions == nil || *a.Sessions != 0 {
			t.Errorf("proj-a sessions = %v; want 0: the last was seen 90 minutes ago", a.Sessions)
		}
		if a.LastUsed == nil || !a.LastUsed.Equal(ck.now()) {
			t.Errorf("proj-a last used %v, want now (%s): it is answering this request", a.LastUsed, ck.now())
		}
		// Each handshake wrote a registry row, and bob two issues.
		if w := a.Writes; w == nil || w.LastMinute != 6 || w.Samples != 6 || w.P50Micros <= 0 || w.P99Micros < w.P50Micros {
			t.Errorf("proj-a writes = %+v; want 6 in the last minute, and their latency", w)
		}
		for _, name := range []string{"proj-b", "proj-c"} {
			p := ps[name]
			if p.Open || p.Conns != 0 || p.FileConns != 0 || p.Sessions != nil || p.Writes != nil || p.LastUsed != nil {
				t.Errorf("%s = %+v; want a closed store, and nothing that needs one", name, p)
			}
		}
		if ps["proj-b"].State != "active" || ps["proj-c"].State != "archived" {
			t.Errorf("states = %s and %s, want proj-b active and proj-c archived", ps["proj-b"].State, ps["proj-c"].State)
		}
		if m.opened(projB) != 0 || m.opened(projC) != 0 {
			t.Errorf("status opened the store of a project to report on it: b %d times, c %d", m.opened(projB), m.opened(projC))
		}
	})

	t.Run("sessions seen lately", func(t *testing.T) {
		mustDialTo(t, m.Server, bob, projA).alive()
		res := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
		if a := byName(res)["proj-a"]; a.Sessions == nil || *a.Sessions != 1 {
			t.Errorf("proj-a sessions = %v, want 1: bob's new connection", a.Sessions)
		}
	})

	t.Run("pages", func(t *testing.T) {
		res := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{AdminListArgs: proto.AdminListArgs{Limit: 1, After: "proj-a"}})
		if len(res.Projects) != 1 || res.Projects[0].Name != "proj-b" || res.More != 1 || res.Server == nil {
			t.Errorf("status after proj-a, one = %+v and %d more; want proj-b and 1 more, with the server", res.Projects, res.More)
		}
		_, perr := ask[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{AdminListArgs: proto.AdminListArgs{Limit: proto.AdminListMax + 1}})
		if perr == nil || perr.Code != proto.CodeInvalid || perr.Fix == "" {
			t.Errorf("status with a limit past the most = %+v, want invalid with a fix", perr)
		}
	})

	t.Run("project admin", func(t *testing.T) {
		e := mustDialTo(t, m.Server, erin, projB)
		res := mustAsk[proto.StatusResult](e, proto.OpStatus, proto.StatusArgs{})
		if res.Server != nil {
			t.Errorf("status for a project admin has the server's figures: %+v", res.Server)
		}
		// proj-c is erin's too, and archived: like `project list`, status
		// shows a project admin only what is served.
		if len(res.Projects) != 1 || res.Projects[0].Name != "proj-b" || !res.Projects[0].Open || res.Projects[0].Conns != 1 {
			t.Errorf("status for erin = %+v; want proj-b alone, open, with her connection", res.Projects)
		}
		a := mustDialTo(t, m.Server, alice, projA)
		res = mustAsk[proto.StatusResult](a, proto.OpStatus, proto.StatusArgs{})
		if names := slices.Sorted(maps.Keys(byName(res))); !slices.Equal(names, []string{"proj-a"}) || res.Server != nil {
			t.Errorf("status for alice = %v (server %v); want proj-a alone", names, res.Server)
		}
	})

	t.Run("anyone else", func(t *testing.T) {
		before, err := m.ctl.Events(t.Context(), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []*conn{b, byFile} { // a member of one project, and of two
			_, perr := ask[proto.StatusResult](c, proto.OpStatus, proto.StatusArgs{})
			if perr == nil || perr.Code != proto.CodeForbidden || !strings.Contains(perr.Message, "may not see the server's status") ||
				perr.Fix != "ask a server admin, or an admin of the project you want to know about, to run `aries admin status`" {
				t.Errorf("status for a member = %+v; want forbidden, with who to ask", perr)
			}
		}
		after, err := m.ctl.Events(t.Context(), 0, 1000)
		if err != nil || len(after) != len(before) {
			t.Errorf("status recorded %d events (%v), want none", len(after)-len(before), err)
		}
	})

	t.Run("a disabled server admin's role does not count", func(t *testing.T) {
		ms := newMultiAdmins(t, Limits{}, nil, dana.Principal, sam.Principal)
		ms.project(t, projA, "alice")
		c := mustDialTo(t, ms.Server, sam, projA)
		if _, err := ms.ctl.AddPrincipal(t.Context(), dana, "sam", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := ms.ctl.DisablePrincipal(t.Context(), dana, "sam"); err != nil {
			t.Fatal(err)
		}
		if _, perr := ask[proto.StatusResult](c, proto.OpStatus, proto.StatusArgs{}); perr == nil || perr.Code != proto.CodeForbidden {
			t.Errorf("status for a disabled server admin on a connection still open = %+v, want forbidden", perr)
		}
	})
}

// A status request is a read: it is not counted against the control
// write bucket, and a server of one project refuses it as it refuses
// every admin op (TestAdminNeedsManyProjects).
func TestStatusIsARead(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{ControlWriteRate: 0.001, ControlWriteBurst: 1, WriteRate: 0.001, WriteBurst: 1})
	d := mustDialTo(t, m.Server, dana, projA)
	for range 5 {
		mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
	}
}

// TestFileConnections pins what the daemon says of connections that
// named no key of the store: sshd let them in by the hand-maintained
// authorized_keys file, so removing a key from the store does not close
// them. `keys` counts them for a principal and `status` for a project.
// Whether the key removed is itself in that file is another question,
// which the daemon answers by reading it (TestKeyAlsoInAuthorizedKeys).
func TestFileConnections(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{}) // carol has a key in the store
	carols := fingerprintOf(t, "carol")
	d := mustDialTo(t, m.Server, dana, projA)
	_, byKey, _ := dialKey(t, m.Server, carol, projA, carols)
	if !byKey.alive() {
		t.Fatal("the connection by carol's key is not open")
	}
	keys := func() proto.KeysResult {
		t.Helper()
		return mustAsk[proto.KeysResult](d, proto.OpKeys, proto.KeysArgs{Name: "carol"})
	}

	// Only connections by the store's keys: nothing to count.
	if got := keys(); got.FileConns != 0 {
		t.Errorf("keys of carol counts %d connections by the hand-maintained file, want none", got.FileConns)
	}

	// Two connections by that file, to two projects, and one by the key.
	fileA, fileB := mustDialTo(t, m.Server, carol, projA), mustDialTo(t, m.Server, carol, projB)
	for _, c := range []*conn{fileA, fileB, mustDialTo(t, m.Server, bob, projA)} {
		if !c.alive() {
			t.Fatal("a connection is not open")
		}
	}
	if got := keys(); got.FileConns != 2 {
		t.Errorf("keys of carol counts %d connections by the hand-maintained file, want 2", got.FileConns)
	}
	// carol sees the count for herself; it is hers.
	if own := mustAsk[proto.KeysResult](fileA, proto.OpKeys, proto.KeysArgs{Name: "carol"}); own.FileConns != 2 {
		t.Errorf("carol's own key list counts %d connections by the hand-maintained file, want 2", own.FileConns)
	}
	// The removal closes the connection the key signed in and no other,
	// and warns of nothing: the file does not hold this key.
	rm := mustAsk[proto.KeyResult](d, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: carols})
	if rm.Dropped != 1 || rm.InAuthorizedKeys || rm.Warning != "" {
		t.Errorf("key.remove = %+v; want 1 dropped and no warning", rm)
	}
	if !byKey.dropped() || !fileA.alive() || !fileB.alive() {
		t.Error("key.remove did not close exactly the connection the key had authenticated")
	}

	fileB.hangUp()
	waitUntil(t, "carol's connection to b is forgotten", func() bool { return m.connections("carol", projB) == 0 })
	if got := keys(); got.FileConns != 1 {
		t.Errorf("keys of carol counts %d connections by the hand-maintained file after one hung up, want 1", got.FileConns)
	}

	// Status counts them by project.
	res := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
	if a, b := byName(res)["proj-a"], byName(res)["proj-b"]; a.FileConns != 3 || a.Conns != 3 || b.FileConns != 0 {
		t.Errorf("status = proj-a %d of %d by the hand-maintained file, proj-b %d; want 3 of 3 (dana, carol and bob) and 0", a.FileConns, a.Conns, b.FileConns)
	}
}

// A request in flight when its project is archived may take a claim
// after the admin's release has ended every claim there: the connection
// is closed, but its handler finishes the request it had read. The store
// is closed once nothing uses it, and no reaper tick sweeps a closed
// store, so it is swept once more as it closes (decision 21). The soak
// found the claim left behind (seed 15536472690001368833).
func TestArchiveSweepsAsTheStoreCloses(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projA)
	c := mustDialTo(t, m.Server, carol, projB)
	held := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "held at the archive"})
	mustAsk[proto.StartResult](c, proto.OpStart, proto.StartArgs{ID: held.ID})
	late := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "started as the project is archived"})

	// The store is pinned as a request being handled pins it.
	var released int
	m.stores.eachOf(projB, func(pr *served) {
		released = mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projB}).Released
		args, err := json.Marshal(proto.StartArgs{ID: late.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, perr := pr.dispatch(t.Context(), carol, proto.OpStart, args); perr != nil {
			t.Fatalf("the start in flight: %v", perr)
		}
	})
	if released != 1 {
		t.Errorf("project.archive released %d claims, want the 1 held then", released)
	}
	m.stores.closeWhere("test", func(*entry) bool { return false }) // waits for the store to close
	if got := m.opened(projB); got != 1 {
		t.Fatalf("the store of the archived project was opened %d times, want once: nothing reopens it", got)
	}

	// Read from the database, as the next open will find it.
	p, ok := m.ctl.ProjectNamed("proj-b")
	if !ok {
		t.Fatal("no project proj-b")
	}
	st, err := m.cfg.Open(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, id := range []string{held.ID, late.ID} {
		cl, err := realStore(st).ClaimOf(t.Context(), store.IssueID(id))
		if err != nil {
			t.Fatal(err)
		}
		if cl != nil {
			t.Errorf("%s is still claimed by %s in the archived project, with its store closed", id, cl.Holder.Principal)
		}
	}
	evs, err := realStore(st).History(t.Context(), store.IssueID(late.ID))
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	for _, e := range evs {
		ops = append(ops, string(e.Op)+" by "+e.Actor.Principal+"/"+e.Actor.Session)
	}
	want := []string{"issue.create by carol/s-c", "claim.take by carol/s-c", "issue.update by carol/s-c",
		"claim.release by ariesd/access", "issue.update by ariesd/access"}
	if !slices.Equal(ops, want) {
		t.Errorf("the history of the issue started as the project was archived:\n%v\nwant\n%v", ops, want)
	}
}
