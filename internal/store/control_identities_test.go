package store

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// identityAt is when the identity tests' clocks start.
var identityAt = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

// clockControl opens a control store whose clock the test moves, with
// opts' limits and invitation lifetime.
func clockControl(t *testing.T, opts ControlOptions) (*Control, *clock) {
	t.Helper()
	clk := &clock{t: identityAt}
	opts.Now = clk.now
	return openControl(t, newDSN(t), opts), clk
}

// mustInvite invites address at provider for principal, as dana.
func mustInvite(t *testing.T, c *Control, principal, provider, address string) Identity {
	t.Helper()
	id, _, err := c.Invite(t.Context(), dana, principal, Invitation{Provider: provider, Address: address})
	if err != nil {
		t.Fatalf("Invite(%s, %s, %s): %v", principal, provider, address, err)
	}
	return id
}

// bind binds identity id to issuer and subject as a sign-in will (slice
// T2), by hand: N1 has no op that binds.
func bind(t *testing.T, c *Control, id int64, issuer, subject string) error {
	t.Helper()
	_, err := c.s.w.ExecContext(t.Context(), `UPDATE identities SET issuer = $1, subject = $2, bound_at = $3, invite_until = NULL, rev = rev + 1 WHERE id = $4`,
		issuer, subject, c.Now(), id)
	return err
}

// The migration makes every principal there was a person, with no
// display name and no owner, and adds the identities, empty.
func TestControlMigrationIdentities(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	// The control store as the two migrations before identities left it.
	before := fstest.MapFS{}
	for _, name := range []string{"0001_init.sql", "0002_schema_name.sql"} {
		b, err := fs.ReadFile(controlMigrationFiles, "controlmigrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		before["controlmigrations/"+name] = &fstest.MapFile{Data: b}
	}
	old := controlSchema
	old.fsys = before
	db := testDB(t, dsn)
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn := db.sqlDB(d.Schema, 1)
	t.Cleanup(func() { _ = conn.Close() })
	if n, err := migrate(t.Context(), conn, identityAt, old, true); err != nil || n != 2 {
		t.Fatalf("migrate to version 2 ran %d: %v", n, err)
	}
	if _, err := conn.ExecContext(t.Context(), `INSERT INTO principals (name, state, created_by, created_at, updated_at, rev)
  VALUES ('alice', 'active', 'import', $1, $1, 1)`, identityAt); err != nil {
		t.Fatal(err)
	}

	c := openControl(t, dsn, ControlOptions{})
	p, err := c.Principal(t.Context(), "alice")
	if err != nil || p.Kind != KindPerson || p.DisplayName != "" || p.Owner != "" || p.Rev != 1 {
		t.Errorf("Principal(alice) after the migration = %+v, %v; want a person with no display name or owner, at rev 1", p, err)
	}
	if ids, err := c.Identities(t.Context(), "alice"); err != nil || len(ids) != 0 {
		t.Errorf("Identities(alice) after the migration = %+v, %v; want none", ids, err)
	}
	// The schema's own rules hold, whatever the store checks first.
	for _, stmt := range []string{
		`UPDATE principals SET kind = 'robot' WHERE name = 'alice'`,
		`UPDATE principals SET kind = 'service' WHERE name = 'alice'`,
		`UPDATE principals SET owner = 'nobody' WHERE name = 'alice'`,
		`INSERT INTO identities (principal, provider, invited_as, invited_by, invited_at, rev) VALUES ('alice', 'google', 'a@example.com', 'dana', NOW(), 1)`,
		`INSERT INTO identities (principal, provider, invited_as, invited_by, invited_at, invite_until, rev) VALUES ('nobody', 'google', 'a@example.com', 'dana', NOW(), NOW(), 1)`,
	} {
		if _, err := c.s.w.ExecContext(t.Context(), stmt); err == nil {
			t.Errorf("%s: no error, want the schema to refuse it", stmt)
		}
	}
}

// One person per external identity, and one open invitation per
// address and provider, the address without case: the schema's unique
// indexes, which a sign-in (T2) relies on.
func TestIdentityUniqueness(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")
	a := mustInvite(t, c, "alice", "google", "alice@example.com")
	b := mustInvite(t, c, "bob", "google", "bob@example.com")
	if err := bind(t, c, a.ID, "https://accounts.google.com", "1001"); err != nil {
		t.Fatal(err)
	}
	if err := bind(t, c, b.ID, "https://accounts.google.com", "1001"); err == nil {
		t.Errorf("binding bob's identity to alice's subject: no error, want the unique index to refuse it")
	}
	if err := bind(t, c, b.ID, "https://login.example.com", "1001"); err != nil {
		t.Errorf("the same subject at another issuer: %v, want it bound", err)
	}
	// Once bound, the address may be invited again: nothing matches a
	// bound identity by its address.
	if _, _, err := c.Invite(ctx, dana, "bob", Invitation{Provider: "google", Address: "alice@example.com"}); err != nil {
		t.Errorf("inviting the address of a bound identity: %v", err)
	}
	if _, err := c.s.w.ExecContext(ctx, `INSERT INTO identities (principal, provider, invited_as, invited_by, invited_at, invite_until, rev)
  VALUES ('alice', 'google', 'ALICE@example.COM', 'dana', $1, $1, 1)`, identityAt); err == nil {
		t.Errorf("a second open invitation of an address in other case: no error, want the unique index to refuse it")
	}
	// Case is folded for ASCII letters alone, by the index and by the
	// store's lookup alike: these are two addresses.
	if _, _, err := c.Invite(ctx, dana, "alice", Invitation{Provider: "okta", Address: "zoë@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Invite(ctx, dana, "bob", Invitation{Provider: "okta", Address: "ZOË@example.com"}); err != nil {
		t.Errorf("inviting an address that differs in the case of a letter outside ASCII: %v", err)
	}
}

func TestControlInvite(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")

	seq := controlSeq(t, c)
	got, changed, err := c.Invite(ctx, dana, "alice", Invitation{Provider: "google", Address: "Alice@Example.com"})
	want := Identity{ID: got.ID, Principal: "alice", Provider: "google", InvitedAs: "Alice@Example.com", InvitedBy: "dana",
		InvitedAt: identityAt, InviteUntil: identityAt.Add(DefaultInviteTTL), Rev: 1}
	if err != nil || !changed || got != want || got.ID < 1 {
		t.Fatalf("Invite = %+v, %t, %v; want %+v, changed", got, changed, err, want)
	}
	if got.State(clk.now()) != IdentityPending {
		t.Errorf("a new invitation's state = %s, want %s", got.State(clk.now()), IdentityPending)
	}
	wantEvents(t, c, seq, ev{OpIdentityInvite, "alice", dana, `"provider":"google"`})
	// The event names no address: an address is personal, and the log is
	// kept for good.
	evs, err := c.Events(ctx, seq, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if strings.Contains(strings.ToLower(string(e.After)), "example.com") {
			t.Errorf("event %s after state %s holds the address", e.Op, e.After)
		}
	}
	if ids, err := c.Identities(ctx, "alice"); err != nil || len(ids) != 1 || ids[0] != want {
		t.Errorf("Identities(alice) = %+v, %v; want [%+v]", ids, err, want)
	}
	if one, err := c.Identity(ctx, got.ID); err != nil || one != want {
		t.Errorf("Identity(%d) = %+v, %v; want %+v", got.ID, one, err, want)
	}

	// Inviting the same address again, in any case, renews the
	// invitation: its end moves, and it stays one row.
	clk.add(time.Hour)
	again, changed, err := c.Invite(ctx, dana, "alice", Invitation{Provider: "google", Address: "alice@example.com"})
	if err != nil || !changed || again.ID != got.ID || again.Rev != 2 || !again.InviteUntil.Equal(clk.now().Add(DefaultInviteTTL)) ||
		again.InvitedAs != "alice@example.com" {
		t.Errorf("Invite again = %+v, %t, %v; want invitation %d renewed to %v at rev 2, as typed now", again, changed, err, got.ID, clk.now().Add(DefaultInviteTTL))
	}
	// Another principal is refused the address while it is open, and told
	// whose it is.
	seq = controlSeq(t, c)
	_, _, err = c.Invite(ctx, dana, "bob", Invitation{Provider: "google", Address: "ALICE@example.com"})
	if ie, ok := errors.AsType[*InvitedError](err); !ok || !errors.Is(err, ErrConflict) || ie.Principal != "alice" || ie.ID != got.ID {
		t.Errorf("Invite(bob) of alice's address = %v, want an *InvitedError naming alice and invitation %d, wrapping ErrConflict", err, got.ID)
	}
	wantEvents(t, c, seq)
	// Another provider is another identity, never joined by address.
	if _, _, err := c.Invite(ctx, dana, "bob", Invitation{Provider: "entra", Address: "alice@example.com"}); err != nil {
		t.Errorf("Invite(bob) of the address at another provider: %v", err)
	}
}

// Who may invite, and what: a server admin, never onto their own
// principal, and only for a person.
func TestControlInviteRefusals(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustProject(t, c, projA)
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "bob", projA+"=member")
	mustPrincipal(t, c, "dana")
	if _, err := c.AddNewPrincipal(ctx, dana, NewPrincipal{Name: "ci-bot", Kind: KindService, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}
	google := func(address string) Invitation { return Invitation{Provider: "google", Address: address} }
	cases := []struct {
		name      string
		actor     Actor
		principal string
		inv       Invitation
		check     func(error) bool
	}{
		{"a project admin", alice, "bob", google("bob@example.com"), access(AccessServerAdmin)},
		{"a member", bob, "bob", google("bob@example.com"), access(AccessOwnIdentity)},
		{"a member, for another", bob, "alice", google("alice@example.com"), access(AccessServerAdmin)},
		{"a server admin, onto their own principal", dana, "dana", google("dana@example.com"), access(AccessOwnIdentity)},
		{"a server admin with no row, onto their own name", sam, "sam", google("sam@example.com"), access(AccessOwnIdentity)},
		{"a principal no one has", dana, "nobody", google("x@example.com"), is(ErrNotFound)},
		{"a service principal", dana, "ci-bot", google("ci@example.com"), identityRule(IdentityRuleService)},
		{"a reserved name", dana, "ariesd", google("x@example.com"), is(ErrInvalid)},
		{"no provider", dana, "bob", google(""), is(ErrInvalid)},
		{"a provider in capitals", dana, "bob", Invitation{Provider: "Google", Address: "bob@example.com"}, is(ErrInvalid)},
		{"no address", dana, "bob", google(""), is(ErrInvalid)},
		{"no at sign", dana, "bob", google("bob.example.com"), is(ErrInvalid)},
		{"two at signs", dana, "bob", google("bob@x@example.com"), is(ErrInvalid)},
		{"no local part", dana, "bob", google("@example.com"), is(ErrInvalid)},
		{"no domain", dana, "bob", google("bob@"), is(ErrInvalid)},
		{"a domain with no dot", dana, "bob", google("bob@localhost"), is(ErrInvalid)},
		{"a domain with an empty label", dana, "bob", google("bob@example..com"), is(ErrInvalid)},
		{"a space", dana, "bob", google("bob smith@example.com"), is(ErrInvalid)},
		{"a display form", dana, "bob", google("Bob <bob@example.com>"), is(ErrInvalid)},
		{"a control character", dana, "bob", google("bob\x1b@example.com"), is(ErrInvalid)},
		{"too long", dana, "bob", google(strings.Repeat("b", 64) + "@" + strings.Repeat("e", 186) + ".com"), is(ErrInvalid)},
		{"a local part too long", dana, "bob", google(strings.Repeat("b", 65) + "@example.com"), is(ErrInvalid)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			if _, _, err := c.Invite(ctx, tc.actor, tc.principal, tc.inv); !tc.check(err) {
				t.Errorf("Invite(%s, %+v) by %s = %v", tc.principal, tc.inv, tc.actor.Principal, err)
			}
			wantEvents(t, c, seq)
		})
	}
	// Disabled wins (decision 20): a disabled server admin invites no one.
	if _, err := c.DisablePrincipal(ctx, sam, "dana"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Invite(ctx, dana, "bob", google("bob@example.com")); !access(AccessServerAdmin)(err) {
		t.Errorf("Invite by a disabled server admin = %v, want an *AccessError for server admins", err)
	}
	// The longest address there may be is taken.
	long := strings.Repeat("b", 64) + "@" + strings.Repeat("e", 185) + ".com"
	if _, _, err := c.Invite(ctx, sam, "bob", google(long)); err != nil {
		t.Errorf("Invite of a %d-byte address: %v", len(long), err)
	}
}

// access is a check that an error is an *AccessError for reason.
func access(reason AccessReason) func(error) bool {
	return func(err error) bool {
		ae, ok := errors.AsType[*AccessError](err)
		return ok && ae.Reason == reason && errors.Is(err, ErrForbidden)
	}
}

// is is a check that an error wraps target.
func is(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }

// identityRule is a check that an error is an *IdentityError for rule.
func identityRule(rule IdentityRule) func(error) bool {
	return func(err error) bool {
		ie, ok := errors.AsType[*IdentityError](err)
		return ok && ie.Rule == rule && errors.Is(err, ErrInvalid)
	}
}

// An invitation lapses unused, after the invitation lifetime: it is no
// longer pending, no longer counts against invites_per_server, and
// inviting the address again opens it again.
func TestControlInviteExpiry(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{InviteTTL: 48 * time.Hour, Limits: ControlLimits{Invites: 1}})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")
	inv := mustInvite(t, c, "alice", "google", "alice@example.com")
	if want := identityAt.Add(48 * time.Hour); !inv.InviteUntil.Equal(want) {
		t.Errorf("an invitation with InviteTTL 48h ends at %v, want %v", inv.InviteUntil, want)
	}
	pending := func() []int64 {
		t.Helper()
		ids, more, err := c.Invites(ctx, InviteQuery{Pending: true})
		if err != nil || more != 0 {
			t.Fatalf("Invites(pending) = %v, %d more, %v", ids, more, err)
		}
		var out []int64
		for _, i := range ids {
			out = append(out, i.ID)
		}
		return out
	}
	if got := pending(); len(got) != 1 || got[0] != inv.ID {
		t.Errorf("pending invitations = %v, want [%d]", got, inv.ID)
	}

	clk.add(48*time.Hour - time.Microsecond)
	if s := inv.State(clk.now()); s != IdentityPending {
		t.Errorf("state a microsecond before the end = %s, want pending", s)
	}
	clk.add(time.Microsecond)
	if s := inv.State(clk.now()); s != IdentityLapsed {
		t.Errorf("state at the end = %s, want lapsed", s)
	}
	if got := pending(); len(got) != 0 {
		t.Errorf("pending invitations after the end = %v, want none", got)
	}
	all, _, err := c.Invites(ctx, InviteQuery{})
	if err != nil || len(all) != 1 || all[0].State(clk.now()) != IdentityLapsed {
		t.Errorf("every invitation after the end = %+v, %v; want the lapsed one", all, err)
	}
	// A lapsed invitation leaves room under invites_per_server.
	if _, _, err := c.Invite(ctx, dana, "bob", Invitation{Provider: "google", Address: "bob@example.com"}); err != nil {
		t.Errorf("Invite(bob) beside a lapsed invitation, at invites_per_server 1: %v", err)
	}
	// And the lapsed one, invited again, is open again, which the cap now
	// refuses.
	_, _, err = c.Invite(ctx, dana, "alice", Invitation{Provider: "google", Address: "alice@example.com"})
	if le, ok := errors.AsType[*ControlLimitError](err); !ok || le.Limit != "invites_per_server" {
		t.Errorf("renewing a lapsed invitation at the cap = %v, want a *ControlLimitError for invites_per_server", err)
	}
	if _, err := c.RemoveInvite(ctx, dana, all[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestControlRemoveIdentity(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustProject(t, c, projA)
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "bob", projA+"=member")
	open := mustInvite(t, c, "bob", "google", "bob@example.com")
	bound := mustInvite(t, c, "bob", "entra", "bob@example.com")
	if err := bind(t, c, bound.ID, "https://login.example.com", "oid-1"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		remove func() error
		check  func(error) bool
	}{
		{"an invitation, by a project admin", func() error { _, err := c.RemoveInvite(ctx, alice, open.ID); return err }, access(AccessServerAdmin)},
		{"an invitation, by its principal", func() error { _, err := c.RemoveInvite(ctx, bob, open.ID); return err }, access(AccessServerAdmin)},
		{"an identity, by a project admin", func() error { _, err := c.RemoveIdentity(ctx, alice, bound.ID); return err }, access(AccessServerAdmin)},
		{"a bound identity, as an invitation", func() error { _, err := c.RemoveInvite(ctx, dana, bound.ID); return err }, identityRule(IdentityRuleBound)},
		{"an invitation no one has", func() error { _, err := c.RemoveInvite(ctx, dana, 999); return err }, is(ErrNotFound)},
		{"an identity no one has", func() error { _, err := c.RemoveIdentity(ctx, dana, 999); return err }, is(ErrNotFound)},
		{"an id that is none", func() error { _, err := c.RemoveIdentity(ctx, dana, 0); return err }, is(ErrInvalid)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			if err := tc.remove(); !tc.check(err) {
				t.Errorf("= %v", err)
			}
			wantEvents(t, c, seq)
		})
	}

	seq := controlSeq(t, c)
	if _, err := c.RemoveInvite(ctx, dana, open.ID); err != nil {
		t.Fatalf("RemoveInvite(%d): %v", open.ID, err)
	}
	gone, err := c.RemoveIdentity(ctx, dana, bound.ID)
	if err != nil || gone.ID != bound.ID || gone.Subject != "oid-1" {
		t.Fatalf("RemoveIdentity(%d) = %+v, %v; want the bound identity", bound.ID, gone, err)
	}
	wantEvents(t, c, seq, ev{OpIdentityRemove, "bob", dana, ""}, ev{OpIdentityRemove, "bob", dana, ""})
	evs, err := c.Events(ctx, seq, 0)
	if err != nil || len(evs) != 2 {
		t.Fatalf("Events = %+v, %v", evs, err)
	}
	var removed identityState
	if err := json.Unmarshal(evs[1].Before, &removed); err != nil || removed.ID != bound.ID || !removed.Bound || removed.Provider != "entra" ||
		evs[1].After != nil {
		t.Errorf("identity.remove states = %s, %s (%v); want before the bound identity %d and no after", evs[1].Before, evs[1].After, err, bound.ID)
	}
	if strings.Contains(string(evs[1].Before), "oid-1") || strings.Contains(string(evs[0].Before), "example.com") {
		t.Errorf("identity.remove before states %s and %s hold a subject or an address", evs[0].Before, evs[1].Before)
	}
	if ids, err := c.Identities(ctx, "bob"); err != nil || len(ids) != 0 {
		t.Errorf("Identities(bob) after removing both = %+v, %v; want none", ids, err)
	}
	// The address is free again, for anyone.
	if _, _, err := c.Invite(ctx, dana, "alice", Invitation{Provider: "google", Address: "bob@example.com"}); err != nil {
		t.Errorf("inviting a removed invitation's address for another: %v", err)
	}
}

// The caps on identities (rule 18): identities_per_principal counts every
// identity of a principal, bound, open or lapsed; invites_per_server
// counts the open invitations on the server.
func TestControlIdentityLimits(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{Limits: ControlLimits{Identities: 2, Invites: 3}})
	ctx := t.Context()
	for _, name := range []string{"alice", "bob", "carol"} {
		mustPrincipal(t, c, name)
	}
	mustInvite(t, c, "alice", "google", "alice@example.com")
	lapses := mustInvite(t, c, "alice", "entra", "alice@example.com")
	mustInvite(t, c, "bob", "google", "bob@example.com")

	cases := []struct {
		name         string
		over, within func() error
		limit        string
		max          int
	}{
		{name: "identities per principal",
			over: func() error {
				_, _, err := c.Invite(ctx, dana, "alice", Invitation{Provider: "okta", Address: "alice@example.com"})
				return err
			},
			within: func() error {
				_, _, err := c.Invite(ctx, dana, "alice", Invitation{Provider: "google", Address: "alice@example.com"})
				return err
			},
			limit: "identities_per_principal", max: 2},
		{name: "invites per server",
			over: func() error {
				_, _, err := c.Invite(ctx, dana, "carol", Invitation{Provider: "google", Address: "carol@example.com"})
				return err
			},
			within: func() error {
				_, _, err := c.Invite(ctx, dana, "bob", Invitation{Provider: "google", Address: "bob@example.com"})
				return err
			},
			limit: "invites_per_server", max: 3},
		{name: "invites per server, with a new principal",
			over: func() error {
				_, err := c.AddNewPrincipal(ctx, dana, NewPrincipal{Name: "dave", Invite: &Invitation{Provider: "google", Address: "dave@example.com"}})
				return err
			},
			within: func() error { _, err := c.AddNewPrincipal(ctx, dana, NewPrincipal{Name: "erin"}); return err },
			limit:  "invites_per_server", max: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			err := tc.over()
			le, ok := errors.AsType[*ControlLimitError](err)
			if !ok || !errors.Is(err, ErrInvalid) || le.Limit != tc.limit || le.Max != tc.max {
				t.Errorf("past the cap = %v, want a *ControlLimitError for %s at %d, wrapping ErrInvalid", err, tc.limit, tc.max)
			}
			if err != nil && !strings.Contains(err.Error(), "at most 2") && !strings.Contains(err.Error(), "at most 3") {
				t.Errorf("refusal %q does not give the cap", err)
			}
			wantEvents(t, c, seq)
			if err := tc.within(); err != nil {
				t.Errorf("a write that adds no row past the cap: %v", err)
			}
		})
	}
	if _, err := c.Principal(ctx, "dave"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Principal(dave) = %v, want ErrNotFound: the refused add left nothing", err)
	}
	// A lapsed invitation still counts against its principal's identities.
	clk.add(DefaultInviteTTL)
	if lapses.State(clk.now()) != IdentityLapsed {
		t.Fatal("the invitation did not lapse")
	}
	_, _, err := c.Invite(ctx, dana, "alice", Invitation{Provider: "okta", Address: "alice@example.com"})
	if le, ok := errors.AsType[*ControlLimitError](err); !ok || le.Limit != "identities_per_principal" {
		t.Errorf("Invite(alice) beside her lapsed invitation, at identities_per_principal 2 = %v, want refused", err)
	}
}

// A principal's kind, display name and owner: set by a server admin, on
// the record, with the display name's value never in the log.
func TestControlSetPrincipal(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustProject(t, c, projA)
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "bob", projA+"=member")
	mustPrincipal(t, c, "carol", projA+"=member")
	mustInvite(t, c, "carol", "google", "carol@example.com")
	name := func(s string) *string { return &s }

	seq := controlSeq(t, c)
	p, changed, err := c.SetPrincipal(ctx, dana, "bob", PrincipalChange{DisplayName: name("Bob Example")})
	if err != nil || !changed || p.DisplayName != "Bob Example" || p.Kind != KindPerson || p.Rev != 2 {
		t.Fatalf("SetPrincipal(bob, display) = %+v, %t, %v; want the display name set at rev 2", p, changed, err)
	}
	wantEvents(t, c, seq, ev{OpPrincipalSet, "bob", dana, `"fields":["display_name"]`})
	if got, err := c.Principal(ctx, "bob"); err != nil || got != p {
		t.Errorf("Principal(bob) = %+v, %v; want %+v", got, err, p)
	}
	evs, err := c.Events(ctx, seq, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(evs[0].After), "Bob Example") {
		t.Errorf("principal.set after state %s holds the display name", evs[0].After)
	}
	// The same again changes nothing, and records nothing.
	seq = controlSeq(t, c)
	if again, changed, err := c.SetPrincipal(ctx, dana, "bob", PrincipalChange{DisplayName: name("Bob Example")}); err != nil || changed || again != p {
		t.Errorf("SetPrincipal(bob) again = %+v, %t, %v; want it unchanged", again, changed, err)
	}
	wantEvents(t, c, seq)

	// A service answers to a person.
	seq = controlSeq(t, c)
	svc, changed, err := c.SetPrincipal(ctx, dana, "bob", PrincipalChange{Kind: KindService, Owner: "alice"})
	if err != nil || !changed || svc.Kind != KindService || svc.Owner != "alice" || svc.DisplayName != "Bob Example" {
		t.Fatalf("SetPrincipal(bob, service) = %+v, %t, %v", svc, changed, err)
	}
	wantEvents(t, c, seq, ev{OpPrincipalSet, "bob", dana, `"fields":["kind","owner"]`})
	if back, _, err := c.SetPrincipal(ctx, dana, "bob", PrincipalChange{Kind: KindPerson, DisplayName: name("")}); err != nil ||
		back.Kind != KindPerson || back.Owner != "" || back.DisplayName != "" {
		t.Errorf("SetPrincipal(bob, person, no display name) = %+v, %v; want a person with no owner and no display name", back, err)
	}
	if _, _, err := c.SetPrincipal(ctx, dana, "bob", PrincipalChange{Kind: KindService, Owner: "alice"}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		actor Actor
		who   string
		ch    PrincipalChange
		check func(error) bool
	}{
		{"by a project admin", alice, "carol", PrincipalChange{DisplayName: name("Carol")}, access(AccessServerAdmin)},
		{"by the principal", carolActor, "carol", PrincipalChange{DisplayName: name("Carol")}, access(AccessServerAdmin)},
		{"a principal no one has", dana, "nobody", PrincipalChange{DisplayName: name("N")}, is(ErrNotFound)},
		{"a service with no owner", dana, "alice", PrincipalChange{Kind: KindService}, identityRule(IdentityRuleOwner)},
		{"a service owned by itself", dana, "alice", PrincipalChange{Kind: KindService, Owner: "alice"}, identityRule(IdentityRuleOwner)},
		{"a service owned by a service", dana, "alice", PrincipalChange{Kind: KindService, Owner: "bob"}, identityRule(IdentityRuleOwner)},
		{"a service owned by no one", dana, "alice", PrincipalChange{Kind: KindService, Owner: "nobody"}, is(ErrNotFound)},
		{"a person with an owner", dana, "carol", PrincipalChange{Owner: "alice"}, identityRule(IdentityRulePersonOwner)},
		{"a person with identities, as a service", dana, "carol", PrincipalChange{Kind: KindService, Owner: "alice"}, identityRule(IdentityRuleService)},
		{"the owner of a service, as a service", dana, "alice", PrincipalChange{Kind: KindService, Owner: "carol"}, identityRule(IdentityRuleOwns)},
		{"a kind there is not", dana, "carol", PrincipalChange{Kind: "robot"}, is(ErrInvalid)},
		{"a display name with a control character", dana, "carol", PrincipalChange{DisplayName: name("Carol\x1b[31m")}, is(ErrInvalid)},
		{"a display name of two lines", dana, "carol", PrincipalChange{DisplayName: name("Carol\nExample")}, is(ErrInvalid)},
		{"a display name too long", dana, "carol", PrincipalChange{DisplayName: name(strings.Repeat("é", 129))}, is(ErrInvalid)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			if _, _, err := c.SetPrincipal(ctx, tc.actor, tc.who, tc.ch); !tc.check(err) {
				t.Errorf("SetPrincipal(%s, %+v) by %s = %v", tc.who, tc.ch, tc.actor.Principal, err)
			}
			wantEvents(t, c, seq)
		})
	}
	// 128 characters is the most, whatever their bytes.
	if p, _, err := c.SetPrincipal(ctx, dana, "carol", PrincipalChange{DisplayName: name(strings.Repeat("é", 128))}); err != nil ||
		p.DisplayName != strings.Repeat("é", 128) {
		t.Errorf("SetPrincipal(carol) with a display name of 128 characters = %+v, %v", p, err)
	}
}

// carolActor is carol, for the tests that need her to act.
var carolActor = Actor{Principal: "carol", Session: "sess-c", Machine: "laptop-c"}

// A principal is added with its kind, display name, owner and an
// invitation, all in one write or none of it.
func TestControlAddNewPrincipal(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustProject(t, c, projA)
	mustPrincipal(t, c, "alice", projA+"=admin")

	seq := controlSeq(t, c)
	p, err := c.AddNewPrincipal(ctx, dana, NewPrincipal{Name: "bob", Project: projA, DisplayName: "Bob Example",
		Invite: &Invitation{Provider: "google", Address: "bob@example.com"}})
	if err != nil || p.Kind != KindPerson || p.DisplayName != "Bob Example" || p.Owner != "" {
		t.Fatalf("AddNewPrincipal(bob) = %+v, %v", p, err)
	}
	ids, err := c.Identities(ctx, "bob")
	if err != nil || len(ids) != 1 || ids[0].InvitedAs != "bob@example.com" || ids[0].InvitedBy != "dana" {
		t.Errorf("Identities(bob) = %+v, %v; want the invitation", ids, err)
	}
	wantEvents(t, c, seq, ev{OpPrincipalAdd, "bob", dana, `"kind":"person"`}, ev{OpMemberSet, projA + "/bob", dana, ""},
		ev{OpIdentityInvite, "bob", dana, `"provider":"google"`})
	evs, err := c.Events(ctx, seq, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(evs[0].After), "Bob Example") {
		t.Errorf("principal.add after state %s holds the display name", evs[0].After)
	}

	// A project admin adds a service of their project, which answers to
	// them, and may not invite.
	svc, err := c.AddNewPrincipal(ctx, alice, NewPrincipal{Name: "ci-bot", Project: projA, Kind: KindService, Owner: "alice"})
	if err != nil || svc.Kind != KindService || svc.Owner != "alice" {
		t.Errorf("AddNewPrincipal(ci-bot) by alice = %+v, %v; want a service owned by alice", svc, err)
	}
	cases := []struct {
		name  string
		actor Actor
		in    NewPrincipal
		check func(error) bool
	}{
		{"an invitation, by a project admin", alice, NewPrincipal{Name: "carol", Project: projA,
			Invite: &Invitation{Provider: "google", Address: "carol@example.com"}}, access(AccessServerAdmin)},
		{"an invitation onto one's own name", sam, NewPrincipal{Name: "sam", Invite: &Invitation{Provider: "google", Address: "sam@example.com"}},
			access(AccessOwnIdentity)},
		{"a service with an invitation", dana, NewPrincipal{Name: "carol", Kind: KindService, Owner: "alice",
			Invite: &Invitation{Provider: "google", Address: "carol@example.com"}}, identityRule(IdentityRuleService)},
		{"a service with no owner", dana, NewPrincipal{Name: "carol", Kind: KindService}, identityRule(IdentityRuleOwner)},
		{"a service owned by a service", dana, NewPrincipal{Name: "carol", Kind: KindService, Owner: "ci-bot"}, identityRule(IdentityRuleOwner)},
		{"a service owned by itself", dana, NewPrincipal{Name: "carol", Kind: KindService, Owner: "carol"}, identityRule(IdentityRuleOwner)},
		{"a person with an owner", dana, NewPrincipal{Name: "carol", Owner: "alice"}, identityRule(IdentityRulePersonOwner)},
		{"an invitation of an address another has", dana, NewPrincipal{Name: "carol",
			Invite: &Invitation{Provider: "google", Address: "BOB@example.com"}}, is(ErrConflict)},
		{"a bad address", dana, NewPrincipal{Name: "carol", Invite: &Invitation{Provider: "google", Address: "carol"}}, is(ErrInvalid)},
		{"a bad display name", dana, NewPrincipal{Name: "carol", DisplayName: "a\tb"}, is(ErrInvalid)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seq := controlSeq(t, c)
			if _, err := c.AddNewPrincipal(ctx, tc.actor, tc.in); !tc.check(err) {
				t.Errorf("AddNewPrincipal(%+v) by %s = %v", tc.in, tc.actor.Principal, err)
			}
			wantEvents(t, c, seq)
			if _, err := c.Principal(ctx, tc.in.Name); !errors.Is(err, ErrNotFound) {
				t.Errorf("Principal(%s) after the refusal = %v, want ErrNotFound", tc.in.Name, err)
			}
		})
	}
}

// The status report's counts of identities come from memory, and follow
// the store's writes and its clock.
func TestControlIdentityCounts(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")
	if got := c.Counts().Identities; len(got) != 0 {
		t.Errorf("Counts().Identities with none = %v, want empty", got)
	}
	a := mustInvite(t, c, "alice", "google", "alice@example.com")
	clk.add(time.Hour)
	mustInvite(t, c, "bob", "google", "bob@example.com")
	b := mustInvite(t, c, "bob", "entra", "bob@example.com")
	if err := bind(t, c, b.ID, "https://login.example.com", "oid-1"); err != nil {
		t.Fatal(err)
	}
	// The binding was made behind the store's back, as another process
	// would make it.
	if err := c.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	clk.add(DefaultInviteTTL - time.Hour)
	want := map[IdentityState]int{IdentityBound: 1, IdentityPending: 1, IdentityLapsed: 1}
	if got := c.Counts().Identities; !maps.Equal(got, want) {
		t.Errorf("Counts().Identities = %v, want %v", got, want)
	}
	if _, err := c.RemoveInvite(ctx, dana, a.ID); err != nil {
		t.Fatal(err)
	}
	want = map[IdentityState]int{IdentityBound: 1, IdentityPending: 1}
	if got := c.Counts().Identities; !maps.Equal(got, want) {
		t.Errorf("Counts().Identities after removing the lapsed one = %v, want %v", got, want)
	}
}

// Invites pages the invitations by id, as an admin list pages its rows.
func TestControlInvitesPaging(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	var ids []int64
	for _, p := range []string{"google", "entra", "okta"} {
		ids = append(ids, mustInvite(t, c, "alice", p, "alice@example.com").ID)
	}
	cases := []struct {
		q    InviteQuery
		want []int64
		more int
	}{
		{InviteQuery{}, ids, 0},
		{InviteQuery{Limit: 2}, ids[:2], 1},
		{InviteQuery{After: ids[0]}, ids[1:], 0},
		{InviteQuery{After: ids[0], Limit: 1}, ids[1:2], 1},
		{InviteQuery{After: ids[2]}, nil, 0},
	}
	for _, tc := range cases {
		got, more, err := c.Invites(ctx, tc.q)
		var gotIDs []int64
		for _, i := range got {
			gotIDs = append(gotIDs, i.ID)
		}
		if err != nil || !slices.Equal(gotIDs, tc.want) || more != tc.more {
			t.Errorf("Invites(%+v) = %v, %d more, %v; want %v, %d more", tc.q, gotIDs, more, err, tc.want, tc.more)
		}
	}
}
