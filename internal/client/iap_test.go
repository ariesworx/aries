//go:build unix

package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/clienttest"
	"github.com/ariesworx/aries/internal/iaptest"
	"github.com/ariesworx/aries/internal/proto"
)

func TestMain(m *testing.M) {
	iaptest.Main()
	// No test here reads the real ~/.ssh or ssh-agent.
	restore := clienttest.Isolate()
	code := m.Run()
	restore()
	os.Exit(code)
}

// sshServer runs an SSH server on loopback that accepts any key and
// answers the handshake as starfixd would. It returns its port and its
// host key's fingerprint.
func sshServer(t *testing.T) (int, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(signer)
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
			wg.Go(func() { serveHandshake(nc, cfg) })
		}
	})
	return l.Addr().(*net.TCPAddr).Port, ssh.FingerprintSHA256(signer.PublicKey())
}

func serveHandshake(nc net.Conn, cfg *ssh.ServerConfig) {
	defer func() { _ = nc.Close() }()
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer func() { _ = sc.Close() }()
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range creqs {
				_ = req.Reply(req.Type == "exec", nil)
			}
		}()
		dec, enc := proto.NewDecoder(ch), proto.NewEncoder(ch)
		if f, err := dec.Decode(); err != nil || f.T != proto.FrameHello {
			return
		}
		_ = enc.Encode(&proto.Frame{T: proto.FrameWelcome, Version: "v0.2.0", Min: proto.ProtoMin, Max: proto.ProtoMax, Session: "s-iap"})
		_, _ = io.Copy(io.Discard, ch)
		_ = ch.Close()
	}
}

// iapConfig is a config that reaches port through IAP, pinning fpr.
func iapConfig(t *testing.T, port int, fpr string) *Config {
	t.Helper()
	c, err := ParseConfig(fmt.Appendf(nil, "project: %s\nserver:\n  host: starfix-1\n  port: %d\n  host_key: %s\n"+
		"  iap:\n    project: example-project\n    zone: us-central1-a\nkey: id\n", testProject, port, fpr))
	if err != nil {
		t.Fatal(err)
	}
	// The repository, which holds the key: a committed key: names a file
	// inside it.
	c.Root = t.TempDir()
	writeKey(t, c.Root, "id", nil)
	return c
}

func noAgent(string) string { return "" }

// reaped reports whether pid is gone: not running and not a zombie
// waiting to be reaped. Signal 0 still reaches a zombie.
func reaped(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// Dial runs gcloud with exactly the tunnel's arguments, speaks SSH and
// the handshake through its stdin and stdout, and Close kills and reaps
// it. Wait returning means os/exec's copying goroutines have ended too.
func TestDialIAP(t *testing.T) {
	// Serial: puts a stand-in gcloud on the process's PATH (iaptest.Install).
	port, fpr := sshServer(t)
	gcloud := iaptest.Install(t, iaptest.Bridge)
	c, err := Dial(t.Context(), iapConfig(t, port, fpr), Options{Getenv: noAgent})
	if err != nil {
		t.Fatalf("Dial through IAP = %v", err)
	}
	if c.Session() != "s-iap" {
		t.Errorf("Session() = %q, want s-iap", c.Session())
	}
	inv := gcloud.Last(t)
	want := []string{"compute", "start-iap-tunnel", "starfix-1", strconv.Itoa(port), "--listen-on-stdin",
		"--project=example-project", "--zone=us-central1-a", "--verbosity=warning"}
	if !slices.Equal(inv.Args, want) {
		t.Errorf("gcloud args = %q, want %q", inv.Args, want)
	}
	if reaped(inv.PID) {
		t.Fatalf("gcloud (pid %d) is gone while the connection is open", inv.PID)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
	if !reaped(inv.PID) {
		t.Errorf("gcloud (pid %d) still exists after Close", inv.PID)
	}
}

// Every failure through the tunnel is a typed refusal with a fix, and
// leaves no gcloud behind.
func TestDialIAPRefusals(t *testing.T) {
	// Serial: puts a stand-in gcloud on the process's PATH (iaptest.Install).
	port, fpr := sshServer(t)
	tests := []struct {
		name    string
		mode    iaptest.Mode
		pin     string
		timeout time.Duration
		code    proto.Code
		msg     []string
		fix     string
	}{
		{name: "host key mismatch", mode: iaptest.Bridge, pin: testFpr, code: proto.CodeAuth,
			msg: []string{"host key mismatch for starfix-1"}, fix: "server.host_key"},
		{name: "tunnel refused", mode: iaptest.Deny, pin: fpr, code: proto.CodeUnavailable,
			// gcloud's words are quoted, escaped.
			msg: []string{"instance starfix-1", "4033: 'not authorized'", `\x1b]0;pwned\x07`}, fix: "IAP-secured Tunnel User"},
		{name: "tunnel silent", mode: iaptest.Hang, pin: fpr, timeout: time.Second, code: proto.CodeUnavailable,
			msg: []string{"timed out", "instance starfix-1"}, fix: "gcloud compute start-iap-tunnel starfix-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gcloud := iaptest.Install(t, tc.mode)
			_, err := Dial(t.Context(), iapConfig(t, port, tc.pin), Options{Getenv: noAgent, Timeout: tc.timeout})
			var pe *proto.Error
			if !errors.As(err, &pe) || pe.Code != tc.code || !strings.Contains(pe.Fix, tc.fix) {
				t.Fatalf("Dial = %#v, want code %s with a fix naming %q", err, tc.code, tc.fix)
			}
			for _, m := range tc.msg {
				if !strings.Contains(pe.Message, m) {
					t.Errorf("Dial message = %q, want it to contain %q", pe.Message, m)
				}
			}
			if strings.ContainsRune(pe.Message, '\x1b') {
				t.Errorf("Dial message = %q carries a raw escape", pe.Message)
			}
			if pid := gcloud.Last(t).PID; !reaped(pid) {
				t.Errorf("gcloud (pid %d) still exists after the failed Dial", pid)
			}
		})
	}
}

// Without gcloud on PATH, Dial says to install it.
func TestDialIAPNoGcloud(t *testing.T) {
	// Serial: sets the process's environment (t.Setenv).
	t.Setenv("PATH", t.TempDir())
	_, err := Dial(t.Context(), iapConfig(t, 22, testFpr), Options{Getenv: noAgent})
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeUnavailable || !strings.Contains(pe.Message, "gcloud") ||
		!strings.Contains(pe.Fix, "Google Cloud CLI") || !strings.Contains(pe.Fix, "gcloud auth login") {
		t.Fatalf("Dial without gcloud = %#v, want unavailable, fix: install the Google Cloud CLI", err)
	}
	// A setup error, not an outage: no later attempt from this machine
	// delivers a write, so none is queued for one.
	if Unreachable(err) {
		t.Errorf("Unreachable(%v) = true, want false", err)
	}
}
