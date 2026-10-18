package server

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/wsconn"
)

// testCert makes a CA and a certificate it signs for 127.0.0.1 and
// localhost, and returns the server's TLS configuration and a pool that
// trusts the CA.
func testCert(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "aries"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}, pool
}

// listenHTTPS gives cfg an HTTPS listener on loopback with a test
// certificate, edited by edit when it is not nil, and returns the
// server's URL and a pool that trusts its certificate.
func listenHTTPS(t *testing.T, cfg *Config, edit func(*HTTPS)) (string, *x509.CertPool) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	tc, pool := testCert(t)
	u := "https://" + l.Addr().String()
	cfg.HTTPS = &HTTPS{Listener: l, TLS: tc, URLs: []string{u}}
	if edit != nil {
		edit(cfg.HTTPS)
	}
	return u, pool
}

// serving runs s.Serve, with a socket no one dials, until the test ends.
func serving(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, newFakeListener()) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve = %v", err)
			}
		case <-time.After(patience):
			t.Error("Serve did not return after its context ended")
		}
	})
}

// handFile writes an authorized_keys file kept by hand in a directory of
// its own, with lines, and returns its path.
func handFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// ed25519Of is the private half of owner's test key (testKey).
func ed25519Of(owner string) ed25519.PrivateKey {
	sum := sha256.Sum256([]byte(owner))
	return ed25519.NewKeyFromSeed(sum[:])
}

// signerOf is the signer of owner's test key (testKey).
func signerOf(t *testing.T, owner string) ssh.Signer {
	t.Helper()
	_, _, _, pub := testKey(t, owner)
	s, err := ssh.NewSignerFromKey(ed25519Of(owner))
	if err != nil {
		t.Fatal(err)
	}
	if string(s.PublicKey().Marshal()) != string(pub.Marshal()) {
		t.Fatal("signerOf: not testKey's key")
	}
	return s
}

// bridgeLine is a line of the hand-kept file that forces the bridge for
// principal with owner's test key.
func bridgeLine(t *testing.T, owner, principal string) string {
	t.Helper()
	typ, blob, _, _ := testKey(t, owner)
	return fmt.Sprintf(`restrict,command="/usr/local/bin/ariesd stdio --principal %s" %s %s`, principal, typ, blob)
}

// wsDial opens /v1/connect at u, trusting pool, with header.
func wsDial(t *testing.T, u string, pool *x509.CertPool, header http.Header) (*wsconn.Conn, *http.Response, error) {
	t.Helper()
	c, resp, err := wsconn.Dial(t.Context(), keyproof.ConnectURL(u), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, header, 0)
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, resp, err
}

// prove runs the key proof on c as aries does, offering signer's key for
// the server at offered, and signing with sign (signer itself when nil)
// the v2 transcript, bound to c's TLS session when the challenge asks.
// It returns the server's refusal, a welcome frame with its error, or
// nil once the proof is sent.
func prove(t *testing.T, c *wsconn.Conn, offered string, signer, sign ssh.Signer) *proto.Frame {
	t.Helper()
	return proveAs(t, c, offered, signer, sign, 2, nil)
}

// proveV1 is prove as v0.5.0's aries, of protocol 8, proves: the v1
// transcript, whatever the challenge asks.
func proveV1(t *testing.T, c *wsconn.Conn, offered string, signer ssh.Signer) *proto.Frame {
	t.Helper()
	return proveAs(t, c, offered, signer, nil, 1, nil)
}

// proveAs is prove with the transcript of version v, bound to exporter
// where the challenge asks, or to c's TLS session's when it is nil.
func proveAs(t *testing.T, c *wsconn.Conn, offered string, signer, sign ssh.Signer, v int, exporter []byte) *proto.Frame {
	t.Helper()
	if sign == nil {
		sign = signer
	}
	if err := c.Write(keyproof.NewOffer(offered, []ssh.PublicKey{signer.PublicKey()})); err != nil {
		t.Fatalf("send the key offer: %v", err)
	}
	m, err := c.Read()
	if err != nil {
		t.Fatalf("read the answer to the key offer: %v", err)
	}
	if f := refusalIn(m); f != nil {
		return f
	}
	ch, err := keyproof.ParseChallenge(m)
	if err != nil {
		t.Fatalf("the answer to the key offer: %v (%q)", err, m)
	}
	if v == 1 {
		sig, err := keyproof.Sign(sign, offered, ch.Nonce)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Write(keyproof.NewProof(sig)); err != nil {
			t.Fatalf("send the key proof: %v", err)
		}
		return nil
	}
	switch ch.Bind {
	case keyproof.BindTLSExporter:
		if exporter == nil {
			if exporter, err = keyproof.Exporter(c.TLS()); err != nil {
				t.Fatalf("the challenge asks for the TLS exporter: %v", err)
			}
		}
	case keyproof.BindNone:
		exporter = nil
	default:
		t.Fatalf("the challenge asks for the binding %q", ch.Bind)
	}
	sig, err := keyproof.SignV2(sign, offered, ch.Nonce, exporter)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(keyproof.NewProofV2(sig)); err != nil {
		t.Fatalf("send the key proof: %v", err)
	}
	return nil
}

// refusalIn is m as a welcome frame that refuses, or nil.
func refusalIn(m []byte) *proto.Frame {
	var f proto.Frame
	if json.Unmarshal(m, &f) != nil || f.T != proto.FrameWelcome || f.Err == nil {
		return nil
	}
	return &f
}

// hello sends a hello on c and returns the welcome.
func hello(t *testing.T, c *wsconn.Conn) *proto.Frame { return helloTo(t, c, project) }

// helloTo is hello for project proj.
func helloTo(t *testing.T, c *wsconn.Conn, proj string) *proto.Frame {
	t.Helper()
	return helloProto(t, c, proj, proto.Proto)
}

// helloProto is helloTo from a client of protocol p.
func helloProto(t *testing.T, c *wsconn.Conn, proj string, p int) *proto.Frame {
	t.Helper()
	s := wsconn.Stream(c)
	if err := proto.NewEncoder(s).Encode(&proto.Frame{T: proto.FrameHello, Proto: p, Project: proj, Session: "s-h", Machine: "m"}); err != nil {
		t.Fatalf("send the hello: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(patience))
	f, err := proto.NewDecoder(s).Decode()
	if err != nil {
		t.Fatalf("read the welcome: %v", err)
	}
	return f
}

// httpsServer is a server of one project, with a real store, whose
// hand-kept authorized_keys gives alice her key, served over HTTPS.
func httpsServer(t *testing.T, edit func(*HTTPS)) (*Server, string, *x509.CertPool) {
	t.Helper()
	cfg := Config{AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"),
		`command="/bin/sh -c 'echo --principal bob'" `+strings.Join(strings.Fields(keyLine(t, "bob"))[:2], " "),
		bridgeLine(t, "importer", "import"))}
	u, pool := listenHTTPS(t, &cfg, edit)
	s, _ := newServerConfig(t, cfg, nil)
	serving(t, s)
	return s, u, pool
}

// A key the hand-kept file gives alice proves itself over HTTPS, and the
// connection is then alice's, served as one through sshd is.
func TestHTTPSWelcomesKey(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", u, err)
	}
	if f := prove(t, c, u, signerOf(t, "alice"), nil); f != nil {
		t.Fatalf("alice's key proof refused: %v", f.Err)
	}
	w := hello(t, c)
	if w.Err != nil || w.Principal != "alice" || w.Session != "s-h" {
		t.Fatalf("the welcome after alice's proof = %+v, %v; want alice's", w, w.Err)
	}
	s := wsconn.Stream(c)
	req, _ := proto.Request(1, proto.OpCreate, proto.CreateArgs{Title: "over HTTPS"})
	if err := proto.NewEncoder(s).Encode(req); err != nil {
		t.Fatal(err)
	}
	res, err := proto.NewDecoder(s).Decode()
	if err != nil || res.Err != nil {
		t.Fatalf("create over HTTPS = %+v, %v", res, err)
	}
	var wr proto.CreateResult
	if err := json.Unmarshal(res.OK, &wr); err != nil || !strings.HasPrefix(wr.ID, "ar-") {
		t.Errorf("create over HTTPS = %s, %v; want an issue", res.OK, err)
	}
}

// Every way the key proof fails is refused with a welcome that says so,
// before any hello is read.
func TestHTTPSKeyProofRefusals(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	tests := []struct {
		name    string
		offered string
		signer  string // whose key is offered
		sign    string // whose key signs; "" for the same
		code    proto.Code
		message string
	}{
		{name: "a key the server does not know", offered: u, signer: "mallory", code: proto.CodeAuth, message: "none of the keys"},
		{name: "a line that does not force the bridge", offered: u, signer: "bob", code: proto.CodeAuth, message: "none of the keys"},
		{name: "a line for a reserved principal", offered: u, signer: "importer", code: proto.CodeAuth, message: "none of the keys"},
		{name: "a proof for another server", offered: "https://aries.example.com", signer: "alice", code: proto.CodeAuth, message: "aries.example.com"},
		{name: "a proof signed by another key", offered: u, signer: "alice", sign: "mallory", code: proto.CodeAuth, message: "does not verify"},
	}
	for _, tc := range tests {
		// In turn: the test holds a Postgres server (CONTRIBUTING.md).
		t.Run(tc.name, func(t *testing.T) {
			c, _, err := wsDial(t, u, pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			var sign ssh.Signer
			if tc.sign != "" {
				sign = signerOf(t, tc.sign)
			}
			f := prove(t, c, tc.offered, signerOf(t, tc.signer), sign)
			if f == nil {
				m, err := c.Read()
				if f = refusalIn(m); f == nil {
					t.Fatalf("after %s: read %q, %v; want a refusal", tc.name, m, err)
				}
			}
			if f.Err.Code != tc.code || !strings.Contains(f.Err.Message, tc.message) || f.Err.Fix == "" {
				t.Errorf("%s: refused %+v; want %s naming %q, with a fix", tc.name, f.Err, tc.code, tc.message)
			}
			if _, err := c.Read(); err == nil {
				t.Errorf("%s: the connection stayed open after the refusal", tc.name)
			}
		})
	}
}

// A hello in place of the key offer is refused: nothing reaches the
// protocol before a key has proved itself.
func TestHTTPSHelloWithoutProof(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := wsconn.Stream(c)
	if err := proto.NewEncoder(s).Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: project, Principal: "alice"}); err != nil {
		t.Fatal(err)
	}
	m, err := c.Read()
	f := refusalIn(m)
	if f == nil || f.Err.Code != proto.CodeInvalid || f.Principal != "" {
		t.Fatalf("a hello in place of the key offer: %q, %v; want a refusal that names no one", m, err)
	}
	if _, err := c.Read(); err == nil {
		t.Error("the connection stayed open after a hello without a proof")
	}
}

// A browser's page could open the WebSocket on a person's machine; a
// request with an Origin is refused before the upgrade.
func TestHTTPSRefusesBrowsers(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	_, resp, err := wsDial(t, u, pool, http.Header{"Origin": {"https://evil.example.com"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("an upgrade with an Origin = %v, %v; want 403", resp, err)
	}
	var body struct{ Error proto.Error }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Error.Code != proto.CodeForbidden || body.Error.Fix == "" {
		t.Errorf("the refusal of an Origin: %+v, %v; want a proto.Error, forbidden, with a fix", body, err)
	}
}

// The listener serves its two paths and nothing else: no path of the
// loopback listener, no status, no metrics.
func TestHTTPSPaths(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u+path, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	resp, body := get("/.well-known/aries")
	var wk struct {
		Version    string   `json:"version"`
		Transports []string `json:"transports"`
		Auth       []string `json:"auth"`
	}
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &wk) != nil || wk.Version != "v0.2.0" ||
		strings.Join(wk.Transports, ",") != "https,ssh" || strings.Join(wk.Auth, ",") != "key" {
		t.Errorf("GET /.well-known/aries = %d %s; want the version, the transports and the ways to sign in", resp.StatusCode, body)
	}
	if strings.Contains(body, "alice") || resp.ProtoMajor != 2 {
		t.Errorf("GET /.well-known/aries over %s = %s; want HTTP/2 and no principal", resp.Proto, body)
	}
	if h := resp.Header.Get("Strict-Transport-Security"); !strings.Contains(h, "max-age=") {
		t.Errorf("GET /.well-known/aries: Strict-Transport-Security %q, want it set", h)
	}
	for _, path := range []string{"/", "/metrics", "/healthz", "/readyz", "/status", "/v1/connect/x", "/.well-known/aries/x"} {
		if resp, _ := get(path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
	if resp, _ := get("/v1/connect"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /v1/connect without an upgrade = %d, want 400", resp.StatusCode)
	}
}

// Behind a proxy that terminates TLS, the listener speaks plain HTTP to
// the addresses it trusts and closes a connection from any other.
func TestHTTPSBehindProxy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		trusted string
		ok      bool
	}{
		{name: "from the proxy", trusted: "127.0.0.1/32", ok: true},
		{name: "from elsewhere", trusted: "10.0.0.0/8", ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
			listenHTTPS(t, &cfg, func(h *HTTPS) {
				h.TLS, h.Trusted, h.URLs = nil, []netip.Prefix{netip.MustParsePrefix(tc.trusted)}, []string{"https://aries.example.com"}
			})
			addr := cfg.HTTPS.Listener.Addr().String()
			s, _ := newServerConfig(t, cfg, nil)
			serving(t, s)
			c, _, err := wsconn.Dial(t.Context(), "ws://"+addr+keyproof.ConnectPath, nil, nil, 0)
			if !tc.ok {
				if err == nil {
					_ = c.Close()
					t.Fatalf("a connection %s was served", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("dial through the proxy's address: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			// The proof names the URL clients use, which the proxy
			// serves: never what the request's Host said.
			if f := prove(t, c, "https://aries.example.com", signerOf(t, "alice"), nil); f != nil {
				t.Fatalf("alice's proof through the proxy: %v", f.Err)
			}
			if w := hello(t, c); w.Err != nil || w.Principal != "alice" {
				t.Errorf("alice through the proxy: %+v", w)
			}
		})
	}
}

// A client that has upgraded and says nothing is closed after
// upgrade_timeout.
func TestHTTPSUpgradeTimeout(t *testing.T) {
	t.Parallel()
	cfg := Config{Limits: Limits{UpgradeTimeout: Duration(50 * time.Millisecond)}, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
	u, pool := listenHTTPS(t, &cfg, nil)
	s, _ := newServerConfig(t, cfg, nil)
	serving(t, s)
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(patience))
	if m, err := c.Read(); err == nil {
		t.Errorf("a silent connection read %q, want it closed", m)
	} else if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		t.Error("a silent connection was not closed by the server")
	}
}

// The listener's caps (rule 18): connections in all and from one
// address, the headers' size, and refusals of the key proof from one
// address, past which an upgrade is refused busy.
func TestHTTPSLimits(t *testing.T) {
	t.Parallel()
	t.Run("connections from one address", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Limits: Limits{HTTPSConnsPerAddr: 2}, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
		u, pool := listenHTTPS(t, &cfg, nil)
		s, _ := newServerConfig(t, cfg, nil)
		serving(t, s)
		for i := range 2 {
			if _, _, err := wsDial(t, u, pool, nil); err != nil {
				t.Fatalf("connection %d of 2: %v", i+1, err)
			}
		}
		if c, _, err := wsDial(t, u, pool, nil); err == nil {
			_ = c.Close()
			t.Error("a third connection from one address was served, past https_conns_per_addr of 2")
		}
	})
	t.Run("connections in all", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Limits: Limits{HTTPSConns: 1}, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
		u, pool := listenHTTPS(t, &cfg, nil)
		s, _ := newServerConfig(t, cfg, nil)
		serving(t, s)
		if _, _, err := wsDial(t, u, pool, nil); err != nil {
			t.Fatal(err)
		}
		if c, _, err := wsDial(t, u, pool, nil); err == nil {
			_ = c.Close()
			t.Error("a second connection was served, past https_conns_per_server of 1")
		}
	})
	t.Run("headers", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Limits: Limits{HTTPSHeaderBytes: 1 << 10}, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
		u, pool := listenHTTPS(t, &cfg, nil)
		s, _ := newServerConfig(t, cfg, nil)
		serving(t, s)
		_, resp, err := wsDial(t, u, pool, http.Header{"X-Big": {strings.Repeat("x", 16<<10)}})
		if err == nil || resp == nil || resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Errorf("an upgrade with 16 KiB of headers, past https_header_bytes of 1 KiB = %v, %v; want 431", resp, err)
		}
	})
	// A connection kept alive after one answered request and then left
	// idle holds a place for no traffic: it has https_header_timeout to
	// send its next request's headers, as a new connection would.
	t.Run("an idle connection", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Limits: Limits{HTTPSHeaderTimeout: Duration(100 * time.Millisecond)}, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
		u, pool := listenHTTPS(t, &cfg, nil)
		s, _ := newServerConfig(t, cfg, nil)
		serving(t, s)
		c, err := tls.Dial("tcp", strings.TrimPrefix(u, "https://"), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := io.WriteString(c, "GET /.well-known/aries HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		const hold = 3 * time.Second // 30 times the headers' timeout
		_ = c.SetReadDeadline(time.Now().Add(hold))
		if _, err := br.ReadByte(); !errors.Is(err, os.ErrDeadlineExceeded) {
			return // closed by the server
		}
		t.Errorf("a connection idle after its answer was open %v later, past https_header_timeout of 100ms", hold)
	})
	t.Run("body timeout", func(t *testing.T) {
		t.Parallel()
		// A request that names a body and sends none, or sends it a byte
		// at a time, is cut off within https_body_timeout, and its slot
		// (here the only one) is free again.
		for _, tc := range []struct {
			name  string
			drip  bool
			extra string
		}{{name: "no body"}, {name: "a byte at a time", drip: true}} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				cfg := Config{Limits: Limits{HTTPSBodyTimeout: Duration(300 * time.Millisecond), HTTPSConns: 1}, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
				u, pool := listenHTTPS(t, &cfg, nil)
				s, _ := newServerConfig(t, cfg, nil)
				serving(t, s)
				nc, err := tls.Dial("tcp", strings.TrimPrefix(u, "https://"), &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"http/1.1"}})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = nc.Close() }()
				if _, err := nc.Write([]byte("POST /nothing HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1000\r\n\r\n")); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				done := make(chan struct{})
				if tc.drip {
					go func() {
						for {
							select {
							case <-done:
								return
							case <-time.After(50 * time.Millisecond):
								if _, err := nc.Write([]byte("x")); err != nil {
									return
								}
							}
						}
					}()
				}
				defer close(done)
				_ = nc.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, err = io.Copy(io.Discard, nc)
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					t.Fatalf("the connection was still open after %v, past https_body_timeout of 300ms", time.Since(start))
				}
				if d := time.Since(start); d > 3*time.Second {
					t.Errorf("the connection was cut off after %v; want about 300ms", d)
				}
				// The slot is free: a new connection is served.
				// (the listener counts it closed a moment after the client sees it).
				var c *wsconn.Conn
				for end := time.Now().Add(3 * time.Second); ; {
					if c, _, err = wsDial(t, u, pool, nil); err == nil {
						break
					}
					if time.Now().After(end) {
						t.Fatalf("a connection after the cut-off one: %v", err)
					}
					time.Sleep(20 * time.Millisecond)
				}
				_ = c.Close()
			})
		}
	})
	t.Run("refusals from one address", func(t *testing.T) {
		t.Parallel()
		cfg := Config{Limits: Limits{AuthFailureRate: 1, AuthFailureBurst: 2}, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
		u, pool := listenHTTPS(t, &cfg, nil)
		s, _ := newServerConfig(t, cfg, nil)
		serving(t, s)
		for range 2 {
			c, _, err := wsDial(t, u, pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			if f := prove(t, c, u, signerOf(t, "mallory"), nil); f == nil {
				t.Fatal("mallory's key was not refused")
			}
		}
		_, resp, err := wsDial(t, u, pool, nil)
		if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("an upgrade after 2 refused proofs, at a burst of 2 = %v, %v; want 429", resp, err)
		}
		var body struct{ Error proto.Error }
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Error.Code != proto.CodeBusy || !strings.Contains(body.Error.Fix, "wait") {
			t.Errorf("the refusal past the failures' limit: %+v, %v; want busy, with a fix to wait", body, err)
		}
	})
}

// The server pings a WebSocket that has nothing to say, so that a proxy
// keeps it: a relay between the two sees the pings while the client is
// silent, and the connection then answers.
func TestHTTPSPings(t *testing.T) {
	t.Parallel()
	cfg := Config{AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
	u, pool := listenHTTPS(t, &cfg, func(h *HTTPS) { h.Ping = 20 * time.Millisecond })
	s, _ := newServerConfig(t, cfg, nil)
	serving(t, s)
	// The relay forwards bytes both ways, and counts what the server
	// sends while counting is on.
	target := strings.TrimPrefix(u, "https://")
	rl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rl.Close() })
	down := make(chan struct{}, 64)
	var counting sync.Mutex
	count := false
	var wg sync.WaitGroup
	t.Cleanup(wg.Wait)
	wg.Go(func() {
		cc, err := rl.Accept()
		if err != nil {
			return
		}
		defer func() { _ = cc.Close() }()
		sc, err := net.Dial("tcp", target)
		if err != nil {
			return
		}
		defer func() { _ = sc.Close() }()
		go func() { _, _ = io.Copy(sc, cc) }()
		buf := make([]byte, 32<<10)
		for {
			n, err := sc.Read(buf)
			if n > 0 {
				counting.Lock()
				if count {
					select {
					case down <- struct{}{}:
					default:
					}
				}
				counting.Unlock()
				if _, err := cc.Write(buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})
	via := "https://" + rl.Addr().String()
	c, _, err := wsconn.Dial(t.Context(), keyproof.ConnectURL(via), &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if f := prove(t, c, u, signerOf(t, "alice"), nil); f != nil {
		t.Fatalf("alice's proof: %v", f.Err)
	}
	if w := hello(t, c); w.Err != nil {
		t.Fatalf("welcome: %v", w.Err)
	}
	counting.Lock()
	count = true
	counting.Unlock()
	for i := range 3 {
		select {
		case <-down:
		case <-time.After(patience):
			t.Fatalf("the server sent nothing to a silent client; %d pings seen, want 3", i)
		}
	}
	st := wsconn.Stream(c)
	req, _ := proto.Request(1, proto.OpReady, proto.LimitArgs{})
	if err := proto.NewEncoder(st).Encode(req); err != nil {
		t.Fatal(err)
	}
	if res, err := proto.NewDecoder(st).Decode(); err != nil || res.T != proto.FrameRes || res.Err != nil {
		t.Errorf("a request after the pings = %+v, %v; want an answer", res, err)
	}
}

// When Serve stops, it closes the HTTPS listener and every connection it
// handed off, and waits for them.
func TestHTTPSStops(t *testing.T) {
	t.Parallel()
	cfg := Config{AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
	u, pool := listenHTTPS(t, &cfg, nil)
	s, _ := newServerConfig(t, cfg, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, newFakeListener()) }()
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c, u, signerOf(t, "alice"), nil); f != nil {
		t.Fatal(f.Err)
	}
	if w := hello(t, c); w.Err != nil {
		t.Fatal(w.Err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(patience):
		t.Fatal("Serve did not return with a connection open over HTTPS")
	}
	_ = c.SetReadDeadline(time.Now().Add(patience))
	if _, err := c.Read(); err == nil {
		t.Error("the connection over HTTPS stayed open after Serve returned")
	}
	if _, _, err := wsDial(t, u, pool, nil); err == nil {
		t.Error("the listener took a connection after Serve returned")
	}
}

// httpsMulti is a server of many projects with an HTTPS listener: carol
// is a member of projA with a key in the store, and dana a server admin.
func httpsMulti(t *testing.T, edit func(*Config)) (*multi, string, *x509.CertPool) {
	t.Helper()
	var u string
	var pool *x509.CertPool
	m := newMultiWith(t, Limits{}, nil, nil, func(c *Config) {
		u, pool = listenHTTPS(t, c, nil)
		if edit != nil {
			edit(c)
		}
	}, dana.Principal)
	m.project(t, projA, "carol")
	if _, err := m.ctl.AddKey(t.Context(), dana, "carol", keyLine(t, "carol")); err != nil {
		t.Fatal(err)
	}
	m.refresh(t.Context())
	serving(t, m.Server)
	return m, u, pool
}

// A key of the control store proves itself over HTTPS; its connection
// names the key, as one by the keys file's line does, so that removing
// the key closes it.
func TestHTTPSStoreKey(t *testing.T) {
	t.Parallel()
	m, u, pool := httpsMulti(t, nil)
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c, u, signerOf(t, "carol"), nil); f != nil {
		t.Fatalf("carol's proof by her key in the store: %v", f.Err)
	}
	if w := helloTo(t, c, projA); w.Err != nil || w.Principal != "carol" {
		t.Fatalf("carol over HTTPS: %+v", w)
	}
	d := mustDialTo(t, m.Server, dana, projA)
	if rm := mustAsk[proto.KeyResult](d, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fingerprintOf(t, "carol")}); rm.Dropped != 1 {
		t.Errorf("key.remove dropped %d connections, want carol's one over HTTPS", rm.Dropped)
	}
	_ = c.SetReadDeadline(time.Now().Add(patience))
	for {
		if _, err := c.Read(); err != nil {
			if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
				t.Error("carol's connection over HTTPS stayed open after her key was removed")
			}
			break
		}
	}
	c2, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c2, u, signerOf(t, "carol"), nil); f == nil || f.Err.Code != proto.CodeAuth {
		t.Errorf("carol's proof once her key is removed = %+v, want refused auth", f)
	}
}

// A disabled principal's key of the store, proved, is refused as
// disabled, and is not a refused sign-in of its address: its agents,
// still trying, do not shut out everyone else at that address. A proof
// of it that does not verify still is one.
func TestHTTPSDisabledKey(t *testing.T) {
	t.Parallel()
	const burst = 2
	m, u, pool := httpsMulti(t, func(c *Config) { c.Limits.AuthFailureRate, c.Limits.AuthFailureBurst = 1, burst })
	if _, err := m.ctl.DisablePrincipal(t.Context(), dana, "carol"); err != nil {
		t.Fatal(err)
	}
	// refusal runs carol's key proof, signed by sign, and returns the
	// server's refusal.
	refusal := func(sign ssh.Signer) *proto.Error {
		t.Helper()
		c, resp, err := wsDial(t, u, pool, nil)
		if err != nil {
			t.Fatalf("upgrade: %v (%v)", err, resp)
		}
		f := prove(t, c, u, signerOf(t, "carol"), sign)
		if f == nil {
			m, err := c.Read()
			if f = refusalIn(m); f == nil {
				t.Fatalf("after carol's proof: read %q, %v; want a refusal", m, err)
			}
		}
		return f.Err
	}
	for i := range burst + 2 {
		if e := refusal(nil); e.Code != proto.CodeForbidden || !strings.Contains(e.Message, "carol is disabled") || !strings.Contains(e.Fix, "principal enable carol") {
			t.Fatalf("disabled carol's proof %d of %d = %+v; want forbidden, naming her disabled, with a fix to enable her", i+1, burst+2, e)
		}
	}
	for i := range burst {
		if e := refusal(signerOf(t, "mallory")); e.Code != proto.CodeAuth || !strings.Contains(e.Message, "does not verify") {
			t.Fatalf("carol's key proved by another's, %d of %d = %+v; want auth, not verified", i+1, burst, e)
		}
	}
	if _, resp, err := wsDial(t, u, pool, nil); err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("an upgrade after %d proofs that did not verify, at a burst of %d = %v, %v; want 429", burst, burst, resp, err)
	}
}

// The status report names the transports, with the connections that
// came over each.
func TestHTTPSStatus(t *testing.T) {
	t.Parallel()
	m, u, pool := httpsMulti(t, nil)
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c, u, signerOf(t, "carol"), nil); f != nil {
		t.Fatal(f.Err)
	}
	if w := helloTo(t, c, projA); w.Err != nil {
		t.Fatal(w.Err)
	}
	d := mustDialTo(t, m.Server, dana, projA)
	res := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
	tr := res.Server.Transports
	if tr == nil || tr.HTTPS == nil || tr.HTTPS.Conns != 1 || tr.HTTPS.Mode != "files" || len(tr.HTTPS.URLs) != 1 || tr.HTTPS.URLs[0] != u ||
		!tr.SSH.On || tr.SSH.Conns != 1 {
		t.Errorf("status's transports = %s; want https (files, %s) with carol's connection, and ssh on with dana's", js(tr), u)
	}
}

// New refuses an HTTPS listener it could not serve safely.
func TestNewRefusesHTTPS(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	tc, _ := testCert(t)
	for _, tc := range []struct {
		name  string
		https HTTPS
		want  string
	}{
		{name: "no listener", https: HTTPS{TLS: tc, URLs: []string{"https://aries.example.com"}}, want: "listener"},
		{name: "no URL", https: HTTPS{Listener: l, TLS: tc}, want: "URL"},
		{name: "a URL that is not https", https: HTTPS{Listener: l, TLS: tc, URLs: []string{"http://aries.example.com"}}, want: "https"},
		{name: "plain with no proxy", https: HTTPS{Listener: l, URLs: []string{"https://aries.example.com"}}, want: "trusted"},
	} {
		h := tc.https
		_, err := New(Config{Store: &fakeStore{}, Project: project, HTTPS: &h})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("New with %s = %v, want an error naming %s", tc.name, err, tc.want)
		}
	}
}
