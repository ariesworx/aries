package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/logs"
)

// writeCert writes a self-signed certificate for name, ending at end,
// and its key, to dir/cert.pem and dir/key.pem (0600), and returns their
// paths.
func writeCert(t *testing.T, dir, name string, end time.Time) (cert, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: end}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	cert, key = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil { //nolint:gosec // a certificate is public
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// certServed is the certificate cfg serves for name.
func certServed(t *testing.T, cfg *tls.Config, name string) *x509.Certificate {
	t.Helper()
	c, err := cfg.GetCertificate(&tls.ClientHelloInfo{ServerName: name})
	if err != nil {
		t.Fatalf("GetCertificate(%s) = %v", name, err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// A certificate from files is served, and read again once its files
// change, with no restart; a change that does not read keeps the one
// served.
func TestTLSFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	end := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	cert, key := writeCert(t, dir, "aries.example.com", end)
	log := &syncBuffer{}
	tel, m := measured(t, false)
	cfg, err := HTTPSSettings{TLS: TLSSettings{Mode: TLSModeFiles, MinVersion: "1.2", Files: TLSFiles{Cert: cert, Key: key}}}.TLSConfig(slogTo(log), tel)
	if err != nil {
		t.Fatalf("TLSConfig(files) = %v", err)
	}
	if cfg.TLS.MinVersion != tls.VersionTLS12 || cfg.Mode != TLSModeFiles {
		t.Errorf("TLSConfig(files): min version %x, mode %q; want TLS 1.2, files", cfg.TLS.MinVersion, cfg.Mode)
	}
	if got := certServed(t, cfg.TLS, "aries.example.com"); got.Subject.CommonName != "aries.example.com" || !cfg.NotAfter().Equal(end) {
		t.Fatalf("served %s ending %s, want aries.example.com ending %s", got.Subject.CommonName, cfg.NotAfter(), end)
	}
	// Renewed: new files, a later end.
	later := end.Add(30 * 24 * time.Hour)
	writeCert(t, dir, "renewed.example.com", later)
	waitUntil(t, "the renewed certificate to be served", func() bool {
		return certServed(t, cfg.TLS, "renewed.example.com").Subject.CommonName == "renewed.example.com"
	})
	if !cfg.NotAfter().Equal(later) {
		t.Errorf("NotAfter after the renewal = %s, want %s", cfg.NotAfter(), later)
	}
	// A key that is not the certificate's is not taken.
	if err := os.WriteFile(key, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := certServed(t, cfg.TLS, "renewed.example.com"); got.Subject.CommonName != "renewed.example.com" {
		t.Errorf("after a bad key, served %s, want the renewed certificate kept", got.Subject.CommonName)
	}
	if !strings.Contains(log.String(), "event="+string(logs.TLSCertReloadFailed)) || !strings.Contains(log.String(), "event="+string(logs.TLSCertReloaded)) {
		t.Errorf("the log of a renewal and a bad key:\n%s\nwant a line of each", log.String())
	}
	if n := strings.Count(log.String(), string(logs.TLSCertReloadFailed)); n != 1 {
		certServed(t, cfg.TLS, "renewed.example.com")
		t.Errorf("a bad key logged %d times; want once, until the files change again", n)
	}
	// Each change of the files is counted once, however many handshakes
	// see it.
	certServed(t, cfg.TLS, "renewed.example.com")
	check(t, snapshot(t, m), []want{
		{"aries.https.cert.reloads", []string{"result", "ok"}, 1},
		{"aries.https.cert.reloads", []string{"result", "failed"}, 1},
	})
}

// Files that cannot be served, and a key others may read, are refused
// at start, each naming its fix.
func TestTLSFilesRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cert, key := writeCert(t, dir, "aries.example.com", time.Now().Add(time.Hour))
	open := filepath.Join(dir, "open.pem")
	b, err := os.ReadFile(key) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(open, b, 0o644); err != nil { //nolint:gosec // the mode under test
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		cert, key string
		want      string
	}{
		{"a missing certificate", filepath.Join(dir, "none.pem"), key, "none.pem"},
		{"a key others may read", cert, open, "chmod 600"},
		{"a key that is not the certificate's", cert, cert, "fix:"},
	} {
		_, err := HTTPSSettings{TLS: TLSSettings{Mode: TLSModeFiles, Files: TLSFiles{Cert: tc.cert, Key: tc.key}}}.TLSConfig(slogTo(&syncBuffer{}), nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("TLSConfig with %s = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

// ACME's configuration answers the TLS-ALPN-01 challenge on the
// listener, keeps its cache in a directory of its own, and refuses one
// others may read.
func TestTLSACME(t *testing.T) {
	t.Parallel()
	cache := filepath.Join(t.TempDir(), "acme")
	h := HTTPSSettings{TLS: TLSSettings{Mode: TLSModeACME, MinVersion: "1.3", ACME: ACMESettings{Domains: []string{"aries.example.com"}, Cache: cache}}}
	cfg, err := h.TLSConfig(slogTo(&syncBuffer{}), nil)
	if err != nil {
		t.Fatalf("TLSConfig(acme) = %v", err)
	}
	if cfg.TLS.GetCertificate == nil || !slices.Contains(cfg.TLS.NextProtos, "acme-tls/1") || cfg.TLS.MinVersion != tls.VersionTLS13 || cfg.Mode != TLSModeACME {
		t.Errorf("TLSConfig(acme) = protos %q, min version %x, mode %q; want the ACME challenge's protocol, TLS 1.3 and acme", cfg.TLS.NextProtos, cfg.TLS.MinVersion, cfg.Mode)
	}
	if fi, err := os.Stat(cache); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the ACME cache: %v, %v; want a directory of mode 0700", fi, err)
	}
	if _, err := cfg.TLS.GetCertificate(&tls.ClientHelloInfo{ServerName: "other.example.com"}); err == nil {
		t.Error("ACME would get a certificate for a name not in domains:")
	}
	if err := os.Chmod(cache, 0o755); err != nil { //nolint:gosec // the mode under test
		t.Fatal(err)
	}
	if _, err := h.TLSConfig(slogTo(&syncBuffer{}), nil); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Errorf("TLSConfig(acme) with a cache others may read = %v, want a refusal naming chmod 700", err)
	}
}

// Behind a proxy the listener has no TLS of its own.
func TestTLSProxy(t *testing.T) {
	t.Parallel()
	cfg, err := HTTPSSettings{TLS: TLSSettings{Mode: TLSModeProxy, Proxy: ProxySettings{Trusted: []string{"127.0.0.1"}}}}.TLSConfig(slogTo(&syncBuffer{}), nil)
	if err != nil || cfg.TLS != nil || cfg.Mode != TLSModeProxy {
		t.Errorf("TLSConfig(proxy) = %+v, %v; want no TLS", cfg, err)
	}
}
