package store

import (
	"context"
	"fmt"
	"slices"
)

// Who may write to the control store (design §15.3, decisions 14 and 15).
// Each write is checked inside its transaction, before it writes
// anything, so a refusal records nothing and the check sees the roles as
// they are when the write commits.
//
//   - A server admin (ControlOptions.Admins) may make every change.
//   - A project admin (role admin on the project, while active) may set
//     and remove that project's members and their roles, and may create
//     a principal as a member of a project they administer. They may
//     add to it only a principal that already belongs to a project, or
//     that they created (decision 17): an unassigned principal someone
//     else created is placed by a server admin, or a project admin could
//     bring it within their reach, below, and manage its keys.
//   - A project admin may add or remove a principal's keys, or disable
//     or enable it, only while every project the principal belongs to is
//     one they administer. A principal in no project is outside every
//     project admin's reach, and so is a server admin. Otherwise a
//     project admin could add a key of their own to someone with access
//     elsewhere, and act as them there.
//   - Only a server admin adds, activates or archives a project.
//   - Anyone else is refused.

// AccessReason says which rule an [*AccessError] applied.
type AccessReason string

// The reasons an [*AccessError] gives.
const (
	// AccessServerAdmin: the change is for server admins only, such as
	// adding a project, or a principal that belongs to no project.
	AccessServerAdmin AccessReason = "server admin"
	// AccessAdmin: the actor is not an admin of Project, or, when the
	// error names no project, of any project.
	AccessAdmin AccessReason = "admin"
	// AccessReach: the actor is a project admin, and Principal belongs
	// to a project the actor does not administer, or to none.
	AccessReach AccessReason = "reach"
	// AccessServerAdminPrincipal: the actor is a project admin, and
	// Principal is a server admin, whose keys and state only a server
	// admin changes.
	AccessServerAdminPrincipal AccessReason = "server admin principal"
	// AccessUnassigned: the actor is a project admin adding Principal to
	// Project, and Principal belongs to no project and was not created
	// by the actor. Only a server admin places such a principal: a
	// project admin who could would bring it within reach and manage
	// its keys (decision 17).
	AccessUnassigned AccessReason = "unassigned"
)

// AccessError refuses a change to the control store that Actor, the
// acting principal, may not make. Op is the change, Principal and
// Project what it was to change, where it names them, and Reason the
// rule. It names no project or principal the request did not. It wraps
// ErrForbidden.
type AccessError struct {
	Actor     string
	Op        Op
	Principal string
	Project   string
	Reason    AccessReason
}

func (e *AccessError) Error() string {
	what := string(e.Op)
	switch {
	case e.Principal != "" && e.Project != "":
		what += " " + e.Principal + " in project " + e.Project
	case e.Principal != "":
		what += " " + e.Principal
	case e.Project != "":
		what += " " + e.Project
	}
	why := "it is for server admins"
	switch e.Reason {
	case AccessServerAdmin:
	case AccessAdmin:
		why = "it is for project admins and server admins"
		if e.Project != "" {
			why = "it is for the project's admins and server admins"
		}
	case AccessReach:
		why = fmt.Sprintf("%s belongs to a project %s does not administer, or to none, so it is for server admins", e.Principal, e.Actor)
	case AccessServerAdminPrincipal:
		why = e.Principal + " is a server admin, so it is for server admins"
	case AccessUnassigned:
		why = fmt.Sprintf("%s belongs to no project and %s did not create it, so it is for server admins", e.Principal, e.Actor)
	}
	return fmt.Sprintf("%s may not %s: %s", e.Actor, what, why)
}

// Unwrap makes errors.Is(err, ErrForbidden) hold.
func (e *AccessError) Unwrap() error { return ErrForbidden }

// IsServerAdmin reports whether principal is one of the server admins
// (ControlOptions.Admins). It reads no database.
func (c *Control) IsServerAdmin(principal string) bool { return c.s.IsAdmin(principal) }

// IsAdminOf reports whether principal may act as an admin in project: it
// is a server admin, or it is active and its role there is admin. It is
// the check for a handshake and for an issue store's admin rights.
func (c *Control) IsAdminOf(ctx context.Context, principal, project string) (bool, error) {
	if c.IsServerAdmin(principal) {
		return true, nil
	}
	projects, err := administered(ctx, c.s.r, principal)
	if err != nil {
		return false, err
	}
	return slices.Contains(projects, project), nil
}

// administered returns the projects principal is an admin of. A disabled
// principal administers none.
func administered(ctx context.Context, q querier, principal string) ([]string, error) {
	projects, err := queryStrings(ctx, q, `SELECT m.project FROM members m JOIN principals p ON p.name = m.principal
  WHERE m.principal = ? AND m.role = ? AND p.state = ?`, principal, string(RoleAdmin), string(PrincipalActive))
	if err != nil {
		return nil, fmt.Errorf("read projects administered by %s: %w", principal, err)
	}
	return projects, nil
}

// denied is the refusal of op on principal and project, either of which
// may be empty, for reason.
func (w *cwtx) denied(op Op, principal, project string, reason AccessReason) error {
	return &AccessError{Actor: w.actor.Principal, Op: op, Principal: principal, Project: project, Reason: reason}
}

// serverOnly refuses op to anyone but a server admin.
func (w *cwtx) serverOnly(op Op, principal, project string) error {
	if w.admin {
		return nil
	}
	return w.denied(op, principal, project, AccessServerAdmin)
}

// projectAdmin refuses op to anyone but an admin of project or a server
// admin. A project that does not exist has no admins, so the refusal
// does not say whether it exists.
func (w *cwtx) projectAdmin(ctx context.Context, op Op, principal, project string) error {
	if w.admin {
		return nil
	}
	projects, err := administered(ctx, w.tx, w.actor.Principal)
	if err != nil {
		return err
	}
	if !slices.Contains(projects, project) {
		return w.denied(op, principal, project, AccessAdmin)
	}
	return nil
}

// scope is how far an actor's rights over principals go: every principal
// for a server admin, and for a project admin those whose projects are
// all among projects.
type scope struct {
	server   bool
	projects []string
}

// scope returns the actor's reach, refusing op (on principal, when the
// request names one) to an actor who administers nothing.
func (w *cwtx) scope(ctx context.Context, op Op, principal string) (scope, error) {
	if w.admin {
		return scope{server: true}, nil
	}
	projects, err := administered(ctx, w.tx, w.actor.Principal)
	if err != nil {
		return scope{}, err
	}
	if len(projects) == 0 {
		return scope{}, w.denied(op, principal, "", AccessAdmin)
	}
	return scope{projects: projects}, nil
}

// within refuses op on principal unless it is within sc: a project
// admin reaches a principal only while every project it belongs to is
// one they administer, and never a server admin or a principal in no
// project.
func (w *cwtx) within(ctx context.Context, sc scope, op Op, principal string) error {
	if sc.server {
		return nil
	}
	if w.admins(principal) {
		return w.denied(op, principal, "", AccessServerAdminPrincipal)
	}
	of, err := queryStrings(ctx, w.tx, `SELECT project FROM members WHERE principal = ?`, principal)
	if err != nil {
		return fmt.Errorf("read projects of %s: %w", principal, err)
	}
	if len(of) == 0 || slices.ContainsFunc(of, func(p string) bool { return !slices.Contains(sc.projects, p) }) {
		return w.denied(op, principal, "", AccessReach)
	}
	return nil
}

// placeable refuses a project admin adding p to project unless p already
// belongs to a project or the actor created it (decision 17).
func (w *cwtx) placeable(ctx context.Context, p Principal, project string) error {
	if w.admin || p.CreatedBy == w.actor.Principal {
		return nil
	}
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM members WHERE principal = ?`, p.Name).Scan(&n); err != nil {
		return fmt.Errorf("read projects of %s: %w", p.Name, err)
	}
	if n == 0 {
		return w.denied(OpMemberSet, p.Name, project, AccessUnassigned)
	}
	return nil
}
