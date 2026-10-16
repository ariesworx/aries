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
	// Control, not Read: Read takes the descriptor's read lock first, and
	// pgx can leave a background reader holding it, blocked, on an idle
	// connection (draining after a canceled query), so a Read here would
	// wait as long as that reader, and the pool's acquire with it. A
	// peek that does not wait needs no lock.
	err = raw.Control(func(fd uintptr) {
		n, _, rerr = syscall.Recvfrom(int(fd), buf[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT) //nolint:gosec // a socket's descriptor
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
