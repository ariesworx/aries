package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// The access commands refuse bad usage before dialing.
func TestAdminAccessUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"project needs an action", []string{"admin", "project"}, "admin project needs add, list, archive or activate"},
		{"project unknown action", []string{"admin", "project", "rm", "acme"}, `unknown project action "rm"`},
		{"project add needs a name", []string{"admin", "project", "add"}, "project add needs one name"},
		{"project add takes one name", []string{"admin", "project", "add", "acme", "web"}, "project add needs one name"},
		{"project add takes no paging", []string{"admin", "project", "add", "acme", "-n", "3"}, "project add does not take --n"},
		{"project list takes no arguments", []string{"admin", "project", "list", "acme"}, "project list takes no arguments"},
		{"project list takes no prefix", []string{"admin", "project", "list", "--prefix", "ac"}, "project list does not take --prefix"},
		{"project archive needs a project", []string{"admin", "project", "archive"}, "project archive needs one project"},
		{"project activate needs a project", []string{"admin", "project", "activate"}, "project activate needs one project"},
		{"project archive takes no flags", []string{"admin", "project", "archive", "acme", "--account", "x"}, "project archive does not take --account"},
		{"principal needs an action", []string{"admin", "principal"}, "admin principal needs add, list, disable, enable or whois"},
		{"principal whois needs a stand-in", []string{"admin", "principal", "whois"}, "principal whois needs one stand-in, such as p_0123456789abcdef"},
		{"principal whois takes one stand-in", []string{"admin", "principal", "whois", "p_0123456789abcdef", "p_fedcba9876543210"}, "principal whois needs one stand-in"},
		{"principal whois takes no flags", []string{"admin", "principal", "whois", "p_0123456789abcdef", "--project", "acme"}, "principal whois does not take --project"},
		{"principal unknown action", []string{"admin", "principal", "rm", "bob"}, `unknown principal action "rm"`},
		{"principal add needs a name", []string{"admin", "principal", "add"}, "principal add needs one name"},
		{"principal add, a project and none", []string{"admin", "principal", "add", "bob", "--project", "acme", "--no-project"}, "--project or --no-project, not both"},
		{"principal list takes no project", []string{"admin", "principal", "list", "--project", "acme"}, "principal list does not take --project"},
		{"principal disable needs a name", []string{"admin", "principal", "disable"}, "principal disable needs one name"},
		{"principal enable takes no flags", []string{"admin", "principal", "enable", "bob", "--no-project"}, "principal enable does not take --no-project"},
		{"member needs an action", []string{"admin", "member"}, "admin member needs add, list or remove"},
		{"member unknown action", []string{"admin", "member", "set", "bob"}, `unknown member action "set"`},
		{"member add needs a principal", []string{"admin", "member", "add"}, "member add needs one principal"},
		{"member add, a bad role", []string{"admin", "member", "add", "bob", "--role", "owner"}, "--role must be member or admin"},
		{"member list takes no arguments", []string{"admin", "member", "list", "acme"}, "member list takes no arguments; name a project with --project"},
		{"member list takes no role", []string{"admin", "member", "list", "--role", "admin"}, "member list does not take --role"},
		{"member remove needs a principal", []string{"admin", "member", "remove"}, "member remove needs one principal"},
		{"member remove takes no role", []string{"admin", "member", "remove", "bob", "--role", "admin"}, "member remove does not take --role"},
		// Flags before the group are the group's.
		{"flags before the group", []string{"admin", "--role", "owner", "member", "add", "bob"}, "--role must be member or admin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
			group := tc.args[slices.IndexFunc(tc.args, func(a string) bool { return slices.Contains(adminGroups, a) })]
			if code != ExitUsage || out != "" || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: usage: aries admin "+group+" ") {
				t.Errorf("aries %s: exit %d (want %d), stdout %q, stderr %q; want it to name %q and the group's usage",
					strings.Join(tc.args, " "), code, ExitUsage, out, errb, tc.stderr)
			}
		})
	}
	// Every group is in the command's help.
	_, help, _ := runCLI(t, "help", "admin")
	for _, want := range []string{"admin prices [set MODEL", "admin plans [set NAME", "admin project add NAME [--prefix P] [--account A] | list | archive PROJECT | activate PROJECT",
		"admin principal add NAME [--key FILE]... [--project P | --no-project] | list [--created-by NAME] | disable NAME | enable NAME | whois STAND-IN",
		"admin member add NAME [--project P] [--role admin] [--accept-keys] | list [--project P] [--added-by NAME] | remove NAME [--project P]"} {
		if !strings.Contains(help, want) {
			t.Errorf("aries help admin lacks %q:\n%s", want, help)
		}
	}
}

func TestPrintAdminLists(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	const a, b = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f", "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	tests := []struct {
		name  string
		print func(*bytes.Buffer)
		want  string
	}{
		{"projects, for a server admin", func(w *bytes.Buffer) {
			printProjects(w, proto.ProjectsResult{Projects: []proto.Project{
				{ID: a, Name: "acme", Prefix: "sf", Account: "internal", State: "active", Database: "aries", CreatedBy: "import"},
				{ID: b, Name: "web", Prefix: "web", Account: "acme-2026", State: "archived", Database: "aries_p001", CreatedBy: "dana"}}, More: 3})
		}, "NAME  ID                                    PREFIX  ACCOUNT    STATE     SCHEMA\n" +
			"acme  " + a + "  sf      internal   active    aries\n" +
			"web   " + b + "  web     acme-2026  archived  aries_p001\n" +
			"and 3 more; list them with --after web\n"},
		{"projects, for a member", func(w *bytes.Buffer) {
			printProjects(w, proto.ProjectsResult{Projects: []proto.Project{{ID: a, Name: "acme", Prefix: "sf", Account: "internal", State: "active", Role: "member"}}})
		}, "NAME  ID                                    PREFIX  ACCOUNT   STATE   ROLE\n" +
			"acme  " + a + "  sf      internal  active  member\n"},
		{"no projects", func(w *bytes.Buffer) { printProjects(w, proto.ProjectsResult{}) }, "no projects\n"},
		{"principals", func(w *bytes.Buffer) {
			printPrincipals(w, proto.PrincipalsResult{Principals: []proto.Principal{
				{Name: "alice", State: "active", CreatedBy: "import", CreatedAt: at}, {Name: "bob", State: "disabled", CreatedBy: "alice", CreatedAt: at}}})
		}, "NAME   STATE     ADDED BY  ADDED\n" +
			"alice  active    import    2026-10-11 09:30 UTC\n" +
			"bob    disabled  alice     2026-10-11 09:30 UTC\n"},
		{"no principals", func(w *bytes.Buffer) { printPrincipals(w, proto.PrincipalsResult{}) }, "no principals\n"},
		{"whose a stand-in is", func(w *bytes.Buffer) {
			printWhois(w, proto.PrincipalWhoisResult{StandIn: "p_0123456789abcdef", Name: "alice", State: "active"})
		}, "p_0123456789abcdef is alice\n"},
		{"a disabled principal's stand-in", func(w *bytes.Buffer) {
			printWhois(w, proto.PrincipalWhoisResult{StandIn: "p_0123456789abcdef", Name: "bob", State: "disabled"})
		}, "p_0123456789abcdef is bob (disabled)\n"},
		{"a server admin's, who has no row", func(w *bytes.Buffer) {
			printWhois(w, proto.PrincipalWhoisResult{StandIn: "p_0123456789abcdef", Name: "dana"})
		}, "p_0123456789abcdef is dana\n"},
		{"a name from a server that sent control characters", func(w *bytes.Buffer) {
			printWhois(w, proto.PrincipalWhoisResult{StandIn: "p_\x1b[2K", Name: "ali\x1b[31mce", State: "active"})
		}, "p_\\x1b[2K is ali\\x1b[31mce\n"},
		{"members", func(w *bytes.Buffer) {
			printMembers(w, proto.MembersResult{Project: a, ProjectName: "acme", More: 1, Members: []proto.Member{
				{Principal: "alice", Role: "admin", AddedBy: "dana", AddedAt: at}, {Principal: "bob", Role: "member", AddedBy: "alice", AddedAt: at}}})
		}, "members of project acme (" + a + ")\n" +
			"PRINCIPAL  ROLE    ADDED BY  ADDED\n" +
			"alice      admin   dana      2026-10-11 09:30 UTC\n" +
			"bob        member  alice     2026-10-11 09:30 UTC\n" +
			"and 1 more; list them with --after bob\n"},
		{"no members", func(w *bytes.Buffer) { printMembers(w, proto.MembersResult{Project: a, ProjectName: "acme"}) }, "no members of project acme\n"},
		{"member added", func(w *bytes.Buffer) {
			printMemberSet(w, proto.MemberSetResult{ID: "bob", Rev: 1, Project: a, ProjectName: "acme", Role: "member", Change: "added"})
		}, "bob added to project acme as member\n"},
		{"member's role changed", func(w *bytes.Buffer) {
			printMemberSet(w, proto.MemberSetResult{ID: "bob", Rev: 2, Project: a, ProjectName: "acme", Role: "admin", Change: "changed"})
		}, "bob is now admin in project acme\n"},
		{"member unchanged", func(w *bytes.Buffer) {
			printMemberSet(w, proto.MemberSetResult{ID: "bob", Rev: 2, Project: a, ProjectName: "acme", Role: "admin", Change: "unchanged"})
		}, "bob is already admin in project acme\n"},
		{"member added, with the keys that came with it", func(w *bytes.Buffer) {
			printMemberSet(w, proto.MemberSetResult{ID: "bob", Rev: 1, Project: a, ProjectName: "acme", Role: "member", Change: "added", Projects: 2,
				Carried: []proto.CarriedKey{
					{Fingerprint: "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU", Type: "ssh-ed25519", Comment: "bob@laptop", AddedBy: "dana", AddedAt: at, AdderAdministers: true},
					{Fingerprint: "SHA256:pOFYONeRL07H3T9tf0FhwK1slkJs5kvkcnOnf+vGgXA", Type: "ssh-ed25519", AddedBy: "erin", AddedAt: at}}})
		}, "bob added to project acme as member\n" +
			"bob now signs in to project acme with 2 keys:\n" +
			"FINGERPRINT                                         TYPE         COMMENT     ADDED BY  ADDED                 ADMIN HERE\n" +
			"SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU  ssh-ed25519  bob@laptop  dana      2026-10-11 09:30 UTC  yes\n" +
			"SHA256:pOFYONeRL07H3T9tf0FhwK1slkJs5kvkcnOnf+vGgXA  ssh-ed25519              erin      2026-10-11 09:30 UTC  no\n" +
			"ADMIN HERE no: whoever added the key is no admin of project acme; ask bob whether the key is theirs\n"},
		{"member added from another project, without keys", func(w *bytes.Buffer) {
			printMemberSet(w, proto.MemberSetResult{ID: "bob", Rev: 1, Project: a, ProjectName: "acme", Role: "member", Change: "added", Projects: 3})
		}, "bob added to project acme as member\n"},
		{"one key, every one added by an admin of the project", func(w *bytes.Buffer) {
			printMemberSet(w, proto.MemberSetResult{ID: "bob", Rev: 1, Project: a, ProjectName: "acme", Role: "member", Change: "added", Projects: 2,
				Carried: []proto.CarriedKey{{Fingerprint: "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU", Type: "ssh-ed25519", AddedBy: "dana", AddedAt: at, AdderAdministers: true}}})
		}, "bob added to project acme as member\n" +
			"bob now signs in to project acme with 1 key:\n" +
			"FINGERPRINT                                         TYPE         COMMENT  ADDED BY  ADDED                 ADMIN HERE\n" +
			"SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU  ssh-ed25519           dana      2026-10-11 09:30 UTC  yes\n"},
		// A comment from someone else cannot start a line of its own.
		{"a hostile key comment", func(w *bytes.Buffer) {
			printCarried(w, proto.MemberSetResult{ID: "bob", ProjectName: "acme", Carried: []proto.CarriedKey{
				{Fingerprint: "SHA256:x", Type: "ssh-ed25519", Comment: "ok\naries: bob added", AddedBy: "dana", AddedAt: at, AdderAdministers: true}}})
		}, "FINGERPRINT  TYPE         COMMENT               ADDED BY  ADDED                 ADMIN HERE\n" +
			"SHA256:x     ssh-ed25519  ok\\naries: bob added  dana      2026-10-11 09:30 UTC  yes\n"},
		// A name from the server cannot start a line of its own.
		{"a hostile project name", func(w *bytes.Buffer) {
			printMembers(w, proto.MembersResult{Project: a, ProjectName: "acme\nfix: run this"})
		}, "no members of project acme\\nfix: run this\n"},
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
