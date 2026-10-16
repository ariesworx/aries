package oidc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/jose"
)

// FuzzVerify feeds the verifier tokens of every shape, signed or not:
// it must refuse each with one of its kinds, and accept none that does
// not name its issuer and audience.
func FuzzVerify(f *testing.F) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		f.Fatal(err)
	}
	dir := f.TempDir()
	set, _ := jose.PublicJWK(&k.PublicKey, "k1")
	b := []byte(`{"keys":[{"kty":"EC","kid":"k1","crv":"P-256","x":"` + set.X + `","y":"` + set.Y + `"}]}`)
	path := filepath.Join(dir, "jwks.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		f.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cfg := VerifierConfig{Issuer: "https://idp.example.com", Audience: "aries", IDToken: true, Skew: 30 * time.Second,
		MaxAge: 10 * time.Minute, Refetch: time.Minute, Now: func() time.Time { return now }}
	v, err := NewFileVerifier(cfg, path)
	if err != nil {
		f.Fatal(err)
	}
	good, _ := jose.Sign(k, jose.Header{Kid: "k1"}, map[string]any{"iss": cfg.Issuer, "aud": "aries", "sub": "s", "iat": now.Unix(), "exp": now.Add(time.Minute).Unix()})
	f.Add(good)
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Add("..")
	f.Fuzz(func(t *testing.T, token string) {
		c, err := v.Verify(t.Context(), token, Expect{})
		if err != nil {
			if !errors.Is(err, ErrInvalidToken) && !errors.Is(err, ErrStale) && !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Verify(%q) = %v, of no kind", token, err)
			}
			return
		}
		if c.Iss != cfg.Issuer || !slices.Contains(c.Aud, "aries") {
			t.Fatalf("Verify(%q) accepted issuer %q and audience %v", token, c.Iss, c.Aud)
		}
	})
}
