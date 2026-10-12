package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// What `sfx admin` needs of the control store beyond its rows: a
// project's database created, and the questions the daemon asks before
// it answers an admin (which project a name means, who is within whose
// reach).

// CreateDatabase creates the Dolt database of a project still being
// created, the step between [Control.AddProject] and
// [Control.ActivateProject] (design §15.5, 1). Only a server admin may.
// It is safe to repeat: a database that exists is left as it is.
//
// The statement runs on the control store's writer connection, because
// Dolt checks CREATE DATABASE against the connection's current database,
// on which this account has every right. That the account can then use
// the new database is another matter: it can only if Dolt's root granted
// it that exact name, which [Open] finds out ([DatabaseDenied]).
//
// It refuses a project that does not exist with ErrNotFound, and one
// that is not being created with ErrInvalid.
func (c *Control) CreateDatabase(ctx context.Context, actor Actor, project string) error {
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
		return fmt.Errorf("%w: project %s is %s; only a project being created has a database to create", ErrInvalid, project, p.State)
	}
	// The row's name passed this check when it was written; it is
	// checked again here because the statement takes no placeholder.
	if !ValidDBName(p.DBName) {
		return fmt.Errorf("%w: database name %q of project %s", ErrInvalid, p.DBName, project)
	}
	// Like BackupTag, this holds the writer connection outside
	// Store.write: it changes no table of the control database, so there
	// is no transaction to make and no event to record. The project's
	// own events say that it was added and activated.
	conn, err := c.s.w.Conn(ctx)
	if err != nil {
		return fmt.Errorf("create database %s: %w", p.DBName, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+p.DBName); err != nil {
		return fmt.Errorf("create database %s: %w", p.DBName, err)
	}
	return nil
}

// DatabaseDenied reports whether err is Dolt refusing the account a
// database: one that root never granted it by name, whether or not the
// database exists.
func DatabaseDenied(err error) bool {
	me, ok := errors.AsType[*mysql.MySQLError](err)
	// Dolt reports it as 1105, its general error, where MySQL has 1044.
	return ok && (me.Number == 1105 || me.Number == 1044) && strings.HasPrefix(me.Message, "Access denied for user")
}

// Account returns the Dolt account the store is connected as, in the
// form a GRANT statement names it: 'user'@'host'.
func (c *Control) Account(ctx context.Context) (string, error) {
	var current string
	if err := c.s.r.QueryRowContext(ctx, `SELECT CURRENT_USER()`).Scan(&current); err != nil {
		return "", fmt.Errorf("read the account: %w", err)
	}
	user, host, _ := strings.Cut(current, "@")
	return "'" + user + "'@'" + host + "'", nil
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
