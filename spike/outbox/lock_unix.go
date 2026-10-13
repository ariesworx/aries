//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// defaultLock is flock(2): a lock on the open file description, which the
// kernel drops when the last descriptor of that description closes, and so
// when the process dies however it dies. It is what the Go command's own
// lockedfile package uses on Linux and macOS.
const defaultLock = "flock"

func lockKinds() []string { return []string{"flock", "fcntl", "ofd", "excl"} }

// fdLock is a kernel lock on an open lock file, by one of three calls:
//
//   - flock: flock(2), whole file, owned by the open file description.
//   - fcntl: POSIX record locks (F_SETLK), owned by the process. The
//     process loses the lock when it closes any descriptor of the file,
//     and two handles in one process do not exclude each other.
//   - ofd: open file description locks (F_OFD_SETLK): fcntl's byte ranges
//     with flock's ownership. Linux 3.15 and later; macOS has the numbers
//     in its headers.
type fdLock struct {
	f    *os.File
	kind string
}

func openOSLock(kind, path string) (locker, error) {
	switch kind {
	case "flock", "fcntl", "ofd":
	default:
		return nil, fmt.Errorf("no lock primitive %q on this system", kind)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &fdLock{f: f, kind: kind}, nil
}

// set takes or releases the lock. It reports false when wait is false and
// another holder has it.
func (l *fdLock) set(lock, wait bool) (bool, error) {
	fd := int(l.f.Fd())
	for {
		var err error
		if l.kind == "flock" {
			how := syscall.LOCK_UN
			if lock {
				how = syscall.LOCK_EX
				if !wait {
					how |= syscall.LOCK_NB
				}
			}
			err = syscall.Flock(fd, how)
		} else {
			lk := syscall.Flock_t{Type: syscall.F_UNLCK} // start 0, length 0: the whole file
			if lock {
				lk.Type = syscall.F_WRLCK
			}
			cmd := syscall.F_SETLK
			if wait {
				cmd = syscall.F_SETLKW
			}
			if l.kind == "ofd" {
				cmd = ofdSetLk
				if wait {
					cmd = ofdSetLkW
				}
			}
			err = syscall.FcntlFlock(uintptr(fd), cmd, &lk)
		}
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			// The Go runtime interrupts blocked threads with signals.
			continue
		case !wait && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EACCES)):
			return false, nil
		default:
			return false, fmt.Errorf("%s: %w", l.kind, err)
		}
	}
}

func (l *fdLock) Lock() error            { _, err := l.set(true, true); return err }
func (l *fdLock) TryLock() (bool, error) { return l.set(true, false) }
func (l *fdLock) Unlock() error          { _, err := l.set(false, false); return err }
func (l *fdLock) Close() error           { return l.f.Close() }
