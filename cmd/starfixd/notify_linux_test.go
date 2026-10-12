//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Under a unit with Type=notify and a watchdog, the daemon tells systemd
// it is ready once it listens, feeds the watchdog while its reaper's
// goroutine runs, and says when it starts to stop. The socket here is
// the test's own, standing in for systemd's.
func TestRunDaemonNotifiesSystemd(t *testing.T) {
	noLeak(t)
	// Not t.TempDir, whose path can be too long for a unix socket.
	dir, err := os.MkdirTemp("", "sfn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	addr := filepath.Join(dir, "notify.sock")
	sock, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sock.Close() })
	next := func() string {
		t.Helper()
		_ = sock.SetReadDeadline(time.Now().Add(60 * time.Second))
		buf := make([]byte, 256)
		n, err := sock.Read(buf)
		if err != nil {
			t.Fatalf("no notification arrived: %v", err)
		}
		return string(buf[:n])
	}

	s := newSite(t, "")
	rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\n")
	// A watchdog of 100 ms is fed every 50 ms.
	d := runSite(t, s, map[string]string{"NOTIFY_SOCKET": addr, "WATCHDOG_USEC": "100000"})
	if got := next(); got != "READY=1" {
		t.Fatalf("the first notification is %q, want READY=1", got)
	}
	// The socket takes connections by the time systemd is told so.
	if w := hello(t, d.sock, "alice", adopted); w.Err != nil {
		t.Errorf("hello once the daemon said it was ready: %v", w.Err)
	}
	for i := range 3 {
		if got := next(); got != "WATCHDOG=1" {
			t.Fatalf("notification %d after READY is %q, want WATCHDOG=1", i+1, got)
		}
	}
	stopped := make(chan error, 1)
	go func() { stopped <- d.stop() }()
	for {
		got := next()
		if got == "STOPPING=1" {
			break
		}
		if got != "WATCHDOG=1" {
			t.Fatalf("while stopping, got the notification %q", got)
		}
	}
	if err := <-stopped; err != nil {
		t.Fatalf("runDaemon = %v, want nil", err)
	}
}
