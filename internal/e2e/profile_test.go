package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/server"
)

// TestProfileServer serves a world to sfx binaries started outside the
// test, for measuring whole processes (docs/design/profile-sfx.md). It
// runs only when STARFIX_PROFILE_DIR names a directory, and then until a
// file named stop appears there:
//
//	STARFIX_PROFILE_DIR=DIR go test -run 'TestProfileServer$' -timeout 0 ./internal/e2e/
//
// Once it serves it writes DIR/server.env: the repository whose
// .starfix.yaml names the world's server and whose key signs in as alice,
// an admin; the port; and the host key's fingerprint. It answers two
// more files, removing each when it has: drop closes every SSH
// connection, as a network drop would, and stat writes DIR/stats, the
// SSH connections open and the goroutines and heap of this process,
// which holds the SSH server, each connection's bridge, the daemon and
// the store.
//
// The limits a profile would otherwise meet first are raised: one
// principal's connections, handshakes, sessions and write rate.
// STARFIX_PROFILE_LIMITS=default leaves the server's defaults.
func TestProfileServer(t *testing.T) {
	// Serial: measures this process, its heap and its goroutines, which a test beside it would add to.
	dir := os.Getenv("STARFIX_PROFILE_DIR")
	if dir == "" {
		t.Skip("set STARFIX_PROFILE_DIR to serve a world for profiling sfx from outside the test")
	}
	limits := server.Limits{Conns: 4096, ConnsPerPrincipal: 4096, Handshakes: 4096, WriteRate: 1e6, WriteBurst: 1_000_000}
	limits.Sessions = 4096
	if os.Getenv("STARFIX_PROFILE_LIMITS") == "default" {
		limits = server.Limits{}
	}
	w := newWorld(t, daemonOpts{admins: []string{"alice"}, limits: limits})
	u := w.newUser("alice", "")

	env := fmt.Sprintf("REPO=%s\nPORT=%d\nHOST_KEY=%s\n", u.repo, w.addr.Port, w.hostFpr)
	if err := os.WriteFile(filepath.Join(dir, "server.env"), []byte(env), 0o600); err != nil { //nolint:gosec // the directory the person running the profile named
		t.Fatal(err)
	}
	// asked reports whether the file named name is there, and removes it.
	asked := func(name string) bool {
		return os.Remove(filepath.Join(dir, name)) == nil //nolint:gosec // a fixed name in that directory
	}
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-t.Context().Done():
			return
		case <-tick.C:
		}
		if asked("stop") {
			return
		}
		if asked("drop") {
			w.dropAll()
		}
		if !asked("stat") {
			continue
		}
		w.connsMu.Lock()
		conns := len(w.conns)
		w.connsMu.Unlock()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		stats := fmt.Sprintf("ssh_conns=%d goroutines=%d heap_alloc=%d heap_inuse=%d stack_inuse=%d sys=%d\n",
			conns, runtime.NumGoroutine(), m.HeapAlloc, m.HeapInuse, m.StackInuse, m.Sys)
		if err := os.WriteFile(filepath.Join(dir, "stats"), []byte(stats), 0o600); err != nil { //nolint:gosec // as server.env
			t.Fatal(err)
		}
	}
}
