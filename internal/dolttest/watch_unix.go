//go:build unix

package dolttest

import (
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"syscall"
)

// watchScript is the watcher: a shell that waits on its standard input, a
// pipe whose other end only the process that started the server holds.
// The first line is dolt's process id, which is also its process group.
// A second line means the server was stopped, and the watcher exits. The
// end of input instead means the process is gone, by whatever means, since
// nothing else can close its end of the pipe: the watcher then kills
// dolt's process group, waits up to 10 seconds for it to go, and removes
// from the server's directory, $1, what dolttest and dolt put there, and
// the directory itself if that leaves it empty.
//
// It ignores the signals a terminal or a harness sends a whole group or
// session, so only its input ends it. The names it removes are the ones
// launch and StartWith write, and nothing else: a file of the caller's
// in the directory stays, and so does the directory.
const watchScript = `trap '' INT TERM HUP QUIT
read -r pid || exit 0
case $pid in '' | *[!0-9]*) exit 0 ;; esac
if read -r stopped; then exit 0; fi
kill -s KILL -- "-$pid" 2>/dev/null
n=0
while kill -s 0 -- "-$pid" 2>/dev/null && [ "$n" -lt 100 ]; do
	n=$((n + 1))
	sleep 0.1 2>/dev/null || sleep 1
done
cd -- "$1" 2>/dev/null || exit 0
rm -rf -- home data doltcfg config.yaml server.log dolt0.sock dolt1.sock dolt2.sock
cd /
rmdir -- "$1" 2>/dev/null
exit 0
`

// watcher is the process that ends a server whose own process is gone.
type watcher struct {
	cmd *exec.Cmd
	in  io.WriteCloser // the pipe the watcher reads
}

// startWatcher starts the watcher of a server in dir, before dolt itself,
// so that there is no moment at which dolt runs unwatched. It is a session
// of its own: a signal to the process group or the terminal of the tests
// does not reach it.
func startWatcher(dir string) (*watcher, error) {
	cmd := exec.Command("/bin/sh", "-c", watchScript, "sh", dir) //nolint:gosec // a constant script; dir is an argument, not script text
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("dolttest: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("dolttest: start the watcher: %w", err)
	}
	return &watcher{cmd: cmd, in: in}, nil
}

// watch tells the watcher which process group to kill.
func (w *watcher) watch(pid int) error {
	if _, err := io.WriteString(w.in, strconv.Itoa(pid)+"\n"); err != nil {
		return fmt.Errorf("dolttest: the watcher: %w", err)
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
