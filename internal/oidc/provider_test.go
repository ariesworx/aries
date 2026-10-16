package oidc

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/oidc/oidctest"
)

// secretFile writes the stand-in's client secret to a 0600 file.
func secretFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(p, []byte(oidctest.Secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func options(clk *clock) Options {
	return Options{Now: clk.Now, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute}
}

func challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize follows the provider's authorization URL as a browser
// whose person is signed in at the stand-in would, and returns the
// redirect's query.
func authorize(t *testing.T, iss *oidctest.Issuer, u string) url.Values {
	t.Helper()
	c := iss.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("GET %s = %s, Location %q", u, resp.Status, resp.Header.Get("Location"))
	}
	return loc.Query()
}

func TestProviderSignIn(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	clk := newClock()
	iss.Clock(clk.Now)
	p, err := Open(Settings{Name: "corp", Kind: KindOIDC, Issuer: iss.URL, ClientID: oidctest.ClientID,
		ClientSecretFile: secretFile(t), CAFile: iss.CAFile}, options(clk))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	if iss.Discovery.Load() != 0 {
		t.Errorf("Open fetched the discovery document; it is read at the first sign-in, so that a provider down at start delays nothing")
	}
	const redirect = "https://aries.example.com/auth/callback"
	verifier := "a-verifier-of-enough-length-for-pkce-0123456789abcdef"
	u, err := p.AuthorizeURL(t.Context(), AuthRequest{State: "st", Nonce: "nn", Challenge: challenge(verifier), Redirect: redirect, MaxAge: 10 * time.Minute})
	if err != nil {
		t.Fatalf("AuthorizeURL = %v", err)
	}
	pu, _ := url.Parse(u)
	q := pu.Query()
	for k, want := range map[string]string{"client_id": oidctest.ClientID, "redirect_uri": redirect, "response_type": "code",
		"state": "st", "nonce": "nn", "code_challenge_method": "S256", "prompt": "login", "max_age": "600", "scope": "openid email"} {
		if got := q.Get(k); got != want {
			t.Errorf("AuthorizeURL's %s = %q, want %q", k, got, want)
		}
	}
	back := authorize(t, iss, u)
	if back.Get("state") != "st" {
		t.Fatalf("the provider's redirect carries state %q, want st", back.Get("state"))
	}
	if _, err := p.Exchange(t.Context(), back.Get("code"), "another-verifier-of-enough-length-0123456789abcdef", redirect); err == nil {
		t.Error("Exchange(the wrong PKCE verifier) = nil error, want the provider's refusal")
	}
	back = authorize(t, iss, u)
	idt, err := p.Exchange(t.Context(), back.Get("code"), verifier, redirect)
	if err != nil {
		t.Fatalf("Exchange = %v", err)
	}
	id, err := p.Check(t.Context(), idt, Expect{Nonce: "nn", AuthAge: 10 * time.Minute})
	if err != nil {
		t.Fatalf("Check = %v", err)
	}
	if id.Issuer != iss.URL || id.Subject != "00000000-0000-4000-8000-00000000a11c" || id.Address != "alice@example.com" || !id.Verified {
		t.Errorf("Check = %+v, want alice's verified address and subject at %s", id, iss.URL)
	}
	if _, err := p.Check(t.Context(), idt, Expect{Nonce: "another"}); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Check(another sign-in's nonce) = %v, want ErrInvalidToken", err)
	}
	if s := p.Status(); s.Name != "corp" || s.Keys.Source != SourceFetch || len(s.Keys.KeyIDs) != 1 {
		t.Errorf("Status() = %+v, want corp's fetched key", s)
	}
}

// TestProviderOffline is transport.md §8.2's static discovery: every
// endpoint written out and the keys from a file. A sign-in calls the
// token endpoint, inside the network, and nothing else.
func TestProviderOffline(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	clk := newClock()
	iss.Clock(clk.Now)
	p, err := Open(Settings{Name: "corp-offline", Kind: KindOIDC, Issuer: iss.URL, ClientID: oidctest.ClientID,
		ClientSecretFile: secretFile(t), CAFile: iss.CAFile, Discovery: DiscoveryStatic,
		AuthorizationEndpoint: iss.URL + "/authorize", TokenEndpoint: iss.URL + "/token", JWKSFile: jwksFile(t, iss)}, options(clk))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	verifier := "a-verifier-of-enough-length-for-pkce-0123456789abcdef"
	u, err := p.AuthorizeURL(t.Context(), AuthRequest{State: "st", Nonce: "nn", Challenge: challenge(verifier), Redirect: "https://aries.example.com/auth/callback"})
	if err != nil {
		t.Fatal(err)
	}
	back := authorize(t, iss, u)
	idt, err := p.Exchange(t.Context(), back.Get("code"), verifier, "https://aries.example.com/auth/callback")
	if err != nil {
		t.Fatalf("Exchange = %v", err)
	}
	if _, err := p.Check(t.Context(), idt, Expect{Nonce: "nn"}); err != nil {
		t.Fatalf("Check = %v", err)
	}
	if d, k := iss.Discovery.Load(), iss.Keys.Load(); d+k != 0 {
		t.Errorf("an offline provider fetched the discovery document %d times and the key set %d times, want neither", d, k)
	}
	if s := p.Status(); s.Keys.Source != SourceFile {
		t.Errorf("Status().Keys.Source = %q, want file", s.Keys.Source)
	}
}

func TestProviderRules(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	clk := newClock()
	iss.Clock(clk.Now)
	now := clk.Now()
	claims := func(extra map[string]any) map[string]any {
		c := map[string]any{"iss": iss.URL, "sub": "s-1", "aud": oidctest.ClientID, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
			"email": "alice@example.com", "email_verified": true}
		for k, v := range extra {
			if v == nil {
				delete(c, k)
			} else {
				c[k] = v
			}
		}
		return c
	}
	for _, tc := range []struct {
		name     string
		settings Settings
		claims   map[string]any
		err      error
		verified bool
	}{
		{"the domain required", Settings{Domains: []string{"Example.com"}}, claims(nil), nil, true},
		{"another domain", Settings{Domains: []string{"example.org"}}, claims(nil), ErrNotAllowed, false},
		{"a domain required and no address", Settings{Domains: []string{"example.com"}}, claims(map[string]any{"email": nil}), ErrNotAllowed, false},
		{"a domain required and the address unverified", Settings{Domains: []string{"example.com"}}, claims(map[string]any{"email_verified": false}),
			ErrNotAllowed, false},
		{"a claim required", Settings{Require: map[string]string{"hd": "example.com"}}, claims(map[string]any{"hd": "example.com"}), nil, true},
		{"a claim required, another value", Settings{Require: map[string]string{"hd": "example.com"}}, claims(map[string]any{"hd": "example.org"}), ErrNotAllowed, false},
		{"a claim required, absent", Settings{Require: map[string]string{"hd": "example.com"}}, claims(nil), ErrNotAllowed, false},
		{"an unverified address", Settings{}, claims(map[string]any{"email_verified": false}), nil, false},
		{"a verified address given as a string", Settings{}, claims(map[string]any{"email_verified": "true"}), nil, true},
		{"no subject", Settings{}, claims(map[string]any{"sub": nil}), ErrInvalidToken, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tc.settings
			s.Name, s.Kind, s.Issuer, s.ClientID, s.CAFile = "corp", KindOIDC, iss.URL, oidctest.ClientID, iss.CAFile
			s.Discovery, s.AuthorizationEndpoint, s.TokenEndpoint, s.JWKSFile = DiscoveryStatic, iss.URL+"/authorize", iss.URL+"/token", jwksFile(t, iss)
			p, err := Open(s, options(clk))
			if err != nil {
				t.Fatal(err)
			}
			id, err := p.Check(t.Context(), iss.Sign(tc.claims), Expect{})
			if !errors.Is(err, tc.err) || (err == nil) != (tc.err == nil) {
				t.Fatalf("Check(%s) = %+v, %v; want %v", tc.name, id, err, tc.err)
			}
			if err == nil && id.Verified != tc.verified {
				t.Errorf("Check(%s).Verified = %t, want %t", tc.name, id.Verified, tc.verified)
			}
		})
	}
}

// TestEntra holds the entra kind to transport.md §8.5: an authority the
// settings name, with no default; the tenant's id in tid; the version
// configured in ver; the subject oid.
func TestEntra(t *testing.T) {
	t.Parallel()
	const tenant = "00000000-0000-4000-8000-0000000000e1"
	iss := oidctest.New(t, oidctest.Path("/"+tenant+"/v2.0"))
	authority := strings.TrimSuffix(iss.URL, "/"+tenant+"/v2.0")
	clk := newClock()
	iss.Clock(clk.Now)
	p, err := Open(Settings{Name: "entra", Kind: KindEntra, Authority: authority, Tenant: tenant, TokenVersion: 2,
		ClientID: oidctest.ClientID, ClientSecretFile: secretFile(t), CAFile: iss.CAFile}, options(clk))
	if err != nil {
		t.Fatalf("Open = %v", err)
	}
	now := clk.Now()
	claims := func(extra map[string]any) map[string]any {
		c := map[string]any{"iss": iss.URL, "sub": "per-app-sub", "oid": "00000000-0000-4000-8000-0000000000a1", "tid": tenant, "ver": "2.0",
			"aud": oidctest.ClientID, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "email": "alice@example.com", "xms_edov": true}
		for k, v := range extra {
			if v == nil {
				delete(c, k)
			} else {
				c[k] = v
			}
		}
		return c
	}
	id, err := p.Check(t.Context(), iss.Sign(claims(nil)), Expect{})
	if err != nil || id.Subject != "00000000-0000-4000-8000-0000000000a1" || id.Issuer != iss.URL || !id.Verified {
		t.Fatalf("Check(an Entra token) = %+v, %v; want subject oid, its issuer, and the address verified by xms_edov", id, err)
	}
	if id, err := p.Check(t.Context(), iss.Sign(claims(map[string]any{"xms_edov": nil, "email_verified": true})), Expect{}); err != nil || id.Verified {
		t.Errorf("Check(Entra, email_verified with no xms_edov) = %+v, %v; want the address not verified: Entra's single-tenant addresses are verified by xms_edov", id, err)
	}
	for _, tc := range []struct {
		name   string
		claims map[string]any
	}{
		{"another tenant", claims(map[string]any{"tid": "00000000-0000-4000-8000-0000000000e2"})},
		{"version 1.0", claims(map[string]any{"ver": "1.0"})},
		{"no oid", claims(map[string]any{"oid": nil})},
		{"the commercial issuer", claims(map[string]any{"iss": "https://login.microsoftonline.com/" + tenant + "/v2.0"})},
	} {
		if _, err := p.Check(t.Context(), iss.Sign(tc.claims), Expect{}); err == nil {
			t.Errorf("Check(Entra, %s) = nil error, want a refusal", tc.name)
		}
	}
}

// TestAnswerIssuer: the iss of a provider's authorization responses is
// its tokens' issuer, but for Entra's version 1 tokens, whose responses
// come from the tenant's v2.0 endpoint.
func TestAnswerIssuer(t *testing.T) {
	t.Parallel()
	const tenant = "00000000-0000-4000-8000-0000000000e1"
	for _, tc := range []struct {
		name string
		s    Settings
		want string
	}{
		{"oidc", Settings{Kind: KindOIDC, Issuer: "https://sso.example.com/realms/example"}, "https://sso.example.com/realms/example"},
		{"entra, version 2", Settings{Kind: KindEntra, Authority: "https://login.example.com/", Tenant: tenant, TokenVersion: 2},
			"https://login.example.com/" + tenant + "/v2.0"},
		{"entra, version 1", Settings{Kind: KindEntra, Authority: "https://login.example.com", Tenant: tenant, TokenVersion: 1,
			Issuer: "https://sts.example.com/" + tenant + "/"}, "https://login.example.com/" + tenant + "/v2.0"},
	} {
		s := tc.s
		s.Name, s.ClientID = "corp", oidctest.ClientID
		p, err := Open(s, options(newClock()))
		if err != nil {
			t.Fatalf("Open(%s) = %v", tc.name, err)
		}
		if got := p.AnswerIssuer(); got != tc.want {
			t.Errorf("Open(%s).AnswerIssuer() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestOpenRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose-secret")
	if err := os.WriteFile(loose, []byte("s"), 0o644); err != nil { //nolint:gosec // too open, on purpose
		t.Fatal(err)
	}
	ok := Settings{Name: "corp", Kind: KindOIDC, Issuer: "https://idp.example.com/realms/example", ClientID: "aries"}
	with := func(f func(*Settings)) Settings {
		s := ok
		f(&s)
		return s
	}
	static := with(func(s *Settings) {
		s.Discovery, s.AuthorizationEndpoint, s.TokenEndpoint = DiscoveryStatic, s.Issuer+"/auth", s.Issuer+"/token"
	})
	for _, tc := range []struct {
		name string
		s    Settings
		want string
	}{
		{"no name", with(func(s *Settings) { s.Name = "" }), "name"},
		{"an uppercase name", with(func(s *Settings) { s.Name = "Corp" }), "name"},
		{"another kind", with(func(s *Settings) { s.Kind = "saml" }), "kind"},
		{"github, not yet", with(func(s *Settings) { s.Kind = "github" }), "github"},
		{"no client id", with(func(s *Settings) { s.ClientID = "" }), "client_id"},
		{"no issuer", with(func(s *Settings) { s.Issuer = "" }), "issuer"},
		{"an http issuer elsewhere", with(func(s *Settings) { s.Issuer = "http://idp.example.com" }), "https"},
		{"a secret file others can read", with(func(s *Settings) { s.ClientSecretFile = loose }), "0600"},
		{"no secret file", with(func(s *Settings) { s.ClientSecretFile = filepath.Join(dir, "missing") }), "client_secret_file"},
		{"a missing CA file", with(func(s *Settings) { s.CAFile = filepath.Join(dir, "missing.pem") }), "ca_file"},
		{"an unknown discovery", with(func(s *Settings) { s.Discovery = "guess" }), "discovery"},
		{"a key set file with discovery", with(func(s *Settings) { s.JWKSFile = filepath.Join(dir, "jwks.json") }), "static"},
		{"static with no endpoints", with(func(s *Settings) { s.Discovery = DiscoveryStatic; s.JWKSURL = s.Issuer + "/keys" }), "authorization_endpoint"},
		{"static with no keys", static, "jwks_file or jwks_url"},
		{"static with both", with(func(s *Settings) {
			*s = static
			s.JWKSFile, s.JWKSURL = filepath.Join(dir, "jwks.json"), s.Issuer+"/keys"
		}), "jwks_file or jwks_url"},
		{"static with an http endpoint", with(func(s *Settings) {
			*s = static
			s.JWKSURL, s.TokenEndpoint = s.Issuer+"/keys", "http://idp.example.com/token"
		}), "https"},
		{"entra with no authority", Settings{Name: "entra", Kind: KindEntra, Tenant: "t", TokenVersion: 2, ClientID: "a"}, "authority"},
		{"entra for every tenant", Settings{Name: "entra", Kind: KindEntra, Authority: "https://login.example.com", Tenant: "common", TokenVersion: 2, ClientID: "a"}, "tenant"},
		{"entra with no version", Settings{Name: "entra", Kind: KindEntra, Authority: "https://login.example.com", Tenant: "t", ClientID: "a"}, "token_version"},
		{"entra version 1 with no issuer", Settings{Name: "entra", Kind: KindEntra, Authority: "https://login.example.com", Tenant: "t", TokenVersion: 1, ClientID: "a"}, "issuer"},
		{"a domain that is not one", with(func(s *Settings) { s.Domains = []string{"@example.com"} }), "domains"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.name == "a secret file others can read" && runtime.GOOS == "windows" {
				t.Skip("Windows has no mode bits to check")
			}
			_, err := Open(tc.s, Options{Now: time.Now})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Open(%s) = %v, want an error naming %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestNoProviderHosts holds the package to naming no provider's host
// (transport.md §8.5): every provider's URLs are the operator's
// settings, so nothing reaches a cloud the site did not configure.
func TestNoProviderHosts(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f) //nolint:gosec // the package's own source
		if err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"microsoftonline", "windows.net", "accounts.google", "googleapis", "okta.com", "github.com/login", "api.github.com"} {
			if strings.Contains(string(b), host) {
				t.Errorf("%s names %s; a provider's hosts are the operator's settings", f, host)
			}
		}
	}
}
