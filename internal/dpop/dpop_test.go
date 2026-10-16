package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/jose"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

const (
	connect = "https://aries.example.com/v1/connect"
	token   = "an-access-token-of-the-server"
)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func proof(t *testing.T, k *ecdsa.PrivateKey, method, htu, tok string, at time.Time) string {
	t.Helper()
	p, err := Proof(k, method, htu, tok, at)
	if err != nil {
		t.Fatalf("Proof = %v", err)
	}
	return p
}

// forged signs a proof with claims and a header of the test's choosing.
func forged(t *testing.T, k *ecdsa.PrivateKey, h jose.Header, c map[string]any) string {
	t.Helper()
	if h.JWK == nil {
		j, _ := jose.PublicJWK(&k.PublicKey, "")
		h.JWK = &j
	}
	p, err := jose.Sign(k, h, c)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestVerify(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	v := NewVerifier(100)
	jkt, err := v.Verify(proof(t, k, "GET", connect, token, t0), "GET", []string{connect}, token, t0)
	if err != nil {
		t.Fatalf("Verify(a good proof) = %v", err)
	}
	if want := Thumbprint(&k.PublicKey); jkt != want {
		t.Errorf("Verify = thumbprint %s, want the key's, %s", jkt, want)
	}
	// htu is compared without its query, with the host's case and the
	// default port as they come.
	if _, err := v.Verify(proof(t, k, "GET", "https://Aries.Example.com:443/v1/connect?x=1", token, t0), "GET", []string{connect}, token, t0); err != nil {
		t.Errorf("Verify(an htu with a query, a capital and :443) = %v, want it the same URL", err)
	}
	// Each of the server's URLs is its own: a proof for either passes.
	if _, err := v.Verify(proof(t, k, "GET", "https://aries.internal.example.com/v1/connect", token, t0), "GET",
		[]string{connect, "https://aries.internal.example.com/v1/connect"}, token, t0); err != nil {
		t.Errorf("Verify(a proof for the server's second URL) = %v", err)
	}
	// At the token endpoint there is no access token yet, and no ath.
	if _, err := v.Verify(proof(t, k, "POST", "https://aries.example.com/oauth/token", "", t0), "POST",
		[]string{"https://aries.example.com/oauth/token"}, "", t0); err != nil {
		t.Errorf("Verify(a proof with no token) = %v", err)
	}
}

func TestVerifyRefuses(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	other := newKey(t)
	jwk, _ := jose.PublicJWK(&other.PublicKey, "")
	good := map[string]any{"jti": "j-1", "htm": "GET", "htu": connect, "iat": t0.Unix(), "ath": ath(token)}
	with := func(name string, value any) map[string]any {
		c := map[string]any{}
		for k, v := range good {
			c[k] = v
		}
		if value == nil {
			delete(c, name)
		} else {
			c[name] = value
		}
		return c
	}
	typ := jose.Header{Typ: "dpop+jwt"}
	rsa := strings.Join([]string{base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"dpop+jwt","alg":"RS256","jwk":{"kty":"RSA","n":"AQAB","e":"AQAB"}}`)), "e30", "eA"}, ".")
	for _, tc := range []struct {
		name, proof string
		reason      Reason
	}{
		{"not a JWT", "nope", ReasonMalformed},
		{"empty", "", ReasonMissing},
		{"no typ", forged(t, k, jose.Header{}, good), ReasonMalformed},
		{"another typ", forged(t, k, jose.Header{Typ: "JWT"}, good), ReasonMalformed},
		{"no key", strings.Join([]string{base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"dpop+jwt","alg":"ES256"}`)), "e30", "eA"}, "."), ReasonMalformed},
		{"an RSA key", rsa, ReasonMalformed},
		{"signed by another key than it carries", forged(t, k, jose.Header{Typ: "dpop+jwt", JWK: &jwk}, good), ReasonSignature},
		{"another method", forged(t, k, typ, with("htm", "POST")), ReasonRequest},
		{"another URL", forged(t, k, typ, with("htu", "https://evil.example.com/v1/connect")), ReasonRequest},
		{"another path", forged(t, k, typ, with("htu", "https://aries.example.com/v1/other")), ReasonRequest},
		{"plain http", forged(t, k, typ, with("htu", "http://aries.example.com/v1/connect")), ReasonRequest},
		{"another token", forged(t, k, typ, with("ath", ath("another"))), ReasonToken},
		{"no ath", forged(t, k, typ, with("ath", nil)), ReasonToken},
		{"from the past", forged(t, k, typ, with("iat", t0.Add(-2*time.Minute).Unix())), ReasonTime},
		{"from the future", forged(t, k, typ, with("iat", t0.Add(2*time.Minute).Unix())), ReasonTime},
		{"no jti", forged(t, k, typ, with("jti", nil)), ReasonMalformed},
		{"a long jti", forged(t, k, typ, with("jti", strings.Repeat("j", 65))), ReasonMalformed},
		{"oversized", strings.Repeat("a", MaxProof+1), ReasonMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewVerifier(100).Verify(tc.proof, "GET", []string{connect}, token, t0)
			var e *Error
			if !errors.As(err, &e) || e.Reason != tc.reason {
				t.Errorf("Verify(%s) = %v, want reason %s", tc.name, err, tc.reason)
			}
		})
	}
}

// TestReplay: a proof is spent once, for the window in which its iat
// would be accepted; and the proofs remembered are bounded (rule 18),
// refusing busy when full until the oldest age out.
func TestReplay(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	v := NewVerifier(2)
	p := proof(t, k, "GET", connect, token, t0)
	if _, err := v.Verify(p, "GET", []string{connect}, token, t0); err != nil {
		t.Fatal(err)
	}
	_, err := v.Verify(p, "GET", []string{connect}, token, t0.Add(30*time.Second))
	if e, ok := errors.AsType[*Error](err); !ok || e.Reason != ReasonReplayed {
		t.Errorf("Verify(the same proof again) = %v, want replayed", err)
	}
	if _, err := v.Verify(proof(t, k, "GET", connect, token, t0), "GET", []string{connect}, token, t0); err != nil {
		t.Fatal(err)
	}
	_, err = v.Verify(proof(t, k, "GET", connect, token, t0), "GET", []string{connect}, token, t0)
	if e, ok := errors.AsType[*Error](err); !ok || e.Reason != ReasonBusy {
		t.Errorf("Verify(a third proof with two remembered, the most) = %v, want busy", err)
	}
	later := t0.Add(Window + time.Second)
	if _, err := v.Verify(proof(t, k, "GET", connect, token, later), "GET", []string{connect}, token, later); err != nil {
		t.Errorf("Verify(a proof once the others aged out) = %v", err)
	}
	if n := v.Len(); n != 1 {
		t.Errorf("Len() = %d once two proofs aged out and one came, want 1", n)
	}
	// A refused proof spends nothing.
	bad := proof(t, k, "POST", connect, token, later)
	_, _ = v.Verify(bad, "GET", []string{connect}, token, later)
	if n := v.Len(); n != 1 {
		t.Errorf("Len() = %d after a refused proof, want 1", n)
	}
}

func TestKeyFile(t *testing.T) {
	t.Parallel()
	k := newKey(t)
	b, err := MarshalKey(k)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseKey(b)
	if err != nil || !back.Equal(k) {
		t.Fatalf("ParseKey(MarshalKey(k)) = %v, %v; want k", back, err)
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if b, err := MarshalKey(p384); err == nil {
		if _, err := ParseKey(b); err == nil {
			t.Error("ParseKey(a P-384 key) = nil error, want a refusal")
		}
	}
	if _, err := ParseKey([]byte("not a key")); err == nil {
		t.Error("ParseKey(not a key) = nil error")
	}
}

func FuzzVerify(f *testing.F) {
	k, err := NewKey()
	if err != nil {
		f.Fatal(err)
	}
	p, _ := Proof(k, "GET", connect, token, t0)
	f.Add(p)
	f.Add("eyJ0eXAiOiJkcG9wK2p3dCIsImFsZyI6IkVTMjU2In0.e30.eA")
	f.Add("..")
	f.Fuzz(func(t *testing.T, proof string) {
		v := NewVerifier(10)
		jkt, err := v.Verify(proof, "GET", []string{connect}, token, t0)
		if err != nil {
			if _, ok := errors.AsType[*Error](err); !ok {
				t.Fatalf("Verify(%q) = %v, not an *Error", proof, err)
			}
			return
		}
		var c claims
		parts := strings.Split(proof, ".")
		b, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if json.Unmarshal(b, &c) != nil || c.HTM != "GET" || jkt == "" {
			t.Fatalf("Verify(%q) accepted claims %+v", proof, c)
		}
	})
}
