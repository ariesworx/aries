package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// defaultLock is LockFileEx: a lock on a byte range of an open file, which
// Windows releases when the handle closes, and so when the process ends.
// It is what the Go command's own lockedfile package uses on Windows. The
// syscall package does not wrap it, so it is called through kernel32.
const defaultLock = "lockfileex"

func lockKinds() []string { return []string{"lockfileex", "excl"} }

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errLockViolation        = syscall.Errno(33)  // ERROR_LOCK_VIOLATION
	errIOPending            = syscall.Errno(997) // ERROR_IO_PENDING
)

type fileLock struct{ f *os.File }

func openOSLock(kind, path string) (locker, error) {
	if kind != "lockfileex" {
		return nil, fmt.Errorf("no lock primitive %q on this system", kind)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	return &fileLock{f: f}, nil
}

// lock locks the first byte of the file. The handle is synchronous, so
// without lockfileFailImmediately the call blocks until the lock is free.
func (l *fileLock) lock(flags uintptr) (bool, error) {
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(l.f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r != 0 {
		return true, nil
	}
	if errors.Is(err, errLockViolation) || errors.Is(err, errIOPending) {
		return false, nil
	}
	return false, fmt.Errorf("LockFileEx: %w", err)
}

func (l *fileLock) Lock() error {
	ok, err := l.lock(lockfileExclusiveLock)
	if err == nil && !ok {
		err = errors.New("LockFileEx: returned without the lock")
	}
	return err
}

func (l *fileLock) TryLock() (bool, error) {
	return l.lock(lockfileExclusiveLock | lockfileFailImmediately)
}

func (l *fileLock) Unlock() error {
	var ol syscall.Overlapped
	r, _, err := procUnlockFileEx.Call(l.f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}

func (l *fileLock) Close() error { return l.f.Close() }
