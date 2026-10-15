//go:build !windows

package client

import "syscall"

// resets are the errors of a connection the far end reset or aborted.
var resets = []error{syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE}
