// Package sdnotify tells systemd how the daemon is doing, over the
// socket systemd names in NOTIFY_SOCKET (sd_notify(3)): that it is
// ready, that it is stopping, and, when the unit sets WatchdogSec, that
// it is alive. It uses the standard library only: a notification is one
// datagram to a unix socket.
//
// It does something on Linux only. Elsewhere, and when the variables are
// absent, as they are for a daemon not started by systemd or by a unit
// without Type=notify, every method does nothing. The package reads no
// environment variable itself: the main package hands it the lookup.
package sdnotify

import "time"

// The variables systemd sets for a service.
const (
	// EnvSocket names the notify socket: a path, or an abstract socket
	// when it starts with "@".
	EnvSocket = "NOTIFY_SOCKET"
	// EnvWatchdog is the unit's WatchdogSec in microseconds, and
	// EnvWatchdogPID the process it applies to.
	EnvWatchdog    = "WATCHDOG_USEC"
	EnvWatchdogPID = "WATCHDOG_PID"
)

// Notifier sends notifications to systemd. A nil Notifier, and one made
// where there is no notify socket, send nothing.
type Notifier struct {
	// addr is the notify socket, or empty when there is none.
	addr string
	// every is how often the watchdog must be fed, or zero when the unit
	// has none.
	every time.Duration
}

// Ready tells systemd the daemon has started: a unit with Type=notify is
// then active, and what is ordered after it starts.
func (n *Notifier) Ready() error { return n.notify("READY=1") }

// Stopping tells systemd the daemon is shutting down.
func (n *Notifier) Stopping() error { return n.notify("STOPPING=1") }

// Watchdog tells systemd the daemon is alive. With WatchdogSec set,
// systemd restarts a service that has not said so for that long.
func (n *Notifier) Watchdog() error {
	if n == nil || n.every == 0 {
		return nil
	}
	return n.notify("WATCHDOG=1")
}

// WatchdogEvery returns how often Watchdog must be called: half the
// unit's WatchdogSec, as sd_watchdog_enabled(3) advises, or zero when
// the unit sets none, there is no notify socket, or the watchdog is for
// another process.
func (n *Notifier) WatchdogEvery() time.Duration {
	if n == nil {
		return 0
	}
	return n.every
}

func (n *Notifier) notify(state string) error {
	if n == nil || n.addr == "" {
		return nil
	}
	return send(n.addr, state)
}
