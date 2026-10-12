package store

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestControlAccessIndex(t *testing.T) {
	c := newControl(t)
	mustProject(t, c, projA)
	mustProject(t, c, projB)
	if _, err := c.AddProject(t.Context(), dana, newProject(projC)); err != nil { // never activated
		t.Fatal(err)
	}
	mustPrincipal(t, c, "alice", projA+"=member", projC+"=member")
	mustPrincipal(t, c, "erin", projA+"=admin", projB+"=member")
	mustPrincipal(t, c, "frank") // in no project

	tests := []struct {
		name               string
		principal, project string
		active             bool
		role               Role
	}{
		{"member", "alice", projA, true, RoleMember},
		{"not a member", "alice", projB, true, ""},
		{"project admin", "erin", projA, true, RoleAdmin},
		{"admin of another project is a member here", "erin", projB, true, RoleMember},
		{"in no project", "frank", projA, true, ""},
		{"no principal row", "ivy", projA, true, ""},
		{"server admin with a row elsewhere or none", "sam", projA, true, RoleAdmin},
		{"server admin in a project being created", "dana", projC, false, RoleAdmin},
		{"member of a project being created", "alice", projC, false, RoleMember},
		{"unknown project", "alice", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", false, ""},
		{"not a project id", "alice", "nonsense", false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Access(tc.principal, tc.project)
			if got.Active != tc.active || got.Role != tc.role {
				t.Errorf("Access(%s, %s) = active %v, role %q; want active %v, role %q",
					tc.principal, tc.project, got.Active, got.Role, tc.active, tc.role)
			}
			if tc.active && got.Project.ID != tc.project {
				t.Errorf("Access(%s, %s).Project.ID = %q, want the project", tc.principal, tc.project, got.Project.ID)
			}
		})
	}

	// Each write changes the answer at once, with no reopening.
	steps := []struct {
		name   string
		do     func() error
		who    string
		proj   string
		active bool
		role   Role
	}{
		{"role raised", func() error { _, err := c.SetMember(t.Context(), dana, "alice", projA, RoleAdmin); return err },
			"alice", projA, true, RoleAdmin},
		{"principal disabled", func() error { _, err := c.DisablePrincipal(t.Context(), dana, "alice"); return err },
			"alice", projA, true, ""},
		{"principal enabled", func() error { _, err := c.EnablePrincipal(t.Context(), dana, "alice"); return err },
			"alice", projA, true, RoleAdmin},
		{"member removed", func() error { _, err := c.RemoveMember(t.Context(), dana, "alice", projA); return err },
			"alice", projA, true, ""},
		{"project archived", func() error { _, err := c.ArchiveProject(t.Context(), dana, projA); return err },
			"erin", projA, false, RoleAdmin},
		{"project activated again", func() error { _, err := c.ActivateProject(t.Context(), dana, projA); return err },
			"erin", projA, true, RoleAdmin},
		{"added with a project", func() error { _, err := c.AddPrincipal(t.Context(), dana, "ivy", projB); return err },
			"ivy", projB, true, RoleMember},
	}
	for _, st := range steps {
		if err := st.do(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if got := c.Access(st.who, st.proj); got.Active != st.active || got.Role != st.role {
			t.Errorf("after %s: Access(%s, %s) = active %v, role %q; want active %v, role %q",
				st.name, st.who, st.proj, got.Active, got.Role, st.active, st.role)
		}
	}

	// A refused write leaves the index as it was.
	if _, err := c.SetMember(t.Context(), frank, "frank", projA, RoleAdmin); !errors.Is(err, ErrForbidden) {
		t.Fatalf("SetMember by frank = %v, want ErrForbidden", err)
	}
	if got := c.Access("frank", projA); got.Role != "" {
		t.Errorf("after a refused write: Access(frank).Role = %q, want none", got.Role)
	}
}

// TestControlReload checks that Reload brings in what another process
// wrote to the control database, as `starfixd control init` does beside a
// running daemon.
func TestControlReload(t *testing.T) {
	dsn := newDSN(t)
	daemon := openControl(t, dsn, ControlOptions{})
	other := openControl(t, dsn, ControlOptions{})
	mustProject(t, other, projA)
	mustPrincipal(t, other, "alice", projA+"=member")
	k := mustKey(t, other, "alice", "alice-1")

	if got := daemon.Access("alice", projA); got.Active || got.Role != "" {
		t.Fatalf("before Reload: Access = %+v, want nothing: the index follows this store's writes only", got)
	}
	if err := daemon.Reload(t.Context()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := daemon.Access("alice", projA); !got.Active || got.Role != RoleMember || got.Project.DBName != "starfix_pa" {
		t.Errorf("after Reload: Access(alice, a) = %+v, want an active project and a member", got)
	}
	if who, _, ok := daemon.AuthorizedKey(k.Fingerprint); !ok || who != "alice" {
		t.Errorf("after Reload: AuthorizedKey = %q, %v; want alice", who, ok)
	}
	if n := daemon.ActiveProjects(); n != 1 {
		t.Errorf("ActiveProjects() = %d, want 1", n)
	}
}

// TestControlOperator checks the operator's exemption: a store opened by
// a command on the server lets the reserved operator principal write as
// a server admin does, and no other store does.
func TestControlOperator(t *testing.T) {
	op := Actor{Principal: OperatorPrincipal, Session: "control-init", Machine: "server"}
	if !Reserved(OperatorPrincipal) {
		t.Fatalf("%s is not reserved, so a key could authenticate as it", OperatorPrincipal)
	}

	t.Run("refused by the daemon's store", func(t *testing.T) {
		c := newControl(t)
		seq := controlSeq(t, c)
		_, err := c.AddProject(t.Context(), op, newProject(projA))
		if ae, ok := errors.AsType[*AccessError](err); !ok || ae.Reason != AccessServerAdmin {
			t.Errorf("AddProject as the operator without Operator = %v, want an *AccessError for server admins", err)
		}
		wantEvents(t, c, seq)
	})

	t.Run("allowed in the operator's store", func(t *testing.T) {
		c := openControl(t, newDSN(t), ControlOptions{Operator: true})
		seq := controlSeq(t, c)
		if _, err := c.AddProject(t.Context(), op, newProject(projA)); err != nil {
			t.Fatalf("AddProject: %v", err)
		}
		if _, err := c.ActivateProject(t.Context(), op, projA); err != nil {
			t.Fatalf("ActivateProject: %v", err)
		}
		p, err := c.AddPrincipal(t.Context(), op, "alice", "")
		if err != nil {
			t.Fatalf("AddPrincipal: %v", err)
		}
		if p.CreatedBy != OperatorPrincipal {
			t.Errorf("CreatedBy = %q, want %q", p.CreatedBy, OperatorPrincipal)
		}
		line, fp := testKey(t, "alice-1")
		if _, err := c.AddKey(t.Context(), op, "alice", line); err != nil {
			t.Fatalf("AddKey: %v", err)
		}
		if _, err := c.SetMember(t.Context(), op, "alice", projA, RoleMember); err != nil {
			t.Fatalf("SetMember: %v", err)
		}
		// Every write is history, under the operator's name.
		wantEvents(t, c, seq,
			ev{OpProjectAdd, projA, op, `"state":"creating"`},
			ev{OpProjectActivate, projA, op, `"state":"active"`},
			ev{OpPrincipalAdd, "alice", op, `"created_by":"import"`},
			ev{OpKeyAdd, fp, op, `"principal":"alice"`},
			ev{OpMemberSet, projA + "/alice", op, `"role":"member"`},
		)
		// The exemption is the operator principal's alone.
		if _, err := c.AddProject(t.Context(), frank, newProject(projB)); !errors.Is(err, ErrForbidden) {
			t.Errorf("AddProject by frank in the operator's store = %v, want ErrForbidden", err)
		}
		// It makes no one a server admin for the handshake.
		if c.IsServerAdmin(OperatorPrincipal) {
			t.Errorf("IsServerAdmin(%s) = true, want false", OperatorPrincipal)
		}
	})
}

func TestControlBackupTag(t *testing.T) {
	c := newControl(t)
	mustProject(t, c, projA)
	got, err := c.BackupTag(t.Context(), "starfix-v0.1.0")
	if err != nil || got != "starfix-v0.1.0" {
		t.Fatalf("BackupTag = %q, %v; want the tag", got, err)
	}
	if again, err := c.BackupTag(t.Context(), "starfix-v0.1.0"); err != nil || again != got {
		t.Errorf("BackupTag again = %q, %v; want %q unchanged", again, err, got)
	}
	if _, err := c.BackupTag(t.Context(), "Bad Tag"); !errors.Is(err, ErrInvalid) {
		t.Errorf("BackupTag(bad name) = %v, want ErrInvalid", err)
	}
}

// TestIsAdminHook checks Options.IsAdmin: the store asks it on every
// write, beside Options.Admins, so a role given or taken away counts at
// once.
func TestIsAdminHook(t *testing.T) {
	var erinAdmin atomic.Bool
	s := openStore(t, newDSN(t), Options{IsAdmin: func(p string) bool { return p == "erin" && erinAdmin.Load() }})
	is := mustCreate(t, s, NewIssue{Title: "held by alice"})
	if _, _, err := s.StartIssue(t.Context(), alice, is.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	title := "changed"
	update := func(a Actor) error {
		cur, err := s.GetIssue(t.Context(), is.ID)
		if err != nil {
			return err
		}
		_, err = s.UpdateIssue(t.Context(), a, is.ID, cur.Rev, IssuePatch{Title: &title})
		return err
	}

	if s.IsAdmin("erin") {
		t.Error("IsAdmin(erin) = true before the hook says so")
	}
	if err := update(erin); !errors.Is(err, ErrForbidden) {
		t.Errorf("update by erin, no admin = %v, want ErrForbidden", err)
	}
	erinAdmin.Store(true)
	if !s.IsAdmin("erin") {
		t.Error("IsAdmin(erin) = false after the hook says so")
	}
	if err := update(erin); err != nil {
		t.Errorf("update by erin, now an admin: %v", err)
	}
	erinAdmin.Store(false)
	if err := update(erin); !errors.Is(err, ErrForbidden) {
		t.Errorf("update by erin, admin no longer = %v, want ErrForbidden", err)
	}
	// Options.Admins still counts with a hook set.
	if !s.IsAdmin("dana") {
		t.Error("IsAdmin(dana) = false, want true from Options.Admins")
	}
	if err := update(dana); err != nil {
		t.Errorf("update by dana: %v", err)
	}
	evs, err := s.Events(t.Context(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var overrides []string
	for _, e := range evs {
		if e.Op == OpAdminOverride {
			overrides = append(overrides, e.Actor.Principal)
		}
	}
	if got := strings.Join(overrides, ","); got != "erin,dana" {
		t.Errorf("admin.override events by %q, want erin,dana", got)
	}
}

// TestAuthorizedKeys checks the list the daemon writes sshd's file from:
// every key AuthorizedKey answers for and no other, in an order that
// does not depend on the order of the writes.
func TestAuthorizedKeys(t *testing.T) {
	dsn := newDSN(t)
	c := openControl(t, dsn, ControlOptions{})
	ctx := t.Context()
	if got := c.AuthorizedKeys(); len(got) != 0 {
		t.Errorf("AuthorizedKeys() of an empty store = %+v, want none", got)
	}
	want := map[string][]PrincipalKey{}
	for _, tc := range []struct{ principal, seed string }{{"bob", "bob-2"}, {"alice", "alice-1"}, {"bob", "bob-1"}, {"carol", "carol-1"}, {"erin", "erin-1"}} {
		if _, err := c.Principal(ctx, tc.principal); err != nil {
			mustPrincipal(t, c, tc.principal)
		}
		line, fp := testKey(t, tc.seed)
		if _, err := c.AddKey(ctx, dana, tc.principal, line+" a comment"); err != nil {
			t.Fatal(err)
		}
		want[tc.principal] = append(want[tc.principal], PrincipalKey{Fingerprint: fp, Principal: tc.principal, Key: line})
	}
	if _, err := c.DisablePrincipal(ctx, dana, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RemoveKey(ctx, dana, want["erin"][0].Fingerprint); err != nil {
		t.Fatal(err)
	}
	bobs := want["bob"]
	slices.SortFunc(bobs, func(a, b PrincipalKey) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	all := append(slices.Clone(want["alice"]), bobs...)

	got := c.AuthorizedKeys()
	if !slices.Equal(got, all) {
		t.Errorf("AuthorizedKeys() = %+v\nwant %+v: the keys of alice and bob, by principal and then fingerprint, without carol's (disabled) or erin's (removed)", got, all)
	}
	for _, k := range got {
		if p, key, ok := c.AuthorizedKey(k.Fingerprint); !ok || p != k.Principal || key != k.Key {
			t.Errorf("AuthorizedKey(%s) = %q, %q, %v; AuthorizedKeys lists it as %+v", k.Fingerprint, p, key, ok, k)
		}
	}
	// What another store on the database loads is the same list.
	if again := openControl(t, dsn, ControlOptions{}).AuthorizedKeys(); !slices.Equal(again, all) {
		t.Errorf("AuthorizedKeys() of a second store = %+v, want %+v", again, all)
	}
}
