package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/pgtest"
)

// schemaDSN is dsn with its schema replaced by name.
func schemaDSN(t *testing.T, dsn, name string) string {
	t.Helper()
	out, err := SchemaDSN(dsn, name)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// asSuperuser runs each statement on dsn's database as the server's
// superuser.
func asSuperuser(t *testing.T, dsn string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("pgx", superuserDSN(t, dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, q := range stmts {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestControlCreateDatabase(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	c := openControl(t, dsn, ControlOptions{})
	ctx := t.Context()
	p := newProject(projA)
	if _, err := c.AddProject(ctx, dana, p); err != nil {
		t.Fatal(err)
	}
	mustPrincipal(t, c, "alice")

	if err := c.CreateSchema(ctx, alice, projA); !errors.Is(err, ErrForbidden) {
		t.Errorf("CreateSchema by alice, no admin = %v, want ErrForbidden", err)
	} else if ae, ok := errors.AsType[*AccessError](err); !ok || ae.Reason != AccessServerAdmin {
		t.Errorf("CreateSchema by alice = %v, want an AccessError for server admins", err)
	}
	if err := c.CreateSchema(ctx, dana, projB); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateSchema of an unknown project = %v, want ErrNotFound", err)
	}
	if err := c.CreateSchema(ctx, dana, "nonsense"); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreateSchema(nonsense) = %v, want ErrInvalid", err)
	}
	// Before it is created, the project's schema does not open.
	if _, err := Open(ctx, schemaDSN(t, dsn, p.Schema), Options{Prefix: p.Prefix}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Open of a schema not yet created = %v, want ErrInvalid", err)
	}
	// Safe to repeat: the second run finds the schema there.
	for i := range 2 {
		if err := c.CreateSchema(ctx, dana, projA); err != nil {
			t.Fatalf("CreateSchema, run %d: %v", i+1, err)
		}
	}
	st, err := Open(ctx, schemaDSN(t, dsn, p.Schema), Options{Prefix: p.Prefix})
	if err != nil {
		t.Fatalf("open the created schema %s: %v", p.Schema, err)
	}
	if err := st.Close(); err != nil {
		t.Error(err)
	}
	// Only a project still being created has a schema to create.
	if _, err := c.ActivateProject(ctx, dana, projA); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateSchema(ctx, dana, projA); !errors.Is(err, ErrInvalid) {
		t.Errorf("CreateSchema of an active project = %v, want ErrInvalid", err)
	}
}

// A schema the role may not make, or that another role made and did not
// give it, is refused with an error SchemaDenied knows, so the daemon
// can name the grant; given the grant GrantSQL or CreateSchemaGrantSQL
// names, the same schema opens.
func TestDatabaseDenied(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	t.Run("made by another role", func(t *testing.T) {
		dsn := newDSN(t)
		c := openControl(t, dsn, ControlOptions{})
		p := newProject(projA)
		if _, err := c.AddProject(ctx, dana, p); err != nil {
			t.Fatal(err)
		}
		asSuperuser(t, dsn, "CREATE SCHEMA "+p.Schema)
		if err := c.CreateSchema(ctx, dana, projA); err != nil {
			t.Fatalf("CreateSchema of a schema another role made: %v", err)
		}
		_, err := Open(ctx, schemaDSN(t, dsn, p.Schema), Options{Prefix: p.Prefix})
		if err == nil || !SchemaDenied(err) {
			t.Fatalf("Open of a schema another role made = %v, want an error SchemaDenied reports", err)
		}
		role, err := c.Account(ctx)
		if err != nil {
			t.Fatal(err)
		}
		asSuperuser(t, dsn, GrantSQL(p.Schema, role))
		st, err := Open(ctx, schemaDSN(t, dsn, p.Schema), Options{Prefix: p.Prefix})
		if err != nil {
			t.Fatalf("open after %s: %v", GrantSQL(p.Schema, role), err)
		}
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})

	t.Run("no right to make schemas", func(t *testing.T) {
		dsn := newDSN(t)
		c := openControl(t, dsn, ControlOptions{})
		p := newProject(projA)
		if _, err := c.AddProject(ctx, dana, p); err != nil {
			t.Fatal(err)
		}
		d, err := ParseDSN(dsn)
		if err != nil {
			t.Fatal(err)
		}
		asSuperuser(t, dsn, "REVOKE CREATE ON DATABASE "+d.Database+" FROM "+d.User)
		err = c.CreateSchema(ctx, dana, projA)
		if err == nil || !SchemaDenied(err) {
			t.Fatalf("CreateSchema without the right to make schemas = %v, want an error SchemaDenied reports", err)
		}
		asSuperuser(t, dsn, CreateSchemaGrantSQL(d.Database, d.User))
		if err := c.CreateSchema(ctx, dana, projA); err != nil {
			t.Fatalf("CreateSchema after %s: %v", CreateSchemaGrantSQL(d.Database, d.User), err)
		}
	})

	if SchemaDenied(ErrNotFound) || SchemaDenied(nil) {
		t.Error("SchemaDenied reports an error that is no refusal of a schema")
	}
}

func TestControlAccount(t *testing.T) {
	t.Parallel()
	c := newControl(t)
	got, err := c.Account(t.Context())
	if want := pgtest.User; err != nil || got != want {
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
