package server

import (
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// resolveFile resolves the settings of a config file holding body.
func resolveFile(t *testing.T, body string) (Settings, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ariesd.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return ResolveSettings(Settings{}, p, func(string) string { return "" })
}

// transports: is [https] unless the file says otherwise (transport.md
// §11.1, decision 87): SSH is off by default.
func TestResolveSettingsTransports(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want []string
		err  string
	}{
		{name: "the default", body: "", want: []string{TransportHTTPS}},
		{name: "both", body: "transports: [https, ssh]\n", want: []string{TransportHTTPS, TransportSSH}},
		{name: "ssh alone", body: "transports: [ssh]\n", want: []string{TransportSSH}},
		{name: "none", body: "transports: []\n", err: "at least one"},
		{name: "an unknown one", body: "transports: [https, telnet]\n", err: `"telnet"`},
		{name: "one twice", body: "transports: [ssh, ssh]\n", err: "twice"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := resolveFile(t, tc.body)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "fix:") {
					t.Errorf("ResolveSettings(%q) = %v, want an error naming %s, with a fix", tc.body, err, tc.err)
				}
				return
			}
			if err != nil || !slices.Equal(s.Transports, tc.want) {
				t.Errorf("ResolveSettings(%q).Transports = %q, %v; want %q", tc.body, s.Transports, err, tc.want)
			}
			if s.SSH() != slices.Contains(tc.want, TransportSSH) || s.HTTPSOn() != slices.Contains(tc.want, TransportHTTPS) {
				t.Errorf("ResolveSettings(%q): SSH() %v, HTTPSOn() %v; want them to say what transports names", tc.body, s.SSH(), s.HTTPSOn())
			}
		})
	}
}

// https: is checked for its shape when the file is read, and for being
// there at all when serve starts with HTTPS on (CheckServe).
func TestResolveSettingsHTTPS(t *testing.T) {
	t.Parallel()
	const files = "https:\n  listen: \":443\"\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: files\n    files:\n      cert: /etc/aries/tls/cert.pem\n      key: /etc/aries/tls/key.pem\n"
	tests := []struct {
		name  string
		body  string
		check func(Settings) bool
		err   string // from ResolveSettings
		serve string // from CheckServe
	}{
		{name: "files", body: files, check: func(s Settings) bool {
			h := s.HTTPS
			return h.Listen == ":443" && h.TLS.Mode == "files" && h.TLS.Files.Cert == "/etc/aries/tls/cert.pem" &&
				slices.Equal(h.PublicURLs, []string{"https://aries.example.com"}) && h.TLS.MinVersion == "1.2"
		}},
		{name: "acme", body: "https:\n  listen: \":443\"\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: acme\n    acme:\n      domains: [aries.example.com]\n      email: ops@example.com\n",
			check: func(s Settings) bool {
				return s.HTTPS.TLS.ACME.Cache == DefaultACMECache && slices.Equal(s.HTTPS.TLS.ACME.Domains, []string{"aries.example.com"})
			}},
		{name: "proxy", body: "https:\n  listen: 127.0.0.1:8443\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: proxy\n    proxy:\n      trusted: [127.0.0.1, 10.0.0.0/8]\n",
			check: func(s Settings) bool {
				p, err := s.HTTPS.Trusted()
				return err == nil && slices.Equal(p, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8")})
			}},
		// A peer's address is compared unmapped, so an IPv4 address or
		// network written in IPv6's form is taken as IPv4's: else it
		// would match no peer, and the proxy would be refused.
		{name: "proxy trusting IPv4 in IPv6's form", body: "https:\n  listen: 127.0.0.1:8443\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: proxy\n    proxy:\n      trusted: [\"::ffff:10.0.0.5\", \"::ffff:10.0.0.0/104\", \"::ffff:0.0.0.0/96\", \"fd00::/8\"]\n",
			check: func(s Settings) bool {
				p, err := s.HTTPS.Trusted()
				return err == nil && slices.Equal(p, []netip.Prefix{netip.MustParsePrefix("10.0.0.5/32"), netip.MustParsePrefix("10.0.0.0/8"),
					netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("fd00::/8")})
			}},
		{name: "TLS 1.3 only", body: strings.Replace(files, "    mode: files\n", "    mode: files\n    min_version: \"1.3\"\n", 1),
			check: func(s Settings) bool { return s.HTTPS.TLS.MinVersion == "1.3" }},
		{name: "HTTPS on with nothing set", body: "", serve: "https:"},
		{name: "SSH alone needs nothing", body: "transports: [ssh]\n", check: func(Settings) bool { return true }},
		{name: "no listen", body: strings.Replace(files, "  listen: \":443\"\n", "", 1), serve: "listen"},
		{name: "a listen that is no address", body: strings.Replace(files, `":443"`, "aries.example.com", 1), err: "listen"},
		{name: "no URL", body: strings.Replace(files, "  public_urls: [https://aries.example.com]\n", "", 1), serve: "public_urls"},
		{name: "a URL not https", body: strings.Replace(files, "https://aries", "http://aries", 1), err: "public_urls"},
		{name: "no mode", body: strings.Replace(files, "    mode: files\n", "", 1), serve: "mode"},
		{name: "an unknown mode", body: strings.Replace(files, "mode: files", "mode: magic", 1), err: `"magic"`},
		{name: "files without a key", body: strings.Replace(files, "      key: /etc/aries/tls/key.pem\n", "", 1), err: "key"},
		{name: "files with a relative cert", body: strings.Replace(files, "cert: /etc/aries/tls/cert.pem", "cert: cert.pem", 1), err: "absolute"},
		{name: "acme without a domain", body: "https:\n  listen: \":443\"\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: acme\n", err: "domains"},
		{name: "acme with a directory not https", body: "https:\n  listen: \":443\"\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: acme\n    acme:\n      domains: [aries.example.com]\n      directory: http://ca.example.com/dir\n", err: "directory"},
		{name: "proxy with no one trusted", body: "https:\n  listen: 127.0.0.1:8443\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: proxy\n", err: "trusted"},
		{name: "proxy trusting a name", body: "https:\n  listen: 127.0.0.1:8443\n  public_urls: [https://aries.example.com]\n  tls:\n    mode: proxy\n    proxy:\n      trusted: [proxy.example.com]\n", err: "proxy.example.com"},
		{name: "an unknown TLS version", body: strings.Replace(files, "    mode: files\n", "    mode: files\n    min_version: \"1.1\"\n", 1), err: "min_version"},
		{name: "an unknown key", body: files + "  client_ca: /etc/aries/ca.pem\n", err: "client_ca"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := resolveFile(t, tc.body)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Errorf("ResolveSettings(%s) = %v, want an error naming %s", tc.name, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSettings(%s) = %v", tc.name, err)
			}
			serr := s.CheckServe()
			if tc.serve != "" {
				if serr == nil || !strings.Contains(serr.Error(), tc.serve) || !strings.Contains(serr.Error(), "fix:") {
					t.Errorf("CheckServe(%s) = %v, want an error naming %s, with a fix", tc.name, serr, tc.serve)
				}
				return
			}
			if serr != nil || !tc.check(s) {
				t.Errorf("ResolveSettings(%s).HTTPS = %+v, CheckServe %v", tc.name, s.HTTPS, serr)
			}
		})
	}
}
