package sdnotify

import (
	"os"
	"testing"
)

// With no notify socket, and on a nil Notifier, nothing is sent and
// nothing fails. Off Linux that is every Notifier.
func TestNoSocket(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    *Notifier
	}{
		{"nil", nil},
		{"no variables", New(func(string) string { return "" }, os.Getpid())},
		{"a relative socket", New(func(k string) string {
			return map[string]string{EnvSocket: "notify.sock", EnvWatchdog: "30000000"}[k]
		}, os.Getpid())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.n.Ready(); err != nil {
				t.Errorf("Ready() = %v, want nil", err)
			}
			if err := tc.n.Watchdog(); err != nil {
				t.Errorf("Watchdog() = %v, want nil", err)
			}
			if err := tc.n.Stopping(); err != nil {
				t.Errorf("Stopping() = %v, want nil", err)
			}
			if d := tc.n.WatchdogEvery(); d != 0 {
				t.Errorf("WatchdogEvery() = %s, want 0", d)
			}
		})
	}
}
