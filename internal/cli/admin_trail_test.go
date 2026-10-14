package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// The lists of what one account added (security review of stage 3a,
// F-2): their usage, refused before dialing, and what they print.
func TestAdminTrailUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"key list, a name and --added-by", []string{"admin", "key", "list", "bob", "--added-by", "dana"}, "key list takes a principal, or --added-by NAME, not both"},
		{"key list, neither", []string{"admin", "key", "list"}, "key list needs one principal, such as bob, or --added-by NAME"},
		{"key list NAME takes no paging", []string{"admin", "key", "list", "bob", "-n", "3"}, "key list, without --added-by, does not take --n"},
		{"key add takes no --added-by", []string{"admin", "key", "add", "bob", "f.pub", "--added-by", "dana"}, "key add, without --added-by, does not take --added-by"},
		{"key remove takes no paging", []string{"admin", "key", "remove", "SHA256:x", "--after", "y"}, "key remove, without --added-by, does not take --after"},
		{"principal add takes no --created-by", []string{"admin", "principal", "add", "bob", "--created-by", "dana"}, "principal add does not take --created-by"},
		{"member add takes no --added-by", []string{"admin", "member", "add", "bob", "--added-by", "dana"}, "member add does not take --added-by"},
		{"member remove takes no --added-by", []string{"admin", "member", "remove", "bob", "--added-by", "dana"}, "member remove does not take --added-by"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
			if code != ExitUsage || out != "" || !strings.Contains(errb, tc.stderr) {
				t.Errorf("sfx %s: exit %d (want %d), stdout %q, stderr %q; want it to say %q", strings.Join(tc.args, " "), code, ExitUsage, out, errb, tc.stderr)
			}
		})
	}
	_, help, _ := runCLI(t, "help", "admin")
	for _, want := range []string{"list [--created-by NAME]", "list [--project P] [--added-by NAME]", "list --added-by NAME"} {
		if !strings.Contains(help, want) {
			t.Errorf("sfx help admin lacks %q:\n%s", want, help)
		}
	}
}

func TestPrintAdminTrail(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name  string
		print func(*bytes.Buffer)
		want  string
	}{
		{"keys someone added", func(w *bytes.Buffer) {
			printKeysAddedBy(w, proto.KeysResult{AddedBy: "dana", More: 2, Keys: []proto.Key{
				{Fingerprint: "SHA256:aaa", Type: "ssh-ed25519", Comment: "laptop", AddedBy: "dana", AddedAt: at, Principal: "dave"},
				{Fingerprint: "SHA256:bbb", Type: "ssh-rsa", AddedBy: "dana", AddedAt: at, Principal: "ghost"}}})
		}, "keys added by dana\n" +
			"FINGERPRINT  PRINCIPAL  TYPE         COMMENT  ADDED\n" +
			"SHA256:aaa   dave       ssh-ed25519  laptop   2026-10-12 09:30 UTC\n" +
			"SHA256:bbb   ghost      ssh-rsa               2026-10-12 09:30 UTC\n" +
			"and 2 more; list them with --after SHA256:bbb\n"},
		{"no keys added", func(w *bytes.Buffer) { printKeysAddedBy(w, proto.KeysResult{AddedBy: "dana"}) }, "no keys added by dana\n"},
		{"memberships someone added", func(w *bytes.Buffer) {
			printMembersAddedBy(w, "dana", proto.MembersResult{More: 1, Members: []proto.Member{
				{Principal: "ghost", Role: "admin", AddedBy: "dana", AddedAt: at, Project: "a", ProjectName: "acme"},
				{Principal: "ghost", Role: "admin", AddedBy: "dana", AddedAt: at, Project: "b", ProjectName: "web"}}})
		}, "memberships added by dana\n" +
			"PROJECT  PRINCIPAL  ROLE   ADDED\n" +
			"acme     ghost      admin  2026-10-12 09:30 UTC\n" +
			"web      ghost      admin  2026-10-12 09:30 UTC\n" +
			"and 1 more; list them with --after web/ghost\n"},
		{"no memberships added", func(w *bytes.Buffer) { printMembersAddedBy(w, "dana", proto.MembersResult{}) }, "no memberships added by dana\n"},
		// A name from the server cannot start a line of its own.
		{"a hostile principal name", func(w *bytes.Buffer) {
			printKeysAddedBy(w, proto.KeysResult{AddedBy: "dana\nsfx: all clear"})
		}, "no keys added by dana\\nsfx: all clear\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tc.print(&b)
			if got := b.String(); got != tc.want {
				t.Errorf("printed\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}
