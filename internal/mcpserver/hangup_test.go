package mcpserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/proto"
)

// hungUp returns what DialRepo says of a server that accepts the TCP
// connection and closes it before the SSH handshake, as sshd does to an
// address it holds back after failed connections (PerSourcePenalties).
func hungUp(t *testing.T) error {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			_ = nc.Close()
		}
	})
	repo := t.TempDir()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "id_ed25519"), pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f\nserver:\n  host: 127.0.0.1\n  port: %d\n"+
		"  host_key: SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU\nkey: id_ed25519\n", l.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(filepath.Join(repo, client.ConfigFile), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	c, err := DialRepo(t.Context(), repo, client.Options{Timeout: 10 * time.Second,
		Getenv: func(string) string { return "" }, UserHomeDir: func() (string, error) { return home, nil }})
	if err == nil {
		_ = c.Close()
		t.Fatal("DialRepo to a server that hangs up succeeded")
	}
	if !client.ClosedBeforeHandshake(err) {
		t.Fatalf("DialRepo to a server that hangs up = %v, want it closed before the handshake", err)
	}
	return err
}

// TestExplainHangUp pins the agent's next step when the server closed
// the connection before the SSH handshake, beside the failures to
// connect it must not be taken for.
func TestExplainHangUp(t *testing.T) {
	t.Parallel()
	refusal := hungUp(t)
	down := proto.Errf(proto.CodeUnavailable, "check server.host and server.port in .aries.yaml, and your network",
		"cannot reach 127.0.0.1:2222: connection refused")
	for _, tc := range []struct {
		name string
		err  error
		want string // the start of the fix
	}{
		{"a dial", &dialError{err: refusal}, hangUpFix() + ", quoting the server: \"wait a minute"},
		{"a write never queued", &unqueuedError{err: &dialError{err: refusal}, why: "it takes a claim"},
			"this was not queued (it takes a claim); " + hangUpFix() + ", quoting the server: \"wait a minute"},
		{"nothing listens", &dialError{err: down}, "tell the user aries cannot connect, quoting the server: \"check server.host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explain(tc.err).Fix; !strings.HasPrefix(got, tc.want) {
				t.Errorf("explain(%v).Fix = %q, want it to begin %q", tc.err, got, tc.want)
			}
		})
	}
}

// A session whose server closed the connection before the SSH handshake
// tells the agent to tell the user and not to call again in a loop, and
// does not dial again for hangUpWait: while sshd turns the address away
// every attempt is refused, and each can lengthen its penalty. After it,
// the next call dials.
func TestHungUpWaitsBeforeDialingAgain(t *testing.T) {
	t.Parallel()
	refusal := hungUp(t)
	ok := &fakeConn{who: "alice", reply: func(string, any) (any, error) { return proto.ListResult{Issues: []proto.Summary{}}, nil }}
	d := &turns{next: []any{refusal, refusal, ok}}
	clk := &clock{t: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	s := New(Options{Version: "v0.2.0", Dial: d.dial, RenewEvery: -1})
	s.now = clk.now
	t.Cleanup(func() { _ = s.Close() })
	cs := mcpSession(t, s)

	refused := func(when string) {
		t.Helper()
		res := callTool(t, cs, "ready", nil)
		got := text(t, res)
		msg, fix, _ := strings.Cut(got, "\nfix: ")
		if !res.IsError || !strings.HasPrefix(msg, "unavailable: 127.0.0.1:") || !strings.Contains(msg, "closed the connection before the ssh handshake") {
			t.Fatalf("ready %s = %q, want the server's hang-up", when, got)
		}
		for _, w := range []string{"tell the user", "do not call aries tools again in a loop", "carry on with the work you hold", "penalty"} {
			if !strings.Contains(fix, w) {
				t.Errorf("ready %s: fix %q, want it to say %q", when, fix, w)
			}
		}
		if strings.Contains(fix, "retry") {
			t.Errorf("ready %s: fix %q has the agent retry", when, fix)
		}
	}
	refused("on the first call")
	refused("just after the refusal")
	clk.add(hangUpWait - time.Second)
	s.Renew(t.Context())
	refused("within hangUpWait of the refusal")
	if n := d.dials(); n != 1 {
		t.Errorf("dials within hangUpWait of a refusal = %d, want 1: the calls and the renewal waited", n)
	}

	clk.add(time.Second)
	refused("once hangUpWait has passed, and sshd still hangs up")
	if n := d.dials(); n != 2 {
		t.Errorf("dials once hangUpWait passed = %d, want 2", n)
	}
	clk.add(hangUpWait)
	if res := callTool(t, cs, "ready", nil); res.IsError {
		t.Fatalf("ready once sshd lets the address in: %s", text(t, res))
	}
	if n := d.dials(); n != 3 {
		t.Errorf("dials = %d, want 3", n)
	}
}
