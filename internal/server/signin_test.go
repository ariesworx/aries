package server

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
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
	"github.com/ariesworx/aries/internal/telemetry"
	"github.com/ariesworx/aries/internal/wsconn"
)

// loopbackRedirect is where the tests' aries says the browser comes
// back; nothing listens there: the test's browser stops before it.
const loopbackRedirect = "http://127.0.0.1:53682/callback"

// signInWorld is a server of many projects over HTTPS that signs people
// in at a stand-in provider, corp, with alice invited there and a member
// of projA.
type signInWorld struct {
	*multi
	url  string
	pool *x509.CertPool
	iss  *oidctest.Issuer
	clk  *clock
	// browser trusts the server and the provider, and stops at the
	// loopback redirect.
	browser *http.Client
}

// newSignInWorld starts the world; edit, when it is not nil, changes
// the provider's settings and the server's Login before it starts.
func newSignInWorld(t *testing.T, edit func(*oidctest.Issuer, *oidc.Settings, *Login)) *signInWorld {
	t.Helper()
	return newMeasuredSignInWorld(t, edit, nil)
}

// newMeasuredSignInWorld is newSignInWorld with the server's telemetry
// tel, and admins as server admins beside dana.
func newMeasuredSignInWorld(t *testing.T, edit func(*oidctest.Issuer, *oidc.Settings, *Login), tel *telemetry.Telemetry, admins ...string) *signInWorld {
	t.Helper()
	return newLimitedSignInWorld(t, Limits{}, edit, tel, admins...)
}

// newLimitedSignInWorld is newMeasuredSignInWorld with the server's
// limits lim.
func newLimitedSignInWorld(t *testing.T, lim Limits, edit func(*oidctest.Issuer, *oidc.Settings, *Login), tel *telemetry.Telemetry, admins ...string) *signInWorld {
	t.Helper()
	return newSignInWorldWith(t, lim, edit, tel, nil, admins...)
}

// newSignInWorldWith is newLimitedSignInWorld whose HTTPS settings
// https, when it is not nil, changes before the server starts.
func newSignInWorldWith(t *testing.T, lim Limits, edit func(*oidctest.Issuer, *oidc.Settings, *Login), tel *telemetry.Telemetry, https func(*HTTPS), admins ...string) *signInWorld {
	t.Helper()
	iss := oidctest.New(t)
	// The world's time is not the wall clock's (rule 19): what it dates
	// by the wall clock is refused on every run, not only on a slow one.
	clk := newClock()
	clk.advance(time.Hour)
	iss.Clock(clk.now)
	secret := t.TempDir() + "/client-secret"
	if err := os.WriteFile(secret, []byte(oidctest.Secret), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := oidc.Settings{Name: "corp", Kind: oidc.KindOIDC, Issuer: iss.URL, ClientID: oidctest.ClientID, ClientSecretFile: secret, CAFile: iss.CAFile}
	login := Login{}
	if edit != nil {
		edit(iss, &ps, &login)
	}
	p, err := oidc.Open(ps, oidc.Options{Now: clk.now, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	login.Providers = []*oidc.Provider{p}
	w := &signInWorld{iss: iss, clk: clk}
	w.multi = newMultiWith(t, lim, clk, nil, func(cfg *Config) {
		w.url, w.pool = listenHTTPS(t, cfg, https)
		cfg.Login = &login
		cfg.Telemetry = tel
	}, append([]string{dana.Principal}, admins...)...)
	serving(t, w.Server)
	w.project(t, projA, "alice")
	if _, _, err := w.ctl.Invite(t.Context(), dana, "alice", store.Invitation{Provider: "corp", Address: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(iss.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	both := w.pool.Clone()
	both.AppendCertsFromPEM(pem)
	w.browser = &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: both, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if strings.HasPrefix(req.URL.String(), loopbackRedirect) {
				return http.ErrUseLastResponse
			}
			return nil
		}}
	return w
}

// attempt is one sign-in as aries makes it: its PKCE verifier, state
// and DPoP key.
type attempt struct {
	verifier, state string
	key             *ecdsa.PrivateKey
	scope           string
}

func newAttempt(t *testing.T) *attempt {
	t.Helper()
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &attempt{verifier: rand.Text() + rand.Text(), state: rand.Text(), key: k, scope: "cli mcp"}
}

// authorizeURL is the URL aries opens the browser at.
func (w *signInWorld) authorizeURL(a *attempt) string {
	sum := sha256.Sum256([]byte(a.verifier))
	v := url.Values{"response_type": {"code"}, "client_id": {ClientCLI}, "redirect_uri": {loopbackRedirect}, "state": {a.state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "scope": {a.scope}}
	return w.url + pathAuthorize + "?" + v.Encode()
}

// browse follows u to the loopback redirect, and returns its query.
func (w *signInWorld) browse(t *testing.T, u string) url.Values {
	t.Helper()
	resp, err := w.browser.Get(u)
	if err != nil {
		t.Fatalf("browse %s: %v", u, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, loopbackRedirect) {
		t.Fatalf("the browser ended at %s (%s), not the loopback redirect: %s", resp.Request.URL, resp.Status, body)
	}
	back, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return back.Query()
}

// tokenAnswer is the token endpoint's answer, a token or a refusal.
type tokenAnswer struct {
	Status      int
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresAt   string `json:"expires_at"`
	Scope       string `json:"scope"`
	Principal   string `json:"principal"`
	Grant       string `json:"grant"`
	Error       string `json:"error"`
	Description string `json:"error_description"`
	Code        string `json:"code"`
	Fix         string `json:"fix"`
}

// exchange spends code at the token endpoint with a's verifier and a
// proof by a's key, or proof when it is set.
func (w *signInWorld) exchange(t *testing.T, a *attempt, code, proof string) tokenAnswer {
	t.Helper()
	if proof == "" {
		proof = w.proof(t, a.key, http.MethodPost, w.url+pathToken, "")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {loopbackRedirect}, "client_id": {ClientCLI},
		"code_verifier": {a.verifier}, "machine": {"laptop-a"}}
	req, _ := http.NewRequest(http.MethodPost, w.url+pathToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if proof != "-" {
		req.Header.Set("DPoP", proof)
	}
	resp, err := w.browser.Do(req)
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

// signIn signs a in at corp as whoever the stand-in signs in, and
// returns the token endpoint's answer.
func (w *signInWorld) signIn(t *testing.T, a *attempt) tokenAnswer {
	t.Helper()
	back := w.browse(t, w.authorizeURL(a))
	if back.Get("state") != a.state || back.Get("iss") != w.url {
		t.Fatalf("the redirect back carries state %q and iss %q, want %q and %q", back.Get("state"), back.Get("iss"), a.state, w.url)
	}
	return w.exchange(t, a, back.Get("code"), "")
}

// mustSignIn is signIn for a sign-in that must be granted.
func (w *signInWorld) mustSignIn(t *testing.T, a *attempt) tokenAnswer {
	t.Helper()
	ans := w.signIn(t, a)
	if ans.Status != http.StatusOK || ans.AccessToken == "" {
		t.Fatalf("sign-in = %+v, want a token", ans)
	}
	return ans
}

// connectWith opens /v1/connect presenting token with proof, made by
// a's key for the server's URL when proof is empty.
func (w *signInWorld) connectWith(t *testing.T, a *attempt, token, proof string) (*wsconn.Conn, *proto.Error) {
	t.Helper()
	if proof == "" {
		proof = w.proof(t, a.key, http.MethodGet, w.url+keyproof.ConnectPath, token)
	}
	h := http.Header{"Authorization": {"DPoP " + token}}
	if proof != "-" {
		h.Set("DPoP", proof)
	}
	c, resp, err := wsDial(t, w.url, w.pool, h)
	if err == nil {
		return c, nil
	}
	if resp == nil {
		t.Fatalf("connect: %v", err)
	}
	var body struct {
		Error *proto.Error `json:"error"`
	}
	b, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(b, &body) != nil || body.Error == nil {
		t.Fatalf("connect refused %s with %q, not a refusal of the protocol's", resp.Status, b)
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("connect refused with %s, want 401", resp.Status)
	}
	return nil, body.Error
}

// call sends one request on c and returns its answer.
func wsCall(t *testing.T, c *wsconn.Conn, id uint64, op string, args any) *proto.Frame {
	t.Helper()
	s := wsconn.Stream(c)
	req, err := proto.Request(id, op, args)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.NewEncoder(s).Encode(req); err != nil {
		t.Fatalf("send %s: %v", op, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(patience))
	f, err := proto.NewDecoder(s).Decode()
	if err != nil {
		t.Fatalf("read the answer to %s: %v", op, err)
	}
	return f
}

// helloAs sends a hello for projA in session on c and returns the
// welcome.
func helloAs(t *testing.T, c *wsconn.Conn, session string) *proto.Frame {
	t.Helper()
	return helloIn(t, c, projA, session)
}

// helloIn sends a hello for project in session on c and returns the
// welcome.
func helloIn(t *testing.T, c *wsconn.Conn, project, session string) *proto.Frame {
	t.Helper()
	s := wsconn.Stream(c)
	if err := proto.NewEncoder(s).Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: session, Machine: "laptop-a"}); err != nil {
		t.Fatalf("send the hello: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(patience))
	f, err := proto.NewDecoder(s).Decode()
	if err != nil {
		t.Fatalf("read the welcome: %v", err)
	}
	return f
}

// TestSignIn: an invited person signs in through the server, and the
// token, with a proof by its key, is a connection of theirs.
func TestSignIn(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	ans := w.mustSignIn(t, a)
	if ans.TokenType != "DPoP" || ans.Principal != "alice" || ans.Scope != "cli mcp" || ans.Grant == "" ||
		ans.ExpiresAt != w.clk.now().Add(DefaultTokenLifetime).UTC().Truncate(time.Second).Format(time.RFC3339) {
		t.Errorf("sign-in = %+v, want alice's DPoP token for the day", ans)
	}
	if len(ans.AccessToken) < 52 {
		t.Errorf("the token is %d characters, want at least 52: 256 random bits in base32", len(ans.AccessToken))
	}
	ids, err := w.ctl.Identities(t.Context(), "alice")
	if err != nil || len(ids) != 1 || !ids[0].Bound() || ids[0].Issuer != w.iss.URL {
		t.Errorf("alice's identities after signing in = %+v, %v; want her invitation bound", ids, err)
	}
	c, perr := w.connectWith(t, a, ans.AccessToken, "")
	if perr != nil {
		t.Fatalf("connect with the token = %v", perr)
	}
	if wel := helloAs(t, c, proto.CLISession); wel.Err != nil || wel.Principal != "alice" {
		t.Fatalf("the welcome = %+v, %v; want alice's", wel, wel.Err)
	}
	if f := wsCall(t, c, 1, proto.OpCreate, proto.CreateArgs{Title: "signed in"}); f.Err != nil {
		t.Errorf("create, signed in = %v", f.Err)
	}
	// The sign-in names no address, token or subject in the log.
	for _, secret := range []string{ans.AccessToken, "alice@example.com", "00000000-0000-4000-8000-00000000a11c"} {
		if strings.Contains(w.log.String(), secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
	if !strings.Contains(w.log.String(), "event=signin.granted") {
		t.Errorf("the log has no signin.granted line:\n%s", w.log.String())
	}
	// The audit stream has the binding and the grant, each the person's.
	for _, ev := range []string{"audit.identity.bind", "audit.signin.grant"} {
		if !regexp.MustCompile(`event=` + regexp.QuoteMeta(ev) + ` audit=true .* principal=alice session=signin .* subject=alice`).MatchString(w.log.String()) {
			t.Errorf("the audit stream has no %s line for alice, as subject:\n%s", ev, w.log.String())
		}
	}
}

// TestSignInAnnounced: each sign-in is announced to the principal's
// inbox in every project it is a member of (identity.md §13), with the
// provider, the flow, the client, the machine and how to end it, and
// with no token, address, subject or issuer. A project it is not a
// member of, or one archived, is not even opened.
func TestSignInAnnounced(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	w.project(t, projB, "alice")
	w.project(t, projC, "alice")
	if _, err := w.ctl.ArchiveProject(t.Context(), dana, projC); err != nil {
		t.Fatal(err)
	}
	const projD = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	w.project(t, projD, "bob")
	opened := map[string]int{projC: w.opened(projC), projD: w.opened(projD)}
	a := newAttempt(t)
	ans := w.mustSignIn(t, a)

	for _, project := range []string{projA, projB} {
		c, perr := w.connectWith(t, a, ans.AccessToken, "")
		if perr != nil {
			t.Fatalf("connect with the token = %v", perr)
		}
		if wel := helloIn(t, c, project, proto.CLISession); wel.Err != nil {
			t.Fatalf("the welcome to %s = %v", project, wel.Err)
		}
		f := wsCall(t, c, 1, proto.OpInbox, proto.InboxArgs{})
		if f.Err != nil {
			t.Fatalf("inbox in %s = %v", project, f.Err)
		}
		var page proto.InboxResult
		if err := json.Unmarshal(f.OK, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Unread != 1 {
			t.Fatalf("alice's inbox in %s after signing in = %+v, want one unread item", project, page)
		}
		it := page.Items[0]
		if it.Kind != "signin.granted" || it.From != "ariesd" || it.Issue != "" || it.Session != "" {
			t.Errorf("the item in %s = %+v, want signin.granted from ariesd, for every session, naming no issue", project, it)
		}
		for _, want := range []string{"corp", store.FlowLoopback, ClientCLI, "laptop-a", "`aries admin signin end " + ans.Grant + "`", "`aries logout`"} {
			if !strings.Contains(it.Body, want) {
				t.Errorf("the item in %s says %q, which does not name %q", project, it.Body, want)
			}
		}
		for _, secret := range []string{ans.AccessToken, "alice@example.com", "00000000-0000-4000-8000-00000000a11c", w.iss.URL} {
			if strings.Contains(it.Body, secret) {
				t.Errorf("the item in %s says %q, which holds %q", project, it.Body, secret)
			}
		}
	}
	for id, before := range opened {
		if n := w.opened(id) - before; n != 0 {
			t.Errorf("the store of %s, archived or not alice's, was opened %d times for her sign-in, want 0", id, n)
		}
	}
	if strings.Contains(w.log.String(), "signin.announce_failed") {
		t.Errorf("the log has an announcement that failed:\n%s", w.log.String())
	}
}

// TestSignInOffline is transport.md §8.2's offline provider at the
// server: every endpoint written out and the keys from a file, and a
// whole sign-in fetches nothing from the provider but the code's
// exchange.
func TestSignInOffline(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, func(iss *oidctest.Issuer, ps *oidc.Settings, _ *Login) {
		jwks := t.TempDir() + "/jwks.json"
		if err := os.WriteFile(jwks, iss.JWKS(), 0o600); err != nil {
			t.Fatal(err)
		}
		ps.Discovery, ps.AuthorizationEndpoint, ps.TokenEndpoint, ps.JWKSFile = oidc.DiscoveryStatic, iss.URL+"/authorize", iss.URL+"/token", jwks
	})
	w.mustSignIn(t, newAttempt(t))
	if d, k := w.iss.Discovery.Load(), w.iss.Keys.Load(); d+k != 0 {
		t.Errorf("an offline sign-in read the discovery document %d times and the keys %d times, want neither", d, k)
	}
	if n := w.iss.Tokens.Load(); n != 1 {
		t.Errorf("an offline sign-in called the token endpoint %d times, want once", n)
	}
}

func TestSignInRefusals(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	w.member(t, projA, "bob")
	for _, tc := range []struct {
		name   string
		before func()
		after  func()
		status int
		error  string
		code   proto.Code
		names  string
	}{
		{"an uninvited person", func() {
			w.iss.SignIn(oidctest.Person{Subject: "bob-sub", Email: "bob@example.com", Verified: true})
		}, nil, http.StatusForbidden, "access_denied", proto.CodeForbidden, "no invitation for bob@example.com at corp"},
		{"prompt=login's auth_time too old", func() { w.iss.AuthAge(20 * time.Minute) }, func() { w.iss.AuthAge(0) },
			http.StatusForbidden, "access_denied", proto.CodeAuth, "not fresh"},
		{"an unverified address", func() {
			w.iss.SignIn(oidctest.Person{Subject: "alice-2", Email: "alice@example.com", Verified: false})
		}, nil, http.StatusForbidden, "access_denied", proto.CodeForbidden, "verified"},
	} {
		// Serial: the cases share the world's stand-in, whose person each
		// sets.
		tc.before()
		ans := w.signIn(t, newAttempt(t))
		if tc.after != nil {
			tc.after()
		}
		w.iss.SignIn(oidctest.Person{Subject: "00000000-0000-4000-8000-00000000a11c", Email: "alice@example.com", Verified: true})
		if ans.Status != tc.status || ans.Error != tc.error || proto.Code(ans.Code) != tc.code || !strings.Contains(ans.Description, tc.names) || ans.Fix == "" {
			t.Errorf("%s: %+v, want %d %s, code %s naming %q, with a fix", tc.name, ans, tc.status, tc.error, tc.code, tc.names)
		}
		if ans.AccessToken != "" {
			t.Errorf("%s: a token was issued", tc.name)
		}
	}
}

// TestTokenEndpointRefusals: the code is spent once, with its own
// verifier, and with a DPoP proof.
func TestTokenEndpointRefusals(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	code := w.browse(t, w.authorizeURL(a)).Get("code")
	if ans := w.exchange(t, a, code, "-"); ans.Error != "invalid_dpop_proof" || ans.AccessToken != "" {
		t.Errorf("a code spent with no DPoP proof = %+v, want invalid_dpop_proof", ans)
	}
	other := newAttempt(t)
	other.key = a.key
	if ans := w.exchange(t, other, code, ""); ans.Error != "invalid_grant" {
		t.Errorf("a code spent with another verifier = %+v, want invalid_grant", ans)
	}
	// Each refusal above spent the code too.
	if ans := w.exchange(t, a, code, ""); ans.Error != "invalid_grant" {
		t.Errorf("a code spent a second time = %+v, want invalid_grant", ans)
	}
	a2 := newAttempt(t)
	code = w.browse(t, w.authorizeURL(a2)).Get("code")
	proof := w.proof(t, a2.key, http.MethodPost, w.url+pathToken, "")
	if ans := w.exchange(t, a2, code, proof); ans.AccessToken == "" {
		t.Fatalf("a good exchange = %+v", ans)
	}
	a3 := newAttempt(t)
	a3.key = a2.key
	code = w.browse(t, w.authorizeURL(a3)).Get("code")
	if ans := w.exchange(t, a3, code, proof); ans.Error != "invalid_dpop_proof" {
		t.Errorf("a DPoP proof replayed at the token endpoint = %+v, want invalid_dpop_proof", ans)
	}
}

// TestConnectRefusals holds /v1/connect's token check to transport.md
// §6.4: no proof, a proof by another key, a proof replayed, a proof for
// another URL, a token that ended or never was, all refused 401 before
// the upgrade.
func TestConnectRefusals(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	tok := w.mustSignIn(t, a).AccessToken
	foreign := newAttempt(t)
	replayed := w.proof(t, a.key, http.MethodGet, w.url+keyproof.ConnectPath, tok)
	if _, perr := w.connectWith(t, a, tok, replayed); perr != nil {
		t.Fatalf("a first proof = %v", perr)
	}
	elsewhere := w.proof(t, a.key, http.MethodGet, "https://aries.example.com"+keyproof.ConnectPath, tok)
	for _, tc := range []struct {
		name, token, proof, names string
		code                      proto.Code
	}{
		{"no proof", tok, "-", "none was sent", proto.CodeAuth},
		{"a proof by another key", tok, w.proof(t, foreign.key, http.MethodGet, w.url+keyproof.ConnectPath, tok), "another key", proto.CodeAuth},
		{"a proof replayed", tok, replayed, "used already", proto.CodeAuth},
		{"a proof for another server's URL", tok, elsewhere, "another URL", proto.CodeAuth},
		{"a token never issued", rand.Text() + rand.Text(), "", "unknown", proto.CodeAuth},
	} {
		// In turn: the test holds a Postgres server (CONTRIBUTING.md).
		t.Run(tc.name, func(t *testing.T) {
			c, perr := w.connectWith(t, a, tc.token, tc.proof)
			if c != nil || perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.names) || !strings.Contains(perr.Fix, "aries login") {
				t.Errorf("connect with %s = %v, want %s naming %q with a fix to sign in", tc.name, perr, tc.code, tc.names)
			}
		})
	}
	// A bearer presentation of a DPoP-bound token.
	c, resp, err := wsDial(t, w.url, w.pool, http.Header{"Authorization": {"Bearer " + tok}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a DPoP token presented as Bearer = %v, %v; want 401", c, err)
	}
}

// TestMetricsOfTokens: every sign-in's token presented to /v1/connect
// is counted by how its check ended, as a key proof is, and a refused
// one as a refused upgrade; a connection that brought a token proves no
// key.
func TestMetricsOfTokens(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, false)
	w := newMeasuredSignInWorld(t, nil, tel)
	a := newAttempt(t)
	tok := w.mustSignIn(t, a).AccessToken
	replayed := w.proof(t, a.key, http.MethodGet, w.url+keyproof.ConnectPath, tok)
	c, perr := w.connectWith(t, a, tok, replayed)
	if perr != nil {
		t.Fatalf("connect with a sign-in = %v", perr)
	}
	_ = c.Close()
	for _, tc := range []struct{ name, token, proof string }{
		{"no proof", tok, "-"},
		{"a proof replayed", tok, replayed},
		{"a proof by another key", tok, w.proof(t, newAttempt(t).key, http.MethodGet, w.url+keyproof.ConnectPath, tok)},
		{"a token never issued", rand.Text() + rand.Text(), ""},
	} {
		if _, perr := w.connectWith(t, a, tc.token, tc.proof); perr == nil {
			t.Errorf("connect with %s was not refused", tc.name)
		}
	}
	if _, resp, err := wsDial(t, w.url, w.pool, http.Header{"Authorization": {"Bearer " + tok}}); err == nil || resp == nil {
		t.Errorf("a DPoP token presented as Bearer = %v; want a refusal", err)
	}
	checkSoon(t, m, []want{
		{"aries.https.tokens", []string{"result", "ok"}, 1},
		{"aries.https.tokens", []string{"result", "dpop_missing"}, 1},
		{"aries.https.tokens", []string{"result", "dpop_replayed"}, 1},
		{"aries.https.tokens", []string{"result", "dpop_key"}, 1},
		{"aries.https.tokens", []string{"result", "token_unknown"}, 1},
		{"aries.https.tokens", []string{"result", "token_scheme"}, 1},
		{"aries.https.upgrades", []string{"result", "refused", "reason", "token"}, 5},
		{"aries.https.upgrades", []string{"result", "ok", "reason", "none"}, 1},
		{"aries.https.key_proofs", []string{"result", "ok"}, 0},
	})
}

// proof is a DPoP proof by k of a request to htu, made at the world's
// time, which the server checks it against.
func (w *signInWorld) proof(t *testing.T, k *ecdsa.PrivateKey, method, htu, token string) string {
	t.Helper()
	p, err := dpop.Proof(k, method, htu, token, w.clk.now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestConnectExpired: a token whose sign-in has ended is refused, on the
// store's time.
func TestConnectExpired(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	tok := w.mustSignIn(t, a).AccessToken
	w.clk.advance(DefaultTokenLifetime)
	_, perr := w.connectWith(t, a, tok, "")
	if perr == nil || perr.Code != proto.CodeAuth || !strings.Contains(perr.Message, "ended at") {
		t.Errorf("connect with a token whose day has passed = %v, want auth naming its end", perr)
	}
}

// TestScopes: a sign-in of scope mcp alone, as an agent's would be,
// takes no session cli, sends no admin op and decides no gate.
func TestScopes(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	a.scope = "mcp"
	ans := w.mustSignIn(t, a)
	if ans.Scope != "mcp" {
		t.Fatalf("a sign-in for mcp = scope %q", ans.Scope)
	}
	c, perr := w.connectWith(t, a, ans.AccessToken, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if wel := helloAs(t, c, proto.CLISession); wel.Err == nil || wel.Err.Code != proto.CodeForbidden {
		t.Errorf("hello in the session cli with scope mcp = %+v, want forbidden", wel)
	}
	c, perr = w.connectWith(t, a, ans.AccessToken, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if wel := helloAs(t, c, "agent-1"); wel.Err != nil {
		t.Fatalf("hello in an agent's session with scope mcp = %v", wel.Err)
	}
	if f := wsCall(t, c, 1, proto.OpCreate, proto.CreateArgs{Title: "an agent's"}); f.Err != nil {
		t.Errorf("create with scope mcp = %v", f.Err)
	}
	for _, op := range []string{proto.OpGatePass, proto.OpGateWaive, proto.OpStatus, proto.OpKeyAdd, proto.OpLockAcquire} {
		f := wsCall(t, c, 2, op, map[string]any{})
		if f.Err == nil || f.Err.Code != proto.CodeForbidden || !strings.Contains(f.Err.Message, op) {
			t.Errorf("%s with scope mcp = %+v, want forbidden naming it", op, f.Err)
		}
	}
}

// TestCIScopeNeverPassesAGate (decision 68): a CI job's sign-in reports
// its check from the session ci, and passes no person's gate.
func TestCIScopeNeverPassesAGate(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	ids, _ := w.ctl.Identities(t.Context(), "alice")
	// A ci sign-in, as slice N5's token exchange will make for a service
	// principal; here made in the store by hand, for alice.
	w.mustSignIn(t, newAttempt(t))
	tok := rand.Text() + rand.Text()
	if _, err := w.ctl.SignIn(t.Context(), store.SignIn{Provider: "corp", Issuer: w.iss.URL, Subject: "00000000-0000-4000-8000-00000000a11c",
		Address: "alice@example.com", ClientID: "ci", Scopes: []string{proto.ScopeCI}, JKT: dpop.Thumbprint(&k.PublicKey), Resource: w.url,
		Flow: store.FlowWorkload, Machine: "runner", Lifetime: time.Hour, TokenHash: hashOf(tok)}); err != nil || len(ids) != 1 {
		t.Fatal(err)
	}
	a := &attempt{key: k}
	c, perr := w.connectWith(t, a, tok, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if wel := helloAs(t, c, proto.CISession); wel.Err != nil {
		t.Fatalf("hello in the session ci with scope ci = %v", wel.Err)
	}
	created := wsCall(t, c, 1, proto.OpCreate, proto.CreateArgs{Title: "gated"})
	var cr proto.CreateResult
	if created.Err != nil || json.Unmarshal(created.OK, &cr) != nil {
		t.Fatalf("create = %+v", created)
	}
	added := wsCall(t, c, 2, proto.OpGateAdd, proto.GateAddArgs{Blocks: cr.ID, Kind: proto.GateHuman, Approvers: []string{"alice"}})
	var g proto.Gate
	if added.Err != nil || json.Unmarshal(added.OK, &g) != nil {
		t.Fatalf("gate.add = %+v", added)
	}
	pass := wsCall(t, c, 3, proto.OpGatePass, proto.GateArgs{ID: g.ID})
	if pass.Err == nil || pass.Err.Code != proto.CodeForbidden {
		t.Errorf("gate.pass of a person's gate with scope ci = %+v, want forbidden", pass.Err)
	}
	if f := wsCall(t, c, 4, proto.OpGateWaive, proto.GateArgs{ID: g.ID, Reason: "no"}); f.Err == nil || f.Err.Code != proto.CodeForbidden {
		t.Errorf("gate.waive with scope ci = %+v, want forbidden", f.Err)
	}
}

// TestLogoutClosesConnections (decision 59): a revoked token's
// connections are closed at once, each told why.
func TestLogoutClosesConnections(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	tok := w.mustSignIn(t, a).AccessToken
	c, perr := w.connectWith(t, a, tok, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if wel := helloAs(t, c, proto.CLISession); wel.Err != nil {
		t.Fatal(wel.Err)
	}
	resp, err := w.browser.PostForm(w.url+pathRevoke, url.Values{"token": {tok}, "client_id": {ClientCLI}})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
	expectEnded(t, c, "logout")
	if _, perr := w.connectWith(t, a, tok, ""); perr == nil || perr.Code != proto.CodeAuth {
		t.Errorf("connect with a revoked token = %v, want auth", perr)
	}
	// Revoking a token no one has is not refused (RFC 7009).
	resp, err = w.browser.PostForm(w.url+pathRevoke, url.Values{"token": {"nonesuch"}})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("revoke of an unknown token = %v, %v; want 200", resp, err)
	}
	_ = resp.Body.Close()
}

// expectEnded reads c until the server's last frame, which must say the
// sign-in ended, naming why, and then the end of the connection.
func expectEnded(t *testing.T, c *wsconn.Conn, why string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(patience))
	f, err := proto.NewDecoder(wsconn.Stream(c)).Decode()
	if err != nil {
		t.Fatalf("read the last frame: %v", err)
	}
	if f.ID != 0 || f.Err == nil || f.Err.Code != proto.CodeAuth || !strings.Contains(f.Err.Message, why) || !strings.Contains(f.Err.Fix, "aries login") {
		t.Fatalf("the last frame = %+v, %v; want auth naming %q, with a fix to sign in", f, f.Err, why)
	}
	if _, err := c.Read(); err == nil {
		t.Error("the connection stayed open after its sign-in ended")
	}
}

// TestSignInEndClosesConnections: a connection closes at its sign-in's
// end, without a request (decision 59).
func TestSignInEndClosesConnections(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, func(_ *oidctest.Issuer, _ *oidc.Settings, l *Login) { l.TokenLifetime = 2 * time.Second })
	a := newAttempt(t)
	tok := w.mustSignIn(t, a).AccessToken
	c, perr := w.connectWith(t, a, tok, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if wel := helloAs(t, c, proto.CLISession); wel.Err != nil {
		t.Fatal(wel.Err)
	}
	expectEnded(t, c, "ended at")
}

// TestIdentityRemoveEndsSignIns: removing an identity ends its sign-ins
// and closes their connections.
func TestIdentityRemoveEndsSignIns(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	tok := w.mustSignIn(t, a).AccessToken
	c, perr := w.connectWith(t, a, tok, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if wel := helloAs(t, c, proto.CLISession); wel.Err != nil {
		t.Fatal(wel.Err)
	}
	ids, _ := w.ctl.Identities(t.Context(), "alice")
	admin := mustDialTo(t, w.Server, dana, projA)
	mustAsk[proto.IdentityResult](admin, proto.OpIdentityRemove, proto.IdentityArgs{ID: ids[0].ID})
	expectEnded(t, c, "identity removed")
	if _, perr := w.connectWith(t, a, tok, ""); perr == nil {
		t.Error("connect with a sign-in of a removed identity = nil error")
	}
}

// expectClosed reads c until the server closes it, and fails if it is
// still open after patience.
func expectClosed(t *testing.T, c *wsconn.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(patience))
	for {
		_, err := c.Read()
		if errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatal("the connection is still open")
		}
		if err != nil {
			return
		}
	}
}

// signedInConn signs a in, connects with its token and says hello in
// the session cli, and returns the connection and the token endpoint's
// answer.
func (w *signInWorld) signedInConn(t *testing.T, a *attempt) (*wsconn.Conn, tokenAnswer) {
	t.Helper()
	ans := w.mustSignIn(t, a)
	c, perr := w.connectWith(t, a, ans.AccessToken, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if wel := helloAs(t, c, proto.CLISession); wel.Err != nil {
		t.Fatal(wel.Err)
	}
	return c, ans
}

// TestIdentityRemoveOwnClosesOwn: a server admin who removes the
// identity behind their own sign-in is answered, and then their own
// connection closes, as signin.end's does.
func TestIdentityRemoveOwnClosesOwn(t *testing.T) {
	t.Parallel()
	w := newMeasuredSignInWorld(t, nil, nil, "alice")
	c, _ := w.signedInConn(t, newAttempt(t))
	ids, err := w.ctl.Identities(t.Context(), "alice")
	if err != nil || len(ids) != 1 {
		t.Fatalf("alice's identities = %+v, %v; want one", ids, err)
	}
	if f := wsCall(t, c, 1, proto.OpIdentityRemove, proto.IdentityArgs{ID: ids[0].ID}); f.Err != nil {
		t.Fatalf("identity.remove of alice's own identity, by alice = %v", f.Err)
	}
	expectClosed(t, c)
}

// TestDisableEndsSignIns: disabling a principal ends its sign-ins, so
// that a token from before is refused once it is enabled again.
func TestDisableEndsSignIns(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	c, ans := w.signedInConn(t, a)
	admin := mustDialTo(t, w.Server, dana, projA)
	mustAsk[proto.PrincipalResult](admin, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "alice"})
	expectClosed(t, c)
	mustAsk[proto.PrincipalResult](admin, proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "alice"})
	if c, perr := w.connectWith(t, a, ans.AccessToken, ""); perr == nil || perr.Code != proto.CodeAuth {
		if c != nil {
			_ = c.Close()
		}
		t.Errorf("connect, once alice is enabled again, with her token from before she was disabled = %v, want auth", perr)
	}
	gs := mustAsk[proto.SignInsResult](admin, proto.OpSignIns, proto.SignInsArgs{Name: "alice"})
	if len(gs.SignIns) != 1 || gs.SignIns[0].State != proto.SignInEnded || gs.SignIns[0].EndReason != store.EndPrincipalDisabled {
		t.Errorf("alice's sign-ins after disable and enable = %+v, want hers ended, reason %s", gs.SignIns, store.EndPrincipalDisabled)
	}
}

// TestSweepClosesEndedSignIns: a sign-in the control store ended
// without the server closing its connections, as when the write that
// ended it failed after it committed and the index was read again, is
// closed by the reaper's sweep.
func TestSweepClosesEndedSignIns(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	c, ans := w.signedInConn(t, newAttempt(t))
	byKey := mustDialTo(t, w.Server, alice, projA)
	// The store alone: the server is told nothing.
	if _, _, err := w.ctl.EndSignIn(t.Context(), dana, ans.Grant, store.EndRevoked); err != nil {
		t.Fatal(err)
	}
	if f := wsCall(t, c, 1, proto.OpSignIns, proto.SignInsArgs{Name: "alice"}); f.Err != nil {
		t.Fatalf("a request before the sweep = %v", f.Err)
	}
	w.sweepSignIns(t.Context())
	expectEnded(t, c, "sign-in ended")
	if _, perr := ask[proto.SignInsResult](byKey, proto.OpSignIns, proto.SignInsArgs{Name: "alice"}); perr != nil {
		t.Errorf("a request on alice's connection by a key after the sweep = %v, want an answer", perr)
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	good, _ := url.Parse(w.authorizeURL(a))
	with := func(k, v string) string {
		q := good.Query()
		q.Set(k, v)
		u := *good
		u.RawQuery = q.Encode()
		return u.String()
	}
	// Not aries's request: a page, never a redirect.
	for _, u := range []string{with("client_id", "someone"), with("redirect_uri", "https://evil.example.com/callback"),
		with("redirect_uri", "http://192.0.2.1:53682/callback")} {
		resp, err := w.browser.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("GET %s = %s, Location %q; want 400 and no redirect", u, resp.Status, resp.Header.Get("Location"))
		}
	}
	// aries's request, wrong: back to its redirect, with the error.
	for _, tc := range []struct{ u, err string }{
		{with("code_challenge_method", "plain"), "invalid_request"},
		{with("scope", "cli ci"), "invalid_scope"},
		{with("scope", " "), "invalid_scope"},
		{with("provider", "nonesuch"), "invalid_request"},
		{with("resource", "https://aries.example.com"), "invalid_target"},
	} {
		back := w.browse(t, tc.u)
		if back.Get("error") != tc.err || back.Get("state") != a.state || back.Get("iss") != w.url || back.Get("code") != "" {
			t.Errorf("GET %s = %v, want error %s with the state and iss", tc.u, back, tc.err)
		}
	}
}

// TestTokenResponseNotStored: the token endpoint's answers, a token's
// above all, are kept by no cache (RFC 6749 §5.1).
func TestTokenResponseNotStored(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	a := newAttempt(t)
	code := w.browse(t, w.authorizeURL(a)).Get("code")
	proof := w.proof(t, a.key, http.MethodPost, w.url+pathToken, "")
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {loopbackRedirect}, "client_id": {ClientCLI},
		"code_verifier": {a.verifier}}
	req, _ := http.NewRequest(http.MethodPost, w.url+pathToken, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("DPoP", proof)
	resp, err := w.browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("Pragma") != "no-cache" {
		t.Errorf("POST %s = %s, Cache-Control %q, Pragma %q; want 200, no-store and no-cache", pathToken, resp.Status,
			resp.Header.Get("Cache-Control"), resp.Header.Get("Pragma"))
	}
}

// TestProviderAnswerIssuer: an iss on the provider's answer (RFC 9207)
// must be the provider's; another is a mix-up of providers, and the
// server neither spends its code nor signs anyone in. A provider that
// sends none is taken, as most do.
func TestProviderAnswerIssuer(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	w.iss.AnswerIssuer(w.iss.URL)
	w.mustSignIn(t, newAttempt(t))
	for _, other := range []string{"https://sso.example.com", w.iss.URL + "/"} {
		w.iss.AnswerIssuer(other)
		before := w.iss.Tokens.Load()
		ans := w.signIn(t, newAttempt(t))
		if ans.AccessToken != "" || ans.Error != "access_denied" || proto.Code(ans.Code) != proto.CodeAuth ||
			!strings.Contains(ans.Description, "iss") || ans.Fix == "" {
			t.Errorf("a sign-in answered with iss %s = %+v, want access_denied, auth naming iss, with a fix", other, ans)
		}
		if n := w.iss.Tokens.Load() - before; n != 0 {
			t.Errorf("a sign-in answered with iss %s spent its code at the provider %d times, want none", other, n)
		}
	}
}

// TestSignInAtAnotherPublicURL: aries configured with any of the
// server's public URLs names it as the resource, and is answered with it
// as iss, which it requires to be its own URL (RFC 9207); the sign-in is
// for that URL.
func TestSignInAtAnotherPublicURL(t *testing.T) {
	t.Parallel()
	var second string
	w := newSignInWorldWith(t, Limits{}, nil, nil, func(h *HTTPS) {
		second = strings.Replace(h.URLs[0], "127.0.0.1", "localhost", 1)
		h.URLs = append(h.URLs, second)
	})
	a := newAttempt(t)
	u, _ := url.Parse(w.authorizeURL(a))
	q := u.Query()
	q.Set("resource", second)
	u.RawQuery = q.Encode()
	back := w.browse(t, u.String())
	if back.Get("iss") != second || back.Get("code") == "" {
		t.Fatalf("sign-in for resource %s = %v, want a code with iss %s", second, back, second)
	}
	if ans := w.exchange(t, a, back.Get("code"), ""); ans.AccessToken == "" {
		t.Fatalf("the exchange = %+v, want a token", ans)
	}
	gs, err := w.ctl.Grants(t.Context(), "alice")
	if err != nil || len(gs) != 1 || gs[0].Resource != second {
		t.Errorf("alice's sign-ins = %+v, %v; want one for %s", gs, err, second)
	}
	// A refusal goes back with the requested URL as iss too.
	q.Set("scope", "root")
	u.RawQuery = q.Encode()
	if back := w.browse(t, u.String()); back.Get("error") != "invalid_scope" || back.Get("iss") != second {
		t.Errorf("a refused sign-in for resource %s = %v, want invalid_scope with iss %s", second, back, second)
	}
}

func TestWellKnownSignIn(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	resp, err := w.browser.Get(w.url + "/.well-known/aries")
	if err != nil {
		t.Fatal(err)
	}
	var wk WellKnown
	_ = json.NewDecoder(resp.Body).Decode(&wk)
	_ = resp.Body.Close()
	if strings.Join(wk.Auth, " ") != "key signin" || len(wk.Providers) != 1 || wk.Providers[0] != (WellProvider{Name: "corp", Kind: "oidc"}) {
		t.Errorf("/.well-known/aries = %+v, want key and signin, and corp", wk)
	}
	resp, err = w.browser.Get(w.url + pathASMeta)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	_ = resp.Body.Close()
	if meta["issuer"] != w.url || meta["token_endpoint"] != w.url+pathToken || meta["authorization_response_iss_parameter_supported"] != true {
		t.Errorf("%s = %v, want this server as its issuer", pathASMeta, meta)
	}
}

// TestInviteProviderChecked: with providers configured, an invitation
// names one of them (identity.md §6.5, left to T2 by N1).
func TestInviteProviderChecked(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	admin := mustDialTo(t, w.Server, dana, projA)
	w.member(t, projA, "bob")
	_, perr := ask[proto.IdentityResult](admin, proto.OpInviteAdd, proto.InviteAddArgs{Name: "bob", Provider: "google", Address: "bob@example.com"})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "google") || !strings.Contains(perr.Fix, "corp") {
		t.Errorf("invite.add at a provider the server does not have = %+v, want invalid naming it, with a fix naming corp", perr)
	}
	if _, perr := ask[proto.IdentityResult](admin, proto.OpInviteAdd, proto.InviteAddArgs{Name: "bob", Provider: "corp", Address: "bob@example.com"}); perr != nil {
		t.Errorf("invite.add at corp = %v", perr)
	}
}

// TestSignInAdmin: a principal lists their own sign-ins and a server
// admin anyone's; an admin ends one, which closes its connections.
func TestSignInAdmin(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	w.member(t, projA, "bob")
	a := newAttempt(t)
	ans := w.mustSignIn(t, a)
	c, perr := w.connectWith(t, a, ans.AccessToken, "")
	if perr != nil {
		t.Fatal(perr)
	}
	if wel := helloAs(t, c, proto.CLISession); wel.Err != nil {
		t.Fatal(wel.Err)
	}
	own := wsCall(t, c, 1, proto.OpSignIns, proto.SignInsArgs{Name: "alice"})
	var list proto.SignInsResult
	if own.Err != nil || json.Unmarshal(own.OK, &list) != nil || len(list.SignIns) != 1 || list.SignIns[0].ID != ans.Grant ||
		list.SignIns[0].State != proto.SignInLive || list.SignIns[0].Provider != "corp" || list.SignIns[0].Machine != "laptop-a" {
		t.Fatalf("alice's own sign-ins = %s, %v; want her one, live", own.OK, own.Err)
	}
	if strings.Contains(string(own.OK), ans.AccessToken) {
		t.Error("the list of sign-ins holds the token")
	}
	bob := mustDialTo(t, w.Server, store.Actor{Principal: "bob", Session: "s-b", Machine: "m"}, projA)
	if _, perr := ask[proto.SignInsResult](bob, proto.OpSignIns, proto.SignInsArgs{Name: "alice"}); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("bob lists alice's sign-ins = %v, want forbidden", perr)
	}
	if _, perr := ask[proto.SignInEndResult](bob, proto.OpSignInEnd, proto.SignInEndArgs{ID: ans.Grant}); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("bob ends alice's sign-in = %v, want forbidden", perr)
	}
	admin := mustDialTo(t, w.Server, dana, projA)
	ended := mustAsk[proto.SignInEndResult](admin, proto.OpSignInEnd, proto.SignInEndArgs{ID: ans.Grant})
	if ended.Change != "ended" || ended.Connections != 1 || ended.State != proto.SignInEnded || ended.EndReason != store.EndRevoked {
		t.Errorf("signin.end by a server admin = %+v, want it ended, closing alice's one connection", ended)
	}
	expectEnded(t, c, "revoked")
	if again := mustAsk[proto.SignInEndResult](admin, proto.OpSignInEnd, proto.SignInEndArgs{ID: ans.Grant}); again.Change != "" {
		t.Errorf("signin.end of an ended sign-in = %+v, want no change", again)
	}
	if _, perr := ask[proto.SignInEndResult](admin, proto.OpSignInEnd, proto.SignInEndArgs{ID: "g-nonesuch"}); perr == nil || perr.Code != proto.CodeNotFound || perr.Fix == "" {
		t.Errorf("signin.end of no such sign-in = %v, want not_found with a fix", perr)
	}
}

// TestStatusSignIn: the status report counts live sign-ins and shows
// each provider, with where its keys come from.
func TestStatusSignIn(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	admin := mustDialTo(t, w.Server, dana, projA)
	before := mustAsk[proto.StatusResult](admin, proto.OpStatus, proto.StatusArgs{})
	if si := before.Server.SignIn; si == nil || si.Live != 0 || len(si.Providers) != 1 || si.Providers[0].Discovered || si.Providers[0].KeysReadAt != nil {
		t.Fatalf("status before a sign-in = %+v, want corp, not yet discovered or read", before.Server.SignIn)
	}
	w.mustSignIn(t, newAttempt(t))
	after := mustAsk[proto.StatusResult](admin, proto.OpStatus, proto.StatusArgs{})
	si := after.Server.SignIn
	if si == nil || si.Live != 1 || !si.Providers[0].Discovered || si.Providers[0].Keys != "fetch" || len(si.Providers[0].KeyIDs) != 1 ||
		si.Providers[0].KeysReadAt == nil || si.Providers[0].Error != "" {
		t.Errorf("status after a sign-in = %+v, want one live, corp discovered and its key read", si)
	}
}
