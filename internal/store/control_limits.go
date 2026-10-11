package store

import (
	"context"
	"fmt"
)

// ControlLimits bound the rows of the control database (rule 18). Zero
// fields take [DefaultControlLimits]. A row past a limit is refused with
// a [*ControlLimitError].
type ControlLimits struct {
	// Projects caps the projects on the server, in every state: an
	// archived project keeps its database.
	Projects int `yaml:"projects"`
	// Principals caps the principals on the server, disabled ones
	// included.
	Principals int `yaml:"principals"`
	// Keys caps the public keys of one principal.
	Keys int `yaml:"keys_per_principal"`
	// Members caps the principals with a role in one project.
	Members int `yaml:"members_per_project"`
}

// DefaultControlLimits are the limits a zero field takes.
var DefaultControlLimits = ControlLimits{Projects: 500, Principals: 5000, Keys: 20, Members: 1000}

// The limits' names, as a [*ControlLimitError] gives them.
const (
	limitProjects   = "projects"
	limitPrincipals = "principals"
	limitKeys       = "keys_per_principal"
	limitMembers    = "members_per_project"
)

// withDefaults returns l with each zero field set from
// DefaultControlLimits.
func (l ControlLimits) withDefaults() ControlLimits {
	d := DefaultControlLimits
	for _, f := range []struct{ v, d *int }{
		{&l.Projects, &d.Projects}, {&l.Principals, &d.Principals}, {&l.Keys, &d.Keys}, {&l.Members, &d.Members},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	return l
}

// Validate refuses, with ErrInvalid, a negative limit. Zero is valid: it
// means the default.
func (l ControlLimits) Validate() error {
	for _, f := range []struct {
		name string
		v    int
	}{
		{limitProjects, l.Projects}, {limitPrincipals, l.Principals}, {limitKeys, l.Keys}, {limitMembers, l.Members},
	} {
		if f.v < 0 {
			return fmt.Errorf("%w: limit %s is %d; give a positive number, or leave it out for the default", ErrInvalid, f.name, f.v)
		}
	}
	return nil
}

// ControlLimitError refuses a new row when the control store holds Max
// of them already. Limit names the limit as starfixd's settings will
// (projects, principals, keys_per_principal, members_per_project). It
// wraps ErrInvalid.
type ControlLimitError struct {
	Limit string
	Max   int
}

func (e *ControlLimitError) Error() string {
	switch e.Limit {
	case limitKeys:
		return fmt.Sprintf("%v: a principal keeps at most %d keys", ErrInvalid, e.Max)
	case limitMembers:
		return fmt.Sprintf("%v: a project keeps at most %d members", ErrInvalid, e.Max)
	}
	return fmt.Sprintf("%v: the server keeps at most %d %s", ErrInvalid, e.Max, e.Limit)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *ControlLimitError) Unwrap() error { return ErrInvalid }

// room refuses, with a *ControlLimitError, one more row when count, a
// query that counts the rows a limit bounds, already gives most.
func (w *cwtx) room(ctx context.Context, limit string, most int, count string, args ...any) error {
	var n int
	if err := w.tx.QueryRowContext(ctx, count, args...).Scan(&n); err != nil {
		return fmt.Errorf("count %s: %w", limit, err)
	}
	if n >= most {
		return &ControlLimitError{Limit: limit, Max: most}
	}
	return nil
}

// TakenKind says what a [*TakenError] found in use.
type TakenKind string

// The kinds a [*TakenError] gives.
const (
	// TakenProjectID: a project has the id, with other settings.
	TakenProjectID TakenKind = "project id"
	// TakenProjectName: another project has the name.
	TakenProjectName TakenKind = "project name"
	// TakenDBName: another project has the database.
	TakenDBName TakenKind = "database name"
	// TakenPrincipal: a principal has the name.
	TakenPrincipal TakenKind = "principal name"
	// TakenKey: another principal has the key.
	TakenKey TakenKind = "key"
)

// TakenError refuses a new project, principal or key whose Value, of the
// Kind given, is in use. For a key it does not say whose it is. It wraps
// ErrConflict.
type TakenError struct {
	Kind  TakenKind
	Value string
}

func (e *TakenError) Error() string {
	switch e.Kind {
	case TakenProjectID:
		return fmt.Sprintf("project %s already exists with another name, prefix, account or database", e.Value)
	case TakenKey:
		return fmt.Sprintf("key %s already belongs to another principal", e.Value)
	}
	return fmt.Sprintf("%s %q is taken", e.Kind, e.Value)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *TakenError) Unwrap() error { return ErrConflict }
