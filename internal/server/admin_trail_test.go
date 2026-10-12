package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// What an account added outlives it (security review of stage 3a, F-2).
// Decision 19 keeps an admin from adding a key to their own account. It
// does not, and cannot, keep a server admin from adding a key to another
// server admin's account, or from creating a principal with a key and
// making it an admin everywhere: a server admin has total control. So
// disabling the first account does not end what it set up. What the
// server owes the other admins is to say so, and to let them find it.

// TestWhatADisabledAdminAddedIsFoundAndRemoved is the review's scenario,
// to its end: with dana's key alone, an agent plants a key on server
// admin dave and makes a principal, ghost, an admin of every project.
// dana is disabled; both still sign in. dave is told what dana left,
// lists it, removes it, and then neither signs in.
func TestWhatADisabledAdminAddedIsFoundAndRemoved(t *testing.T) {
	m := newMultiAdmins(t, Limits{}, nil, "dana", "dave")
	m.project(t, projA, "alice=admin")
	m.project(t, projB, "erin=admin")
	dave := store.Actor{Principal: "dave", Session: "s-dave", Machine: "m"}
	for _, name := range []string{"dana", "dave"} {
		if _, err := m.ctl.AddPrincipal(t.Context(), dave, name, ""); err != nil {
			t.Fatal(err)
		}
	}
	// An agent with dana's key, steered by what it read, runs `sfx admin`.
	d := mustDialTo(t, m.Server, dana, projA)
	if _, perr := ask[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "dana", Key: keyLine(t, "attacker-0")}); perr == nil {
		t.Fatal("dana added a key to her own account: decision 19 is not enforced")
	}
	mustAsk[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "dave", Key: keyLine(t, "attacker-1")})
	mustAsk[proto.PrincipalAddResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "ghost", Unassigned: true, Keys: []string{keyLine(t, "attacker-2")}})
	for _, p := range []string{projA, projB} {
		mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "ghost", Project: p, Role: "admin", AcceptKeys: true})
	}
	planted := []string{fingerprintOf(t, "attacker-1"), fingerprintOf(t, "attacker-2")}
	slices.Sort(planted)
	asDave := store.Actor{Principal: "dave", Session: "s-attacker", Machine: "elsewhere"}
	ghost := store.Actor{Principal: "ghost", Session: "s-ghost", Machine: "elsewhere"}
	signsIn := func(a store.Actor, seed string) bool {
		t.Helper()
		w, _, closed := dialKey(t, m.Server, a, projB, fingerprintOf(t, seed))
		return !closed && w.Err == nil
	}

	// dana's own access ends: the other server admin disables her, and is
	// told what she left behind and how to list it.
	dv := mustDialTo(t, m.Server, dave, projA)
	res := mustAsk[proto.PrincipalResult](dv, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "dana"})
	// The fixture's projects and their admins were dana's work too.
	const warning = "dana added 2 keys that are still in the store, created 3 principals and added 4 memberships, and disabling dana undoes none of them: " +
		"review them with `sfx admin key list --added-by dana`, `sfx admin principal list --created-by dana` and `sfx admin member list --added-by dana`"
	if res.State != "disabled" || res.KeysAdded != 2 || res.PrincipalsCreated != 3 || res.MembersAdded != 4 || res.Warning != warning {
		t.Errorf("principal.disable dana = %+v\nwant 2 keys added, 3 principals created, 4 memberships added and the warning\n%s", res, warning)
	}
	if w, _ := dialTo(t, m.Server, dana, projA); w.Err == nil {
		t.Fatal("dana was let in after being disabled")
	}
	// Disabling her ended nothing she set up.
	if !signsIn(asDave, "attacker-1") || !signsIn(ghost, "attacker-2") {
		t.Fatal("the planted keys no longer sign in, with nothing done about them: the scenario is not the review's")
	}

	// dave follows the three commands.
	keys := mustAsk[proto.KeysResult](dv, proto.OpKeys, proto.KeysArgs{AddedBy: "dana"})
	var fps, whose []string
	for _, k := range keys.Keys {
		fps = append(fps, k.Fingerprint)
		whose = append(whose, k.Principal)
		if k.AddedBy != "dana" || k.AddedAt.IsZero() || k.Type != "ssh-ed25519" {
			t.Errorf("a key dana added = %+v, want who added it, when, and its type", k)
		}
	}
	slices.Sort(whose)
	if keys.AddedBy != "dana" || keys.Principal != "" || !slices.Equal(fps, planted) || !slices.Equal(whose, []string{"dave", "ghost"}) {
		t.Errorf("keys added by dana = %+v; want the two planted keys, by fingerprint, each with whose it is", keys)
	}
	made := mustAsk[proto.PrincipalsResult](dv, proto.OpPrincipals, proto.PrincipalsArgs{CreatedBy: "dana"})
	var names []string
	for _, p := range made.Principals {
		names = append(names, p.Name)
	}
	if !slices.Equal(names, []string{"alice", "erin", "ghost"}) {
		t.Errorf("principals created by dana = %v, want alice, erin and ghost", names)
	}
	set := mustAsk[proto.MembersResult](dv, proto.OpMembers, proto.MembersArgs{AddedBy: "dana"})
	var rows []string
	for _, mb := range set.Members {
		rows = append(rows, mb.ProjectName+"/"+mb.Principal+"="+mb.Role)
	}
	if want := []string{"proj-a/alice=admin", "proj-a/ghost=admin", "proj-b/erin=admin", "proj-b/ghost=admin"}; set.Project != "" || !slices.Equal(rows, want) {
		t.Errorf("memberships added by dana = %v (project %q), want %v across the projects, by project and principal", rows, set.Project, want)
	}

	// And what should not be there is removed. Nothing was removed for
	// him. The key planted on his own account is his to remove, with
	// nobody's help (decision 38): he lists his keys, sees one that dana
	// added, and takes it out. He still may not add one.
	own := mustAsk[proto.KeysResult](dv, proto.OpKeys, proto.KeysArgs{Name: "dave"})
	if len(own.Keys) != 1 || own.Keys[0].Fingerprint != fingerprintOf(t, "attacker-1") || own.Keys[0].AddedBy != "dana" || own.Keys[0].AddedAt.IsZero() {
		t.Errorf("dave's own keys = %+v, want the planted one, with who added it and when", own.Keys)
	}
	if rm := mustAsk[proto.KeyResult](dv, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fingerprintOf(t, "attacker-1")}); rm.Principal != "dave" {
		t.Errorf("key.remove by dave of the key planted on his own account = %+v, want it removed", rm)
	}
	if _, perr := ask[proto.KeyResult](dv, proto.OpKeyAdd, proto.KeyAddArgs{Name: "dave", Key: keyLine(t, "daves-new")}); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("key.add dave by dave = %v, want forbidden", perr)
	}
	mustAsk[proto.PrincipalResult](dv, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "ghost"})
	if signsIn(asDave, "attacker-1") || signsIn(ghost, "attacker-2") {
		t.Error("a planted key still signs in after dave removed one and disabled the other's principal")
	}
	if left := mustAsk[proto.KeysResult](dv, proto.OpKeys, proto.KeysArgs{AddedBy: "dana"}); len(left.Keys) != 1 {
		t.Errorf("keys added by dana after one was removed = %+v, want the one of ghost, who is disabled", left.Keys)
	}
}

// TestTrailReadRules pins who may list what an account added, and how
// much of it: a server admin all of it, a project admin what is within
// the projects they administer, and nobody else anything.
func TestTrailReadRules(t *testing.T) {
	m := adminWorld(t, Limits{}) // carol, in a and b, has a key; all of it is dana's work
	ctx := t.Context()
	for name, proj := range map[string]string{"hal": projA, "ned": projB} {
		if _, err := m.ctl.AddPrincipal(ctx, dana, name, proj); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ctl.AddKey(ctx, dana, name, keyLine(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	conns := map[string]*conn{
		"dana":  mustDialTo(t, m.Server, dana, projA),
		"alice": mustDialTo(t, m.Server, alice, projA), // admin of a
		"erin":  mustDialTo(t, m.Server, erin, projB),  // admin of b
		"bob":   mustDialTo(t, m.Server, bob, projA),   // a member
	}
	keysFor := func(who string, args proto.KeysArgs) []string {
		t.Helper()
		res := mustAsk[proto.KeysResult](conns[who], proto.OpKeys, args)
		var out []string
		for _, k := range res.Keys {
			out = append(out, k.Principal)
		}
		slices.Sort(out)
		return out
	}
	principalsFor := func(who string) []string {
		t.Helper()
		res := mustAsk[proto.PrincipalsResult](conns[who], proto.OpPrincipals, proto.PrincipalsArgs{CreatedBy: "dana"})
		var out []string
		for _, p := range res.Principals {
			out = append(out, p.Name)
		}
		return out
	}
	membersFor := func(who string, args proto.MembersArgs) []string {
		t.Helper()
		res := mustAsk[proto.MembersResult](conns[who], proto.OpMembers, args)
		var out []string
		for _, mb := range res.Members {
			out = append(out, mb.ProjectName+"/"+mb.Principal)
		}
		return out
	}
	inA := []string{"proj-a/alice", "proj-a/bob", "proj-a/carol", "proj-a/hal"}
	inB := []string{"proj-b/carol", "proj-b/erin", "proj-b/ned"}

	t.Run("keys", func(t *testing.T) {
		for who, want := range map[string][]string{"dana": {"carol", "hal", "ned"}, "alice": {"carol", "hal"}, "erin": {"carol", "ned"}} {
			if got := keysFor(who, proto.KeysArgs{AddedBy: "dana"}); !slices.Equal(got, want) {
				t.Errorf("keys added by dana, for %s, are keys of %v; want %v", who, got, want)
			}
		}
		if got := keysFor("dana", proto.KeysArgs{AddedBy: "nobody"}); got != nil {
			t.Errorf("keys added by someone who added none = %v, want none", got)
		}
	})
	t.Run("principals", func(t *testing.T) {
		for who, want := range map[string][]string{
			"dana":  {"alice", "bob", "carol", "erin", "frank", "hal", "ned"}, // frank is in no project
			"alice": {"alice", "bob", "carol", "hal"},
			"erin":  {"carol", "erin", "ned"},
		} {
			if got := principalsFor(who); !slices.Equal(got, want) {
				t.Errorf("principals created by dana, for %s = %v; want %v", who, got, want)
			}
		}
	})
	t.Run("members", func(t *testing.T) {
		for _, tc := range []struct {
			who  string
			args proto.MembersArgs
			want []string
		}{
			{"dana", proto.MembersArgs{AddedBy: "dana"}, slices.Concat(inA, inB)},
			{"alice", proto.MembersArgs{AddedBy: "dana"}, inA},
			{"erin", proto.MembersArgs{AddedBy: "dana"}, inB},
			{"dana", proto.MembersArgs{AddedBy: "dana", Project: "proj-b"}, inB},
			{"alice", proto.MembersArgs{AddedBy: "dana", Project: projA}, inA},
			{"dana", proto.MembersArgs{AddedBy: "erin"}, nil},
		} {
			if got := membersFor(tc.who, tc.args); !slices.Equal(got, tc.want) {
				t.Errorf("members %+v, for %s = %v; want %v", tc.args, tc.who, got, tc.want)
			}
		}
	})
	t.Run("paged", func(t *testing.T) {
		first := mustAsk[proto.MembersResult](conns["dana"], proto.OpMembers, proto.MembersArgs{AddedBy: "dana", AdminListArgs: proto.AdminListArgs{Limit: 5}})
		if len(first.Members) != 5 || first.More != 2 {
			t.Fatalf("the first 5 memberships dana added = %d rows and %d more, want 5 and 2", len(first.Members), first.More)
		}
		last := first.Members[4]
		rest := membersFor("dana", proto.MembersArgs{AddedBy: "dana", AdminListArgs: proto.AdminListArgs{After: last.ProjectName + "/" + last.Principal}})
		if !slices.Equal(rest, inB[1:]) {
			t.Errorf("the memberships after %s/%s = %v, want %v", last.ProjectName, last.Principal, rest, inB[1:])
		}
		keys := mustAsk[proto.KeysResult](conns["dana"], proto.OpKeys, proto.KeysArgs{AddedBy: "dana", AdminListArgs: proto.AdminListArgs{Limit: 2}})
		if len(keys.Keys) != 2 || keys.More != 1 || keys.Keys[0].Fingerprint >= keys.Keys[1].Fingerprint {
			t.Fatalf("the first 2 keys dana added = %+v and %d more; want 2, by fingerprint, and 1 more", keys.Keys, keys.More)
		}
		after := mustAsk[proto.KeysResult](conns["dana"], proto.OpKeys, proto.KeysArgs{AddedBy: "dana", AdminListArgs: proto.AdminListArgs{After: keys.Keys[1].Fingerprint}})
		if len(after.Keys) != 1 || after.More != 0 || after.Keys[0].Fingerprint <= keys.Keys[1].Fingerprint {
			t.Errorf("the keys after %s = %+v and %d more; want the one left", keys.Keys[1].Fingerprint, after.Keys, after.More)
		}
	})
	t.Run("refused", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			who  string
			op   string
			args any
			code proto.Code
			msg  string
		}{
			{"a member lists the keys someone added", "bob", proto.OpKeys, proto.KeysArgs{AddedBy: "dana"}, proto.CodeForbidden, "bob may not list the keys dana added"},
			{"a member lists the principals someone created", "bob", proto.OpPrincipals, proto.PrincipalsArgs{CreatedBy: "dana"}, proto.CodeForbidden, "bob may not list the principals"},
			{"a member lists the memberships someone added", "bob", proto.OpMembers, proto.MembersArgs{AddedBy: "dana"}, proto.CodeForbidden, "bob may not list the members"},
			{"a project admin lists every principal", "alice", proto.OpPrincipals, proto.PrincipalsArgs{}, proto.CodeForbidden, "alice may not list the principals"},
			{"a project admin, in a project that is not theirs", "alice", proto.OpMembers, proto.MembersArgs{AddedBy: "dana", Project: projB}, proto.CodeForbidden, "alice may not list the members of project"},
			{"a name and who added", "dana", proto.OpKeys, proto.KeysArgs{Name: "bob", AddedBy: "dana"}, proto.CodeInvalid, "shows a principal's keys, or the keys someone added"},
			{"neither", "dana", proto.OpKeys, proto.KeysArgs{}, proto.CodeInvalid, "shows a principal's keys, or the keys someone added"},
			{"not a name", "dana", proto.OpKeys, proto.KeysArgs{AddedBy: "Not A Name"}, proto.CodeInvalid, "is not a principal's name"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, perr := ask[json.RawMessage](conns[tc.who], tc.op, tc.args)
				if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || perr.Fix == "" {
					t.Errorf("%s %+v by %s = %+v; want %s, a message containing %q and a fix", tc.op, tc.args, tc.who, perr, tc.code, tc.msg)
				}
			})
		}
	})
	t.Run("disable says what the account left", func(t *testing.T) {
		// alice, admin of a, made gina and gave her a key; erin's project
		// is none of alice's business, and nothing of it is counted.
		mustAsk[proto.PrincipalAddResult](conns["alice"], proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "gina", Keys: []string{keyLine(t, "gina")}})
		mustAsk[proto.KeyResult](conns["alice"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "hal", Key: keyLine(t, "hal-2")})
		res := mustAsk[proto.PrincipalResult](conns["alice"], proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "hal"})
		if res.KeysAdded != 0 || res.PrincipalsCreated != 0 || res.MembersAdded != 0 || res.Warning != "" {
			t.Errorf("principal.disable hal, who added nothing = %+v, want no counts and no warning", res)
		}
		// dana disables alice: 2 keys, 1 principal, 1 membership.
		res = mustAsk[proto.PrincipalResult](conns["dana"], proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "alice"})
		if res.KeysAdded != 2 || res.PrincipalsCreated != 1 || res.MembersAdded != 1 || !strings.Contains(res.Warning, "alice added 2 keys that are still in the store, created 1 principal and added 1 membership") {
			t.Errorf("principal.disable alice = %+v, want 2 keys, 1 principal, 1 membership and the warning", res)
		}
	})
}
