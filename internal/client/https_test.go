package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/wsconn"
)

// fakeHTTPS is a server of Aries's HTTPS transport: the key proof, for
// the keys it knows, then a welcome, then an empty answer to every
// request. answer, when set, answers the upgrade in its place.
type fakeHTTPS struct {
	srv    *httptest.Server
	known  map[string]string // fingerprint → principal
	answer http.HandlerFunc
	// silent says nothing after the upgrade.
	silent bool
}

func newFakeHTTPS(t *testing.T, edit func(*fakeHTTPS)) *fakeHTTPS {
	t.Helper()
	f := &fakeHTTPS{known: map[string]string{}}
	if edit != nil {
		edit(f)
	}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0) // a client that refuses the certificate is a case under test
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHTTPS) serve(w http.ResponseWriter, r *http.Request) {
	if f.answer != nil {
		f.answer(w, r)
		return
	}
	if r.URL.Path != keyproof.ConnectPath {
		http.NotFound(w, r)
		return
	}
	c, err := wsconn.Accept(w, r, 0)
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	if f.silent {
		for {
			if _, err := c.Read(); err != nil {
				return // the client gave up
			}
		}
	}
	refuse := func(e *proto.Error) {
		b, _ := json.Marshal(&proto.Frame{T: proto.FrameWelcome, Err: e})
		_ = c.Write(append(b, '\n'))
	}
	m, err := c.Read()
	if err != nil {
		return
	}
	offer, err := keyproof.ParseOffer(m)
	if err != nil || offer.URL != f.url() {
		refuse(proto.Errf(proto.CodeInvalid, "fix the offer", "bad offer"))
		return
	}
	key, who := -1, ""
	for i, k := range offer.Keys {
		if p, ok := f.known[ssh.FingerprintSHA256(k)]; ok {
			key, who = i, p
			break
		}
	}
	if key < 0 {
		refuse(proto.Errf(proto.CodeAuth, "send your key", "the server knows none of the keys offered"))
		return
	}
	nonce := keyproof.NewNonce()
	_ = c.Write(keyproof.NewChallenge(key, nonce))
	if m, err = c.Read(); err != nil {
		return
	}
	sig, err := keyproof.ParseProof(m)
	if err != nil || keyproof.Verify(offer.Keys[key], offer.URL, nonce, sig) != nil {
		refuse(proto.Errf(proto.CodeAuth, "check your key", "the key proof does not verify"))
		return
	}
	s := wsconn.Stream(c)
	enc, dec := proto.NewEncoder(s), proto.NewDecoder(s)
	hello, err := dec.Decode()
	if err != nil || hello.T != proto.FrameHello {
		return
	}
	_ = enc.Encode(&proto.Frame{T: proto.FrameWelcome, Version: "v1.0.0", Min: proto.ProtoMin, Max: proto.ProtoMax, Session: hello.Session, Principal: who})
	for {
		req, err := dec.Decode()
		if err != nil {
			return
		}
		res, _ := proto.Response(req.ID, struct{}{}, nil)
		_ = enc.Encode(res)
	}
}

// url is the server's URL, normalized.
func (f *fakeHTTPS) url() string {
	u, _ := keyproof.NormalizeURL(f.srv.URL)
	return u
}

// caPEMOf is the PEM of the fake's certificate, which is its own CA.
func (f *fakeHTTPS) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
}

// httpsUser is a checkout whose .aries.yaml names the fake by URL, with
// its CA unless noCA, and a key file; it returns the config and the
// environment that names the key.
func httpsUser(t *testing.T, f *fakeHTTPS, noCA bool, extra string) (*Config, func(string) string, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	body := "project: " + testProject + "\nserver:\n  url: " + f.url() + "\n" + extra
	if !noCA {
		body += "  ca: |\n" + indent(f.caPEM(), 4)
	}
	cfg, err := ParseConfig([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Root = dir
	env := map[string]string{EnvKeyFile: keyFile}
	return cfg, func(k string) string { return env[k] }, signer.PublicKey()
}

func dialOpts(getenv func(string) string) Options {
	return Options{Version: "v1.0.0", Session: "s-1", Machine: "m", Getenv: getenv,
		UserHomeDir: func() (string, error) { return "", errors.New("no home in this test") }, Timeout: 10 * time.Second}
}

// A config with server.url connects over HTTPS: the key proves itself,
// the welcome names the principal, and calls are answered.
func TestDialHTTPS(t *testing.T) {
	t.Parallel()
	f := newFakeHTTPS(t, nil)
	cfg, getenv, pub := httpsUser(t, f, false, "")
	f.known[ssh.FingerprintSHA256(pub)] = "alice"
	c, err := Dial(t.Context(), cfg, dialOpts(getenv))
	if err != nil {
		t.Fatalf("Dial over HTTPS = %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if c.Principal() != "alice" || c.Session() != "s-1" || c.Key().Fingerprint != ssh.FingerprintSHA256(pub) {
		t.Errorf("Dial over HTTPS: principal %q, session %q, key %+v; want alice, s-1 and the key file's", c.Principal(), c.Session(), c.Key())
	}
	if err := c.Call(t.Context(), proto.OpReady, proto.LimitArgs{}, nil); err != nil {
		t.Errorf("a call over HTTPS = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// How each failure to reach the server over HTTPS is reported, and which
// are Unreachable, the ones whose writes are queued (transport.md §5.6).
func TestDialHTTPSFailures(t *testing.T) {
	t.Parallel()
	refusal := func(status int, e *proto.Error) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]*proto.Error{"error": e})
		}
	}
	status := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { http.Error(w, http.StatusText(code), code) }
	}
	tests := []struct {
		name        string
		edit        func(*fakeHTTPS)
		noCA        bool
		known       bool
		code        proto.Code
		message     string
		fix         string
		unreachable bool
	}{
		{name: "a key the server does not know", code: proto.CodeAuth, message: "accepted no key: aries offered SHA256:", fix: "aries admin key add"},
		{name: "a certificate that does not verify", noCA: true, known: true, code: proto.CodeAuth, message: "certificate", fix: "server.ca"},
		{name: "a proxy's 503", edit: func(f *fakeHTTPS) { f.answer = status(http.StatusServiceUnavailable) }, code: proto.CodeUnavailable, message: "503", unreachable: true},
		{name: "a proxy's 502", edit: func(f *fakeHTTPS) { f.answer = status(http.StatusBadGateway) }, code: proto.CodeUnavailable, message: "502", unreachable: true},
		{name: "a proxy's 504", edit: func(f *fakeHTTPS) { f.answer = status(http.StatusGatewayTimeout) }, code: proto.CodeUnavailable, message: "504", unreachable: true},
		{name: "an upgrade a proxy refuses", edit: func(f *fakeHTTPS) { f.answer = status(http.StatusBadRequest) }, code: proto.CodeUnavailable, message: "400", fix: "WebSocket"},
		{name: "no such path", edit: func(f *fakeHTTPS) { f.answer = status(http.StatusNotFound) }, code: proto.CodeInvalid, message: "404", fix: "server.url"},
		{name: "the server's refusal", edit: func(f *fakeHTTPS) {
			f.answer = refusal(http.StatusForbidden, proto.Errf(proto.CodeForbidden, "connect with aries", "a browser is not served"))
		}, code: proto.CodeForbidden, message: "a browser is not served", fix: "connect with aries"},
		{name: "too many refusals", edit: func(f *fakeHTTPS) {
			f.answer = refusal(http.StatusTooManyRequests, proto.Errf(proto.CodeBusy, "wait 2s and retry", "too many refused sign-ins"))
		}, code: proto.CodeBusy, message: "too many refused sign-ins", fix: "wait 2s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeHTTPS(t, tc.edit)
			cfg, getenv, pub := httpsUser(t, f, tc.noCA, "")
			if tc.known {
				f.known[ssh.FingerprintSHA256(pub)] = "alice"
			}
			c, err := Dial(t.Context(), cfg, dialOpts(getenv))
			if err == nil {
				_ = c.Close()
				t.Fatalf("Dial with %s succeeded", tc.name)
			}
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok || pe.Code != tc.code || !strings.Contains(pe.Message, tc.message) || !strings.Contains(pe.Fix, tc.fix) || pe.Fix == "" {
				t.Errorf("Dial with %s = %v; want %s naming %q, with a fix naming %q", tc.name, err, tc.code, tc.message, tc.fix)
			}
			if Unreachable(err) != tc.unreachable {
				t.Errorf("Unreachable(Dial with %s) = %v, want %v", tc.name, Unreachable(err), tc.unreachable)
			}
		})
	}
}

// A server nothing answers at, or one that answers nothing after the
// upgrade, is unreachable: the write is queued.
func TestDialHTTPSUnreachable(t *testing.T) {
	t.Parallel()
	t.Run("nothing listens", func(t *testing.T) {
		t.Parallel()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		_ = l.Close()
		cfg, err := ParseConfig([]byte("project: " + testProject + "\nserver:\n  url: https://" + addr + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		_, getenv, _ := httpsUser(t, newFakeHTTPS(t, nil), true, "")
		_, err = Dial(t.Context(), cfg, dialOpts(getenv))
		if !Unreachable(err) || !strings.Contains(err.Error(), addr) {
			t.Errorf("Dial to a port nothing listens on = %v, want unreachable, naming %s", err, addr)
		}
	})
	t.Run("silent after the upgrade", func(t *testing.T) {
		t.Parallel()
		f := newFakeHTTPS(t, func(f *fakeHTTPS) { f.silent = true })
		cfg, getenv, _ := httpsUser(t, f, false, "")
		o := dialOpts(getenv)
		o.Timeout = 200 * time.Millisecond
		_, err := Dial(t.Context(), cfg, o)
		if !Unreachable(err) || !strings.Contains(err.Error(), "timed out") {
			t.Errorf("Dial to a server that says nothing = %v, want unreachable, timed out", err)
		}
	})
}

// sshd refuses every key when the server has SSH off (its account has
// none). With server.url beside the SSH fields, the refusal says to use
// it (transport.md §11.1); without it, it is as it was.
func TestSSHRefusalNamesTheURL(t *testing.T) {
	t.Parallel()
	d := newFakeSSHD(t, nil)
	k := newKeyPair(t)
	path := k.write(t, d.root, "id_ed25519", "", false)
	for _, tc := range []struct {
		name, url string
		want      bool
	}{{"with a URL", "https://aries.example.com", true}, {"without one", "", false}} {
		cfg := &Config{Project: testProject, Root: d.root}
		cfg.Server.URL = tc.url
		cfg.Server.Host, cfg.Server.Port, cfg.Server.User, cfg.Server.HostKey = "127.0.0.1", d.port, "aries", d.hostKey
		env := map[string]string{EnvKeyFile: path, EnvTransport: "ssh"}
		_, err := Dial(t.Context(), cfg, Options{Timeout: 10 * time.Second, Getenv: func(k string) string { return env[k] },
			UserHomeDir: func() (string, error) { return d.home, nil }})
		pe, ok := errors.AsType[*proto.Error](err)
		if !ok || pe.Code != proto.CodeAuth {
			t.Fatalf("Dial %s, every key refused = %v, want auth", tc.name, err)
		}
		if got := strings.Contains(pe.Fix, "SSH off") && strings.Contains(pe.Fix, "unset "+EnvTransport); got != tc.want {
			t.Errorf("Dial %s, every key refused: fix %q; want a word of SSH off and the URL: %v", tc.name, pe.Fix, tc.want)
		}
	}
}

// ARIES_TRANSPORT=ssh dials the server's SSH address where the config
// names both; a config's transport that cannot be is refused before
// anything is dialed.
func TestDialChoosesTransport(t *testing.T) {
	t.Parallel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	f := newFakeHTTPS(t, nil)
	cfg, getenv, pub := httpsUser(t, f, false, fmt.Sprintf("  host: 127.0.0.1\n  port: %d\n  host_key: %s\n", port, testFpr))
	f.known[ssh.FingerprintSHA256(pub)] = "alice"
	env := func(k string) string {
		if k == EnvTransport {
			return "ssh"
		}
		return getenv(k)
	}
	_, err = Dial(t.Context(), cfg, dialOpts(env))
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("127.0.0.1:%d", port)) || !strings.Contains(err.Error(), "server.host") {
		t.Errorf("Dial with %s=ssh = %v, want it to try SSH at 127.0.0.1:%d", EnvTransport, err, port)
	}
	c, err := Dial(t.Context(), cfg, dialOpts(getenv))
	if err != nil {
		t.Fatalf("Dial with both named and no %s = %v, want HTTPS", EnvTransport, err)
	}
	_ = c.Close()
	bad := func(k string) string {
		if k == EnvTransport {
			return "telnet"
		}
		return getenv(k)
	}
	if _, err := Dial(t.Context(), cfg, dialOpts(bad)); err == nil || !strings.Contains(err.Error(), EnvTransport) {
		t.Errorf("Dial with %s=telnet = %v, want a refusal naming it", EnvTransport, err)
	}
}
