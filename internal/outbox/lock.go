package outbox

import (
	"errors"
	"fmt"
	"os"
)

// lock is a held lock between processes: an exclusive kernel lock on a
// lock file of its own, which the kernel drops when the holder's handle
// closes, and so when the process dies however it dies.
//
// The lock file is opened anew for each acquisition. flock and LockFileEx
// belong to the open handle, so two goroutines of one process, each with
// its own handle, exclude each other as two processes do; `aries mcp`
// appends for its tool calls while it replays in the background.
type lock struct{ f *os.File }

// acquire takes the lock at path. With wait it blocks until the lock is
// free; without, it returns nil and no error when another holder has it.
func acquire(path string, wait bool) (*lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // the outbox's own lock file, in the directory its caller chose
	if err != nil {
		return nil, fmt.Errorf("open lock: %w", err)
	}
	ok, err := lockFile(f, wait)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !ok {
		return nil, f.Close()
	}
	return &lock{f: f}, nil
}

// release lets the lock go. Closing the handle is what releases it; the
// explicit unlock only makes that immediate.
func (l *lock) release() error {
	return errors.Join(unlockFile(l.f), l.f.Close())
}
