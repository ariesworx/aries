//go:build linux

package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestStatusHelperProcess is TestCheckPeerKeepsStatusFromOtherUsers's
// child: it runs `starfixd status`'s request and prints how it ended.
func TestStatusHelperProcess(t *testing.T) {
	t.Parallel()
	addr := os.Getenv(statusHelperEnv)
	if addr == "" {
		t.Skip("helper process for TestCheckPeerKeepsStatusFromOtherUsers")
	}
	res, err := Status(t.Context(), addr, "v0.2.0")
	fmt.Printf("STATUS uid=%d server=%t projects=%d err=%v\n", os.Getuid(), res.Server != nil, len(res.Projects), err)
}

// `starfixd status` is not a way for any local user to read the status:
// asked by another user, through a real socket, the daemon answers
// nothing, and the same request by the daemon's own user is answered.
// Changing uid needs root; CI runs the tests named TestCheckPeer under
// sudo, which is why this one is named so.
func TestCheckPeerKeepsStatusFromOtherUsers(t *testing.T) {
	// Serial: runs a program it has just written, which fails (text file busy) if a test beside it starts a process meanwhile.
	if os.Getuid() != 0 {
		t.Skip("needs root to run a peer as another uid")
	}
	const nobody = 65534
	s := newServer(t)
	s.cfg.PeerCheck = CheckPeer // the real check, which newServer replaces
	log := &syncBuffer{}
	s.cfg.Logger = slogTo(log)
	// The child runs as nobody, so it needs a binary and a socket it can
	// reach: a copy of this test binary in a world-readable directory, and
	// an abstract socket, which has no file permissions. A real daemon's
	// socket is also behind a directory of mode 0700; here the peer check
	// stands alone.
	dir, err := os.MkdirTemp("", "sfstat")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the child runs as another user
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "status.test")
	b, err := os.ReadFile(self) //nolint:gosec // this test binary
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil { //nolint:gosec // the child runs as another user
		t.Fatal(err)
	}
	addr := "@starfix-status-test-" + filepath.Base(dir)
	l, err := net.Listen("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	ask := func(uid uint32) string {
		t.Helper()
		cctx, stop := context.WithTimeout(t.Context(), 60*time.Second)
		defer stop()
		cmd := exec.CommandContext(cctx, bin, "-test.run=^TestStatusHelperProcess$") //nolint:gosec // this test binary
		cmd.Env = append(os.Environ(), statusHelperEnv+"="+addr)
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the helper as uid %d: %v\n%s", uid, err, out)
		}
		for line := range strings.Lines(string(out)) {
			if strings.HasPrefix(line, "STATUS ") {
				return strings.TrimSpace(line)
			}
		}
		t.Fatalf("the helper as uid %d printed no result:\n%s", uid, out)
		return ""
	}
	if got := ask(0); !strings.Contains(got, "uid=0 server=true projects=1 err=<nil>") {
		t.Errorf("status asked by the daemon's own user: %q, want the server and its project", got)
	}
	got := ask(nobody)
	if !strings.Contains(got, "uid=65534 server=false projects=0") || !strings.Contains(got, "fix: run it as the user starfixd runs as") {
		t.Errorf("status asked by uid %d: %q, want nothing answered and a fix", nobody, got)
	}
	if !strings.Contains(log.String(), "refused socket peer") || !strings.Contains(log.String(), "peer uid 65534") {
		t.Errorf("the daemon did not log the refused peer:\n%s", log)
	}
}
