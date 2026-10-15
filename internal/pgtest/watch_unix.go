//go:build unix

package pgtest

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// watchScript is the watcher: a shell that waits on its standard input, a
// pipe whose other end only the process that started the server holds.
// The first line is the postmaster's process id, which is also the
// process group of every backend it forks.
// A second line means the server was stopped, and the watcher exits. The
// end of input instead means the process is gone, by whatever means, since
// nothing else can close its end of the pipe: the watcher then kills
// the server's process group, waits up to 10 seconds for it to go, and
// removes from the server's directory, $1, what pgtest and Postgres put
// there (the data directory, the log, the socket and its lock file), and
// the System V shared memory segment the server named in its
// postmaster.pid: a server that is killed leaves it behind, and macOS
// allows a user 32 in all (kern.sysv.shmmni), and
// the directory itself if that leaves it empty.
//
// It ignores the signals a terminal or a harness sends a whole group or
// session, so only its input ends it. The names it removes are the ones
// Start and launch write, and Postgres beside them, and nothing else: a file of the caller's
// in the directory stays, and so does the directory.
const watchScript = `trap '' INT TERM HUP QUIT
read -r pid || exit 0
case $pid in '' | *[!0-9]* | 0 | 1) exit 0 ;; esac
if read -r stopped; then exit 0; fi
kill -s KILL -- "-$pid" 2>/dev/null
n=0
while kill -s 0 -- "-$pid" 2>/dev/null && [ "$n" -lt 100 ]; do
	n=$((n + 1))
	sleep 0.1 2>/dev/null || sleep 1
done
cd -- "$1" 2>/dev/null || exit 0
shm=$(sed -n 7p data/postmaster.pid 2>/dev/null | awk '{print $2}')
case $shm in '' | *[!0-9]*) ;; *) ipcrm -m "$shm" 2>/dev/null ;; esac
rm -rf -- data server.log .s.PGSQL.5432 .s.PGSQL.5432.lock
cd /
rmdir -- "$1" 2>/dev/null
exit 0
`

// watcher is the process that ends a server whose own process is gone.
type watcher struct {
	cmd *exec.Cmd
	in  io.WriteCloser // the pipe the watcher reads
}

// startWatcher starts the watcher of a server in dir, before Postgres
// itself, so that there is no moment at which it runs unwatched. It is a session
// of its own: a signal to the process group or the terminal of the tests
// does not reach it.
func startWatcher(dir string) (*watcher, error) {
	cmd := exec.Command("/bin/sh", "-c", watchScript, "sh", dir) //nolint:gosec // a constant script; dir is an argument, not script text
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pgtest: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("pgtest: start the watcher: %w", err)
	}
	return &watcher{cmd: cmd, in: in}, nil
}

// watch tells the watcher which process group to kill.
func (w *watcher) watch(pid int) error {
	if _, err := io.WriteString(w.in, strconv.Itoa(pid)+"\n"); err != nil {
		return fmt.Errorf("pgtest: the watcher: %w", err)
	}
	return nil
}

// dismiss ends the watcher without it doing anything, and waits for it:
// the server has exited, or never started.
func (w *watcher) dismiss() {
	_, _ = io.WriteString(w.in, "\nstopped\n")
	_ = w.in.Close()
	_ = w.cmd.Wait()
}

// ownGroup makes cmd, when started, the leader of a new process group,
// which the watcher can kill whole.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills the process group cmd leads.
func killGroup(cmd *exec.Cmd) error {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// Signal sends sig to the postmaster and to every process it started,
// each of which Postgres makes a session of its own: a signal to the
// postmaster's process group reaches none of them. A test stops them
// all with SIGSTOP, so that a connection open to the server waits and
// is not refused, as with a server that is swapping, and continues
// them with SIGCONT. The postmaster is signaled first, so that it starts
// no process the signal misses.
func (s *Server) Signal(sig syscall.Signal) error {
	pid := s.PID()
	if err := syscall.Kill(pid, sig); err != nil {
		return fmt.Errorf("pgtest: signal the postmaster: %w", err)
	}
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return fmt.Errorf("pgtest: list the server's processes: %w", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || f[1] != strconv.Itoa(pid) {
			continue
		}
		if child, err := strconv.Atoi(f[0]); err == nil {
			// One that has just exited cannot be signaled, and needs not be.
			_ = syscall.Kill(child, sig)
		}
	}
	return nil
}

// removeSegment removes the System V shared memory segment that the
// server on data named in its postmaster.pid, which a server that was
// killed leaves behind; the watcher's script does the same.
func removeSegment(data string) error {
	id, ok := segment(data)
	if !ok {
		return nil
	}
	if out, err := exec.Command("ipcrm", "-m", id).CombinedOutput(); err != nil { //nolint:gosec // id is digits
		return fmt.Errorf("pgtest: ipcrm -m %s: %w: %s", id, err, out)
	}
	return nil
}

// segment returns the id of the shared memory segment that the server on
// data named in its postmaster.pid (the seventh line, "KEY ID"), if
// there is one.
func segment(data string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(data, "postmaster.pid")) //nolint:gosec // the server's own file
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 7 {
		return "", false
	}
	f := strings.Fields(lines[6])
	if len(f) != 2 || f[1] == "" || strings.Trim(f[1], "0123456789") != "" {
		return "", false
	}
	return f[1], true
}
