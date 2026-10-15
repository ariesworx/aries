package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// Decision 36: joining a project shows a person's keys and needs
// acceptance (security review of stage 3a, F-3).

// carried returns the fingerprints of the keys a member.set result
// carries, sorted.
func carried(res proto.MemberSetResult) []string {
	var out []string
	for _, k := range res.Carried {
		out = append(out, k.Fingerprint)
	}
	slices.Sort(out)
	return out
}

// TestPlantedKeyIsShownBeforeItReachesAnotherProject is the review's
// scenario. alice administers a only, and adds a key of her own to
// victor, a member of a only, which the rules allow. erin, who
// administers b only, then adds victor to b. She was once shown nothing
// and refused `key list victor`, and alice's key read and wrote b as
// victor. Now victor is not added until erin has been shown every key,
// with who added it, and accepts.
func TestPlantedKeyIsShownBeforeItReachesAnotherProject(t *testing.T) {
	t.Parallel()
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice=admin", "victor")
	m.project(t, projB, "erin=admin")
	a := mustDialTo(t, m.Server, alice, projA)
	planted := mustAsk[proto.KeyResult](a, proto.OpKeyAdd, proto.KeyAddArgs{Name: "victor", Key: keyLine(t, "alice-second")})
	asVictor := store.Actor{Principal: "victor", Session: "s-alice", Machine: "alices-laptop"}

	e := mustDialTo(t, m.Server, erin, projB)
	seq := func() int {
		t.Helper()
		evs, err := m.ctl.Events(t.Context(), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	before := seq()
	set := mustAsk[proto.MemberSetResult](e, proto.OpMemberSet, proto.MemberSetArgs{Name: "victor"})
	if set.Change != proto.MemberPending || set.Rev != 0 || set.ID != "victor" || set.Project != projB || set.ProjectName != "proj-b" || set.Role != "member" || set.Projects != 1 {
		t.Errorf("member.set victor by erin, not accepting his keys = %+v; want pending, rev 0, the project and role asked for, and the 1 project he is in", set)
	}
	if len(set.Carried) != 1 {
		t.Fatalf("member.set victor carries %+v, want his one key", set.Carried)
	}
	k := set.Carried[0]
	if k.Fingerprint != planted.ID || k.Type != "ssh-ed25519" || k.Comment != "alice-second@example.com" || k.AddedBy != "alice" || k.AddedAt.IsZero() || k.AdderAdministers {
		t.Errorf("the key shown to erin = %+v; want fingerprint %s, its type and comment, added by alice, when, and that alice does not administer b", k, planted.ID)
	}
	if after := seq(); after != before {
		t.Errorf("the unaccepted member.set recorded %d events, want none", after-before)
	}
	// Nothing was added, so the planted key reaches nothing yet.
	if w, _, closed := dialKey(t, m.Server, asVictor, projB, planted.ID); closed || w.Err == nil || w.Err.Code != proto.CodeForbidden {
		t.Fatalf("the planted key in b before erin accepted = %+v (closed %v), want forbidden", w, closed)
	}

	// erin accepts, having seen it. The key is then victor's in b too,
	// which is what she agreed to; she can list his keys from now on, and
	// still not change them.
	set = mustAsk[proto.MemberSetResult](e, proto.OpMemberSet, proto.MemberSetArgs{Name: "victor", AcceptKeys: true})
	if set.Change != "added" || set.Rev != 1 || set.Projects != 2 || !slices.Equal(carried(set), []string{planted.ID}) {
		t.Errorf("member.set victor by erin, accepting = %+v; want added, 2 projects and the key that came with him", set)
	}
	list, perr := ask[proto.KeysResult](e, proto.OpKeys, proto.KeysArgs{Name: "victor"})
	if perr != nil || len(list.Keys) != 1 || list.Keys[0].Fingerprint != planted.ID || list.Keys[0].AddedBy != "alice" {
		t.Errorf("keys of victor for erin, an admin of a project he belongs to = %+v, %v; want the key and who added it", list, perr)
	}
	for _, tc := range []struct {
		op   string
		args any
	}{
		{proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: planted.ID}},
		{proto.OpKeyAdd, proto.KeyAddArgs{Name: "victor", Key: keyLine(t, "erins-own")}},
	} {
		if _, perr := ask[json.RawMessage](e, tc.op, tc.args); perr == nil || perr.Code != proto.CodeForbidden {
			t.Errorf("%s on victor by erin, who has no reach over him = %v, want forbidden", tc.op, perr)
		}
	}
	if w, _ := dialTo(t, m.Server, alice, projB); w.Err == nil {
		t.Error("alice herself was let into b")
	}
}

// TestMemberSetAcceptance pins when member.set asks for acceptance and
// what it shows.
func TestMemberSetAcceptance(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{}) // carol, in a and b, has a key added by dana; frank is in no project
	ctx := t.Context()
	if _, err := m.ctl.AddPrincipal(ctx, dana, "hal", projB); err != nil { // in b only, with two keys
		t.Fatal(err)
	}
	if _, err := m.ctl.AddKey(ctx, dana, "hal", keyLine(t, "hal")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.AddKey(ctx, erin, "hal", keyLine(t, "hal-from-erin")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.AddKey(ctx, dana, "frank", keyLine(t, "frank")); err != nil {
		t.Fatal(err)
	}
	conns := map[string]*conn{
		"alice": mustDialTo(t, m.Server, alice, projA),
		"erin":  mustDialTo(t, m.Server, erin, projB),
		"dana":  mustDialTo(t, m.Server, dana, projA),
		"bob":   mustDialTo(t, m.Server, bob, projA),
	}
	hals := []string{fingerprintOf(t, "hal"), fingerprintOf(t, "hal-from-erin")}
	slices.Sort(hals)

	t.Run("who added each key, and whether they administer the project", func(t *testing.T) {
		// hal, of b, is to join a. dana is a server admin, who administers
		// every project; erin administers b and not a.
		res := mustAsk[proto.MemberSetResult](conns["alice"], proto.OpMemberSet, proto.MemberSetArgs{Name: "hal"})
		administers := map[string]bool{}
		for _, k := range res.Carried {
			administers[k.AddedBy] = k.AdderAdministers
		}
		if len(res.Carried) != 2 || !administers["dana"] || administers["erin"] {
			t.Errorf("hal's keys, shown to alice = %+v; want the one dana added marked as an admin's of a, and the one erin added not", res.Carried)
		}
	})

	tests := []struct {
		name    string
		who     string
		args    proto.MemberSetArgs
		change  string
		carried []string
	}{
		{"a principal with no key is added at once", "erin", proto.MemberSetArgs{Name: "bob"}, "added", nil},
		{"one with keys is pending, for a project admin", "alice", proto.MemberSetArgs{Name: "hal"}, proto.MemberPending, hals},
		{"and for a server admin alike", "dana", proto.MemberSetArgs{Name: "hal", Project: projA}, proto.MemberPending, hals},
		{"a server admin placing an unassigned principal with a key", "dana", proto.MemberSetArgs{Name: "frank", Project: projB}, proto.MemberPending,
			[]string{fingerprintOf(t, "frank")}},
		{"accepted in the first request", "alice", proto.MemberSetArgs{Name: "hal", AcceptKeys: true}, "added", hals},
		{"a role change of a member with keys needs none", "alice", proto.MemberSetArgs{Name: "carol", Role: "admin"}, "changed", nil},
		{"nor does setting the role it has", "alice", proto.MemberSetArgs{Name: "carol", Role: "admin"}, "unchanged", nil},
		{"accepting where there is nothing to accept", "erin", proto.MemberSetArgs{Name: "alice", AcceptKeys: true}, "added", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, perr := ask[proto.MemberSetResult](conns[tc.who], proto.OpMemberSet, tc.args)
			if perr != nil {
				t.Fatalf("member.set %+v by %s: %v", tc.args, tc.who, perr)
			}
			if res.Change != tc.change || !slices.Equal(carried(res), tc.carried) {
				t.Errorf("member.set %+v by %s = %+v\nwant change %q carrying %v", tc.args, tc.who, res, tc.change, tc.carried)
			}
			project := projA
			if tc.who == "erin" || tc.args.Project == projB {
				project = projB
			}
			_, member, err := m.ctl.Role(ctx, tc.args.Name, project)
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.change != proto.MemberPending; member != want {
				t.Errorf("after member.set %+v (%s), %s a member = %t, want %t", tc.args, res.Change, tc.args.Name, member, want)
			}
		})
	}

	t.Run("no key is shown to someone who may not add the principal", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			who  string
			args proto.MemberSetArgs
		}{
			{"a plain member", "bob", proto.MemberSetArgs{Name: "frank"}},
			{"an admin of another project", "alice", proto.MemberSetArgs{Name: "frank", Project: projB}},
			{"a project admin placing an unassigned principal", "alice", proto.MemberSetArgs{Name: "frank"}},
			{"the same, accepting", "alice", proto.MemberSetArgs{Name: "frank", AcceptKeys: true}},
		} {
			raw, perr := ask[json.RawMessage](conns[tc.who], proto.OpMemberSet, tc.args)
			if perr == nil || perr.Code != proto.CodeForbidden || strings.Contains(perr.Message+perr.Fix+string(raw), "SHA256:") {
				t.Errorf("%s: member.set %+v = %s, %+v; want forbidden and no fingerprint", tc.name, tc.args, raw, perr)
			}
		}
	})

	t.Run("a key added after the keys were shown is in the answer to the acceptance", func(t *testing.T) {
		if _, err := m.ctl.AddPrincipal(ctx, dana, "ned", projA); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ctl.AddKey(ctx, dana, "ned", keyLine(t, "ned")); err != nil {
			t.Fatal(err)
		}
		shown := mustAsk[proto.MemberSetResult](conns["erin"], proto.OpMemberSet, proto.MemberSetArgs{Name: "ned"})
		if _, err := m.ctl.AddKey(ctx, alice, "ned", keyLine(t, "ned-late")); err != nil { // ned is in a only: within alice's reach
			t.Fatal(err)
		}
		added := mustAsk[proto.MemberSetResult](conns["erin"], proto.OpMemberSet, proto.MemberSetArgs{Name: "ned", AcceptKeys: true})
		want := []string{fingerprintOf(t, "ned"), fingerprintOf(t, "ned-late")}
		slices.Sort(want)
		if len(shown.Carried) != 1 || added.Change != "added" || !slices.Equal(carried(added), want) {
			t.Errorf("shown %v, then added carrying %v; want the late key in the second answer: %v", carried(shown), carried(added), want)
		}
	})
}

// TestKeysReadableByAProjectsAdmin pins who may list a principal's keys:
// a server admin, the principal, and an admin of an active project the
// principal belongs to, with or without reach over it (decision 36).
// Anyone else is refused the same way whether or not the principal
// exists.
func TestKeysReadableByAProjectsAdmin(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{}) // carol is in a and b; frank in no project; bob in a
	conns := map[string]*conn{
		"alice": mustDialTo(t, m.Server, alice, projA),
		"erin":  mustDialTo(t, m.Server, erin, projB),
		"bob":   mustDialTo(t, m.Server, bob, projA),
		"carol": mustDialTo(t, m.Server, carol, projA),
		"dana":  mustDialTo(t, m.Server, dana, projA),
	}
	const refusedMsg = " may not list the keys of NAME: it is for the principal, for an admin of a project it belongs to, and for server admins"
	tests := []struct {
		name      string
		who, of   string
		readable  bool
		wantCarol bool
	}{
		{"an admin of one of the principal's projects, without reach", "alice", "carol", true, true},
		{"an admin of the other", "erin", "carol", true, true},
		{"the principal itself", "carol", "carol", true, true},
		{"a server admin", "dana", "carol", true, true},
		{"an admin with reach", "alice", "bob", true, false},
		{"an admin of a project the principal is not in", "erin", "bob", false, false},
		{"a principal in no project", "alice", "frank", false, false},
		{"a name no one has", "alice", "zed", false, false},
		{"a plain member of the same project", "bob", "carol", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, perr := ask[proto.KeysResult](conns[tc.who], proto.OpKeys, proto.KeysArgs{Name: tc.of})
			if !tc.readable {
				want := tc.who + strings.ReplaceAll(refusedMsg, "NAME", tc.of)
				if perr == nil || perr.Code != proto.CodeForbidden || perr.Message != want || perr.Fix == "" {
					t.Errorf("keys of %s for %s = %+v, %+v\nwant forbidden: %q", tc.of, tc.who, res, perr, want)
				}
				return
			}
			if perr != nil || res.Principal != tc.of {
				t.Fatalf("keys of %s for %s = %+v, %v; want them listed", tc.of, tc.who, res, perr)
			}
			if has := len(res.Keys) == 1 && res.Keys[0].Fingerprint == fingerprintOf(t, "carol"); has != tc.wantCarol {
				t.Errorf("keys of %s for %s = %+v; carol's key listed = %t, want %t", tc.of, tc.who, res.Keys, has, tc.wantCarol)
			}
		})
	}
	// Once the project is archived it is served to no one, and its admin
	// has no view by it. (Archived behind the server, so erin's connection
	// is still there to ask on.)
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projB); err != nil {
		t.Fatal(err)
	}
	if _, perr := ask[proto.KeysResult](conns["erin"], proto.OpKeys, proto.KeysArgs{Name: "carol"}); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("keys of carol for erin with b archived = %v, want forbidden", perr)
	}
}
