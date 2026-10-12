//go:build !linux

package sdnotify

// New returns a Notifier that sends nothing: systemd is Linux's.
func New(func(string) string, int) *Notifier { return &Notifier{} }

// send is never reached off Linux, where no Notifier has a socket.
func send(string, string) error { return nil }
