package server

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/telemetry"
	"github.com/ariesworx/aries/internal/wsconn"
)

// checkSoon is check, once wants hold or patience has passed: what the
// listener counts after the client can see it, such as a connection the
// client closed, is counted a moment later.
func checkSoon(t *testing.T, m *telemetry.Manual, wants []want) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for !allHold(snapshot(t, m), wants) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	check(t, snapshot(t, m), wants)
}

// Every upgrade and key proof over HTTPS is counted by how it ended,
// a TLS handshake that did not complete is counted, and the listener's
// connections are counted up and down.
func TestMetricsOfHTTPS(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, false)
	cfg := Config{Telemetry: tel, Limits: Limits{AuthFailureRate: 1, AuthFailureBurst: 3},
		AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
	u, pool := listenHTTPS(t, &cfg, nil)
	s, _ := newServerConfig(t, cfg, nil)
	serving(t, s)
	dial := func() *wsconn.Conn {
		t.Helper()
		c, _, err := wsDial(t, u, pool, nil)
		if err != nil {
			t.Fatalf("dial %s: %v", u, err)
		}
		return c
	}
	// refused reads the refusal that ends a key proof, if prove has not.
	refused := func(c *wsconn.Conn, f *proto.Frame, what string) {
		t.Helper()
		if f == nil {
			msg, err := c.Read()
			if f = refusalIn(msg); f == nil {
				t.Fatalf("%s: read %q, %v; want a refusal", what, msg, err)
			}
		}
		_ = c.Close()
	}

	held := dial()
	if f := prove(t, held, u, signerOf(t, "alice"), nil); f != nil {
		t.Fatalf("alice's key proof refused: %v", f.Err)
	}
	if w := hello(t, held); w.Err != nil {
		t.Fatalf("alice's hello over HTTPS: %v", w.Err)
	}
	_ = dial().Close() // upgraded, and left without a word
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u+keyproof.ConnectPath, nil)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET %s without an upgrade = %v, %v; want 400", keyproof.ConnectPath, resp, err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	tr.CloseIdleConnections()
	c := dial()
	if err := c.Write([]byte(`{"t":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	refused(c, nil, "a hello in place of the key offer")
	// These three spend the address's burst of refusals, 3.
	c = dial()
	refused(c, prove(t, c, "https://aries.example.com", signerOf(t, "alice"), nil), "a proof for another server")
	c = dial()
	refused(c, prove(t, c, u, signerOf(t, "mallory"), nil), "a key the server does not know")
	c = dial()
	refused(c, prove(t, c, u, signerOf(t, "alice"), signerOf(t, "mallory")), "a proof signed by another key")
	for _, tc := range []struct {
		name   string
		header http.Header
		status int
	}{
		{"a browser's", http.Header{"Origin": {"https://evil.example.com"}}, http.StatusForbidden},
		{"past the address's refused proofs", nil, http.StatusTooManyRequests},
	} {
		if _, resp, err := wsDial(t, u, pool, tc.header); err == nil || resp == nil || resp.StatusCode != tc.status {
			t.Errorf("an upgrade %s = %v, %v; want %d", tc.name, resp, err, tc.status)
		}
	}
	// A client that does not trust the certificate ends the handshake.
	if tc, err := tls.Dial("tcp", cfg.HTTPS.Listener.Addr().String(), &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}); err == nil {
		_ = tc.Close()
		t.Error("a TLS handshake with a client that trusts no certificate completed")
	}

	checkSoon(t, m, []want{
		{"aries.https.upgrades", []string{"result", "ok", "reason", "none"}, 6},
		{"aries.https.upgrades", []string{"result", "refused", "reason", "origin"}, 1},
		{"aries.https.upgrades", []string{"result", "refused", "reason", "auth_failures"}, 1},
		{"aries.https.upgrades", []string{"result", "refused", "reason", "bad_request"}, 1},
		{"aries.https.key_proofs", []string{"result", "ok"}, 1},
		{"aries.https.key_proofs", []string{"result", "invalid"}, 1},
		{"aries.https.key_proofs", []string{"result", "url"}, 1},
		{"aries.https.key_proofs", []string{"result", "unknown_key"}, 1},
		{"aries.https.key_proofs", []string{"result", "bad_proof"}, 1},
		{"aries.https.key_proofs", []string{"result", "failed"}, 1},
		{"aries.https.tls_handshakes.failed", nil, 1},
		{"aries.limit.refusals", []string{"limit", "auth_failures_per_minute"}, 1},
		{"aries.https.connections", nil, 1}, // alice's
	})
	_ = held.Close()
	checkSoon(t, m, []want{{"aries.https.connections", nil, 0}})
}

// The listener counts a connection it closed unanswered: from an address
// it does not trust behind a proxy, or past one of its limits, which is
// counted by name as well.
func TestMetricsOfHTTPSListener(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		edit   func(*Config)
		reason string
		limit  string
	}{
		{"untrusted", func(c *Config) {
			c.HTTPS.TLS, c.HTTPS.Trusted, c.HTTPS.URLs = nil, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, []string{"https://aries.example.com"}
		}, "untrusted", ""},
		{"https_conns_per_server", func(c *Config) { c.Limits.HTTPSConns = 1 }, "limit", "https_conns_per_server"},
		{"https_conns_per_addr", func(c *Config) { c.Limits.HTTPSConnsPerAddr = 1 }, "limit", "https_conns_per_addr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tel, m := measured(t, false)
			cfg := Config{Telemetry: tel, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
			listenHTTPS(t, &cfg, nil)
			tc.edit(&cfg)
			s, _ := newServerConfig(t, cfg, nil)
			serving(t, s)
			addr := cfg.HTTPS.Listener.Addr().String()
			if tc.limit != "" {
				first, err := net.Dial("tcp", addr) // holds the one connection allowed
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = first.Close() })
				checkSoon(t, m, []want{{"aries.https.connections", nil, 1}})
			}
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			_ = c.SetReadDeadline(time.Now().Add(patience))
			if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
				t.Fatalf("a connection %s was answered (%d bytes, %v), want it closed", tc.name, n, err)
			}
			wants := []want{{"aries.https.connections.refused", []string{"reason", tc.reason}, 1}}
			if tc.limit != "" {
				wants = append(wants, want{"aries.limit.refusals", []string{"limit", tc.limit}, 1})
			}
			checkSoon(t, m, wants)
		})
	}
}
