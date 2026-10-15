package store

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// grantedName returns a database name no test has used, derived from the
// control database's own, and grants it to the tests' account when
// granted is set, as an operator grants a block of names at setup.
func grantedName(t *testing.T, controlDSN string, granted bool) string {
	t.Helper()
	cfg, err := mysql.ParseDSN(controlDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := cfg.DBName + "_p001"
	if granted {
		if err := doltOf(t).Grant(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

// databaseDSN is dsn with its database replaced by name.
func databaseDSN(t *testing.T, dsn, name string) string {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = name
	return cfg.FormatDSN()
}

func TestControlCreateDatabase(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	c := openControl(t, dsn, ControlOptions{})
	ctx := t.Context()
	p := newProject(projA)
	p.DBName = grantedName(t, dsn, true)
	if _, err := c.AddProject(ctx, dana, p); err != nil {
		t.Fatal(err)
	}
	mustPrincipal(t, c, "alice")

	if err := c.CreateDatabase(ctx, alice, projA); !errors.Is(err, ErrForbidden) {
		t.Errorf("CreateDatabase by alice, no admin = %v, want ErrForbidden", err)
	} else if ae, ok := errors.AsType[*AccessError](err); !ok || ae.Reason != AccessServerAdmin {
		t.Errorf("CreateDatabase by alice = %v, want an AccessError for server admins", err)
	}
	if err := c.CreateDatabase(ctx, dana, projB); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateDatabase of an unknown project = %v, want ErrNotFound", err)
	}
	if err := c.CreateDatabase(ctx, dana, "nonsense"); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreateDatabase(nonsense) = %v, want ErrInvalid", err)
	}
	// Safe to repeat: the second run finds the database there.
	for i := range 2 {
		if err := c.CreateDatabase(ctx, dana, projA); err != nil {
			t.Fatalf("CreateDatabase, run %d: %v", i+1, err)
		}
	}
	st, err := Open(ctx, databaseDSN(t, dsn, p.DBName), Options{Prefix: p.Prefix, CommitInterval: -1})
	if err != nil {
		t.Fatalf("open the created database %s: %v", p.DBName, err)
	}
	if err := st.Close(); err != nil {
		t.Error(err)
	}
	// Only a project still being created has a database to create.
	if _, err := c.ActivateProject(ctx, dana, projA); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateDatabase(ctx, dana, projA); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreateDatabase of an active project = %v, want ErrInvalid", err)
	}
}

// A name root never granted can be created and not used: Open's error is
// then one DatabaseDenied knows, so the daemon can name the grant.
func TestDatabaseDenied(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	c := openControl(t, dsn, ControlOptions{})
	ctx := t.Context()
	p := newProject(projA)
	p.DBName = grantedName(t, dsn, false)
	if _, err := c.AddProject(ctx, dana, p); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateDatabase(ctx, dana, projA); err != nil {
		t.Fatalf("CreateDatabase of a name not granted: %v", err)
	}
	_, err := Open(ctx, databaseDSN(t, dsn, p.DBName), Options{Prefix: p.Prefix, CommitInterval: -1})
	if err == nil || !DatabaseDenied(err) {
		t.Fatalf("Open of a database not granted = %v, want an error DatabaseDenied reports", err)
	}
	if DatabaseDenied(ErrNotFound) || DatabaseDenied(nil) {
		t.Error("DatabaseDenied reports an error that is no refusal of a database")
	}
	// Granted afterward, the same database opens.
	if err := doltOf(t).Grant(ctx, p.DBName); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, databaseDSN(t, dsn, p.DBName), Options{Prefix: p.Prefix, CommitInterval: -1})
	if err != nil {
		t.Fatalf("open after the grant: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Error(err)
	}
}

func TestControlAccount(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	got, err := c.Account(t.Context())
	if want := "'starfix'@'localhost'"; err != nil || got != want {
		t.Errorf("Account() = %q, %v; want %q", got, err, want)
	}
}

func TestControlProjectNamed(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	mustProject(t, c, projA)
	if _, err := c.AddProject(t.Context(), dana, newProject(projB)); err != nil { // still creating
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, id string
		state    ProjectState
	}{{"proj-a", projA, ProjectActive}, {"proj-b", projB, ProjectCreating}} {
		p, ok := c.ProjectNamed(tc.name)
		if !ok || p.ID != tc.id || p.State != tc.state {
			t.Errorf("ProjectNamed(%q) = %+v, %t; want %s, %s", tc.name, p, ok, tc.id, tc.state)
		}
	}
	if p, ok := c.ProjectNamed("proj-z"); ok {
		t.Errorf("ProjectNamed(proj-z) = %+v, want none", p)
	}
}

// TestControlReach pins who is within whose reach, as AddKey and
// DisablePrincipal apply it, read from memory.
func TestControlReach(t *testing.T) {
	t.Parallel()
	c := accessWorld(t)
	tests := []struct {
		actor, principal string
		want             bool
	}{
		{"dana", "carol", true},   // a server admin reaches everyone
		{"sam", "frank", true},    // even one in no project
		{"alice", "bob", true},    // bob is in a only, which alice administers
		{"alice", "carol", false}, // carol is also in b
		{"ivy", "carol", true},    // ivy administers both
		{"erin", "bob", false},    // erin administers b, bob is in a
		{"alice", "frank", false}, // in no project
		{"alice", "dana", false},  // a server admin is out of reach
		{"bob", "hal", false},     // a member administers nothing
		{"alice", "nobody", false},
	}
	for _, tc := range tests {
		if got := c.Reaches(tc.actor, tc.principal); got != tc.want {
			t.Errorf("Reaches(%s, %s) = %t, want %t", tc.actor, tc.principal, got, tc.want)
		}
	}
	if _, err := c.DisablePrincipal(t.Context(), dana, "alice"); err != nil {
		t.Fatal(err)
	}
	if c.Reaches("alice", "bob") {
		t.Error("Reaches(alice, bob) with alice disabled = true; a disabled admin administers nothing")
	}
}

// TestControlReachGained pins the warning of `member remove`: the project
// admins who could not manage a principal's keys before it left a
// project, and can now.
func TestControlReachGained(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		setup     func(*testing.T, *Control)
		principal string
		leaves    string
		want      []string
	}{
		{name: "the remaining project's other admin", principal: "carol", leaves: projA, want: []string{"erin"}},
		{name: "the other way round", principal: "carol", leaves: projB, want: []string{"alice"}},
		{name: "left in no project, out of every reach", principal: "bob", leaves: projA},
		{name: "a server admin is never within reach", principal: "dana", leaves: projA,
			setup: func(t *testing.T, c *Control) {
				if _, err := c.SetMember(t.Context(), dana, "dana", projB, RoleMember); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a disabled admin gains nothing", principal: "carol", leaves: projA,
			setup: func(t *testing.T, c *Control) {
				if _, err := c.DisablePrincipal(t.Context(), dana, "erin"); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "an admin does not gain reach over themselves", principal: "ivy", leaves: projA, want: []string{"erin"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := accessWorld(t)
			if tc.setup != nil {
				tc.setup(t, c)
			}
			if _, err := c.RemoveMember(t.Context(), dana, tc.principal, tc.leaves); err != nil {
				t.Fatal(err)
			}
			if got := c.ReachGained(tc.principal, tc.leaves); !slices.Equal(got, tc.want) {
				t.Errorf("ReachGained(%s, left %s) = %v, want %v", tc.principal, strings.ToUpper(tc.leaves[:1]), got, tc.want)
			}
		})
	}
}
