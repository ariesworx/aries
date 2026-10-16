//go:build linux || darwin

package store

import (
	"errors"
	"net"
	"syscall"
)

// idleNews reports whether nc, a connection between statements, has
// bytes waiting to be read or has been closed by its peer, without
// reading them or waiting: one recv that peeks and does not block. Where
// it cannot tell, it reports false.
func idleNews(nc net.Conn) bool {
	sc, ok := nc.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	var (
		n    int
		rerr error
		buf  [1]byte
	)
	// Returning true from the function never waits for the socket to
	// become readable.
	err = raw.Read(func(fd uintptr) bool {
		n, _, rerr = syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT) //nolint:gosec // a socket's descriptor
		return true
	})
	switch {
	case err != nil:
		return true // the descriptor is closed
	case n > 0:
		return true // a message nobody asked for: an error before closing
	case errors.Is(rerr, syscall.EAGAIN), errors.Is(rerr, syscall.EINTR):
		return false
	}
	// End of file, or an error such as a reset.
	return true
}
