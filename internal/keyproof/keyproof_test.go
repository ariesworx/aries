package keyproof

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func signer(t *testing.T, kind string) ssh.Signer {
	t.Helper()
	var key any
	var err error
	switch kind {
	case "ed25519":
		_, key, err = ed25519.GenerateKey(rand.Reader)
	case "ecdsa":
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "rsa":
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const serverURL = "https://aries.example.com"

func TestSignVerify(t *testing.T) {
	t.Parallel()
	nonce := NewNonce()
	for _, kind := range []string{"ed25519", "ecdsa", "rsa"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			s := signer(t, kind)
			sig, err := Sign(s, serverURL, nonce)
			if err != nil {
				t.Fatalf("Sign(%s) = %v", kind, err)
			}
			if kind == "rsa" && sig.Format != ssh.KeyAlgoRSASHA256 {
				t.Errorf("Sign(rsa) used %s, want %s", sig.Format, ssh.KeyAlgoRSASHA256)
			}
			if err := Verify(s.PublicKey(), serverURL, nonce, sig); err != nil {
				t.Errorf("Verify(%s) of its own proof = %v, want nil", kind, err)
			}
		})
	}
}

func TestVerifyRefuses(t *testing.T) {
	t.Parallel()
	s, other := signer(t, "ed25519"), signer(t, "ed25519")
	nonce := NewNonce()
	good, err := Sign(s, serverURL, nonce)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey := signer(t, "rsa")
	sha1, err := rsaKey.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, transcript(serverURL, nonce, rsaKey.PublicKey()), ssh.KeyAlgoRSA)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		pub   ssh.PublicKey
		url   string
		nonce []byte
		sig   *ssh.Signature
	}{
		{"another key", other.PublicKey(), serverURL, nonce, good},
		{"another URL", s.PublicKey(), "https://evil.example.com", nonce, good},
		{"another nonce", s.PublicKey(), serverURL, NewNonce(), good},
		{"a short nonce", s.PublicKey(), serverURL, nonce[:8], good},
		{"no signature", s.PublicKey(), serverURL, nonce, nil},
		{"rsa with SHA-1", rsaKey.PublicKey(), serverURL, nonce, sha1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := Verify(tc.pub, tc.url, tc.nonce, tc.sig); err == nil {
				t.Errorf("Verify with %s = nil, want a refusal", tc.name)
			}
		})
	}
}

// A certificate is not a key the server holds: it is refused before it
// could be looked up.
func TestParseOffer(t *testing.T) {
	t.Parallel()
	s := signer(t, "ed25519")
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
	cert := &ssh.Certificate{Key: s.PublicKey(), CertType: ssh.UserCert, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, signer(t, "ed25519")); err != nil {
		t.Fatal(err)
	}
	certLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert)))
	many := make([]string, MaxKeys+1)
	for i := range many {
		many[i] = line
	}
	tests := []struct {
		name string
		msg  string
		keys int
		err  string
	}{
		{name: "one key", msg: `{"t":"key.offer","url":"https://aries.example.com","keys":["` + line + `"]}`, keys: 1},
		{name: "a comment ignored", msg: `{"t":"key.offer","url":"https://aries.example.com","keys":["` + line + ` alice@example.com"]}`, keys: 1},
		{name: "another type", msg: `{"t":"hello","url":"https://aries.example.com","keys":["` + line + `"]}`, err: "key.offer"},
		{name: "no keys", msg: `{"t":"key.offer","url":"https://aries.example.com","keys":[]}`, err: "no key"},
		{name: "too many keys", msg: `{"t":"key.offer","url":"https://aries.example.com","keys":` + mustJSON(t, many) + `}`, err: "at most"},
		{name: "not a key", msg: `{"t":"key.offer","url":"https://aries.example.com","keys":["ssh-ed25519 notbase64"]}`, err: "key 1"},
		{name: "options", msg: `{"t":"key.offer","url":"https://aries.example.com","keys":["restrict ` + line + `"]}`, err: "key 1"},
		{name: "a certificate", msg: `{"t":"key.offer","url":"https://aries.example.com","keys":["` + certLine + `"]}`, err: "certificate"},
		{name: "not JSON", msg: `{"t":`, err: "offer"},
		{name: "no URL", msg: `{"t":"key.offer","keys":["` + line + `"]}`, err: "url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, err := ParseOffer([]byte(tc.msg))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("ParseOffer(%s) = %v, want an error naming %q", tc.name, err, tc.err)
				}
				return
			}
			if err != nil || len(o.Keys) != tc.keys {
				t.Errorf("ParseOffer(%s) = %d keys, %v; want %d", tc.name, len(o.Keys), err, tc.keys)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The three messages make a round trip through their JSON.
func TestMessages(t *testing.T) {
	t.Parallel()
	s := signer(t, "ed25519")
	b := NewOffer(serverURL, []ssh.PublicKey{s.PublicKey()})
	o, err := ParseOffer(b)
	if err != nil || o.URL != serverURL || len(o.Keys) != 1 || string(o.Keys[0].Marshal()) != string(s.PublicKey().Marshal()) {
		t.Fatalf("ParseOffer(NewOffer) = %+v, %v", o, err)
	}
	nonce := NewNonce()
	c, err := ParseChallenge(NewChallenge(0, nonce))
	if err != nil || c.Key != 0 || string(c.Nonce) != string(nonce) {
		t.Fatalf("ParseChallenge(NewChallenge) = %+v, %v", c, err)
	}
	sig, err := Sign(s, serverURL, nonce)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseProof(NewProof(sig))
	if err != nil {
		t.Fatalf("ParseProof(NewProof) = %v", err)
	}
	if err := Verify(s.PublicKey(), serverURL, nonce, p); err != nil {
		t.Errorf("Verify of a proof through its JSON = %v", err)
	}
	for _, bad := range []string{`{"t":"key.proof","sig":"AAAA"}`, `{"t":"key.proof"}`, `{"t":"key.challenge","sig":""}`} {
		if _, err := ParseProof([]byte(bad)); err == nil {
			t.Errorf("ParseProof(%s) = nil, want an error", bad)
		}
	}
	for _, bad := range []string{`{"t":"key.challenge","key":0,"nonce":"AAAA"}`, `{"t":"key.challenge","key":-1,"nonce":"` + strings.Repeat("A", 44) + `"}`} {
		if _, err := ParseChallenge([]byte(bad)); err == nil {
			t.Errorf("ParseChallenge(%s) = nil, want an error", bad)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want, err string }{
		{in: "https://aries.example.com", want: "https://aries.example.com"},
		{in: "https://Aries.Example.COM/", want: "https://aries.example.com"},
		{in: "HTTPS://aries.example.com:443", want: "https://aries.example.com"},
		{in: "https://aries.example.com:8443/", want: "https://aries.example.com:8443"},
		{in: "https://aries.example.com/aries/", want: "https://aries.example.com/aries"},
		{in: "https://[::1]:8443", want: "https://[::1]:8443"},
		{in: "http://aries.example.com", err: "https"},
		{in: "https://aries.example.com?x=1", err: "query"},
		{in: "https://alice@aries.example.com", err: "user"},
		{in: "https://aries.example.com/#x", err: "fragment"},
		{in: "https://", err: "host"},
		{in: "aries.example.com", err: "https"},
		{in: "https://aries.example.com/a b", err: "path"},
	}
	for _, tc := range tests {
		got, err := NormalizeURL(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("NormalizeURL(%q) = %q, %v; want an error naming %q", tc.in, got, err, tc.err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestConnectURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"https://aries.example.com", "wss://aries.example.com/v1/connect"},
		{"https://aries.example.com:8443/aries", "wss://aries.example.com:8443/aries/v1/connect"},
	} {
		if got := ConnectURL(tc.in); got != tc.want {
			t.Errorf("ConnectURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func FuzzParseOffer(f *testing.F) {
	f.Add([]byte(`{"t":"key.offer","url":"https://aries.example.com","keys":["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGrjpvZtqfZ6W0d0bdz9l3ip2MEqI1vk8JjkxfvF2wJ8"]}`))
	f.Add([]byte(`{"t":"key.offer","url":"https://a","keys":["","x"]}`))
	f.Add([]byte(`{}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		o, err := ParseOffer(b)
		if err != nil {
			return
		}
		if len(o.Keys) == 0 || len(o.Keys) > MaxKeys {
			t.Fatalf("ParseOffer accepted %d keys", len(o.Keys))
		}
		if _, err := NormalizeURL(o.URL); err != nil {
			t.Fatalf("ParseOffer accepted the URL %q: %v", o.URL, err)
		}
	})
}

func FuzzParseProof(f *testing.F) {
	f.Add([]byte(`{"t":"key.proof","sig":"AAAAC3NzaC1lZDI1NTE5AAAAQA=="}`))
	f.Add([]byte(`{"t":"key.proof","sig":""}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ParseProof(b)
		if err == nil && (p == nil || p.Format == "" || len(p.Blob) == 0) {
			t.Fatalf("ParseProof(%q) = %+v and no error, want a whole signature", b, p)
		}
	})
}
