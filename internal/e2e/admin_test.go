package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
)

// adminWorld is a server of many projects with a block of dbs database
// names, granted of them granted, and room for a test's control writes.
func adminWorld(t *testing.T, dbs, granted int) *world {
	t.Helper()
	return newWorld(t, daemonOpts{many: true, admins: []string{"dana"}, dbs: dbs, granted: granted,
		limits: server.Limits{ControlWriteBurst: 500}})
}

// in returns this user in another repository: the same key, and the
// .starfix.yaml given, as `sfx admin project add` printed it, with the
// key line a committed config never has.
func (u *user) in(config string) *user {
	t := u.w.t
	t.Helper()
	repo := t.TempDir()
	key, err := os.ReadFile(filepath.Join(u.repo, "id_ed25519"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "id_ed25519"), key, 0o600); err != nil { //nolint:gosec // the test's own temp dir
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".starfix.yaml"), []byte(config+"key: id_ed25519\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &user{w: u.w, repo: repo, env: map[string]string{}}
}

// refused runs the CLI, which must fail with code, and returns the
// refusal as --json prints it, after checking that the text form says
// the same on stderr and nothing on stdout.
func (u *user) refused(code proto.Code, args ...string) proto.Error {
	t := u.w.t
	t.Helper()
	r := u.run("v0.2.0", append([]string{"--json"}, args...)...)
	if r.code != cli.ExitFailure || r.stderr != "" {
		t.Fatalf("sfx --json %s: exit %d, stderr %q; want exit %d and one JSON document\n%s", strings.Join(args, " "), r.code, r.stderr, cli.ExitFailure, r.stdout)
	}
	e := decode[map[string]proto.Error](t, r.stdout)["error"]
	if e.Code != code || e.Fix == "" {
		t.Fatalf("sfx %s refused with %+v, want code %s and a fix", strings.Join(args, " "), e, code)
	}
	text := u.run("v0.2.0", args...)
	if want := "sfx: " + e.Message + "\nfix: " + e.Fix + "\n"; text.code != cli.ExitFailure || text.stderr != want || text.stdout != "" {
		t.Fatalf("sfx %s: exit %d, stdout %q\nstderr %q\nwant   %q", strings.Join(args, " "), text.code, text.stdout, text.stderr, want)
	}
	return e
}

// keyFor records a public key for principal in the control store, as
// `starfixd control init` does, and returns its fingerprint. No command
// adds keys yet.
func (w *world) keyFor(principal string) string {
	t := w.t
	t.Helper()
	sum := sha256.Sum256([]byte(principal))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	k, err := w.ctl.AddKey(t.Context(), dana, principal, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))+" "+principal+"@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return k.Fingerprint
}

// TestAdminCommands runs every `sfx admin` access command as someone
// who may, through SSH and the daemon, with its text and its --json.
func TestAdminCommands(t *testing.T) {
	w := adminWorld(t, 0, 0)
	dana := w.newUser("dana", "")

	// Principals.
	if got := dana.ok("admin", "principal", "add", "alice"); got != "principal alice added, a member of project first\n" {
		t.Errorf("principal add: %q", got)
	}
	if got := decode[proto.PrincipalResult](t, dana.ok("admin", "principal", "add", "bob", "--json")); got !=
		(proto.PrincipalResult{ID: "bob", Rev: 1, State: "active", Project: project, ProjectName: "first"}) {
		t.Errorf("principal add --json: %+v", got)
	}
	if got := dana.ok("admin", "principal", "add", "ci-bot", "--no-project"); got != "principal ci-bot added, in no project\n" {
		t.Errorf("principal add --no-project: %q", got)
	}
	list := dana.ok("admin", "principal", "list")
	for _, want := range []string{"NAME", "ADDED BY", "alice   active  dana", "bob     active  dana", "ci-bot  active  dana"} {
		if !strings.Contains(list, want) {
			t.Errorf("principal list lacks %q:\n%s", want, list)
		}
	}
	ps := decode[proto.PrincipalsResult](t, dana.ok("admin", "principal", "list", "--json", "-n", "2"))
	if len(ps.Principals) != 2 || ps.Principals[0].Name != "alice" || ps.Principals[0].CreatedBy != "dana" || ps.More != 1 {
		t.Errorf("principal list --json -n 2: %+v", ps)
	}
	if got := dana.ok("admin", "principal", "list", "-n", "1", "--after", "alice"); !strings.Contains(got, "bob") ||
		!strings.Contains(got, "and 1 more; list them with --after bob\n") {
		t.Errorf("principal list -n 1 --after alice:\n%s", got)
	}
	if got := dana.ok("admin", "principal", "disable", "bob"); got != "bob: disabled\n" {
		t.Errorf("principal disable: %q", got)
	}
	if got := decode[proto.PrincipalResult](t, dana.ok("--json", "admin", "principal", "enable", "bob")); got != (proto.PrincipalResult{ID: "bob", Rev: 3, State: "active"}) {
		t.Errorf("principal enable --json: %+v", got)
	}

	// Members.
	if got := dana.ok("admin", "member", "add", "alice", "--role", "admin"); got != "alice is now admin in project first\n" {
		t.Errorf("member add --role admin, a change of role: %q", got)
	}
	if got := dana.ok("admin", "member", "add", "alice", "--role", "admin"); got != "alice is already admin in project first\n" {
		t.Errorf("member add again: %q", got)
	}
	set := decode[proto.MemberSetResult](t, dana.ok("admin", "member", "add", "ci-bot", "--project", "first", "--json"))
	if set.ID != "ci-bot" || set.Rev != 1 || set.Project != project || set.ProjectName != "first" || set.Role != "member" || set.Change != "added" ||
		set.Projects != 0 || set.Keys != nil {
		t.Errorf("member add --json: %+v", set)
	}
	members := dana.ok("admin", "member", "list")
	for _, want := range []string{"members of project first (" + project + ")\n", "PRINCIPAL  ROLE", "alice      admin   dana", "bob        member  dana", "ci-bot     member  dana"} {
		if !strings.Contains(members, want) {
			t.Errorf("member list lacks %q:\n%s", want, members)
		}
	}
	// A project admin manages their project from their own repository.
	alice := w.newUser("alice", "")
	ms := decode[proto.MembersResult](t, alice.ok("admin", "member", "list", "--json"))
	if ms.Project != project || ms.ProjectName != "first" || len(ms.Members) != 3 || ms.Members[0].Principal != "alice" || ms.Members[0].Role != "admin" {
		t.Errorf("member list --json by the project's admin: %+v", ms)
	}
	if got := alice.ok("admin", "principal", "add", "carol"); got != "principal carol added, a member of project first\n" {
		t.Errorf("principal add by a project admin: %q", got)
	}
	if got := alice.ok("admin", "member", "remove", "carol"); got != "carol removed from project first\n" {
		t.Errorf("member remove: %q", got)
	}
	if got := decode[proto.MemberRemoveResult](t, alice.ok("admin", "member", "remove", "ci-bot", "--json")); got !=
		(proto.MemberRemoveResult{ID: "ci-bot", Project: project, ProjectName: "first"}) {
		t.Errorf("member remove --json: %+v", got)
	}
	// The new member works at once.
	w.newUser("bob", "").ok("create", "bob's first issue")

	// Projects: a server admin sees every one with its database, anyone
	// else the active ones they belong to.
	plist := dana.ok("admin", "project", "list")
	if !strings.Contains(plist, "NAME   ID") || !strings.Contains(plist, "DATABASE") || !strings.Contains(plist, "first  "+project+"  sf      internal  active") {
		t.Errorf("project list for a server admin:\n%s", plist)
	}
	mine := decode[proto.ProjectsResult](t, alice.ok("admin", "project", "list", "--json"))
	if len(mine.Projects) != 1 || mine.Projects[0] != (proto.Project{ID: project, Name: "first", Prefix: "sf", Account: "internal", State: "active", Role: "admin"}) {
		t.Errorf("project list --json for a project admin: %+v", mine)
	}
	if got := alice.ok("admin", "project", "list"); !strings.Contains(got, "ROLE") || strings.Contains(got, "DATABASE") {
		t.Errorf("project list for a project admin shows no role, or a database:\n%s", got)
	}

	// Every change is on the control store's record, by who made it.
	evs, err := w.ctl.Events(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var by []string
	for _, e := range evs {
		if e.Op == store.OpMemberRemove || e.Op == store.OpPrincipalDisable {
			by = append(by, string(e.Op)+" by "+e.Actor.Principal)
		}
	}
	if want := []string{"principal.disable by dana", "member.remove by alice", "member.remove by alice"}; !slices.Equal(by, want) {
		t.Errorf("events = %v, want %v", by, want)
	}
}

// TestAdminForbidden runs the commands as people who may not: each
// reaches the user as forbidden, on stderr and in --json, with a fix
// that names who can.
func TestAdminForbidden(t *testing.T) {
	w := adminWorld(t, 1, 1)
	w.addProject(second, "second", "two")
	w.member("alice", project, store.RoleAdmin) // admin of the first
	w.member("erin", second, store.RoleAdmin)   // admin of the second
	w.member("bob", project, store.RoleMember)
	w.member("carol", project, store.RoleMember)
	w.member("carol", second, store.RoleMember)
	if _, err := w.ctl.AddPrincipal(t.Context(), dana, "frank", ""); err != nil { // in no project
		t.Fatal(err)
	}
	if _, err := w.ctl.AddPrincipal(t.Context(), dana, "dana", ""); err != nil {
		t.Fatal(err)
	}
	alice, bob, erin := w.newUser("alice", ""), w.newUser("bob", ""), w.newUserIn("erin", "", second)
	const (
		forServer  = "ask a server admin (admins: in starfixd's config) to run it"
		forProject = "ask one of the project's admins, or a server admin, to run it"
	)
	tests := []struct {
		name string
		u    *user
		args []string
		msg  string
		fix  string
	}{
		{"a member adds a project", bob, []string{"admin", "project", "add", "web"}, "bob may not add a project: it is for server admins", forServer},
		{"a member archives", bob, []string{"admin", "project", "archive", "first"}, "bob may not archive project first: it is for server admins", forServer},
		{"a member lists principals", bob, []string{"admin", "principal", "list"}, "bob may not list the principals: it is for server admins", forServer},
		{"a member adds a principal", bob, []string{"admin", "principal", "add", "hal"},
			"bob may not add principal hal in project " + project + ": it is for the project's admins and server admins", forProject},
		{"a member adds a member", bob, []string{"admin", "member", "add", "frank"},
			"bob may not set the role of frank in project " + project + ": it is for the project's admins and server admins", forProject},
		{"a member lists members", bob, []string{"admin", "member", "list"},
			"bob may not list the members of project " + project + ": it is for the project's admins and server admins", forProject},
		{"a member removes a member", bob, []string{"admin", "member", "remove", "carol"},
			"bob may not remove carol from project " + project + ": it is for the project's admins and server admins", forProject},
		{"a member disables", bob, []string{"admin", "principal", "disable", "carol"}, "bob may not disable carol: it is for project admins and server admins",
			"ask a server admin to run it, or a project admin who administers every project the principal belongs to"},

		{"a project admin adds a project", alice, []string{"admin", "project", "add", "web"}, "alice may not add a project: it is for server admins", forServer},
		{"a project admin activates", alice, []string{"admin", "project", "activate", "first"}, "alice may not activate project first: it is for server admins", forServer},
		{"a project admin adds to no project", alice, []string{"admin", "principal", "add", "hal", "--no-project"},
			"alice may not add principal hal: it is for server admins", forServer},
		{"an admin of another project adds a member", alice, []string{"admin", "member", "add", "bob", "--project", "second"},
			"alice may not set the role of bob in project second: it is for the project's admins and server admins", forProject},
		{"an admin of another project lists its members", erin, []string{"admin", "member", "list", "--project", project},
			"erin may not list the members of project " + project + ": it is for the project's admins and server admins", forProject},
		{"an admin of another project removes a member", erin, []string{"admin", "member", "remove", "bob", "--project", "first"},
			"erin may not remove bob from project first: it is for the project's admins and server admins", forProject},
		{"an admin of another project adds a principal there", erin, []string{"admin", "principal", "add", "hal", "--project", "first"},
			"erin may not add principal hal in project first: it is for the project's admins and server admins", forProject},
		{"a project admin outside reach disables", alice, []string{"admin", "principal", "disable", "carol"},
			"alice may not disable carol: carol belongs to a project alice does not administer, or to none",
			"ask a server admin to run it; a project admin may only while every project the principal belongs to is one they administer"},
		{"a project admin outside reach enables", erin, []string{"admin", "principal", "enable", "bob"},
			"erin may not enable bob: bob belongs to a project erin does not administer, or to none",
			"ask a server admin to run it; a project admin may only while every project the principal belongs to is one they administer"},
		{"a project admin disables a server admin", alice, []string{"admin", "principal", "disable", "dana"},
			"alice may not disable dana: dana is a server admin", "ask a server admin to run it"},
		{"a project admin places an unassigned principal", alice, []string{"admin", "member", "add", "frank"},
			"alice may not set the role of frank in project " + project + ": frank belongs to no project and alice did not create it",
			"ask a server admin to add frank to the project; a project admin adds only a principal that already belongs to a project, or one they created"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if e := tc.u.refused(proto.CodeForbidden, tc.args...); e.Message != tc.msg || e.Fix != tc.fix {
				t.Errorf("sfx %s\n got %q\n     fix %q\nwant %q\n     fix %q", strings.Join(tc.args, " "), e.Message, e.Fix, tc.msg, tc.fix)
			}
		})
	}

	// What a server admin is refused is not forbidden: it says what is
	// wrong and what to do.
	d := w.newUser("dana", "")
	for _, tc := range []struct {
		name string
		args []string
		code proto.Code
		msg  string
		fix  string
	}{
		{"an unknown project", []string{"admin", "member", "list", "--project", "nowhere"}, proto.CodeNotFound,
			`project "nowhere" not found`, "list the projects with `sfx admin project list`"},
		{"an unknown principal", []string{"admin", "member", "add", "nobody"}, proto.CodeNotFound,
			"principal nobody not found", "add the principal first with `sfx admin principal add NAME`"},
		{"a name that is taken", []string{"admin", "principal", "add", "bob"}, proto.CodeExists,
			`a principal named "bob" already exists`, "choose another name, or if bob is the person you mean, add them with `sfx admin member add bob`"},
		{"a project name that is taken", []string{"admin", "project", "add", "second"}, proto.CodeExists,
			`a project named "second" already exists`, "choose another name; `sfx admin project list` shows the projects you can see"},
		{"not a member", []string{"admin", "member", "remove", "erin"}, proto.CodeNotFound,
			"member erin of project " + project + " not found", "see who is a member with `sfx admin member list`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if e := d.refused(tc.code, tc.args...); e.Message != tc.msg || e.Fix != tc.fix {
				t.Errorf("sfx %s\n got %q\n     fix %q\nwant %q\n     fix %q", strings.Join(tc.args, " "), e.Message, e.Fix, tc.msg, tc.fix)
			}
		})
	}
}

// uuidLine finds the project id in what `sfx admin project add` prints.
var uuidLine = regexp.MustCompile(`(?m)^project: ([0-9a-f-]{36})$`)

// TestAdminProjectAdd creates projects from anywhere, with no shell on
// the server: from a block of three database names of which two were
// granted.
func TestAdminProjectAdd(t *testing.T) {
	w := adminWorld(t, 3, 2)
	w.member("alice", project, store.RoleMember)
	d, alice := w.newUser("dana", ""), w.newUser("alice", "")

	out := d.ok("admin", "project", "add", "web", "--prefix", "web", "--account", "acme-2026")
	m := uuidLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("project add printed no project id:\n%s", out)
	}
	id := m[1]
	head, config, _ := strings.Cut(out, "\n\n")
	wantHead := fmt.Sprintf("project web (%s) created in database %s: active\ncommit this as .starfix.yaml in the project's repository:", id, w.block.Name(1))
	wantConfig := fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: %d\n  host_key: %s\n", id, w.addr.Port, w.hostFpr)
	if head != wantHead || config != wantConfig {
		t.Fatalf("project add printed\n%s\nwant\n%s\n\n%s", out, wantHead, wantConfig)
	}
	if strings.Contains(out, "id_ed25519") || strings.Contains(out, "key:") && !strings.Contains(out, "host_key:") {
		t.Errorf("project add printed this machine's key path:\n%s", out)
	}

	// The printed config connects, and the project is usable at once by
	// the admin who made it, who is a member of nothing.
	inWeb := d.in(config)
	issue := decode[proto.WriteResult](t, inWeb.ok("create", "First in web", "--json"))
	if !strings.HasPrefix(issue.ID, "web-") {
		t.Errorf("an issue of the new project has id %q, want the prefix web-", issue.ID)
	}
	// By a member, once added: not before.
	aliceWeb := alice.in(config)
	if e := aliceWeb.refused(proto.CodeForbidden, "list"); !strings.Contains(e.Fix, "`sfx admin member add alice`") {
		t.Errorf("alice in the new project before she is a member: %+v", e)
	}
	if got := inWeb.ok("admin", "member", "add", "alice"); got != "alice added to project web as member\n"+
		"alice belongs to 2 projects; no key is recorded for alice\n" {
		t.Errorf("member add in the new project, from its repository: %q", got)
	}
	if l := decode[proto.ListResult](t, aliceWeb.ok("list", "--json")); len(l.Issues) != 1 || l.Issues[0].ID != issue.ID {
		t.Errorf("alice, now a member, lists %+v; want %s", l.Issues, issue.ID)
	}
	// It is another project: the first has none of it.
	if l := decode[proto.ListResult](t, alice.ok("list", "--json")); len(l.Issues) != 0 {
		t.Errorf("the first project lists %+v, want nothing of the new one", l.Issues)
	}

	// --json is one document, with the config in it.
	api := decode[struct {
		proto.ProjectAddResult
		Config string `json:"starfix_yaml"`
	}](t, d.ok("--json", "admin", "project", "add", "api"))
	if api.Name != "api" || api.State != "active" || api.Database != w.block.Name(2) || api.Resumed ||
		api.Config != fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: %d\n  host_key: %s\n", api.ID, w.addr.Port, w.hostFpr) {
		t.Errorf("project add --json: %+v", api)
	}
	d.in(api.Config).ok("create", "First in api")

	// An active project is refused as existing.
	if e := d.refused(proto.CodeExists, "admin", "project", "add", "web"); e.Message != `a project named "web" already exists` {
		t.Errorf("project add of an active project: %+v", e)
	}

	// The third name was never granted. The refusal gives the operator
	// the SQL, and leaves the project to be resumed.
	grant := fmt.Sprintf("GRANT ALL ON %s.* TO 'starfix'@'localhost';", w.block.Name(3))
	e := d.refused(proto.CodeUnavailable, "admin", "project", "add", "docs")
	if e.Message != "starfixd's Dolt account was never granted database "+w.block.Name(3)+", so project docs cannot use it" ||
		e.Fix != "ask the server's operator to run this as Dolt's root, then run `sfx admin project add docs` again: "+grant+" (docs/server.md, Serving many projects)" {
		t.Fatalf("project add of a name not granted:\n%q\nfix %q", e.Message, e.Fix)
	}
	if p, ok := w.ctl.ProjectNamed("docs"); !ok || p.State != store.ProjectCreating {
		t.Fatalf("project docs after the refusal = %+v, %t; want it left creating", p, ok)
	}
	// While it holds the last name, the block is exhausted.
	e = d.refused(proto.CodeInvalid, "admin", "project", "add", "more")
	if e.Message != fmt.Sprintf("every database name for new projects is in use (%s to %s)", w.block.Name(1), w.block.Name(3)) ||
		!strings.Contains(e.Fix, "grant starfixd's Dolt account more names, from "+w.block.Name(4)+" on, and raise count under project_dbs:") {
		t.Errorf("project add past the block:\n%q\nfix %q", e.Message, e.Fix)
	}
	// The operator runs the SQL; the same command then finishes.
	if err := dolt.Grant(t.Context(), w.block.Name(3)); err != nil {
		t.Fatal(err)
	}
	out = d.ok("admin", "project", "add", "docs")
	docs, _ := w.ctl.ProjectNamed("docs")
	if !strings.HasPrefix(out, fmt.Sprintf("project docs (%s) created, finishing an earlier attempt, in database %s: active\n", docs.ID, w.block.Name(3))) {
		t.Errorf("project add resumed printed:\n%s", out)
	}
	_, config, _ = strings.Cut(out, "\n\n")
	d.in(config).ok("create", "First in docs")

	plist := decode[proto.ProjectsResult](t, d.ok("admin", "project", "list", "--json"))
	var names []string
	for _, p := range plist.Projects {
		names = append(names, p.Name+"="+p.State+"@"+p.Database)
	}
	if want := []string{"api=active@" + w.block.Name(2), "docs=active@" + w.block.Name(3), "first=active@" + plist.Projects[2].Database,
		"web=active@" + w.block.Name(1)}; !slices.Equal(names, want) {
		t.Errorf("project list = %v, want %v", names, want)
	}
}

// A server with no block of names says how to get one.
func TestAdminProjectAddNeedsBlock(t *testing.T) {
	w := adminWorld(t, 0, 0)
	e := w.newUser("dana", "").refused(proto.CodeInvalid, "admin", "project", "add", "web")
	if e.Message != "the server has no database names for new projects" || !strings.Contains(e.Fix, "set project_dbs: in its config (docs/server.md, Serving many projects)") {
		t.Errorf("project add with no block: %+v", e)
	}
}

// watcher is `sfx watch` running as a user: a connection that stays
// open until the server closes it.
type watcher struct {
	code   chan int
	errb   *lines
	cancel context.CancelFunc
}

// watch starts `sfx watch` as u and waits until it is connected.
func (u *user) watch() *watcher {
	t := u.w.t
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	wt := &watcher{code: make(chan int, 1), errb: &lines{}, cancel: cancel}
	var out lines
	go func() { wt.code <- u.runTo(ctx, &out, wt.errb, "v0.2.0", "watch") }()
	waitFor(t, "sfx watch to start", func() bool { return strings.Contains(wt.errb.String(), "watching your inbox") })
	return wt
}

// closedByServer waits for the watch to end because the server closed
// its connection, and fails the test if it does not.
func (wt *watcher) closedByServer(t *testing.T, who string) {
	t.Helper()
	waitFor(t, who+"'s connection to be closed", func() bool { return len(wt.code) == 1 })
	if c := <-wt.code; c != cli.ExitFailure {
		t.Errorf("%s's watch exited %d, want %d: the server closed its connection\n%s", who, c, cli.ExitFailure, wt.errb)
	}
}

// stillOpen stops a watch that must still be connected.
func (wt *watcher) stillOpen(t *testing.T, who string) {
	t.Helper()
	if len(wt.code) != 0 {
		t.Errorf("%s's connection was closed:\n%s", who, wt.errb)
		return
	}
	wt.cancel()
	if c := <-wt.code; c != cli.ExitOK {
		t.Errorf("%s's watch, interrupted, exited %d; want %d, its connection still open\n%s", who, c, cli.ExitOK, wt.errb)
	}
}

// TestAdminEndsAccess pins what happens to connections that are open
// when access ends: a removed member's, a disabled principal's and an
// archived project's are closed, and the next is refused; a demotion
// closes nothing.
func TestAdminEndsAccess(t *testing.T) {
	w := adminWorld(t, 0, 0)
	w.addProject(second, "second", "two")
	for _, name := range []string{"alice", "bob", "carol"} {
		w.member(name, project, store.RoleMember)
	}
	w.member("alice", project, store.RoleAdmin)
	w.member("carol", second, store.RoleMember)
	d := w.newUser("dana", "")
	alice, bob, carol, carol2 := w.newUser("alice", ""), w.newUser("bob", ""), w.newUser("carol", ""), w.newUserIn("carol", "", second)

	// A demotion: alice stays connected, and is no admin any more.
	aw := alice.watch()
	if got := d.ok("admin", "member", "add", "alice", "--role", "member"); got != "alice is now member in project first\n" {
		t.Errorf("member add --role member, a demotion: %q", got)
	}
	alice.refused(proto.CodeForbidden, "admin", "member", "list")
	aw.stillOpen(t, "alice")

	// A membership removed: bob's connection to the project closes; the
	// admin's request was answered.
	bw, cw, cw2 := bob.watch(), carol.watch(), carol2.watch()
	if got := d.ok("admin", "member", "remove", "bob"); got != "bob removed from project first\n" {
		t.Errorf("member remove: %q", got)
	}
	bw.closedByServer(t, "bob")
	if e := bob.refused(proto.CodeForbidden, "list"); !strings.Contains(e.Message, "bob is not a member of project") {
		t.Errorf("bob's next command: %+v", e)
	}

	// A principal disabled: carol's connections to both projects close.
	if got := d.ok("admin", "principal", "disable", "carol"); got != "carol: disabled\n" {
		t.Errorf("principal disable: %q", got)
	}
	cw.closedByServer(t, "carol")
	cw2.closedByServer(t, "carol, in the second project,")
	carol.refused(proto.CodeForbidden, "list")
	d.ok("admin", "principal", "enable", "carol")
	carol.ok("list")

	// A project archived: everyone in it is disconnected, and it is not
	// served until it is activated again, with what it held.
	id := strings.TrimSpace(carol2.ok("create", "Kept through the archive"))
	cw2, aw = carol2.watch(), alice.watch()
	if got := d.ok("admin", "project", "archive", "second"); got != "project second ("+second+"): archived\n" {
		t.Errorf("project archive: %q", got)
	}
	cw2.closedByServer(t, "carol, in the archived project,")
	aw.stillOpen(t, "alice, in another project,")
	if e := carol2.refused(proto.CodeNotFound, "list"); !strings.Contains(e.Message, "is not served by this server") {
		t.Errorf("a command in the archived project: %+v", e)
	}
	if got := decode[proto.ProjectResult](t, d.ok("admin", "project", "activate", second, "--json")); got.ID != second || got.State != "active" || got.Name != "second" {
		t.Errorf("project activate --json: %+v", got)
	}
	if out := carol2.ok("list"); !strings.Contains(out, id) {
		t.Errorf("the reactivated project lists no %s:\n%s", id, out)
	}
}

// TestAdminPrompts pins what an admin is told beyond what they asked:
// that a removal put someone within other admins' reach, and which keys
// come with someone added from another project.
func TestAdminPrompts(t *testing.T) {
	w := adminWorld(t, 0, 0)
	w.addProject(second, "second", "two")
	w.member("carol", project, store.RoleMember)
	w.member("carol", second, store.RoleMember)
	w.member("erin", second, store.RoleAdmin)
	fp := w.keyFor("carol")
	d := w.newUser("dana", "")

	r := d.run("v0.2.0", "admin", "member", "remove", "carol")
	warning := "every project carol still belongs to is administered by a project admin who could not manage carol before, and now can: " +
		"disable or enable carol, and manage its keys"
	if r.code != cli.ExitOK || r.stdout != "carol removed from project first\n" || r.stderr != "sfx: warning: "+warning+"\n" {
		t.Errorf("member remove that widens another admin's reach: exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	// And it is so: erin, who could not, now disables carol.
	erin := w.newUserIn("erin", "", second)
	erin.ok("admin", "principal", "disable", "carol")
	erin.ok("admin", "principal", "enable", "carol")

	// Back in a second project, her keys come with her.
	if got := d.ok("admin", "member", "add", "carol"); got != "carol added to project first as member\n"+
		"carol belongs to 2 projects, and each key recorded for carol now reaches this one too:\n  "+fp+"\n" {
		t.Errorf("member add of someone from another project: %q", got)
	}
	// In --json the warning and the keys are in the one document.
	rm := decode[proto.MemberRemoveResult](t, d.ok("--json", "admin", "member", "remove", "carol"))
	if rm.Warning != warning {
		t.Errorf("member remove --json: %+v", rm)
	}
	set := decode[proto.MemberSetResult](t, d.ok("--json", "admin", "member", "add", "carol"))
	if set.Projects != 2 || !slices.Equal(set.Keys, []string{fp}) {
		t.Errorf("member add --json: %+v, want 2 projects and the key %s", set, fp)
	}
	// A project admin who does not administer her other project is not
	// shown them.
	w.member("alice", project, store.RoleAdmin)
	d.ok("admin", "member", "remove", "carol")
	if got := w.newUser("alice", "").ok("admin", "member", "add", "carol"); got != "carol added to project first as member\n" {
		t.Errorf("member add by an admin without reach over her: %q", got)
	}
}

// A server of one project refuses the access commands, and says how a
// server opts in.
func TestAdminOnAServerOfOneProject(t *testing.T) {
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	d := w.newUser("dana", "")
	for _, args := range [][]string{
		{"admin", "project", "list"}, {"admin", "project", "add", "web"}, {"admin", "principal", "add", "bob"},
		{"admin", "member", "add", "bob"}, {"admin", "member", "list"}, {"admin", "member", "remove", "bob"},
	} {
		e := d.refused(proto.CodeInvalid, args...)
		if e.Message != "this server holds one project, so it has no projects, principals or members to manage" ||
			!strings.Contains(e.Fix, "setting control_db (docs/server.md, Serving many projects, Opt in)") {
			t.Errorf("sfx %s on a server of one project: %+v", strings.Join(args, " "), e)
		}
	}
	// The commands it had still work.
	d.ok("admin", "prices")
}

// Control writes have their own rate limit, per principal: past it the
// command is refused with busy and a wait, and says which settings.
func TestAdminControlWriteRate(t *testing.T) {
	w := newWorld(t, daemonOpts{many: true, admins: []string{"dana"}, limits: server.Limits{ControlWriteBurst: 2, ControlWriteRate: 0.001}})
	d := w.newUser("dana", "")
	d.ok("admin", "principal", "add", "alice")
	d.ok("admin", "principal", "add", "bob")
	e := d.refused(proto.CodeBusy, "admin", "principal", "add", "carol")
	if e.Message != "dana is over the limit on access changes (0.001 a second, bursts of 2)" || !strings.HasPrefix(e.Fix, "wait ") ||
		!strings.Contains(e.Fix, "raise control_write_rate and control_write_burst under limits: in starfixd's config") {
		t.Errorf("a third control write: %+v", e)
	}
	// Reads, and the project's own writes, go on.
	d.ok("admin", "member", "list")
	d.ok("create", "an issue")
	w.newUser("alice", "").ok("create", "another")
}

// A server of the last release, v0.4.x, which accepts protocols 2 to 4,
// refuses this client at the handshake, with exit code 3, before an
// admin or a key command could fail as an unknown operation.
func TestAdminOldServer(t *testing.T) {
	w := newWorld(t, daemonOpts{protoMin: 2, protoMax: 4})
	dana := w.newUser("dana", "")
	if r := dana.run("v0.2.0", "admin", "key", "list", "dana"); r.code != cli.ExitVersion {
		t.Errorf("a key command against an older server: exit %d, stderr %q; want exit %d", r.code, r.stderr, cli.ExitVersion)
	}
	r := dana.run("v0.2.0", "admin", "project", "list")
	want := fmt.Sprintf("sfx: starfix speaks protocol %d; the server (v0.2.0) is older and accepts protocols 2 to 4\n", proto.Proto) +
		"fix: ask the server admin to run `starfixd upgrade`, or install the starfix release that matches the server\n"
	if r.code != cli.ExitVersion || r.stderr != want {
		t.Errorf("exit %d, stderr %q\nwant exit %d, %q", r.code, r.stderr, cli.ExitVersion, want)
	}
}

// An agent has no admin tool, and one whose principal is not a member is
// told to ask a person, never to run the admin's command.
func TestMCPNotAMember(t *testing.T) {
	w := adminWorld(t, 0, 0)
	ag := w.newUser("mallory", "").mcp("v0.2.0")
	tools, err := ag.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 24 {
		t.Errorf("MCP has %d tools, want the 24 it had: access is managed with sfx admin only", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		if strings.HasPrefix(tool.Name, "admin") || slices.Contains([]string{"project", "projects", "principal", "principals", "member", "members", "key", "keys"}, tool.Name) {
			t.Errorf("MCP has a tool named %q; access is managed with sfx admin only", tool.Name)
		}
	}
	out, isErr := ag.call("ready", nil)
	if !isErr || !strings.HasPrefix(out, "forbidden: mallory is not a member of project") ||
		!strings.Contains(out, "\nfix: you have no access to this project: stop and tell the user, who must ask one of its admins; never run an admin command yourself, quoting the server: \"") {
		t.Errorf("ready as a principal who is no member: error %t\n%s", isErr, out)
	}
	// Removed while connected, an agent is told the same at its next call.
	w.member("bob", project, store.RoleMember)
	bob := w.newUser("bob", "").mcp("v0.2.0")
	bob.ok("ready", nil)
	w.newUser("dana", "").ok("admin", "member", "remove", "bob")
	waitFor(t, "bob's agent to be refused", func() bool {
		out, isErr = bob.call("ready", nil)
		return isErr && strings.HasPrefix(out, "forbidden: bob is not a member of project")
	})
	if !strings.Contains(out, "never run an admin command yourself") {
		t.Errorf("ready after bob was removed:\n%s", out)
	}
}
