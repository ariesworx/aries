//go:build linux

package sdnotify

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// sendTimeout bounds one notification. A datagram to a local socket does
// not wait for its reader unless the reader's queue is full.
const sendTimeout = time.Second

// New returns a Notifier for the socket NOTIFY_SOCKET names, and for the
// watchdog WATCHDOG_USEC sets when WATCHDOG_PID is absent or is pid, the
// daemon's own. getenv looks a variable up, as os.Getenv does. With no
// NOTIFY_SOCKET, or one that is neither an absolute path nor an abstract
// name, the Notifier sends nothing.
func New(getenv func(string) string, pid int) *Notifier {
	n := &Notifier{}
	addr := getenv(EnvSocket)
	if len(addr) < 2 || (addr[0] != '/' && addr[0] != '@') {
		return n
	}
	n.addr = addr
	usec, err := strconv.ParseInt(strings.TrimSpace(getenv(EnvWatchdog)), 10, 64)
	if err != nil || usec <= 0 {
		return n
	}
	if p := strings.TrimSpace(getenv(EnvWatchdogPID)); p != "" && p != strconv.Itoa(pid) {
		return n // the watchdog is for another process
	}
	n.every = time.Duration(usec) * time.Microsecond / 2
	return n
}

// send writes state as one datagram to the unix socket at addr. The net
// package takes a name that starts with "@" as an abstract socket's.
func send(addr, state string) error {
	c, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetWriteDeadline(time.Now().Add(sendTimeout))
	if _, err := c.Write([]byte(state)); err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	return nil
}
