package server

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/oidc/oidctest"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/wsconn"
)

// The workload tests' repository, as GitHub Actions' tokens name it.
var jobClaims = map[string]any{"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/main",
	"repository": "example/app", "run_id": "8123", "event_name": "push", "sub": "repo:example/app:ref:refs/heads/main"}

// workloadWorld is a server of many projects over HTTPS that signs in
// no person, and signs service principals in by the tokens of a stand-in
// platform, github-actions: ci-example, owned by alice, a member of
// projA, by the rule ci-example-main.
type workloadWorld struct {
	*multi
	url    string
	pool   *x509.CertPool
	iss    *oidctest.Issuer
	clk    *clock
	client *http.Client
}

// newWorkloadWorld starts the world; edit, when it is not nil, changes
// the provider's settings and the server's Workload before it starts.
func newWorkloadWorld(t *testing.T, lim Limits, edit func(*oidc.WorkloadSettings, *Workload)) *workloadWorld {
	t.Helper()
	iss := oidctest.New(t)
	clk := newClock()
	clk.advance(time.Hour)
	iss.Clock(clk.now)
	ws := oidc.WorkloadSettings{Name: "github-actions", Kind: oidc.KindGitHubActions, Issuer: iss.URL, CAFile: iss.CAFile}
	wl := Workload{TokenLifetime: time.Hour, MaxTokenLifetime: 6 * time.Hour, Fresh: 2 * time.Minute}
	if edit != nil {
		edit(&ws, &wl)
	}
	w := &workloadWorld{iss: iss, clk: clk}
	w.multi = newMultiWith(t, lim, clk, nil, func(cfg *Config) {
		w.url, w.pool = listenHTTPS(t, cfg, nil)
		p, err := oidc.OpenWorkload(ws, w.url, oidc.Options{Now: clk.now, Skew: 30 * time.Second, MaxAge: wl.Fresh, Refetch: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		wl.Providers = []*oidc.Workload{p}
		cfg.Workload = &wl
	}, dana.Principal)
	serving(t, w.Server)
	w.project(t, projA, "alice")
	if _, err := w.ctl.AddNewPrincipal(t.Context(), dana, store.NewPrincipal{Name: "ci-example", Kind: store.KindService, Owner: "alice", Project: projA}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ctl.AddTrustRule(t.Context(), dana, store.TrustRule{Name: "ci-example-main", Principal: "ci-example", Provider: ws.Name,
		Claims: map[string]string{"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/main"}, TokenLifetime: time.Hour}); err != nil {
		t.Fatal(err)
	}
	w.client = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: w.pool, MinVersion: tls.VersionTLS12}}}
	return w
}

// job is one CI job: its DPoP key, and its key's thumbprint.
type job struct {
	key *ecdsa.PrivateKey
	jkt string
}

func newJob(t *testing.T) *job {
	t.Helper()
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &job{key: k, jkt: dpop.Thumbprint(&k.PublicKey)}
}

// token is the platform's token for j, bound to its key, issued now,
// with claims over jobClaims.
func (w *workloadWorld) token(j *job, claims map[string]any) string {
	now := w.clk.now()
	c := map[string]any{"iss": w.iss.URL, "aud": oidc.BoundAudience(w.url, j.jkt), "iat": now.Unix(), "nbf": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(), "jti": now.String() + j.jkt}
	for k, v := range jobClaims {
		c[k] = v
	}
	for k, v := range claims {
		if v == nil {
			delete(c, k)
			continue
		}
		c[k] = v
	}
	return w.iss.Sign(c)
}

// exchangeForm is a token exchange of subject.
func exchangeForm(subject string) url.Values {
	return url.Values{"grant_type": {grantTokenExchange}, "subject_token": {subject}, "subject_token_type": {tokenTypeJWT}, "machine": {"runner-1"}}
}

// post sends form to the token endpoint with a proof by j's key, or
// proof when it is set ("-" for none), and returns the answer.
func (w *workloadWorld) post(t *testing.T, j *job, form url.Values, proof string) tokenAnswer {
	t.Helper()
	if proof == "" {
		var err error
		if proof, err = dpop.Proof(j.key, http.MethodPost, w.url+pathToken, "", w.clk.now()); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, w.url+pathToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if proof != "-" {
		req.Header.Set("DPoP", proof)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out tokenAnswer
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("the token endpoint's answer (%s): %v", resp.Status, err)
	}
	out.Status = resp.StatusCode
	return out
}

// connect opens /v1/connect with token and a proof by j's key, and says
// hello in session.
func (w *workloadWorld) connect(t *testing.T, j *job, token, session string) (*wsconn.Conn, *proto.Frame) {
	t.Helper()
	proof, err := dpop.Proof(j.key, http.MethodGet, w.url+keyproof.ConnectPath, token, w.clk.now())
	if err != nil {
		t.Fatal(err)
	}
	c, resp, err := wsDial(t, w.url, w.pool, http.Header{"Authorization": {"DPoP " + token}, "DPoP": {proof}})
	if err != nil {
		t.Fatalf("connect with the job's token: %v (%v)", err, resp)
	}
	return c, helloAs(t, c, session)
}

// TestWorkloadExchange: a CI job's token, bound to its key, is exchanged
// for a sign-in of scope ci as the service its trust rule names, which
// connects in the session ci, does a member's work, and nothing more.
func TestWorkloadExchange(t *testing.T) {
	t.Parallel()
	w := newWorkloadWorld(t, Limits{}, nil)
	j := newJob(t)
	subject := w.token(j, nil)
	ans := w.post(t, j, exchangeForm(subject), "")
	if ans.Status != http.StatusOK || ans.AccessToken == "" || ans.TokenType != "DPoP" || ans.Scope != proto.ScopeCI || ans.Principal != "ci-example" ||
		ans.ExpiresAt != w.clk.now().Add(time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339) {
		t.Fatalf("exchange = %+v, want ci-example's DPoP token of scope ci for an hour", ans)
	}
	g, ok := w.ctl.TokenGrant(hashOf(ans.AccessToken))
	if !ok || g.TrustRule != "ci-example-main" || g.Flow != store.FlowWorkload || g.Provider != "github-actions" || g.Machine != "runner-1" ||
		g.JKT != j.jkt || g.Kind != store.KindService {
		t.Errorf("the grant = %+v, %t; want the rule's, bound to the job's key", g, ok)
	}

	c, wel := w.connect(t, j, ans.AccessToken, proto.CISession)
	if wel.Err != nil || wel.Principal != "ci-example" {
		t.Fatalf("the welcome = %+v, %v; want ci-example's", wel, wel.Err)
	}
	if f := wsCall(t, c, 1, proto.OpCreate, proto.CreateArgs{Title: "from CI"}); f.Err != nil {
		t.Errorf("create, as a CI job = %v", f.Err)
	}
	if f := wsCall(t, c, 2, proto.OpPrincipals, proto.AdminListArgs{}); f.Err == nil || f.Err.Code != proto.CodeForbidden {
		t.Errorf("principals, as a CI job = %+v, want forbidden: the scope ci is a member's", f.Err)
	}
	if _, wel := w.connect(t, j, ans.AccessToken, proto.CLISession); wel.Err == nil || wel.Err.Code != proto.CodeForbidden {
		t.Errorf("the session cli, as a CI job = %+v, want forbidden", wel.Err)
	}

	log := w.log.String()
	for _, secret := range []string{ans.AccessToken, subject} {
		if strings.Contains(log, secret) {
			t.Error("the log holds the job's token or the platform's")
		}
	}
	if !regexp.MustCompile(`event=signin.granted .*principal=ci-example provider=github-actions grant=g-\w+ flow=workload trust_rule=ci-example-main`).MatchString(log) {
		t.Errorf("the log has no signin.granted line for the workload:\n%s", log)
	}
	if !regexp.MustCompile(`event=audit.signin.grant audit=true .* principal=ci-example session=signin .* subject=ci-example`).MatchString(log) {
		t.Errorf("the audit stream has no signin.grant for ci-example:\n%s", log)
	}
	evs, err := w.ctl.Events(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Op != store.OpSignInGrant || !strings.Contains(string(last.After), `"run_id":"8123"`) || !strings.Contains(string(last.After), `"trust_rule":"ci-example-main"`) {
		t.Errorf("the last event = %s %s, want signin.grant with the rule and the run", last.Op, last.After)
	}

	// What a client reads before it signs in.
	var wk WellKnown
	getJSON(t, w.client, w.url+"/.well-known/aries", &wk)
	if !strings.Contains(strings.Join(wk.Auth, " "), "workload") || strings.Contains(strings.Join(wk.Auth, " "), "signin") || len(wk.Workloads) != 1 ||
		wk.Workloads[0] != (WellWorkload{Name: "github-actions", Kind: oidc.KindGitHubActions, Audience: w.url, Bound: true}) {
		t.Errorf("/.well-known/aries = %+v, want the workload provider, bound to the server's audience, and no person's sign-in", wk)
	}
	var meta map[string]any
	getJSON(t, w.client, w.url+pathASMeta, &meta)
	if g, _ := json.Marshal(meta["grant_types_supported"]); string(g) != `["`+grantTokenExchange+`"]` || meta["authorization_endpoint"] != nil {
		t.Errorf("%s = %v, want token exchange alone", pathASMeta, meta)
	}
	if resp, err := w.client.Get(w.url + pathAuthorize); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET %s on a server that signs in no person = %v, %v; want 404", pathAuthorize, resp, err)
	}
	st := mustAsk[proto.StatusResult](mustDialTo(t, w.Server, dana, projA), proto.OpStatus, proto.StatusArgs{})
	if st.Server == nil || st.Server.SignIn == nil || len(st.Server.SignIn.Workloads) != 1 || st.Server.SignIn.Workloads[0].Name != "github-actions" ||
		!st.Server.SignIn.Workloads[0].Discovered {
		t.Errorf("status = %+v, want the workload provider, discovered", st.Server)
	}
}

func getJSON(t *testing.T, c *http.Client, u string, out any) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("GET %s (%s): %v", u, resp.Status, err)
	}
}

// TestWorkloadExchangeRefusals: each refusal at the exchange has its
// code and a fix, and none is a sign-in.
func TestWorkloadExchangeRefusals(t *testing.T) {
	t.Parallel()
	w := newWorkloadWorld(t, Limits{}, nil)
	for _, tc := range []struct {
		name   string
		form   func(*job) url.Values
		proof  func(*job) string
		status int
		code   proto.Code
		want   string // in the description or the fix
	}{
		{"the audience unbound", func(*job) url.Values {
			now := w.clk.now()
			return exchangeForm(w.iss.Sign(map[string]any{"iss": w.iss.URL, "aud": w.url, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
				"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/main"}))
		}, nil, http.StatusBadRequest, proto.CodeAuth, "audience"},
		{"bound to another key", func(*job) url.Values { return exchangeForm(w.token(newJob(t), nil)) }, nil, http.StatusBadRequest, proto.CodeAuth, "audience"},
		{"too old", func(j *job) url.Values {
			return exchangeForm(w.token(j, map[string]any{"iat": w.clk.now().Add(-3 * time.Minute).Unix()}))
		}, nil, http.StatusBadRequest, proto.CodeAuth, "issued"},
		{"another issuer", func(j *job) url.Values {
			return exchangeForm(w.token(j, map[string]any{"iss": "https://token.example.com"}))
		}, nil, http.StatusBadRequest, proto.CodeForbidden, "workload: providers"},
		{"no rule", func(j *job) url.Values {
			return exchangeForm(w.token(j, map[string]any{"repository_id": "987654321"}))
		}, nil, http.StatusForbidden, proto.CodeForbidden, "--claim repository_id=987654321"},
		{"no proof", func(j *job) url.Values { return exchangeForm(w.token(j, nil)) }, func(*job) string { return "-" },
			http.StatusBadRequest, proto.CodeAuth, "proof"},
		{"a proof for another server", func(j *job) url.Values { return exchangeForm(w.token(j, nil)) }, func(j *job) string {
			p, _ := dpop.Proof(j.key, http.MethodPost, "https://aries.example.com"+pathToken, "", w.clk.now())
			return p
		}, http.StatusBadRequest, proto.CodeAuth, ""},
		{"the scope cli", func(j *job) url.Values {
			f := exchangeForm(w.token(j, nil))
			f.Set("scope", "cli")
			return f
		}, nil, http.StatusBadRequest, proto.CodeInvalid, "scope ci"},
		{"another token type", func(j *job) url.Values {
			f := exchangeForm(w.token(j, nil))
			f.Set("subject_token_type", "urn:ietf:params:oauth:token-type:saml2")
			return f
		}, nil, http.StatusBadRequest, proto.CodeInvalid, "subject_token_type"},
		{"another resource", func(j *job) url.Values {
			f := exchangeForm(w.token(j, nil))
			f.Set("resource", "https://other.example.com")
			return f
		}, nil, http.StatusBadRequest, proto.CodeInvalid, "not this server"},
		{"not a token", func(*job) url.Values { return exchangeForm("not.a.token") }, nil, http.StatusBadRequest, proto.CodeAuth, "JWT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := newJob(t)
			proof := ""
			if tc.proof != nil {
				proof = tc.proof(j)
			}
			ans := w.post(t, j, tc.form(j), proof)
			if ans.Status != tc.status || proto.Code(ans.Code) != tc.code || ans.AccessToken != "" || ans.Fix == "" ||
				!strings.Contains(ans.Description+" "+ans.Fix, tc.want) {
				t.Errorf("exchange(%s) = %+v, want %d, %s naming %q, with a fix", tc.name, ans, tc.status, tc.code, tc.want)
			}
		})
	}
	if gs, err := w.ctl.Grants(t.Context(), "ci-example"); err != nil || len(gs) != 0 {
		t.Errorf("ci-example's sign-ins after the refusals = %+v, %v; want none", gs, err)
	}

	// A token is spent once, whatever proof comes with it; a proof too.
	j := newJob(t)
	subject := w.token(j, nil)
	if ans := w.post(t, j, exchangeForm(subject), ""); ans.Status != http.StatusOK {
		t.Fatalf("exchange = %+v", ans)
	}
	if ans := w.post(t, j, exchangeForm(subject), ""); ans.Status != http.StatusBadRequest || !strings.Contains(ans.Description, "exchanged already") {
		t.Errorf("the same token again = %+v, want refused as spent", ans)
	}
	proof, _ := dpop.Proof(j.key, http.MethodPost, w.url+pathToken, "", w.clk.now())
	if ans := w.post(t, j, exchangeForm(w.token(j, map[string]any{"jti": "another"})), proof); ans.Status != http.StatusOK {
		t.Fatalf("exchange = %+v", ans)
	}
	if ans := w.post(t, j, exchangeForm(w.token(j, map[string]any{"jti": "a third"})), proof); ans.Status != http.StatusBadRequest || ans.Error != "invalid_dpop_proof" {
		t.Errorf("the same proof again = %+v, want refused", ans)
	}
}

// TestWorkloadUnbound: a token bound to no key, Azure's, issued before
// the server started is refused, since the server may have forgotten
// that it spent it; one issued since is taken once.
func TestWorkloadUnbound(t *testing.T) {
	t.Parallel()
	w := newWorkloadWorld(t, Limits{}, func(ws *oidc.WorkloadSettings, _ *Workload) {
		ws.Name, ws.Kind, ws.Audience = "azure", oidc.KindAzure, "api://aries.example.com"
	})
	if _, err := w.ctl.AddTrustRule(t.Context(), dana, store.TrustRule{Name: "ci-example-azure", Principal: "ci-example", Provider: "azure",
		Claims: map[string]string{"tid": "00000000-0000-4000-8000-000000000001", "oid": "00000000-0000-4000-8000-000000000002"}, TokenLifetime: time.Hour}); err != nil {
		t.Fatal(err)
	}
	token := func(iat time.Time) string {
		return w.iss.Sign(map[string]any{"iss": w.iss.URL, "aud": "api://aries.example.com", "iat": iat.Unix(), "exp": iat.Add(time.Hour).Unix(),
			"tid": "00000000-0000-4000-8000-000000000001", "oid": "00000000-0000-4000-8000-000000000002", "xms_mirid": "/subscriptions/x/vm"})
	}
	j := newJob(t)
	if ans := w.post(t, j, exchangeForm(token(w.clk.now().Add(-30*time.Second))), ""); ans.Status != http.StatusBadRequest ||
		!strings.Contains(ans.Description, "before the server started") {
		t.Errorf("a token from before the start = %+v, want refused", ans)
	}
	w.clk.advance(time.Second)
	ans := w.post(t, j, exchangeForm(token(w.clk.now())), "")
	if ans.Status != http.StatusOK || ans.Principal != "ci-example" {
		t.Fatalf("a token since the start = %+v, want ci-example signed in", ans)
	}
	if g, _ := w.ctl.TokenGrant(hashOf(ans.AccessToken)); g.TrustRule != "ci-example-azure" {
		t.Errorf("the grant's rule = %q", g.TrustRule)
	}
}

// TestWorkloadLimits: a rule gives at most grants_per_trust_rule live
// sign-ins, and the server remembers at most workload_tokens_per_server
// tokens; each refusal is busy, with a fix.
func TestWorkloadLimits(t *testing.T) {
	t.Parallel()
	w := newWorkloadWorld(t, Limits{WorkloadTokens: 3, Control: store.ControlLimits{GrantsPerTrustRule: 2}}, nil)
	for range 2 {
		j := newJob(t)
		if ans := w.post(t, j, exchangeForm(w.token(j, nil)), ""); ans.Status != http.StatusOK {
			t.Fatalf("exchange = %+v", ans)
		}
	}
	j := newJob(t)
	if ans := w.post(t, j, exchangeForm(w.token(j, nil)), ""); ans.Status != http.StatusServiceUnavailable || proto.Code(ans.Code) != proto.CodeBusy ||
		!strings.Contains(ans.Fix, "aries logout") || !strings.Contains(ans.Fix, "grants_per_trust_rule") {
		t.Errorf("a third job of a rule of two = %+v, want busy naming aries logout and the limit", ans)
	}
	// Three tokens are remembered now, each until its window ends.
	j = newJob(t)
	if ans := w.post(t, j, exchangeForm(w.token(j, nil)), ""); ans.Status != http.StatusServiceUnavailable || !strings.Contains(ans.Description, "workload tokens") {
		t.Errorf("a fourth token with three remembered = %+v, want busy", ans)
	}
	w.clk.advance(2*time.Minute + maxSkew + time.Second)
	j = newJob(t)
	if ans := w.post(t, j, exchangeForm(w.token(j, nil)), ""); ans.Code == string(proto.CodeBusy) && strings.Contains(ans.Description, "workload tokens") {
		t.Errorf("a token once the others aged out = %+v, want it remembered", ans)
	}
}

// TestTrustOps: trust rules over the protocol, for server admins; a
// service's owner reads its rules; changing a rule's claims ends the
// sign-ins it gave and closes their connections.
func TestTrustOps(t *testing.T) {
	t.Parallel()
	w := newWorkloadWorld(t, Limits{}, nil)
	w.project(t, projB, "bob")
	admin := mustDialTo(t, w.Server, dana, projA)
	claims := map[string]string{"repository_owner_id": "111111111", "repository_id": "123456789", "environment": "release"}
	added := mustAsk[proto.TrustResult](admin, proto.OpTrustAdd, proto.TrustAddArgs{Name: "ci-example-release", Principal: "ci-example",
		Provider: "github-actions", Claims: claims, TokenLifetime: "90m"})
	if added.Change != "added" || added.TokenLifetime != "90m" || added.State != proto.TrustActive || added.CreatedBy != "dana" ||
		strings.Join(added.Required, " ") != "repository_owner_id repository_id" {
		t.Errorf("trust.add = %+v", added)
	}
	if !regexp.MustCompile(`event=audit.trust.add audit=true .* principal=dana .* target=ci-example-release subject=ci-example`).MatchString(w.log.String()) {
		t.Errorf("the audit stream has no trust.add line:\n%s", w.log.String())
	}
	for _, tc := range []struct {
		name string
		args proto.TrustAddArgs
		code proto.Code
		want string
	}{
		{"an unknown provider", proto.TrustAddArgs{Name: "r", Principal: "ci-example", Provider: "gitlab", Claims: claims}, proto.CodeInvalid, "github-actions"},
		{"a required claim missing", proto.TrustAddArgs{Name: "r", Principal: "ci-example", Provider: "github-actions",
			Claims: map[string]string{"repository_id": "123456789", "ref": "refs/heads/main"}}, proto.CodeInvalid, "--claim repository_owner_id=VALUE"},
		{"a person", proto.TrustAddArgs{Name: "r", Principal: "alice", Provider: "github-actions", Claims: claims}, proto.CodeInvalid, "--kind service"},
		{"a name in use", proto.TrustAddArgs{Name: "ci-example-main", Principal: "ci-example", Provider: "github-actions", Claims: claims}, proto.CodeExists, "trust set"},
		{"past the server's lifetime", proto.TrustAddArgs{Name: "r", Principal: "ci-example", Provider: "github-actions", Claims: claims, TokenLifetime: "7h"},
			proto.CodeInvalid, "--token-lifetime"},
		{"nobody", proto.TrustAddArgs{Name: "r", Principal: "nobody", Provider: "github-actions", Claims: claims}, proto.CodeNotFound, "principal list"},
	} {
		if _, perr := ask[proto.TrustResult](admin, proto.OpTrustAdd, tc.args); perr == nil || perr.Code != tc.code || !strings.Contains(perr.Fix, tc.want) {
			t.Errorf("trust.add(%s) = %v, want %s with a fix naming %q", tc.name, perr, tc.code, tc.want)
		}
	}

	// alice owns ci-example and reads its rules; bob, and a list of all,
	// are a server admin's.
	owner := mustDialTo(t, w.Server, alice, projA)
	if rs := mustAsk[proto.TrustRulesResult](owner, proto.OpTrustRules, proto.TrustRulesArgs{Principal: "ci-example"}); len(rs.Rules) != 2 {
		t.Errorf("trust_rules(ci-example) by its owner = %+v, want both", rs)
	}
	if r := mustAsk[proto.TrustResult](owner, proto.OpTrustShow, proto.TrustArgs{Name: "ci-example-main"}); r.Claims["ref"] != "refs/heads/main" {
		t.Errorf("trust.show by its owner = %+v", r)
	}
	if _, perr := ask[proto.TrustRulesResult](owner, proto.OpTrustRules, proto.TrustRulesArgs{}); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("trust_rules(all) by an owner = %v, want forbidden", perr)
	}
	if _, perr := ask[proto.TrustResult](owner, proto.OpTrustAdd, proto.TrustAddArgs{Name: "r", Principal: "ci-example", Provider: "github-actions", Claims: claims}); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("trust.add by an owner = %v, want forbidden: it is a server admin's", perr)
	}
	other := mustDialTo(t, w.Server, store.Actor{Principal: "bob", Session: "s", Machine: "m"}, projB)
	for _, name := range []string{"ci-example-main", "nope"} {
		if _, perr := ask[proto.TrustResult](other, proto.OpTrustShow, proto.TrustArgs{Name: name}); perr == nil || perr.Code != proto.CodeForbidden {
			t.Errorf("trust.show(%s) by bob = %v, want forbidden whether or not it exists", name, perr)
		}
	}
	if rs := mustAsk[proto.TrustRulesResult](admin, proto.OpTrustRules, proto.TrustRulesArgs{}); len(rs.Rules) != 2 {
		t.Errorf("trust_rules(all) by dana = %+v", rs)
	}

	// A job signs in; new claims end its sign-in, and close its
	// connection.
	j := newJob(t)
	ans := w.post(t, j, exchangeForm(w.token(j, nil)), "")
	if ans.Status != http.StatusOK {
		t.Fatalf("exchange = %+v", ans)
	}
	c, wel := w.connect(t, j, ans.AccessToken, proto.CISession)
	if wel.Err != nil {
		t.Fatal(wel.Err)
	}
	set := mustAsk[proto.TrustResult](admin, proto.OpTrustSet, proto.TrustSetArgs{Name: "ci-example-main",
		Claims: map[string]string{"repository_owner_id": "111111111", "repository_id": "123456789", "ref": "refs/heads/trunk"}})
	if set.Change != "changed" || set.Ended != 1 || set.Claims["ref"] != "refs/heads/trunk" {
		t.Errorf("trust.set = %+v, want the claims changed and one sign-in ended", set)
	}
	expectEnded(t, c, "trust rule changed")
	if _, perr := ask[proto.TrustResult](admin, proto.OpTrustSet, proto.TrustSetArgs{Name: "ci-example-main", Claims: map[string]string{"repository_id": "1", "ref": "x"}}); perr == nil ||
		!strings.Contains(perr.Fix, "repository_owner_id") {
		t.Errorf("trust.set without a required claim = %v", perr)
	}
	if _, perr := ask[proto.TrustResult](admin, proto.OpTrustSet, proto.TrustSetArgs{Name: "ci-example-main"}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("trust.set of nothing = %v, want invalid", perr)
	}
	if r := mustAsk[proto.TrustResult](admin, proto.OpTrustSet, proto.TrustSetArgs{Name: "ci-example-main", State: proto.TrustDisabled}); r.State != proto.TrustDisabled {
		t.Errorf("trust.set(disabled) = %+v", r)
	}
	gone := mustAsk[proto.TrustResult](admin, proto.OpTrustRemove, proto.TrustArgs{Name: "ci-example-main"})
	if gone.Change != "removed" {
		t.Errorf("trust.remove = %+v", gone)
	}
	if _, perr := ask[proto.TrustResult](admin, proto.OpTrustRemove, proto.TrustArgs{Name: "ci-example-main"}); perr == nil || perr.Code != proto.CodeNotFound ||
		!strings.Contains(perr.Fix, "aries admin trust list") {
		t.Errorf("trust.remove again = %v, want not found, with a fix", perr)
	}
	// A service with rules does not become a person.
	if _, perr := ask[proto.PrincipalResult](admin, proto.OpPrincipalSet, proto.PrincipalSetArgs{Name: "ci-example", Kind: proto.KindPerson}); perr == nil ||
		!strings.Contains(perr.Fix, "aries admin trust remove") {
		t.Errorf("principal.set(ci-example, person) with a rule = %v, want refused naming trust remove", perr)
	}
}

// TestTrustAddNeedsWorkload: a server with no workload: providers takes
// no trust rule, since nothing would check its claims.
func TestTrustAddNeedsWorkload(t *testing.T) {
	t.Parallel()
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice")
	admin := mustDialTo(t, m.Server, dana, projA)
	_, perr := ask[proto.TrustResult](admin, proto.OpTrustAdd, proto.TrustAddArgs{Name: "r", Principal: "ci", Provider: "github-actions",
		Claims: map[string]string{"repository_owner_id": "1", "repository_id": "2"}})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Fix, "workload: providers") {
		t.Errorf("trust.add with no workload providers = %v, want invalid, naming workload: providers", perr)
	}
}

// The workload: settings are read with their defaults and bounds, and
// need a server of many projects and HTTPS.
func TestResolveSettingsWorkload(t *testing.T) {
	t.Parallel()
	const provider = "workload:\n  providers:\n    - name: github-actions\n      kind: github-actions\n      issuer: https://token.example.com\n"
	for _, tc := range []struct {
		name  string
		body  string
		check func(WorkloadSettings) bool
		err   string
	}{
		{name: "the defaults", body: "", check: func(w WorkloadSettings) bool {
			return w.TokenLifetime == Duration(time.Hour) && w.MaxTokenLifetime == Duration(6*time.Hour) && w.Fresh == Duration(2*time.Minute) && len(w.Providers) == 0
		}},
		{name: "a provider", body: provider, check: func(w WorkloadSettings) bool {
			return len(w.Providers) == 1 && w.Providers[0].Kind == oidc.KindGitHubActions
		}},
		{name: "a lifetime past the ceiling", body: "workload:\n  token_lifetime: 7h\n", err: "token_lifetime is 7h; it can be at most max_token_lifetime, 6h"},
		{name: "a ceiling past a day", body: "workload:\n  max_token_lifetime: 2d\n", err: "max_token_lifetime is 2d; it can be at most 1d"},
		{name: "fresh too long", body: "workload:\n  fresh: 1h\n", err: "fresh is 1h; it must be from 30s to 10m"},
		{name: "two providers of one name", body: provider + "    - name: github-actions\n      kind: google\n      issuer: https://accounts.example.com\n",
			err: "two providers are named github-actions"},
		{name: "an unknown key", body: provider + "      client_secret: x\n", err: "field client_secret not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := resolveFile(t, tc.body)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(tc.err, "not found") && !strings.Contains(err.Error(), "fix:") {
					t.Errorf("ResolveSettings(%q) = %v, want %q, with a fix", tc.body, err, tc.err)
				}
				return
			}
			if err != nil || !tc.check(s.Workload) {
				t.Errorf("ResolveSettings(%q).Workload = %+v, %v", tc.body, s.Workload, err)
			}
		})
	}
	const https = "https:\n  listen: 127.0.0.1:8443\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: proxy\n    proxy:\n      trusted: [127.0.0.1/32]\n"
	for _, tc := range []struct{ name, body, err string }{
		{"a server of one project", https + provider, "control_schema"},
		{"SSH alone", "transports: [ssh]\ncontrol_schema: aries_control\n" + provider, "HTTPS"},
		{"both", https + "control_schema: aries_control\n" + provider, ""},
	} {
		s, err := resolveFile(t, tc.body)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		err = s.CheckServe()
		if (tc.err == "") != (err == nil) || err != nil && (!strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "fix:")) {
			t.Errorf("%s: CheckServe = %v, want an error naming %q", tc.name, err, tc.err)
		}
	}
}

// OpenWorkload opens the providers, fetching nothing, and refuses two of
// one issuer.
func TestOpenWorkload(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	ws := WorkloadSettings{TokenLifetime: Duration(time.Hour), MaxTokenLifetime: Duration(6 * time.Hour), Fresh: Duration(2 * time.Minute),
		Providers: []oidc.WorkloadSettings{{Name: "github-actions", Kind: oidc.KindGitHubActions, Issuer: iss.URL}}}
	login := LoginSettings{Skew: Duration(30 * time.Second), JWKSRefetch: Duration(time.Minute)}
	wl, err := OpenWorkload(ws, login, "https://aries.example.com", time.Now)
	if err != nil || len(wl.Providers) != 1 || wl.Providers[0].Audience() != "https://aries.example.com" || wl.Fresh != 2*time.Minute {
		t.Fatalf("OpenWorkload = %+v, %v", wl, err)
	}
	if n := iss.Discovery.Load() + iss.Keys.Load(); n != 0 {
		t.Errorf("OpenWorkload made %d requests of the issuer, want none", n)
	}
	ws.Audience = "https://aud.example.com"
	if wl, _ := OpenWorkload(ws, login, "https://aries.example.com", time.Now); wl.Providers[0].Audience() != "https://aud.example.com" {
		t.Errorf("OpenWorkload(audience set) = audience %q", wl.Providers[0].Audience())
	}
	ws.Providers = append(ws.Providers, oidc.WorkloadSettings{Name: "gha-b", Kind: oidc.KindGitHubActions, Issuer: iss.URL})
	if _, err := OpenWorkload(ws, login, "https://aries.example.com", time.Now); err == nil || !strings.Contains(err.Error(), "one issuer") {
		t.Errorf("OpenWorkload(two of one issuer) = %v", err)
	}
	if wl, err := OpenWorkload(WorkloadSettings{}, login, "", time.Now); wl != nil || err != nil {
		t.Errorf("OpenWorkload(none) = %+v, %v", wl, err)
	}
}
