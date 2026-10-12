package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// sam is a second server admin, in the tests that name one.
var sam = store.Actor{Principal: "sam", Session: "s-s", Machine: "laptop-s"}

// testKey is the public key keyLine gives owner: its type, its base64,
// its fingerprint and the key itself.
func testKey(t *testing.T, owner string) (typ, blob, fingerprint string, pub ssh.PublicKey) {
	t.Helper()
	sum := sha256.Sum256([]byte(owner))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	return pub.Type(), base64.StdEncoding.EncodeToString(pub.Marshal()), ssh.FingerprintSHA256(pub), pub
}

// fingerprintOf is testKey's fingerprint alone.
func fingerprintOf(t *testing.T, owner string) string {
	t.Helper()
	_, _, fp, _ := testKey(t, owner)
	return fp
}

// lookup asks s about fingerprint as `starfixd keys` does, over a pipe,
// and returns the daemon's answer; closed is true when it closed the
// connection without one.
func lookup(t *testing.T, s *Server, fingerprint string) (f *proto.Frame, closed bool) {
	t.Helper()
	srv, cli := net.Pipe()
	var wg sync.WaitGroup
	defer func() { _ = cli.Close(); wg.Wait() }()
	wg.Go(func() {
		defer func() { _ = srv.Close() }()
		s.handle(t.Context(), srv)
	})
	wg.Go(func() { _ = proto.NewEncoder(cli).Encode(&proto.Frame{T: proto.FrameKeys, Key: fingerprint}) })
	_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := proto.NewDecoder(cli).Decode()
	if err != nil {
		return nil, true
	}
	return f, false
}

// TestKeyLookup is the daemon's side of sshd's question: it answers for
// a key of an active principal and for nothing else, from memory, with
// no connection slot taken and no store opened.
func TestKeyLookup(t *testing.T) {
	m := adminWorld(t, Limits{}) // carol has a key
	ctx := t.Context()
	carols := fingerprintOf(t, "carol")
	if _, err := m.ctl.AddKey(ctx, dana, "bob", keyLine(t, "bob")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.AddKey(ctx, dana, "frank", keyLine(t, "frank")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.DisablePrincipal(ctx, dana, "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.RemoveKey(ctx, dana, fingerprintOf(t, "frank")); err != nil {
		t.Fatal(err)
	}
	_, blob, _, _ := testKey(t, "carol")

	tests := []struct {
		name        string
		fingerprint string
		principal   string // "" means no answer for the key
	}{
		{"a key of an active principal", carols, "carol"},
		{"a key no one has", fingerprintOf(t, "nobody"), ""},
		{"a key of a disabled principal", fingerprintOf(t, "bob"), ""},
		{"a key that was removed", fingerprintOf(t, "frank"), ""},
		{"no fingerprint", "", ""},
		{"an MD5 fingerprint", "MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99", ""},
		{"the key itself", "ssh-ed25519 " + blob, ""},
		{"a fingerprint with a newline", carols + "\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, closed := lookup(t, m.Server, tc.fingerprint)
			if closed {
				t.Fatal("the daemon closed the connection without an answer")
			}
			if f.T != proto.FrameKeys || f.Principal != tc.principal {
				t.Fatalf("lookup = %q frame naming %q, want a keys frame naming %q", f.T, f.Principal, tc.principal)
			}
			if want := ""; tc.principal == "" && f.PublicKey != want {
				t.Errorf("lookup returned the key %q with no principal", f.PublicKey)
			}
			if tc.principal != "" && f.PublicKey != "ssh-ed25519 "+blob {
				t.Errorf("lookup returned the key %q, want the stored one", f.PublicKey)
			}
		})
	}
	if m.slots.total != 0 || len(m.slots.by) != 0 {
		t.Errorf("the lookups hold %d connection slots (%v), want none", m.slots.total, m.slots.by)
	}
	if n := m.OpenStores(); n != 0 || m.opened(projA) != 0 || m.opened(projB) != 0 {
		t.Errorf("the lookups opened stores: %d open, a %d times, b %d times; want none", n, m.opened(projA), m.opened(projB))
	}
	if n := m.lookups.Load(); n != 0 {
		t.Errorf("%d lookups still counted in flight, want 0", n)
	}
	// Logged at debug, and never the key.
	log := m.log.String()
	if !strings.Contains(log, `level=DEBUG msg="key lookup" found=true fingerprint=`+carols+" principal=carol") {
		t.Errorf("the log lacks the debug line of the lookup that found carol:\n%s", log)
	}
	if strings.Contains(log, blob) || strings.Contains(log, "MD5:") {
		t.Errorf("the log holds a key, or a stranger's text:\n%s", log)
	}

	// A server of one project serves no keys.
	if f, closed := lookup(t, newServer(t), carols); closed || f.T != proto.FrameKeys || f.Principal != "" || f.PublicKey != "" {
		t.Errorf("lookup on a server of one project = %+v, closed %v; want an empty keys frame", f, closed)
	}
}

// TestKeyLookupLimit holds key_lookups lookups in flight and checks the
// next is closed unanswered, and that the cap frees as they end.
func TestKeyLookupLimit(t *testing.T) {
	m := adminWorld(t, Limits{KeyLookups: 2})
	carols := fingerprintOf(t, "carol")
	// A lookup whose asker does not read stays in flight: a pipe's write
	// waits for the read.
	var held []net.Conn
	var wg sync.WaitGroup
	for range 2 {
		srv, cli := net.Pipe()
		held = append(held, cli)
		wg.Go(func() {
			defer func() { _ = srv.Close() }()
			m.handle(t.Context(), srv)
		})
		if err := proto.NewEncoder(cli).Encode(&proto.Frame{T: proto.FrameKeys, Key: carols}); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, "two lookups in flight", func() bool { return m.lookups.Load() == 2 })

	if f, closed := lookup(t, m.Server, carols); !closed {
		t.Errorf("a lookup past key_lookups was answered: %+v; want the connection closed", f)
	}
	if !strings.Contains(m.log.String(), `msg="key lookup refused" reason="key_lookups limit" limit=2`) {
		t.Errorf("the log lacks the refusal:\n%s", m.log.String())
	}

	for _, cli := range held {
		_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
		if f, err := proto.NewDecoder(cli).Decode(); err != nil || f.Principal != "carol" {
			t.Errorf("a held lookup, read at last = %+v, %v; want carol", f, err)
		}
		_ = cli.Close()
	}
	wg.Wait()
	if f, closed := lookup(t, m.Server, carols); closed || f.Principal != "carol" {
		t.Errorf("a lookup after the others ended = %+v, closed %v; want carol", f, closed)
	}
}

// waitUntil polls cond for up to 10 seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// listen serves s on a unix socket until the test ends, and returns the
// socket's path.
func listen(t *testing.T, s *Server) string {
	t.Helper()
	// Not t.TempDir, whose path can be too long for a unix socket.
	dir, err := os.MkdirTemp("", "sfk")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "run", "d.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return sock
}

// fakeDaemon answers every connection to a unix socket with the frame
// answer returns, or closes it unanswered when answer returns nil, as a
// daemon from before key lookups does.
func fakeDaemon(t *testing.T, answer func(asked *proto.Frame) *proto.Frame) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sff")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = c.Close() }()
				f, err := proto.NewDecoder(c).Decode()
				if err != nil {
					return
				}
				if out := answer(f); out != nil {
					_ = proto.NewEncoder(c).Encode(out)
				}
			})
		}
	})
	return sock
}

// TestAuthorizedKey is `starfixd keys`: the one line sshd is given for a
// key of an active principal, exactly, and no line for anything else.
func TestAuthorizedKey(t *testing.T) {
	m := adminWorld(t, Limits{}) // carol has a key
	ctx := t.Context()
	if _, err := m.ctl.AddKey(ctx, dana, "bob", keyLine(t, "bob")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.DisablePrincipal(ctx, dana, "bob"); err != nil {
		t.Fatal(err)
	}
	sock := listen(t, m.Server)
	typ, blob, fp, pub := testKey(t, "carol")

	line, err := AuthorizedKey(ctx, sock, DefaultBridge, typ, blob)
	if err != nil {
		t.Fatalf("AuthorizedKey(carol's key): %v", err)
	}
	if want := `restrict,command="/usr/local/bin/starfixd stdio --principal carol --key ` + fp + `" ssh-ed25519 ` + blob; line != want {
		t.Errorf("AuthorizedKey(carol's key) =\n%s\nwant\n%s", line, want)
	}
	// sshd reads it as one key with exactly two options.
	got, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line + "\n"))
	if err != nil {
		t.Fatalf("the line does not parse as an authorized_keys line: %v", err)
	}
	wantOptions := []string{"restrict", `command="/usr/local/bin/starfixd stdio --principal carol --key ` + fp + `"`}
	if !slices.Equal(options, wantOptions) || comment != "" || len(rest) != 0 || string(got.Marshal()) != string(pub.Marshal()) {
		t.Errorf("the line parses to options %q, comment %q, rest %q; want options %q and carol's key alone", options, comment, rest, wantOptions)
	}
	if other, err := AuthorizedKey(ctx, sock, "/opt/starfix/bin/starfixd", typ, blob); err != nil ||
		!strings.HasPrefix(other, `restrict,command="/opt/starfix/bin/starfixd stdio --principal carol `) {
		t.Errorf("AuthorizedKey with another path = %q, %v; want that path in the forced command", other, err)
	}

	bobType, bobBlob, _, _ := testKey(t, "bob")
	_, nobodyBlob, _, _ := testKey(t, "nobody")
	rsaBlob := "AAAAB3NzaC1yc2EAAAADAQABAAAAQQC7"
	single := listen(t, newServer(t))
	none := []struct {
		name      string
		socket    string
		bridge    string
		typ, blob string
	}{
		{"a key no one has", sock, DefaultBridge, typ, nobodyBlob},
		{"a key of a disabled principal", sock, DefaultBridge, bobType, bobBlob},
		{"no daemon", filepath.Join(filepath.Dir(sock), "none.sock"), DefaultBridge, typ, blob},
		{"a server of one project", single, DefaultBridge, typ, blob},
		{"a type that is not the key's", sock, DefaultBridge, "ssh-rsa", blob},
		{"base64 that is no key", sock, DefaultBridge, typ, rsaBlob},
		{"not base64", sock, DefaultBridge, typ, `"; touch /tmp/owned; "`},
		{"an empty key", sock, DefaultBridge, typ, ""},
		{"an empty type", sock, DefaultBridge, "", blob},
		{"options before the type", sock, DefaultBridge, `command="sh" ` + typ, blob},
		{"a second line", sock, DefaultBridge, typ, blob + "\n" + typ + " " + nobodyBlob},
		{"a comment after the key", sock, DefaultBridge, typ, blob + ` x" evil`},
		{"a certificate", sock, DefaultBridge, "ssh-ed25519-cert-v01@openssh.com", certBlob(t)},
		{"a path with a space", sock, "/usr/local/bin/starfixd --config /tmp/x", typ, blob},
		{"a path with a quote", sock, `/usr/local/bin/starfixd"`, typ, blob},
		{"a relative path", sock, "starfixd", typ, blob},
	}
	for _, tc := range none {
		t.Run(tc.name, func(t *testing.T) {
			line, err := AuthorizedKey(ctx, tc.socket, tc.bridge, tc.typ, tc.blob)
			if err == nil || line != "" {
				t.Fatalf("AuthorizedKey = %q, %v; want no line and an error", line, err)
			}
			for _, arg := range []string{tc.blob, tc.typ} {
				if len(arg) > 12 && strings.Contains(err.Error(), arg) {
					t.Errorf("the error repeats an argument: %v", err)
				}
			}
		})
	}
}

// certBlob is the base64 of an OpenSSH user certificate.
func certBlob(t *testing.T) string {
	t.Helper()
	_, _, _, pub := testKey(t, "certified")
	sum := sha256.Sum256([]byte("authority"))
	ca, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: pub, CertType: ssh.UserCert, KeyId: "carol", ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(cert.Marshal())
}

// TestAuthorizedKeyDistrustsTheAnswer checks that no line is built from
// an answer that is not a principal and the very key that was asked
// about, and that a daemon from before key lookups gives no line.
func TestAuthorizedKeyDistrustsTheAnswer(t *testing.T) {
	typ, blob, fp, _ := testKey(t, "carol")
	_, otherBlob, _, _ := testKey(t, "mallory")
	stored := "ssh-ed25519 " + blob
	tests := []struct {
		name   string
		answer func(*proto.Frame) *proto.Frame
	}{
		{"a daemon from before key lookups, which closes", func(*proto.Frame) *proto.Frame { return nil }},
		{"a principal that would end the forced command", func(*proto.Frame) *proto.Frame {
			return &proto.Frame{T: proto.FrameKeys, Principal: `carol" ssh-ed25519 ` + otherBlob + ` #`, PublicKey: stored}
		}},
		{"a principal with a space", func(*proto.Frame) *proto.Frame {
			return &proto.Frame{T: proto.FrameKeys, Principal: "carol --socket /tmp/x", PublicKey: stored}
		}},
		{"a reserved principal", func(*proto.Frame) *proto.Frame {
			return &proto.Frame{T: proto.FrameKeys, Principal: store.OperatorPrincipal, PublicKey: stored}
		}},
		{"another key", func(*proto.Frame) *proto.Frame {
			return &proto.Frame{T: proto.FrameKeys, Principal: "carol", PublicKey: "ssh-ed25519 " + otherBlob}
		}},
		{"the key with options", func(*proto.Frame) *proto.Frame {
			return &proto.Frame{T: proto.FrameKeys, Principal: "carol", PublicKey: `command="sh" ` + stored}
		}},
		{"the key and a second line", func(*proto.Frame) *proto.Frame {
			return &proto.Frame{T: proto.FrameKeys, Principal: "carol", PublicKey: stored + "\nssh-ed25519 " + otherBlob}
		}},
		{"a principal and no key", func(*proto.Frame) *proto.Frame { return &proto.Frame{T: proto.FrameKeys, Principal: "carol"} }},
		{"a welcome", func(*proto.Frame) *proto.Frame {
			return &proto.Frame{T: proto.FrameWelcome, Principal: "carol", PublicKey: stored}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if line, err := AuthorizedKey(t.Context(), fakeDaemon(t, tc.answer), DefaultBridge, typ, blob); err == nil || line != "" {
				t.Errorf("AuthorizedKey = %q, %v; want no line and an error", line, err)
			}
		})
	}
	// What it asks is the fingerprint it computed, and an honest answer
	// gives the line, whatever comment the daemon's key carries.
	var asked *proto.Frame
	sock := fakeDaemon(t, func(f *proto.Frame) *proto.Frame {
		asked = f
		return &proto.Frame{T: proto.FrameKeys, Principal: "carol", PublicKey: stored + " a comment"}
	})
	line, err := AuthorizedKey(t.Context(), sock, DefaultBridge, typ, blob)
	if err != nil || line != `restrict,command="/usr/local/bin/starfixd stdio --principal carol --key `+fp+`" `+stored {
		t.Errorf("AuthorizedKey with an honest answer = %q, %v", line, err)
	}
	if asked == nil || asked.T != proto.FrameKeys || asked.Key != fp {
		t.Errorf("AuthorizedKey asked %+v, want a keys frame with the fingerprint %s", asked, fp)
	}
}

// dialKey connects a to project proj as a bridge whose forced command
// named the key fingerprint, and returns the welcome and the connection;
// closed is true when the daemon closed it without a welcome.
func dialKey(t *testing.T, s *Server, a store.Actor, proj, fingerprint string) (w *proto.Frame, c *conn, closed bool) {
	t.Helper()
	srv, cli := net.Pipe()
	c = &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	t.Cleanup(c.hangUp)
	c.wg.Go(func() {
		defer func() { _ = srv.Close() }()
		s.handle(t.Context(), srv)
	})
	c.wg.Go(func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: a.Principal, Key: fingerprint})
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: proj, Session: a.Session, Machine: a.Machine})
	})
	_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := c.dec.Decode()
	if err != nil {
		return nil, c, true
	}
	return f, c, false
}

// TestBridgeFrameKey pins what the daemon makes of the fingerprint a
// bridge frame carries, and that a frame without one, as a bridge from
// before this release sends and a line of authorized_keys still does,
// is served as before.
func TestBridgeFrameKey(t *testing.T) {
	m := adminWorld(t, Limits{}) // carol has a key
	carols := fingerprintOf(t, "carol")

	// An old-style frame, decoded from the bytes an old bridge writes.
	var old proto.Frame
	if err := json.Unmarshal([]byte(`{"t":"bridge","pr":"carol"}`), &old); err != nil || old.Key != "" {
		t.Fatalf("an old bridge frame decodes to %+v, %v", old, err)
	}
	w, c, closed := dialKey(t, m.Server, carol, projA, old.Key)
	if closed || w.Err != nil || w.Principal != "carol" {
		t.Fatalf("a bridge frame with no key = %+v, closed %v; want a welcome", w, closed)
	}
	// A request answered means the connection has been recorded.
	if !c.alive() || !m.has(owner{principal: "carol", project: projA}) || m.has(owner{principal: "carol", project: projA, key: carols}) {
		t.Error("the connection with no key is not recorded as carol's, with no key")
	}

	w, c, closed = dialKey(t, m.Server, carol, projA, carols)
	if closed || w.Err != nil {
		t.Fatalf("a bridge frame with carol's key = %+v, closed %v; want a welcome", w, closed)
	}
	if !c.alive() || !m.has(owner{principal: "carol", project: projA, key: carols}) {
		t.Error("the connection is not recorded with the fingerprint of the key that authenticated it")
	}

	refused := []struct {
		name string
		who  store.Actor
		key  string
		code proto.Code // "" means closed without a welcome
	}{
		{"a key that is no fingerprint", carol, "ssh-ed25519 AAAA", ""},
		{"a fingerprint with text after it", carol, carols + " --socket /tmp/x", ""},
		{"a key the store does not hold", carol, fingerprintOf(t, "nobody"), proto.CodeAuth},
		{"another principal's key", bob, carols, proto.CodeAuth},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			w, _, closed := dialKey(t, m.Server, tc.who, projA, tc.key)
			switch {
			case tc.code == "" && !closed:
				t.Errorf("got %+v (%v), want the connection closed", w, w.Err)
			case tc.code != "" && (closed || w.Err == nil || w.Err.Code != tc.code || w.Err.Fix == ""):
				t.Errorf("got %+v, closed %v; want a refusal %s with a fix", w, closed, tc.code)
			}
		})
	}

	// A server of one project serves no keys, and ignores one named.
	if w, _, closed := dialKey(t, newServer(t), alice, project, carols); closed || w.Err != nil {
		t.Errorf("a bridge frame with a key, on a server of one project = %+v, closed %v; want a welcome", w, closed)
	}
}

// has reports whether the server has recorded a connection as o's.
func (m *multi) has(o owner) bool {
	m.Server.mu.Lock()
	defer m.Server.mu.Unlock()
	for _, got := range m.conns {
		if got == o {
			return true
		}
	}
	return false
}

// TestKeyRemoveDrops removes a key and checks that the connections it
// had authenticated close, that those of the same principal that came in
// by authorized_keys stay, and that the key lets nobody in again.
func TestKeyRemoveDrops(t *testing.T) {
	m := adminWorld(t, Limits{})
	carols := fingerprintOf(t, "carol")
	d := mustDialTo(t, m.Server, dana, projA)
	_, byKeyA, _ := dialKey(t, m.Server, carol, projA, carols)
	_, byKeyB, _ := dialKey(t, m.Server, carol, projB, carols)
	byFile := mustDialTo(t, m.Server, carol, projA)
	other := mustDialTo(t, m.Server, bob, projA)
	for _, c := range []*conn{byKeyA, byKeyB} {
		if !c.alive() {
			t.Fatal("a connection by carol's key is not open")
		}
	}

	res := mustAsk[proto.KeyResult](d, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: carols})
	// carol's connection by the file stays, and the admin is told so.
	if want := (proto.KeyResult{ID: carols, Principal: "carol", Type: "ssh-ed25519", Comment: "carol@example.com", Dropped: 2,
		FileConns: 1, Warning: fileWarning("carol", 1)}); res != want {
		t.Errorf("key.remove = %+v\nwant %+v: carol's key, 2 connections dropped and 1 by the file", res, want)
	}
	if !byKeyA.dropped() || !byKeyB.dropped() {
		t.Error("a connection the removed key authenticated is still open")
	}
	if !byFile.alive() || !other.alive() || !d.alive() {
		t.Error("removing the key closed a connection it had not authenticated")
	}
	if f, closed := lookup(t, m.Server, carols); closed || f.Principal != "" {
		t.Errorf("lookup of the removed key = %+v, closed %v; want no principal", f, closed)
	}
	if w, _, closed := dialKey(t, m.Server, carol, projA, carols); closed || w.Err == nil || w.Err.Code != proto.CodeAuth {
		t.Errorf("a handshake naming the removed key = %+v, closed %v; want an auth refusal", w, closed)
	}
	if !strings.Contains(m.log.String(), `msg="connections dropped" reason="key removed" by=dana connections=2`) {
		t.Errorf("the log lacks the drop:\n%s", m.log.String())
	}

	// A key removed while a handshake ran is caught as the connection is
	// recorded.
	if _, err := m.ctl.AddKey(t.Context(), dana, "carol", keyLine(t, "carol")); err != nil {
		t.Fatal(err)
	}
	sess := &session{actor: carol, proj: &served{id: projA}, fingerprint: carols}
	if !m.track(byFile.nc, sess) {
		t.Fatal("track refused a connection by a key the store holds")
	}
	if _, err := m.ctl.RemoveKey(t.Context(), dana, carols); err != nil {
		t.Fatal(err)
	}
	if m.track(byFile.nc, sess) {
		t.Error("track accepted a connection whose key was removed during the handshake")
	}
	m.untrack(byFile.nc)
}

// TestKeyOps runs key.add, keys and key.remove as each kind of caller.
func TestKeyOps(t *testing.T) {
	m := adminWorld(t, Limits{Control: store.ControlLimits{Keys: 2}})
	if _, err := m.ctl.AddPrincipal(t.Context(), dana, "dana", ""); err != nil { // a server admin with a row
		t.Fatal(err)
	}
	conns := map[string]*conn{
		"dana":  mustDialTo(t, m.Server, dana, projA),
		"alice": mustDialTo(t, m.Server, alice, projA), // admin of a: reaches bob, and herself
		"erin":  mustDialTo(t, m.Server, erin, projB),  // admin of b
		"bob":   mustDialTo(t, m.Server, bob, projA),   // a member
	}
	carols := fingerprintOf(t, "carol")
	const (
		ownFix   = "ask another admin to run it: a server admin, or a project admin who administers every project you belong to; the server's operator can also change authorized_keys on the server"
		reachFix = "ask a server admin to run it; a project admin may only while every project the principal belongs to is one they administer"
		listFix  = "ask a server admin, or a project admin who administers every project the principal belongs to; `sfx admin key list %s` lists your own"
	)
	seq := func() int {
		t.Helper()
		evs, err := m.ctl.Events(t.Context(), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	before := seq()
	refused := []struct {
		name string
		who  string
		op   string
		args any
		code proto.Code
		msg  string
		fix  string
	}{
		{"a member adds their own key", "bob", proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob")}, proto.CodeForbidden,
			"bob may not add a key to bob: nobody changes the keys of their own account", ownFix},
		{"a project admin adds their own key", "alice", proto.OpKeyAdd, proto.KeyAddArgs{Name: "alice", Key: keyLine(t, "alice")}, proto.CodeForbidden,
			"alice may not add a key to alice: nobody changes the keys of their own account", ownFix},
		{"a server admin adds their own key", "dana", proto.OpKeyAdd, proto.KeyAddArgs{Name: "dana", Key: keyLine(t, "dana")}, proto.CodeForbidden,
			"dana may not add a key to dana: nobody changes the keys of their own account", ownFix},
		{"a member adds a key to another", "bob", proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "x")}, proto.CodeForbidden,
			"bob may not add a key to carol: it is for project admins and server admins",
			"ask a server admin to run it, or a project admin who administers every project the principal belongs to"},
		{"a project admin adds a key out of reach", "alice", proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "x")}, proto.CodeForbidden,
			"alice may not add a key to carol: carol belongs to a project alice does not administer, or to none", reachFix},
		{"a project admin adds a key to a server admin", "alice", proto.OpKeyAdd, proto.KeyAddArgs{Name: "dana", Key: keyLine(t, "x")}, proto.CodeForbidden,
			"alice may not add a key to dana: dana is a server admin", "ask a server admin to run it"},
		// The same refusal for a key out of reach as for one no one has.
		{"a project admin removes a key out of reach", "alice", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: carols}, proto.CodeForbidden,
			"alice may not remove that key: it is no key of a principal alice may manage",
			"check the fingerprint with `sfx admin key list NAME`, or ask a server admin to run it"},
		{"a project admin removes a key no one has", "alice", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fingerprintOf(t, "nobody")}, proto.CodeForbidden,
			"alice may not remove that key: it is no key of a principal alice may manage",
			"check the fingerprint with `sfx admin key list NAME`, or ask a server admin to run it"},
		{"a member removes a key", "bob", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: carols}, proto.CodeForbidden,
			"bob may not remove that key: it is for project admins and server admins",
			"ask a server admin to run it, or a project admin who administers every project the principal belongs to"},
		{"a member lists another's keys", "bob", proto.OpKeys, proto.KeysArgs{Name: "carol"}, proto.CodeForbidden,
			"bob may not list the keys of carol: it is for the principal, for an admin who manages it, and for server admins", strings.ReplaceAll(listFix, "%s", "bob")},
		{"a project admin lists keys out of reach", "alice", proto.OpKeys, proto.KeysArgs{Name: "carol"}, proto.CodeForbidden,
			"alice may not list the keys of carol: it is for the principal, for an admin who manages it, and for server admins", strings.ReplaceAll(listFix, "%s", "alice")},
		// A name no one has is refused as one out of reach is.
		{"a project admin lists the keys of no one", "alice", proto.OpKeys, proto.KeysArgs{Name: "zed"}, proto.CodeForbidden,
			"alice may not list the keys of zed: it is for the principal, for an admin who manages it, and for server admins", strings.ReplaceAll(listFix, "%s", "alice")},
		{"a server admin lists the keys of no one", "dana", proto.OpKeys, proto.KeysArgs{Name: "zed"}, proto.CodeNotFound,
			"principal zed not found", "check the name; a server admin lists them with `sfx admin principal list`"},
		{"a server admin removes a key no one has", "dana", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fingerprintOf(t, "nobody")}, proto.CodeNotFound,
			"key " + fingerprintOf(t, "nobody") + " not found", "list a principal's keys, with their fingerprints, with `sfx admin key list NAME`"},
		{"a key another principal has", "dana", proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "carol")}, proto.CodeExists,
			"key " + carols + " already belongs to another principal",
			"use another key: a key belongs to one principal, and an agent or a machine that needs less access gets a key and a principal of its own"},
		{"a private key", "dana", proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n"},
			proto.CodeInvalid, "the key must be one public key in authorized_keys form: TYPE BASE64 [COMMENT], with no options",
			"give the public key file, such as ~/.ssh/id_ed25519.pub: `sfx admin key add NAME FILE`"},
		{"a fingerprint that is none", "dana", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: "carol"}, proto.CodeInvalid,
			"a key is named by its SHA256: fingerprint", "correct it and retry; `sfx admin key remove -h` shows the usage"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := ask[json.RawMessage](conns[tc.who], tc.op, tc.args)
			if perr == nil || perr.Code != tc.code || perr.Message != tc.msg || perr.Fix != tc.fix {
				t.Errorf("%s by %s = %+v\nwant %s: %q\nfix %q", tc.op, tc.who, perr, tc.code, tc.msg, tc.fix)
			}
			if perr != nil && strings.Contains(perr.Message+perr.Fix, "PRIVATE") {
				t.Errorf("the refusal repeats what was sent: %+v", perr)
			}
		})
	}
	if after := seq(); after != before {
		t.Errorf("the refusals recorded %d events, want none", after-before)
	}

	// Another admin with reach may add and remove; the same key again
	// changes nothing.
	bobs := fingerprintOf(t, "bob")
	add := mustAsk[proto.KeyResult](conns["alice"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob")})
	if add != (proto.KeyResult{ID: bobs, Principal: "bob", Type: "ssh-ed25519", Comment: "bob@example.com", Change: "added"}) {
		t.Errorf("key.add bob by alice = %+v", add)
	}
	if again := mustAsk[proto.KeyResult](conns["dana"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob")}); again.Change != "unchanged" || again.ID != bobs {
		t.Errorf("key.add of a key bob has = %+v, want unchanged", again)
	}
	mustAsk[proto.KeyResult](conns["dana"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob 2")})
	// The cap on a principal's keys.
	_, perr := ask[proto.KeyResult](conns["dana"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob 3")})
	if perr == nil || perr.Code != proto.CodeInvalid || perr.Message != "a principal keeps at most 2 keys" ||
		perr.Fix != "remove a key the principal no longer uses with `sfx admin key remove FINGERPRINT`, or ask the server's operator to raise keys_per_principal under limits: in starfixd's config" {
		t.Errorf("a third key with keys_per_principal 2 = %+v", perr)
	}

	// Who may list: a server admin, an admin with reach, and the
	// principal itself.
	for _, who := range []string{"dana", "alice", "bob"} {
		ks := mustAsk[proto.KeysResult](conns[who], proto.OpKeys, proto.KeysArgs{Name: "bob"})
		var fps []string
		for _, k := range ks.Keys {
			fps = append(fps, k.Fingerprint)
			if k.Type != "ssh-ed25519" || k.AddedAt.IsZero() || k.AddedBy == "" {
				t.Errorf("keys of bob for %s: %+v, want its type, who added it and when", who, k)
			}
		}
		if want := []string{bobs, fingerprintOf(t, "bob 2")}; ks.Principal != "bob" || !slices.Equal(fps, want) {
			t.Errorf("keys of bob for %s = %v, want %v, oldest first", who, fps, want)
		}
	}
	if own := mustAsk[proto.KeysResult](conns["alice"], proto.OpKeys, proto.KeysArgs{Name: "alice"}); own.Keys == nil || len(own.Keys) != 0 {
		t.Errorf("alice's own keys = %+v, want an empty list", own.Keys)
	}
	rm := mustAsk[proto.KeyResult](conns["alice"], proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: bobs})
	if rm.ID != bobs || rm.Principal != "bob" || rm.Dropped != 0 {
		t.Errorf("key.remove of bob's key by alice = %+v", rm)
	}

	// principal.add with keys: the principal and its keys together.
	hal := mustAsk[proto.PrincipalAddResult](conns["alice"], proto.OpPrincipalAdd,
		proto.PrincipalAddArgs{Name: "hal", Keys: []string{keyLine(t, "hal"), keyLine(t, "hal 2")}})
	if hal.ID != "hal" || hal.Project != projA || !slices.Equal(hal.Keys, []string{fingerprintOf(t, "hal"), fingerprintOf(t, "hal 2")}) {
		t.Errorf("principal.add hal with two keys = %+v", hal)
	}
	if f, closed := lookup(t, m.Server, fingerprintOf(t, "hal")); closed || f.Principal != "hal" {
		t.Errorf("lookup of hal's new key = %+v, closed %v; want hal", f, closed)
	}
	for _, tc := range []struct {
		name string
		who  string
		args proto.PrincipalAddArgs
		code proto.Code
		msg  string
	}{
		{"with a key another has", "alice", proto.PrincipalAddArgs{Name: "gus", Keys: []string{keyLine(t, "carol")}}, proto.CodeExists, "already belongs to another principal"},
		{"with a line that is no key", "alice", proto.PrincipalAddArgs{Name: "gus", Keys: []string{"nonsense"}}, proto.CodeInvalid, "the key must be one public key"},
		{"with more keys than a principal keeps", "alice", proto.PrincipalAddArgs{Name: "gus", Keys: []string{keyLine(t, "g1"), keyLine(t, "g2"), keyLine(t, "g3")}},
			proto.CodeInvalid, "a principal keeps at most 2 keys"},
		{"with more keys than one request carries", "dana", proto.PrincipalAddArgs{Name: "gus", Keys: make([]string, proto.MaxKeysPerAdd+1)},
			proto.CodeInvalid, "more than one principal add carries"},
		{"a server admin with no row, of themselves", "sam", proto.PrincipalAddArgs{Name: "sam", Unassigned: true, Keys: []string{keyLine(t, "sam")}},
			proto.CodeForbidden, "sam may not add a key to sam: nobody changes the keys of their own account"},
	} {
		t.Run("principal.add "+tc.name, func(t *testing.T) {
			c := conns[tc.who]
			if tc.who == "sam" {
				ms := newMultiAdmins(t, Limits{}, nil, dana.Principal, sam.Principal)
				ms.project(t, projA)
				c = mustDialTo(t, ms.Server, sam, projA)
			}
			_, perr := ask[proto.PrincipalAddResult](c, proto.OpPrincipalAdd, tc.args)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || perr.Fix == "" {
				t.Errorf("principal.add %s = %+v; want %s, a message containing %q and a fix", tc.args.Name, perr, tc.code, tc.msg)
			}
			if _, err := m.ctl.Principal(t.Context(), "gus"); err == nil {
				t.Error("the refused add left the principal behind")
			}
		})
	}
}

// TestDisabledServerAdmin pins decision 20 in the daemon: a server admin
// whose row is disabled is refused at the handshake and is no admin in
// any project's store, until enabled; one with no row is not disabled;
// and the last enabled server admin cannot be disabled.
func TestDisabledServerAdmin(t *testing.T) {
	m := newMultiAdmins(t, Limits{}, nil, dana.Principal, sam.Principal)
	p := m.project(t, projA, "dana", "bob")
	ctx := t.Context()
	st, err := m.cfg.Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	admins := func(want map[string]bool) {
		t.Helper()
		for name, w := range want {
			if got := st.IsAdmin(name); got != w {
				t.Errorf("the project store's IsAdmin(%s) = %v, want %v", name, got, w)
			}
			if got := m.ctl.IsServerAdmin(name); got != w {
				t.Errorf("the control store's IsServerAdmin(%s) = %v, want %v", name, got, w)
			}
		}
	}
	admins(map[string]bool{"dana": true, "sam": true, "bob": false})

	// sam, who has no row, is let in everywhere and disables dana.
	s := mustDialTo(t, m.Server, sam, projA)
	d := mustDialTo(t, m.Server, dana, projA)
	is := mustAsk[proto.CreateResult](d, proto.OpCreate, proto.CreateArgs{Title: "held by bob"})
	b := mustDialTo(t, m.Server, bob, projA)
	mustAsk[proto.StartResult](b, proto.OpStart, proto.StartArgs{ID: is.ID})
	mustAsk[proto.PrincipalResult](s, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "dana"})
	if !d.dropped() {
		t.Error("the disabled server admin's connection is still open")
	}
	admins(map[string]bool{"dana": false, "sam": true})
	w, _ := dialTo(t, m.Server, dana, projA)
	if w.Err == nil || w.Err.Code != proto.CodeForbidden || w.Err.Message != "dana is disabled on this server" ||
		!strings.Contains(w.Err.Fix, "`sfx admin principal enable dana`") {
		t.Errorf("a disabled server admin's handshake = %+v; want forbidden, told it is disabled and what enables it", w.Err)
	}
	// A project's store asks on every write: she may not override bob's
	// claim, as an admin may.
	title := "changed"
	cur, err := st.GetIssue(ctx, store.IssueID(is.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateIssue(ctx, dana, cur.ID, cur.Rev, store.IssuePatch{Title: &title}); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("a disabled server admin changing an issue bob holds = %v, want ErrForbidden", err)
	}

	mustAsk[proto.PrincipalResult](s, proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "dana"})
	admins(map[string]bool{"dana": true})
	d = mustDialTo(t, m.Server, dana, projA)
	if _, err := st.UpdateIssue(ctx, dana, cur.ID, cur.Rev, store.IssuePatch{Title: &title}); err != nil {
		t.Errorf("an enabled server admin changes an issue bob holds: %v", err)
	}

	// With sam disabled, dana is the last: she cannot be disabled, by
	// herself or at all, and her connection stays.
	if _, err := m.ctl.AddPrincipal(ctx, dana, "sam", ""); err != nil {
		t.Fatal(err)
	}
	mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "sam"})
	if !s.dropped() {
		t.Error("sam's connection is still open")
	}
	_, perr := ask[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "dana"})
	if perr == nil || perr.Code != proto.CodeInvalid ||
		perr.Message != "dana is the last server admin who is not disabled; without one, nobody could enable anyone from here" ||
		perr.Fix != "enable another server admin first with `sfx admin principal enable NAME`, or ask the server's operator to name another under admins: in starfixd's config" {
		t.Errorf("disabling the last enabled server admin = %+v", perr)
	}
	if !d.alive() {
		t.Error("the refused disable closed the admin's connection")
	}
}

// A protocol 5 client, as the build before the key commands speaks,
// still works against this server: it sends no key op, and its
// principal.add, which has no keys field, adds a principal without
// keys as it did.
func TestProtocol5ClientStillServed(t *testing.T) {
	m := adminWorld(t, Limits{})
	srv, cli := net.Pipe()
	c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	t.Cleanup(c.hangUp)
	c.wg.Go(func() { m.handle(t.Context(), srv) })
	c.wg.Go(func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: dana.Principal})
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: 5, Project: projA, Session: dana.Session, Machine: dana.Machine})
	})
	if w := c.read(); w.T != proto.FrameWelcome || w.Err != nil || w.Min != 2 || w.Max != proto.ProtoMax {
		t.Fatalf("a protocol 5 hello = %+v (%v), want a welcome", w, w.Err)
	}
	// The arguments as a protocol 5 client encodes them.
	hal := mustAsk[proto.PrincipalResult](c, proto.OpPrincipalAdd, json.RawMessage(`{"name":"hal"}`))
	if hal != (proto.PrincipalResult{ID: "hal", Rev: 1, State: "active", Project: projA, ProjectName: "proj-a"}) {
		t.Errorf("a protocol 5 principal.add = %+v", hal)
	}
	if ks, err := m.ctl.Keys(t.Context(), "hal"); err != nil || len(ks) != 0 {
		t.Errorf("hal's keys = %v, %v; want none", ks, err)
	}
	for _, r := range []struct {
		op   string
		args string
	}{
		{proto.OpMemberSet, `{"name":"hal","role":"admin"}`},
		{proto.OpMembers, `{}`},
		{proto.OpPrincipalDisable, `{"name":"hal"}`},
		{proto.OpPrincipalEnable, `{"name":"hal"}`},
		{proto.OpMemberRemove, `{"name":"hal"}`},
		{proto.OpProjects, `{}`},
		{proto.OpList, `{}`},
	} {
		if _, perr := ask[json.RawMessage](c, r.op, json.RawMessage(r.args)); perr != nil {
			t.Errorf("protocol 5 %s: %v", r.op, perr)
		}
	}
}

// A protocol 5 server, as the build before the key commands runs,
// refuses this client at the handshake, telling it to have the server
// upgraded, before a key op could fail as unknown.
func TestHandshakeProtocol5ServerRefusesNewClient(t *testing.T) {
	s := newServer(t)
	s.cfg.ProtoMin, s.cfg.ProtoMax = 2, 5
	f, err := handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: "alice"},
		&proto.Frame{T: proto.FrameHello, Version: "v0.5.0", Proto: proto.Proto, Project: project, Session: "s", Machine: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Err == nil || f.Err.Code != proto.CodeVersion || !strings.Contains(f.Err.Message, "accepts 2 to 5") ||
		!strings.Contains(f.Err.Fix, "starfixd upgrade") {
		t.Fatalf("a protocol %d client to a protocol 5 server = %+v, want a version refusal naming starfixd upgrade", proto.Proto, f.Err)
	}
}

// TestBridgeKey checks what the bridge sends: the fingerprint its forced
// command named, in a frame an older daemon reads as it always did, and
// nothing at all for a key that is no fingerprint.
func TestBridgeKey(t *testing.T) {
	fp := fingerprintOf(t, "carol")
	frames := make(chan *proto.Frame, 2)
	sock := fakeDaemon(t, func(f *proto.Frame) *proto.Frame {
		frames <- f
		return &proto.Frame{T: proto.FrameWelcome, Principal: f.Principal}
	})
	for _, key := range []string{fp, ""} {
		var out strings.Builder
		if err := Bridge(t.Context(), sock, "carol", key, strings.NewReader(""), &out); err != nil {
			t.Fatalf("Bridge with key %q: %v", key, err)
		}
		if f := <-frames; f.T != proto.FrameBridge || f.Principal != "carol" || f.Key != key {
			t.Errorf("Bridge with key %q sent %+v", key, f)
		}
	}
	// Without a key the frame is byte for byte what it was.
	b, err := json.Marshal(&proto.Frame{T: proto.FrameBridge, Principal: "carol"})
	if err != nil || string(b) != `{"t":"bridge","pr":"carol"}` {
		t.Errorf("a bridge frame with no key encodes to %s, %v; want the frame of a bridge before keys", b, err)
	}
	// A daemon from before keys reads a frame with one, and ignores it.
	var old struct {
		T         string `json:"t"`
		Principal string `json:"pr"`
	}
	if err := json.Unmarshal([]byte(`{"t":"bridge","pr":"carol","k":"`+fp+`"}`), &old); err != nil || old.T != "bridge" || old.Principal != "carol" {
		t.Errorf("an older frame type reads a bridge frame with a key as %+v, %v", old, err)
	}

	for _, key := range []string{"ssh-ed25519 AAAA", fp + " --socket /tmp/x", "SHA256:short"} {
		var out strings.Builder
		err := Bridge(t.Context(), sock, "carol", key, strings.NewReader(""), &out)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.CodeAuth || !strings.Contains(out.String(), `"c":"auth"`) {
			t.Errorf("Bridge with key %q = %v, wrote %q; want an auth refusal written for the client", key, err, out.String())
		}
		select {
		case f := <-frames:
			t.Errorf("Bridge with key %q reached the daemon: %+v", key, f)
		default:
		}
	}
}

// FuzzAuthorizedKey gives `starfixd keys` arbitrary arguments, as a
// stranger's client might make sshd pass, against a daemon that always
// answers with carol's key. Whatever the arguments, either there is no
// line, or they were exactly carol's key and the line is exactly hers.
func FuzzAuthorizedKey(f *testing.F) {
	dir, err := os.MkdirTemp("", "sfz")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = os.RemoveAll(dir) })
	sum := sha256.Sum256([]byte("carol"))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		f.Fatal(err)
	}
	typ, blob, fp := pub.Type(), base64.StdEncoding.EncodeToString(pub.Marshal()), ssh.FingerprintSHA256(pub)
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		f.Fatal(err)
	}
	var wg sync.WaitGroup
	f.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = c.Close() }()
				if _, err := proto.NewDecoder(c).Decode(); err == nil {
					_ = proto.NewEncoder(c).Encode(&proto.Frame{T: proto.FrameKeys, Principal: "carol", PublicKey: typ + " " + blob})
				}
			})
		}
	})
	want := `restrict,command="/usr/local/bin/starfixd stdio --principal carol --key ` + fp + `" ` + typ + " " + blob
	for _, seed := range [][2]string{
		{typ, blob}, {"ssh-rsa", blob}, {typ, blob + " comment"}, {typ, blob + "\n" + typ + " " + blob}, {`command="sh" ` + typ, blob},
		{typ, ""}, {"", blob}, {typ, `x" ssh-ed25519 AAAA`}, {typ + "\x00", blob}, {typ, blob[:len(blob)-4]}, {typ, "=" + blob},
		{"ssh-ed25519-cert-v01@openssh.com", blob}, {typ, strings.Repeat("A", 4096)},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, keyType, keyBlob string) {
		line, err := AuthorizedKey(t.Context(), sock, DefaultBridge, keyType, keyBlob)
		switch {
		case err != nil && line != "":
			t.Errorf("AuthorizedKey(%q, %q) = %q with error %v; want no line with an error", keyType, keyBlob, line, err)
		case err == nil && (keyType != typ || keyBlob != blob || line != want):
			t.Errorf("AuthorizedKey(%q, %q) = %q; want a line only for the key itself, and then exactly %q", keyType, keyBlob, line, want)
		}
	})
}
