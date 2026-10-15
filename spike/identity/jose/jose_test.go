package jose

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// sign makes a compact JWS of header and payload under key, by alg.
func sign(t *testing.T, header, payload string, key crypto.Signer) string {
	t.Helper()
	in := enc([]byte(header)) + "." + enc([]byte(payload))
	d := sha256.Sum256([]byte(in))
	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		s, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, d[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = s
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, d[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	}
	return in + "." + enc(sig)
}

func TestVerify(t *testing.T) {
	t.Parallel()
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ek, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rs := sign(t, `{"alg":"RS256","kid":"r"}`, `{"sub":"alice"}`, rk)
	es := sign(t, `{"alg":"ES256","kid":"e"}`, `{"sub":"alice"}`, ek)
	// The ES256 header over an RSA signature: an algorithm switch.
	switched := sign(t, `{"alg":"ES256"}`, `{"sub":"alice"}`, rk)
	parts := strings.Split(rs, ".")
	tampered := parts[0] + "." + enc([]byte(`{"sub":"mallory"}`)) + "." + parts[2]

	tests := []struct {
		name  string
		token string
		key   crypto.PublicKey
		ok    bool
	}{
		{"RS256", rs, &rk.PublicKey, true},
		{"ES256", es, &ek.PublicKey, true},
		{"ES256 under another key", es, &other.PublicKey, false},
		{"RS256 tampered payload", tampered, &rk.PublicKey, false},
		{"RS256 under an EC key", rs, &ek.PublicKey, false},
		{"ES256 header, RSA signature", switched, &rk.PublicKey, false},
	}
	for _, tc := range tests {
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
}

func TestParseRefuses(t *testing.T) {
	t.Parallel()
	sig := enc([]byte("x"))
	tests := []struct{ name, token string }{
		{"alg none", enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{}`)) + "."},
		{"HS256", enc([]byte(`{"alg":"HS256"}`)) + "." + enc([]byte(`{}`)) + "." + sig},
		{"no alg", enc([]byte(`{}`)) + "." + enc([]byte(`{}`)) + "." + sig},
		{"crit", enc([]byte(`{"alg":"RS256","crit":["b64"]}`)) + "." + enc([]byte(`{}`)) + "." + sig},
		{"two parts", enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{}`))},
		{"four parts", enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(`{}`)) + "." + sig + "." + sig},
		{"padded", enc([]byte(`{"alg":"RS256"}`)) + "=." + enc([]byte(`{}`)) + "." + sig},
		{"header not an object", enc([]byte(`"RS256"`)) + "." + enc([]byte(`{}`)) + "." + sig},
		{"oversized", strings.Repeat("a", MaxToken+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tc.token); err == nil {
				t.Errorf("Parse(%s) = nil error, want a refusal", tc.name)
			}
		})
	}
}

func TestJWK(t *testing.T) {
	t.Parallel()
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		k    JWK
		ok   bool
	}{
		{"encryption key", JWK{Kty: "RSA", Use: "enc", N: enc(make([]byte, 256)), E: "AQAB"}, false},
		{"1024-bit RSA", JWK{Kty: "RSA", N: enc(small.N.Bytes()), E: "AQAB"}, false},
		{"even exponent", JWK{Kty: "RSA", N: enc(append([]byte{0x80}, make([]byte, 255)...)), E: "Ag"}, false},
		{"P-384", JWK{Kty: "EC", Crv: "P-384", X: enc(make([]byte, 48)), Y: enc(make([]byte, 48))}, false},
		{"point not on the curve", JWK{Kty: "EC", Crv: "P-256", X: enc(make([]byte, 32)), Y: enc(make([]byte, 32))}, false},
		{"oct", JWK{Kty: "oct"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.k.PublicKey(); (err == nil) != tc.ok {
				t.Errorf("JWK(%s).PublicKey() = %v, want ok %v", tc.name, err, tc.ok)
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	f.Add("eyJhbGciOiJSUzI1NiJ9.e30.eA")
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Add("..")
	f.Fuzz(func(t *testing.T, token string) {
		j, err := Parse(token)
		if err != nil {
			return
		}
		if j.Header.Alg != "RS256" && j.Header.Alg != "ES256" {
			t.Fatalf("Parse(%q) accepted algorithm %q", token, j.Header.Alg)
		}
	})
}
