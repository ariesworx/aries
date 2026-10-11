package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Role is what a principal is in a project.
type Role string

// Roles. A member works in the project; an admin also manages its
// members and acts as an admin of its issues.
const (
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == RoleMember || r == RoleAdmin }

// Event operations on memberships. Each one's target is
// PROJECT/PRINCIPAL. member.set's after state is the membership, with a
// before state when it changed a role; member.remove's before state is
// the membership removed.
const (
	OpMemberSet    Op = "member.set"
	OpMemberRemove Op = "member.remove"
)

// Member is one principal's role in one project.
type Member struct {
	Principal string `json:"principal"`
	// Project is the project's id.
	Project   string    `json:"project"`
	Role      Role      `json:"role"`
	AddedBy   string    `json:"added_by"`
	AddedAt   time.Time `json:"added_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Rev       Rev       `json:"rev"`
}

// memberTarget is the event target of a membership.
func memberTarget(principal, project string) string { return project + "/" + principal }

// validMember refuses, with ErrInvalid, a bad principal name or project
// id.
func validMember(principal, project string) error {
	if err := validPrincipal(principal); err != nil {
		return err
	}
	return validProjectID(project)
}

// SetMember gives a principal a role in a project: it adds the
// membership, or changes its role. A server admin may in any project; a
// project admin in the projects they administer.
//
// The principal must exist and be active (a disabled one is refused
// with ErrInvalid), and the project must exist; either missing is
// ErrNotFound. A membership that already has the role is returned as it
// is, and a new one past ControlLimits.Members is refused with a
// [*ControlLimitError].
func (c *Control) SetMember(ctx context.Context, actor Actor, principal, project string, role Role) (Member, error) {
	if err := validMember(principal, project); err != nil {
		return Member{}, err
	}
	if !role.Valid() {
		return Member{}, fmt.Errorf("%w: role %q must be %s or %s", ErrInvalid, role, RoleMember, RoleAdmin)
	}
	var out Member
	err := c.write(ctx, actor, func(w *cwtx) error {
		if err := w.projectAdmin(ctx, OpMemberSet, principal, project); err != nil {
			return err
		}
		p, err := readPrincipal(ctx, w.tx, principal)
		if err != nil {
			return err
		}
		if p.State != PrincipalActive {
			return fmt.Errorf("%w: principal %s is disabled", ErrInvalid, principal)
		}
		if _, err := readProject(ctx, w.tx, project); err != nil {
			return err
		}
		cur, err := readMember(ctx, w.tx, principal, project)
		switch {
		case errors.Is(err, ErrNotFound):
			if err := w.memberRoom(ctx, project); err != nil {
				return err
			}
			out, err = w.addMember(ctx, principal, project, role)
			return err
		case err != nil:
			return err
		}
		out = cur
		if cur.Role == role {
			return nil
		}
		n, err := w.exec(ctx, `UPDATE members SET role = ?, updated_at = ?, rev = rev + 1, write_id = ?
  WHERE principal = ? AND project = ? AND rev = ?`, string(role), w.now, randomInt63(), principal, project, cur.Rev)
		if err != nil {
			return fmt.Errorf("set member %s of %s: %w", principal, project, err)
		}
		if n == 0 {
			return fmt.Errorf("%w: member %s of %s changed during the write", errRetry, principal, project)
		}
		out.Role, out.UpdatedAt, out.Rev = role, w.now, cur.Rev+1
		return w.event(ctx, OpMemberSet, memberTarget(principal, project), cur, out)
	})
	if err != nil {
		return Member{}, err
	}
	return out, nil
}

// memberRoom refuses a new member of project past ControlLimits.Members.
func (w *cwtx) memberRoom(ctx context.Context, project string) error {
	return w.room(ctx, limitMembers, w.lim.Members, `SELECT COUNT(*) FROM members WHERE project = ?`, project)
}

// addMember inserts a membership and records it. The caller has checked
// the actor, the principal, the project and memberRoom.
func (w *cwtx) addMember(ctx context.Context, principal, project string, role Role) (Member, error) {
	m := Member{Principal: principal, Project: project, Role: role, AddedBy: w.actor.Principal, AddedAt: w.now, UpdatedAt: w.now, Rev: 1}
	if _, err := w.exec(ctx, `INSERT INTO members (principal, project, role, added_by, added_at, updated_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, ?, 1, ?)`, principal, project, string(role), m.AddedBy, w.now, w.now, randomInt63()); err != nil {
		return Member{}, fmt.Errorf("add member %s to %s: %w", principal, project, err)
	}
	return m, w.event(ctx, OpMemberSet, memberTarget(principal, project), nil, m)
}

// RemoveMember ends a principal's membership of a project and returns
// it. Who may is as for [Control.SetMember]. A principal that is not a
// member is ErrNotFound.
func (c *Control) RemoveMember(ctx context.Context, actor Actor, principal, project string) (Member, error) {
	if err := validMember(principal, project); err != nil {
		return Member{}, err
	}
	var out Member
	err := c.write(ctx, actor, func(w *cwtx) error {
		if err := w.projectAdmin(ctx, OpMemberRemove, principal, project); err != nil {
			return err
		}
		cur, err := readMember(ctx, w.tx, principal, project)
		if err != nil {
			return err
		}
		if _, err := w.exec(ctx, `DELETE FROM members WHERE principal = ? AND project = ?`, principal, project); err != nil {
			return fmt.Errorf("remove member %s from %s: %w", principal, project, err)
		}
		out = cur
		return w.event(ctx, OpMemberRemove, memberTarget(principal, project), cur, nil)
	})
	if err != nil {
		return Member{}, err
	}
	return out, nil
}

// Members returns a project's members, by principal; none for a project
// that has none or does not exist. Reads are not guarded: the caller
// decides who may list them.
func (c *Control) Members(ctx context.Context, project string) ([]Member, error) {
	if err := validProjectID(project); err != nil {
		return nil, err
	}
	return c.members(ctx, ` WHERE project = ? ORDER BY principal`, project)
}

// ProjectsOf returns a principal's memberships, by project id, whatever
// the state of the principal or of the projects; none for a principal
// that has none or does not exist. Reads are not guarded: the caller
// decides who may list them.
func (c *Control) ProjectsOf(ctx context.Context, principal string) ([]Member, error) {
	if err := validPrincipal(principal); err != nil {
		return nil, err
	}
	return c.members(ctx, ` WHERE principal = ? ORDER BY project`, principal)
}

// Role returns the role principal has in project; ok is false when it is
// not a member. It reports the membership as stored: it does not say
// whether the principal is active or the project is, nor that a server
// admin acts as an admin everywhere ([Control.IsAdminOf] does).
func (c *Control) Role(ctx context.Context, principal, project string) (role Role, ok bool, err error) {
	if err := validMember(principal, project); err != nil {
		return "", false, err
	}
	m, err := readMember(ctx, c.s.r, principal, project)
	if errors.Is(err, ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return m.Role, true, nil
}

const memberSelect = `SELECT principal, project, role, added_by, added_at, updated_at, rev FROM members`

// members reads the memberships that where, a constant clause with a
// placeholder for arg, selects.
func (c *Control) members(ctx context.Context, where, arg string) ([]Member, error) {
	rows, err := c.s.r.QueryContext(ctx, memberSelect+where, arg) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, fmt.Errorf("members: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("members: %w", err)
	}
	return out, nil
}

func scanMember(r scanner) (Member, error) {
	var m Member
	if err := r.Scan(&m.Principal, &m.Project, &m.Role, &m.AddedBy, &m.AddedAt, &m.UpdatedAt, &m.Rev); err != nil {
		return Member{}, err
	}
	m.AddedAt, m.UpdatedAt = m.AddedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

// readMember reads one membership, or returns ErrNotFound.
func readMember(ctx context.Context, q querier, principal, project string) (Member, error) {
	m, err := scanMember(q.QueryRowContext(ctx, memberSelect+` WHERE principal = ? AND project = ?`, principal, project))
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, fmt.Errorf("member %s of project %s: %w", principal, project, ErrNotFound)
	}
	if err != nil {
		return Member{}, fmt.Errorf("member %s of project %s: %w", principal, project, err)
	}
	return m, nil
}
