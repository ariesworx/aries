package cli

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// The access commands refuse bad usage before dialing.
func TestAdminAccessUsage(t *testing.T) {
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
		{"principal needs an action", []string{"admin", "principal"}, "admin principal needs add, list, disable or enable"},
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
			if code != ExitUsage || out != "" || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: usage: sfx admin "+group+" ") {
				t.Errorf("sfx %s: exit %d (want %d), stdout %q, stderr %q; want it to name %q and the group's usage",
					strings.Join(tc.args, " "), code, ExitUsage, out, errb, tc.stderr)
			}
		})
	}
	// Every group is in the command's help.
	_, help, _ := runCLI(t, "help", "admin")
	for _, want := range []string{"admin prices [set MODEL", "admin plans [set NAME", "admin project add NAME [--prefix P] [--account A] | list | archive PROJECT | activate PROJECT",
		"admin principal add NAME [--project P | --no-project] | list | disable NAME | enable NAME",
		"admin member add NAME [--project P] [--role admin] | list [--project P] | remove NAME [--project P]"} {
		if !strings.Contains(help, want) {
			t.Errorf("sfx help admin lacks %q:\n%s", want, help)
		}
	}
}

func TestPrintAdminLists(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	const a, b = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f", "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	tests := []struct {
		name  string
		print func(*bytes.Buffer)
		want  string
	}{
		{"projects, for a server admin", func(w *bytes.Buffer) {
			printProjects(w, proto.ProjectsResult{Projects: []proto.Project{
				{ID: a, Name: "acme", Prefix: "sf", Account: "internal", State: "active", Database: "starfix", CreatedBy: "import"},
				{ID: b, Name: "web", Prefix: "web", Account: "acme-2026", State: "archived", Database: "starfix_p001", CreatedBy: "dana"}}, More: 3})
		}, "NAME  ID                                    PREFIX  ACCOUNT    STATE     DATABASE\n" +
			"acme  " + a + "  sf      internal   active    starfix\n" +
			"web   " + b + "  web     acme-2026  archived  starfix_p001\n" +
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
		{"member added from another project, with keys", func(w *bytes.Buffer) {
			printMemberSet(w, proto.MemberSetResult{ID: "bob", Rev: 1, Project: a, ProjectName: "acme", Role: "member", Change: "added", Projects: 2,
				Keys: []string{"SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"}})
		}, "bob added to project acme as member\n" +
			"bob belongs to 2 projects, and each key recorded for bob now reaches this one too:\n" +
			"  SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU\n"},
		{"member added from another project, without keys", func(w *bytes.Buffer) {
			printMemberSet(w, proto.MemberSetResult{ID: "bob", Rev: 1, Project: a, ProjectName: "acme", Role: "member", Change: "added", Projects: 3})
		}, "bob added to project acme as member\nbob belongs to 3 projects; no key is recorded for bob\n"},
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
