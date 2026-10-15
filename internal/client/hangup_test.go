package client

import (
	"bytes"
	"cmp"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// hangUp is a stand-in for an sshd that turns every caller away once it
// has accepted the TCP connection, as sshd does to an address it holds
// back (PerSourcePenalties) or past MaxStartups: serve does what it does
// to the connection before it is closed. It returns the port.
func hangUp(t *testing.T, serve func(net.Conn)) int {
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
			wg.Go(func() {
				defer func() { _ = nc.Close() }()
				_ = nc.SetDeadline(time.Now().Add(patience))
				serve(nc)
			})
		}
	})
	return l.Addr().(*net.TCPAddr).Port
}

// closeAtOnce closes the connection before sshd's greeting.
func closeAtOnce(net.Conn) {}

// resetAtOnce resets the connection before sshd's greeting.
func resetAtOnce(nc net.Conn) { _ = nc.(*net.TCPConn).SetLinger(0) }

// closeInKeyExchange sends sshd's greeting, waits for the client's and
// the first bytes of its key exchange, and closes the connection.
func closeInKeyExchange(nc net.Conn) {
	if _, err := nc.Write([]byte("SSH-2.0-OpenSSH_10.2\r\n")); err != nil {
		return
	}
	var got []byte
	buf := make([]byte, 512)
	for {
		n, err := nc.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return
		}
		if _, rest, ok := bytes.Cut(got, []byte("\r\n")); ok && len(rest) > 0 {
			return
		}
	}
}

// TestDialClosedBeforeHandshake runs Dial into a server that accepts the
// TCP connection and hangs up before the SSH handshake is done, and into
// each other way a dial fails that it must not be taken for: nothing
// listening, a host key that does not match its pin, a key the server
// does not know, and a server that takes the key and runs nothing. Only
// the first is ClosedBeforeHandshake; it stays Unreachable, so a write
// made meanwhile is queued, and says why sshd may have done it and what
// to do.
func TestDialClosedBeforeHandshake(t *testing.T) {
	t.Parallel()
	mine, stranger := newKeyPair(t), newKeyPair(t)
	d := newFakeSSHD(t, map[string]keyPair{"me": mine})
	mine.write(t, d.root, "mine", "", false)
	stranger.write(t, d.root, "stranger", "", false)

	dial := func(t *testing.T, port int, hostKey, key string) error {
		t.Helper()
		cfg := &Config{Project: testProject, Key: key, Root: d.root}
		cfg.Server.Host, cfg.Server.Port, cfg.Server.User, cfg.Server.HostKey = "127.0.0.1", port, "aries", hostKey
		c, err := Dial(t.Context(), cfg, Options{Timeout: patience, Getenv: noEnv,
			UserHomeDir: func() (string, error) { return d.home, nil }})
		if err == nil {
			_ = c.Close()
			t.Fatal("Dial succeeded against a stand-in that runs no ariesd")
		}
		return err
	}
	// What the message and the fix of the refusal say, whenever sshd hung up.
	why := []string{"PerSourcePenalties", "MaxStartups", "a firewall or proxy"}
	fix := []string{"wait a minute", "not in a loop", "aries whoami", EnvKeyFile, "penalty"}
	for _, tc := range []struct {
		name   string
		port   int
		host   string // the pinned host key; d's when empty
		key    string
		code   proto.Code
		closed bool // ClosedBeforeHandshake
	}{
		{name: "closed before the greeting", port: hangUp(t, closeAtOnce), key: "mine",
			code: proto.CodeUnavailable, closed: true},
		{name: "reset before the greeting", port: hangUp(t, resetAtOnce), key: "mine",
			code: proto.CodeUnavailable, closed: true},
		{name: "closed in the key exchange", port: hangUp(t, closeInKeyExchange), key: "mine",
			code: proto.CodeUnavailable, closed: true},
		{name: "nothing listens", port: closedAddr(t), key: "mine", code: proto.CodeUnavailable},
		{name: "host key mismatch", port: d.port, host: testFpr, key: "mine", code: proto.CodeAuth},
		{name: "a key the server does not know", port: d.port, key: "stranger", code: proto.CodeAuth},
		{name: "sshd lets the key in and runs nothing", port: d.port, key: "mine", code: proto.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := dial(t, tc.port, cmp.Or(tc.host, d.hostKey), tc.key)
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok || pe.Code != tc.code {
				t.Fatalf("Dial = %v, want a refusal coded %s", err, tc.code)
			}
			if got := ClosedBeforeHandshake(err); got != tc.closed {
				t.Errorf("ClosedBeforeHandshake(%v) = %v, want %v", err, got, tc.closed)
			}
			if got, want := Unreachable(err), tc.code == proto.CodeUnavailable; got != want {
				t.Errorf("Unreachable(%v) = %v, want %v", err, got, want)
			}
			if !tc.closed {
				return
			}
			for _, w := range append([]string{"127.0.0.1:", "closed the connection before the ssh handshake"}, why...) {
				if !strings.Contains(pe.Message, w) {
					t.Errorf("Dial: message %q, want it to say %q", pe.Message, w)
				}
			}
			for _, w := range fix {
				if !strings.Contains(pe.Fix, w) {
					t.Errorf("Dial: fix %q, want it to say %q", pe.Fix, w)
				}
			}
		})
	}
}
