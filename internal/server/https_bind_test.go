package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/telemetry"
	"github.com/ariesworx/aries/internal/wsconn"
)

// relayTLS stands between a client and the server at target, as whoever
// holds a certificate the client trusts for the server's name can (a
// mis-issued one, a leaked CA of server.ca, a TLS-inspecting
// middlebox): it terminates the client's TLS with a certificate of its
// own CA, opens its own TLS to target, and relays each WebSocket
// message, passed through edit on its way to the client when edit is
// not nil. It returns the relay's URL and a pool that trusts its
// certificate.
func relayTLS(t *testing.T, target string, targetPool *x509.CertPool, edit func([]byte) []byte) (string, *x509.CertPool) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tc, pool := testCert(t)
	tc.NextProtos = []string{"http/1.1"}
	var mu sync.Mutex
	var held []*wsconn.Conn
	hold := func(c *wsconn.Conn) {
		mu.Lock()
		held = append(held, c)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	srv := &http.Server{ReadHeaderTimeout: patience, ErrorLog: log.New(io.Discard, "", 0), Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		in, err := wsconn.Accept(rw, r, 0)
		if err != nil {
			return
		}
		hold(in)
		out, _, err := wsconn.Dial(context.Background(), keyproof.ConnectURL(target), &tls.Config{RootCAs: targetPool, MinVersion: tls.VersionTLS12}, nil, 0)
		if err != nil {
			_ = in.Close()
			return
		}
		hold(out)
		wg.Go(func() {
			defer func() { _ = in.Close() }()
			for {
				m, err := out.Read()
				if err != nil {
					return
				}
				if edit != nil {
					m = edit(m)
				}
				if in.Write(m) != nil {
					return
				}
			}
		})
		defer func() { _ = out.Close() }()
		for {
			m, err := in.Read()
			if err != nil || out.Write(m) != nil {
				return
			}
		}
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(tls.NewListener(l, tc))
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-done
		mu.Lock()
		for _, c := range held {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return "https://" + l.Addr().String(), pool
}

// rebind edits a challenge on its way to the client to ask for bind,
// or for nothing when bind is empty.
func rebind(bind string) func([]byte) []byte {
	return func(m []byte) []byte {
		var w map[string]any
		if json.Unmarshal(m, &w) != nil || w["t"] != keyproof.TypeChallenge {
			return m
		}
		if bind == "" {
			delete(w, "bind")
		} else {
			w["bind"] = bind
		}
		b, _ := json.Marshal(w)
		return append(b, '\n')
	}
}

// dialVia opens /v1/connect at via, trusting pool, as a client whose
// name for the server resolves there: what it offers is still the
// server's URL.
func dialVia(t *testing.T, via string, pool *x509.CertPool) *wsconn.Conn {
	t.Helper()
	c, _, err := wsDial(t, via, pool, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", via, err)
	}
	return c
}

// The finding F-2 test: where ariesd terminates TLS, a proof relayed
// between two TLS sessions is refused, as is one the relay had the
// client make unbound, by changing the challenge's binding to none; the
// refusal's fix names TLS inspection, the case it exists to refuse. A
// v1 proof, which binds nothing, is still taken from a client of
// protocol 8 until refuse_key_proof_v1 is set: the transition's window,
// which the setting closes.
func TestHTTPSProofRelayed(t *testing.T) {
	t.Parallel()
	alice := signerOf(t, "alice")
	refusedForInspection := func(t *testing.T, f *proto.Frame) {
		t.Helper()
		if f == nil {
			t.Fatal("the relayed proof was not refused at the proof")
		}
		if f.Err.Code != proto.CodeAuth || !strings.Contains(f.Err.Fix, "inspect") || !strings.Contains(f.Err.Message, "TLS session") {
			t.Errorf("the relayed proof's refusal = %+v; want code auth, a message naming the TLS session and a fix naming inspection", f.Err)
		}
	}
	t.Run("a bound proof", func(t *testing.T) {
		t.Parallel()
		_, u, pool := httpsServer(t, nil)
		via, viaPool := relayTLS(t, u, pool, nil)
		c := dialVia(t, via, viaPool)
		if f := prove(t, c, u, alice, nil); f != nil {
			t.Fatalf("the relay's proof was refused before it was sent: %+v", f.Err)
		}
		refusedForInspection(t, readRefusal(t, c, "the relayed proof"))
	})
	t.Run("the binding changed to none", func(t *testing.T) {
		t.Parallel()
		_, u, pool := httpsServer(t, nil)
		via, viaPool := relayTLS(t, u, pool, rebind(keyproof.BindNone))
		c := dialVia(t, via, viaPool)
		if f := prove(t, c, u, alice, nil); f != nil {
			t.Fatalf("the relay's proof was refused before it was sent: %+v", f.Err)
		}
		refusedForInspection(t, readRefusal(t, c, "the unbound proof"))
	})
	t.Run("a v1 proof", func(t *testing.T) {
		t.Parallel()
		_, u, pool := httpsServerMin(t, 8, nil)
		via, viaPool := relayTLS(t, u, pool, nil)
		c := dialVia(t, via, viaPool)
		if f := proveV1(t, c, u, alice); f != nil {
			t.Fatalf("a v1 proof through the relay: %+v", f.Err)
		}
		if w := helloProto(t, c, project, 8); w.Err != nil || w.Principal != "alice" {
			t.Errorf("a v1 proof relayed, with refuse_key_proof_v1 off = %+v; want it served, the window the setting closes", w)
		}
	})
	t.Run("a v1 proof with refuse_key_proof_v1", func(t *testing.T) {
		t.Parallel()
		_, u, pool := httpsServer(t, func(h *HTTPS) { h.RefuseKeyProofV1 = true })
		via, viaPool := relayTLS(t, u, pool, nil)
		c := dialVia(t, via, viaPool)
		if f := proveV1(t, c, u, alice); f != nil {
			t.Fatalf("a v1 proof through the relay was refused before it was sent: %+v", f.Err)
		}
		if f := readRefusal(t, c, "the v1 proof"); f.Err.Code != proto.CodeVersion {
			t.Errorf("a v1 proof with refuse_key_proof_v1 = %+v; want code version", f.Err)
		}
	})
	// The same client, straight to the server, is served: what the relay
	// broke is the binding, not the proof.
	t.Run("not relayed", func(t *testing.T) {
		t.Parallel()
		_, u, pool := httpsServer(t, nil)
		c := dialVia(t, u, pool)
		if f := prove(t, c, u, alice, nil); f != nil {
			t.Fatalf("alice's proof: %+v", f.Err)
		}
		if w := hello(t, c); w.Err != nil || w.Principal != "alice" {
			t.Errorf("alice's bound proof = %+v; want her served", w)
		}
	})
}

// Where ariesd terminates TLS its challenge asks for the TLS exporter,
// over TLS 1.2 (with the extended master secret, which Go always
// offers) as over 1.3; a v2 proof bound to the session is served.
func TestHTTPSChallengeBinds(t *testing.T) {
	t.Parallel()
	for _, max := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(tls.VersionName(max), func(t *testing.T) {
			t.Parallel()
			_, u, pool := httpsServer(t, nil)
			c, _, err := wsconn.Dial(t.Context(), keyproof.ConnectURL(u), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, MaxVersion: max}, nil, 0)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			if v := c.TLS().Version; v != max {
				t.Fatalf("negotiated %s, want %s", tls.VersionName(v), tls.VersionName(max))
			}
			alice := signerOf(t, "alice")
			if ch := challenge(t, c, u, alice.PublicKey()); ch.Bind != keyproof.BindTLSExporter {
				t.Fatalf("the challenge asks for the binding %q, want %q", ch.Bind, keyproof.BindTLSExporter)
			}
			c2, _, err := wsconn.Dial(t.Context(), keyproof.ConnectURL(u), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12, MaxVersion: max}, nil, 0)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			t.Cleanup(func() { _ = c2.Close() })
			if f := prove(t, c2, u, alice, nil); f != nil {
				t.Fatalf("alice's proof: %+v", f.Err)
			}
			if w := hello(t, c2); w.Err != nil || w.Principal != "alice" {
				t.Errorf("alice's bound proof over %s = %+v; want her served", tls.VersionName(max), w)
			}
		})
	}
}

// A client of protocol 8 (v0.5.0's aries) signs v1 and says hello in 8:
// served, until refuse_key_proof_v1 is set, which refuses its proof with
// code version and a fix to upgrade, and charges the address nothing.
func TestHTTPSProofV1(t *testing.T) {
	t.Parallel()
	alice := signerOf(t, "alice")
	t.Run("served", func(t *testing.T) {
		t.Parallel()
		_, u, pool := httpsServerMin(t, 8, nil)
		c := dialVia(t, u, pool)
		if f := proveV1(t, c, u, alice); f != nil {
			t.Fatalf("alice's v1 proof: %+v", f.Err)
		}
		if w := helloProto(t, c, project, 8); w.Err != nil || w.Principal != "alice" {
			t.Errorf("a protocol 8 client = %+v; want it served", w)
		}
	})
	t.Run("refused", func(t *testing.T) {
		t.Parallel()
		// One refused sign-in more and the address would wait: the v1
		// refusal must not be the one.
		cfg := Config{Limits: Limits{AuthFailureRate: 0.01, AuthFailureBurst: 1}, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
		u, pool := listenHTTPS(t, &cfg, func(h *HTTPS) { h.RefuseKeyProofV1 = true })
		s, _ := newServerConfig(t, cfg, nil)
		serving(t, s)
		c := dialVia(t, u, pool)
		if f := proveV1(t, c, u, alice); f != nil {
			t.Fatalf("alice's v1 proof was refused before it was sent: %+v", f.Err)
		}
		f := readRefusal(t, c, "the v1 proof")
		if f.Err.Code != proto.CodeVersion || !strings.Contains(f.Err.Fix, "aries upgrade") {
			t.Errorf("a v1 proof with refuse_key_proof_v1 = %+v; want code version and a fix naming `aries upgrade`", f.Err)
		}
		if f.Min != proto.ProtoMin || f.Max != proto.ProtoMax {
			t.Errorf("the refusal's range = [%d,%d], want [%d,%d]", f.Min, f.Max, proto.ProtoMin, proto.ProtoMax)
		}
		// The same key, by this aries, is served, from the same address.
		c2 := dialVia(t, u, pool)
		if f := prove(t, c2, u, alice, nil); f != nil {
			t.Fatalf("alice's v2 proof: %+v", f.Err)
		}
		if w := hello(t, c2); w.Err != nil || w.Principal != "alice" {
			t.Errorf("alice's v2 proof with refuse_key_proof_v1 = %+v; want her served", w)
		}
	})
}

// Behind a proxy, which terminates TLS, ariesd sees no session of the
// client's to bind to: its challenge asks for none, and a v1 proof and
// an unbound v2 proof are served as before. A v2 proof bound to some
// session is refused: it is no session the server can see.
func TestHTTPSProxyBindsNothing(t *testing.T) {
	t.Parallel()
	const public = "https://aries.example.com"
	setup := func(t *testing.T) func() *wsconn.Conn {
		cfg := Config{ProtoMin: 8, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))} // 8's clients sign the v1 proof
		listenHTTPS(t, &cfg, func(h *HTTPS) {
			h.TLS, h.Trusted, h.URLs = nil, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, []string{public}
		})
		addr := cfg.HTTPS.Listener.Addr().String()
		s, _ := newServerConfig(t, cfg, nil)
		serving(t, s)
		return func() *wsconn.Conn {
			c, _, err := wsconn.Dial(t.Context(), "ws://"+addr+keyproof.ConnectPath, nil, nil, 0)
			if err != nil {
				t.Fatalf("dial through the proxy's address: %v", err)
			}
			t.Cleanup(func() { _ = c.Close() })
			return c
		}
	}
	alice := signerOf(t, "alice")
	dial := setup(t)
	if ch := challenge(t, dial(), public, alice.PublicKey()); ch.Bind != keyproof.BindNone {
		t.Errorf("behind a proxy the challenge asks for the binding %q, want %q", ch.Bind, keyproof.BindNone)
	}
	c := dial()
	if f := proveV1(t, c, public, alice); f != nil {
		t.Fatalf("a v1 proof behind a proxy: %+v", f.Err)
	}
	if w := helloProto(t, c, project, 8); w.Err != nil || w.Principal != "alice" {
		t.Errorf("a protocol 8 client behind a proxy = %+v; want it served", w)
	}
	c = dial()
	if f := prove(t, c, public, alice, nil); f != nil {
		t.Fatalf("an unbound v2 proof behind a proxy: %+v", f.Err)
	}
	if w := hello(t, c); w.Err != nil || w.Principal != "alice" {
		t.Errorf("this aries behind a proxy = %+v; want it served", w)
	}
	c = dial()
	ch := challenge(t, c, public, alice.PublicKey())
	sig, err := keyproof.SignV2(alice, public, ch.Nonce, make([]byte, keyproof.ExporterSize))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(keyproof.NewProofV2(sig)); err != nil {
		t.Fatal(err)
	}
	if f := readRefusal(t, c, "a bound proof behind a proxy"); f.Err.Code != proto.CodeAuth {
		t.Errorf("a v2 proof bound to a session behind a proxy = %+v; want code auth", f.Err)
	}
}

// verify's every outcome, by the proof's version, the binding asked for
// and the server's exporter: what a client of each protocol signs, and
// what a relay can make it sign. A session that exports no binding
// (TLS 1.2 without the extended master secret, which no Go client
// leaves out) takes no v2 proof, rather than ask for none.
func TestVerifyKeyProof(t *testing.T) {
	t.Parallel()
	alice := signerOf(t, "alice")
	const public = "https://aries.example.com"
	nonce := keyproof.NewNonce()
	ours, theirs := bytes.Repeat([]byte{1}, keyproof.ExporterSize), bytes.Repeat([]byte{2}, keyproof.ExporterSize)
	v1, err := keyproof.Sign(alice, public, nonce)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(exporter []byte) *ssh.Signature {
		sig, err := keyproof.SignV2(alice, public, nonce, exporter)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	bound, other, unbound := sign(ours), sign(theirs), sign(nil)
	tests := []struct {
		name     string
		proxy    bool
		refuseV1 bool
		exporter []byte
		proof    keyproof.Proof
		code     proto.Code // empty when taken
		message  string
	}{
		{name: "bound to this session", exporter: ours, proof: keyproof.Proof{Sig: bound, V: 2}},
		{name: "bound to another session", exporter: ours, proof: keyproof.Proof{Sig: other, V: 2}, code: proto.CodeAuth, message: "for this TLS session"},
		{name: "unbound where bound is asked", exporter: ours, proof: keyproof.Proof{Sig: unbound, V: 2}, code: proto.CodeAuth, message: "changed the server's challenge"},
		{name: "a session that exports none", exporter: nil, proof: keyproof.Proof{Sig: bound, V: 2}, code: proto.CodeAuth, message: "exports no channel binding"},
		{name: "v1 where bound is asked", exporter: ours, proof: keyproof.Proof{Sig: v1, V: 1}},
		{name: "v1 refused", exporter: ours, refuseV1: true, proof: keyproof.Proof{Sig: v1, V: 1}, code: proto.CodeVersion, message: "protocol 9"},
		{name: "v1 labeled v2", exporter: ours, proof: keyproof.Proof{Sig: v1, V: 2}, code: proto.CodeAuth},
		{name: "v2 labeled v1", exporter: ours, proof: keyproof.Proof{Sig: bound, V: 1}, code: proto.CodeAuth, message: "does not verify"},
		{name: "unbound behind a proxy", proxy: true, proof: keyproof.Proof{Sig: unbound, V: 2}},
		{name: "v1 behind a proxy", proxy: true, proof: keyproof.Proof{Sig: v1, V: 1}},
		{name: "bound behind a proxy", proxy: true, proof: keyproof.Proof{Sig: bound, V: 2}, code: proto.CodeAuth, message: "does not verify"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &HTTPS{TLS: &tls.Config{MinVersion: tls.VersionTLS12}, RefuseKeyProofV1: tc.refuseV1}
			bind := keyproof.BindTLSExporter
			if tc.proxy {
				h.TLS, bind = nil, keyproof.BindNone
			}
			w := &web{h: h}
			offer := keyproof.Offer{URL: public, Keys: []ssh.PublicKey{alice.PublicKey()}}
			perr, reason := w.verify(offer, 0, nonce, bind, tc.exporter, tc.proof)
			switch {
			case tc.code == "" && perr != nil:
				t.Errorf("verify = %v (%s), want the proof taken", perr, reason)
			case tc.code != "" && (perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.message) || perr.Fix == ""):
				t.Errorf("verify = %+v (%s), want %s naming %q, with a fix", perr, reason, tc.code, tc.message)
			case perr != nil && perr.Code == proto.CodeAuth && reason != telemetry.ProofBadProof:
				t.Errorf("verify's reason = %q, want %q, which charges the address", reason, telemetry.ProofBadProof)
			}
		})
	}
}
