package client

import "syscall"

// resets are the errors of a connection the far end reset or aborted.
// Windows reports codes of its own, which syscall.ECONNRESET is not.
var resets = []error{syscall.WSAECONNRESET, syscall.WSAECONNABORTED}
