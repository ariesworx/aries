//go:build unix

package pgtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// orphanEnv makes the test binary a helper process: it starts a server in
// the directory the variable names, prints the postmaster's process id and then does
// nothing until it is killed, as a test binary that hangs does. Its value
// is the directory, then a colon and "stopped" for a helper that stops the
// server first.
const orphanEnv = "ARIES_PGTEST_ORPHAN"

// poolEnv makes the test binary a helper process that stands for a test
// binary with a pool: it warms a pool, prints its own process id, the postmaster's
// and the server's directory, and does nothing more, as a test binary in
// the middle of its tests.
const poolEnv = "ARIES_PGTEST_POOL"

func TestMain(m *testing.M) {
	if v := os.Getenv(orphanEnv); v != "" {
		os.Exit(orphanHelper(v))
	}
	if os.Getenv(poolEnv) != "" {
		os.Exit(poolHelper())
	}
	os.Exit(m.Run())
}

func poolHelper() int {
	p := NewPool("sfq")
	p.Warm()
	s, err := p.take("helper")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, ErrNoPostgres) {
			return 3
		}
		return 1
	}
	fmt.Println(os.Getpid(), s.PID(), s.dir)
	select {} // until killed, or until its parent is gone
}

// A test binary whose go test is killed is no one's child: it ran on to
// the end of its tests, for minutes, with its servers. With a pool
// it ends when the process that started it is gone, and its servers and
// their directories go with it.
func TestPoolEndsWithItsParent(t *testing.T) {
	t.Parallel()
	if _, err := lookPostgres(); errors.Is(err, ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the pgtest tests")
	}
	// The shell stands for go test: it starts the helper and waits.
	parent := exec.Command("/bin/sh", "-c", `"$1" & wait`, "sh", os.Args[0]) //nolint:gosec // the test binary itself
	parent.Env = append(os.Environ(), poolEnv+"=1")
	parent.Stderr = os.Stderr
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	// Whatever the test finds, nothing of it is left: the group is the
	// shell and the helper, and the server is in a group of its own.
	t.Cleanup(func() { _ = syscall.Kill(-parent.Process.Pid, syscall.SIGKILL); _ = parent.Wait() })
	var helperPID, pgPID int
	var dir string
	if _, err := fmt.Fscan(bufio.NewReader(out), &helperPID, &pgPID, &dir); err != nil {
		t.Fatalf("the helper printed no process ids: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-pgPID, syscall.SIGKILL); _ = os.RemoveAll(dir) })
	if got := mentioning(t, dir); len(got) == 0 {
		t.Fatalf("no process names %s while its server runs, so this test would see nothing", dir)
	}

	// Only the shell: the helper lives on, with no parent.
	if err := parent.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = parent.Wait()
	for end := time.Now().Add(patience); ; time.Sleep(100 * time.Millisecond) {
		// Signal 0 asks only whether the process is there.
		gone := errors.Is(syscall.Kill(helperPID, 0), syscall.ESRCH)
		_, err := os.Stat(dir)
		if gone && len(mentioning(t, dir)) == 0 && errors.Is(err, os.ErrNotExist) {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("a minute after its parent was killed: the helper (pid %d) gone: %t; still running for %s: %q; the directory: %v",
				helperPID, gone, dir, mentioning(t, dir), err)
		}
	}
}

func orphanHelper(v string) int {
	dir, stopped := strings.CutSuffix(v, ":stopped")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := Start(ctx, dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if errors.Is(err, ErrNoPostgres) {
			return 3
		}
		return 1
	}
	pid := s.PID()
	if stopped {
		if err := s.Stop(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	fmt.Println(pid)
	select {} // until killed
}

// startOrphanHelper runs the test binary as a helper in a process group of
// its own, and returns it with its server's directory and the postmaster's process
// id. The directory is one the helper's server alone uses, as every
// caller's is.
func startOrphanHelper(t *testing.T, mode string) (helper *exec.Cmd, dir string, pgPID int) {
	t.Helper()
	if _, err := lookPostgres(); errors.Is(err, ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the pgtest tests")
	}
	// Not t.TempDir: what removes the directory is under test. A short
	// path, since a socket's is limited.
	dir, err := os.MkdirTemp("", "sfo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	helper = exec.Command(os.Args[0]) //nolint:gosec // the test binary itself
	helper.Env = append(os.Environ(), orphanEnv+"="+dir+mode)
	helper.Stderr = os.Stderr
	helper.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = helper.Process.Kill(); _ = helper.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("the helper printed no process id: %v", err)
	}
	pgPID, err = strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	// Whatever the test finds, it leaves no server of its own behind.
	t.Cleanup(func() { _ = syscall.Kill(pgPID, syscall.SIGKILL) })
	return helper, dir, pgPID
}

// mentioning returns the process id and command line of each process
// whose command line names dir: the server, by its config file, and
// whatever watches over it.
func mentioning(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-axo", "pid=,args=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var found []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.Contains(line, dir) {
			found = append(found, strings.TrimSpace(line))
		}
	}
	return found
}

// waitGone waits up to 10 seconds for no process to name dir, and reports
// those that still do.
func waitGone(t *testing.T, dir string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		left := mentioning(t, dir)
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestServerDiesWithItsProcess checks that a server does not outlive the
// process that started it, however that process ends without calling
// Stop, and that its directory goes with it. A test binary that is killed
// runs no deferred Stop: servers started by internal/e2e's TestMain were
// found running hours after their tests, each holding hundreds of
// megabytes, with their data left in the temporary directory.
func TestServerDiesWithItsProcess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		end  func(helper *exec.Cmd) error
	}{
		{name: "killed", end: func(h *exec.Cmd) error { return h.Process.Kill() }},
		// go test, at its timeout, sends the test binary SIGQUIT and then
		// kills it.
		{name: "quit", end: func(h *exec.Cmd) error { return h.Process.Signal(syscall.SIGQUIT) }},
		{name: "interrupted", end: func(h *exec.Cmd) error { return h.Process.Signal(syscall.SIGINT) }},
		// A harness that stops an agent kills the process group of what
		// the agent ran, which takes with it anything in that group.
		{name: "process group killed", end: func(h *exec.Cmd) error { return syscall.Kill(-h.Process.Pid, syscall.SIGKILL) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			helper, dir, pgPID := startOrphanHelper(t, "")
			if got := mentioning(t, dir); len(got) == 0 {
				t.Fatalf("no process names %s while its server runs, so this test would see nothing", dir)
			}
			shm, ok := segment(filepath.Join(dir, "data"))
			if !ok || !segmentExists(t, shm) {
				t.Fatalf("the running server names no shared memory segment that exists (%q), so this test would see nothing", shm)
			}
			if err := tc.end(helper); err != nil {
				t.Fatal(err)
			}
			_ = helper.Wait()
			if left := waitGone(t, dir); len(left) > 0 {
				t.Fatalf("10s after the process that started postgres (pid %d) ended, still running: %q", pgPID, left)
			}
			// The directory goes once the server has, so shortly after.
			deadline := time.Now().Add(10 * time.Second)
			for {
				_, err := os.Stat(dir)
				if errors.Is(err, os.ErrNotExist) {
					break
				}
				if time.Now().After(deadline) {
					entries, _ := os.ReadDir(dir)
					t.Fatalf("the server's directory %s is still there (stat: %v), holding %v", dir, err, entries)
				}
				time.Sleep(100 * time.Millisecond)
			}
			if segmentExists(t, shm) {
				t.Errorf("the server's shared memory segment %s is still there after it was killed", shm)
			}
		})
	}
}

// segmentExists reports whether ipcs lists the shared memory segment id.
func segmentExists(t *testing.T, id string) bool {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ipcs", "-m").Output()
	if err != nil {
		t.Fatalf("ipcs: %v", err)
	}
	// A segment's line is "m ID KEY …" on macOS and "KEY ID …" on Linux,
	// where KEY is hexadecimal.
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 1 && (f[0] == "m" || strings.HasPrefix(f[0], "0x")) && f[1] == id {
			return true
		}
	}
	return false
}

// TestStopLeavesNoWatcher checks that a process which stopped its server
// has nothing left running for it, while it lives and after it is killed,
// and that being killed then removes nothing: the directory is the
// caller's again once Stop returns.
func TestStopLeavesNoWatcher(t *testing.T) {
	t.Parallel()
	helper, dir, _ := startOrphanHelper(t, ":stopped")
	if left := mentioning(t, dir); len(left) > 0 {
		t.Errorf("after Stop, still running: %q", left)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(dir, "data")); err != nil {
		t.Errorf("the data of a server that was stopped is gone after its process was killed: %v", err)
	}
}

// TestStartCanceled checks that a Start whose context ends while
// Postgres is starting leaves no process behind.
func TestStartCanceled(t *testing.T) {
	// Serial: replaces the package's launching.
	if _, err := lookPostgres(); errors.Is(err, ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the pgtest tests")
	}
	dir := shortDir(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Postgres is started under a context that has ended.
	launching = cancel
	t.Cleanup(func() { launching = func() {} })
	s, err := Start(ctx, dir)
	if err == nil {
		_ = s.Stop()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start: %v, want context.Canceled", err)
	}
	if left := mentioning(t, dir); len(left) > 0 {
		t.Errorf("after a canceled Start, still running: %q", left)
	}
}

// TestWatcherKeepsTheCallersFiles checks that what goes with a killed
// process is only what this package and Postgres put in the directory: a file
// of the caller's stays, and the directory with it.
func TestWatcherKeepsTheCallersFiles(t *testing.T) {
	t.Parallel()
	helper, dir, _ := startOrphanHelper(t, "")
	mine := filepath.Join(dir, "mine")
	if err := os.WriteFile(mine, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	if left := waitGone(t, dir); len(left) > 0 {
		t.Fatalf("still running: %q", left)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("the directory holding the caller's file: %v", err)
		}
		if len(entries) == 1 && entries[0].Name() == "mine" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the directory holds %v, want only the caller's file", entries)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestStopLeavesNothingRunning checks that nothing the server started,
// no backend and no watcher, still runs once Stop returns, so the caller
// can remove the directory.
func TestStopLeavesNothingRunning(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s, err := Start(ctx, dir)
	if errors.Is(err, ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the pgtest tests")
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// A session, so that there is a backend as well as the postmaster.
	if err := open(t, s.RootDSN(superuser)).PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if len(mentioning(t, dir)) == 0 {
		t.Fatalf("no process names %s while its server runs, so this test would see nothing", dir)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if left := mentioning(t, dir); len(left) > 0 {
		t.Errorf("after Stop, still running: %q", left)
	}
}
