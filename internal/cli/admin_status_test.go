package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// `aries admin status` refuses bad usage before dialing.
func TestAdminStatusUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	for _, tc := range []struct {
		name   string
		args   []string
		stderr string
	}{
		{"takes no arguments", []string{"admin", "status", "acme"}, "admin status takes no arguments"},
		{"takes no project", []string{"admin", "status", "--project", "acme"}, "flag provided but not defined: -project"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
			if code != ExitUsage || out != "" || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: usage: aries admin status [-n N] [--after NAME]") {
				t.Errorf("aries %s: exit %d (want %d), stdout %q, stderr %q; want it to name %q and the usage",
					strings.Join(tc.args, " "), code, ExitUsage, out, errb, tc.stderr)
			}
		})
	}
	if help := helpText(t, "admin"); !strings.Contains(help, "admin status [-n N] [--after NAME]") {
		t.Errorf("aries help admin lacks the status command:\n%s", help)
	}
}

// The keys of a principal say when connections came in by the server's
// hand-maintained authorized_keys file.
func TestPrintFileConns(t *testing.T) {
	t.Parallel()
	const fp = "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name  string
		print func(*bytes.Buffer)
		want  string
	}{
		{"keys, with connections by the file", func(w *bytes.Buffer) {
			printKeys(w, proto.KeysResult{Principal: "bob", FileConns: 2, Keys: []proto.Key{{Fingerprint: fp, Type: "ssh-ed25519", AddedBy: "dana", AddedAt: at}}})
		}, "keys of bob\n" +
			"FINGERPRINT                                         TYPE         COMMENT  ADDED BY  ADDED\n" +
			fp + "  ssh-ed25519           dana      2026-10-11 09:30 UTC\n" +
			"2 open connections of bob came in by a line of the server's hand-maintained authorized_keys file; removing a key from the store does not close them\n"},
		{"no keys, and a connection by the file", func(w *bytes.Buffer) { printKeys(w, proto.KeysResult{Principal: "bob", FileConns: 1}) },
			"no keys recorded for bob\n" +
				"1 open connection of bob came in by a line of the server's hand-maintained authorized_keys file; removing a key from the store does not close it\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var w bytes.Buffer
			tc.print(&w)
			if got := w.String(); got != tc.want {
				t.Errorf("printed\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}
