package provider

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/spike/identity/oidc"
)

const tenant = "00000000-0000-0000-0000-0000000000aa"

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// entraStandIn serves what a Microsoft Entra ID tenant serves, at an
// authority that is not the commercial cloud's: a v2.0 discovery
// document under /TENANT/v2.0 and its keys. It stands in for a sovereign
// cloud's authority (login.microsoftonline.us, or one that exists only
// inside a disconnected network), which this spike cannot reach.
type entraStandIn struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	jwks  atomic.Int32
	caPEM string // the test server's certificate, as an operator's CA file
}

func newEntraStandIn(t *testing.T) *entraStandIn {
	t.Helper()
	e := &entraStandIn{}
	var err error
	if e.key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+tenant+"/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		a := e.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           a + "/" + tenant + "/v2.0",
			"authorization_endpoint":           a + "/" + tenant + "/oauth2/v2.0/authorize",
			"token_endpoint":                   a + "/" + tenant + "/oauth2/v2.0/token",
			"jwks_uri":                         a + "/" + tenant + "/discovery/v2.0/keys",
			"code_challenge_methods_supported": []string{"S256"},
			"tenant_region_scope":              "USGov",
		})
	})
	mux.HandleFunc("/"+tenant+"/discovery/v2.0/keys", func(w http.ResponseWriter, r *http.Request) {
		e.jwks.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{e.jwk()}})
	})
	e.srv = httptest.NewTLSServer(mux)
	t.Cleanup(e.srv.Close)
	e.caPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: e.srv.Certificate().Raw}))
	return e
}

func (e *entraStandIn) jwk() map[string]string {
	return map[string]string{"kty": "RSA", "use": "sig", "kid": "k1", "x5t": "k1", "n": enc(e.key.N.Bytes()), "e": enc(big.NewInt(int64(e.key.E)).Bytes()),
		"issuer": e.srv.URL + "/{tenantid}/v2.0"}
}

func (e *entraStandIn) issuer() string { return e.srv.URL + "/" + tenant + "/v2.0" }

// token is an Entra-shaped v2.0 ID token: oid and tid, ver 2.0.
func (e *entraStandIn) token(t *testing.T, now time.Time, edit func(map[string]any)) string {
	t.Helper()
	c := map[string]any{
		"iss": e.issuer(), "aud": "11111111-1111-1111-1111-111111111111", "tid": tenant,
		"oid": "22222222-2222-2222-2222-222222222222", "sub": "per-app-subject", "ver": "2.0",
		"preferred_username": "alice@example.com", "iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if edit != nil {
		edit(c)
	}
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
	p, _ := json.Marshal(c)
	in := enc(h) + "." + enc(p)
	d := sha256.Sum256([]byte(in))
	s, err := rsa.SignPKCS1v15(rand.Reader, e.key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + enc(s)
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func entraConfig(e *entraStandIn, caFile string) Config {
	return Config{
		Name: "entra-gov", Kind: KindEntra, Authority: e.srv.URL, Tenant: tenant, TokenVersion: 2,
		ClientID: "11111111-1111-1111-1111-111111111111", CAFile: caFile,
	}
}

// TestEntraAtAnotherAuthority: an Entra provider configured with an
// authority other than the commercial cloud's verifies that authority's
// tokens, and takes the subject from oid, not sub (identity.md §6.4).
func TestEntraAtAnotherAuthority(t *testing.T) {
	t.Parallel()
	e := newEntraStandIn(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ca := writeFile(t, "ca.pem", e.caPEM)
	p, err := Open(t.Context(), entraConfig(e, ca), func() time.Time { return now })
	if err != nil {
		t.Fatalf("Open(entra at %s) = %v", e.srv.URL, err)
	}
	tests := []struct {
		name    string
		edit    func(map[string]any)
		subject string
		errText string
	}{
		{name: "good", subject: "22222222-2222-2222-2222-222222222222"},
		{name: "the commercial cloud's issuer", edit: func(c map[string]any) { c["iss"] = "https://login.microsoftonline.com/" + tenant + "/v2.0" }, errText: "issuer"},
		{name: "another tenant", edit: func(c map[string]any) { c["tid"] = "00000000-0000-0000-0000-0000000000bb" }, errText: "tid"},
		{name: "a v1.0 token", edit: func(c map[string]any) { c["ver"] = "1.0" }, errText: "ver"},
		{name: "no oid", edit: func(c map[string]any) { delete(c, "oid") }, errText: "oid"},
		{name: "another audience", edit: func(c map[string]any) { c["aud"] = "api://other" }, errText: "audience"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id, err := p.Check(t.Context(), e.token(t, now, tc.edit), oidc.Expect{})
			if tc.errText != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errText) {
					t.Errorf("Check(%s) = %+v, %v, want an error naming %q", tc.name, id, err, tc.errText)
				}
				return
			}
			if err != nil {
				t.Fatalf("Check(%s) = %v", tc.name, err)
			}
			if id.Issuer != e.issuer() || id.Subject != tc.subject {
				t.Errorf("Check(%s) = issuer %q subject %q, want %q and %q", tc.name, id.Issuer, id.Subject, e.issuer(), tc.subject)
			}
		})
	}
}

// TestEntraKeysFromFile: with a key set file and the endpoints written
// in the settings, Open and Check reach nothing at all.
func TestEntraKeysFromFile(t *testing.T) {
	t.Parallel()
	e := newEntraStandIn(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	b, _ := json.Marshal(map[string]any{"keys": []map[string]string{e.jwk()}})
	cfg := entraConfig(e, "")
	cfg.JWKSFile = writeFile(t, "jwks.json", string(b))
	cfg.AuthorizationEndpoint = e.srv.URL + "/" + tenant + "/oauth2/v2.0/authorize"
	cfg.TokenEndpoint = e.srv.URL + "/" + tenant + "/oauth2/v2.0/token"
	e.srv.Close() // nothing may be fetched: the authority is gone
	p, err := Open(t.Context(), cfg, func() time.Time { return now })
	if err != nil {
		t.Fatalf("Open(keys from a file, the authority unreachable) = %v", err)
	}
	if _, err := p.Check(t.Context(), e.token(t, now, nil), oidc.Expect{}); err != nil {
		t.Errorf("Check = %v, want nil", err)
	}
	if got := e.jwks.Load(); got != 0 {
		t.Errorf("keys fetched %d times, want 0", got)
	}
}

func TestOpenRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		cfg     Config
		errText string
	}{
		{name: "entra with no authority", cfg: Config{Name: "e", Kind: KindEntra, Tenant: tenant, TokenVersion: 2, ClientID: "c"}, errText: "authority"},
		{name: "entra with no tenant", cfg: Config{Name: "e", Kind: KindEntra, Authority: "https://login.example.us", TokenVersion: 2, ClientID: "c"}, errText: "tenant"},
		{name: "entra with no token version", cfg: Config{Name: "e", Kind: KindEntra, Authority: "https://login.example.us", Tenant: tenant, ClientID: "c"}, errText: "token_version"},
		{name: "entra v1 with no issuer", cfg: Config{Name: "e", Kind: KindEntra, Authority: "https://login.example.us", Tenant: tenant, TokenVersion: 1, ClientID: "c"}, errText: "issuer"},
		{name: "oidc with no issuer", cfg: Config{Name: "o", Kind: KindOIDC, ClientID: "c"}, errText: "issuer"},
		{name: "plain http authority", cfg: Config{Name: "e", Kind: KindEntra, Authority: "http://login.example.us", Tenant: tenant, TokenVersion: 2, ClientID: "c"}, errText: "https"},
		{name: "a key set file without endpoints", cfg: Config{Name: "o", Kind: KindOIDC, Issuer: "https://idp.example.com", ClientID: "c", JWKSFile: "/nonexistent"}, errText: "endpoint"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Open(t.Context(), tc.cfg, time.Now); err == nil || !strings.Contains(err.Error(), tc.errText) {
				t.Errorf("Open(%s) = %v, want an error naming %q", tc.name, err, tc.errText)
			}
		})
	}
}

// TestNoCommercialDefault: nothing in the provider code names the
// commercial cloud's hosts, so a sovereign or disconnected tenant can
// never be sent to them by a default.
func TestNoCommercialDefault(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("provider.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"microsoftonline.com", "sts.windows.net", "microsoftonline.us", "accounts.google.com"} {
		if strings.Contains(string(b), host) {
			t.Errorf("provider.go names %s; every authority must come from the settings", host)
		}
	}
}
