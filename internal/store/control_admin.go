package store

import (
	"context"
	"fmt"
	"slices"
)

// What `aries admin` needs of the control store beyond its rows: a
// project's schema created, and the questions the daemon asks before it
// answers an admin (which project a name means, who is within whose
// reach).

// CreateSchema creates the schema of a project still being created,
// the step between [Control.AddProject] and [Control.ActivateProject]
// (design §15.5, 1), in the control store's database, owned by its role.
// Only a server admin may. It is safe to repeat: a schema that exists is
// left as it is, so the caller checks first that a new project's schema
// is not one already there ([Control.SchemaExists]).
//
// The role needs the right to make schemas in its database, which owning
// the database gives it; refused, the error is one [SchemaDenied]
// recognizes. That it can then use a schema that already existed is
// another matter, which [Open] finds out.
//
// It refuses a project that does not exist with ErrNotFound, and one
// that is not being created with ErrInvalid.
func (c *Control) CreateSchema(ctx context.Context, actor Actor, project string) error {
	if err := actor.validate(); err != nil {
		return err
	}
	if err := validProjectID(project); err != nil {
		return err
	}
	if !c.IsServerAdmin(actor.Principal) && (!c.operator || actor.Principal != OperatorPrincipal) {
		return &AccessError{Actor: actor.Principal, Op: OpProjectAdd, Project: project, Reason: AccessServerAdmin}
	}
	p, err := readProject(ctx, c.s.r, project)
	if err != nil {
		return err
	}
	if p.State != ProjectCreating {
		return fmt.Errorf("%w: project %s is %s; only a project being created has a schema to create", ErrInvalid, project, p.State)
	}
	// The row's name passed this check when it was written; it is
	// checked again here because the statement takes no placeholder.
	if !ValidSchemaName(p.Schema) {
		return fmt.Errorf("%w: schema name %q of project %s", ErrInvalid, p.Schema, project)
	}
	// Outside Store.write: it changes no table of the control store, so
	// there is no event to record. The project's own events say that it
	// was added and activated.
	if _, err := c.s.w.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdent(p.Schema)); err != nil {
		return fmt.Errorf("create schema %s: %w", p.Schema, err)
	}
	return nil
}

// SchemaExists reports whether the control store's database holds a
// schema named name, whoever made it: a project's schema must be a new
// one, never one another daemon, an earlier project or a person left
// there.
func (c *Control) SchemaExists(ctx context.Context, name string) (bool, error) {
	var ok bool
	if err := c.s.r.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, name).Scan(&ok); err != nil {
		return false, fmt.Errorf("look for schema %s: %w", name, err)
	}
	return ok, nil
}

// HeldClaims returns how many claims are held in the project schema
// named schema, with the holds and places in its locks' queues, which end
// with access as claims do, by one read through the control store's own
// connection: the schemas are in one database, and no store is opened
// for it. ariesd asks it of each archived project at start, so that one
// with nothing to end costs a query and not a store.
//
// It reads the tables as this binary's schema has them, without checking
// the schema's version. An error therefore says only that the caller
// must open the schema to learn more: it may not exist, may not be the
// role's, or may hold another version's tables.
func (c *Control) HeldClaims(ctx context.Context, schema string) (int, error) {
	// The statement takes no placeholder for a schema name.
	if !ValidSchemaName(schema) {
		return 0, fmt.Errorf("%w: schema name %q", ErrInvalid, schema)
	}
	s := quoteIdent(schema)
	var n int
	if err := c.s.r.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM `+s+`.claims WHERE principal IS NOT NULL)
  + (SELECT COUNT(*) FROM `+s+`.lock_holders) + (SELECT COUNT(*) FROM `+s+`.lock_waiters)`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the claims and locks held in %s: %w", schema, err)
	}
	return n, nil
}

// Database returns the database the store is connected to, in which
// every project's schema is made: what a fix that grants the right to
// make them names.
func (c *Control) Database(ctx context.Context) (string, error) {
	var db string
	if err := c.s.r.QueryRowContext(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		return "", fmt.Errorf("read the database: %w", err)
	}
	return db, nil
}

// Account returns the role the store is connected as, as a statement
// names it.
func (c *Control) Account(ctx context.Context) (string, error) {
	var role string
	if err := c.s.r.QueryRowContext(ctx, `SELECT current_user`).Scan(&role); err != nil {
		return "", fmt.Errorf("read the role: %w", err)
	}
	return role, nil
}

// ProjectNamed returns the project with the given name, in whatever
// state, from memory.
func (c *Control) ProjectNamed(name string) (Project, bool) {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	for _, p := range c.ix.projects {
		if p.Name == name {
			return p, true
		}
	}
	return Project{}, false
}

// Reaches reports whether actor may manage principal's keys and state,
// by the rule the writes apply (control_authz.go): a server admin
// reaches everyone, and an active project admin a principal that is no
// server admin's, belongs to at least one project, and belongs only to
// projects the admin administers. It reads memory, so the daemon can
// decide what of a principal to show an admin. It does not apply
// decision 19: an admin reaches themselves here, and may still not
// change their own keys.
func (c *Control) Reaches(actor, principal string) bool {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	if c.serverAdmin(actor) {
		return true
	}
	return c.ix.reaches(actor, c.ix.projectsOf(principal)) && !c.ConfigAdmin(principal)
}

// AdministersProjectOf reports whether actor is an active admin of an
// active project that principal belongs to. Such an admin may see
// principal's keys, since each of them signs in to that project
// (decision 36), though only one with reach ([Control.Reaches]) may
// change them. It does not count a server admin's rights: the caller
// asks [Control.IsServerAdmin] too. It reads memory.
func (c *Control) AdministersProjectOf(actor, principal string) bool {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	if !c.ix.active[actor] {
		return false
	}
	for m := range c.ix.roles {
		if m.principal == principal && c.ix.projects[m.project].State == ProjectActive &&
			c.ix.roles[membership{actor, m.project}] == RoleAdmin {
			return true
		}
	}
	return false
}

// ReachGained returns the project admins who can manage principal's keys
// and state now and could not before it left the project left, by name:
// those who administer every project it still belongs to and do not
// administer left. A membership that ends can bring a principal within
// reach of the admins of what remains; whoever ends it should be told.
//
// It returns none for a server admin, who is outside every project
// admin's reach, and for a principal left in no project, who is too. It
// never names the principal itself.
func (c *Control) ReachGained(principal, left string) []string {
	if c.ConfigAdmin(principal) {
		return nil
	}
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	of := c.ix.projectsOf(principal)
	var out []string
	for m, role := range c.ix.roles {
		// Each admin is met once: at their role in the first remaining
		// project, which they must administer to reach the principal.
		if len(of) == 0 || m.project != of[0] || role != RoleAdmin || m.principal == principal || c.ConfigAdmin(m.principal) {
			continue
		}
		if c.ix.reaches(m.principal, of) && c.ix.roles[membership{m.principal, left}] != RoleAdmin {
			out = append(out, m.principal)
		}
	}
	slices.Sort(out)
	return out
}

// projectsOf returns the projects principal belongs to, sorted. The
// caller holds ix.mu.
func (ix *index) projectsOf(principal string) []string {
	var out []string
	for m := range ix.roles {
		if m.principal == principal {
			out = append(out, m.project)
		}
	}
	slices.Sort(out)
	return out
}

// reaches reports whether admin, who is no server admin, is active and
// administers every one of projects, of which there must be one. The
// caller holds ix.mu.
func (ix *index) reaches(admin string, projects []string) bool {
	if len(projects) == 0 || !ix.active[admin] {
		return false
	}
	for _, p := range projects {
		if ix.roles[membership{admin, p}] != RoleAdmin {
			return false
		}
	}
	return true
}
