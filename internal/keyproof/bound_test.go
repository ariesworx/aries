package keyproof

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// exporter is a stand-in for a TLS session's exporter: ExporterSize
// bytes of b.
func exporter(b byte) []byte { return bytes.Repeat([]byte{b}, ExporterSize) }

// A v2 proof verifies for the exporter it was signed with, and for no
// other: a relay's two TLS sessions have two exporters, so a proof made
// on one is refused on the other. An unbound v2 proof (no exporter) is
// not a bound one, and no v2 proof is a v1 proof or the reverse.
func TestSignVerifyV2(t *testing.T) {
	t.Parallel()
	nonce := NewNonce()
	for _, kind := range []string{"ed25519", "ecdsa", "rsa"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			s := signer(t, kind)
			bound, err := SignV2(s, serverURL, nonce, exporter(1))
			if err != nil {
				t.Fatalf("SignV2(%s) = %v", kind, err)
			}
			if kind == "rsa" && bound.Format != ssh.KeyAlgoRSASHA256 {
				t.Errorf("SignV2(rsa) used %s, want %s", bound.Format, ssh.KeyAlgoRSASHA256)
			}
			if err := VerifyV2(s.PublicKey(), serverURL, nonce, exporter(1), bound); err != nil {
				t.Errorf("VerifyV2 of its own bound proof = %v, want nil", err)
			}
			unbound, err := SignV2(s, serverURL, nonce, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyV2(s.PublicKey(), serverURL, nonce, nil, unbound); err != nil {
				t.Errorf("VerifyV2 of its own unbound proof = %v, want nil", err)
			}
			v1, err := Sign(s, serverURL, nonce)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name     string
				exporter []byte
				sig      *ssh.Signature
			}{
				{"a bound proof on another session", exporter(2), bound},
				{"a bound proof taken as unbound", nil, bound},
				{"an unbound proof taken as bound", exporter(1), unbound},
				{"a v1 proof taken as bound", exporter(1), v1},
				{"a v1 proof taken as unbound", nil, v1},
			} {
				if err := VerifyV2(s.PublicKey(), serverURL, nonce, tc.exporter, tc.sig); err == nil {
					t.Errorf("VerifyV2 of %s = nil, want a refusal", tc.name)
				}
			}
			for _, sig := range []*ssh.Signature{bound, unbound} {
				if err := Verify(s.PublicKey(), serverURL, nonce, sig); err == nil {
					t.Error("Verify (v1) of a v2 proof = nil, want a refusal")
				}
			}
		})
	}
}

// An exporter is ExporterSize bytes or none; anything else is refused
// on both sides rather than signed or checked.
func TestV2ExporterLength(t *testing.T) {
	t.Parallel()
	s := signer(t, "ed25519")
	nonce := NewNonce()
	if _, err := SignV2(s, serverURL, nonce, exporter(1)[:16]); err == nil {
		t.Error("SignV2 with a 16-byte exporter = nil error, want a refusal")
	}
	good, err := SignV2(s, serverURL, nonce, exporter(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyV2(s.PublicKey(), serverURL, nonce, exporter(1)[:16], good); err == nil {
		t.Error("VerifyV2 with a 16-byte exporter = nil, want a refusal")
	}
}

// The v2 transcript has its own domain, which is not v1's and is no SSH
// session id's length, and frames the exporter as every other part.
func TestTranscriptV2Framing(t *testing.T) {
	t.Parallel()
	s := signer(t, "ed25519")
	nonce := NewNonce()
	tr := transcriptV2(serverURL, nonce, s.PublicKey(), exporter(1))
	want := append([]byte{0, 0, 0, byte(len(domainV2))}, domainV2...)
	if !bytes.HasPrefix(tr, want) {
		t.Fatalf("the v2 transcript begins %q, want the domain %q as an SSH string", tr[:len(want)], want)
	}
	if domainV2 == domain {
		t.Error("v2's domain is v1's")
	}
	if n := len(domainV2); n >= 20 && n <= 64 {
		t.Errorf("the v2 domain's length %d is that of an SSH session id", n)
	}
	if !bytes.HasSuffix(tr, append([]byte{0, 0, 0, ExporterSize}, exporter(1)...)) {
		t.Error("the v2 transcript does not end with the exporter as an SSH string")
	}
	if bytes.Equal(tr, transcriptV2(serverURL, nonce, s.PublicKey(), nil)) {
		t.Error("a bound and an unbound transcript are the same")
	}
}

// A challenge carries the binding the server asks for, and one from a
// server that asks for none (v0.5.0's) parses with an empty one. A
// proof says its version: none (v1, what v0.5.0's aries sends) or 2.
func TestMessagesV2(t *testing.T) {
	t.Parallel()
	nonce := NewNonce()
	for _, bind := range []string{BindTLSExporter, BindNone, ""} {
		c, err := ParseChallenge(NewChallenge(0, nonce, bind))
		if err != nil || c.Bind != bind {
			t.Errorf("ParseChallenge(NewChallenge(%q)) = %+v, %v", bind, c, err)
		}
	}
	if b := NewChallenge(0, nonce, ""); strings.Contains(string(b), "bind") {
		t.Errorf("a challenge that asks for no binding says %s; v0.5.0's says nothing of it", b)
	}
	s := signer(t, "ed25519")
	sig, err := SignV2(s, serverURL, nonce, exporter(1))
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseProof(NewProofV2(sig))
	if err != nil || p.V != 2 {
		t.Fatalf("ParseProof(NewProofV2) = %+v, %v; want version 2", p, err)
	}
	if err := VerifyV2(s.PublicKey(), serverURL, nonce, exporter(1), p.Sig); err != nil {
		t.Errorf("VerifyV2 of a proof through its JSON = %v", err)
	}
	if p, err := ParseProof(NewProof(sig)); err != nil || p.V != 1 {
		t.Errorf("ParseProof(NewProof) = %+v, %v; want version 1", p, err)
	}
	if b := NewProof(sig); strings.Contains(string(b), `"v"`) {
		t.Errorf("a v1 proof says %s; v0.5.0's aries sends no version", b)
	}
	for _, bad := range []string{`{"t":"key.proof","v":3,"sig":"` + mustSig(t, sig) + `"}`, `{"t":"key.proof","v":0,"sig":"` + mustSig(t, sig) + `"}`} {
		if _, err := ParseProof([]byte(bad)); err == nil {
			t.Errorf("ParseProof(%s) = nil, want a version it does not read refused", bad)
		}
	}
}

// mustSig is sig in a proof's JSON form.
func mustSig(t *testing.T, sig *ssh.Signature) string {
	t.Helper()
	b := NewProof(sig)
	i := strings.Index(string(b), `"sig":"`)
	j := strings.LastIndex(string(b), `"`)
	return string(b[i+len(`"sig":"`) : j])
}

// Exporter is RFC 9266's tls-exporter of a TLS session: its two ends
// have the same one, and another session has another.
func TestExporter(t *testing.T) {
	t.Parallel()
	cfg, pool := selfSigned(t)
	one, two := tlsPair(t, cfg, pool), tlsPair(t, cfg, pool)
	cs1, ss1 := one[0].ConnectionState(), one[1].ConnectionState()
	c1, err := Exporter(&cs1)
	if err != nil {
		t.Fatalf("Exporter of the client's end = %v", err)
	}
	s1, err := Exporter(&ss1)
	if err != nil {
		t.Fatalf("Exporter of the server's end = %v", err)
	}
	if len(c1) != ExporterSize || !bytes.Equal(c1, s1) {
		t.Fatalf("the two ends' exporters are %x and %x, want the same %d bytes", c1, s1, ExporterSize)
	}
	cs2 := two[0].ConnectionState()
	c2, err := Exporter(&cs2)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(c1, c2) {
		t.Error("two TLS sessions have one exporter")
	}
	if _, err := Exporter(nil); err == nil {
		t.Error("Exporter(nil) = nil error, want one: no TLS, no exporter")
	}
}

// tlsPair is the two ends, client first, of a TLS session over a pipe.
func tlsPair(t *testing.T, cfg *tls.Config, pool *x509.CertPool) [2]*tls.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	client := tls.Client(a, &tls.Config{RootCAs: pool, ServerName: "aries.example.com", MinVersion: tls.VersionTLS12})
	server := tls.Server(b, cfg)
	errc := make(chan error, 1)
	go func() { errc <- server.HandshakeContext(context.Background()) }()
	if err := client.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	return [2]*tls.Conn{client, server}
}

// selfSigned is a server's TLS configuration with a self-signed
// certificate for aries.example.com, and a pool that trusts it.
func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "aries.example.com"}, DNSNames: []string{"aries.example.com"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}
