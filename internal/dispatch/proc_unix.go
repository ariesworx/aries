//go:build unix

package dispatch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/ariesworx/aries/internal/proto"
)

// ownGroup starts cmd in a process group of its own, which a terminal's
// ctrl-c does not reach and which outlives the dispatcher.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends the process group pid leads SIGINT, or SIGTERM.
func signalGroup(pid int, terminate bool) error {
	sig := syscall.SIGINT
	if terminate {
		sig = syscall.SIGTERM
	}
	return syscall.Kill(-pid, sig)
}

// alive reports whether pid runs with session in its command line, so a
// pid reused by another process is not taken for the run.
func alive(pid int, session string) bool {
	if pid <= 0 || session == "" || syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // ps, with a pid
	return err == nil && bytes.Contains(out, []byte(session))
}

// LockInstance holds the file at path locked while one dispatcher runs
// for a project on this machine, writing its pid in it, and returns the
// function that lets it go. A second is refused, naming the first.
func LockInstance(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the dispatcher's own lock file
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(path) //nolint:gosec // the dispatcher's own lock file
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, proto.Errf(proto.CodeBusy, "stop it with `aries dispatch stop`, or see it with `aries dispatch status`",
				fmt.Sprintf("a dispatcher already runs for this project on this machine (pid %s)", strings.TrimSpace(string(b))))
		}
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() { _ = f.Close() }, nil
}

// Running reports whether the dispatcher whose pid the state names still
// holds the instance lock at path.
func Running(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // the dispatcher's own lock file
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
