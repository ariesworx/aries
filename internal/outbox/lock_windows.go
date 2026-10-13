package outbox

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// The syscall package does not wrap LockFileEx, so it is called through
// kernel32, as the F0 spike did. Nothing in this file has run on Windows
// (doc.go, "Windows").
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

// lockFile takes LockFileEx's exclusive lock on the first byte of f,
// which belongs to the handle and is released when the handle closes. It
// reports false when wait is false and another holder has it. The handle
// is synchronous, so with wait the call blocks until the lock is free.
func lockFile(f *os.File, wait bool) (bool, error) {
	flags := uintptr(lockfileExclusiveLock)
	if !wait {
		flags |= lockfileFailImmediately
	}
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&ol))) //nolint:gosec // LockFileEx takes a pointer to the range
	if r != 0 {
		return true, nil
	}
	if !wait && (errors.Is(err, errLockViolation) || errors.Is(err, errIOPending)) {
		return false, nil
	}
	return false, fmt.Errorf("LockFileEx: %w", err)
}

func unlockFile(f *os.File) error {
	var ol syscall.Overlapped
	r, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&ol))) //nolint:gosec // UnlockFileEx takes a pointer to the range
	if r == 0 {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}

// syncDir does nothing: Windows has no call that syncs a directory.
func syncDir(string) error { return nil }
