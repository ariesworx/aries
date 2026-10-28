package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/store"
)

// A server's login: provider, as an operator writes it. control invite
// reads its name alone, so it needs no secret.
const corpProvider = "login:\n  providers:\n    - name: corp\n      kind: oidc\n      issuer: https://sso.example.com/realms/example\n      client_id: aries\n"

// initSite adopts the site's project with dana, the config's only
// server admin, imported from authorized_keys.
func initSite(t *testing.T, s *site) {
	t.Helper()
	keys := writeKeys(t, forced(t, "dana", "dana-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
}

// danaAtCorp is dana's sign-in at corp, her address verified.
func danaAtCorp(address string) store.SignIn {
	return store.SignIn{Provider: "corp", Issuer: "https://sso.example.com/realms/example", Subject: "4f0d2c3e-dana", Address: address, Verified: true,
		ClientID: "aries-cli", Scopes: []string{"cli", "mcp"}, JKT: "thumbprint-of-danas-key-0000000000000000000",
		Resource: "https://aries.example.com", Flow: store.FlowLoopback, Machine: "laptop-d", Lifetime: 24 * time.Hour,
		TokenHash: strings.Repeat("ab", 32)}
}

// A server whose only server admin is dana cannot invite dana: nobody
// invites onto their own principal. The operator can, on the server, and
// dana's first sign-in binds it.
func TestControlInvite(t *testing.T) {
	t.Parallel()
	s := newSite(t, corpProvider)
	initSite(t, s)
	c := s.control(t)
	ctx := t.Context()
	dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
	if _, _, err := c.Invite(ctx, dana, "dana", store.Invitation{Provider: "corp", Address: "dana@example.com"}); err == nil {
		t.Fatal("dana invited herself; want the refusal that makes control invite needed")
	}
	before, err := c.Events(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	got := s.run(t, "", controlCmd, "invite", "--provider", "corp", "dana", "dana@example.com")
	if got.err != nil {
		t.Fatalf("control invite: %v\n%s", got.err, got.stderr)
	}
	if !strings.HasPrefix(got.stdout, "invitation ") || !strings.Contains(got.stdout, " for dana: dana@example.com at corp, open until ") ||
		!strings.Contains(got.stdout, "aries login") {
		t.Errorf("stdout = %q, want the invitation and the sign-in that binds it", got.stdout)
	}
	evs, err := c.Events(ctx, before[len(before)-1].Seq, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Op != store.OpIdentityInvite || evs[0].Target != "dana" || evs[0].Actor.Principal != store.OperatorPrincipal {
		t.Errorf("events = %+v, want one identity.invite of dana by %s", evs, store.OperatorPrincipal)
	}

	// The daemon's store, open all along, binds it at the first sign-in.
	g, err := c.SignIn(ctx, danaAtCorp("Dana@Example.com"))
	if err != nil {
		t.Fatalf("SignIn after control invite = %v", err)
	}
	if g.Principal != "dana" {
		t.Errorf("SignIn = %+v, want dana's", g)
	}

	// Again, for an address still open, it renews.
	got = s.run(t, "", controlCmd, "invite", "--provider", "corp", "dana", "dana+2@example.com")
	if got.err != nil {
		t.Fatalf("control invite (second address): %v", got.err)
	}
	got = s.run(t, "", controlCmd, "invite", "--provider", "corp", "dana", "dana+2@example.com")
	if got.err != nil || !strings.Contains(got.stdout, "renewed") {
		t.Errorf("control invite (again) = %q, %v; want it renewed", got.stdout, got.err)
	}
}

// Each refusal of control invite says what to do next, and prints nothing.
func TestControlInviteRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		extra string
		setup func(t *testing.T, s *site)
		args  []string
		usage bool
		want  []string
	}{
		{name: "no provider", extra: corpProvider, args: []string{"invite", "dana", "dana@example.com"}, usage: true,
			want: []string{"needs --provider NAME"}},
		{name: "one argument", extra: corpProvider, args: []string{"invite", "--provider", "corp", "dana"}, usage: true,
			want: []string{"takes NAME and ADDRESS"}},
		{name: "flags after the arguments", extra: corpProvider, args: []string{"invite", "dana", "dana@example.com", "--provider", "corp"}, usage: true,
			want: []string{"flags come before NAME and ADDRESS"}},
		{name: "no providers", setup: initSite, args: []string{"invite", "--provider", "corp", "dana", "dana@example.com"},
			want: []string{"this server signs no one in", "fix: add the provider under login: providers"}},
		{name: "an unknown provider", extra: corpProvider, setup: initSite, args: []string{"invite", "--provider", "google", "dana", "dana@example.com"},
			want: []string{"signs no one in at google", "fix: give --provider one of corp"}},
		{name: "no control_schema", extra: corpProvider, args: []string{"invite", "--provider", "corp", "dana", "dana@example.com"},
			setup: func(t *testing.T, s *site) { rewrite(t, s, "project: "+adopted+"\n"+corpProvider) },
			want:  []string{"no control store; fix: set control_schema:"}},
		{name: "control store never made", extra: corpProvider, args: []string{"invite", "--provider", "corp", "dana", "dana@example.com"},
			want: []string{"open the control store", "ariesd control init"}},
		{name: "an unknown principal", extra: corpProvider, setup: initSite, args: []string{"invite", "--provider", "corp", "erin", "erin@example.com"},
			want: []string{"no principal erin", "fix: add erin first"}},
		{name: "a service", extra: corpProvider, args: []string{"invite", "--provider", "corp", "ci", "ci@example.com"},
			setup: func(t *testing.T, s *site) {
				initSite(t, s)
				dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
				if _, err := s.control(t).AddNewPrincipal(t.Context(), dana, store.NewPrincipal{Name: "ci", Kind: store.KindService, Owner: "dana"}); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"ci", "fix: a service signs in by a key or a trust rule"}},
		{name: "not an address", extra: corpProvider, setup: initSite, args: []string{"invite", "--provider", "corp", "dana", "dana at example"},
			want: []string{"fix: give the address as the provider names it"}},
		{name: "an address invited for another", extra: corpProvider, args: []string{"invite", "--provider", "corp", "dana", "bob@example.com"},
			setup: func(t *testing.T, s *site) {
				initSite(t, s)
				c := s.control(t)
				dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
				if _, err := c.AddPrincipal(t.Context(), dana, "bob", adopted); err != nil {
					t.Fatal(err)
				}
				if _, _, err := c.Invite(t.Context(), dana, "bob", store.Invitation{Provider: "corp", Address: "bob@example.com"}); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"already", "fix: remove invitation"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSite(t, tc.extra)
			if tc.setup != nil {
				tc.setup(t, s)
			}
			got := s.run(t, "", controlCmd, tc.args...)
			if got.err == nil {
				t.Fatalf("control %v succeeded: %q", tc.args, got.stdout)
			}
			for _, want := range tc.want {
				if !strings.Contains(got.err.Error(), want) {
					t.Errorf("control %v = %v\nwant it to contain %q", tc.args, got.err, want)
				}
			}
			if _, usage := errors.AsType[usageError](got.err); usage != tc.usage {
				t.Errorf("control %v = %v; usage error %v, want %v", tc.args, got.err, usage, tc.usage)
			}
			if !tc.usage && strings.Count(got.err.Error(), "fix: ") != 1 {
				t.Errorf("refusal %q has %d fixes, want one", got.err, strings.Count(got.err.Error(), "fix: "))
			}
			if got.stdout != "" {
				t.Errorf("a refused run printed %q", got.stdout)
			}
		})
	}
}
