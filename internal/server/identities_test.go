package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// bindIdentity binds identity id as a sign-in will (slice T2), behind
// the server's back: N1 has no op that binds.
func (m *multi) bindIdentity(t *testing.T, id int64, subject string) {
	t.Helper()
	m.asSuperuser(t, fmt.Sprintf(`UPDATE %s.identities SET issuer = 'https://login.example.com', subject = '%s', bound_at = NOW(),
  invite_until = NULL, rev = rev + 1 WHERE id = %d`, store.DefaultSchema, subject, id))
	if err := m.ctl.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// The identity ops of a server admin, end to end: invite, renew, list,
// show, set a principal's kind and display name, and remove.
func TestIdentityOps(t *testing.T) {
	t.Parallel()
	ck := newClock()
	m := newMulti(t, Limits{}, ck)
	m.project(t, projA, "alice=admin", "bob")
	d := mustDialTo(t, m.Server, dana, projA)

	inv := mustAsk[proto.IdentityResult](d, proto.OpInviteAdd, proto.InviteAddArgs{Name: "bob", Provider: "google", Address: "Bob@example.com"})
	until := ck.now().Add(store.DefaultInviteTTL)
	if inv.Change != "added" || inv.Principal != "bob" || inv.Provider != "google" || inv.Address != "Bob@example.com" ||
		inv.State != proto.IdentityPending || inv.InvitedBy != "dana" || inv.ExpiresAt == nil || !inv.ExpiresAt.Equal(until) || inv.ID < 1 {
		t.Errorf("invite.add = %+v, want bob's invitation, pending until %v", inv, until)
	}
	ck.advance(time.Hour)
	again := mustAsk[proto.IdentityResult](d, proto.OpInviteAdd, proto.InviteAddArgs{Name: "bob", Provider: "google", Address: "bob@example.com"})
	if again.Change != "renewed" || again.ID != inv.ID || !again.ExpiresAt.Equal(until.Add(time.Hour)) {
		t.Errorf("invite.add again = %+v, want invitation %d renewed", again, inv.ID)
	}
	other := mustAsk[proto.IdentityResult](d, proto.OpInviteAdd, proto.InviteAddArgs{Name: "bob", Provider: "entra", Address: "bob@example.com"})

	list := mustAsk[proto.InvitesResult](d, proto.OpInvites, proto.InvitesArgs{})
	if len(list.Invites) != 2 || list.Invites[0].ID != inv.ID || list.Invites[1].ID != other.ID || list.More != 0 {
		t.Errorf("invites = %+v, want %d and %d", list, inv.ID, other.ID)
	}
	paged := mustAsk[proto.InvitesResult](d, proto.OpInvites, proto.InvitesArgs{AdminListArgs: proto.AdminListArgs{Limit: 1}})
	if len(paged.Invites) != 1 || paged.More != 1 {
		t.Errorf("invites -n 1 = %+v, want one and 1 more", paged)
	}
	rest := mustAsk[proto.InvitesResult](d, proto.OpInvites, proto.InvitesArgs{AdminListArgs: proto.AdminListArgs{After: fmt.Sprint(inv.ID)}})
	if len(rest.Invites) != 1 || rest.Invites[0].ID != other.ID {
		t.Errorf("invites --after %d = %+v, want %d", inv.ID, rest, other.ID)
	}

	// Bound, an identity is no invitation, and shows its issuer.
	m.bindIdentity(t, other.ID, "oid-1")
	if got := mustAsk[proto.InvitesResult](d, proto.OpInvites, proto.InvitesArgs{}); len(got.Invites) != 1 {
		t.Errorf("invites with one bound = %+v, want the other alone", got)
	}
	ids := mustAsk[proto.IdentitiesResult](d, proto.OpIdentities, proto.IdentitiesArgs{Name: "bob"})
	if len(ids.Identities) != 2 || ids.Identities[1].State != proto.IdentityBound || ids.Identities[1].Issuer != "https://login.example.com" ||
		ids.Identities[1].ExpiresAt != nil || ids.Identities[1].BoundAt == nil {
		t.Errorf("identities bob = %+v, want the invitation and the bound identity", ids)
	}
	if raw := mustAsk[json.RawMessage](d, proto.OpIdentities, proto.IdentitiesArgs{Name: "bob"}); strings.Contains(string(raw), "oid-1") {
		t.Errorf("identities sends the subject: %s", raw)
	}
	// bob may list his own identities.
	b := mustDialTo(t, m.Server, bob, projA)
	if own := mustAsk[proto.IdentitiesResult](b, proto.OpIdentities, proto.IdentitiesArgs{Name: "bob"}); len(own.Identities) != 2 {
		t.Errorf("identities bob, by bob = %+v", own)
	}

	// A principal's kind and display name.
	set := mustAsk[proto.PrincipalResult](d, proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "alice", DisplayName: new("Alice Example")})
	if set.Change != "changed" || set.DisplayName != "Alice Example" || set.Kind != proto.KindPerson || set.Rev != 2 {
		t.Errorf("principal.set alice = %+v, want her display name set", set)
	}
	same := mustAsk[proto.PrincipalResult](d, proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "alice", DisplayName: new("Alice Example")})
	if same.Change != "unchanged" || same.Rev != 2 {
		t.Errorf("principal.set alice again = %+v, want unchanged", same)
	}
	add := mustAsk[proto.PrincipalAddResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "ci-bot", Kind: proto.KindService, Owner: "alice",
		DisplayName: "CI"})
	if add.Kind != proto.KindService || add.Owner != "alice" || add.DisplayName != "CI" || add.Invitation != nil {
		t.Errorf("principal.add ci-bot = %+v, want a service owned by alice", add)
	}
	carolAdd := mustAsk[proto.PrincipalAddResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "carol", Invite: "carol@example.com",
		Provider: "google"})
	if carolAdd.Invitation == nil || carolAdd.Invitation.Address != "carol@example.com" || carolAdd.Invitation.State != proto.IdentityPending {
		t.Errorf("principal.add carol with an invitation = %+v, want the invitation in the result", carolAdd)
	}
	ps := mustAsk[proto.PrincipalsResult](d, proto.OpPrincipals, proto.PrincipalsArgs{})
	for _, p := range ps.Principals {
		if p.Name == "alice" && (p.DisplayName != "Alice Example" || p.Kind != proto.KindPerson) ||
			p.Name == "ci-bot" && (p.Kind != proto.KindService || p.Owner != "alice") {
			t.Errorf("principals lists %+v", p)
		}
	}
	show := mustAsk[proto.PrincipalShowResult](d, proto.OpPrincipalShow, proto.PrincipalArgs{Name: "bob"})
	if show.Name != "bob" || show.Kind != proto.KindPerson || len(show.Identities) != 2 || show.Keys == nil {
		t.Errorf("principal.show bob = %+v, want bob with his two identities", show)
	}
	if own := mustAsk[proto.PrincipalShowResult](b, proto.OpPrincipalShow, proto.PrincipalArgs{Name: "bob"}); own.Name != "bob" {
		t.Errorf("principal.show bob, by bob = %+v", own)
	}

	// Removing.
	gone := mustAsk[proto.IdentityResult](d, proto.OpInviteRemove, proto.IdentityArgs{ID: inv.ID})
	if gone.ID != inv.ID || gone.Address != "bob@example.com" {
		t.Errorf("invite.remove %d = %+v", inv.ID, gone)
	}
	if bound := mustAsk[proto.IdentityResult](d, proto.OpIdentityRemove, proto.IdentityArgs{ID: other.ID}); bound.State != proto.IdentityBound {
		t.Errorf("identity.remove %d = %+v, want the bound identity", other.ID, bound)
	}
	if left := mustAsk[proto.IdentitiesResult](d, proto.OpIdentities, proto.IdentitiesArgs{Name: "bob"}); len(left.Identities) != 0 || left.Identities == nil {
		t.Errorf("identities bob after removing both = %+v, want an empty list", left)
	}

	// The status report counts them.
	st := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
	if st.Server == nil || st.Server.Identities[proto.IdentityPending] != 1 || len(st.Server.Identities) != 1 {
		t.Errorf("status identities = %+v, want carol's one pending invitation", st.Server)
	}
}

// Every refusal of an identity op says what to do next (rule 11).
func TestIdentityRefusals(t *testing.T) {
	t.Parallel()
	m := newMulti(t, Limits{Control: store.ControlLimits{Identities: 2, Invites: 3}}, nil)
	m.project(t, projA, "alice=admin", "bob", "carol", "dana", "hal")
	ctx := t.Context()
	if _, err := m.ctl.AddNewPrincipal(ctx, dana, store.NewPrincipal{Name: "ci-bot", Kind: store.KindService, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	invite := func(name, provider, address string) int64 {
		t.Helper()
		id, _, err := m.ctl.Invite(ctx, dana, name, store.Invitation{Provider: provider, Address: address})
		if err != nil {
			t.Fatal(err)
		}
		return id.ID
	}
	bobs := invite("bob", "google", "bob@example.com")
	bound := invite("bob", "entra", "bob@example.com")
	m.bindIdentity(t, bound, "oid-1")
	invite("carol", "google", "carol@example.com")
	invite("hal", "google", "hal@example.com")
	conns := map[string]*conn{"dana": mustDialTo(t, m.Server, dana, projA), "alice": mustDialTo(t, m.Server, alice, projA),
		"bob": mustDialTo(t, m.Server, bob, projA)}

	cases := []struct {
		name string
		who  string
		op   string
		args any
		code proto.Code
		msg  string // part of the message
		fix  string // part of the fix
	}{
		{"a project admin invites", "alice", proto.OpInviteAdd, proto.InviteAddArgs{Name: "carol", Provider: "google", Address: "c@example.com"},
			proto.CodeForbidden, "alice may not invite an identity for carol: it is for server admins", "ask a server admin"},
		{"a server admin invites for themselves", "dana", proto.OpInviteAdd, proto.InviteAddArgs{Name: "dana", Provider: "google", Address: "d@example.com"},
			proto.CodeForbidden, "nobody invites an identity onto their own principal", "ask another server admin"},
		{"an invitation for a service", "dana", proto.OpInviteAdd, proto.InviteAddArgs{Name: "ci-bot", Provider: "google", Address: "ci@example.com"},
			proto.CodeInvalid, "ci-bot is a service", "a service signs in by a key"},
		{"an address that is none", "dana", proto.OpInviteAdd, proto.InviteAddArgs{Name: "carol", Provider: "google", Address: "carol"},
			proto.CodeInvalid, `"carol" is not an address to invite`, "aries admin invite add -h"},
		{"an address invited for another", "dana", proto.OpInviteAdd, proto.InviteAddArgs{Name: "carol", Provider: "google", Address: "BOB@example.com"},
			proto.CodeExists, "bob@example.com is invited at google for bob already", fmt.Sprintf("aries admin invite remove %d", bobs)},
		{"past identities_per_principal", "dana", proto.OpInviteAdd, proto.InviteAddArgs{Name: "bob", Provider: "okta", Address: "bob@example.com"},
			proto.CodeInvalid, "a principal keeps at most 2 identities", "aries admin identity list bob"},
		{"past invites_per_server", "dana", proto.OpInviteAdd, proto.InviteAddArgs{Name: "carol", Provider: "okta", Address: "c@example.com"},
			proto.CodeInvalid, "at most 3 open invitations", "invites_per_server"},
		{"no principal", "dana", proto.OpInviteAdd, proto.InviteAddArgs{Name: "zed", Provider: "google", Address: "z@example.com"},
			proto.CodeNotFound, "principal zed not found", "aries admin principal list"},
		{"a bound identity as an invitation", "dana", proto.OpInviteRemove, proto.IdentityArgs{ID: bound},
			proto.CodeInvalid, "is bound to a sign-in", fmt.Sprintf("aries admin identity remove %d", bound)},
		{"no such invitation", "dana", proto.OpInviteRemove, proto.IdentityArgs{ID: 999},
			proto.CodeNotFound, "identity 999 not found", "aries admin invite list"},
		{"no such identity", "dana", proto.OpIdentityRemove, proto.IdentityArgs{ID: 999},
			proto.CodeNotFound, "identity 999 not found", "aries admin identity list NAME"},
		{"a project admin removes an invitation", "alice", proto.OpInviteRemove, proto.IdentityArgs{ID: bobs},
			proto.CodeForbidden, "it is for server admins", "ask a server admin"},
		{"a member removes their own identity", "bob", proto.OpIdentityRemove, proto.IdentityArgs{ID: bound},
			proto.CodeForbidden, "it is for server admins", "ask a server admin"},
		{"a project admin lists the invitations", "alice", proto.OpInvites, proto.InvitesArgs{},
			proto.CodeForbidden, "it is for server admins", "ask a server admin"},
		{"a page past the most", "dana", proto.OpInvites, proto.InvitesArgs{AdminListArgs: proto.AdminListArgs{Limit: proto.AdminListMax + 1}},
			proto.CodeInvalid, "out of range", "give -n from 1 to 1000"},
		{"a page after no id", "dana", proto.OpInvites, proto.InvitesArgs{AdminListArgs: proto.AdminListArgs{After: "bob"}},
			proto.CodeInvalid, `"bob" is no invitation's id`, "--after takes the id"},
		{"a project admin lists another's identities", "alice", proto.OpIdentities, proto.IdentitiesArgs{Name: "bob"},
			proto.CodeForbidden, "alice may not list the identities of bob", "aries admin identity list alice"},
		{"a member shows another", "bob", proto.OpPrincipalShow, proto.PrincipalArgs{Name: "alice"},
			proto.CodeForbidden, "bob may not show alice", "aries admin principal show bob"},
		{"a server admin shows no one", "dana", proto.OpPrincipalShow, proto.PrincipalArgs{Name: "zed"},
			proto.CodeNotFound, "principal zed not found", "aries admin principal list"},
		{"a project admin sets a display name", "alice", proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "bob", DisplayName: new("Bob")},
			proto.CodeForbidden, "alice may not set bob: it is for server admins", "ask a server admin"},
		{"a service with no owner", "dana", proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "alice", Kind: proto.KindService},
			proto.CodeInvalid, "service alice needs an owner", "--owner"},
		{"a person with identities as a service", "dana", proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "bob", Kind: proto.KindService, Owner: "alice"},
			proto.CodeInvalid, "bob is a service, or has identities", "aries admin identity list bob"},
		{"a person with an owner", "dana", proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "carol", Owner: "alice"},
			proto.CodeInvalid, "a person has no owner", "--kind service"},
		{"the owner of a service as a service", "dana", proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "alice", Kind: proto.KindService, Owner: "carol"},
			proto.CodeInvalid, "alice owns services", "aries admin principal list"},
		{"a kind there is not", "dana", proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "carol", Kind: "robot"},
			proto.CodeInvalid, `kind "robot"`, "aries admin principal set -h"},
		{"nothing to set", "dana", proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "carol"},
			proto.CodeInvalid, "principal set changes nothing", "--display-name"},
		{"an invitation with no provider", "dana", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "gus", Invite: "g@example.com"},
			proto.CodeInvalid, "an invitation needs a provider", "--provider"},
		{"a provider with no invitation", "dana", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "gus", Provider: "google"},
			proto.CodeInvalid, "a provider is for an invitation", "--invite ADDRESS"},
		{"a project admin adds with an invitation", "alice", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "gus", Invite: "g@example.com", Provider: "google"},
			proto.CodeForbidden, "alice may not invite an identity for gus", "ask a server admin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := ask[json.RawMessage](conns[tc.who], tc.op, tc.args)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Errorf("%s %+v by %s = %+v\nwant %s, a message with %q and a fix with %q", tc.op, tc.args, tc.who, perr, tc.code, tc.msg, tc.fix)
			}
		})
	}
	if _, err := m.ctl.Principal(ctx, "gus"); err == nil {
		t.Error("a refused principal.add left gus behind")
	}
}
