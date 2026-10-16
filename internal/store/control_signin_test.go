package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	corpIssuer = "https://sso.example.com/realms/example"
	aliceSub   = "00000000-0000-4000-8000-00000000a11c"
)

// tokenHash is the hash the store keeps of a token, as the daemon makes
// it.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// aliceSignIn is a sign-in of alice at corp, her address verified, for
// a day, with token tok.
func aliceSignIn(tok string) SignIn {
	return SignIn{Provider: "corp", Issuer: corpIssuer, Subject: aliceSub, Address: "Alice@Example.com", Verified: true,
		ClientID: "aries-cli", Scopes: []string{"cli", "mcp"}, JKT: "thumbprint-of-alices-key-000000000000000000",
		Resource: "https://aries.example.com", Flow: FlowLoopback, Machine: "laptop-a", Lifetime: 24 * time.Hour, TokenHash: tokenHash(tok)}
}

// eventsSince returns the control store's events after seq.
func eventsSince(t *testing.T, c *Control, seq int64) []Event {
	t.Helper()
	evs, err := c.Events(t.Context(), seq, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// TestSignInBinds: the first sign-in whose verified address is an open
// invitation's binds it, in the same write as the grant; from then on
// (issuer, subject) alone is matched, whatever address the token says.
func TestSignInBinds(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	inv := mustInvite(t, c, "alice", "corp", "alice@example.com")
	seq := controlSeq(t, c)

	g, err := c.SignIn(ctx, aliceSignIn("tok-1"))
	if err != nil {
		t.Fatalf("SignIn(alice, invited) = %v", err)
	}
	if g.Principal != "alice" || g.Kind != KindPerson || g.Identity != inv.ID || g.Provider != "corp" || !g.EndsAt.Equal(identityAt.Add(24*time.Hour)) ||
		strings.Join(g.Scopes, " ") != "cli mcp" || g.Machine != "laptop-a" || g.Flow != FlowLoopback || g.ID == "" {
		t.Errorf("SignIn = %+v, want alice's grant for a day by identity %d", g, inv.ID)
	}
	id, err := c.Identity(ctx, inv.ID)
	if err != nil || !id.Bound() || id.Issuer != corpIssuer || id.Subject != aliceSub || !id.BoundAt.Equal(identityAt) ||
		!id.LastSignIn.Equal(identityAt) || id.Rev != inv.Rev+1 {
		t.Errorf("Identity(%d) after the sign-in = %+v, %v; want it bound to the token's issuer and subject", inv.ID, id, err)
	}
	evs := eventsSince(t, c, seq)
	if len(evs) != 2 || evs[0].Op != OpIdentityBind || evs[1].Op != OpSignInGrant || evs[0].Actor.Principal != "alice" || evs[1].Target != "alice" {
		t.Fatalf("events of the sign-in = %+v, want identity.bind and signin.grant by alice", evs)
	}
	for _, e := range evs {
		for _, secret := range []string{"alice@example.com", "Alice@Example.com", aliceSub, tokenHash("tok-1"), "thumbprint-of-alices-key"} {
			if strings.Contains(string(e.Before)+string(e.After), secret) {
				t.Errorf("event %s holds %q; an event holds no address, subject, token or key", e.Op, secret)
			}
		}
	}
	if got, ok := c.TokenGrant(tokenHash("tok-1")); !ok || got.ID != g.ID || got.JKT != g.JKT {
		t.Errorf("TokenGrant(tok-1) = %+v, %t; want the grant", got, ok)
	}
	if _, ok := c.TokenGrant(tokenHash("another")); ok {
		t.Error("TokenGrant(a token never issued) found a grant")
	}

	// A day later, the address changed at the provider: the subject
	// still names alice, and nothing is bound again.
	clk.add(25 * time.Hour)
	seq = controlSeq(t, c)
	in := aliceSignIn("tok-2")
	in.Address = "a.example@example.com"
	g2, err := c.SignIn(ctx, in)
	if err != nil || g2.Principal != "alice" || g2.Identity != inv.ID {
		t.Fatalf("SignIn(alice's subject, another address) = %+v, %v; want alice by her bound identity", g2, err)
	}
	if evs := eventsSince(t, c, seq); len(evs) != 1 || evs[0].Op != OpSignInGrant {
		t.Errorf("events of a second sign-in = %+v, want signin.grant alone", evs)
	}
	if id, _ := c.Identity(ctx, inv.ID); !id.LastSignIn.Equal(identityAt.Add(25*time.Hour)) || id.Rev != inv.Rev+1 {
		t.Errorf("Identity after a second sign-in = %+v; want last_sign_in moved, as bookkeeping, and the rev unchanged", id)
	}
}

func TestSignInRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, c *Control, clk *clock)
		in     func(SignIn) SignIn
		reason SignInReason
	}{
		{"nobody invited", func(*testing.T, *Control, *clock) {}, nil, SignInUninvited},
		{"invited at another provider", func(t *testing.T, c *Control, _ *clock) {
			mustInvite(t, c, "alice", "google", "alice@example.com")
		}, nil, SignInUninvited},
		{"an unverified address", func(t *testing.T, c *Control, _ *clock) {
			mustInvite(t, c, "alice", "corp", "alice@example.com")
		}, func(s SignIn) SignIn { s.Verified = false; return s }, SignInUnverified},
		{"a lapsed invitation", func(t *testing.T, c *Control, clk *clock) {
			mustInvite(t, c, "alice", "corp", "alice@example.com")
			clk.add(DefaultInviteTTL + time.Second)
		}, nil, SignInLapsed},
		{"the same address, another subject", func(t *testing.T, c *Control, _ *clock) {
			inv := mustInvite(t, c, "alice", "corp", "alice@example.com")
			if err := bind(t, c, inv.ID, corpIssuer, "another-subject"); err != nil {
				t.Fatal(err)
			}
		}, nil, SignInRebound},
		{"a disabled principal", func(t *testing.T, c *Control, _ *clock) {
			mustInvite(t, c, "alice", "corp", "alice@example.com")
			if _, err := c.DisablePrincipal(t.Context(), dana, "alice"); err != nil {
				t.Fatal(err)
			}
		}, nil, SignInDisabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, clk := clockControl(t, ControlOptions{})
			mustPrincipal(t, c, "alice")
			tc.setup(t, c, clk)
			seq := controlSeq(t, c)
			in := aliceSignIn("tok")
			if tc.in != nil {
				in = tc.in(in)
			}
			_, err := c.SignIn(t.Context(), in)
			se, ok := errors.AsType[*SignInError](err)
			if !ok || se.Reason != tc.reason || !errors.Is(err, ErrForbidden) {
				t.Fatalf("SignIn(%s) = %v, want a *SignInError of reason %s wrapping ErrForbidden", tc.name, err, tc.reason)
			}
			if se.Provider != "corp" || se.Address != in.Address {
				t.Errorf("SignIn(%s)'s refusal names provider %q and address %q, want what the token gave", tc.name, se.Provider, se.Address)
			}
			if evs := eventsSince(t, c, seq); len(evs) != 0 {
				t.Errorf("a refused sign-in recorded %d events, want none", len(evs))
			}
			if _, ok := c.TokenGrant(in.TokenHash); ok {
				t.Error("a refused sign-in's token found a grant")
			}
		})
	}
}

// TestSignInUnverifiedRevealsNothing: a sign-in whose address the
// provider does not say is verified, and whose subject is bound to no
// identity, is refused alike whatever the address's state, so that
// someone who sets another's address on an account of theirs learns
// nothing of it.
func TestSignInUnverifiedRevealsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, c *Control, clk *clock)
	}{
		{"nobody invited", func(*testing.T, *Control, *clock) {}},
		{"an open invitation", func(t *testing.T, c *Control, _ *clock) {
			mustInvite(t, c, "alice", "corp", "alice@example.com")
		}},
		{"a lapsed invitation", func(t *testing.T, c *Control, clk *clock) {
			mustInvite(t, c, "alice", "corp", "alice@example.com")
			clk.add(DefaultInviteTTL + time.Second)
		}},
		{"an address bound to another subject", func(t *testing.T, c *Control, _ *clock) {
			inv := mustInvite(t, c, "alice", "corp", "alice@example.com")
			if err := bind(t, c, inv.ID, corpIssuer, "another-subject"); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, clk := clockControl(t, ControlOptions{})
			mustPrincipal(t, c, "alice")
			tc.setup(t, c, clk)
			in := aliceSignIn("tok")
			in.Verified = false
			_, err := c.SignIn(t.Context(), in)
			se, ok := errors.AsType[*SignInError](err)
			if !ok || se.Reason != SignInUnverified || !se.Until.IsZero() {
				t.Fatalf("SignIn(unverified, %s) = %#v, want a *SignInError of reason %s with no time", tc.name, err, SignInUnverified)
			}
			want := (&SignInError{Reason: SignInUnverified, Provider: in.Provider, Address: in.Address}).Error()
			if err.Error() != want {
				t.Errorf("SignIn(unverified, %s) = %q, want %q", tc.name, err, want)
			}
		})
	}
}

func TestSignInChecks(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	mustPrincipal(t, c, "alice")
	mustInvite(t, c, "alice", "corp", "alice@example.com")
	for _, tc := range []struct {
		name string
		f    func(*SignIn)
	}{
		{"no provider", func(s *SignIn) { s.Provider = "" }},
		{"no subject", func(s *SignIn) { s.Subject = "" }},
		{"no token", func(s *SignIn) { s.TokenHash = "" }},
		{"a token hash that is not one", func(s *SignIn) { s.TokenHash = "abc" }},
		{"no lifetime", func(s *SignIn) { s.Lifetime = 0 }},
		{"another flow", func(s *SignIn) { s.Flow = "carrier-pigeon" }},
		{"a machine with a control character", func(s *SignIn) { s.Machine = "lap\ntop" }},
		{"no scope", func(s *SignIn) { s.Scopes = nil }},
		{"an unknown scope", func(s *SignIn) { s.Scopes = []string{"root"} }},
	} {
		in := aliceSignIn("tok")
		tc.f(&in)
		if _, err := c.SignIn(t.Context(), in); !errors.Is(err, ErrInvalid) {
			t.Errorf("SignIn(%s) = %v, want ErrInvalid", tc.name, err)
		}
	}
}

func TestSignInCap(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{Limits: ControlLimits{Grants: 2}})
	mustPrincipal(t, c, "alice")
	mustInvite(t, c, "alice", "corp", "alice@example.com")
	for _, tok := range []string{"tok-1", "tok-2"} {
		if _, err := c.SignIn(t.Context(), aliceSignIn(tok)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.SignIn(t.Context(), aliceSignIn("tok-3"))
	if ce, ok := errors.AsType[*ControlLimitError](err); !ok || ce.Limit != limitGrants || ce.Max != 2 {
		t.Fatalf("SignIn(a third live sign-in, at most 2) = %v, want the grants_per_principal limit", err)
	}
	// Sign-ins that have ended do not count.
	clk.add(24 * time.Hour)
	if _, err := c.SignIn(t.Context(), aliceSignIn("tok-3")); err != nil {
		t.Errorf("SignIn once the others ended = %v", err)
	}
}

// TestEndSignIn: a sign-in's holder ends it (logout), a server admin
// ends anyone's, and nobody else ends one; its token finds nothing at
// once.
func TestEndSignIn(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustPrincipal(t, c, "bob")
	mustInvite(t, c, "alice", "corp", "alice@example.com")
	g, err := c.SignIn(ctx, aliceSignIn("tok-1"))
	if err != nil {
		t.Fatal(err)
	}
	bob := Actor{Principal: "bob", Session: "s", Machine: "m"}
	if _, _, err := c.EndSignIn(ctx, bob, g.ID, EndRevoked); !errors.Is(err, ErrForbidden) {
		t.Errorf("EndSignIn(alice's, by bob) = %v, want ErrForbidden", err)
	}
	alice := Actor{Principal: "alice", Session: "signin", Machine: "laptop-a"}
	seq := controlSeq(t, c)
	ended, changed, err := c.EndSignIn(ctx, alice, g.ID, EndLogout)
	if err != nil || !changed || ended.EndReason != EndLogout || !ended.RevokedAt.Equal(identityAt) {
		t.Fatalf("EndSignIn(her own, logout) = %+v, %t, %v; want it ended now", ended, changed, err)
	}
	if _, ok := c.TokenGrant(tokenHash("tok-1")); ok {
		t.Error("TokenGrant(an ended sign-in's token) found it")
	}
	if evs := eventsSince(t, c, seq); len(evs) != 1 || evs[0].Op != OpSignInEnd || evs[0].Target != "alice" {
		t.Errorf("events of the end = %+v, want signin.end on alice", evs)
	}
	if _, changed, err := c.EndSignIn(ctx, dana, g.ID, EndRevoked); err != nil || changed {
		t.Errorf("EndSignIn(an ended sign-in) = %t, %v; want nothing changed", changed, err)
	}
	if _, _, err := c.EndSignIn(ctx, dana, "g-nonesuch", EndRevoked); !errors.Is(err, ErrNotFound) {
		t.Errorf("EndSignIn(no such sign-in) = %v, want ErrNotFound", err)
	}
	g2, err := c.SignIn(ctx, aliceSignIn("tok-2"))
	if err != nil {
		t.Fatal(err)
	}
	if ended, _, err := c.EndSignIn(ctx, dana, g2.ID, EndRevoked); err != nil || ended.EndReason != EndRevoked {
		t.Errorf("EndSignIn(alice's, by a server admin) = %+v, %v", ended, err)
	}
	live, err := c.Grants(ctx, "alice")
	if err != nil || len(live) != 2 || live[0].EndReason == "" {
		t.Errorf("Grants(alice) = %+v, %v; want both, ended", live, err)
	}
}

// TestRemoveIdentityEndsSignIns: removing an identity ends what its
// sign-ins made (identity.md §6.5).
func TestRemoveIdentityEndsSignIns(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	inv := mustInvite(t, c, "alice", "corp", "alice@example.com")
	g, err := c.SignIn(ctx, aliceSignIn("tok-1"))
	if err != nil {
		t.Fatal(err)
	}
	seq := controlSeq(t, c)
	if _, err := c.RemoveIdentity(ctx, dana, inv.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.TokenGrant(tokenHash("tok-1")); ok {
		t.Error("TokenGrant(a sign-in by a removed identity) found it")
	}
	gs, err := c.Grants(ctx, "alice")
	if err != nil || len(gs) != 1 || gs[0].ID != g.ID || gs[0].EndReason != EndIdentityRemoved || gs[0].Identity != 0 {
		t.Errorf("Grants(alice) after the identity's removal = %+v, %v; want the sign-in ended for it", gs, err)
	}
	var ops []Op
	for _, e := range eventsSince(t, c, seq) {
		ops = append(ops, e.Op)
	}
	if strings.Join([]string{string(OpSignInEnd), string(OpIdentityRemove)}, " ") != joinOps(ops) {
		t.Errorf("events of the removal = %v, want signin.end then identity.remove", ops)
	}
}

// TestDisablePrincipalEndsSignIns: disabling a principal ends its live
// sign-ins in the same write, so that enabling it again does not bring
// back a token from before.
func TestDisablePrincipalEndsSignIns(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustInvite(t, c, "alice", "corp", "alice@example.com")
	g, err := c.SignIn(ctx, aliceSignIn("tok-1"))
	if err != nil {
		t.Fatal(err)
	}
	seq := controlSeq(t, c)
	if _, err := c.DisablePrincipal(ctx, dana, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EnablePrincipal(ctx, dana, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.TokenGrant(tokenHash("tok-1")); ok {
		t.Error("TokenGrant(a sign-in from before its principal was disabled) found it after the principal was enabled again")
	}
	gs, err := c.Grants(ctx, "alice")
	if err != nil || len(gs) != 1 || gs[0].ID != g.ID || gs[0].EndReason != EndPrincipalDisabled {
		t.Errorf("Grants(alice) after disable and enable = %+v, %v; want the sign-in ended, reason %s", gs, err, EndPrincipalDisabled)
	}
	var ops []Op
	for _, e := range eventsSince(t, c, seq) {
		ops = append(ops, e.Op)
	}
	if want := strings.Join([]string{string(OpSignInEnd), string(OpPrincipalDisable), string(OpPrincipalEnable)}, " "); joinOps(ops) != want {
		t.Errorf("events of disable and enable = %v, want %s", ops, want)
	}
}

func joinOps(ops []Op) string {
	s := make([]string, len(ops))
	for i, o := range ops {
		s[i] = string(o)
	}
	return strings.Join(s, " ")
}

// TestPruneSignIns: sign-ins that ended before a time are deleted,
// with their tokens, as bookkeeping that records no event.
func TestPruneSignIns(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustPrincipal(t, c, "alice")
	mustInvite(t, c, "alice", "corp", "alice@example.com")
	old, err := c.SignIn(ctx, aliceSignIn("tok-1"))
	if err != nil {
		t.Fatal(err)
	}
	clk.add(23 * time.Hour)
	live, err := c.SignIn(ctx, aliceSignIn("tok-2"))
	if err != nil {
		t.Fatal(err)
	}
	clk.add(2 * time.Hour) // the first ended an hour ago
	seq := controlSeq(t, c)
	n, err := c.PruneSignIns(ctx, clk.now().Add(-30*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("PruneSignIns(ended half an hour ago) = %d, %v; want the first", n, err)
	}
	gs, err := c.Grants(ctx, "alice")
	if err != nil || len(gs) != 1 || gs[0].ID != live.ID {
		t.Errorf("Grants after pruning = %+v, %v; want the live one alone, not %s", gs, err, old.ID)
	}
	if evs := eventsSince(t, c, seq); len(evs) != 0 {
		t.Errorf("pruning recorded %d events, want none", len(evs))
	}
}

// TestSignInIndexReload: a sign-in is found by its token after the
// store is opened again, as after a restart of the daemon.
func TestSignInIndexReload(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	clk := &clock{t: identityAt}
	c := openControl(t, dsn, ControlOptions{Now: clk.now})
	mustPrincipal(t, c, "alice")
	mustInvite(t, c, "alice", "corp", "alice@example.com")
	g, err := c.SignIn(t.Context(), aliceSignIn("tok-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, ok := c.TokenGrant(tokenHash("tok-1")); !ok || got.ID != g.ID || !got.EndsAt.Equal(g.EndsAt) || strings.Join(got.Scopes, " ") != "cli mcp" {
		t.Errorf("TokenGrant after a reload = %+v, %t; want %+v", got, ok, g)
	}
	if n := c.Counts().SignIns; n != 1 {
		t.Errorf("Counts().SignIns = %d, want 1", n)
	}
}

// TestSignInSchema: the tables' own rules hold, whatever the store
// checks first.
func TestSignInSchema(t *testing.T) {
	t.Parallel()
	c, _ := clockControl(t, ControlOptions{})
	mustPrincipal(t, c, "alice")
	mustInvite(t, c, "alice", "corp", "alice@example.com")
	g, err := c.SignIn(t.Context(), aliceSignIn("tok-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO tokens (hash, grant_id, expires_at) VALUES ('not-a-hash', '` + g.ID + `', NOW())`,
		`INSERT INTO tokens (hash, grant_id, expires_at) VALUES ('` + tokenHash("x") + `', 'g-nonesuch', NOW())`,
		`INSERT INTO tokens (hash, grant_id, kind, expires_at) VALUES ('` + tokenHash("y") + `', '` + g.ID + `', 'id', NOW())`,
		`UPDATE grants SET revoked_at = NOW() WHERE id = '` + g.ID + `'`,
		`UPDATE grants SET flow = 'fax' WHERE id = '` + g.ID + `'`,
		`UPDATE grants SET ends_at = created_at WHERE id = '` + g.ID + `'`,
		`UPDATE grants SET principal = 'nobody' WHERE id = '` + g.ID + `'`,
	} {
		if _, err := c.s.w.ExecContext(t.Context(), stmt); err == nil {
			t.Errorf("%s: no error, want the schema to refuse it", stmt)
		}
	}
}
