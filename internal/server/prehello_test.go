package server

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Connections between their bridge frame and their hello (security
// review of stage 3a, F-4). The bridge frame names the principal, so
// from there on a connection is somebody's: it counts against that
// principal, and a principal the server would let into no project is
// refused at once.

// serveOn serves s on a socket of its own until the test ends, and
// returns the socket's path.
func serveOn(t *testing.T, s *Server) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sfh") // a short path: a socket's is limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background()) //nolint:usetesting // canceled before the test's own context is
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve = %v", err)
		}
	})
	return sock
}

// halfOpen is a connection that has sent its bridge frame, as the
// `starfixd stdio` of an SSH session does at once, and no hello.
type halfOpen struct {
	t   *testing.T
	nc  net.Conn
	enc *proto.Encoder
	dec *proto.Decoder
}

// openHalf connects to sock and sends principal's bridge frame.
func openHalf(t *testing.T, sock, principal string) *halfOpen {
	t.Helper()
	nc, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	h := &halfOpen{t: t, nc: nc, enc: proto.NewEncoder(nc), dec: proto.NewDecoder(nc)}
	if err := h.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: principal}); err != nil {
		t.Fatal(err)
	}
	return h
}

// answer reads what the server sends next: a frame, or closed when it
// closed the connection without one.
func (h *halfOpen) answer() (f *proto.Frame, closed bool) {
	h.t.Helper()
	_ = h.nc.SetReadDeadline(time.Now().Add(patience))
	f, err := h.dec.Decode()
	if errors.Is(err, io.EOF) {
		return nil, true
	}
	if err != nil {
		h.t.Fatalf("read from the server: %v", err)
	}
	return f, false
}

// hello sends the hello for proj and returns the welcome.
func (h *halfOpen) hello(proj string) *proto.Frame {
	h.t.Helper()
	// A server that refused at the bridge frame has closed already; its
	// refusal is still there to read.
	_ = h.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: proj, Session: "s-half", Machine: "m"})
	f, closed := h.answer()
	if closed {
		h.t.Fatal("the server closed the connection without a welcome")
	}
	return f
}

// socketsHeld returns how many connections s holds, in any state.
func socketsHeld(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// TestHalfOpenConnectionsDoNotShutOthersOut is the review's scenario:
// with conns 3, six connections of one disabled principal, each past its
// bridge frame, once filled the server until their hellos timed out, and
// a server admin's connection was closed unread.
func TestHalfOpenConnectionsDoNotShutOthersOut(t *testing.T) {
	t.Parallel()
	m := newMulti(t, Limits{Conns: 3, ConnsPerPrincipal: 1}, nil)
	m.project(t, projA, "alice=admin", "mallory")
	if _, err := m.ctl.DisablePrincipal(t.Context(), dana, "mallory"); err != nil {
		t.Fatal(err)
	}
	sock := serveOn(t, m.Server)
	for i := range 2 * 3 {
		f, closed := openHalf(t, sock, "mallory").answer()
		if closed || f.T != proto.FrameWelcome || f.Err == nil || f.Err.Code != proto.CodeForbidden {
			t.Fatalf("mallory's connection %d, with no hello sent = %+v (closed unread: %v); want a forbidden welcome at once", i+1, f, closed)
		}
	}
	waitUntil(t, "the server to let go of mallory's connections", func() bool { return socketsHeld(m.Server) == 0 })
	// The server admin connects, to see what is going on.
	if w := openHalf(t, sock, "dana").hello(projA); w.Err != nil || w.Principal != "dana" {
		t.Errorf("the server admin's handshake after mallory's connections = %+v, want a welcome", w)
	}
}

// TestBridgeFrameRefusals pins who a server of many projects refuses on
// the bridge frame alone, and who it lets go on to say hello.
func TestBridgeFrameRefusals(t *testing.T) {
	t.Parallel()
	m := newMultiAdmins(t, Limits{}, nil, "dana", "sam")
	m.project(t, projA, "alice=admin", "mallory")
	ctx := t.Context()
	for _, name := range []string{"frank", "sam"} { // frank is in no project; sam is a server admin with a row
		if _, err := m.ctl.AddPrincipal(ctx, dana, name, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"mallory", "sam"} {
		if _, err := m.ctl.DisablePrincipal(ctx, dana, name); err != nil {
			t.Fatal(err)
		}
	}
	sock := serveOn(t, m.Server)
	refused := []struct {
		name, principal, msg, fix string
	}{
		{"disabled", "mallory", "mallory is disabled on this server",
			"ask a server admin, or a project admin who administers every project you belong to, to run `sfx admin principal enable mallory`"},
		{"a disabled server admin", "sam", "sam is disabled on this server",
			"ask a server admin, or a project admin who administers every project you belong to, to run `sfx admin principal enable sam`"},
		{"unknown to the control store", "nobody", "nobody is not a principal of this server",
			"ask a server admin, or an admin of the project, to add you: `sfx admin principal add nobody`, run in the project's repository"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			h := openHalf(t, sock, tc.principal)
			f, closed := h.answer()
			if closed || f.T != proto.FrameWelcome || f.Err == nil || f.Err.Code != proto.CodeForbidden || f.Err.Message != tc.msg || f.Err.Fix != tc.fix {
				t.Fatalf("bridge frame of %s, no hello = %+v (closed unread: %v)\nwant forbidden: %q\nfix %q", tc.principal, f, closed, tc.msg, tc.fix)
			}
			if _, closed := h.answer(); !closed {
				t.Errorf("the connection of %s stayed open after its refusal", tc.principal)
			}
		})
	}
	// These are decided at the hello, which names the project.
	later := []struct {
		name, principal string
		code            proto.Code // "" is welcomed
		msg             string
	}{
		{"a member", "alice", "", ""},
		{"a server admin with no row", "dana", "", ""},
		{"in no project", "frank", proto.CodeForbidden, `frank is not a member of project "` + projA + `"`},
	}
	for _, tc := range later {
		t.Run(tc.name, func(t *testing.T) {
			w := openHalf(t, sock, tc.principal).hello(projA)
			switch {
			case tc.code == "" && w.Err != nil:
				t.Errorf("handshake of %s = %v, want a welcome", tc.principal, w.Err)
			case tc.code != "" && (w.Err == nil || w.Err.Code != tc.code || w.Err.Message != tc.msg):
				t.Errorf("handshake of %s = %+v, want %s: %q", tc.principal, w.Err, tc.code, tc.msg)
			}
		})
	}
	t.Run("the health probe", func(t *testing.T) {
		if v, err := Probe(ctx, sock, projA, "v0.2.0"); err != nil || v != "v0.2.0" {
			t.Errorf("Probe = %q, %v; want the server's version", v, err)
		}
	})
}

// TestHandshakesPerPrincipal pins the cap on one principal's connections
// that have not said hello: past it the next is refused at its bridge
// frame, other principals and the health probe are not affected, and a
// connection that says hello or goes away gives its place back.
func TestHandshakesPerPrincipal(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{Handshakes: 2}) // a server of one project: any name may sign in
	sock := serveOn(t, s)
	first, second := openHalf(t, sock, "alice"), openHalf(t, sock, "alice")
	waitUntil(t, "alice's two connections to be counted", func() bool { return s.hellos.count("alice") == 2 })

	f, closed := openHalf(t, sock, "alice").answer()
	const fix = "wait for your other starfix sessions to finish connecting, or close them, and retry; the server's operator can raise handshakes_per_principal under limits: in starfixd's config"
	if closed || f.Err == nil || f.Err.Code != proto.CodeBusy || f.Err.Fix != fix ||
		f.Err.Message != "alice has 2 connections that have not said hello, the most a principal may (2)" {
		t.Fatalf("alice's third connection before a hello = %+v (closed unread: %v)\nwant busy and the fix %q", f, closed, fix)
	}
	if proto.AtConnLimit(f.Err) {
		t.Errorf("AtConnLimit(%+v) = true, want false: this is the cap on handshakes", f.Err)
	}
	if w := openHalf(t, sock, "bob").hello(project); w.Err != nil {
		t.Errorf("bob's handshake while alice is at her cap = %v, want a welcome", w.Err)
	}
	if _, err := Probe(t.Context(), sock, project, "v0.2.0"); err != nil {
		t.Errorf("Probe while alice is at her cap = %v, want it answered", err)
	}
	// One says hello, the other goes away: both places come back.
	if w := first.hello(project); w.Err != nil {
		t.Fatalf("alice's first connection, saying hello = %v", w.Err)
	}
	_ = second.nc.Close()
	waitUntil(t, "alice's places to come back", func() bool { return s.hellos.count("alice") == 0 })
	if w := openHalf(t, sock, "alice").hello(project); w.Err != nil {
		t.Errorf("alice's next handshake = %v, want a welcome", w.Err)
	}
}

// A refusal at handshakes_per_principal is counted under the setting's
// name, like the other caps on connections.
func TestHandshakesPerPrincipalIsCounted(t *testing.T) {
	t.Parallel()
	tel, m := measured(t, true)
	s := newServerTel(t, Limits{Handshakes: 1}, tel)
	sock := serveOn(t, s)
	openHalf(t, sock, "alice")
	waitUntil(t, "alice's connection to be counted", func() bool { return s.hellos.count("alice") == 1 })
	if f, closed := openHalf(t, sock, "alice").answer(); closed || f.Err == nil || f.Err.Code != proto.CodeBusy {
		t.Fatalf("alice's second connection before a hello = %+v (closed unread: %v), want busy", f, closed)
	}
	const name, limit = "starfix.limit.refusals", "handshakes_per_principal"
	waitUntil(t, "the refusal to be counted as "+name+"{limit="+limit+"}", func() bool {
		return snapshot(t, m).Value(name, "limit", limit) == 1
	})
}

// TestConnectionCapClosesTheOldestHandshake pins what happens at the cap
// on sockets (twice conns): the connection that has waited longest
// without a hello is closed to make room, not the one that just arrived.
// The review saw a server admin's closed unread while half-open ones
// stayed.
func TestConnectionCapClosesTheOldestHandshake(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{Conns: 2})
	sock := serveOn(t, s)
	var waiting []*halfOpen
	for i, name := range []string{"alice", "bob", "carol", "erin"} {
		waiting = append(waiting, openHalf(t, sock, name))
		waitUntil(t, "the server to hold "+name+"'s connection", func() bool { return socketsHeld(s) == i+1 })
	}
	// A fifth arrives with the server at twice conns.
	if w := openHalf(t, sock, "dana").hello(project); w.Err != nil || w.Principal != "dana" {
		t.Fatalf("the handshake that arrived at the cap = %+v, want a welcome", w)
	}
	if f, closed := waiting[0].answer(); !closed {
		t.Errorf("the oldest connection without a hello got %+v, want it closed to make room", f)
	}
	// The others were not closed: each is answered. conns is 2, so one
	// is welcomed and the next told the server is full.
	if w := waiting[1].hello(project); w.Err != nil {
		t.Errorf("bob's hello after the cap was reached = %v, want a welcome", w.Err)
	}
	for _, h := range waiting[2:] {
		if w := h.hello(project); w.Err == nil || w.Err.Code != proto.CodeBusy || !strings.Contains(w.Err.Message, "the server is at its connection limit (2)") {
			t.Errorf("a hello past conns = %+v, want busy naming the server's limit", w.Err)
		}
	}
}

// TestHelloDeadline pins how long a connection may wait between its
// bridge frame and its hello.
func TestHelloDeadline(t *testing.T) {
	// Serial: a synctest bubble's virtual time takes no real time worth sharing.
	s := newServer(t)
	synctest.Test(t, func(t *testing.T) {
		srv, cli := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.handle(t.Context(), srv)
			_ = srv.Close()
		}()
		if err := proto.NewEncoder(cli).Encode(&proto.Frame{T: proto.FrameBridge, Principal: "alice"}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(helloTimeout - time.Second)
		synctest.Wait()
		select {
		case <-done:
			t.Fatalf("the connection was closed %s after its bridge frame, before the hello deadline of %s", helloTimeout-time.Second, helloTimeout)
		default:
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Errorf("the connection was still held %s after its bridge frame with no hello", helloTimeout+time.Second)
		}
		_ = cli.Close()
		<-done
		if n := s.hellos.count("alice"); n != 0 {
			t.Errorf("alice has %d connections counted as waiting after the deadline, want 0", n)
		}
	})
	if helloTimeout != 10*time.Second {
		t.Errorf("helloTimeout = %s, want 10s: see its comment before changing it", helloTimeout)
	}
}
