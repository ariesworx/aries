package server

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/oidc/oidctest"
)

// login: is read with its defaults and bounds (identity.md §10.1,
// transport.md §8.2), and its providers as the operator writes them.
func TestResolveSettingsLogin(t *testing.T) {
	t.Parallel()
	const provider = "login:\n  providers:\n    - name: corp\n      kind: oidc\n      issuer: https://sso.example.com/realms/example\n      client_id: aries\n"
	tests := []struct {
		name  string
		body  string
		check func(LoginSettings) bool
		err   string
	}{
		{name: "the defaults", body: "", check: func(l LoginSettings) bool {
			return l.TokenLifetime == Duration(DefaultTokenLifetime) && l.MaxTokenLifetime == Duration(MaxTokenLifetime) &&
				l.FreshAuth == Duration(DefaultFreshAuth) && l.MaxTokenAge == Duration(10*time.Minute) && l.Skew == Duration(30*time.Second) &&
				l.JWKSRefetch == Duration(time.Minute) && l.PruneAfter == Duration(DefaultSignInKeep) && len(l.Providers) == 0
		}},
		{name: "a provider", body: provider, check: func(l LoginSettings) bool {
			return len(l.Providers) == 1 && l.Providers[0].Name == "corp" && l.Providers[0].ClientID == "aries"
		}},
		{name: "an offline provider", body: "login:\n  providers:\n    - name: corp\n      kind: oidc\n      issuer: https://sso.example.com\n" +
			"      client_id: aries\n      discovery: static\n      authorization_endpoint: https://sso.example.com/auth\n" +
			"      token_endpoint: https://sso.example.com/token\n      jwks_file: /etc/aries/corp-jwks.json\n      require: {hd: example.com}\n",
			check: func(l LoginSettings) bool {
				p := l.Providers[0]
				return p.Discovery == oidc.DiscoveryStatic && p.JWKSFile == "/etc/aries/corp-jwks.json" && p.Require["hd"] == "example.com"
			}},
		{name: "a day of two", body: "login:\n  token_lifetime: 2d\n", check: func(l LoginSettings) bool { return l.TokenLifetime == Duration(48*time.Hour) }},
		{name: "a lifetime past the ceiling", body: "login:\n  token_lifetime: 3d\n  max_token_lifetime: 2d\n", err: "token_lifetime is 3d; it can be at most max_token_lifetime, 2d"},
		{name: "a ceiling past a week", body: "login:\n  max_token_lifetime: 8d\n", err: "max_token_lifetime is 8d; it can be at most 7d"},
		{name: "a lifetime under a minute", body: "login:\n  token_lifetime: 30s\n", err: "token_lifetime is 30s; it must be at least 1m"},
		{name: "fresh_auth too long", body: "login:\n  fresh_auth: 2h\n", err: "fresh_auth is 2h; it must be from 1m to 1h"},
		{name: "a skew too large", body: "login:\n  skew: 10m\n", err: "skew is 10m; it can be at most 5m"},
		{name: "two providers of one name", body: provider + "    - name: corp\n      kind: oidc\n      issuer: https://sso.example.com/b\n      client_id: b\n",
			err: "two providers are named corp"},
		{name: "an unknown key of a provider", body: provider + "      secret: hunter2\n", err: "field secret not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := resolveFile(t, tc.body)
			if tc.err != "" {
				// The config reader refuses an unknown key in words of its
				// own, as for any key of the file.
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(tc.err, "not found") && !strings.Contains(err.Error(), "fix:") {
					t.Errorf("ResolveSettings(%q) = %v, want an error naming %q, with a fix", tc.body, err, tc.err)
				}
				return
			}
			if err != nil || !tc.check(s.Login) {
				t.Errorf("ResolveSettings(%q).Login = %+v, %v", tc.body, s.Login, err)
			}
		})
	}
}

// Sign-in needs a server of many projects, whose identities say who
// signs in, and HTTPS, which its tokens come over (CheckServe).
func TestCheckServeLogin(t *testing.T) {
	t.Parallel()
	const https = "https:\n  listen: 127.0.0.1:8443\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: proxy\n    proxy:\n      trusted: [127.0.0.1/32]\n"
	const provider = "login:\n  providers:\n    - name: corp\n      kind: oidc\n      issuer: https://sso.example.com\n      client_id: aries\n"
	for _, tc := range []struct{ name, body, err string }{
		{"a server of one project", https + provider, "control_schema"},
		{"SSH alone", "transports: [ssh]\ncontrol_schema: aries_control\n" + provider, "HTTPS"},
		{"both", https + "control_schema: aries_control\n" + provider, ""},
		{"no providers, no need", https, ""},
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

// OpenLogin opens the providers of login: settings, reading their files
// and fetching nothing, into what the server signs people in with.
func TestOpenLogin(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte(oidctest.Secret), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := resolveFile(t, "login:\n  token_lifetime: 12h\n  providers:\n    - name: corp\n      kind: oidc\n      issuer: "+iss.URL+
		"\n      client_id: "+oidctest.ClientID+"\n      client_secret_file: "+secret+"\n      ca_file: "+iss.CAFile+"\n")
	if err != nil {
		t.Fatal(err)
	}
	l, err := OpenLogin(s.Login, time.Now)
	if err != nil {
		t.Fatalf("OpenLogin = %v", err)
	}
	if l == nil || len(l.Providers) != 1 || l.Providers[0].Name() != "corp" || l.TokenLifetime != 12*time.Hour {
		t.Errorf("OpenLogin = %+v, want corp, signing in for 12h", l)
	}
	if n := iss.Discovery.Load() + iss.Keys.Load(); n != 0 {
		t.Errorf("OpenLogin made %d requests of the provider, want none: a provider down at start delays nothing", n)
	}
	if l, err := OpenLogin(LoginSettings{}, time.Now); l != nil || err != nil {
		t.Errorf("OpenLogin(no providers) = %+v, %v; want nil: nobody signs in", l, err)
	}
	// An identity is bound to (issuer, subject) alone, so two providers
	// of one issuer would each sign in the other's people.
	twice := s.Login
	other := twice.Providers[0]
	other.Name = "corp-b"
	twice.Providers = append(slices.Clone(twice.Providers), other)
	if _, err := OpenLogin(twice, time.Now); err == nil || !strings.Contains(err.Error(), "corp and corp-b") || !strings.Contains(err.Error(), iss.URL) ||
		!strings.Contains(err.Error(), "fix:") {
		t.Errorf("OpenLogin(two providers of issuer %s) = %v, want a refusal naming both and the issuer, with a fix", iss.URL, err)
	}
	if err := os.Chmod(secret, 0o644); err != nil { //nolint:gosec // too open, on purpose
		t.Fatal(err)
	}
	if _, err := OpenLogin(s.Login, time.Now); err == nil || !strings.Contains(err.Error(), "0600") || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("OpenLogin(a secret others can read) = %v, want a refusal naming 0600, with a fix", err)
	}
}
