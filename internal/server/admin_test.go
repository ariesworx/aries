package server

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/ssh"
)

// carol is one more person for the admin tests.
var carol = store.Actor{Principal: "carol", Session: "s-c", Machine: "laptop-c"}

// block gives m a block of n database names for new projects, of which
// the first granted are granted to the tests' account, as an operator
// grants names at setup.
func (m *multi) block(t *testing.T, n, granted int) ProjectDBs {
	t.Helper()
	cfg, err := mysql.ParseDSN(m.dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := ProjectDBs{Prefix: cfg.DBName + "_p", Count: n}
	for i := 1; i <= granted; i++ {
		if err := doltOf(t).Grant(t.Context(), b.Name(i)); err != nil {
			t.Fatal(err)
		}
	}
	m.cfg.ProjectDBs = b
	return b
}

// connections returns how many connections past the handshake principal
// has to project.
func (s *Server) connections(principal, project string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, o := range s.conns {
		if o == (owner{principal: principal, project: project}) {
			n++
		}
	}
	return n
}

// keyLine returns a public key in authorized_keys form, commented for
// its owner. The same owner gives the same key.
func keyLine(t *testing.T, owner string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(owner))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + owner + "@example.com"
}

// dropped reports whether the server closed c: the next read ends
// without a frame.
func (c *conn) dropped() bool {
	c.t.Helper()
	_ = c.nc.SetReadDeadline(time.Now().Add(patience))
	f, err := c.dec.Decode()
	if err == nil {
		c.t.Errorf("read a %q frame from a connection that should be closed", f.T)
		return false
	}
	return !strings.Contains(err.Error(), "timeout")
}

// alive reports whether c still answers a request.
func (c *conn) alive() bool {
	c.t.Helper()
	_, perr := ask[proto.ListResult](c, proto.OpList, proto.ListArgs{})
	return perr == nil
}

// adminWorld is a server of two projects:
//
//	dana        server admin
//	alice       admin of a
//	erin        admin of b
//	bob         member of a
//	carol       member of a and of b
//	frank       in no project, added by dana
//
// with one key recorded for carol.
func adminWorld(t *testing.T, lim Limits) *multi {
	t.Helper()
	m := newMulti(t, lim, nil)
	m.project(t, projA, "alice=admin", "bob", "carol")
	m.project(t, projB, "erin=admin", "carol")
	if _, err := m.ctl.AddPrincipal(t.Context(), dana, "frank", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.AddKey(t.Context(), dana, "carol", keyLine(t, "carol")); err != nil {
		t.Fatal(err)
	}
	// These writes went behind the server, as an operator's do; a tick
	// brings them to the keys file.
	m.refresh(t.Context())
	return m
}

// adminRequests lists every admin op with arguments that a server admin's
// request would succeed with in adminWorld, apart from project.add,
// which needs a block of names, and principal.whois, which needs a
// server that has made stand-ins (TestWhois).
func adminRequests() []struct {
	op   string
	args any
} {
	return []struct {
		op   string
		args any
	}{
		{proto.OpProjectAdd, proto.ProjectAddArgs{Name: "new"}},
		{proto.OpProjects, proto.AdminListArgs{}},
		{proto.OpProjectArchive, proto.ProjectArgs{Project: projB}},
		{proto.OpProjectActivate, proto.ProjectArgs{Project: projB}},
		{proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal"}},
		{proto.OpPrincipals, proto.AdminListArgs{}},
		{proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "bob"}},
		{proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "bob"}},
		{proto.OpPrincipalWhois, proto.PrincipalWhoisArgs{StandIn: "p_0123456789abcdef"}},
		{proto.OpMemberSet, proto.MemberSetArgs{Name: "frank"}},
		{proto.OpMembers, proto.MembersArgs{}},
		{proto.OpMemberRemove, proto.MemberArgs{Name: "bob"}},
		{proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOzBtYcn8/ErMZSIGp7LneCyjOeyByMNjpMP4bznXiVs"}},
		{proto.OpKeys, proto.KeysArgs{Name: "bob"}},
		{proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: "SHA256:pOFYONeRL07H3T9tf0FhwK1slkJs5kvkcnOnf+vGgXA"}},
		{proto.OpStatus, proto.StatusArgs{}},
	}
}

// A server of one project refuses every admin op, on a connection and
// through Dispatch, and says how a server opts in.
func TestAdminNeedsManyProjects(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	c := dialPipe(t, s, dana)
	reqs := adminRequests()
	if len(reqs) != len(adminHandlers) {
		t.Fatalf("adminRequests lists %d ops and the server handles %d: keep them in step", len(reqs), len(adminHandlers))
	}
	for _, r := range reqs {
		if !adminOp(r.op) {
			t.Errorf("adminOp(%s) = false, so the request loop would not route it", r.op)
		}
		for name, perr := range map[string]*proto.Error{
			"connection": func() *proto.Error { _, e := ask[json.RawMessage](c, r.op, r.args); return e }(),
			"Dispatch":   func() *proto.Error { _, e := call[json.RawMessage](t, s, dana, r.op, r.args); return e }(),
		} {
			if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "this server holds one project") ||
				!strings.Contains(perr.Fix, "control_db") || !strings.Contains(perr.Fix, "docs/server.md, Serving many projects, Opt in") {
				t.Errorf("%s on a server of one project, by %s = %+v; want invalid, with a fix naming control_db and the opt-in section", r.op, name, perr)
			}
		}
	}
}

// TestAdminRoles runs the admin ops as each kind of caller. A refusal is
// forbidden, says which rule applied and names who can do it.
func TestAdminRoles(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	// A server admin with a row, as `control init` gives one.
	if _, err := m.ctl.AddPrincipal(t.Context(), dana, "dana", ""); err != nil {
		t.Fatal(err)
	}
	conns := map[string]*conn{
		"bob":   mustDialTo(t, m.Server, bob, projA),   // a member
		"alice": mustDialTo(t, m.Server, alice, projA), // admin of a
		"erin":  mustDialTo(t, m.Server, erin, projB),  // admin of b
	}
	const (
		forServer   = "ask a server admin (admins: in starfixd's config) to run it"
		forProject  = "ask one of the project's admins, or a server admin, to run it"
		forReach    = "check the name, or ask a server admin to run it; a project admin may only while every project the principal belongs to is one they administer"
		forAnyAdmin = "ask a server admin to run it, or a project admin who administers every project the principal belongs to"
	)
	tests := []struct {
		name string
		who  string
		op   string
		args any
		msg  string // part of the refusal's message
		fix  string // the whole fix
	}{
		{"member adds a project", "bob", proto.OpProjectAdd, proto.ProjectAddArgs{Name: "new"}, "bob may not add a project: it is for server admins", forServer},
		{"member archives", "bob", proto.OpProjectArchive, proto.ProjectArgs{Project: projA}, "bob may not archive project " + projA, forServer},
		{"member activates", "bob", proto.OpProjectActivate, proto.ProjectArgs{Project: projA}, "bob may not activate project " + projA, forServer},
		{"member adds a principal", "bob", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal"},
			"bob may not add principal hal in project " + projA + ": it is for the project's admins and server admins", forProject},
		{"member lists principals", "bob", proto.OpPrincipals, proto.AdminListArgs{}, "bob may not list the principals", forServer},
		{"member disables", "bob", proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "carol"},
			"bob may not disable carol: it is for project admins and server admins", forAnyAdmin},
		{"member enables", "bob", proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "carol"}, "bob may not enable carol", forAnyAdmin},
		{"member adds a member", "bob", proto.OpMemberSet, proto.MemberSetArgs{Name: "frank"}, "bob may not set the role of frank in project " + projA, forProject},
		{"member makes themselves admin", "bob", proto.OpMemberSet, proto.MemberSetArgs{Name: "bob", Role: "admin"}, "bob may not set the role of bob", forProject},
		{"member lists members", "bob", proto.OpMembers, proto.MembersArgs{}, "bob may not list the members of project " + projA, forProject},
		{"member removes a member", "bob", proto.OpMemberRemove, proto.MemberArgs{Name: "carol"}, "bob may not remove carol from project " + projA, forProject},

		{"project admin adds a project", "alice", proto.OpProjectAdd, proto.ProjectAddArgs{Name: "new"}, "alice may not add a project", forServer},
		{"project admin archives their own", "alice", proto.OpProjectArchive, proto.ProjectArgs{Project: "proj-a"}, "alice may not archive project proj-a", forServer},
		{"project admin lists principals", "alice", proto.OpPrincipals, proto.AdminListArgs{}, "alice may not list the principals", forServer},
		{"project admin adds a principal to no project", "alice", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal", Unassigned: true},
			"alice may not add principal hal: it is for server admins", forServer},
		{"admin of another project adds a member", "alice", proto.OpMemberSet, proto.MemberSetArgs{Name: "bob", Project: projB},
			"alice may not set the role of bob in project " + projB, forProject},
		{"admin of another project, by its name", "alice", proto.OpMemberSet, proto.MemberSetArgs{Name: "bob", Project: "proj-b"},
			"alice may not set the role of bob in project proj-b", forProject},
		// The same answer for a project that does not exist, so it does
		// not say which names are taken.
		{"admin of another project, a name no project has", "alice", proto.OpMemberSet, proto.MemberSetArgs{Name: "bob", Project: "proj-z"},
			"alice may not set the role of bob in project proj-z", forProject},
		{"admin of another project lists its members", "alice", proto.OpMembers, proto.MembersArgs{Project: projB},
			"alice may not list the members of project " + projB, forProject},
		{"admin of another project removes a member", "erin", proto.OpMemberRemove, proto.MemberArgs{Name: "bob", Project: projA},
			"erin may not remove bob from project " + projA, forProject},
		{"admin of another project adds a principal there", "erin", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal", Project: projA},
			"erin may not add principal hal in project " + projA, forProject},
		{"project admin outside reach disables", "alice", proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "carol"},
			"alice may not disable carol: carol does not exist, or belongs to a project alice does not administer, or to none", forReach},
		{"project admin outside reach enables", "erin", proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "bob"},
			"erin may not enable bob: bob does not exist, or belongs to a project erin does not administer, or to none", forReach},
		{"project admin disables a server admin", "alice", proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "dana"},
			"alice may not disable dana: dana is a server admin", "ask a server admin to run it"},
		{"project admin places someone else's unassigned principal", "alice", proto.OpMemberSet, proto.MemberSetArgs{Name: "frank"},
			"alice may not set the role of frank in project " + projA + ": frank belongs to no project and alice did not create it",
			"ask a server admin to add frank to the project; a project admin adds only a principal that already belongs to a project, or one they created"},
		{"project admin takes a server admin's name", "alice", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "dana"},
			"alice may not add principal dana in project " + projA + ": dana is a server admin", "ask a server admin to run it"},
	}
	seq := func() int64 {
		t.Helper()
		evs, err := m.ctl.Events(t.Context(), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return int64(len(evs))
	}
	before := seq()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := ask[json.RawMessage](conns[tc.who], tc.op, tc.args)
			if perr == nil || perr.Code != proto.CodeForbidden || !strings.Contains(perr.Message, tc.msg) || perr.Fix == "" ||
				(tc.fix != "" && perr.Fix != tc.fix) {
				t.Errorf("%s by %s = %+v\nwant forbidden, a message containing %q and the fix %q", tc.op, tc.who, perr, tc.msg, tc.fix)
			}
		})
	}
	if after := seq(); after != before {
		t.Errorf("the refusals recorded %d events, want none", after-before)
	}

	// What each may do.
	t.Run("project admin in their project", func(t *testing.T) {
		a := conns["alice"]
		hal := mustAsk[proto.PrincipalResult](a, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal"})
		if hal != (proto.PrincipalResult{ID: "hal", Rev: 1, State: "active", Project: projA, ProjectName: "proj-a"}) {
			t.Errorf("principal.add hal = %+v", hal)
		}
		// carol already belongs to a project, so an admin of b could add
		// her; alice changes a role in a, and reaches bob, who is in a only.
		set := mustAsk[proto.MemberSetResult](a, proto.OpMemberSet, proto.MemberSetArgs{Name: "hal", Role: "admin"})
		if set.Change != "changed" || set.Role != "admin" || set.Rev != 2 || set.ID != "hal" || set.Project != projA {
			t.Errorf("member.set hal admin = %+v, want the role changed at rev 2", set)
		}
		if again := mustAsk[proto.MemberSetResult](a, proto.OpMemberSet, proto.MemberSetArgs{Name: "hal", Role: "admin"}); again.Change != "unchanged" {
			t.Errorf("member.set again = %+v, want unchanged", again)
		}
		ms := mustAsk[proto.MembersResult](a, proto.OpMembers, proto.MembersArgs{})
		var names []string
		for _, mem := range ms.Members {
			names = append(names, mem.Principal+"="+mem.Role)
		}
		if want := []string{"alice=admin", "bob=member", "carol=member", "hal=admin"}; !slices.Equal(names, want) || ms.ProjectName != "proj-a" {
			t.Errorf("members of a = %v (%s), want %v", names, ms.ProjectName, want)
		}
		for _, op := range []string{proto.OpPrincipalDisable, proto.OpPrincipalEnable} {
			if _, perr := ask[proto.PrincipalResult](a, op, proto.PrincipalArgs{Name: "bob"}); perr != nil {
				t.Errorf("%s bob by alice, who administers bob's only project: %v", op, perr)
			}
		}
		rm := mustAsk[proto.MemberRemoveResult](a, proto.OpMemberRemove, proto.MemberArgs{Name: "hal"})
		if rm.ID != "hal" || rm.Project != projA || rm.Warning != "" {
			t.Errorf("member.remove hal = %+v", rm)
		}
	})
	t.Run("lists by role", func(t *testing.T) {
		// bob's first connection was closed when alice disabled him.
		conns["bob"] = mustDialTo(t, m.Server, bob, projA)
		for _, tc := range []struct {
			who  string
			want []string
		}{{"bob", []string{"proj-a=member"}}, {"alice", []string{"proj-a=admin"}}} {
			ps := mustAsk[proto.ProjectsResult](conns[tc.who], proto.OpProjects, proto.AdminListArgs{})
			var got []string
			for _, p := range ps.Projects {
				got = append(got, p.Name+"="+p.Role)
				if p.Database != "" || p.CreatedBy != "" {
					t.Errorf("projects for %s shows the database or creator of %s: %+v", tc.who, p.Name, p)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("projects for %s = %v, want %v", tc.who, got, tc.want)
			}
		}
	})
	t.Run("server admin", func(t *testing.T) {
		d := mustDialTo(t, m.Server, dana, projA)
		ps := mustAsk[proto.ProjectsResult](d, proto.OpProjects, proto.AdminListArgs{})
		if len(ps.Projects) != 2 || ps.Projects[0].Name != "proj-a" || ps.Projects[0].Database == "" || ps.Projects[0].CreatedBy != "dana" {
			t.Errorf("projects for dana = %+v, want both with their databases", ps.Projects)
		}
		all := mustAsk[proto.PrincipalsResult](d, proto.OpPrincipals, proto.AdminListArgs{})
		var names []string
		for _, p := range all.Principals {
			names = append(names, p.Name)
		}
		if want := []string{"alice", "bob", "carol", "dana", "erin", "frank", "hal"}; !slices.Equal(names, want) {
			t.Errorf("principals = %v, want %v", names, want)
		}
		mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "frank", Project: "proj-b"})
		mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "carol"})
		mustAsk[proto.PrincipalResult](d, proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "carol"})
		none := mustAsk[proto.PrincipalResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "ivy", Unassigned: true})
		if none.Project != "" || none.ID != "ivy" {
			t.Errorf("principal.add ivy to no project = %+v", none)
		}
		if of, err := m.ctl.ProjectsOf(t.Context(), "ivy"); err != nil || len(of) != 0 {
			t.Errorf("ivy's projects = %v, %v; want none", of, err)
		}
	})
}

// What a server admin is told when a request names something that is
// not there, or is taken, or is not valid.
func TestAdminRefusals(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{Control: store.ControlLimits{Members: 3}})
	d := mustDialTo(t, m.Server, dana, projA)
	tests := []struct {
		name string
		op   string
		args any
		code proto.Code
		msg  string
		fix  string
	}{
		{"unknown project by name", proto.OpMembers, proto.MembersArgs{Project: "proj-z"}, proto.CodeNotFound,
			`project "proj-z" not found`, "list the projects with `sfx admin project list`"},
		{"unknown project by id", proto.OpProjectArchive, proto.ProjectArgs{Project: projC}, proto.CodeNotFound,
			`project "` + projC + `" not found`, "list the projects with `sfx admin project list`"},
		{"archive names no project", proto.OpProjectArchive, proto.ProjectArgs{}, proto.CodeInvalid,
			"no project named", "name the project: `sfx admin project archive PROJECT`"},
		{"unknown principal", proto.OpMemberSet, proto.MemberSetArgs{Name: "nobody"}, proto.CodeNotFound,
			"principal nobody not found", "add the principal first with `sfx admin principal add NAME`"},
		{"not a member", proto.OpMemberRemove, proto.MemberArgs{Name: "erin"}, proto.CodeNotFound,
			"member erin of project " + projA + " not found", "see who is a member with `sfx admin member list`"},
		{"disable an unknown principal", proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "nobody"}, proto.CodeNotFound,
			"principal nobody not found", "check the name"},
		{"principal name taken", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "bob"}, proto.CodeExists,
			`a principal named "bob" already exists`, "add them with `sfx admin member add bob`"},
		{"bad principal name", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "Bob Smith"}, proto.CodeInvalid,
			`principal "Bob Smith" must be a lowercase letter`, "`sfx admin principal add -h` shows the usage"},
		{"reserved principal name", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "import"}, proto.CodeInvalid,
			"a name the server uses itself", "correct it and retry"},
		{"bad role", proto.OpMemberSet, proto.MemberSetArgs{Name: "bob", Role: "owner"}, proto.CodeInvalid,
			`role "owner" must be member or admin`, "`sfx admin member add -h` shows the usage"},
		{"project and no project", proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal", Project: projA, Unassigned: true}, proto.CodeInvalid,
			"a project or to none", "--project or --no-project"},
		{"members past the cap", proto.OpMemberSet, proto.MemberSetArgs{Name: "frank"}, proto.CodeInvalid,
			"a project keeps at most 3 members", "ask the server's operator to raise members_per_project under limits: in starfixd's config"},
		{"limit out of range", proto.OpProjects, proto.AdminListArgs{Limit: proto.AdminListMax + 1}, proto.CodeInvalid,
			"limit 1001 is out of range", "give -n from 1 to 1000"},
		{"unknown argument", proto.OpProjectAdd, json.RawMessage(`{"name":"new","colour":1}`), proto.CodeInvalid, "bad arguments", proto.FixUpgrade},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := ask[json.RawMessage](d, tc.op, tc.args)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Errorf("%s = %+v\nwant %s, a message containing %q and a fix containing %q", tc.op, perr, tc.code, tc.msg, tc.fix)
			}
		})
	}
}

func TestAdminListsPage(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projA)
	tests := []struct {
		name string
		in   proto.AdminListArgs
		want []string
		more int
	}{
		{"all", proto.AdminListArgs{}, []string{"alice", "bob", "carol", "erin", "frank"}, 0},
		{"a limit, and how many are left", proto.AdminListArgs{Limit: 2}, []string{"alice", "bob"}, 3},
		{"after a name", proto.AdminListArgs{Limit: 2, After: "bob"}, []string{"carol", "erin"}, 1},
		{"after a name that is not there", proto.AdminListArgs{After: "dave"}, []string{"erin", "frank"}, 0},
		{"after the last", proto.AdminListArgs{After: "frank"}, nil, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := mustAsk[proto.PrincipalsResult](d, proto.OpPrincipals, tc.in)
			var got []string
			for _, p := range res.Principals {
				got = append(got, p.Name)
			}
			if !slices.Equal(got, tc.want) || res.More != tc.more {
				t.Errorf("principals(%+v) = %v and %d more, want %v and %d more", tc.in, got, res.More, tc.want, tc.more)
			}
		})
	}
	// The same paging for a project's members.
	ms := mustAsk[proto.MembersResult](d, proto.OpMembers, proto.MembersArgs{AdminListArgs: proto.AdminListArgs{Limit: 1, After: "alice"}})
	if len(ms.Members) != 1 || ms.Members[0].Principal != "bob" || ms.More != 1 {
		t.Errorf("members after alice, one = %+v and %d more; want bob and 1 more", ms.Members, ms.More)
	}
}

// TestAdminProjectAdd creates projects from a block of four names of
// which three are granted.
func TestAdminProjectAdd(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projA)
	state := func(name string) store.ProjectState {
		t.Helper()
		p, ok := m.ctl.ProjectNamed(name)
		if !ok {
			return ""
		}
		return p.State
	}

	// No block of names: nothing can be created, and nothing is written.
	_, perr := ask[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "one"})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "no database names for new projects") ||
		!strings.Contains(perr.Fix, "project_dbs:") || state("one") != "" {
		t.Fatalf("project.add with no block = %+v (state %q); want invalid with a fix naming project_dbs, and no row", perr, state("one"))
	}

	b := m.block(t, 3, 2)
	one := mustAsk[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "one"})
	if !uuidPattern.MatchString(one.ID) || one.Name != "one" || one.State != "active" || one.Database != b.Name(1) || one.Resumed {
		t.Fatalf("project.add one = %+v; want an active project with a new UUID in %s", one, b.Name(1))
	}
	// It is served at once to the admin who made it, and to a member
	// once added, with the name as its issue prefix and the default
	// account.
	mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "bob", Project: "one"})
	inOne := mustDialTo(t, m.Server, dana, one.ID)
	is := mustAsk[proto.CreateResult](inOne, proto.OpCreate, proto.CreateArgs{Title: "first"})
	if !strings.HasPrefix(is.ID, "one-") {
		t.Errorf("an issue of project one has id %q, want the prefix one-", is.ID)
	}
	if l := mustAsk[proto.ListResult](mustDialTo(t, m.Server, bob, one.ID), proto.OpList, proto.ListArgs{}); len(l.Issues) != 1 {
		t.Errorf("bob, a member of the new project, lists %d issues, want 1", len(l.Issues))
	}
	if sh := mustAsk[proto.ShowResult](inOne, proto.OpShow, proto.ShowArgs{ID: is.ID}); sh.Usage == nil || sh.Usage.Account != DefaultAccount {
		t.Errorf("project one's default account: usage %+v, want %s", sh.Usage, DefaultAccount)
	}

	// An active project's name is taken.
	_, perr = ask[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "one"})
	if perr == nil || perr.Code != proto.CodeExists || !strings.Contains(perr.Message, `a project named "one" already exists`) || perr.Fix == "" {
		t.Errorf("project.add of an active project = %+v, want exists with a fix", perr)
	}
	// So is an adopted one's, which no block holds.
	if _, perr = ask[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "proj-a"}); perr == nil || perr.Code != proto.CodeExists {
		t.Errorf("project.add of an adopted project's name = %+v, want exists", perr)
	}
	// A name that is no code name is refused before anything is made.
	if _, perr = ask[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "Not A Name"}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("project.add of a bad name = %+v, want invalid", perr)
	}

	two := mustAsk[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "two", Prefix: "tw", Account: "acme"})
	if two.Database != b.Name(2) {
		t.Errorf("the second project is in %s, want the lowest free name %s", two.Database, b.Name(2))
	}
	if p, _ := m.ctl.ProjectNamed("two"); p.Prefix != "tw" || p.Account != "acme" {
		t.Errorf("project two = %+v, want prefix tw and account acme", p)
	}

	// The third name was never granted: the database is created and
	// cannot be used. The refusal gives the SQL, and the row stays.
	_, perr = ask[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "three"})
	wantSQL := fmt.Sprintf("GRANT ALL ON %s.* TO 'starfix'@'localhost';", b.Name(3))
	if perr == nil || perr.Code != proto.CodeUnavailable || !strings.Contains(perr.Message, "was never granted database "+b.Name(3)) ||
		!strings.Contains(perr.Fix, wantSQL) || !strings.Contains(perr.Fix, "as Dolt's root") || !strings.Contains(perr.Fix, "`sfx admin project add three` again") {
		t.Fatalf("project.add of a name not granted = %+v\nwant unavailable, with a fix giving %q and the command to run again", perr, wantSQL)
	}
	if got := state("three"); got != store.ProjectCreating {
		t.Fatalf("project three is %q after the refusal, want it left creating", got)
	}
	// Not served meanwhile, and not activated by hand.
	three, _ := m.ctl.ProjectNamed("three")
	if w, _ := dialTo(t, m.Server, dana, three.ID); w.Err == nil || w.Err.Code != proto.CodeNotFound {
		t.Errorf("a handshake for a project still being created = %+v, want not found", w.Err)
	}
	_, perr = ask[proto.ProjectResult](d, proto.OpProjectActivate, proto.ProjectArgs{Project: "three"})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Fix, "`sfx admin project add three` again") {
		t.Errorf("project.activate of a project still being created = %+v, want invalid with a fix naming project add", perr)
	}
	// A rerun with other settings is refused: they cannot change.
	_, perr = ask[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "three", Prefix: "other"})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "is being created with prefix three") {
		t.Errorf("project.add resumed with another prefix = %+v, want invalid", perr)
	}
	// Still not granted: the same refusal, and still no second row.
	if _, perr = ask[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "three"}); perr == nil || !strings.Contains(perr.Fix, wantSQL) {
		t.Errorf("project.add again before the grant = %+v, want the same fix", perr)
	}
	// The block is full while three holds its name.
	_, perr = ask[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "four"})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "every database name for new projects is in use") ||
		!strings.Contains(perr.Fix, "raise count under project_dbs:") || !strings.Contains(perr.Fix, b.Name(4)) || state("four") != "" {
		t.Errorf("project.add past the block = %+v (state %q)\nwant invalid, with a fix saying to grant more from %s and raise count, and no row",
			perr, state("four"), b.Name(4))
	}

	// After the grant, the same command resumes and finishes.
	if err := doltOf(t).Grant(t.Context(), b.Name(3)); err != nil {
		t.Fatal(err)
	}
	done := mustAsk[proto.ProjectAddResult](d, proto.OpProjectAdd, proto.ProjectAddArgs{Name: "three"})
	if done.ID != three.ID || done.State != "active" || !done.Resumed || done.Database != b.Name(3) {
		t.Errorf("project.add after the grant = %+v; want project %s resumed and active in %s", done, three.ID, b.Name(3))
	}
	mustDialTo(t, m.Server, dana, three.ID)
	if all, err := m.ctl.Projects(t.Context()); err != nil || len(all) != 5 {
		t.Errorf("the server holds %d projects (%v), want the 2 adopted and the 3 added", len(all), err)
	}
}

// TestAdminArchive archives a project in use: its connections are
// closed and its store with them, while the admin, connected elsewhere,
// gets the answer. Activated again, it is served from the same database.
func TestAdminArchive(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projB)
	a1, a2 := mustDialTo(t, m.Server, alice, projA), mustDialTo(t, m.Server, bob, projA)
	mustAsk[proto.CreateResult](a1, proto.OpCreate, proto.CreateArgs{Title: "before the archive"})
	inB := mustDialTo(t, m.Server, carol, projB)

	res := mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: "proj-a"})
	if res.ID != projA || res.State != "archived" || res.Rev != 3 {
		t.Errorf("project.archive = %+v, want %s archived at rev 3", res, projA)
	}
	for name, c := range map[string]*conn{"alice": a1, "bob": a2} {
		if !c.dropped() {
			t.Errorf("%s's connection to the archived project is still open", name)
		}
		c.hangUp()
	}
	if !d.alive() || !inB.alive() {
		t.Error("archiving a closed a connection to b")
	}
	// The handlers have ended, so the store is closing or closed.
	m.stores.closeWhere("test", func(*entry) bool { return false })
	if got := m.OpenStores(); got != 1 {
		t.Errorf("%d stores are open after the archive, want only b's", got)
	}
	if !strings.Contains(m.log.String(), "reason=archived") {
		t.Errorf("the log does not say the store was closed for the archive:\n%s", m.log)
	}
	if w, _ := dialTo(t, m.Server, alice, projA); w.Err == nil || w.Err.Code != proto.CodeNotFound {
		t.Errorf("a handshake for the archived project = %+v, want not found", w.Err)
	}
	// Archiving again changes nothing.
	if again := mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projA}); again.Rev != res.Rev {
		t.Errorf("project.archive again = %+v, want rev %d unchanged", again, res.Rev)
	}

	back := mustAsk[proto.ProjectResult](d, proto.OpProjectActivate, proto.ProjectArgs{Project: "proj-a"})
	if back.State != "active" || back.Rev != 4 {
		t.Errorf("project.activate = %+v, want active at rev 4", back)
	}
	if l := mustAsk[proto.ListResult](mustDialTo(t, m.Server, alice, projA), proto.OpList, proto.ListArgs{}); len(l.Issues) != 1 {
		t.Errorf("the reactivated project lists %d issues, want the 1 written before", len(l.Issues))
	}

	// The last active project is not archived: with none, no connection
	// could be made to activate one again.
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projB); err != nil {
		t.Fatal(err)
	}
	onlyA := mustDialTo(t, m.Server, dana, projA)
	_, perr := ask[proto.ProjectResult](onlyA, proto.OpProjectArchive, proto.ProjectArgs{Project: projA})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "proj-a is the server's only active project") ||
		!strings.Contains(perr.Fix, "`sfx admin project activate PROJECT`") {
		t.Errorf("project.archive of the last active project = %+v; want invalid, with a fix naming activate", perr)
	}
	if p, _ := m.ctl.ProjectNamed("proj-a"); p.State != store.ProjectActive {
		t.Errorf("the last active project is %s after the refused archive", p.State)
	}
	mustAsk[proto.ProjectResult](onlyA, proto.OpProjectActivate, proto.ProjectArgs{Project: projB})

	// An admin who archives the project they are connected to still
	// gets the answer; then that connection closes too.
	own := mustDialTo(t, m.Server, dana, projA)
	if res := mustAsk[proto.ProjectResult](own, proto.OpProjectArchive, proto.ProjectArgs{Project: projA}); res.State != "archived" {
		t.Errorf("project.archive of the connection's own project = %+v", res)
	}
	if !own.dropped() {
		t.Error("the admin's connection to the project they archived is still open")
	}
}

// TestAdminDrops pins which connections each end of access closes, and
// that nothing else is closed.
func TestAdminDrops(t *testing.T) {
	t.Parallel()
	t.Run("membership removed", func(t *testing.T) {
		m := adminWorld(t, Limits{})
		d := mustDialTo(t, m.Server, dana, projA)
		c1, c2 := mustDialTo(t, m.Server, carol, projA), mustDialTo(t, m.Server, carol, projA)
		elsewhere, other := mustDialTo(t, m.Server, carol, projB), mustDialTo(t, m.Server, bob, projA)
		if got := m.connections("carol", projA); got != 2 {
			t.Fatalf("carol has %d connections to a, want 2", got)
		}
		res := mustAsk[proto.MemberRemoveResult](d, proto.OpMemberRemove, proto.MemberArgs{Name: "carol"})
		if res.ID != "carol" || res.Project != projA {
			t.Errorf("member.remove = %+v", res)
		}
		if !c1.dropped() || !c2.dropped() {
			t.Error("carol's connections to the project she was removed from are still open")
		}
		if !elsewhere.alive() || !other.alive() || !d.alive() {
			t.Error("removing carol from a closed her connection to b, bob's, or the admin's")
		}
		if w, _ := dialTo(t, m.Server, carol, projA); w.Err == nil || w.Err.Code != proto.CodeForbidden {
			t.Errorf("carol's next handshake = %+v, want forbidden", w.Err)
		}
	})
	t.Run("a server admin's membership removed", func(t *testing.T) {
		m := adminWorld(t, Limits{})
		m.member(t, projA, "dana")
		d := mustDialTo(t, m.Server, dana, projA)
		other := mustDialTo(t, m.Server, alice, projA)
		mustAsk[proto.MemberRemoveResult](other, proto.OpMemberRemove, proto.MemberArgs{Name: "dana"})
		if !d.alive() {
			t.Error("a server admin's connection was closed with their membership, which they do not need")
		}
	})
	t.Run("an admin removes themselves", func(t *testing.T) {
		m := adminWorld(t, Limits{})
		a := mustDialTo(t, m.Server, alice, projA)
		if res := mustAsk[proto.MemberRemoveResult](a, proto.OpMemberRemove, proto.MemberArgs{Name: "alice"}); res.ID != "alice" {
			t.Errorf("member.remove of oneself = %+v, want the answer before the connection closes", res)
		}
		if !a.dropped() {
			t.Error("the connection of an admin who removed themselves is still open")
		}
	})
	t.Run("demotion closes nothing", func(t *testing.T) {
		m := adminWorld(t, Limits{})
		d := mustDialTo(t, m.Server, dana, projA)
		a := mustDialTo(t, m.Server, alice, projA)
		res := mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "alice", Role: "member"})
		if res.Change != "changed" || res.Role != "member" {
			t.Errorf("member.set alice member = %+v", res)
		}
		if !a.alive() {
			t.Error("alice's connection was closed when she was made a member; she is still one")
		}
		// And she is no admin on that connection any more.
		if _, perr := ask[proto.MembersResult](a, proto.OpMembers, proto.MembersArgs{}); perr == nil || perr.Code != proto.CodeForbidden {
			t.Errorf("members by alice after her demotion = %v, want forbidden", perr)
		}
	})
	t.Run("principal disabled", func(t *testing.T) {
		m := adminWorld(t, Limits{})
		d := mustDialTo(t, m.Server, dana, projB)
		inA, inB := mustDialTo(t, m.Server, carol, projA), mustDialTo(t, m.Server, carol, projB)
		other := mustDialTo(t, m.Server, bob, projA)
		res := mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "carol"})
		if res.ID != "carol" || res.State != "disabled" || res.Rev != 2 {
			t.Errorf("principal.disable = %+v", res)
		}
		if !inA.dropped() || !inB.dropped() {
			t.Error("a disabled principal's connections are still open")
		}
		if !other.alive() || !d.alive() {
			t.Error("disabling carol closed bob's connection or the admin's")
		}
		if w, _ := dialTo(t, m.Server, carol, projA); w.Err == nil || w.Err.Code != proto.CodeForbidden {
			t.Errorf("a disabled principal's next handshake = %+v, want forbidden", w.Err)
		}
		mustAsk[proto.PrincipalResult](d, proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "carol"})
		mustDialTo(t, m.Server, carol, projA)
	})
	// Disabled wins over the config file (decision 20): a disabled server
	// admin loses the connections they had, and the next is refused.
	// TestDisabledServerAdmin has the rest.
	t.Run("a server admin disabled", func(t *testing.T) {
		m := newMultiAdmins(t, Limits{}, nil, dana.Principal, "sam")
		m.project(t, projA, "dana")
		m.project(t, projB)
		d, other := mustDialTo(t, m.Server, dana, projA), mustDialTo(t, m.Server, dana, projB)
		if res := mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "dana"}); res.State != "disabled" {
			t.Errorf("principal.disable of oneself = %+v, want the answer before the connection closes", res)
		}
		if !d.dropped() || !other.dropped() {
			t.Error("a disabled server admin's connections are still open")
		}
		if w, _ := dialTo(t, m.Server, dana, projA); w.Err == nil || w.Err.Code != proto.CodeForbidden {
			t.Errorf("a disabled server admin's next handshake = %+v, want forbidden", w.Err)
		}
	})
	t.Run("a refused removal closes nothing", func(t *testing.T) {
		m := adminWorld(t, Limits{})
		b, c := mustDialTo(t, m.Server, bob, projA), mustDialTo(t, m.Server, carol, projA)
		if _, perr := ask[proto.MemberRemoveResult](b, proto.OpMemberRemove, proto.MemberArgs{Name: "carol"}); perr == nil {
			t.Fatal("a member removed a member")
		}
		if !c.alive() {
			t.Error("a refused removal closed the connection")
		}
	})
}

// A connection whose access ended while its handshake ran is closed as
// it is recorded: drop looks only at connections already recorded.
func TestTrackRechecksAccess(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	c := mustDialTo(t, m.Server, bob, projA)
	sess := &session{actor: bob, proj: &served{id: projA}}
	if !m.track(c.nc, sess) {
		t.Fatal("track refused a member's connection")
	}
	if _, err := m.ctl.RemoveMember(t.Context(), dana, "bob", projA); err != nil {
		t.Fatal(err)
	}
	if m.track(c.nc, sess) {
		t.Error("track accepted a connection whose membership ended during the handshake")
	}
	m.untrack(c.nc)
}

// TestAdminPrompts pins the two things an admin is told beyond the
// change they asked for.
func TestAdminPrompts(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	d := mustDialTo(t, m.Server, dana, projA)
	keys, err := m.ctl.Keys(t.Context(), "carol")
	if err != nil || len(keys) != 1 {
		t.Fatalf("carol's keys = %v, %v", keys, err)
	}

	// carol leaves a: erin, admin of b, could not manage her and now can.
	res := mustAsk[proto.MemberRemoveResult](d, proto.OpMemberRemove, proto.MemberArgs{Name: "carol"})
	want := "every project carol still belongs to is administered by a project admin who could not manage carol before, and now can: " +
		"disable or enable carol, and manage its keys"
	if res.Warning != want {
		t.Errorf("member.remove carol from a: warning %q\nwant %q", res.Warning, want)
	}
	// bob leaves his only project: he is out of every admin's reach.
	if res := mustAsk[proto.MemberRemoveResult](d, proto.OpMemberRemove, proto.MemberArgs{Name: "bob"}); res.Warning != "" {
		t.Errorf("member.remove bob, left in no project: warning %q, want none", res.Warning)
	}

	// carol joins a second project again: the keys that come with her
	// are shown first, and she is added once they are accepted (decision
	// 36; member_keys_test.go has the cases).
	set := mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "carol"})
	if set.Change != proto.MemberPending || set.Projects != 1 || !slices.Equal(carried(set), []string{keys[0].Fingerprint}) {
		t.Errorf("member.set carol into a second project = %+v; want pending, the 1 project she is in and her key %s", set, keys[0].Fingerprint)
	}
	set = mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "carol", AcceptKeys: true})
	if set.Change != "added" || set.Projects != 2 || !slices.Equal(carried(set), []string{keys[0].Fingerprint}) {
		t.Errorf("member.set carol, accepting = %+v; want added, 2 projects and her key %s", set, keys[0].Fingerprint)
	}
	// A role change is no new membership, and shows none.
	if set := mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "carol", Role: "admin"}); set.Projects != 0 || set.Carried != nil {
		t.Errorf("member.set carol admin = %+v, want no keys for a role change", set)
	}
	// Someone with no key is added at once.
	if set := mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "frank"}); set.Change != "added" || set.Projects != 1 || set.Carried != nil {
		t.Errorf("member.set frank, who has no key, into his first project = %+v, want added and no keys", set)
	}
	// A project admin without reach over the principal is shown its keys
	// all the same: erin administers b, and bob, once back in a, is in a.
	mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "bob"})
	if _, err := m.ctl.AddKey(t.Context(), dana, "bob", keyLine(t, "bob")); err != nil {
		t.Fatal(err)
	}
	e := mustDialTo(t, m.Server, erin, projB)
	if set := mustAsk[proto.MemberSetResult](e, proto.OpMemberSet, proto.MemberSetArgs{Name: "bob"}); set.Change != proto.MemberPending ||
		!slices.Equal(carried(set), []string{fingerprintOf(t, "bob")}) {
		t.Errorf("member.set bob into b by erin, who does not administer a = %+v; want pending, and his key shown", set)
	}
}

// Writes to the control store have a bucket of their own, the
// principal's on the whole server: a project's writes do not drain it,
// and it does not drain theirs.
func TestControlWriteRateLimit(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{ControlWriteBurst: 2, ControlWriteRate: 0.001, WriteBurst: 2, WriteRate: 0.001})
	inA, inB := mustDialTo(t, m.Server, dana, projA), mustDialTo(t, m.Server, dana, projB)
	mustAsk[proto.MemberSetResult](inA, proto.OpMemberSet, proto.MemberSetArgs{Name: "frank"})
	mustAsk[proto.MemberSetResult](inB, proto.OpMemberSet, proto.MemberSetArgs{Name: "frank"})
	// The third, on either connection, is over the limit; a refused
	// write counts too, and so would one that names nothing real.
	for name, c := range map[string]*conn{"a": inA, "b": inB} {
		_, perr := ask[proto.MemberRemoveResult](c, proto.OpMemberRemove, proto.MemberArgs{Name: "frank"})
		if perr == nil || perr.Code != proto.CodeBusy || !strings.HasPrefix(perr.Fix, "wait ") ||
			!strings.Contains(perr.Fix, "control_write_rate and control_write_burst under limits:") ||
			!strings.Contains(perr.Message, "dana is over the limit on access changes (0.001 a second, bursts of 2)") {
			t.Errorf("a third control write, on the connection to %s = %+v; want busy, a wait and the limits named", name, perr)
		}
	}
	// Reads of the control store are not counted, nor are the project's
	// own writes, which have their bucket still full.
	mustAsk[proto.MembersResult](inA, proto.OpMembers, proto.MembersArgs{})
	mustAsk[proto.CreateResult](inA, proto.OpCreate, proto.CreateArgs{Title: "one"})
	mustAsk[proto.CreateResult](inA, proto.OpCreate, proto.CreateArgs{Title: "two"})
	if _, perr := ask[proto.CreateResult](inA, proto.OpCreate, proto.CreateArgs{Title: "three"}); perr == nil || perr.Code != proto.CodeBusy ||
		strings.Contains(perr.Fix, "control_write") {
		t.Errorf("a third project write = %v, want busy from the project's own bucket", perr)
	}
	// Another principal has a bucket of their own.
	mustAsk[proto.MemberSetResult](mustDialTo(t, m.Server, alice, projA), proto.OpMemberSet, proto.MemberSetArgs{Name: "bob", Role: "admin"})
}

// Every admin op is routed, and counted as a read or as a control write.
func TestAdminOpsClassified(t *testing.T) {
	t.Parallel()
	for op := range adminHandlers {
		if !adminOp(op) {
			t.Errorf("adminOp(%s) = false for an op with a handler", op)
		}
		read := op == proto.OpProjects || op == proto.OpPrincipals || op == proto.OpMembers || op == proto.OpKeys || op == proto.OpStatus ||
			op == proto.OpPrincipalWhois
		if readOps[op] != read || controlWrites[op] == read {
			t.Errorf("%s: read op %t, control write %t; a list, the status and a stand-in's lookup are reads and everything else a control write", op, readOps[op], controlWrites[op])
		}
	}
	for op := range handlers {
		if adminOp(op) {
			t.Errorf("%s is both an admin op and a project op", op)
		}
	}
}

// TestOpenProjects pins what a daemon does before it listens: every
// active project's database is opened, and so migrated; one that cannot
// be is held against the upgrade's health probe until it opens or is
// archived, while the rest are served.
func TestOpenProjects(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	m.project(t, projC, "alice")
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projC); err != nil {
		t.Fatal(err)
	}
	probe := func() *proto.Frame {
		t.Helper()
		// It names a project that is not served: the answer is about the
		// server.
		w, _ := dialTo(t, m.Server, store.Actor{Principal: ProbePrincipal, Session: "upgrade-probe", Machine: "localhost"}, projC)
		return w
	}

	if err := m.OpenProjects(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a, b, c := m.opened(projA), m.opened(projB), m.opened(projC); a != 1 || b != 1 || c != 0 {
		t.Errorf("OpenProjects opened a %d, b %d and the archived c %d times; want 1, 1 and 0", a, b, c)
	}
	if got := m.OpenStores(); got != 0 {
		t.Errorf("OpenProjects left %d stores open, want none", got)
	}
	if w := probe(); w.Err != nil || w.Principal != ProbePrincipal || w.Version != "v0.2.0" {
		t.Errorf("the probe after every project opened = %+v (%v), want a welcome", w, w.Err)
	}

	// A project whose database starfixd cannot open: never created.
	const broken = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	if _, err := m.ctl.AddProject(t.Context(), dana, store.NewProject{ID: broken, Name: "proj-d", Prefix: "pd", Account: "internal",
		DBName: "not_made_yet"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.ActivateProject(t.Context(), dana, broken); err != nil {
		t.Fatal(err)
	}
	if err := m.OpenProjects(t.Context()); err != nil {
		t.Fatalf("OpenProjects with one project broken = %v; it must go on to the others", err)
	}
	if a, b := m.opened(projA), m.opened(projB); a != 2 || b != 2 {
		t.Errorf("with one project broken, a was opened %d and b %d times in all; want 2 each", a, b)
	}
	w := probe()
	if w.Err == nil || w.Err.Code != proto.CodeUnavailable || w.Err.Fix == "" ||
		!strings.Contains(w.Err.Message, "could not open the database of an active project: proj-d (database not_made_yet)") {
		t.Errorf("the probe with a project unopened = %+v; want unavailable, naming the project and its database", w.Err)
	}
	if !strings.Contains(m.log.String(), "open project store at start") {
		t.Errorf("the log does not name the project that failed:\n%s", m.log)
	}
	// The others are served all the same.
	d := mustDialTo(t, m.Server, dana, projA)
	// Archived, the broken project no longer counts against the server.
	mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: "proj-d"})
	if w := probe(); w.Err != nil {
		t.Errorf("the probe after the broken project was archived = %v, want a welcome", w.Err)
	}
}

// A project whose database opens after all, at a later connection, no
// longer counts against the probe.
func TestOpenProjectsRecovers(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	p, ok := m.ctl.ProjectNamed("proj-a")
	if !ok {
		t.Fatal("no project a")
	}
	m.mu.Lock()
	m.unopened[projA] = p
	m.mu.Unlock()
	if e := m.unopenedErr(); e == nil {
		t.Fatal("unopenedErr() = nil with a project unopened")
	}
	mustDialTo(t, m.Server, alice, projA)
	if e := m.unopenedErr(); e != nil {
		t.Errorf("unopenedErr() = %v after the project's store opened, want nil", e)
	}
}

// A released client is refused at the handshake, before any request: its
// protocol is below ProtoMin (decision 25). What it reads is the server's
// to choose, since a released client prints the welcome's error as it
// is: that the client is the side to upgrade, and how.
func TestReleasedClientsRefused(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	const fix = "upgrade this client: run `sfx upgrade`, or install the latest release (docs/install.md), then restart any agent that runs `sfx mcp`"
	for _, tc := range []struct {
		release string
		proto   int
	}{{"v0.2.x", 2}, {"v0.3.x", 3}, {"v0.4.x", 4}} {
		t.Run(tc.release, func(t *testing.T) {
			w, err := handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: "alice"},
				&proto.Frame{T: proto.FrameHello, Version: "v0.4.0", Proto: tc.proto, Project: project, Session: "s", Machine: "m"})
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("starfix speaks protocol %d; the server (v0.2.0) needs protocol %d", tc.proto, proto.ProtoMin)
			if w.T != proto.FrameWelcome || w.Err == nil || w.Err.Code != proto.CodeVersion || w.Err.Message != want || w.Err.Fix != fix ||
				w.Min != proto.ProtoMin || w.Max != proto.ProtoMax {
				t.Fatalf("a protocol %d hello = %+v (%+v)\nwant a welcome refusing it with code version, %q\nfix %q", tc.proto, w, w.Err, want, fix)
			}
		})
	}
}

// The health check of `starfixd upgrade` is run by the binary being
// replaced, which speaks its own protocol to the daemon that replaced
// it. The daemon answers it whatever it speaks: a v0.4.0 starfixd that
// was refused would take the new daemon for dead and put itself back.
func TestProbeOfAnOlderStarfixdIsAnswered(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	for _, p := range []int{2, 4, proto.Proto, proto.ProtoMax + 1} {
		f, err := handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: ProbePrincipal},
			&proto.Frame{T: proto.FrameHello, Version: "v0.4.0", Proto: p, Project: project, Session: "upgrade-probe", Machine: "localhost"})
		if err != nil {
			t.Fatal(err)
		}
		if f.T != proto.FrameWelcome || f.Err != nil || f.Version != "v0.2.0" {
			t.Errorf("the upgrade probe speaking protocol %d = %+v (%v), want a welcome with the daemon's version", p, f, f.Err)
		}
	}
}

// An older server refuses this client at the handshake, telling it to
// have the server upgraded, before a request could fail on an op or a
// field the server does not know. v0.4.x accepts 2 to 4.
func TestHandshakeOlderServerRefusesThisClient(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		release string
		lo, hi  int
		accepts string
	}{{"v0.2.x", 2, 2, "accepts protocol 2"}, {"v0.3.x", 2, 3, "accepts protocols 2 to 3"}, {"v0.4.x", 2, 4, "accepts protocols 2 to 4"}} {
		t.Run(tc.release, func(t *testing.T) {
			s := newServer(t)
			s.cfg.ProtoMin, s.cfg.ProtoMax = tc.lo, tc.hi
			f, err := handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: "alice"},
				&proto.Frame{T: proto.FrameHello, Version: "v0.5.0", Proto: proto.Proto, Project: project, Session: "s", Machine: "m"})
			if err != nil {
				t.Fatal(err)
			}
			if f.Err == nil || f.Err.Code != proto.CodeVersion || !strings.Contains(f.Err.Message, tc.accepts) ||
				!strings.Contains(f.Err.Fix, "starfixd upgrade") || strings.Contains(f.Err.Fix, "sfx upgrade") {
				t.Fatalf("a protocol %d client to a %s server = %+v, want a version refusal that says it %s and names starfixd upgrade alone",
					proto.Proto, tc.release, f.Err, tc.accepts)
			}
		})
	}
}

func TestProjectDBs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		b    ProjectDBs
		err  string
	}{
		{"none", ProjectDBs{}, ""},
		{"a block", ProjectDBs{Prefix: "starfix_p", Count: 100}, ""},
		{"the most", ProjectDBs{Prefix: "p", Count: MaxProjectDBs}, ""},
		{"no prefix", ProjectDBs{Count: 10}, "does not start a database name"},
		{"prefix with a capital", ProjectDBs{Prefix: "Starfix", Count: 10}, "does not start a database name"},
		{"prefix with a quote", ProjectDBs{Prefix: "p`; DROP", Count: 10}, "does not start a database name"},
		{"prefix too long", ProjectDBs{Prefix: strings.Repeat("p", 62), Count: 10}, "does not start a database name"},
		{"no count", ProjectDBs{Prefix: "starfix_p"}, "count 0 must be 1 to 999"},
		{"negative count", ProjectDBs{Prefix: "starfix_p", Count: -1}, "count -1 must be 1 to 999"},
		{"too many", ProjectDBs{Prefix: "starfix_p", Count: 1000}, "count 1000 must be 1 to 999"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.b.Validate()
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "fix: ")) {
				t.Errorf("%+v.Validate() = %v, want an error containing %q and a fix", tc.b, err, tc.err)
			}
		})
	}
	b := ProjectDBs{Prefix: "starfix_p", Count: 3}
	if got := []string{b.Name(1), b.Name(3)}; !slices.Equal(got, []string{"starfix_p001", "starfix_p003"}) {
		t.Errorf("names = %v", got)
	}
	for name, want := range map[string]bool{"starfix_p001": true, "starfix_p003": true, "starfix_p004": false, "starfix_p000": false,
		"starfix_p01": false, "starfix_p0001": false, "starfix_p00a": false, "starfix_control": false, "other001": false} {
		if got := b.holds(name); got != want {
			t.Errorf("holds(%q) = %t, want %t", name, got, want)
		}
	}
	if n, ok := b.free(map[string]bool{"starfix_p001": true, "starfix_p003": true}); !ok || n != "starfix_p002" {
		t.Errorf("free = %q, %t; want the lowest unused, starfix_p002", n, ok)
	}
	if n, ok := b.free(map[string]bool{"starfix_p001": true, "starfix_p002": true, "starfix_p003": true}); ok {
		t.Errorf("free of a full block = %q, want none", n)
	}
	if _, err := New(Config{Control: nil, Store: newServer(t).cfg.Store, Project: project, ProjectDBs: ProjectDBs{Prefix: "X", Count: 1}}); err == nil {
		t.Error("New accepted an invalid block of project database names")
	}
}
