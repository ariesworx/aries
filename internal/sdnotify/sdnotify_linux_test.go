//go:build linux

package sdnotify

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// notifySocket opens a datagram socket as systemd does, at a path or, for
// a name that starts with "@", in the abstract namespace.
func notifySocket(t *testing.T, name string) *net.UnixConn {
	t.Helper()
	c, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatalf("open the notify socket %q: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// next reads the next notification, or "" when none arrives.
func next(t *testing.T, c *net.UnixConn, wait time.Duration) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if err != nil {
		return ""
	}
	return string(buf[:n])
}

func TestNotify(t *testing.T) {
	t.Parallel()
	pid := os.Getpid()
	for _, tc := range []struct {
		name     string
		abstract bool
		watchdog string // WATCHDOG_USEC
		pid      string // WATCHDOG_PID
		every    time.Duration
	}{
		{name: "a socket file, no watchdog"},
		{name: "an abstract socket, no watchdog", abstract: true},
		{name: "a watchdog", watchdog: "90000000", every: 45 * time.Second},
		{name: "a watchdog for this process", abstract: true, watchdog: "30000000", pid: strconv.Itoa(pid), every: 15 * time.Second},
		{name: "a watchdog for another process", watchdog: "30000000", pid: strconv.Itoa(pid + 1)},
		{name: "a watchdog that is not a number", watchdog: "soon"},
		{name: "a watchdog of zero", watchdog: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr := filepath.Join(t.TempDir(), "notify.sock")
			if tc.abstract {
				addr = fmt.Sprintf("@aries-test-%d-%d", pid, time.Now().UnixNano())
			}
			sock := notifySocket(t, addr)
			n := New(func(k string) string {
				return map[string]string{EnvSocket: addr, EnvWatchdog: tc.watchdog, EnvWatchdogPID: tc.pid}[k]
			}, pid)
			if got := n.WatchdogEvery(); got != tc.every {
				t.Errorf("WatchdogEvery() = %s, want %s", got, tc.every)
			}
			if err := n.Ready(); err != nil {
				t.Fatalf("Ready() = %v", err)
			}
			if got := next(t, sock, 5*time.Second); got != "READY=1" {
				t.Errorf("after Ready the socket received %q, want READY=1", got)
			}
			if err := n.Watchdog(); err != nil {
				t.Fatalf("Watchdog() = %v", err)
			}
			want, wait := "", 50*time.Millisecond // nothing is sent with no watchdog
			if tc.every > 0 {
				want, wait = "WATCHDOG=1", 5*time.Second
			}
			if got := next(t, sock, wait); got != want {
				t.Errorf("after Watchdog the socket received %q, want %q", got, want)
			}
			if err := n.Stopping(); err != nil {
				t.Fatalf("Stopping() = %v", err)
			}
			if got := next(t, sock, 5*time.Second); got != "STOPPING=1" {
				t.Errorf("after Stopping the socket received %q, want STOPPING=1", got)
			}
		})
	}
}

// A socket that is gone is an error the caller can log, not a panic or a
// hang.
func TestNotifyNoListener(t *testing.T) {
	t.Parallel()
	addr := filepath.Join(t.TempDir(), "gone.sock")
	n := New(func(k string) string { return map[string]string{EnvSocket: addr}[k] }, os.Getpid())
	if err := n.Ready(); err == nil {
		t.Errorf("Ready() to a socket that does not exist = nil, want an error")
	}
}
