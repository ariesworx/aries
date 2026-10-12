package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PrincipalState says whether a principal's keys and memberships work.
type PrincipalState string

// Principal states. A disabled principal keeps its keys and memberships,
// and none of them work until it is enabled.
const (
	PrincipalActive   PrincipalState = "active"
	PrincipalDisabled PrincipalState = "disabled"
)

// Event operations on principals. Each one's target is the principal's
// name and its after state the principal.
const (
	OpPrincipalAdd     Op = "principal.add"
	OpPrincipalDisable Op = "principal.disable"
	OpPrincipalEnable  Op = "principal.enable"
)

// Principal is one identity on the server: a person, or an agent or
// service with keys of its own. Names are one namespace for the server.
type Principal struct {
	Name      string         `json:"name"`
	State     PrincipalState `json:"state"`
	CreatedBy string         `json:"created_by"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Rev       Rev            `json:"rev"`
}

// validPrincipal refuses, with ErrInvalid, a name that does not match
// [PrincipalPattern] or is [Reserved].
func validPrincipal(name string) error {
	if !PrincipalPattern.MatchString(name) {
		return fmt.Errorf("%w: principal %q must be a lowercase letter, then up to 63 lowercase letters, digits, dots, hyphens and underscores", ErrInvalid, name)
	}
	if Reserved(name) {
		return fmt.Errorf("%w: principal %q is a name the server uses itself", ErrInvalid, name)
	}
	return nil
}

// AddPrincipal adds an active principal, with no keys. With a project
// it also makes the principal a member of it (role member), in the same
// write.
//
// A server admin may add any, in a project or in none. A project admin
// must name a project they administer, and may not take a server
// admin's name. A name in use is refused with a [*TakenError], which
// tells the caller that it exists; a project that does not exist with
// ErrNotFound; and a row past ControlLimits.Principals or
// ControlLimits.Members with a [*ControlLimitError].
func (c *Control) AddPrincipal(ctx context.Context, actor Actor, name, project string) (Principal, error) {
	if err := validPrincipal(name); err != nil {
		return Principal{}, err
	}
	if project != "" {
		if err := validProjectID(project); err != nil {
			return Principal{}, err
		}
	}
	var out Principal
	err := c.write(ctx, actor, func(w *cwtx) error {
		if !w.admin {
			if project == "" {
				return w.denied(OpPrincipalAdd, name, "", AccessServerAdmin)
			}
			if err := w.projectAdmin(ctx, OpPrincipalAdd, name, project); err != nil {
				return err
			}
			// A project admin who could create the principal a server
			// admin is named for could give it a key and be one.
			if w.admins(name) {
				return w.denied(OpPrincipalAdd, name, project, AccessServerAdminPrincipal)
			}
		}
		switch _, err := readPrincipal(ctx, w.tx, name); {
		case err == nil:
			return &TakenError{Kind: TakenPrincipal, Value: name}
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if project != "" {
			if _, err := readProject(ctx, w.tx, project); err != nil {
				return err
			}
		}
		if err := w.room(ctx, limitPrincipals, w.lim.Principals, `SELECT COUNT(*) FROM principals`); err != nil {
			return err
		}
		if project != "" {
			if err := w.memberRoom(ctx, project); err != nil {
				return err
			}
		}
		out = Principal{Name: name, State: PrincipalActive, CreatedBy: w.actor.Principal, CreatedAt: w.now, UpdatedAt: w.now, Rev: 1}
		if _, err := w.exec(ctx, `INSERT INTO principals (name, state, created_by, created_at, updated_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, 1, ?)`, name, string(out.State), out.CreatedBy, w.now, w.now, randomInt63()); err != nil {
			return fmt.Errorf("add principal %s: %w", name, err)
		}
		if err := w.event(ctx, OpPrincipalAdd, name, nil, out); err != nil {
			return err
		}
		w.indexed(func(ix *index) { ix.active[name] = true })
		if project == "" {
			return nil
		}
		_, err := w.addMember(ctx, name, project, RoleMember)
		return err
	})
	if err != nil {
		return Principal{}, err
	}
	return out, nil
}

// DisablePrincipal stops a principal's keys and roles from working,
// keeping them: [Control.AuthorizedKey] no longer answers for its keys,
// and it administers nothing. A server admin may disable any principal;
// a project admin only one within their reach, as for its keys
// ([Control.AddKey]). One already disabled is returned as it is.
func (c *Control) DisablePrincipal(ctx context.Context, actor Actor, name string) (Principal, error) {
	return c.setPrincipalState(ctx, actor, name, OpPrincipalDisable, PrincipalDisabled)
}

// EnablePrincipal makes a disabled principal active again, with the
// keys and roles it had. Who may is as for [Control.DisablePrincipal].
// One already active is returned as it is.
func (c *Control) EnablePrincipal(ctx context.Context, actor Actor, name string) (Principal, error) {
	return c.setPrincipalState(ctx, actor, name, OpPrincipalEnable, PrincipalActive)
}

// setPrincipalState moves a principal to state to, recording op.
func (c *Control) setPrincipalState(ctx context.Context, actor Actor, name string, op Op, to PrincipalState) (Principal, error) {
	if err := validPrincipal(name); err != nil {
		return Principal{}, err
	}
	var out Principal
	err := c.write(ctx, actor, func(w *cwtx) error {
		sc, err := w.scope(ctx, op, name)
		if err != nil {
			return err
		}
		cur, err := readPrincipal(ctx, w.tx, name)
		if err != nil {
			return err
		}
		if err := w.within(ctx, sc, op, name); err != nil {
			return err
		}
		out = cur
		if cur.State == to {
			return nil
		}
		n, err := w.exec(ctx, `UPDATE principals SET state = ?, updated_at = ?, rev = rev + 1, write_id = ? WHERE name = ? AND rev = ?`,
			string(to), w.now, randomInt63(), name, cur.Rev)
		if err != nil {
			return fmt.Errorf("%s %s: %w", op, name, err)
		}
		if n == 0 {
			return fmt.Errorf("%w: principal %s changed during %s", errRetry, name, op)
		}
		out.State, out.UpdatedAt, out.Rev = to, w.now, cur.Rev+1
		w.indexed(func(ix *index) {
			if to == PrincipalActive {
				ix.active[name] = true
			} else {
				delete(ix.active, name)
			}
		})
		return w.event(ctx, op, name, cur, out)
	})
	if err != nil {
		return Principal{}, err
	}
	return out, nil
}

// Principal returns the principal with the given name, or ErrNotFound.
// Reads are not guarded: the caller decides who may see a principal.
func (c *Control) Principal(ctx context.Context, name string) (Principal, error) {
	if err := validPrincipal(name); err != nil {
		return Principal{}, err
	}
	return readPrincipal(ctx, c.s.r, name)
}

// Principals returns every principal, disabled ones included, by name.
// Reads are not guarded: the caller decides who may list them.
func (c *Control) Principals(ctx context.Context) ([]Principal, error) {
	rows, err := c.s.r.QueryContext(ctx, principalSelect+` ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("principals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Principal
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, fmt.Errorf("principals: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("principals: %w", err)
	}
	return out, nil
}

const principalSelect = `SELECT name, state, created_by, created_at, updated_at, rev FROM principals`

func scanPrincipal(r scanner) (Principal, error) {
	var p Principal
	if err := r.Scan(&p.Name, &p.State, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.Rev); err != nil {
		return Principal{}, err
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return p, nil
}

// readPrincipal reads one principal, or returns ErrNotFound.
func readPrincipal(ctx context.Context, q querier, name string) (Principal, error) {
	p, err := scanPrincipal(q.QueryRowContext(ctx, principalSelect+` WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, fmt.Errorf("principal %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("principal %s: %w", name, err)
	}
	return p, nil
}
