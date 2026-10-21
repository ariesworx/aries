//go:build unix

package dispatch

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// startRun starts a process whose command line names session, as a run's
// process does, and kills it when the test ends. It waits on stdin, which
// the test never closes, so killing the shell leaves no child behind.
func startRun(t *testing.T, session string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "read line; true", session) //nolint:gosec // a fixed shell, with the test's own session id
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd
}

// endRun kills a process started by startRun and waits for it, so its pid
// is no longer one of a process that runs.
func endRun(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// A run is alive only while its process runs with its session in its
// command line: a run whose process is gone is unended in the state but
// not alive, a pid another process took is not the run's, and an ended run
// is never alive.
func TestRunAlive(t *testing.T) {
	t.Parallel()
	const session = "alive-7f3a9c1e"
	cmd := startRun(t, session)
	if r := (&Run{PID: cmd.Process.Pid, Session: session}); !r.Alive() {
		t.Error("a run whose process runs with its session is not alive")
	}
	if r := (&Run{PID: os.Getpid(), Session: session}); r.Alive() {
		t.Error("a pid of a process with no such session is taken for the run")
	}
	if r := (&Run{PID: cmd.Process.Pid, Session: session, Ended: time.Unix(1, 0)}); r.Alive() {
		t.Error("an ended run is alive")
	}
	endRun(cmd)
	if r := (&Run{PID: cmd.Process.Pid, Session: session}); !r.Live() || r.Alive() {
		t.Errorf("a run whose process is gone: Live %v, Alive %v; want Live and not Alive", r.Live(), r.Alive())
	}
}

// The ^C message counts only runs whose process still runs: a run that
// ended while no dispatcher watched it is not kept running.
func TestInterruptCountsOnlyAliveRuns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	d, _ := w.dispatcher(DefaultConfig())
	running := startRun(t, "left-alive-2b1c")
	gone := startRun(t, "left-gone-9e4d")
	endRun(gone)
	d.State.Runs = []*Run{
		{Issue: "ar-1", Session: "left-alive-2b1c", PID: running.Process.Pid, Started: w.now},
		{Issue: "ar-2", Session: "left-gone-9e4d", PID: gone.Process.Pid, Started: w.now},
	}
	if err := d.left(nil); err != nil {
		t.Fatal(err)
	}
	if got := w.out.String(); !strings.Contains(got, "interrupted: 1 runs keep running") {
		t.Errorf("out = %q; want one run kept running", got)
	}
}
