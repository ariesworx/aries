//go:build linux || darwin

package outbox

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes flock(2)'s exclusive lock on f, which belongs to the
// open file description. It reports false when wait is false and another
// holder has it. Not fcntl record locks: those belong to the process, so
// a second handle in the same process is not kept out, and closing any
// descriptor of the file drops them (spike/outbox/RESULTS.md, section 1).
func lockFile(f *os.File, wait bool) (bool, error) {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), how) //nolint:gosec // a file descriptor fits an int
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			// The Go runtime interrupts blocked threads with signals.
			continue
		case !wait && errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // a file descriptor fits an int
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the outbox's own directory
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
