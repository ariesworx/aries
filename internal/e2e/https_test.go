package e2e

import (
	"cmp"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/wsconn"
)

// EnvTransport chooses the transport the worlds' users connect by: ssh,
// the default, or https. The suite runs whole on either (transport.md
// §15, T1's gate); a test that is about SSH itself pins it
// (daemonOpts.ssh).
const EnvTransport = "ARIES_E2E_TRANSPORT"

// suiteTransport is the transport of this run's worlds.
var suiteTransport = cmp.Or(os.Getenv(EnvTransport), "ssh")

// overHTTPS reports whether the world's users connect over HTTPS.
func (w *world) overHTTPS() bool { return w.opts.https || suiteTransport == "https" && !w.opts.ssh }

// front is the world's HTTPS front, as startSSH's server is its sshd: a
// proxy of the operator's that terminates the client's TLS and relays
// each WebSocket message to the daemon's own HTTPS listener, which has
// TLS of its own. Relaying message by message is what lets the world
// drop a connection at a response (dropReplyTo), or leave one out
// (silentAfter), as its sshd's tap does. The daemon checks key proofs
// against the front's URL, which is what clients use, and binds them
// to the client's TLS session with the front, whose exporter the front
// hands the daemon (frontExporter): the front stands for the network
// between a client and a daemon that terminates TLS, not for a relay,
// which the binding refuses (internal/server's TestHTTPSProofRelayed).
type front struct {
	url  string // https://127.0.0.1:PORT, what users' server.url names
	ca   string // the CA's certificate in PEM, users' server.ca
	tls  *tls.Config
	pool *x509.CertPool

	mu       sync.Mutex
	upstream string // the daemon's listener, HOST:PORT; "" while none runs
	conns    map[*wsconn.Conn]struct{}
}

// testCA makes a CA and a certificate it signs for 127.0.0.1, and
// returns the server's TLS configuration, a pool that trusts the CA and
// the CA's certificate in PEM.
func testCA(t *testing.T) (*tls.Config, *x509.CertPool, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "e2e CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
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
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "aries"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	return cfg, pool, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
}

// startFront runs the world's HTTPS front until the test ends.
func (w *world) startFront() {
	t := w.t
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg, pool, ca := testCA(t)
	w.web = &front{url: "https://" + l.Addr().String(), ca: ca, tls: cfg, pool: pool, conns: map[*wsconn.Conn]struct{}{}}
	var wg sync.WaitGroup
	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { w.relay(&wg, rw, r) }),
		TLSConfig: cfg, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.ServeTLS(l, "", "")
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
		w.web.dropAll()
		wg.Wait()
	})
}

// daemonHTTPS is the HTTPS listener a daemon of the world serves, on a
// port of its own each time it starts, which the front relays to; nil
// for a world whose users connect by SSH.
func (w *world) daemonHTTPS() (*server.HTTPS, error) {
	if w.web == nil {
		return nil, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	w.web.mu.Lock()
	w.web.upstream = l.Addr().String()
	w.web.mu.Unlock()
	return &server.HTTPS{Listener: l, TLS: w.web.tls, URLs: []string{w.web.url}, Keys: w.httpsKey, Exporter: frontExporter}, nil
}

// exporterHeader carries the exporter of a client's TLS session with
// the front to the daemon, which only a test's daemon reads.
const exporterHeader = "X-E2e-Exporter"

// frontExporter is the daemon's tls-exporter of a key proof on r: the
// client's session with the front, as the front passed it on.
func frontExporter(r *http.Request) ([]byte, error) {
	return hex.DecodeString(r.Header.Get(exporterHeader))
}

// httpsKey finds whose a key is as the world's sshd does (startSSH): by
// the keys file the daemon writes, then by the world's own keys.
func (w *world) httpsKey(pub ssh.PublicKey) (principal, fingerprint string, ok bool) {
	if perms := w.fileLine(pub); perms != nil {
		return perms.Extensions["principal"], perms.Extensions["key"], true
	}
	w.keysMu.Lock()
	defer w.keysMu.Unlock()
	p, ok := w.keys[ssh.FingerprintSHA256(pub)]
	return p, "", ok
}

// relay serves one request to the front: a WebSocket on /v1/connect is
// relayed to the daemon, with a sign-in's Authorization and DPoP
// headers, and the daemon's refusal of the upgrade as it came; any
// other path is proxied to the daemon as a reverse proxy of the
// operator's would (the sign-in's); and a daemon that does not answer
// is answered as a proxy would.
func (w *world) relay(wg *sync.WaitGroup, rw http.ResponseWriter, r *http.Request) {
	f := w.web
	f.mu.Lock()
	upstream := f.upstream
	f.mu.Unlock()
	if r.URL.Path != keyproof.ConnectPath {
		if upstream == "" {
			http.Error(rw, "the daemon does not answer", http.StatusBadGateway)
			return
		}
		p := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(&url.URL{Scheme: "https", Host: upstream})
				pr.Out.Host = pr.In.Host // the daemon never trusts it; a proxy may pass it or rewrite it
			},
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.pool, MinVersion: tls.VersionTLS12}},
			ErrorLog:  log.New(io.Discard, "", 0),
		}
		p.ServeHTTP(rw, r)
		return
	}
	var up *wsconn.Conn
	var resp *http.Response
	var err error
	if upstream != "" {
		h := http.Header{}
		for _, k := range []string{"Authorization", "DPoP"} {
			if v := r.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		if exporter, err := keyproof.Exporter(r.TLS); err == nil {
			h.Set(exporterHeader, hex.EncodeToString(exporter))
		}
		up, resp, err = wsconn.Dial(r.Context(), "wss://"+upstream+keyproof.ConnectPath, &tls.Config{RootCAs: f.pool, MinVersion: tls.VersionTLS12}, h, 0)
	}
	if err != nil && resp != nil {
		// The daemon refused the upgrade: its answer goes back whole.
		b, _ := io.ReadAll(resp.Body)
		rw.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		rw.WriteHeader(resp.StatusCode)
		_, _ = rw.Write(b)
		return
	}
	if upstream == "" || err != nil {
		http.Error(rw, "the daemon does not answer", http.StatusBadGateway)
		return
	}
	down, err := wsconn.Accept(rw, r, 0)
	if err != nil {
		_ = up.Close()
		return
	}
	if !f.track(down, up) {
		return
	}
	// What the daemon answered for a request whose answer is to be lost.
	var mu sync.Mutex
	drop, silent := map[uint64]bool{}, map[uint64]bool{}
	end := func() { _ = down.Close(); _ = up.Close() }
	wg.Go(func() {
		defer end()
		for {
			m, err := down.Read()
			if err != nil {
				return
			}
			var fr proto.Frame
			if json.Unmarshal(m, &fr) == nil && fr.T == proto.FrameReq {
				w.connsMu.Lock()
				mu.Lock()
				if fr.Op == w.dropOp {
					w.dropOp, drop[fr.ID] = "", true
				}
				if fr.Op == w.silentOp {
					w.silentOp, silent[fr.ID] = "", true
				}
				mu.Unlock()
				w.connsMu.Unlock()
			}
			if up.Write(m) != nil {
				return
			}
		}
	})
	wg.Go(func() {
		defer func() { end(); f.untrack(down, up) }()
		for {
			m, err := up.Read()
			if err != nil {
				return
			}
			var fr proto.Frame
			if json.Unmarshal(m, &fr) == nil && fr.T == proto.FrameRes {
				mu.Lock()
				dropped, quiet := drop[fr.ID], silent[fr.ID]
				mu.Unlock()
				if dropped {
					return
				}
				if quiet {
					continue
				}
			}
			if down.Write(m) != nil {
				return
			}
		}
	})
}

// track records a relayed pair, unless the front is stopping.
func (f *front) track(down, up *wsconn.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conns == nil {
		_ = down.Close()
		_ = up.Close()
		return false
	}
	f.conns[down], f.conns[up] = struct{}{}, struct{}{}
	return true
}

// untrack forgets a relayed pair.
func (f *front) untrack(down, up *wsconn.Conn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.conns, down)
	delete(f.conns, up)
}

// dropAll closes every relayed connection, as a network drop would.
func (f *front) dropAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		_ = c.Close()
	}
}

// The suite's users reach the daemon by the transport of the run: over
// HTTPS through the front, with the daemon's connection counted as one
// over HTTPS, or through the sshd.
func TestSuiteTransport(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"alice"}})
	alice := w.newUser("alice", "")
	me := decode[struct {
		Server struct {
			Transport string `json:"transport"`
			URL       string `json:"url"`
		} `json:"server"`
	}](t, alice.ok("whoami", "--json"))
	if me.Server.Transport != suiteTransport {
		t.Fatalf("aries whoami says %q, want this run's transport, %s", me.Server.Transport, suiteTransport)
	}
	out := alice.ok("whoami")
	if suiteTransport == "https" && (me.Server.URL != w.web.url || !strings.Contains(out, w.web.url+", over HTTPS")) {
		t.Errorf("aries whoami over HTTPS = %q\n%s\nwant the front's URL, %s", me.Server.URL, out, w.web.url)
	}
	if suiteTransport == "ssh" && !strings.Contains(out, "aries@127.0.0.1:") {
		t.Errorf("aries whoami over SSH:\n%s\nwant the sshd's address", out)
	}
}

// An SSH-pinned world serves SSH whatever the run's transport.
func TestPinnedToSSH(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{ssh: true})
	if out := w.newUser("alice", "").ok("whoami"); !strings.Contains(out, "aries@127.0.0.1:") || w.web != nil {
		t.Errorf("aries whoami in a world pinned to SSH:\n%s\nwant the sshd's address, and no front", out)
	}
}

// HTTPS's own refusals, in either run: a server's certificate the
// checkout does not trust is refused, never queued; a key the server
// does not know is worded as sshd's refusal is; and a write made while
// the front answers 502 is queued, and sent once the daemon is back.
func TestRefusalsHTTPS(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{https: true})
	alice := w.newUser("alice", "")
	id := strings.TrimSpace(alice.ok("create", "Over HTTPS"))

	untrusting := w.newUser("alice", "")
	cfg := filepath.Join(untrusting.repo, ".aries.yaml")
	b, err := os.ReadFile(cfg) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	_, after, _ := strings.Cut(string(b), "  ca: |\n")
	before, _, _ := strings.Cut(string(b), "  ca: |\n")
	rest := after[strings.Index(after, "  host:"):]
	if err := os.WriteFile(cfg, []byte(before+rest), 0o600); err != nil { //nolint:gosec // the test's own checkout
		t.Fatal(err)
	}
	r := untrusting.run("v0.2.0", "comment", id, "never sent")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "does not verify") || !strings.Contains(r.stderr, "server.ca") ||
		strings.Contains(r.stdout+r.stderr, "queued") {
		t.Errorf("aries comment with the server's CA left out = exit %d, %q, %q; want refused, naming server.ca, and not queued", r.code, r.stdout, r.stderr)
	}

	stranger := w.newUser("", "")
	if r := stranger.run("v0.2.0", "ready"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "accepted no key: aries offered SHA256:") ||
		!strings.Contains(r.stderr, "fix: send your public key") {
		t.Errorf("aries ready with a key the server does not know = exit %d, %q; want the keys offered, and the fix", r.code, r.stderr)
	}

	w.stop()
	if out := alice.ok("comment", id, "while ariesd is down"); !strings.HasPrefix(out, "queued (server unreachable since ") {
		t.Errorf("a comment while the front answers 502 printed %q, want it queued", out)
	}
	w.start()
	alice.ok("ready")
	c := decode[proto.CommentsResult](t, alice.ok("comments", id, "--json"))
	if len(c.Comments) != 1 || c.Comments[0].Body != "while ariesd is down" {
		t.Errorf("the comments after the daemon came back = %+v, want the queued one, once", c.Comments)
	}
}

// down is the refusal's message, and its fix, of a command run while
// the world's daemon does not run: the bridge's over SSH, and over
// HTTPS the front's, which answers 502 as a proxy before a stopped
// daemon does.
func (w *world) down() (message, fix string) {
	if w.web == nil {
		return "ariesd is not running on the server", "ask the server admin to start it (`systemctl start ariesd`)"
	}
	return strings.TrimPrefix(w.web.url, "https://") + " answered 502 Bad Gateway: ariesd, or the proxy before it, is not serving",
		"retry in a minute; if it persists, ask the server's operator to check ariesd and the proxy in front of it"
}

// serverLine is what `aries whoami` says of the world's server for a
// user that pins the host key fpr.
func (w *world) serverLine(fpr string) string {
	if w.web != nil {
		return "server     " + w.web.url + ", over HTTPS\n"
	}
	return fmt.Sprintf("server     aries@127.0.0.1:%d, host key %s\n", w.addr.Port, fpr)
}

// configFor is the .aries.yaml `aries admin project add` prints for
// project id: the world's server as its users' configs name it.
func (w *world) configFor(id string) string {
	return fmt.Sprintf("project: %s\nserver:\n%s  host: 127.0.0.1\n  port: %d\n  host_key: %s\n", id, w.httpsConfig(), w.addr.Port, w.hostFpr)
}

// transportsLine is the status report's line of the transports, with
// conns connections past their handshake, all by the world's transport.
func (w *world) transportsLine(conns int) string {
	n := func(c int) string {
		if c == 1 {
			return "1 connection"
		}
		return fmt.Sprintf("%d connections", c)
	}
	if w.web == nil {
		return "transports: https off; ssh on, " + n(conns) + "\n"
	}
	w.web.mu.Lock()
	upstream := w.web.upstream
	w.web.mu.Unlock()
	return fmt.Sprintf("transports: https on %s for %s (files), %s; ssh on, 0 connections\n", upstream, w.web.url, n(conns))
}

// httpsConfig is what a user's .aries.yaml adds for the world's front:
// its URL and its CA.
func (w *world) httpsConfig() string {
	if w.web == nil {
		return ""
	}
	return "  url: " + w.web.url + "\n  ca: |\n    " + strings.ReplaceAll(strings.TrimSpace(w.web.ca), "\n", "\n    ") + "\n"
}
