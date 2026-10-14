package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ariesworx/starfix/internal/lazyregexp"
)

// ProjectState is where a project is in its life.
type ProjectState string

// Project states. A project is added as creating, before its database
// exists; it is active once the database is migrated; and an archived
// project is kept, with its database, and served to no one until it is
// activated again.
const (
	ProjectCreating ProjectState = "creating"
	ProjectActive   ProjectState = "active"
	ProjectArchived ProjectState = "archived"
)

// Event operations on projects. Each one's target is the project's id
// and its after state the project.
const (
	OpProjectAdd      Op = "project.add"
	OpProjectActivate Op = "project.activate"
	OpProjectArchive  Op = "project.archive"
)

// Project is one project on the server and the Dolt database that holds
// it, as the control store reads it back: times in UTC to the
// microsecond.
type Project struct {
	// ID is the project's lowercase UUID, as .starfix.yaml names it.
	ID string `json:"id"`
	// Name is a code name, unique on the server.
	Name string `json:"name"`
	// Prefix starts the project's issue ids.
	Prefix string `json:"prefix"`
	// Account is what the project's issues report against by default.
	Account string `json:"account"`
	// DBName is the project's Dolt database, which no other project has.
	DBName    string       `json:"db_name"`
	State     ProjectState `json:"state"`
	CreatedBy string       `json:"created_by"`
	CreatedAt time.Time    `json:"created_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Rev       Rev          `json:"rev"`
}

// NewProject is the input to [Control.AddProject]; every field is
// required.
type NewProject struct {
	ID      string
	Name    string
	Prefix  string
	Account string
	DBName  string
}

var (
	// projectIDPattern is a lowercase UUID.
	projectIDPattern = lazyregexp.New(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// dbNamePattern is deliberately narrower than what Dolt allows: the
	// name is later written into CREATE DATABASE, which takes no
	// placeholder, so it admits nothing that needs quoting.
	dbNamePattern = lazyregexp.New(`^[a-z][a-z0-9_]{0,63}$`)
)

// validProjectID refuses, with ErrInvalid, an id that is not a lowercase
// UUID.
func validProjectID(id string) error {
	if !projectIDPattern.MatchString(id) {
		return fmt.Errorf("%w: project id %q must be a lowercase UUID", ErrInvalid, id)
	}
	return nil
}

// ValidDBName reports whether n may be a project's database name: a
// lowercase letter, then lowercase letters, digits and underscores, at
// most 64 in all.
func ValidDBName(n string) bool { return dbNamePattern.MatchString(n) }

// validate refuses, with ErrInvalid, a project whose id is not a
// lowercase UUID, whose name or account is not a code name
// ([ValidAccount]), whose prefix is not an issue id prefix, or whose
// database name is not [ValidDBName].
func (p NewProject) validate() error {
	switch {
	case validProjectID(p.ID) != nil:
		return validProjectID(p.ID)
	case !ValidAccount(p.Name):
		return fmt.Errorf("%w: project name %q must be a code name: 1-64 lowercase letters, digits and inner hyphens", ErrInvalid, p.Name)
	case len(p.Prefix) > 32 || !prefixPattern.MatchString(p.Prefix):
		return fmt.Errorf("%w: prefix %q must be lowercase letters, digits and inner hyphens, starting with a letter, at most 32", ErrInvalid, p.Prefix)
	case !ValidAccount(p.Account):
		return fmt.Errorf("%w: account %q must be a code name: 1-64 lowercase letters, digits and inner hyphens", ErrInvalid, p.Account)
	case !ValidDBName(p.DBName):
		return fmt.Errorf("%w: database name %q must be a lowercase letter, then lowercase letters, digits and underscores, at most 64", ErrInvalid, p.DBName)
	}
	return nil
}

// AddProject records a new project in state creating: the first step of
// creating one, before its database exists ([Control.ActivateProject]
// is the last). Only a server admin may.
//
// It is safe to run again: given the id of a project that exists and
// the same name, prefix, account and database, it returns that project
// as it is, in whatever state, and writes nothing. It refuses, with a
// [*TakenError], an id that exists with other settings, and a name or a
// database another project has; and with a [*ControlLimitError] a
// project past ControlLimits.Projects.
func (c *Control) AddProject(ctx context.Context, actor Actor, in NewProject) (Project, error) {
	if err := in.validate(); err != nil {
		return Project{}, err
	}
	var out Project
	err := c.write(ctx, actor, func(w *cwtx) error {
		if err := w.serverOnly(OpProjectAdd, "", in.ID); err != nil {
			return err
		}
		cur, err := readProject(ctx, w.tx, in.ID)
		switch {
		case err == nil:
			if cur.Name != in.Name || cur.Prefix != in.Prefix || cur.Account != in.Account || cur.DBName != in.DBName {
				return &TakenError{Kind: TakenProjectID, Value: in.ID}
			}
			out = cur
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		for _, u := range []struct {
			kind  TakenKind
			query string
			value string
		}{
			{TakenProjectName, `SELECT COUNT(*) FROM projects WHERE name = ?`, in.Name},
			{TakenDBName, `SELECT COUNT(*) FROM projects WHERE db_name = ?`, in.DBName},
		} {
			var n int
			if err := w.tx.QueryRowContext(ctx, u.query, u.value).Scan(&n); err != nil {
				return fmt.Errorf("add project %s: %w", in.ID, err)
			}
			if n > 0 {
				return &TakenError{Kind: u.kind, Value: u.value}
			}
		}
		if err := w.room(ctx, limitProjects, w.lim.Projects, `SELECT COUNT(*) FROM projects`); err != nil {
			return err
		}
		out = Project{ID: in.ID, Name: in.Name, Prefix: in.Prefix, Account: in.Account, DBName: in.DBName,
			State: ProjectCreating, CreatedBy: w.actor.Principal, CreatedAt: w.now, UpdatedAt: w.now, Rev: 1}
		if _, err := w.exec(ctx, `INSERT INTO projects
  (id, name, prefix, account, db_name, state, created_by, created_at, updated_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
			out.ID, out.Name, out.Prefix, out.Account, out.DBName, string(out.State), out.CreatedBy, w.now, w.now, randomInt63()); err != nil {
			return fmt.Errorf("add project %s: %w", in.ID, err)
		}
		added := out
		w.indexed(func(ix *index) { ix.projects[added.ID] = added })
		return w.event(ctx, OpProjectAdd, out.ID, nil, out)
	})
	if err != nil {
		return Project{}, err
	}
	return out, nil
}

// ActivateProject marks a project active: once its database is created
// and migrated, or again after it was archived (decision 18), which the
// event's before state shows. Only a server admin may. A project already
// active is returned as it is, so creating one can be run again.
func (c *Control) ActivateProject(ctx context.Context, actor Actor, id string) (Project, error) {
	return c.setProjectState(ctx, actor, id, OpProjectActivate, ProjectActive)
}

// ArchiveProject marks a project archived, whether it was active or
// never finished being created. Only a server admin may. Its database,
// members and name are kept. A project already archived is returned as
// it is.
func (c *Control) ArchiveProject(ctx context.Context, actor Actor, id string) (Project, error) {
	return c.setProjectState(ctx, actor, id, OpProjectArchive, ProjectArchived)
}

// setProjectState moves a project to state to, recording op.
func (c *Control) setProjectState(ctx context.Context, actor Actor, id string, op Op, to ProjectState) (Project, error) {
	if err := validProjectID(id); err != nil {
		return Project{}, err
	}
	var out Project
	err := c.write(ctx, actor, func(w *cwtx) error {
		if err := w.serverOnly(op, "", id); err != nil {
			return err
		}
		cur, err := readProject(ctx, w.tx, id)
		if err != nil {
			return err
		}
		out = cur
		if cur.State == to {
			return nil
		}
		n, err := w.exec(ctx, `UPDATE projects SET state = ?, updated_at = ?, rev = rev + 1, write_id = ? WHERE id = ? AND rev = ?`,
			string(to), w.now, randomInt63(), id, cur.Rev)
		if err != nil {
			return fmt.Errorf("%s %s: %w", op, id, err)
		}
		if n == 0 {
			return fmt.Errorf("%w: project %s changed during %s", errRetry, id, op)
		}
		out.State, out.UpdatedAt, out.Rev = to, w.now, cur.Rev+1
		moved := out
		w.indexed(func(ix *index) { ix.projects[id] = moved })
		return w.event(ctx, op, id, cur, out)
	})
	if err != nil {
		return Project{}, err
	}
	return out, nil
}

// Project returns the project with the given id, or ErrNotFound. Reads
// are not guarded: the caller decides who may see a project.
func (c *Control) Project(ctx context.Context, id string) (Project, error) {
	if err := validProjectID(id); err != nil {
		return Project{}, err
	}
	return readProject(ctx, c.s.r, id)
}

// Projects returns every project, in every state, by name. Reads are
// not guarded: the caller decides who may list them.
func (c *Control) Projects(ctx context.Context) ([]Project, error) {
	rows, err := c.s.r.QueryContext(ctx, projectSelect+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("projects: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}
	return out, nil
}

const projectSelect = `SELECT id, name, prefix, account, db_name, state, created_by, created_at, updated_at, rev FROM projects`

func scanProject(r scanner) (Project, error) {
	var p Project
	if err := r.Scan(&p.ID, &p.Name, &p.Prefix, &p.Account, &p.DBName, &p.State, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.Rev); err != nil {
		return Project{}, err
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return p, nil
}

// readProject reads one project, or returns ErrNotFound.
func readProject(ctx context.Context, q querier, id string) (Project, error) {
	p, err := scanProject(q.QueryRowContext(ctx, projectSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("project %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Project{}, fmt.Errorf("project %s: %w", id, err)
	}
	return p, nil
}
