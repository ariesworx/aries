package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestVerify(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	key := newKey(t)
	const u = "https://aries.example.com/v1/connect"
	tests := []struct {
		name    string
		proof   func() string
		htm     string
		htu     string
		token   string
		at      time.Time
		wantErr string
	}{
		{name: "good", proof: func() string { return mustProof(t, key, "GET", u, "tok", now) }, htm: "GET", htu: u, token: "tok", at: now},
		{name: "query and fragment ignored", proof: func() string { return mustProof(t, key, "GET", u, "tok", now) }, htm: "GET", htu: u + "?project=p#x", token: "tok", at: now},
		{name: "other method", proof: func() string { return mustProof(t, key, "POST", u, "tok", now) }, htm: "GET", htu: u, token: "tok", at: now, wantErr: "htm"},
		{name: "other URL", proof: func() string { return mustProof(t, key, "GET", "https://evil.example.com/v1/connect", "tok", now) }, htm: "GET", htu: u, token: "tok", at: now, wantErr: "htu"},
		{name: "other token", proof: func() string { return mustProof(t, key, "GET", u, "other", now) }, htm: "GET", htu: u, token: "tok", at: now, wantErr: "ath"},
		{name: "too old", proof: func() string { return mustProof(t, key, "GET", u, "tok", now.Add(-2*time.Minute)) }, htm: "GET", htu: u, token: "tok", at: now, wantErr: "iat"},
		{name: "from the future", proof: func() string { return mustProof(t, key, "GET", u, "tok", now.Add(2*time.Minute)) }, htm: "GET", htu: u, token: "tok", at: now, wantErr: "iat"},
		{name: "not a JWT", proof: func() string { return "x.y.z" }, htm: "GET", htu: u, token: "tok", at: now, wantErr: "proof"},
		{name: "signed by another key than its jwk", proof: func() string {
			p := mustProof(t, key, "GET", u, "tok", now)
			q := mustProof(t, newKey(t), "GET", u, "tok", now)
			h, _, _ := strings.Cut(p, ".")
			_, rest, _ := strings.Cut(q, ".")
			return h + "." + rest
		}, htm: "GET", htu: u, token: "tok", at: now, wantErr: "signature"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := NewVerifier(100, time.Minute)
			jkt, err := v.Verify(tc.proof(), tc.htm, tc.htu, tc.token, tc.at)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Verify(%s) = %q, %v, want an error naming %q", tc.name, jkt, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify(%s) = %v, want nil", tc.name, err)
			}
			if want := Thumbprint(&key.PublicKey); jkt != want {
				t.Errorf("Verify(%s) jkt = %q, want %q", tc.name, jkt, want)
			}
		})
	}
}

func TestVerifyRefusesReplay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	key := newKey(t)
	v := NewVerifier(100, time.Minute)
	p := mustProof(t, key, "GET", "https://aries.example.com/x", "tok", now)
	if _, err := v.Verify(p, "GET", "https://aries.example.com/x", "tok", now); err != nil {
		t.Fatalf("first Verify = %v, want nil", err)
	}
	if _, err := v.Verify(p, "GET", "https://aries.example.com/x", "tok", now.Add(time.Second)); err == nil || !strings.Contains(err.Error(), "jti") {
		t.Errorf("second Verify of the same proof = %v, want a replay refused by jti", err)
	}
}

func TestVerifierIsBounded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	key := newKey(t)
	v := NewVerifier(2, time.Minute)
	for i := range 3 {
		p := mustProof(t, key, "GET", "https://aries.example.com/x", "", now)
		_, err := v.Verify(p, "GET", "https://aries.example.com/x", "", now)
		if i < 2 && err != nil {
			t.Fatalf("Verify %d = %v, want nil", i, err)
		}
		if i == 2 && err == nil {
			t.Errorf("Verify with %d proofs held, over the bound of 2 = nil, want busy", i)
		}
	}
	// Once the window has passed, room is made again.
	p := mustProof(t, key, "GET", "https://aries.example.com/x", "", now.Add(2*time.Minute))
	if _, err := v.Verify(p, "GET", "https://aries.example.com/x", "", now.Add(2*time.Minute)); err != nil {
		t.Errorf("Verify after the window = %v, want nil", err)
	}
}

func mustProof(t *testing.T, key *ecdsa.PrivateKey, htm, htu, token string, at time.Time) string {
	t.Helper()
	p, err := Proof(key, htm, htu, token, at)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
