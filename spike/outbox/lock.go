package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// locker is one named lock between processes, on a lock file of its own.
// The lock file is never the data file: compaction replaces the data file
// by rename, and a lock on the old file would then exclude nobody.
type locker interface {
	// Lock waits for the lock.
	Lock() error
	// TryLock takes the lock if it is free and says whether it did.
	TryLock() (bool, error)
	// Unlock releases it. errLockLost means it had already been taken
	// from this holder, which only the excl lock can report.
	Unlock() error
	Close() error
}

// renewer is a lock held by lease, which its holder must renew.
type renewer interface {
	// Renew extends the lease and says whether this process still holds
	// the lock.
	Renew() (bool, error)
}

var errLockLost = errors.New("lock was taken over while held")

// openLock opens the lock at path with the named primitive. "excl" is the
// same on every system; the rest are the operating system's (lock_unix.go,
// lock_windows.go).
func openLock(kind, path string, lease, poll time.Duration) (locker, error) {
	if kind == "excl" {
		return &exclLock{
			path: path, lease: lease, poll: poll,
			token: fmt.Sprintf("%d %d\n", os.Getpid(), time.Now().UnixNano()),
		}, nil
	}
	return openOSLock(kind, path)
}

// exclLock is a lock file created with O_EXCL and removed to unlock. A
// holder that dies leaves it behind, so a file whose modification time is
// older than the lease is taken over: removed, and created again.
//
// The take-over is not atomic, and nothing in the standard library makes
// it so. Two processes can both see a stale file; the first removes it and
// creates its own, and the second then removes the first's fresh file and
// creates another. Both now hold the lock. The experiments count how often
// that happens.
type exclLock struct {
	path   string
	lease  time.Duration
	poll   time.Duration
	token  string
	steals int
}

func (l *exclLock) TryLock() (bool, error) {
	for range 2 {
		f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.WriteString(l.token)
			return true, errors.Join(werr, f.Close())
		}
		// Windows refuses with "access denied" while a removed file's
		// last handle is still open.
		if !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrPermission) {
			return false, err
		}
		st, err := os.Stat(l.path)
		if err != nil {
			continue // gone between the two calls: try once more
		}
		if time.Since(st.ModTime()) < l.lease {
			return false, nil
		}
		l.steals++
		_ = os.Remove(l.path)
	}
	return false, nil
}

func (l *exclLock) Lock() error {
	for {
		ok, err := l.TryLock()
		if ok || err != nil {
			return err
		}
		time.Sleep(l.poll)
	}
}

// held reports whether the lock file is still this holder's.
func (l *exclLock) held() bool {
	b, err := os.ReadFile(l.path)
	return err == nil && string(b) == l.token
}

func (l *exclLock) Unlock() error {
	if !l.held() {
		return errLockLost
	}
	return os.Remove(l.path)
}

func (l *exclLock) Renew() (bool, error) {
	if !l.held() {
		return false, nil
	}
	now := time.Now()
	return true, os.Chtimes(l.path, now, now)
}

func (l *exclLock) Close() error { return nil }
