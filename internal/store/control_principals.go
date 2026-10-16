package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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
// name and its after state the principal. principal.set's after state
// also names the fields it set, in "fields".
const (
	OpPrincipalAdd     Op = "principal.add"
	OpPrincipalDisable Op = "principal.disable"
	OpPrincipalEnable  Op = "principal.enable"
	OpPrincipalSet     Op = "principal.set"
)

// PrincipalKind says what a principal is (docs/design/identity.md
// §3.1).
type PrincipalKind string

// Principal kinds. A person may have identities to sign in with; a
// service never does, and answers to an owner, a person.
const (
	KindPerson  PrincipalKind = "person"
	KindService PrincipalKind = "service"
)

// Principal is one identity on the server: a person, or an agent or
// service with keys of its own. Names are one namespace for the server.
//
// DisplayName is what people see beside the name. It is never in an
// event (no JSON): the log is kept for good, and a name is personal, so
// an event that changes it says only that it changed.
type Principal struct {
	Name        string         `json:"name"`
	State       PrincipalState `json:"state"`
	Kind        PrincipalKind  `json:"kind"`
	Owner       string         `json:"owner,omitempty"`
	DisplayName string         `json:"-"`
	CreatedBy   string         `json:"created_by"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Rev         Rev            `json:"rev"`
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

// NewPrincipal is a principal to add, with what is added with it.
type NewPrincipal struct {
	Name string
	// Project, when set, is the project the principal joins as a member.
	Project string
	// Keys are its first public keys, each one line as [Control.AddKey]
	// takes it.
	Keys []string
	// Kind is a person when empty. A service needs an Owner, who is a
	// person; a person has none.
	Kind  PrincipalKind
	Owner string
	// DisplayName is what people see beside the name; empty for none.
	DisplayName string
	// Invite, when set, invites an address for the principal, as
	// [Control.Invite] does, which only a server admin may.
	Invite *Invitation
}

// AddPrincipal adds an active person, as [Control.AddNewPrincipal] does,
// with only a name, a project and keys.
func (c *Control) AddPrincipal(ctx context.Context, actor Actor, name, project string, keys ...string) (Principal, error) {
	return c.AddNewPrincipal(ctx, actor, NewPrincipal{Name: name, Project: project, Keys: keys})
}

// AddNewPrincipal adds an active principal. With a project it also makes
// the principal a member of it (role member), with keys, each one line
// as [Control.AddKey] takes it, it adds those too, and with an
// invitation it invites that address for it, all in the same write: all
// of it happens, or none. A key given twice is added once.
//
// A server admin may add any, in a project or in none. A project admin
// must name a project they administer, may not take a server admin's
// name, and may not invite. A name in use is refused with a
// [*TakenError], which tells the caller that it exists; a project or an
// owner that does not exist with ErrNotFound; and a row past
// ControlLimits.Principals, ControlLimits.Members, ControlLimits.Keys or
// ControlLimits.Invites, or, for an actor who is no server admin, past
// ControlLimits.PrincipalsPerCreator, with a [*ControlLimitError]. Keys
// are refused as AddKey refuses them and the invitation as Invite
// refuses it, and an actor who names themselves with keys or an
// invitation is refused too (decision 19): a server admin the config
// file names may have no row yet. A service whose owner is no person, a
// person with an owner, and a service with an invitation are refused
// with an [*IdentityError].
func (c *Control) AddNewPrincipal(ctx context.Context, actor Actor, in NewPrincipal) (Principal, error) {
	name, project := in.Name, in.Project
	if err := validPrincipal(name); err != nil {
		return Principal{}, err
	}
	if project != "" {
		if err := validProjectID(project); err != nil {
			return Principal{}, err
		}
	}
	kind := cmp.Or(in.Kind, KindPerson)
	if err := validKind(kind); err != nil {
		return Principal{}, err
	}
	if err := checkDisplayName(in.DisplayName); err != nil {
		return Principal{}, err
	}
	if in.Owner != "" {
		if err := validPrincipal(in.Owner); err != nil {
			return Principal{}, err
		}
	}
	if in.Invite != nil {
		if err := in.Invite.validate(); err != nil {
			return Principal{}, err
		}
	}
	parsed := make([]parsedKey, 0, len(in.Keys))
	for _, line := range in.Keys {
		fp, key, comment, err := parseKey(line)
		if err != nil {
			return Principal{}, err
		}
		parsed = append(parsed, parsedKey{fp, key, comment})
	}
	var out Principal
	err := c.write(ctx, actor, func(w *cwtx) error {
		if len(parsed) > 0 {
			if err := w.ownKeys(OpKeyAdd, name); err != nil {
				return err
			}
		}
		if in.Invite != nil {
			if err := w.ownIdentities(name); err != nil {
				return err
			}
			if err := w.serverOnly(OpIdentityInvite, name, ""); err != nil {
				return err
			}
		}
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
		if err := w.kindRules(ctx, name, kind, in.Owner, in.Invite != nil); err != nil {
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
		if !w.admin {
			// No op deletes a principal, so each creator who is no server
			// admin has an allowance of their own within the server's.
			if err := w.room(ctx, limitPerCreator, w.lim.PrincipalsPerCreator,
				`SELECT COUNT(*) FROM principals WHERE created_by = $1`, w.actor.Principal); err != nil {
				return err
			}
		}
		if project != "" {
			if err := w.memberRoom(ctx, project); err != nil {
				return err
			}
		}
		out = Principal{Name: name, State: PrincipalActive, Kind: kind, Owner: in.Owner, DisplayName: in.DisplayName,
			CreatedBy: w.actor.Principal, CreatedAt: w.now, UpdatedAt: w.now, Rev: 1}
		if _, err := w.exec(ctx, `INSERT INTO principals (name, state, kind, owner, display_name, created_by, created_at, updated_at, rev)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)`, name, string(out.State), string(kind), nullStr(in.Owner), nullStr(in.DisplayName),
			out.CreatedBy, w.now, w.now); err != nil {
			return fmt.Errorf("add principal %s: %w", name, err)
		}
		if err := w.event(ctx, OpPrincipalAdd, name, nil, out); err != nil {
			return err
		}
		w.indexed(func(ix *index) { ix.active[name] = true })
		if project != "" {
			if _, err := w.addMember(ctx, name, project, RoleMember); err != nil {
				return err
			}
		}
		// The principal is new, in the one project its creator
		// administers or the creator is a server admin, so it is within
		// the creator's reach.
		for _, k := range parsed {
			if _, err := w.addKey(ctx, name, k); err != nil {
				return err
			}
		}
		if in.Invite != nil {
			if _, _, err := w.invite(ctx, name, *in.Invite); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Principal{}, err
	}
	return out, nil
}

// DisablePrincipal stops a principal's keys and roles from working,
// keeping them: [Control.AuthorizedKey] no longer answers for its keys,
// and it administers nothing, even when the config file names it a
// server admin (decision 20). A server admin may disable any principal;
// a project admin only one within their reach, as for its keys
// ([Control.AddKey]). One already disabled is returned as it is.
//
// Disabling the last server admin who is not disabled is refused with a
// [*LastAdminError]: a server admin the config file names and who has no
// row is not disabled, and counts.
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
		// Reach before existence, as in AddKey.
		if err := w.within(ctx, sc, op, name); err != nil {
			return err
		}
		cur, err := readPrincipal(ctx, w.tx, name)
		if err != nil {
			return err
		}
		out = cur
		if cur.State == to {
			return nil
		}
		if to == PrincipalDisabled && w.admins(name) {
			if err := w.anotherAdmin(ctx, name); err != nil {
				return err
			}
		}
		n, err := w.exec(ctx, `UPDATE principals SET state = $1, updated_at = $2, rev = rev + 1 WHERE name = $3 AND rev = $4`,
			string(to), w.now, name, cur.Rev)
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
				delete(ix.disabled, name)
			} else {
				delete(ix.active, name)
				ix.disabled[name] = true
			}
		})
		return w.event(ctx, op, name, cur, out)
	})
	if err != nil {
		return Principal{}, err
	}
	return out, nil
}

// anotherAdmin refuses, with a *LastAdminError, disabling the server
// admin name when no other server admin would be left enabled: one the
// config file names whose row is not disabled, or who has no row.
func (w *cwtx) anotherAdmin(ctx context.Context, name string) error {
	disabled, err := queryStrings(ctx, w.tx, `SELECT name FROM principals WHERE state = $1`, string(PrincipalDisabled))
	if err != nil {
		return fmt.Errorf("read the disabled principals: %w", err)
	}
	for _, a := range w.configAdmins {
		if a != name && !slices.Contains(disabled, a) {
			return nil
		}
	}
	return &LastAdminError{Principal: name}
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
	return c.principals(ctx, ` ORDER BY name`)
}

// validActorName refuses, with ErrInvalid, a name no actor of an event
// or a row can have. Unlike validPrincipal it allows a [Reserved] name:
// the operator's `ariesd control init` writes as one.
func validActorName(name string) error {
	if !PrincipalPattern.MatchString(name) {
		return fmt.Errorf("%w: %q is not a principal's name", ErrInvalid, name)
	}
	return nil
}

// PrincipalsCreatedBy returns the principals that creator created,
// disabled ones included, by name. With [Control.KeysAddedBy] and
// [Control.MembersAddedBy] it is what an account left behind: disabling
// the account ends its own access, and none of these. Reads are not
// guarded: the caller decides who may list them.
func (c *Control) PrincipalsCreatedBy(ctx context.Context, creator string) ([]Principal, error) {
	if err := validActorName(creator); err != nil {
		return nil, err
	}
	return c.principals(ctx, ` WHERE created_by = $1 ORDER BY name`, creator)
}

// principals reads the principals that where, a constant clause with
// placeholders for args, selects.
func (c *Control) principals(ctx context.Context, where string, args ...any) ([]Principal, error) {
	rows, err := c.s.r.QueryContext(ctx, principalSelect+where, args...) //nolint:gosec // where is a constant from the callers
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

const principalSelect = `SELECT name, state, kind, COALESCE(owner, ''), COALESCE(display_name, ''), created_by, created_at, updated_at, rev
  FROM principals`

func scanPrincipal(r scanner) (Principal, error) {
	var p Principal
	if err := r.Scan(&p.Name, &p.State, &p.Kind, &p.Owner, &p.DisplayName, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.Rev); err != nil {
		return Principal{}, err
	}
	p.CreatedAt, p.UpdatedAt = p.CreatedAt.UTC(), p.UpdatedAt.UTC()
	return p, nil
}

// readPrincipal reads one principal, or returns ErrNotFound.
func readPrincipal(ctx context.Context, q querier, name string) (Principal, error) {
	p, err := scanPrincipal(q.QueryRowContext(ctx, principalSelect+` WHERE name = $1`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, fmt.Errorf("principal %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("principal %s: %w", name, err)
	}
	return p, nil
}
