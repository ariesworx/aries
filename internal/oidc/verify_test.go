package oidc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/oidc/oidctest"
)

// clock is a test's settable time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func baseConfig(iss *oidctest.Issuer, now func() time.Time) VerifierConfig {
	return VerifierConfig{Issuer: iss.URL, Audience: oidctest.ClientID, IDToken: true, Skew: 30 * time.Second,
		MaxAge: 10 * time.Minute, Refetch: time.Minute, Now: now}
}

// jwksFile writes iss's key set to a file, as an operator exports it.
func jwksFile(t *testing.T, iss *oidctest.Issuer) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, iss.JWKS(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyClaims(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	clk := newClock()
	v, err := NewFileVerifier(baseConfig(iss, clk.Now), jwksFile(t, iss))
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now()
	good := func() map[string]any {
		return map[string]any{"iss": iss.URL, "sub": "alice-sub", "aud": oidctest.ClientID, "iat": now.Unix(),
			"exp": now.Add(5 * time.Minute).Unix(), "nonce": "n-1", "auth_time": now.Add(-time.Minute).Unix()}
	}
	with := func(k string, val any) map[string]any {
		c := good()
		if val == nil {
			delete(c, k)
		} else {
			c[k] = val
		}
		return c
	}
	several := func(azp string) map[string]any {
		c := with("aud", []string{"other", oidctest.ClientID})
		c["azp"] = azp
		return c
	}
	want := Expect{Nonce: "n-1", AuthAge: 10 * time.Minute}
	for _, tc := range []struct {
		name   string
		claims map[string]any
		want   Expect
		err    error  // what the refusal wraps
		names  string // what its message names
	}{
		{"a good token", good(), want, nil, ""},
		{"another issuer", with("iss", "https://elsewhere.example.com"), want, ErrInvalidToken, "issuer"},
		{"another audience", with("aud", "someone-else"), want, ErrInvalidToken, "audience"},
		{"ours among several, issued to another party", several("someone-else"), Expect{Nonce: "n-1"}, ErrInvalidToken, "authorized party"},
		{"ours among several, issued to us", several(oidctest.ClientID), Expect{Nonce: "n-1"}, nil, ""},
		{"expired", with("exp", now.Add(-time.Minute).Unix()), want, ErrInvalidToken, "expired"},
		{"expired within the skew", with("exp", now.Add(-10*time.Second).Unix()), want, nil, ""},
		{"not yet valid", with("nbf", now.Add(time.Minute).Unix()), want, ErrInvalidToken, "nbf"},
		{"issued in the future", with("iat", now.Add(time.Minute).Unix()), want, ErrInvalidToken, "future"},
		{"issued too long ago", with("iat", now.Add(-11*time.Minute).Unix()), want, ErrInvalidToken, "ago"},
		{"no exp", with("exp", nil), want, ErrInvalidToken, "exp"},
		{"no iat", with("iat", nil), want, ErrInvalidToken, "iat"},
		{"another nonce", with("nonce", "n-2"), want, ErrInvalidToken, "nonce"},
		{"no nonce", with("nonce", nil), want, ErrInvalidToken, "nonce"},
		{"no auth_time", with("auth_time", nil), want, ErrStale, "auth_time"},
		{"an old auth_time", with("auth_time", now.Add(-20*time.Minute).Unix()), want, ErrStale, "authenticated"},
		{"ours among several, with no azp", with("aud", []string{"other", oidctest.ClientID}), Expect{Nonce: "n-1"}, ErrInvalidToken, "authorized party"},
		{"aud as a list of ours alone", with("aud", []string{oidctest.ClientID}), Expect{Nonce: "n-1"}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := v.Verify(t.Context(), iss.Sign(tc.claims), tc.want)
			if tc.err == nil {
				if err != nil || got.Sub != "alice-sub" {
					t.Errorf("Verify(%s) = %+v, %v; want sub alice-sub", tc.name, got, err)
				}
				return
			}
			if !errors.Is(err, tc.err) || !strings.Contains(err.Error(), tc.names) {
				t.Errorf("Verify(%s) = %v, want %v naming %q", tc.name, err, tc.err, tc.names)
			}
		})
	}
}

func TestVerifyRefusesForms(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	clk := newClock()
	v, err := NewFileVerifier(baseConfig(iss, clk.Now), jwksFile(t, iss))
	if err != nil {
		t.Fatal(err)
	}
	good := iss.Sign(map[string]any{"iss": iss.URL, "sub": "s", "aud": oidctest.ClientID, "iat": clk.Now().Unix(), "exp": clk.Now().Add(time.Minute).Unix()})
	parts := strings.Split(good, ".")
	for _, tc := range []struct{ name, token string }{
		{"alg none", "eyJhbGciOiJub25lIn0." + parts[1] + "."},
		{"a signature of another token", parts[0] + "." + parts[1] + "x." + parts[2]},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := v.Verify(t.Context(), tc.token, Expect{}); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("Verify(%s) = %v, want ErrInvalidToken", tc.name, err)
			}
		})
	}
}

// TestFileKeysFetchNothing holds the offline mode (transport.md §8.2):
// a verifier whose keys are a file asks the issuer for nothing, and a
// token of a key not in the file is refused, unavailable, naming the
// file.
func TestFileKeysFetchNothing(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	other := oidctest.New(t)
	clk := newClock()
	file := jwksFile(t, iss)
	v, err := NewFileVerifier(baseConfig(iss, clk.Now), file)
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now()
	claims := map[string]any{"iss": iss.URL, "sub": "s", "aud": oidctest.ClientID, "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()}
	if _, err := v.Verify(t.Context(), iss.Sign(claims), Expect{}); err != nil {
		t.Fatalf("Verify(a token of the file's key) = %v", err)
	}
	// The other issuer's key has the same id: the file's key does not
	// verify its signature.
	if _, err := v.Verify(t.Context(), other.Sign(claims), Expect{}); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("Verify(a token signed by another key of the same id) = %v, want ErrInvalidToken", err)
	}
	tok := iss.SignAs("rotated-2", claims)
	_, err = v.Verify(t.Context(), tok, Expect{})
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), file) {
		t.Errorf("Verify(a token of a key not in the file) = %v, want ErrUnavailable naming %s", err, file)
	}
	if n := iss.Discovery.Load() + iss.Keys.Load(); n != 0 {
		t.Errorf("a verifier of a key set file made %d requests to the issuer, want none", n)
	}
	if s := v.Status(); s.Source != SourceFile || len(s.KeyIDs) != 1 || s.ReadAt.IsZero() {
		t.Errorf("Status() = %+v, want a file with one key, read", s)
	}
}

// TestFileReadAgain: the operator replaces the file before the provider
// signs with a new key, and the verifier reads it again when it meets
// a key it does not know, at most once each Refetch.
func TestFileReadAgain(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	next := oidctest.New(t)
	clk := newClock()
	file := jwksFile(t, iss)
	v, err := NewFileVerifier(baseConfig(iss, clk.Now), file)
	if err != nil {
		t.Fatal(err)
	}
	now := clk.Now()
	claims := map[string]any{"iss": iss.URL, "sub": "s", "aud": oidctest.ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	rotated := next.SignAs("rotated-2", claims)
	if _, err := v.Verify(t.Context(), rotated, Expect{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify(a key not yet in the file) = %v, want ErrUnavailable", err)
	}
	if err := os.WriteFile(file, next.JWKSAs("rotated-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(t.Context(), rotated, Expect{}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Verify(the new key, within Refetch of the last read) = %v, want ErrUnavailable: the file is read at most once a minute", err)
	}
	clk.Add(time.Minute)
	if _, err := v.Verify(t.Context(), rotated, Expect{}); err != nil {
		t.Errorf("Verify(the new key, a minute after the file was replaced) = %v, want it verified", err)
	}
}

// TestFetchedKeys: keys come from the issuer's key set URL, fetched on
// the first token and again only for a key id not known, at most once
// each Refetch, so that made-up key ids cannot make the server fetch at
// will.
func TestFetchedKeys(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	clk := newClock()
	c, err := HTTPClient(iss.CAFile)
	if err != nil {
		t.Fatal(err)
	}
	v := NewFetchVerifier(baseConfig(iss, clk.Now), iss.URL+"/keys", c)
	now := clk.Now()
	claims := map[string]any{"iss": iss.URL, "sub": "s", "aud": oidctest.ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	for range 3 {
		if _, err := v.Verify(t.Context(), iss.Sign(claims), Expect{}); err != nil {
			t.Fatalf("Verify(a token of the issuer's key) = %v", err)
		}
	}
	if n := iss.Keys.Load(); n != 1 {
		t.Errorf("three tokens of one key fetched the key set %d times, want once", n)
	}
	for range 5 {
		if _, err := v.Verify(t.Context(), iss.SignAs("made-up", claims), Expect{}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("Verify(an unknown key id) = %v, want ErrUnavailable", err)
		}
	}
	if n := iss.Keys.Load(); n != 1 {
		t.Errorf("five tokens of an unknown key id within a minute of the first fetch fetched the key set %d times in all, want once", n)
	}
	clk.Add(time.Minute)
	for range 2 {
		_, _ = v.Verify(t.Context(), iss.SignAs("made-up", claims), Expect{})
	}
	if n := iss.Keys.Load(); n != 2 {
		t.Errorf("two tokens of an unknown key id a minute later fetched the key set %d times in all, want 2", n)
	}
	if s := v.Status(); s.Source != SourceFetch || s.ReadAt.IsZero() || len(s.KeyIDs) != 1 {
		t.Errorf("Status() = %+v, want fetched, one key", s)
	}
}

func TestFetchFailsUnavailable(t *testing.T) {
	t.Parallel()
	iss := oidctest.New(t)
	clk := newClock()
	// Not the issuer's CA: the fetch fails, and says unavailable.
	v := NewFetchVerifier(baseConfig(iss, clk.Now), iss.URL+"/keys", noRedirects(nil))
	now := clk.Now()
	claims := map[string]any{"iss": iss.URL, "sub": "s", "aud": oidctest.ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	_, err := v.Verify(context.Background(), iss.Sign(claims), Expect{})
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("Verify(keys that cannot be fetched) = %v, want ErrUnavailable", err)
	}
	if s := v.Status(); s.Err == "" {
		t.Errorf("Status() = %+v, want the failure", s)
	}
}

func TestNewFileVerifierRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct{ name, path, want string }{
		{"no file", filepath.Join(dir, "missing.json"), "missing.json"},
		{"not JSON", write("bad.json", "nope"), "bad.json"},
		{"no signing key", write("enc.json", `{"keys":[{"kty":"RSA","use":"enc","kid":"e","n":"AQAB","e":"AQAB"}]}`), "no signing key"},
		{"too many keys", write("many.json", `{"keys":[`+strings.Repeat(`{"kty":"oct"},`, MaxKeys)+`{"kty":"oct"}]}`), "keys"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewFileVerifier(VerifierConfig{Issuer: "https://idp.example.com", Audience: "a", Now: time.Now}, tc.path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewFileVerifier(%s) = %v, want an error naming %q", tc.name, err, tc.want)
			}
		})
	}
}
