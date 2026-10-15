package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/spike/identity/jose"
)

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// issuer is a fake OpenID Connect issuer with one RSA key.
type issuer struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	kid   string
	jwks  atomic.Int32
	other *rsa.PrivateKey
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{kid: "k1"}
	var err error
	if is.key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if is.other, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		u := is.srv.URL
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer": u, "authorization_endpoint": u + "/auth", "token_endpoint": u + "/token", "jwks_uri": u + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		is.jwks.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "use": "enc", "kid": "enc", "n": enc(is.other.N.Bytes()), "e": "AQAB"},
			{"kty": "RSA", "use": "sig", "kid": is.kid, "n": enc(is.key.N.Bytes()), "e": enc(big.NewInt(int64(is.key.E)).Bytes())},
		}})
	})
	is.srv = httptest.NewServer(mux)
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer) token(t *testing.T, kid string, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
	p, _ := json.Marshal(claims)
	in := enc(h) + "." + enc(p)
	d := sha256.Sum256([]byte(in))
	s, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + enc(s)
}

func TestVerify(t *testing.T) {
	t.Parallel()
	is := newIssuer(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	d, err := Discover(t.Context(), is.srv.Client(), is.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	base := func() map[string]any {
		return map[string]any{
			"iss": is.srv.URL, "sub": "s-1", "aud": "aries-cli", "azp": "aries-cli",
			"iat": now.Add(-30 * time.Second).Unix(), "exp": now.Add(5 * time.Minute).Unix(),
			"auth_time": now.Add(-time.Minute).Unix(), "nonce": "n-1",
		}
	}
	with := func(k string, v any) map[string]any { c := base(); c[k] = v; return c }
	without := func(k string) map[string]any { c := base(); delete(c, k); return c }
	want := Expect{Nonce: "n-1", AuthAge: 5 * time.Minute}
	otherParty := with("aud", []string{"aries-cli", "other"})
	otherParty["azp"] = "other"

	tests := []struct {
		name    string
		kid     string
		key     *rsa.PrivateKey
		claims  map[string]any
		want    Expect
		errText string // "" for success
	}{
		{"good", "k1", is.key, base(), want, ""},
		{"aud as array", "k1", is.key, with("aud", []string{"aries-cli", "other"}), want, ""},
		{"wrong issuer", "k1", is.key, with("iss", "https://evil.example.com"), want, "issuer"},
		{"wrong audience", "k1", is.key, with("aud", "another-app"), want, "audience"},
		{"azp of another party", "k1", is.key, otherParty, want, "authorized party"},
		{"expired", "k1", is.key, with("exp", now.Add(-time.Minute).Unix()), want, "expired"},
		{"issued in the future", "k1", is.key, with("iat", now.Add(time.Minute).Unix()), want, "future"},
		{"too old", "k1", is.key, with("iat", now.Add(-20*time.Minute).Unix()), want, "over"},
		{"no exp", "k1", is.key, without("exp"), want, "no exp"},
		{"wrong nonce", "k1", is.key, with("nonce", "n-2"), want, "nonce"},
		{"no nonce", "k1", is.key, without("nonce"), want, "nonce"},
		{"stale auth_time", "k1", is.key, with("auth_time", now.Add(-time.Hour).Unix()), want, "authenticated"},
		{"no auth_time", "k1", is.key, without("auth_time"), want, "auth_time"},
		{"no nonce wanted", "k1", is.key, without("nonce"), Expect{}, ""},
		{"signed by another key", "k1", is.other, base(), want, "signature"},
		{"the encryption key's kid", "enc", is.other, base(), want, "no key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := New(Config{Issuer: is.srv.URL, Audience: "aries-cli", IDToken: true, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute},
				d, is.srv.Client(), func() time.Time { return now })
			_, err := v.Verify(t.Context(), is.token(t, tc.kid, tc.key, tc.claims), tc.want)
			switch {
			case tc.errText == "" && err != nil:
				t.Errorf("Verify(%s) = %v, want nil", tc.name, err)
			case tc.errText != "" && (err == nil || !strings.Contains(err.Error(), tc.errText)):
				t.Errorf("Verify(%s) = %v, want an error containing %q", tc.name, err, tc.errText)
			}
		})
	}
}

// TestRefetchIsBounded: a stranger who sends tokens with made-up key ids
// makes the server fetch the key set once per Refetch, not once a token.
func TestRefetchIsBounded(t *testing.T) {
	t.Parallel()
	is := newIssuer(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	d, err := Discover(t.Context(), is.srv.Client(), is.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	v := New(Config{Issuer: is.srv.URL, Audience: "aries-cli", Refetch: time.Minute}, d, is.srv.Client(), func() time.Time { return now })
	for i := range 50 {
		_, _ = v.Verify(t.Context(), is.token(t, "made-up-"+string(rune('a'+i%26)), is.key, map[string]any{}), Expect{})
	}
	if got := is.jwks.Load(); got != 1 {
		t.Errorf("key set fetched %d times for 50 unknown kids within a minute, want 1", got)
	}
	now = now.Add(2 * time.Minute)
	_, _ = v.Verify(t.Context(), is.token(t, "made-up-z", is.key, map[string]any{}), Expect{})
	if got := is.jwks.Load(); got != 2 {
		t.Errorf("key set fetched %d times after the interval, want 2", got)
	}
}

func TestDiscoverRefusesAnotherIssuer(t *testing.T) {
	t.Parallel()
	is := newIssuer(t)
	if _, err := Discover(t.Context(), is.srv.Client(), is.srv.URL+"/realms/other"); err == nil {
		t.Errorf("Discover(another issuer's path) = nil error, want a refusal")
	}
}

// TestStaticKeysFetchNothing: on a network with no route to the issuer's
// key set, the operator supplies the set as a file, and the verifier
// checks tokens against it and fetches nothing, even for a key it does
// not hold (transport.md, air-gapped sign-in).
func TestStaticKeysFetchNothing(t *testing.T) {
	t.Parallel()
	is := newIssuer(t)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	set := jose.Set{Keys: []jose.JWK{{Kty: "RSA", Use: "sig", Kid: is.kid, N: enc(is.key.N.Bytes()), E: enc(big.NewInt(int64(is.key.E)).Bytes())}}}
	v, err := NewStatic(Config{Issuer: is.srv.URL, Audience: "aries-cli"}, set, func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewStatic = %v", err)
	}
	claims := map[string]any{"iss": is.srv.URL, "sub": "s-1", "aud": "aries-cli", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	if _, err := v.Verify(t.Context(), is.token(t, is.kid, is.key, claims), Expect{}); err != nil {
		t.Errorf("Verify(a token of a key in the file) = %v, want nil", err)
	}
	if _, err := v.Verify(t.Context(), is.token(t, "rotated", is.other, claims), Expect{}); err == nil || !strings.Contains(err.Error(), "key set file") {
		t.Errorf("Verify(a token of a key not in the file) = %v, want a refusal naming the key set file", err)
	}
	if got := is.jwks.Load(); got != 0 {
		t.Errorf("the issuer's key set was fetched %d times, want 0", got)
	}
}

func TestNewStaticRefusesEmptySet(t *testing.T) {
	t.Parallel()
	set := jose.Set{Keys: []jose.JWK{{Kty: "RSA", Use: "enc", Kid: "e"}}}
	if _, err := NewStatic(Config{Issuer: "https://idp.example.com", Audience: "a"}, set, time.Now); err == nil {
		t.Error("NewStatic(a set with no signing key) = nil error, want a refusal")
	}
}
