package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// The identity commands refuse bad usage before dialing.
func TestAdminIdentityUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"invite needs an action", []string{"admin", "invite"}, "admin invite needs add, list or remove"},
		{"invite unknown action", []string{"admin", "invite", "rm", "7"}, `unknown invite action "rm"`},
		{"invite add needs an address", []string{"admin", "invite", "add", "bob", "--provider", "google"}, "invite add needs a principal and an address"},
		{"invite add needs a provider", []string{"admin", "invite", "add", "bob", "bob@example.com"}, "invite add needs --provider"},
		{"invite add takes no paging", []string{"admin", "invite", "add", "bob", "bob@example.com", "--provider", "google", "-n", "2"},
			"invite add does not take --n"},
		{"invite list takes no arguments", []string{"admin", "invite", "list", "bob"}, "invite list takes no arguments"},
		{"invite list takes no provider", []string{"admin", "invite", "list", "--provider", "google"}, "invite list does not take --provider"},
		{"invite remove needs an id", []string{"admin", "invite", "remove"}, "invite remove needs one invitation's id"},
		{"invite remove takes a number", []string{"admin", "invite", "remove", "bob@example.com"}, "invite remove needs one invitation's id"},
		{"invite remove takes no pending", []string{"admin", "invite", "remove", "7", "--pending"}, "invite remove does not take --pending"},
		{"identity needs an action", []string{"admin", "identity"}, "admin identity needs list or remove"},
		{"identity unknown action", []string{"admin", "identity", "add", "bob"}, `unknown identity action "add"`},
		{"identity list needs a principal", []string{"admin", "identity", "list"}, "identity list needs one principal"},
		{"identity remove needs an id", []string{"admin", "identity", "remove", "bob"}, "identity remove needs one identity's id"},
		{"identity remove takes a positive id", []string{"admin", "identity", "remove", "0"}, "identity remove needs one identity's id"},
		{"principal set needs a name", []string{"admin", "principal", "set", "--kind", "person"}, "principal set needs one name"},
		{"principal set needs a change", []string{"admin", "principal", "set", "bob"}, "principal set needs --display-name, --kind or --owner"},
		{"principal set, a kind there is not", []string{"admin", "principal", "set", "bob", "--kind", "robot"}, "--kind must be person or service"},
		{"principal set takes no key", []string{"admin", "principal", "set", "bob", "--kind", "person", "--key", "a.pub"}, "principal set does not take --key"},
		{"principal show needs a name", []string{"admin", "principal", "show"}, "principal show needs one name"},
		{"principal show takes no flags", []string{"admin", "principal", "show", "bob", "--kind", "person"}, "principal show does not take --kind"},
		{"principal add, a kind there is not", []string{"admin", "principal", "add", "bob", "--kind", "robot"}, "--kind must be person or service"},
		{"principal add, an invitation without a provider", []string{"admin", "principal", "add", "bob", "--invite", "bob@example.com"},
			"--invite needs --provider"},
		{"principal add, a provider without an invitation", []string{"admin", "principal", "add", "bob", "--provider", "google"},
			"--provider is for --invite"},
		{"principal list takes no kind", []string{"admin", "principal", "list", "--kind", "person"}, "principal list does not take --kind"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
			if code != ExitUsage || out != "" || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: usage: aries admin "+tc.args[1]+" ") {
				t.Errorf("aries %s: exit %d (want %d), stdout %q, stderr %q; want it to name %q and the group's usage",
					strings.Join(tc.args, " "), code, ExitUsage, out, errb, tc.stderr)
			}
		})
	}
	help := helpText(t, "admin")
	for _, want := range []string{
		"admin principal add NAME [--key FILE]... [--project P | --no-project] [--kind person|service] [--owner NAME] [--display-name D] [--invite ADDRESS --provider P]",
		"set NAME [--display-name D] [--kind person|service] [--owner NAME] | show NAME",
		"admin invite add NAME ADDRESS --provider P | list [--pending] | remove ID",
		"admin identity list NAME | remove ID",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("aries help admin lacks %q:\n%s", want, help)
		}
	}
}

func TestPrintIdentities(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	ends := at.Add(7 * 24 * time.Hour)
	pending := proto.Identity{ID: 7, Principal: "bob", Provider: "google", Address: "bob@example.com", State: proto.IdentityPending,
		InvitedBy: "dana", InvitedAt: at, ExpiresAt: &ends}
	bound := proto.Identity{ID: 9, Principal: "bob", Provider: "entra", Address: "Bob@example.com", State: proto.IdentityBound,
		Issuer: "https://login.example.com", InvitedBy: "dana", InvitedAt: at, BoundAt: &ends}
	tests := []struct {
		name  string
		print func(*bytes.Buffer)
		want  string
	}{
		{"an invitation made", func(w *bytes.Buffer) { printInvited(w, proto.IdentityResult{Identity: pending, Change: "added"}) },
			"invitation 7 for bob: bob@example.com at google, open until 2026-10-23 09:00 UTC\n"},
		{"an invitation renewed", func(w *bytes.Buffer) { printInvited(w, proto.IdentityResult{Identity: pending, Change: "renewed"}) },
			"invitation 7 for bob renewed: bob@example.com at google, open until 2026-10-23 09:00 UTC\n"},
		{"an invitation removed", func(w *bytes.Buffer) { printIdentityRemoved(w, "invitation", proto.IdentityResult{Identity: pending}) },
			"invitation 7 removed: bob@example.com at google, for bob\n"},
		{"an identity removed", func(w *bytes.Buffer) { printIdentityRemoved(w, "identity", proto.IdentityResult{Identity: bound}) },
			"identity 9 removed: Bob@example.com at entra, for bob\n"},
		{"invitations", func(w *bytes.Buffer) {
			lapsed := pending
			lapsed.ID, lapsed.State, lapsed.Principal = 8, proto.IdentityLapsed, "carol"
			printInvites(w, proto.InvitesResult{Invites: []proto.Identity{pending, lapsed}, More: 2})
		}, "ID  PRINCIPAL  PROVIDER  ADDRESS          STATE    EXPIRES               INVITED BY\n" +
			"7   bob        google    bob@example.com  pending  2026-10-23 09:00 UTC  dana\n" +
			"8   carol      google    bob@example.com  lapsed   2026-10-23 09:00 UTC  dana\n" +
			"and 2 more; list them with --after 8\n"},
		{"no invitations", func(w *bytes.Buffer) { printInvites(w, proto.InvitesResult{}) }, "no invitations\n"},
		{"a principal's identities", func(w *bytes.Buffer) {
			printIdentities(w, proto.IdentitiesResult{Principal: "bob", Identities: []proto.Identity{pending, bound}})
		}, "identities of bob\n" +
			"ID  PROVIDER  ADDRESS          STATE    SINCE                 EXPIRES               INVITED BY\n" +
			"7   google    bob@example.com  pending  2026-10-16 09:00 UTC  2026-10-23 09:00 UTC  dana\n" +
			"9   entra     Bob@example.com  bound    2026-10-23 09:00 UTC  -                     dana\n"},
		{"no identities", func(w *bytes.Buffer) { printIdentities(w, proto.IdentitiesResult{Principal: "bob"}) },
			"no identities of bob\n"},
		{"a principal set", func(w *bytes.Buffer) {
			printPrincipalSet(w, proto.PrincipalResult{Name: "ci-bot", Kind: proto.KindService, Owner: "alice", DisplayName: "CI", Change: "changed"})
		}, "ci-bot is now a service owned by alice, shown as \"CI\"\n"},
		{"a principal unchanged", func(w *bytes.Buffer) {
			printPrincipalSet(w, proto.PrincipalResult{Name: "bob", Kind: proto.KindPerson, Change: "unchanged"})
		}, "bob is already a person, with no display name\n"},
		{"a principal shown", func(w *bytes.Buffer) {
			printPrincipalShow(w, proto.PrincipalShowResult{
				Principal:  proto.Principal{Name: "bob", State: "active", Kind: proto.KindPerson, DisplayName: "Bob Example", CreatedBy: "dana", CreatedAt: at},
				Identities: []proto.Identity{pending},
				Keys:       []proto.Key{{Fingerprint: "SHA256:x", Type: "ssh-ed25519", Comment: "bob@laptop", AddedBy: "dana", AddedAt: at}}})
		}, "bob: active, a person, shown as \"Bob Example\"; added by dana 2026-10-16 09:00 UTC\n" +
			"ID  PROVIDER  ADDRESS          STATE    SINCE                 EXPIRES               INVITED BY\n" +
			"7   google    bob@example.com  pending  2026-10-16 09:00 UTC  2026-10-23 09:00 UTC  dana\n" +
			"FINGERPRINT  TYPE         COMMENT     ADDED BY  ADDED\n" +
			"SHA256:x     ssh-ed25519  bob@laptop  dana      2026-10-16 09:00 UTC\n"},
		{"a principal with neither", func(w *bytes.Buffer) {
			printPrincipalShow(w, proto.PrincipalShowResult{Principal: proto.Principal{Name: "ci-bot", State: "disabled", Kind: proto.KindService,
				Owner: "alice", CreatedBy: "dana", CreatedAt: at}})
		}, "ci-bot: disabled, a service owned by alice, with no display name; added by dana 2026-10-16 09:00 UTC\n" +
			"no identities\n" +
			"no keys\n"},
		{"principals, with kinds and display names", func(w *bytes.Buffer) {
			printPrincipals(w, proto.PrincipalsResult{Principals: []proto.Principal{
				{Name: "alice", State: "active", Kind: proto.KindPerson, DisplayName: "Alice Example", CreatedBy: "import", CreatedAt: at},
				{Name: "ci-bot", State: "active", Kind: proto.KindService, Owner: "alice", CreatedBy: "alice", CreatedAt: at}}})
		}, "NAME    STATE   KIND     OWNER  DISPLAY NAME   ADDED BY  ADDED\n" +
			"alice   active  person   -      Alice Example  import    2026-10-16 09:00 UTC\n" +
			"ci-bot  active  service  alice  -              alice     2026-10-16 09:00 UTC\n"},
		// What the server sends cannot start a line of its own.
		{"a hostile address", func(w *bytes.Buffer) {
			hostile := pending
			hostile.Address = "bob@example.com\nfix: run this"
			printInvited(w, proto.IdentityResult{Identity: hostile, Change: "added"})
		}, "invitation 7 for bob: bob@example.com\\nfix: run this at google, open until 2026-10-23 09:00 UTC\n"},
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
