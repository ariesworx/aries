package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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

	"github.com/ariesworx/aries/internal/proto"
)

// caPEM is a CA's certificate in PEM, made for the test.
func caPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// indent is s with each line indented by n spaces, for a YAML block.
func indent(s string, n int) string {
	pad := strings.Repeat(" ", n)
	return pad + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n"+pad) + "\n"
}

// server.url is HTTPS, the transport aries uses when it is set; server.host
// and its host key are then needed only for SSH beside it.
func TestParseConfigURL(t *testing.T) {
	t.Parallel()
	ca := caPEM(t)
	tests := []struct {
		name string
		in   string
		url  string
		err  string
	}{
		{name: "a URL alone", in: "server:\n  url: https://aries.example.com\n", url: "https://aries.example.com"},
		{name: "a URL normalized", in: "server:\n  url: HTTPS://Aries.Example.com:443/\n", url: "https://aries.example.com"},
		{name: "a URL and SSH", in: "server:\n  url: https://aries.example.com:8443\n  host: aries.example.com\n  host_key: " + testFpr + "\n", url: "https://aries.example.com:8443"},
		{name: "a CA beside it", in: "server:\n  url: https://aries.example.com\n  ca: |\n" + indent(ca, 4), url: "https://aries.example.com"},
		{name: "a CA file in the repository", in: "server:\n  url: https://aries.example.com\n  ca: certs/ca.pem\n", url: "https://aries.example.com"},
		{name: "a URL not https", in: "server:\n  url: http://aries.example.com\n", err: "server.url"},
		{name: "a URL with a user", in: "server:\n  url: https://alice@aries.example.com\n", err: "server.url"},
		{name: "a CA that is not a certificate", in: "server:\n  url: https://aries.example.com\n  ca: |\n    -----BEGIN CERTIFICATE-----\n    bm90IGEgY2VydGlmaWNhdGU=\n    -----END CERTIFICATE-----\n", err: "server.ca"},
		{name: "a CA file outside the repository", in: "server:\n  url: https://aries.example.com\n  ca: /etc/ssl/ca.pem\n", err: "server.ca"},
		{name: "a CA file climbing out", in: "server:\n  url: https://aries.example.com\n  ca: ../ca.pem\n", err: "server.ca"},
		{name: "a host without its key", in: "server:\n  url: https://aries.example.com\n  host: aries.example.com\n", err: "host_key"},
		{name: "neither", in: "server:\n  port: 22\n", err: "server.url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := ParseConfig([]byte("project: " + testProject + "\n" + tc.in))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("ParseConfig(%s) = %v, want an error naming %s", tc.name, err, tc.err)
				}
				return
			}
			if err != nil || c.Server.URL != tc.url {
				t.Errorf("ParseConfig(%s) = url %q, %v; want %q", tc.name, c.Server.URL, err, tc.url)
			}
		})
	}
}

// Which transport a config is reached by: HTTPS when server.url is
// set, SSH otherwise, and ARIES_TRANSPORT to choose where both are.
func TestConfigTransport(t *testing.T) {
	t.Parallel()
	both := "project: " + testProject + "\nserver:\n  url: https://aries.example.com\n  host: aries.example.com\n  host_key: " + testFpr + "\n"
	httpsOnly := "project: " + testProject + "\nserver:\n  url: https://aries.example.com\n"
	sshOnly := "project: " + testProject + "\nserver:\n  host: aries.example.com\n  host_key: " + testFpr + "\n"
	tests := []struct {
		name, in, env string
		want          string
		ids           []string
		err           string
	}{
		{name: "both", in: both, want: TransportHTTPS, ids: []string{"https://aries.example.com"}},
		{name: "both, SSH chosen", in: both, env: "ssh", want: TransportSSH, ids: []string{testFpr}},
		{name: "both, HTTPS chosen", in: both, env: "https", want: TransportHTTPS, ids: []string{"https://aries.example.com"}},
		{name: "HTTPS alone", in: httpsOnly, want: TransportHTTPS},
		{name: "SSH alone", in: sshOnly, want: TransportSSH, ids: []string{testFpr}},
		{name: "SSH chosen without a host", in: httpsOnly, env: "ssh", err: "server.host"},
		{name: "HTTPS chosen without a URL", in: sshOnly, env: "https", err: "server.url"},
		{name: "an unknown transport", in: both, env: "telnet", err: EnvTransport},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := ParseConfig([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			getenv := func(k string) string { return map[string]string{EnvTransport: tc.env}[k] }
			got, err := c.Transport(getenv)
			if tc.err != "" {
				pe, ok := err.(*proto.Error)
				if !ok || !strings.Contains(err.Error(), tc.err) || pe.Fix == "" {
					t.Errorf("Transport(%s) = %q, %v; want a refusal naming %s, with a fix", tc.name, got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("Transport(%s) = %q, %v; want %q", tc.name, got, err, tc.want)
			}
			if tc.ids != nil {
				if ids := c.ServerIDs(getenv); !slices.Equal(ids, tc.ids) {
					t.Errorf("ServerIDs(%s) = %q, want %q", tc.name, ids, tc.ids)
				}
			}
		})
	}
}

// The CA a config names is added to the system's roots for this server,
// never in their place; one in a file is read from the repository.
func TestConfigRoots(t *testing.T) {
	t.Parallel()
	ca := caPEM(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "certs", "ca.pem"), []byte(ca), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, ca string }{{"inline", "  ca: |\n" + indent(ca, 4)}, {"a file", "  ca: certs/ca.pem\n"}} {
		c, err := ParseConfig([]byte("project: " + testProject + "\nserver:\n  url: https://aries.example.com\n" + tc.ca))
		if err != nil {
			t.Fatal(err)
		}
		c.Root = dir
		pool, err := c.Roots()
		if err != nil || pool == nil {
			t.Fatalf("Roots(%s) = %v, %v", tc.name, pool, err)
		}
		block, _ := pem.Decode([]byte(ca))
		cert, _ := x509.ParseCertificate(block.Bytes)
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
			t.Errorf("Roots(%s) does not trust the CA it names: %v", tc.name, err)
		}
	}
	c, err := ParseConfig([]byte("project: " + testProject + "\nserver:\n  url: https://aries.example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if pool, err := c.Roots(); err != nil || pool != nil {
		t.Errorf("Roots() with no CA = %v, %v; want nil, the system's own", pool, err)
	}
}

// The config printed for a new project carries the server's URL and CA.
func TestConfigForProjectURL(t *testing.T) {
	t.Parallel()
	const other = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	ca := caPEM(t)
	for _, tc := range []struct{ name, in, want string }{
		{"HTTPS alone", "project: " + testProject + "\nserver:\n  url: https://aries.example.com\n",
			"project: " + other + "\nserver:\n  url: https://aries.example.com\n"},
		{"HTTPS and SSH", "project: " + testProject + "\nserver:\n  url: https://aries.example.com\n  host: aries.example.com\n  host_key: " + testFpr + "\n",
			"project: " + other + "\nserver:\n  url: https://aries.example.com\n  host: aries.example.com\n  host_key: " + testFpr + "\n"},
		{"a CA", "project: " + testProject + "\nserver:\n  url: https://aries.example.com\n  ca: |\n" + indent(ca, 4),
			"project: " + other + "\nserver:\n  url: https://aries.example.com\n  ca: |\n" + indent(ca, 4)},
	} {
		c, err := ParseConfig([]byte(tc.in))
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.ForProject(other)
		if err != nil || string(got) != tc.want {
			t.Errorf("ForProject(%s) = %v\n%s\nwant\n%s", tc.name, err, got, tc.want)
		}
		if _, err := ParseConfig(got); err != nil {
			t.Errorf("ForProject(%s) printed a config that does not parse: %v", tc.name, err)
		}
	}
}
