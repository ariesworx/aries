package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The trust tests' platform: GitHub Actions' ids for one repository.
var ghaClaims = map[string]string{"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/main"}

// mustService adds service name, owned by alice, a person, as dana.
func mustService(t *testing.T, c *Control, name string) {
	t.Helper()
	if _, err := c.Principal(t.Context(), "alice"); errors.Is(err, ErrNotFound) {
		mustPrincipal(t, c, "alice")
	}
	if _, err := c.AddNewPrincipal(t.Context(), dana, NewPrincipal{Name: name, Kind: KindService, Owner: "alice"}); err != nil {
		t.Fatalf("add service %s: %v", name, err)
	}
}

// ghaRule is a rule of ci-example at github-actions with claims, for an
// hour.
func ghaRule(name, principal string, claims map[string]string) TrustRule {
	return TrustRule{Name: name, Principal: principal, Provider: "github-actions", Claims: claims, TokenLifetime: time.Hour}
}

func mustTrust(t *testing.T, c *Control, r TrustRule) TrustRule {
	t.Helper()
	out, err := c.AddTrustRule(t.Context(), dana, r)
	if err != nil {
		t.Fatalf("AddTrustRule(%s): %v", r.Name, err)
	}
	return out
}

// workloadIn is a verified token of github-actions with claims, for the
// job's token tok.
func workloadIn(tok string, claims map[string]string) WorkloadSignIn {
	return WorkloadSignIn{Provider: "github-actions", Claims: claims, Launch: map[string]string{"run_id": "8123", "event_name": "push"},
		ClientID: "aries-cli", Scopes: []string{"ci"}, JKT: "thumbprint-of-the-jobs-key-0000000000000000", Resource: "https://aries.example.com",
		Machine: "runner-1", MaxLifetime: 6 * time.Hour, TokenHash: tokenHash(tok)}
}

// with is claims and more.
func with(claims map[string]string, more ...string) map[string]string {
	out := map[string]string{}
	for k, v := range claims {
		out[k] = v
	}
	for i := 0; i+1 < len(more); i += 2 {
		out[more[i]] = more[i+1]
	}
	return out
}

func TestTrustRules(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustService(t, c, "ci-example")
	seq := controlSeq(t, c)

	r := mustTrust(t, c, ghaRule("ci-example-main", "ci-example", ghaClaims))
	if r.State != TrustActive || r.CreatedBy != "dana" || r.Rev != 1 || !r.CreatedAt.Equal(identityAt) || r.TokenLifetime != time.Hour {
		t.Errorf("AddTrustRule = %+v, want it active, by dana, rev 1", r)
	}
	evs := eventsSince(t, c, seq)
	if len(evs) != 1 || evs[0].Op != OpTrustAdd || evs[0].Target != "ci-example-main" || evs[0].Actor.Principal != "dana" ||
		!strings.Contains(string(evs[0].After), `"repository_id":"123456789"`) || !strings.Contains(string(evs[0].After), `"principal":"ci-example"`) {
		t.Errorf("events of trust.add = %+v, want one naming the rule, its principal and claims, by dana", evs)
	}
	if _, err := c.AddTrustRule(ctx, dana, ghaRule("ci-example-main", "ci-example", ghaClaims)); !errors.Is(err, ErrExists) {
		t.Errorf("AddTrustRule(a name in use) = %v, want ErrExists", err)
	}
	mustTrust(t, c, ghaRule("ci-example-release", "ci-example", with(ghaClaims, "environment", "release")))
	if rs, err := c.TrustRules(ctx, "ci-example"); err != nil || len(rs) != 2 || rs[0].Name != "ci-example-main" || rs[1].Name != "ci-example-release" {
		t.Errorf("TrustRules(ci-example) = %+v, %v; want both, by name", rs, err)
	}
	if rs, err := c.TrustRules(ctx, ""); err != nil || len(rs) != 2 {
		t.Errorf("TrustRules(all) = %+v, %v; want both", rs, err)
	}
	if got, err := c.TrustRule(ctx, "ci-example-main"); err != nil || got.Claims["ref"] != "refs/heads/main" {
		t.Errorf("TrustRule = %+v, %v", got, err)
	}
	if _, err := c.TrustRule(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TrustRule(nope) = %v, want ErrNotFound", err)
	}

	// A lifetime alone changes nothing a job holds.
	g, _, err := c.WorkloadSignIn(ctx, workloadIn("job-1", ghaClaims))
	if err != nil {
		t.Fatalf("WorkloadSignIn = %v", err)
	}
	clk.add(time.Minute)
	set, ended, changed, err := c.SetTrustRule(ctx, dana, "ci-example-main", TrustChange{TokenLifetime: 30 * time.Minute})
	if err != nil || !changed || len(ended) != 0 || set.TokenLifetime != 30*time.Minute || set.Rev != 2 {
		t.Errorf("SetTrustRule(lifetime) = %+v, %v, %t, %v; want it changed, rev 2, no sign-in ended", set, ended, changed, err)
	}
	if _, _, changed, err := c.SetTrustRule(ctx, dana, "ci-example-main", TrustChange{TokenLifetime: 30 * time.Minute}); err != nil || changed {
		t.Errorf("SetTrustRule(as it is) = %t, %v; want nothing changed", changed, err)
	}
	if _, ok := c.TokenGrant(tokenHash("job-1")); !ok {
		t.Error("a lifetime change ended the rule's sign-in")
	}
	// New claims end its sign-ins: the job may not match them.
	seq = controlSeq(t, c)
	set, ended, changed, err = c.SetTrustRule(ctx, dana, "ci-example-main", TrustChange{Claims: with(ghaClaims, "ref", "refs/heads/trunk")})
	if err != nil || !changed || len(ended) != 1 || ended[0].ID != g.ID || ended[0].EndReason != EndTrustRuleChanged || set.Claims["ref"] != "refs/heads/trunk" {
		t.Fatalf("SetTrustRule(claims) = %+v, %+v, %t, %v; want sign-in %s ended", set, ended, changed, err, g.ID)
	}
	if _, ok := c.TokenGrant(tokenHash("job-1")); ok {
		t.Error("the sign-in of a rule whose claims changed is still live")
	}
	if evs := eventsSince(t, c, seq); len(evs) != 2 || evs[0].Op != OpTrustSet || evs[1].Op != OpSignInEnd {
		t.Errorf("events of trust.set = %+v, want trust.set and signin.end", evs)
	}
	// Disabled, it matches nothing; enabled, it does again.
	if _, _, _, err := c.SetTrustRule(ctx, dana, "ci-example-main", TrustChange{State: TrustDisabled}); err != nil {
		t.Fatal(err)
	}
	trunk := with(ghaClaims, "ref", "refs/heads/trunk")
	if _, _, err := c.WorkloadSignIn(ctx, workloadIn("job-2", trunk)); !trustReason(TrustNoMatch)(err) {
		t.Errorf("WorkloadSignIn(a disabled rule's job) = %v, want no match", err)
	}
	if _, _, _, err := c.SetTrustRule(ctx, dana, "ci-example-main", TrustChange{State: TrustActive}); err != nil {
		t.Fatal(err)
	}
	g, _, err = c.WorkloadSignIn(ctx, workloadIn("job-3", trunk))
	if err != nil {
		t.Fatalf("WorkloadSignIn(enabled again) = %v", err)
	}

	// Removing it ends its sign-ins, which forget it.
	seq = controlSeq(t, c)
	gone, ended, err := c.RemoveTrustRule(ctx, dana, "ci-example-main")
	if err != nil || gone.Name != "ci-example-main" || len(ended) != 1 || ended[0].ID != g.ID || ended[0].EndReason != EndTrustRuleRemoved {
		t.Fatalf("RemoveTrustRule = %+v, %+v, %v; want sign-in %s ended", gone, ended, err, g.ID)
	}
	if evs := eventsSince(t, c, seq); len(evs) != 2 || evs[0].Op != OpSignInEnd || evs[1].Op != OpTrustRemove || evs[1].After != nil {
		t.Errorf("events of trust.remove = %+v, want signin.end and trust.remove", evs)
	}
	if got, err := c.Grant(ctx, g.ID); err != nil || got.TrustRule != "" || got.EndReason != EndTrustRuleRemoved {
		t.Errorf("Grant(%s) after its rule went = %+v, %v; want it ended, the rule forgotten", g.ID, got, err)
	}
	if _, _, err := c.RemoveTrustRule(ctx, dana, "ci-example-main"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveTrustRule(again) = %v, want ErrNotFound", err)
	}

	// A service with rules does not become a person.
	if _, _, err := c.SetPrincipal(ctx, dana, "ci-example", PrincipalChange{Kind: KindPerson}); !trustReason(TrustRules)(err) {
		t.Errorf("SetPrincipal(ci-example, person) with a rule = %v, want a refusal naming its trust rules", err)
	}
}

// trustReason matches a *TrustError of reason.
func trustReason(reason TrustReason) func(error) bool {
	return func(err error) bool {
		te, ok := errors.AsType[*TrustError](err)
		return ok && te.Reason == reason
	}
}

func TestTrustRuleRefusals(t *testing.T) {
	t.Parallel()
	c := openControl(t, newDSN(t), ControlOptions{})
	ctx := t.Context()
	mustService(t, c, "ci-example")
	mustPrincipal(t, c, "erin")
	ciActor := Actor{Principal: "ci-example", Session: "ci", Machine: "runner"}
	accessReason := func(reason AccessReason) func(error) bool {
		return func(err error) bool {
			ae, ok := errors.AsType[*AccessError](err)
			return ok && ae.Reason == reason
		}
	}
	invalid := func(err error) bool { return errors.Is(err, ErrInvalid) }
	for _, tc := range []struct {
		name  string
		actor Actor
		rule  func(TrustRule) TrustRule
		want  func(error) bool
	}{
		{"not a server admin", erin, nil, accessReason(AccessServerAdmin)},
		{"its own principal", ciActor, nil, accessReason(AccessOwnTrust)},
		{"a person", dana, func(r TrustRule) TrustRule { r.Principal = "erin"; return r }, trustReason(TrustNotService)},
		{"nobody", dana, func(r TrustRule) TrustRule { r.Principal = "nobody"; return r }, func(err error) bool { return errors.Is(err, ErrNotFound) }},
		{"no claims", dana, func(r TrustRule) TrustRule { r.Claims = nil; return r }, invalid},
		{"only sub", dana, func(r TrustRule) TrustRule {
			r.Claims = map[string]string{"sub": "repo:example/app:ref:refs/heads/main"}
			return r
		}, invalid},
		{"a pattern", dana, func(r TrustRule) TrustRule { r.Claims = with(ghaClaims, "ref", "refs/heads/*"); return r }, invalid},
		{"an empty value", dana, func(r TrustRule) TrustRule { r.Claims = with(ghaClaims, "ref", ""); return r }, invalid},
		{"a value of two lines", dana, func(r TrustRule) TrustRule { r.Claims = with(ghaClaims, "ref", "a\nb"); return r }, invalid},
		{"a claim's name", dana, func(r TrustRule) TrustRule { r.Claims = with(ghaClaims, "re f", "x"); return r }, invalid},
		{"seventeen claims", dana, func(r TrustRule) TrustRule {
			r.Claims = map[string]string{}
			for i := range MaxTrustClaims + 1 {
				r.Claims[string(rune('a'+i))] = "x"
			}
			return r
		}, invalid},
		{"a rule's name", dana, func(r TrustRule) TrustRule { r.Name = "Main"; return r }, invalid},
		{"a provider's name", dana, func(r TrustRule) TrustRule { r.Provider = "GitHub"; return r }, invalid},
		{"a lifetime too short", dana, func(r TrustRule) TrustRule { r.TokenLifetime = time.Second; return r }, invalid},
		{"a lifetime too long", dana, func(r TrustRule) TrustRule { r.TokenLifetime = 25 * time.Hour; return r }, invalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ghaRule("ci-example-main", "ci-example", ghaClaims)
			if tc.rule != nil {
				r = tc.rule(r)
			}
			if _, err := c.AddTrustRule(ctx, tc.actor, r); !tc.want(err) {
				t.Errorf("AddTrustRule(%s) = %v", tc.name, err)
			}
		})
	}
	mustTrust(t, c, ghaRule("ci-example-main", "ci-example", ghaClaims))
	for _, tc := range []struct {
		name  string
		actor Actor
		rule  string
		want  func(error) bool
	}{
		{"not a server admin", erin, "ci-example-main", accessReason(AccessServerAdmin)},
		{"a rule that is not, not a server admin", erin, "nope", accessReason(AccessServerAdmin)},
		{"its own principal", ciActor, "ci-example-main", accessReason(AccessOwnTrust)},
		{"a rule that is not", dana, "nope", func(err error) bool { return errors.Is(err, ErrNotFound) }},
	} {
		if _, _, _, err := c.SetTrustRule(ctx, tc.actor, tc.rule, TrustChange{State: TrustDisabled}); !tc.want(err) {
			t.Errorf("SetTrustRule(%s) = %v", tc.name, err)
		}
		if _, _, err := c.RemoveTrustRule(ctx, tc.actor, tc.rule); !tc.want(err) {
			t.Errorf("RemoveTrustRule(%s) = %v", tc.name, err)
		}
	}
	if _, _, _, err := c.SetTrustRule(ctx, dana, "ci-example-main", TrustChange{Claims: map[string]string{"sub": "x"}}); !invalid(err) {
		t.Errorf("SetTrustRule(only sub) = %v, want ErrInvalid", err)
	}
	if _, _, _, err := c.SetTrustRule(ctx, dana, "ci-example-main", TrustChange{State: "paused"}); !invalid(err) {
		t.Errorf("SetTrustRule(state paused) = %v, want ErrInvalid", err)
	}
}

func TestTrustRuleLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lim   ControlLimits
		limit string
	}{
		{"per principal", ControlLimits{TrustRulesPerPrincipal: 2}, "trust_rules_per_principal"},
		{"per server", ControlLimits{TrustRules: 2}, "trust_rules_per_server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := openControl(t, newDSN(t), ControlOptions{Limits: tc.lim})
			mustService(t, c, "ci-example")
			mustService(t, c, "deploy-example")
			mustTrust(t, c, ghaRule("rule-1", "ci-example", with(ghaClaims, "ref", "refs/heads/a")))
			mustTrust(t, c, ghaRule("rule-2", "ci-example", with(ghaClaims, "ref", "refs/heads/b")))
			principal := "ci-example"
			if tc.limit == "trust_rules_per_server" {
				principal = "deploy-example"
			}
			_, err := c.AddTrustRule(t.Context(), dana, ghaRule("rule-3", principal, with(ghaClaims, "ref", "refs/heads/c")))
			if ce, ok := errors.AsType[*ControlLimitError](err); !ok || ce.Limit != tc.limit || ce.Max != 2 {
				t.Errorf("AddTrustRule past %s = %v, want a *ControlLimitError naming it", tc.limit, err)
			}
			if principal == "deploy-example" {
				return
			}
			// Another principal still has room per principal.
			if _, err := c.AddTrustRule(t.Context(), dana, ghaRule("rule-3", "deploy-example", with(ghaClaims, "ref", "refs/heads/c"))); err != nil {
				t.Errorf("AddTrustRule(another principal) = %v", err)
			}
		})
	}
}

func TestWorkloadSignIn(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{})
	ctx := t.Context()
	mustService(t, c, "ci-example")
	mustService(t, c, "release-example")
	mustTrust(t, c, ghaRule("ci-example-main", "ci-example", ghaClaims))
	seq := controlSeq(t, c)

	token := with(ghaClaims, "sub", "repo:example/app:ref:refs/heads/main", "run_id", "8123")
	g, r, err := c.WorkloadSignIn(ctx, workloadIn("job-1", token))
	if err != nil {
		t.Fatalf("WorkloadSignIn = %v", err)
	}
	if g.Principal != "ci-example" || g.Kind != KindService || g.TrustRule != "ci-example-main" || r.Name != "ci-example-main" || g.Flow != FlowWorkload ||
		g.Provider != "github-actions" || strings.Join(g.Scopes, " ") != "ci" || !g.EndsAt.Equal(identityAt.Add(time.Hour)) || g.Identity != 0 {
		t.Errorf("WorkloadSignIn = %+v, %+v; want ci-example's grant by ci-example-main for an hour, scope ci", g, r)
	}
	if got, ok := c.TokenGrant(tokenHash("job-1")); !ok || got.ID != g.ID || got.TrustRule != "ci-example-main" {
		t.Errorf("TokenGrant(job-1) = %+v, %t", got, ok)
	}
	evs := eventsSince(t, c, seq)
	if len(evs) != 1 || evs[0].Op != OpSignInGrant || evs[0].Actor.Principal != "ci-example" || evs[0].Actor.Session != signInSession {
		t.Fatalf("events = %+v, want signin.grant by ci-example", evs)
	}
	for _, want := range []string{`"trust_rule":"ci-example-main"`, `"flow":"workload"`, `"run_id":"8123"`, `"event_name":"push"`} {
		if !strings.Contains(string(evs[0].After), want) {
			t.Errorf("signin.grant = %s, want %s in it", evs[0].After, want)
		}
	}
	for _, secret := range []string{tokenHash("job-1"), "thumbprint-of-the-jobs-key"} {
		if strings.Contains(string(evs[0].After), secret) {
			t.Errorf("signin.grant holds %q", secret)
		}
	}
	if gs, err := c.Grants(ctx, "ci-example"); err != nil || len(gs) != 1 || gs[0].TrustRule != "ci-example-main" {
		t.Errorf("Grants(ci-example) = %+v, %v", gs, err)
	}

	// The lifetime is the rule's, at most the server's.
	in := workloadIn("job-2", token)
	in.MaxLifetime = 20 * time.Minute
	if g, _, err := c.WorkloadSignIn(ctx, in); err != nil || !g.EndsAt.Equal(identityAt.Add(20*time.Minute)) {
		t.Errorf("WorkloadSignIn(max 20m) = %+v, %v; want it to end in 20 minutes", g, err)
	}

	// Of one principal's rules, the most specific.
	mustTrust(t, c, ghaRule("ci-example-env", "ci-example", with(ghaClaims, "environment", "staging")))
	if _, r, err := c.WorkloadSignIn(ctx, workloadIn("job-3", with(token, "environment", "staging"))); err != nil || r.Name != "ci-example-env" {
		t.Errorf("WorkloadSignIn(two rules of ci-example) = %+v, %v; want the one with more claims", r, err)
	}

	// Rules of two principals: refused, naming both.
	mustTrust(t, c, ghaRule("release-example", "release-example", with(ghaClaims, "environment", "release")))
	_, _, err = c.WorkloadSignIn(ctx, workloadIn("job-4", with(token, "environment", "release")))
	if te, ok := errors.AsType[*TrustError](err); !ok || te.Reason != TrustAmbiguous || strings.Join(te.Rules, " ") != "ci-example-main release-example" ||
		!errors.Is(err, ErrForbidden) {
		t.Errorf("WorkloadSignIn(rules of two principals) = %v, want refused naming both", err)
	}

	for _, tc := range []struct {
		name   string
		in     WorkloadSignIn
		reason TrustReason
	}{
		{"another repository", workloadIn("job-5", with(token, "repository_id", "987654321")), TrustNoMatch},
		{"a claim missing", workloadIn("job-6", map[string]string{"repository_id": "123456789", "ref": "refs/heads/main"}), TrustNoMatch},
		{"another provider", func() WorkloadSignIn { in := workloadIn("job-7", token); in.Provider = "gcp"; return in }(), TrustNoMatch},
	} {
		if _, _, err := c.WorkloadSignIn(ctx, tc.in); !trustReason(tc.reason)(err) || !errors.Is(err, ErrForbidden) {
			t.Errorf("WorkloadSignIn(%s) = %v, want %s", tc.name, err, tc.reason)
		}
	}
	if _, _, err := c.WorkloadSignIn(ctx, func() WorkloadSignIn { in := workloadIn("job-8", token); in.TokenHash = "x"; return in }()); !errors.Is(err, ErrInvalid) {
		t.Errorf("WorkloadSignIn(a bad hash) = %v, want ErrInvalid", err)
	}

	// A disabled principal is refused, and its sign-ins end.
	if _, err := c.DisablePrincipal(ctx, dana, "ci-example"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.TokenGrant(tokenHash("job-1")); ok {
		t.Error("a disabled service's sign-in is still live")
	}
	clk.add(time.Second)
	if _, _, err := c.WorkloadSignIn(ctx, workloadIn("job-9", token)); !trustReason(TrustDisabledPrincipal)(err) {
		t.Errorf("WorkloadSignIn(disabled) = %v, want refused as disabled", err)
	}
}

func TestWorkloadSignInLimit(t *testing.T) {
	t.Parallel()
	c, clk := clockControl(t, ControlOptions{Limits: ControlLimits{GrantsPerTrustRule: 2}})
	ctx := t.Context()
	mustService(t, c, "ci-example")
	mustTrust(t, c, ghaRule("ci-example-main", "ci-example", ghaClaims))
	for _, tok := range []string{"job-1", "job-2"} {
		if _, _, err := c.WorkloadSignIn(ctx, workloadIn(tok, ghaClaims)); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := c.WorkloadSignIn(ctx, workloadIn("job-3", ghaClaims))
	if ce, ok := errors.AsType[*ControlLimitError](err); !ok || ce.Limit != "grants_per_trust_rule" {
		t.Fatalf("WorkloadSignIn past grants_per_trust_rule = %v", err)
	}
	// Once they end, there is room again.
	clk.add(time.Hour + time.Second)
	if _, _, err := c.WorkloadSignIn(ctx, workloadIn("job-3", ghaClaims)); err != nil {
		t.Errorf("WorkloadSignIn after the others ended = %v", err)
	}
}
