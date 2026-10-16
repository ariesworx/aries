package jose

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// keys are made once for the package's tests: an RSA key is slow to make.
var keys = sync.OnceValues(func() (*rsa.PrivateKey, *ecdsa.PrivateKey) {
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err) // a test helper: the system's randomness failed
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return rk, ek
})

// signRaw signs header and payload, given as JSON text, as Sign would,
// so that a test can sign a header Sign would never write.
func signRaw(t *testing.T, header, payload string, key crypto.Signer) string {
	t.Helper()
	tok, err := sign(enc([]byte(header))+"."+enc([]byte(payload)), key)
	if err != nil {
		t.Fatalf("sign(%s): %v", header, err)
	}
	return tok
}

func TestSignAndVerify(t *testing.T) {
	t.Parallel()
	rk, ek := keys()
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]string{"sub": "alice"}
	rs, err := Sign(rk, Header{Kid: "r"}, claims)
	if err != nil {
		t.Fatalf("Sign(RSA) = %v", err)
	}
	es, err := Sign(ek, Header{Kid: "e", Typ: "JWT"}, claims)
	if err != nil {
		t.Fatalf("Sign(EC) = %v", err)
	}
	// The ES256 header over an RSA signature: an algorithm switch.
	switched := signRaw(t, `{"alg":"ES256"}`, `{"sub":"alice"}`, rk)
	parts := strings.Split(rs, ".")
	tampered := parts[0] + "." + enc([]byte(`{"sub":"mallory"}`)) + "." + parts[2]

	for _, tc := range []struct {
		name  string
		token string
		key   crypto.PublicKey
		ok    bool
	}{
		{"RS256", rs, &rk.PublicKey, true},
		{"ES256", es, &ek.PublicKey, true},
		{"ES256 under another key", es, &other.PublicKey, false},
		{"RS256 with its payload changed", tampered, &rk.PublicKey, false},
		{"RS256 under an EC key", rs, &ek.PublicKey, false},
		{"an ES256 header over an RSA signature", switched, &rk.PublicKey, false},
		{"no key", es, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			j, err := Parse(tc.token)
			if err != nil {
				t.Fatalf("Parse(%s) = %v", tc.name, err)
			}
			if err := j.Verify(tc.key); (err == nil) != tc.ok {
				t.Errorf("Verify(%s) = %v, want ok %v", tc.name, err, tc.ok)
			}
		})
	}
	j, err := Parse(es)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(j.Payload, &got); err != nil || got["sub"] != "alice" || j.Header.Kid != "e" || j.Header.Typ != "JWT" || j.Header.Alg != AlgES256 {
		t.Errorf("Parse(Sign(EC)) = header %+v, claims %v, %v; want kid e, typ JWT, ES256 and sub alice", j.Header, got, err)
	}
}

func TestSignRefuses(t *testing.T) {
	t.Parallel()
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Sign(p384, Header{}, struct{}{}); err == nil {
		t.Error("Sign(P-384 key) = nil error, want a refusal: ES256 is P-256's")
	}
}

func TestParseRefuses(t *testing.T) {
	t.Parallel()
	sig := enc([]byte("x"))
	body := "." + enc([]byte(`{}`)) + "." + sig
	for _, tc := range []struct{ name, token, want string }{
		{"alg none", enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{}`)) + ".", "not accepted"},
		{"HS256", enc([]byte(`{"alg":"HS256"}`)) + body, "not accepted"},
		{"RS512", enc([]byte(`{"alg":"RS512"}`)) + body, "not accepted"},
		{"no alg", enc([]byte(`{}`)) + body, "not accepted"},
		{"crit", enc([]byte(`{"alg":"RS256","crit":["b64"]}`)) + body, "critical"},
		{"an empty crit", enc([]byte(`{"alg":"RS256","crit":[]}`)) + body, "critical"},
		{"two parts", enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{}`)), "three parts"},
		{"four parts", enc([]byte(`{"alg":"RS256"}`)) + body + "." + sig, "three parts"},
		{"no dot", "abc", "three parts"},
		{"padded", enc([]byte(`{"alg":"RS256"}`)) + "=" + body, "header"},
		{"standard base64", strings.ReplaceAll(enc([]byte(`{"alg":"RS256","kid":"??>"}`)), "_", "/") + body, "header"},
		{"header not an object", enc([]byte(`"RS256"`)) + body, "header"},
		{"header with trailing data", enc([]byte(`{"alg":"RS256"} {}`)) + body, "header"},
		{"bad payload", enc([]byte(`{"alg":"RS256"}`)) + ".!!." + sig, "payload"},
		{"bad signature", enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{}`)) + ".!!", "signature"},
		{"empty signature", enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{}`)) + ".", "signature"},
		{"oversized", strings.Repeat("a", MaxToken+1), "over"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.token)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse(%s) = %v, want an error naming %q", tc.name, err, tc.want)
			}
			if err != nil && !errors.Is(err, ErrMalformed) {
				t.Errorf("Parse(%s) = %v, want it to wrap ErrMalformed", tc.name, err)
			}
		})
	}
}

func TestJWKRoundTrip(t *testing.T) {
	t.Parallel()
	rk, ek := keys()
	for _, k := range []crypto.PublicKey{&rk.PublicKey, &ek.PublicKey} {
		j, err := PublicJWK(k, "kid-1")
		if err != nil {
			t.Fatalf("PublicJWK(%T) = %v", k, err)
		}
		back, err := j.PublicKey()
		if err != nil {
			t.Fatalf("PublicJWK(%T).PublicKey() = %v", k, err)
		}
		if eq, ok := back.(interface{ Equal(crypto.PublicKey) bool }); !ok || !eq.Equal(k) {
			t.Errorf("PublicJWK(%T).PublicKey() = a different key", k)
		}
	}
}

func TestJWKRefuses(t *testing.T) {
	t.Parallel()
	small, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // a key too small, on purpose
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		k    JWK
	}{
		{"an encryption key", JWK{Kty: "RSA", Use: "enc", N: enc(make([]byte, 256)), E: "AQAB"}},
		{"a 1024-bit RSA key", JWK{Kty: "RSA", N: enc(small.N.Bytes()), E: "AQAB"}},
		{"an even exponent", JWK{Kty: "RSA", N: enc(append([]byte{0x80}, make([]byte, 255)...)), E: "Ag"}},
		{"an exponent of five bytes", JWK{Kty: "RSA", N: enc(append([]byte{0x80}, make([]byte, 255)...)), E: enc([]byte{1, 0, 0, 0, 1})}},
		{"an RSA key for ES256", JWK{Kty: "RSA", Alg: AlgES256, N: enc(append([]byte{0x80}, make([]byte, 255)...)), E: "AQAB"}},
		{"P-384", JWK{Kty: "EC", Crv: "P-384", X: enc(make([]byte, 48)), Y: enc(make([]byte, 48))}},
		{"a point not on the curve", JWK{Kty: "EC", Crv: "P-256", X: enc(make([]byte, 32)), Y: enc(make([]byte, 32))}},
		{"oct", JWK{Kty: "oct"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.k.PublicKey(); err == nil {
				t.Errorf("JWK(%s).PublicKey() = nil error, want a refusal", tc.name)
			}
		})
	}
}

func TestThumbprint(t *testing.T) {
	t.Parallel()
	// RFC 7638 §3.1's example is RSA; this is RFC 9449 §4.1's EC key,
	// whose thumbprint RFC 9449 §6.1 gives.
	j := JWK{Kty: "EC", Crv: "P-256", X: "l8tFrhx-34tV3hRICRDY9zCkDlpBhF42UQUfWVAWBFs", Y: "9VE4jf_Ok_o64zbTTlcuNJajHmt6v9TDVrU0CdvGRDA"}
	if got, want := j.Thumbprint(), "0ZcOCORZNYy-DWpqq30jZyJGHTN0d2HglBV3uiguA4I"; got != want {
		t.Errorf("Thumbprint(RFC 9449's key) = %s, want %s", got, want)
	}
}

func FuzzParse(f *testing.F) {
	f.Add("eyJhbGciOiJSUzI1NiJ9.e30.eA")
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Add("eyJhbGciOiJFUzI1NiIsImp3ayI6eyJrdHkiOiJFQyJ9fQ.e30.eA")
	f.Add("..")
	f.Fuzz(func(t *testing.T, token string) {
		j, err := Parse(token)
		if err != nil {
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("Parse(%q) = %v, which does not wrap ErrMalformed", token, err)
			}
			return
		}
		if j.Header.Alg != AlgRS256 && j.Header.Alg != AlgES256 {
			t.Fatalf("Parse(%q) accepted algorithm %q", token, j.Header.Alg)
		}
		if j.Header.Crit != nil {
			t.Fatalf("Parse(%q) accepted crit %v", token, j.Header.Crit)
		}
		if j.Header.JWK != nil {
			_, _ = j.Header.JWK.PublicKey()
		}
		_ = j.Verify(nil)
	})
}
