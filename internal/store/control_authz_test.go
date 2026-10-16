package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// accessWorld builds the server the authorization tests act on:
//
//	dana, sam   server admins; dana has a principal row and is a member of a
//	alice       admin of a
//	erin        admin of b
//	ivy         admin of a and of b
//	bob, hal    members of a
//	carol       member of a and of b
//	frank       member of nothing
//
// with one key each for bob, hal, carol, frank and dana, seeded by name.
func accessWorld(t *testing.T) *Control {
	t.Helper()
	c := newControl(t)
	mustProject(t, c, projA)
	mustProject(t, c, projB)
	mustPrincipal(t, c, "dana", projA+"=member")
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "erin", projB+"=admin")
	mustPrincipal(t, c, "ivy", projA+"=admin", projB+"=admin")
	mustPrincipal(t, c, "bob", projA+"=member")
	mustPrincipal(t, c, "hal", projA+"=member")
	mustPrincipal(t, c, "carol", projA+"=member", projB+"=member")
	mustPrincipal(t, c, "frank")
	for _, p := range []string{"bob", "hal", "carol", "frank", "dana"} {
		mustKey(t, c, p, p)
	}
	return c
}

// TestControlAccess runs every guarded write as each kind of actor. A
// write is allowed, or refused with an *AccessError giving the reason;
// a refusal records nothing and leaves the key index alone.
func TestControlAccess(t *testing.T) {
	t.Parallel()
	c := accessWorld(t)
	ctx := t.Context()
	fp := func(seed string) string { _, f := testKey(t, seed); return f }
	line := func(seed string) string { l, _ := testKey(t, seed); return l }

	// The writes, each on the target its name gives. Those that succeed
	// change only what no other case reads.
	type write func(a Actor) error
	projectAdd := func(a Actor) error { _, err := c.AddProject(ctx, a, newProject(projC)); return err }
	projectActivate := func(a Actor) error { _, err := c.ActivateProject(ctx, a, projC); return err }
	projectArchive := func(a Actor) error { _, err := c.ArchiveProject(ctx, a, projC); return err }
	principalAdd := func(name, project string) write {
		return func(a Actor) error { _, err := c.AddPrincipal(ctx, a, name, project); return err }
	}
	keyAdd := func(principal string) write {
		return func(a Actor) error {
			_, err := c.AddKey(ctx, a, principal, line(principal+" by "+a.Principal))
			return err
		}
	}
	keyRemove := func(seed string) write {
		return func(a Actor) error { _, err := c.RemoveKey(ctx, a, fp(seed)); return err }
	}
	disable := func(principal string) write {
		return func(a Actor) error { _, err := c.DisablePrincipal(ctx, a, principal); return err }
	}
	enable := func(principal string) write {
		return func(a Actor) error { _, err := c.EnablePrincipal(ctx, a, principal); return err }
	}
	memberSet := func(principal, project string, r Role) write {
		return func(a Actor) error { _, err := c.SetMember(ctx, a, principal, project, r); return err }
	}
	memberRemove := func(principal, project string) write {
		return func(a Actor) error { _, err := c.RemoveMember(ctx, a, principal, project); return err }
	}

	zed := Actor{Principal: "zed", Session: "sess-z", Machine: "laptop-z"}
	cases := []struct {
		name  string
		actor Actor
		run   write
		// want is the refusal's reason; empty when the write is allowed.
		want AccessReason
		// op, principal and project are what the refusal names.
		op                 Op
		principal, project string
	}{
		// A project admin never adds, activates or archives a project.
		{name: "alice adds a project", actor: alice, run: projectAdd, want: AccessServerAdmin, op: OpProjectAdd, project: projC},
		{name: "a member adds a project", actor: bob, run: projectAdd, want: AccessServerAdmin, op: OpProjectAdd, project: projC},
		{name: "a stranger adds a project", actor: zed, run: projectAdd, want: AccessServerAdmin, op: OpProjectAdd, project: projC},
		{name: "a server admin adds a project", actor: dana, run: projectAdd},
		{name: "alice activates a project", actor: alice, run: projectActivate, want: AccessServerAdmin, op: OpProjectActivate, project: projC},
		{name: "a server admin without a row activates a project", actor: sam, run: projectActivate},
		{name: "ivy archives a project she administers", actor: ivy, run: func(a Actor) error { _, err := c.ArchiveProject(ctx, a, projA); return err },
			want: AccessServerAdmin, op: OpProjectArchive, project: projA},
		{name: "alice archives a project", actor: alice, run: projectArchive, want: AccessServerAdmin, op: OpProjectArchive, project: projC},
		{name: "a server admin archives a project", actor: dana, run: projectArchive},

		// Members: a project admin, in that project only.
		{name: "alice makes bob an admin of a", actor: alice, run: memberSet("bob", projA, RoleAdmin)},
		{name: "alice makes bob a member of a again", actor: alice, run: memberSet("bob", projA, RoleMember)},
		{name: "alice adds erin to a", actor: alice, run: memberSet("erin", projA, RoleMember)},
		{name: "alice removes erin from a", actor: alice, run: memberRemove("erin", projA)},
		// An unassigned principal the actor did not create is placed by a
		// server admin only; one already in a project may be added.
		{name: "alice adds frank, in no project and created by a server admin, to a", actor: alice, run: memberSet("frank", projA, RoleMember),
			want: AccessUnassigned, op: OpMemberSet, principal: "frank", project: projA},
		{name: "ivy adds frank to b", actor: ivy, run: memberSet("frank", projB, RoleAdmin),
			want: AccessUnassigned, op: OpMemberSet, principal: "frank", project: projB},
		{name: "alice adds bob to b", actor: alice, run: memberSet("bob", projB, RoleMember), want: AccessAdmin, op: OpMemberSet, principal: "bob", project: projB},
		{name: "alice removes carol from b", actor: alice, run: memberRemove("carol", projB), want: AccessAdmin, op: OpMemberRemove, principal: "carol", project: projB},
		{name: "alice adds bob to a project that does not exist", actor: alice, run: memberSet("bob", projC[:35]+"d", RoleMember),
			want: AccessAdmin, op: OpMemberSet, principal: "bob", project: projC[:35] + "d"},
		{name: "a member adds hal as admin of a", actor: bob, run: memberSet("hal", projA, RoleAdmin), want: AccessAdmin, op: OpMemberSet, principal: "hal", project: projA},
		{name: "a member makes itself admin of a", actor: bob, run: memberSet("bob", projA, RoleAdmin), want: AccessAdmin, op: OpMemberSet, principal: "bob", project: projA},
		{name: "a member removes hal from a", actor: bob, run: memberRemove("hal", projA), want: AccessAdmin, op: OpMemberRemove, principal: "hal", project: projA},
		{name: "a stranger adds itself to a", actor: zed, run: memberSet("zed", projA, RoleAdmin), want: AccessAdmin, op: OpMemberSet, principal: "zed", project: projA},
		{name: "a server admin adds erin to a", actor: sam, run: memberSet("erin", projA, RoleMember)},
		{name: "a server admin removes erin from a", actor: sam, run: memberRemove("erin", projA)},

		// A new principal: a project admin names a project they administer.
		{name: "alice adds a principal to a", actor: alice, run: principalAdd("new-a", projA)},
		{name: "alice adds a principal to b", actor: alice, run: principalAdd("new-b", projB), want: AccessAdmin, op: OpPrincipalAdd, principal: "new-b", project: projB},
		{name: "alice adds a principal to no project", actor: alice, run: principalAdd("new-none", ""), want: AccessServerAdmin, op: OpPrincipalAdd, principal: "new-none"},
		{name: "alice adds a server admin's name", actor: alice, run: principalAdd("sam", projA), want: AccessServerAdminPrincipal, op: OpPrincipalAdd, principal: "sam", project: projA},
		{name: "a member adds a principal to a", actor: bob, run: principalAdd("new-m", projA), want: AccessAdmin, op: OpPrincipalAdd, principal: "new-m", project: projA},
		{name: "a stranger adds a principal", actor: zed, run: principalAdd("zed", ""), want: AccessServerAdmin, op: OpPrincipalAdd, principal: "zed"},
		{name: "a server admin adds a principal to no project", actor: dana, run: principalAdd("new-d", "")},
		{name: "a server admin adds a server admin's name", actor: dana, run: principalAdd("sam", projA)},

		// Keys and state: only while every project of the principal is
		// one the actor administers.
		{name: "alice adds a key to bob, in a only", actor: alice, run: keyAdd("bob")},
		{name: "alice removes a key of hal, in a only", actor: alice, run: keyRemove("hal")},
		{name: "alice disables hal", actor: alice, run: disable("hal")},
		{name: "alice enables hal", actor: alice, run: enable("hal")},
		{name: "alice adds a key to carol, also in b", actor: alice, run: keyAdd("carol"), want: AccessReach, op: OpKeyAdd, principal: "carol"},
		// A refused removal names no principal: a fingerprint does not say
		// whose key it is, and neither does the refusal.
		{name: "alice removes a key of carol", actor: alice, run: keyRemove("carol"), want: AccessReach, op: OpKeyRemove},
		{name: "alice disables carol", actor: alice, run: disable("carol"), want: AccessReach, op: OpPrincipalDisable, principal: "carol"},
		{name: "alice enables carol", actor: alice, run: enable("carol"), want: AccessReach, op: OpPrincipalEnable, principal: "carol"},
		{name: "alice adds a key to frank, in no project", actor: alice, run: keyAdd("frank"), want: AccessReach, op: OpKeyAdd, principal: "frank"},
		{name: "alice removes a key of frank", actor: alice, run: keyRemove("frank"), want: AccessReach, op: OpKeyRemove},
		{name: "alice disables frank", actor: alice, run: disable("frank"), want: AccessReach, op: OpPrincipalDisable, principal: "frank"},
		{name: "alice adds a key to erin, in b only", actor: alice, run: keyAdd("erin"), want: AccessReach, op: OpKeyAdd, principal: "erin"},
		{name: "alice adds a key to a server admin in a only", actor: alice, run: keyAdd("dana"), want: AccessServerAdminPrincipal, op: OpKeyAdd, principal: "dana"},
		{name: "alice removes a server admin's key", actor: alice, run: keyRemove("dana"), want: AccessReach, op: OpKeyRemove},
		{name: "alice disables a server admin", actor: alice, run: disable("dana"), want: AccessServerAdminPrincipal, op: OpPrincipalDisable, principal: "dana"},
		{name: "ivy, admin of both, adds a key to carol", actor: ivy, run: keyAdd("carol")},
		{name: "ivy disables carol", actor: ivy, run: disable("carol")},
		{name: "ivy enables carol", actor: ivy, run: enable("carol")},
		{name: "ivy adds a key to frank, in no project", actor: ivy, run: keyAdd("frank"), want: AccessReach, op: OpKeyAdd, principal: "frank"},
		{name: "erin adds a key to bob, in a only", actor: erin, run: keyAdd("bob"), want: AccessReach, op: OpKeyAdd, principal: "bob"},
		{name: "a member adds a key to hal", actor: bob, run: keyAdd("hal"), want: AccessAdmin, op: OpKeyAdd, principal: "hal"},
		// Nobody adds a key to their own account (decision 19).
		{name: "a member adds a key to itself", actor: bob, run: keyAdd("bob"), want: AccessOwnKeys, op: OpKeyAdd, principal: "bob"},
		// Its own it may remove (decision 38; TestControlOwnKeyRemoval).
		{name: "a member removes another member's key", actor: bob, run: keyRemove("hal"), want: AccessAdmin, op: OpKeyRemove},
		{name: "a member disables hal", actor: bob, run: disable("hal"), want: AccessAdmin, op: OpPrincipalDisable, principal: "hal"},
		{name: "a member of nothing adds a key to itself", actor: frank, run: keyAdd("frank"), want: AccessOwnKeys, op: OpKeyAdd, principal: "frank"},
		{name: "a stranger adds a key to bob", actor: zed, run: keyAdd("bob"), want: AccessAdmin, op: OpKeyAdd, principal: "bob"},
		{name: "a stranger enables bob", actor: zed, run: enable("bob"), want: AccessAdmin, op: OpPrincipalEnable, principal: "bob"},
		{name: "a server admin adds a key to carol", actor: dana, run: keyAdd("carol")},
		{name: "a server admin adds a key to frank", actor: sam, run: keyAdd("frank")},
		{name: "a server admin removes a key of frank", actor: sam, run: keyRemove("frank")},
		{name: "a server admin disables frank", actor: dana, run: disable("frank")},
		{name: "a server admin enables frank", actor: dana, run: enable("frank")},
		{name: "a server admin adds a key to another", actor: sam, run: keyAdd("dana")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			err := tc.run(tc.actor)
			if tc.want == "" {
				if err != nil {
					t.Errorf("as %s = %v, want it allowed", tc.actor.Principal, err)
				}
				if controlSeq(t, c) == seq {
					t.Errorf("as %s it recorded no event; the case does not show a write", tc.actor.Principal)
				}
				return
			}
			var ae *AccessError
			if !errors.As(err, &ae) || !errors.Is(err, ErrForbidden) {
				t.Fatalf("as %s = %v, want an *AccessError wrapping ErrForbidden", tc.actor.Principal, err)
			}
			want := AccessError{Actor: tc.actor.Principal, Op: tc.op, Principal: tc.principal, Project: tc.project, Reason: tc.want}
			if *ae != want {
				t.Errorf("as %s = %+v, want %+v", tc.actor.Principal, *ae, want)
			}
			if msg := ae.Error(); !strings.Contains(msg, tc.actor.Principal) || strings.Contains(msg, projB) && tc.project != projB {
				t.Errorf("refusal %q: want it to name the actor, and no project the request did not", msg)
			}
			wantEvents(t, c, seq)
		})
	}

	// None of the refusals above changed who a key lets in.
	for _, p := range []string{"bob", "carol", "frank", "dana"} {
		for _, by := range []string{"zed", "erin", "frank"} {
			if got, _, ok := c.AuthorizedKey(fp(p + " by " + by)); ok {
				t.Errorf("AuthorizedKey of a key %s was refused adding to %s = %q, true", by, p, got)
			}
		}
	}
	if got, _, ok := c.AuthorizedKey(fp("carol")); !ok || got != "carol" {
		t.Errorf("AuthorizedKey(carol's key) = %q, %v; want carol: alice's removal was refused", got, ok)
	}
}

// TestControlReachFollowsMemberships: a project admin's reach over a
// principal is read at each write, so it ends when the principal gains a
// project the admin does not administer, and when the admin stops being
// one.
func TestControlReachFollowsMemberships(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	ctx := t.Context()
	mustProject(t, c, projA)
	mustProject(t, c, projB)
	mustPrincipal(t, c, "alice", projA+"=admin")
	n := 0
	addKey := func(a Actor, principal string) error {
		n++
		l, _ := testKey(t, fmt.Sprint("key ", n))
		_, err := c.AddKey(ctx, a, principal, l)
		return err
	}
	reason := func(err error) AccessReason {
		var ae *AccessError
		if errors.As(err, &ae) {
			return ae.Reason
		}
		return ""
	}

	// A principal alice creates is a member of a only, and hers to manage.
	p, err := c.AddPrincipal(ctx, alice, "gina", projA)
	if err != nil || p.CreatedBy != "alice" {
		t.Fatalf("AddPrincipal(gina) by alice = %+v, %v", p, err)
	}
	if r, ok, err := c.Role(ctx, "gina", projA); err != nil || !ok || r != RoleMember {
		t.Errorf("Role(gina, a) = %q, %v, %v; want member, never more", r, ok, err)
	}
	if err := addKey(alice, "gina"); err != nil {
		t.Errorf("alice adds a key to the principal she created: %v", err)
	}

	// A server admin adds gina to b: she is now out of alice's reach.
	if _, err := c.SetMember(ctx, dana, "gina", projB, RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := addKey(alice, "gina"); reason(err) != AccessReach {
		t.Errorf("alice adds a key once gina is also in b = %v, want refused: out of reach", err)
	}
	if _, err := c.DisablePrincipal(ctx, alice, "gina"); reason(err) != AccessReach {
		t.Errorf("alice disables gina once she is also in b = %v, want refused: out of reach", err)
	}
	keys, err := c.Keys(ctx, "gina")
	if err != nil || len(keys) != 1 {
		t.Fatalf("Keys(gina) = %+v, %v; want the one key", keys, err)
	}
	if _, err := c.RemoveKey(ctx, alice, keys[0].Fingerprint); reason(err) != AccessReach {
		t.Errorf("alice removes the key she added once gina is also in b = %v, want refused: out of reach", err)
	}

	// Removed from b again, gina is back within it.
	if _, err := c.RemoveMember(ctx, dana, "gina", projB); err != nil {
		t.Fatal(err)
	}
	if err := addKey(alice, "gina"); err != nil {
		t.Errorf("alice adds a key once gina is in a only again: %v", err)
	}
	// Removed from a too, she belongs to nothing, and to no project admin.
	if _, err := c.RemoveMember(ctx, alice, "gina", projA); err != nil {
		t.Fatal(err)
	}
	if err := addKey(alice, "gina"); reason(err) != AccessReach {
		t.Errorf("alice adds a key once gina is in no project = %v, want refused: out of reach", err)
	}
	if _, err := c.SetMember(ctx, alice, "gina", projA, RoleMember); err != nil {
		t.Fatal(err)
	}

	// An unassigned principal alice did not create is not hers to place:
	// she could otherwise bring it within reach and manage its keys.
	mustPrincipal(t, c, "frank")
	mustPrincipal(t, c, "erin", projB+"=admin")
	if _, err := c.AddPrincipal(ctx, erin, "hank", projB); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RemoveMember(ctx, erin, "hank", projB); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"frank", "hank"} {
		seq := controlSeq(t, c)
		if _, err := c.SetMember(ctx, alice, name, projA, RoleMember); reason(err) != AccessUnassigned {
			t.Errorf("alice adds %s, in no project and created by another, to a = %v, want refused: unassigned", name, err)
		}
		wantEvents(t, c, seq)
	}
	// Its creator may place hank again, and a server admin anyone.
	if _, err := c.SetMember(ctx, erin, "hank", projB, RoleMember); err != nil {
		t.Errorf("erin adds the unassigned principal she created to b: %v", err)
	}
	if _, err := c.SetMember(ctx, dana, "frank", projB, RoleMember); err != nil {
		t.Errorf("a server admin adds an unassigned principal to b: %v", err)
	}
	// Once in a project, a principal may be added to alice's too, and
	// its other project keeps it out of her reach.
	for _, name := range []string{"frank", "hank"} {
		if _, err := c.SetMember(ctx, alice, name, projA, RoleMember); err != nil {
			t.Errorf("alice adds %s, in b only, to a: %v", name, err)
		}
		if err := addKey(alice, name); reason(err) != AccessReach {
			t.Errorf("alice adds a key to %s, in a and b = %v, want refused: out of reach", name, err)
		}
	}
	// A role change of a member is not an addition.
	if _, err := c.RemoveMember(ctx, dana, "frank", projB); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetMember(ctx, alice, "frank", projA, RoleAdmin); err != nil {
		t.Errorf("alice changes the role of frank, in a only: %v", err)
	}

	// Demoted, alice administers nothing.
	if _, err := c.SetMember(ctx, dana, "alice", projA, RoleMember); err != nil {
		t.Fatal(err)
	}
	if err := addKey(alice, "gina"); reason(err) != AccessAdmin {
		t.Errorf("alice adds a key once demoted = %v, want refused: not an admin", err)
	}
	if _, err := c.SetMember(ctx, alice, "alice", projA, RoleAdmin); reason(err) != AccessAdmin {
		t.Errorf("alice promotes herself once demoted = %v, want refused: not an admin", err)
	}

	// Disabled, an admin's role gives nothing either.
	if _, err := c.SetMember(ctx, dana, "alice", projA, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DisablePrincipal(ctx, dana, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := addKey(alice, "gina"); reason(err) != AccessAdmin {
		t.Errorf("alice adds a key once disabled = %v, want refused: not an admin", err)
	}
	if _, err := c.SetMember(ctx, alice, "gina", projA, RoleAdmin); reason(err) != AccessAdmin {
		t.Errorf("alice sets a role once disabled = %v, want refused: not an admin", err)
	}
	if _, err := c.AddPrincipal(ctx, alice, "new", projA); reason(err) != AccessAdmin {
		t.Errorf("alice adds a principal once disabled = %v, want refused: not an admin", err)
	}
}

func TestAccessErrorMessages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  AccessError
		want string
	}{
		{AccessError{Actor: "alice", Op: OpProjectAdd, Project: projC, Reason: AccessServerAdmin},
			"alice may not project.add " + projC + ": it is for server admins"},
		{AccessError{Actor: "bob", Op: OpKeyRemove, Reason: AccessAdmin},
			"bob may not key.remove: it is for project admins and server admins"},
		{AccessError{Actor: "alice", Op: OpPrincipalAdd, Principal: "new", Reason: AccessServerAdmin},
			"alice may not principal.add new: it is for server admins"},
		{AccessError{Actor: "bob", Op: OpMemberSet, Principal: "hal", Project: projA, Reason: AccessAdmin},
			"bob may not member.set hal in project " + projA + ": it is for the project's admins and server admins"},
		{AccessError{Actor: "bob", Op: OpKeyAdd, Principal: "hal", Reason: AccessAdmin},
			"bob may not key.add hal: it is for project admins and server admins"},
		{AccessError{Actor: "alice", Op: OpKeyAdd, Principal: "carol", Reason: AccessReach},
			"alice may not key.add carol: carol does not exist, or belongs to a project alice does not administer, or to none, so it is for server admins"},
		{AccessError{Actor: "alice", Op: OpMemberSet, Principal: "frank", Project: projA, Reason: AccessUnassigned},
			"alice may not member.set frank in project " + projA + ": frank belongs to no project and alice did not create it, so it is for server admins"},
		{AccessError{Actor: "alice", Op: OpPrincipalDisable, Principal: "dana", Reason: AccessServerAdminPrincipal},
			"alice may not principal.disable dana: dana is a server admin, so it is for server admins"},
		{AccessError{Actor: "alice", Op: OpKeyRemove, Reason: AccessReach},
			"alice may not key.remove: it is no key of a principal alice may manage, so it is for server admins"},
		{AccessError{Actor: "dana", Op: OpKeyAdd, Principal: "dana", Reason: AccessOwnKeys},
			"dana may not key.add dana: nobody adds a key to their own account, so it is for another admin"},
	} {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("%+v.Error() = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestControlLimits fills each cap and checks that the next row is
// refused with a *ControlLimitError, that nothing was written, and that
// changing a row already there is still allowed.
func TestControlLimits(t *testing.T) {
	t.Parallel()
	lim := ControlLimits{Projects: 2, Principals: 4, PrincipalsPerCreator: 9, Keys: 2, Members: 2, Identities: 8, Invites: 9, Grants: 9}
	c := openControl(t, newDSN(t), ControlOptions{Limits: lim})
	ctx := t.Context()
	if got := c.Limits(); got != lim {
		t.Fatalf("Limits() = %+v, want %+v", got, lim)
	}
	mustProject(t, c, projA)
	mustProject(t, c, projB)
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "bob", projA+"=member")
	mustPrincipal(t, c, "carol")
	mustKey(t, c, "alice", "alice-1")
	mustKey(t, c, "alice", "alice-2")
	third, _ := testKey(t, "alice-3")

	cases := []struct {
		name string
		// over is the write past the cap, and within one still allowed at it.
		over, within func() error
		limit        string
		max          int
	}{
		{name: "projects",
			over:   func() error { _, err := c.AddProject(ctx, dana, newProject(projC)); return err },
			within: func() error { _, err := c.AddProject(ctx, dana, newProject(projA)); return err },
			limit:  "projects_per_server", max: 2},
		{name: "members per project",
			over:   func() error { _, err := c.SetMember(ctx, dana, "carol", projA, RoleMember); return err },
			within: func() error { _, err := c.SetMember(ctx, dana, "bob", projA, RoleAdmin); return err },
			limit:  "members_per_project", max: 2},
		{name: "members per project, with a new principal",
			over:   func() error { _, err := c.AddPrincipal(ctx, dana, "erin", projA); return err },
			within: func() error { _, err := c.AddPrincipal(ctx, dana, "erin", projB); return err },
			limit:  "members_per_project", max: 2},
		{name: "principals",
			over:   func() error { _, err := c.AddPrincipal(ctx, dana, "frank", ""); return err },
			within: func() error { _, err := c.DisablePrincipal(ctx, dana, "carol"); return err },
			limit:  "principals_per_server", max: 4},
		{name: "keys per principal",
			over:   func() error { _, err := c.AddKey(ctx, dana, "alice", third); return err },
			within: func() error { _, err := c.AddKey(ctx, dana, "bob", third); return err },
			limit:  "keys_per_principal", max: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			err := tc.over()
			var le *ControlLimitError
			if !errors.As(err, &le) || !errors.Is(err, ErrInvalid) || le.Limit != tc.limit || le.Max != tc.max {
				t.Errorf("past the cap = %v, want a *ControlLimitError for %s at %d, wrapping ErrInvalid", err, tc.limit, tc.max)
			}
			if err != nil && !strings.Contains(err.Error(), fmt.Sprint(tc.max)) {
				t.Errorf("refusal %q does not give the cap, %d", err, tc.max)
			}
			wantEvents(t, c, seq)
			if err := tc.within(); err != nil {
				t.Errorf("a write that adds no row past the cap: %v", err)
			}
		})
	}
	// The refused principal was not left behind without its membership.
	if _, err := c.Principal(ctx, "frank"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Principal(frank) = %v, want ErrNotFound", err)
	}
}

// TestControlPrincipalsPerCreator pins the cap on the principals one
// principal who is no server admin may create (security review of stage
// 3a, F-8): principals are never deleted and their names are the whole
// server's, so without it one project admin could use up the server's.
func TestControlPrincipalsPerCreator(t *testing.T) {
	t.Parallel()
	c := openControl(t, newDSN(t), ControlOptions{Limits: ControlLimits{Principals: 12, PrincipalsPerCreator: 2}})
	ctx := t.Context()
	mustProject(t, c, projA)
	mustProject(t, c, projB)
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "erin", projB+"=admin")
	add := func(by Actor, name, project string) error {
		_, err := c.AddPrincipal(ctx, by, name, project)
		return err
	}
	for _, name := range []string{"a1", "a2"} {
		if err := add(alice, name, projA); err != nil {
			t.Fatalf("AddPrincipal(%s) by alice, within the cap: %v", name, err)
		}
	}
	// A disabled principal still has its row, and its name.
	if _, err := c.DisablePrincipal(ctx, alice, "a1"); err != nil {
		t.Fatal(err)
	}
	seq := controlSeq(t, c)
	err := add(alice, "a3", projA)
	if le, ok := errors.AsType[*ControlLimitError](err); !ok || !errors.Is(err, ErrInvalid) || le.Limit != "principals_per_creator" || le.Max != 2 {
		t.Errorf("AddPrincipal(a3) by alice, who created 2 = %v, want a *ControlLimitError for principals_per_creator at 2, wrapping ErrInvalid", err)
	}
	if err != nil && !strings.Contains(err.Error(), "2") {
		t.Errorf("refusal %q does not give the cap, 2", err)
	}
	wantEvents(t, c, seq)
	if _, err := c.Principal(ctx, "a3"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Principal(a3) = %v, want ErrNotFound: the refused add left nothing", err)
	}
	// The cap is each creator's own, and a server admin has none.
	if err := add(erin, "e1", projB); err != nil {
		t.Errorf("AddPrincipal(e1) by erin, another project admin: %v", err)
	}
	for _, name := range []string{"d1", "d2", "d3"} {
		if err := add(dana, name, ""); err != nil {
			t.Errorf("AddPrincipal(%s) by a server admin, past a project admin's cap: %v", name, err)
		}
	}
}

func TestControlLimitsDefaults(t *testing.T) {
	t.Parallel()
	want := ControlLimits{Projects: 500, Principals: 5000, PrincipalsPerCreator: 500, Keys: 20, Members: 1000, Identities: 8, Invites: 1000, Grants: 64}
	if DefaultControlLimits != want {
		t.Errorf("DefaultControlLimits = %+v, want %+v", DefaultControlLimits, want)
	}
	if got := (ControlLimits{}).withDefaults(); got != want {
		t.Errorf("ControlLimits{}.withDefaults() = %+v, want %+v", got, want)
	}
	if got := (ControlLimits{Keys: 3}).withDefaults(); got.Keys != 3 || got.Projects != 500 {
		t.Errorf("ControlLimits{Keys: 3}.withDefaults() = %+v, want keys 3 and the other defaults", got)
	}
	for _, tc := range []struct {
		l    ControlLimits
		name string
	}{
		{ControlLimits{Projects: -1}, "projects"},
		{ControlLimits{Principals: -1}, "principals"},
		{ControlLimits{PrincipalsPerCreator: -1}, "principals_per_creator"},
		{ControlLimits{Keys: -1}, "keys_per_principal"},
		{ControlLimits{Members: -1}, "members_per_project"},
		{ControlLimits{Identities: -1}, "identities_per_principal"},
		{ControlLimits{Invites: -1}, "invites_per_server"},
	} {
		if err := tc.l.Validate(); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%+v.Validate() = %v, want ErrInvalid naming %s", tc.l, err, tc.name)
		}
	}
	if err := (ControlLimits{}).Validate(); err != nil {
		t.Errorf("ControlLimits{}.Validate() = %v, want nil", err)
	}
}

// A project's admins are listed from memory, as a project store asks of
// a gate that names no approvers: its own admins, then the server's,
// each active and listed once.
func TestControlProjectAdmins(t *testing.T) {
	t.Parallel()
	c := accessWorld(t)
	tests := []struct {
		project string
		want    []string
	}{
		{projA, []string{"alice", "ivy", "dana", "sam"}},
		{projB, []string{"erin", "ivy", "dana", "sam"}},
		{projC, []string{"dana", "sam"}},
	}
	for _, tc := range tests {
		if got := c.ProjectAdmins(tc.project); !slices.Equal(got, tc.want) {
			t.Errorf("ProjectAdmins(%s) = %v, want %v", tc.project, got, tc.want)
		}
	}
	if _, err := c.DisablePrincipal(t.Context(), Actor{Principal: "dana", Session: "s", Machine: "m"}, "ivy"); err != nil {
		t.Fatal(err)
	}
	if got, want := c.ProjectAdmins(projA), []string{"alice", "dana", "sam"}; !slices.Equal(got, want) {
		t.Errorf("ProjectAdmins(%s) once ivy is disabled = %v, want %v", projA, got, want)
	}
}
