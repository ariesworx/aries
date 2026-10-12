package e2e

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// second is the second project of the worlds that serve many.
const second = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"

// dana is the control store's server admin in the worlds that serve
// many.
var dana = store.Actor{Principal: "dana", Session: "e2e", Machine: "server"}

// addProject adds an active project in a database of its own, through
// the daemon's control store: `sfx admin project add` does not exist yet.
func (w *world) addProject(id, name, prefix string) {
	t := w.t
	t.Helper()
	dsn, err := dolt.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ctl.AddProject(t.Context(), dana, store.NewProject{ID: id, Name: name, Prefix: prefix, Account: "internal",
		DBName: cfg.DBName}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ctl.ActivateProject(t.Context(), dana, id); err != nil {
		t.Fatal(err)
	}
}

// member gives principal a role in a project, adding the principal if
// the control store has no row for it.
func (w *world) member(principal, proj string, role store.Role) {
	t := w.t
	t.Helper()
	if _, err := w.ctl.Principal(t.Context(), principal); errors.Is(err, store.ErrNotFound) {
		if _, err := w.ctl.AddPrincipal(t.Context(), dana, principal, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.ctl.SetMember(t.Context(), dana, principal, proj, role); err != nil {
		t.Fatal(err)
	}
}

// TestTwoProjects runs two projects on one server, through the CLI, SSH
// and the daemon: each in its own database, reached by its members only.
func TestTwoProjects(t *testing.T) {
	w := newWorld(t, daemonOpts{many: true, admins: []string{"dana"}})
	w.addProject(second, "second", "two")
	w.member("alice", project, store.RoleMember)
	w.member("alice", second, store.RoleMember)
	w.member("bob", project, store.RoleMember) // the first only
	w.member("erin", project, store.RoleAdmin)
	w.member("erin", second, store.RoleMember)

	alice1, alice2 := w.newUser("alice", ""), w.newUserIn("alice", "", second)
	bob1, bob2 := w.newUser("bob", ""), w.newUserIn("bob", "", second)
	erin1, erin2 := w.newUser("erin", ""), w.newUserIn("erin", "", second)
	dana1, dana2 := w.newUser("dana", ""), w.newUserIn("dana", "", second)

	// The same principal works in both, with separate data.
	in1 := decode[proto.WriteResult](t, alice1.ok("create", "In the first", "--json"))
	in2 := decode[proto.WriteResult](t, alice2.ok("create", "In the second", "--json"))
	if !strings.HasPrefix(in1.ID, "sf-") || !strings.HasPrefix(in2.ID, "two-") {
		t.Errorf("ids %q and %q, want each project's prefix (sf-, two-)", in1.ID, in2.ID)
	}
	for _, tc := range []struct {
		u     *user
		name  string
		title string
	}{{alice1, "first", "In the first"}, {alice2, "second", "In the second"}} {
		l := decode[proto.ListResult](t, tc.u.ok("list", "--json"))
		if len(l.Issues) != 1 || l.Issues[0].Title != tc.title {
			t.Errorf("the %s project lists %+v, want only %q", tc.name, l.Issues, tc.title)
		}
	}
	// An issue id from one is not visible in the other.
	if r := alice2.run("v0.2.0", "show", in1.ID); r.code != cli.ExitFailure || !strings.Contains(r.stderr, in1.ID+" not found") {
		t.Errorf("show of the first project's %s in the second: exit %d\n%s", in1.ID, r.code, r.stderr)
	}

	// A member of one only is refused in the other, and told whom to ask.
	bob1.ok("list")
	r := bob2.run("v0.2.0", "list")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, `bob is not a member of project "`+second+`"`) ||
		!strings.Contains(r.stderr, "fix: ask a server admin, or one of the project's admins, to add you to the project") {
		t.Errorf("bob in the second project: exit %d\n%s", r.code, r.stderr)
	}
	// So is someone the control store has never heard of, the same way.
	r = w.newUser("mallory", "").run("v0.2.0", "list")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "mallory is not a member of project") {
		t.Errorf("mallory, in no project: exit %d\n%s", r.code, r.stderr)
	}
	// A project the server does not hold is refused as it always was.
	r = w.newUserIn("alice", "", "00000000-0000-4000-8000-00000000dead").run("v0.2.0", "list")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "is not served by this server") {
		t.Errorf("an unknown project: exit %d\n%s", r.code, r.stderr)
	}

	// A project admin overrides a held issue in their project, and not in
	// the other, where they are a member.
	alice1.ok("start", in1.ID)
	alice2.ok("start", in2.ID)
	erin1.ok("update", in1.ID, "--title", "Changed by the project's admin")
	r = erin2.run("v0.2.0", "update", in2.ID, "--title", "Changed by a member")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "is held by alice") ||
		!strings.Contains(r.stderr, "only the holder or an admin may change it") {
		t.Errorf("erin, a member of the second project, changing alice's held issue: exit %d\n%s", r.code, r.stderr)
	}
	// A server admin overrides in both, with no membership in either.
	dana1.ok("update", in1.ID, "--title", "Changed by a server admin")
	dana2.ok("update", in2.ID, "--title", "Changed by a server admin")
	for _, tc := range []struct {
		u    *user
		id   string
		want []string
	}{{alice1, in1.ID, []string{"erin", "dana"}}, {alice2, in2.ID, []string{"dana"}}} {
		h := decode[proto.HistoryResult](t, tc.u.ok("history", tc.id, "--json"))
		var by []string
		for _, e := range h.Events {
			if e.Op == string(store.OpAdminOverride) {
				by = append(by, e.Principal)
			}
		}
		if strings.Join(by, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s has admin.override events by %v, want %v", tc.id, by, tc.want)
		}
	}

	// A role taken away counts at once, on a store that stays open.
	w.member("erin", project, store.RoleMember)
	r = erin1.run("v0.2.0", "update", in1.ID, "--title", "No longer an admin")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "is held by alice") {
		t.Errorf("erin, no longer an admin of the first project: exit %d\n%s", r.code, r.stderr)
	}
	// A membership ended refuses the next connection.
	if _, err := w.ctl.RemoveMember(t.Context(), dana, "bob", project); err != nil {
		t.Fatal(err)
	}
	if r := bob1.run("v0.2.0", "list"); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "bob is not a member of project") {
		t.Errorf("bob after his membership ended: exit %d\n%s", r.code, r.stderr)
	}

	// The daemon restarts into the same projects: nothing lives in it.
	if err := w.restartDaemon(); err != nil {
		t.Fatal(err)
	}
	if l := decode[proto.ListResult](t, alice2.ok("list", "--json")); len(l.Issues) != 1 || l.Issues[0].ID != in2.ID {
		t.Errorf("after a restart the second project lists %+v, want %s", l.Issues, in2.ID)
	}
}
