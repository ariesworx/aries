package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/safetext"
)

// Identities and invitations (docs/design/identity.md §3.2 and §6.5,
// decisions 57 and 60). A person signs in at a provider the operator
// configured; which principal the sign-in is, is an identity: the
// provider, and the issuer and subject its token names. An admin
// invites an address at a provider for a principal, which is an
// identity with no subject yet; the first sign-in whose verified address
// matches binds it (slice T2, which this file leaves to). Nobody is made
// a principal by signing in (decision 57).
//
// Only a server admin invites, removes an invitation or an identity, or
// sets a principal's kind, owner or display name, and nobody invites an
// identity onto their own principal: an agent holding someone's key
// could otherwise point their principal at an identity it controls
// (decision 19's reasoning).

// Event operations on identities. Each one's target is the principal's
// name; its state is an identity's id, provider, whether it is bound and
// when an invitation ends, never the address or the subject, which are
// personal and the log is kept for good.
const (
	OpIdentityInvite Op = "identity.invite"
	OpIdentityRemove Op = "identity.remove"
)

// DefaultInviteTTL is how long an invitation stays open unused when
// [ControlOptions.InviteTTL] is zero (`login: invite_ttl:`).
const DefaultInviteTTL = 7 * 24 * time.Hour

// The bounds of [ControlOptions.InviteTTL]: an invitation is a match by
// address until it is used, so it does not stay open for long.
const (
	MinInviteTTL = time.Hour
	MaxInviteTTL = 90 * 24 * time.Hour
)

// IdentityState is where an identity stands: bound to a sign-in, or an
// invitation that is open or has lapsed.
type IdentityState string

// Identity states.
const (
	IdentityBound   IdentityState = "bound"
	IdentityPending IdentityState = "pending"
	IdentityLapsed  IdentityState = "lapsed"
)

// Identity is an external identity of a person: an address an admin
// invited at a provider, and, once a sign-in binds it, the issuer and
// subject the provider's token named.
type Identity struct {
	ID        int64
	Principal string
	Provider  string
	// Issuer and Subject are empty until a sign-in binds the identity.
	Issuer  string
	Subject string
	// InvitedAs is the address the admin typed, as typed.
	InvitedAs string
	InvitedBy string
	InvitedAt time.Time
	// InviteUntil is when an unbound invitation lapses; zero once bound.
	InviteUntil time.Time
	// BoundAt is the first sign-in, and LastSignIn the latest; zero
	// until then.
	BoundAt    time.Time
	LastSignIn time.Time
	Rev        Rev
}

// Bound reports whether a sign-in has bound the identity.
func (i Identity) Bound() bool { return i.Subject != "" }

// State is where the identity stands at now: bound, or an invitation
// that is pending until its end and lapsed from then.
func (i Identity) State(now time.Time) IdentityState {
	switch {
	case i.Bound():
		return IdentityBound
	case now.Before(i.InviteUntil):
		return IdentityPending
	}
	return IdentityLapsed
}

// identityState is an identity as its events record it: no address and
// no subject.
type identityState struct {
	ID          int64      `json:"id"`
	Principal   string     `json:"principal"`
	Provider    string     `json:"provider"`
	Bound       bool       `json:"bound,omitempty"`
	InviteUntil *time.Time `json:"invite_until,omitempty"`
	Rev         Rev        `json:"rev"`
}

func (i Identity) state() identityState {
	s := identityState{ID: i.ID, Principal: i.Principal, Provider: i.Provider, Bound: i.Bound(), Rev: i.Rev}
	if !i.Bound() {
		until := i.InviteUntil
		s.InviteUntil = &until
	}
	return s
}

// Invitation is an address to invite at a provider.
type Invitation struct {
	// Provider names a provider of the server's `login:` settings.
	Provider string
	// Address is the person's address at the provider. A sign-in whose
	// verified address equals it, without regard to the case of ASCII
	// letters, binds the invitation.
	Address string
}

// providerPattern is a provider's name in the server's settings.
var providerPattern = lazyregexp.New(`^[a-z][a-z0-9-]{0,31}$`)

// The bounds of an address: what SMTP allows of a path and of its local
// part (RFC 5321, 4.5.3.1).
const (
	maxAddress      = 254
	maxAddressLocal = 64
)

// validate refuses, with ErrInvalid, a provider that is no provider's
// name and an address that is not one plain address.
func (inv Invitation) validate() error {
	if !providerPattern.MatchString(inv.Provider) {
		return fmt.Errorf("%w: provider %q must be a lowercase letter, then up to 31 lowercase letters, digits and hyphens, as the server's login: settings name it", ErrInvalid, inv.Provider)
	}
	return validAddress(inv.Address)
}

// validAddress refuses, with ErrInvalid, anything but one plain address:
// LOCAL@DOMAIN, at most 254 bytes, its local part at most 64, a domain
// of at least two labels, and no space, quote, bracket or other
// character that marks a display form or a list.
func validAddress(a string) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: %q is not an address to invite: %s; give one plain address, such as alice@example.com", ErrInvalid, a, why)
	}
	switch {
	case a == "":
		return fmt.Errorf("%w: give the address to invite, such as alice@example.com", ErrInvalid)
	case len(a) > maxAddress:
		return bad(fmt.Sprintf("it is longer than %d bytes", maxAddress))
	case !safetext.ValidLine(a):
		return bad("it holds a control or bidirectional character")
	case strings.ContainsAny(a, " \t<>()[]\\,;:\""):
		return bad("it holds a space, a quote, a bracket or punctuation no plain address has")
	}
	local, domain, ok := strings.Cut(a, "@")
	switch {
	case !ok:
		return bad("it has no @")
	case strings.Contains(domain, "@"):
		return bad("it has more than one @")
	case local == "":
		return bad("it has nothing before the @")
	case len(local) > maxAddressLocal:
		return bad(fmt.Sprintf("the part before the @ is longer than %d bytes", maxAddressLocal))
	case !strings.Contains(domain, "."):
		return bad("its domain has no dot")
	case slices.Contains(strings.Split(domain, "."), ""):
		return bad("its domain has an empty label")
	}
	return nil
}

// validKind refuses, with ErrInvalid, a kind that is neither a person
// nor a service.
func validKind(k PrincipalKind) error {
	if k != KindPerson && k != KindService {
		return fmt.Errorf("%w: kind %q must be %s or %s", ErrInvalid, k, KindPerson, KindService)
	}
	return nil
}

// maxDisplayName bounds a display name, in characters, as its column
// does.
const maxDisplayName = 128

// checkDisplayName refuses, with ErrInvalid, a display name that is not
// one safe line of at most maxDisplayName characters. Empty is none.
func checkDisplayName(name string) error {
	switch {
	case name == "":
	case utf8.RuneCountInString(name) > maxDisplayName:
		return fmt.Errorf("%w: a display name must be at most %d characters", ErrInvalid, maxDisplayName)
	case !safetext.ValidLine(name) || strings.ContainsRune(name, '\t'):
		return fmt.Errorf("%w: a display name must be one line of UTF-8 without tabs, control or bidirectional characters", ErrInvalid)
	}
	return nil
}

// IdentityRule says which rule of a principal's kind and identities an
// [*IdentityError] applied.
type IdentityRule string

// The rules an [*IdentityError] gives.
const (
	// IdentityRuleService: Principal is, or would become, a service, and
	// a service signs in by a trust rule or a key, never by an identity.
	IdentityRuleService IdentityRule = "service"
	// IdentityRuleOwner: a service needs an Owner who is a person other
	// than itself; Owner is the one given, if any.
	IdentityRuleOwner IdentityRule = "owner"
	// IdentityRulePersonOwner: a person answers to nobody, so has no
	// owner.
	IdentityRulePersonOwner IdentityRule = "person owner"
	// IdentityRuleOwns: Principal owns services, which answer to a
	// person, so it cannot become a service.
	IdentityRuleOwns IdentityRule = "owns"
	// IdentityRuleBound: identity ID is bound to a sign-in, so it is no
	// invitation to remove; it is removed as an identity.
	IdentityRuleBound IdentityRule = "bound"
)

// IdentityError refuses a change that breaks a rule of a principal's
// kind, owner and identities. It wraps ErrInvalid.
type IdentityError struct {
	Principal string
	Rule      IdentityRule
	Owner     string
	ID        int64
}

func (e *IdentityError) Error() string {
	why := ""
	switch e.Rule {
	case IdentityRuleService:
		why = fmt.Sprintf("%s is a service, or has identities: a service signs in by a key or a trust rule, never by an identity", e.Principal)
	case IdentityRuleOwner:
		why = fmt.Sprintf("service %s needs an owner who is a person other than itself", e.Principal)
		if e.Owner != "" {
			why = fmt.Sprintf("service %s needs an owner who is a person other than itself, and %s is not", e.Principal, e.Owner)
		}
	case IdentityRulePersonOwner:
		why = fmt.Sprintf("%s is a person, and a person has no owner", e.Principal)
	case IdentityRuleOwns:
		why = fmt.Sprintf("%s owns services, which answer to a person, so it cannot be a service", e.Principal)
	case IdentityRuleBound:
		why = fmt.Sprintf("identity %d of %s is bound to a sign-in, so it is no invitation", e.ID, e.Principal)
	}
	return fmt.Sprintf("%v: %s", ErrInvalid, why)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *IdentityError) Unwrap() error { return ErrInvalid }

// InvitedError refuses an invitation of Address at Provider while
// invitation ID, for Principal, is open for it: one open invitation per
// address and provider. It wraps ErrConflict.
type InvitedError struct {
	Provider  string
	Address   string
	Principal string
	ID        int64
}

func (e *InvitedError) Error() string {
	return fmt.Sprintf("%s is invited at %s for %s already, as invitation %d", e.Address, e.Provider, e.Principal, e.ID)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *InvitedError) Unwrap() error { return ErrConflict }

// ownIdentities refuses inviting an identity onto principal when the
// actor is principal (decision 19).
func (w *cwtx) ownIdentities(principal string) error {
	if w.actor.Principal == principal {
		return w.denied(OpIdentityInvite, principal, "", AccessOwnIdentity)
	}
	return nil
}

// kindRules refuses, with an *IdentityError, principal name as kind with
// owner: a service needs an owner who is another principal and a person
// (ErrNotFound when there is none of that name), a person has no owner,
// and a service has no identities, nor is one invited for it.
func (w *cwtx) kindRules(ctx context.Context, name string, kind PrincipalKind, owner string, invited bool) error {
	if kind == KindPerson {
		if owner != "" {
			return &IdentityError{Principal: name, Rule: IdentityRulePersonOwner}
		}
		return nil
	}
	if invited {
		return &IdentityError{Principal: name, Rule: IdentityRuleService}
	}
	if owner == "" || owner == name {
		return &IdentityError{Principal: name, Rule: IdentityRuleOwner, Owner: owner}
	}
	o, err := readPrincipal(ctx, w.tx, owner)
	if err != nil {
		return err
	}
	if o.Kind != KindPerson {
		return &IdentityError{Principal: name, Rule: IdentityRuleOwner, Owner: owner}
	}
	return nil
}

// Invite invites inv's address at its provider for principal, a person,
// and returns the invitation, which lapses unused after
// ControlOptions.InviteTTL. changed is false only when nothing was
// written.
//
// An invitation of the same address and provider for the same principal
// that is not bound, open or lapsed, is renewed: its end starts again
// from now, and it records who invited it last, in the case typed this
// time. For another principal it is refused with an [*InvitedError],
// which names that principal and the invitation.
//
// Only a server admin may invite, and not onto their own principal: an
// [*AccessError] whose reason is [AccessServerAdmin] or
// [AccessOwnIdentity]. A principal that does not exist is refused with
// ErrNotFound, a service with an [*IdentityError], and a row past
// ControlLimits.Identities or ControlLimits.Invites with a
// [*ControlLimitError]. A provider or an address that is not one is
// refused with ErrInvalid. The provider is not checked against the
// server's settings here; that is the daemon's.
func (c *Control) Invite(ctx context.Context, actor Actor, principal string, inv Invitation) (Identity, bool, error) {
	if err := validPrincipal(principal); err != nil {
		return Identity{}, false, err
	}
	if err := inv.validate(); err != nil {
		return Identity{}, false, err
	}
	var out Identity
	var changed bool
	err := c.write(ctx, actor, func(w *cwtx) error {
		if err := w.ownIdentities(principal); err != nil {
			return err
		}
		if err := w.serverOnly(OpIdentityInvite, principal, ""); err != nil {
			return err
		}
		p, err := readPrincipal(ctx, w.tx, principal)
		if err != nil {
			return err
		}
		if p.Kind != KindPerson {
			return &IdentityError{Principal: principal, Rule: IdentityRuleService}
		}
		out, changed, err = w.invite(ctx, principal, inv)
		return err
	})
	if err != nil {
		return Identity{}, false, err
	}
	return out, changed, nil
}

// invite invites inv for principal, a person, or renews its invitation
// there, as Invite does. The caller has checked the actor, and that the
// principal exists and is a person.
func (w *cwtx) invite(ctx context.Context, principal string, inv Invitation) (Identity, bool, error) {
	until := w.now.Add(w.inviteTTL)
	cur, err := scanIdentity(w.tx.QueryRowContext(ctx, identitySelect+` WHERE subject IS NULL AND provider = $1 AND lower(invited_as) = lower($2::text COLLATE "C")`,
		inv.Provider, inv.Address))
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return Identity{}, false, fmt.Errorf("read invitations of %s: %w", principal, err)
	case cur.Principal != principal:
		return Identity{}, false, &InvitedError{Provider: inv.Provider, Address: cur.InvitedAs, Principal: cur.Principal, ID: cur.ID}
	default:
		if cur.State(w.now) == IdentityLapsed {
			if err := w.inviteRoom(ctx); err != nil {
				return Identity{}, false, err
			}
		}
		n, err := w.exec(ctx, `UPDATE identities SET invited_as = $1, invited_by = $2, invited_at = $3, invite_until = $4, rev = rev + 1
  WHERE id = $5 AND rev = $6`, inv.Address, w.actor.Principal, w.now, until, cur.ID, cur.Rev)
		if err != nil {
			return Identity{}, false, fmt.Errorf("renew invitation %d: %w", cur.ID, err)
		}
		if n == 0 {
			return Identity{}, false, fmt.Errorf("%w: invitation %d changed during %s", errRetry, cur.ID, OpIdentityInvite)
		}
		out := cur
		out.InvitedAs, out.InvitedBy, out.InvitedAt, out.InviteUntil, out.Rev = inv.Address, w.actor.Principal, w.now, until, cur.Rev+1
		w.indexed(func(ix *index) { ix.identities[out.ID] = indexedIdentity{until: until} })
		return out, true, w.event(ctx, OpIdentityInvite, principal, cur.state(), out.state())
	}
	if err := w.room(ctx, limitIdentities, w.lim.Identities, `SELECT COUNT(*) FROM identities WHERE principal = $1`, principal); err != nil {
		return Identity{}, false, err
	}
	if err := w.inviteRoom(ctx); err != nil {
		return Identity{}, false, err
	}
	out := Identity{Principal: principal, Provider: inv.Provider, InvitedAs: inv.Address, InvitedBy: w.actor.Principal, InvitedAt: w.now,
		InviteUntil: until, Rev: 1}
	if err := w.tx.QueryRowContext(ctx, `INSERT INTO identities (principal, provider, invited_as, invited_by, invited_at, invite_until, rev)
  VALUES ($1, $2, $3, $4, $5, $6, 1) RETURNING id`, principal, inv.Provider, inv.Address, out.InvitedBy, w.now, until).Scan(&out.ID); err != nil {
		return Identity{}, false, fmt.Errorf("invite for %s: %w", principal, err)
	}
	w.mutated = true
	w.indexed(func(ix *index) { ix.identities[out.ID] = indexedIdentity{until: until} })
	return out, true, w.event(ctx, OpIdentityInvite, principal, nil, out.state())
}

// inviteRoom refuses, with a *ControlLimitError, one more open invitation
// when the server has ControlLimits.Invites. A lapsed one is not open.
func (w *cwtx) inviteRoom(ctx context.Context) error {
	return w.room(ctx, limitInvites, w.lim.Invites, `SELECT COUNT(*) FROM identities WHERE subject IS NULL AND invite_until > $1`, w.now)
}

// RemoveInvite removes invitation id, open or lapsed, and returns it. A
// bound identity is refused with an [*IdentityError]: it is removed with
// [Control.RemoveIdentity]. Who may, and the other refusals, are as for
// RemoveIdentity.
func (c *Control) RemoveInvite(ctx context.Context, actor Actor, id int64) (Identity, error) {
	return c.removeIdentity(ctx, actor, id, true)
}

// RemoveIdentity removes identity id, bound or not, and returns it: no
// sign-in matches it from then on. Only a server admin may, any
// principal's, their own included, since removing only takes access
// away: anyone else is refused with an [*AccessError] whose reason is
// [AccessServerAdmin], whether or not the identity exists. One that does
// not exist is refused with ErrNotFound, and an id below 1 with
// ErrInvalid.
func (c *Control) RemoveIdentity(ctx context.Context, actor Actor, id int64) (Identity, error) {
	return c.removeIdentity(ctx, actor, id, false)
}

func (c *Control) removeIdentity(ctx context.Context, actor Actor, id int64, invite bool) (Identity, error) {
	if id < 1 {
		return Identity{}, fmt.Errorf("%w: identity %d: an identity's id is a positive number, as `aries admin identity list NAME` prints it", ErrInvalid, id)
	}
	var out Identity
	err := c.write(ctx, actor, func(w *cwtx) error {
		if err := w.serverOnly(OpIdentityRemove, "", ""); err != nil {
			return err
		}
		cur, err := readIdentity(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if invite && cur.Bound() {
			return &IdentityError{Principal: cur.Principal, Rule: IdentityRuleBound, ID: id}
		}
		if _, err := w.exec(ctx, `DELETE FROM identities WHERE id = $1`, id); err != nil {
			return fmt.Errorf("remove identity %d: %w", id, err)
		}
		out = cur
		w.indexed(func(ix *index) { delete(ix.identities, id) })
		return w.event(ctx, OpIdentityRemove, cur.Principal, cur.state(), nil)
	})
	if err != nil {
		return Identity{}, err
	}
	return out, nil
}

// Identity returns the identity with id, or ErrNotFound. Reads are not
// guarded: the caller decides who may see it.
func (c *Control) Identity(ctx context.Context, id int64) (Identity, error) {
	return readIdentity(ctx, c.s.r, id)
}

// Identities returns principal's identities, bound and not, oldest
// first; none for a principal that has none or does not exist. Reads are
// not guarded: the caller decides who may list them.
func (c *Control) Identities(ctx context.Context, principal string) ([]Identity, error) {
	if err := validPrincipal(principal); err != nil {
		return nil, err
	}
	return queryIdentities(ctx, c.s.r, "identities of "+principal, ` WHERE principal = $1 ORDER BY id`, principal)
}

// InviteQuery pages [Control.Invites].
type InviteQuery struct {
	// After lists the invitations whose id is greater.
	After int64
	// Limit is the most to list; 0 lists them all.
	Limit int
	// Pending lists only the invitations that have not lapsed.
	Pending bool
}

// Invites returns the invitations, the identities no sign-in has bound,
// by id, as q pages them, and how many more there are after the last one
// returned. Reads are not guarded: the caller decides who may list them.
func (c *Control) Invites(ctx context.Context, q InviteQuery) ([]Identity, int, error) {
	if q.Limit < 0 {
		return nil, 0, fmt.Errorf("%w: limit %d", ErrInvalid, q.Limit)
	}
	// A page is read in one transaction with the count of the rest, and no
	// more rows than it holds: the invitations of a whole server may be
	// tens of thousands, lapsed ones included.
	r, end, err := c.s.beginRead(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("invitations: %w", err)
	}
	defer end()
	const where = ` WHERE subject IS NULL AND id > $1 AND invite_until > $2`
	since := time.Time{}
	if q.Pending {
		since = c.Now()
	}
	limit := any(nil) // LIMIT NULL is no limit
	if q.Limit > 0 {
		limit = q.Limit
	}
	rows, err := queryIdentities(ctx, r, "invitations", where+` ORDER BY id LIMIT $3`, q.After, since, limit)
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := r.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities`+where, q.After, since).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count invitations: %w", err)
	}
	return rows, max(total-len(rows), 0), nil
}

// PrincipalChange is what [Control.SetPrincipal] sets. A field left at
// its zero value is not changed.
type PrincipalChange struct {
	// DisplayName, when set, is the new display name; empty clears it.
	DisplayName *string
	// Kind, when set, is the new kind. A person's owner is cleared.
	Kind PrincipalKind
	// Owner, when set, is a service's new owner.
	Owner string
}

// principalSet is principal.set's after state: the principal, and the
// names of the fields the change set. A display name's value is in
// neither.
type principalSet struct {
	Principal
	Fields []string `json:"fields"`
}

// SetPrincipal changes principal name's display name, kind or owner, and
// returns it; changed is false when it already was as asked, and then
// nothing is recorded. Only a server admin may: anyone else is refused
// with an [*AccessError] whose reason is [AccessServerAdmin].
//
// A service needs an owner who is a person other than itself, a person
// has none (making a service a person clears it), a person who has
// identities or owns services cannot become a service: each refused
// with an [*IdentityError]. A principal or owner that does not exist is
// refused with ErrNotFound, and a kind or display name that is not one
// with ErrInvalid.
func (c *Control) SetPrincipal(ctx context.Context, actor Actor, name string, ch PrincipalChange) (Principal, bool, error) {
	if err := validPrincipal(name); err != nil {
		return Principal{}, false, err
	}
	if ch.Kind != "" {
		if err := validKind(ch.Kind); err != nil {
			return Principal{}, false, err
		}
	}
	if ch.Owner != "" {
		if err := validPrincipal(ch.Owner); err != nil {
			return Principal{}, false, err
		}
	}
	if ch.DisplayName != nil {
		if err := checkDisplayName(*ch.DisplayName); err != nil {
			return Principal{}, false, err
		}
	}
	var out Principal
	var changed bool
	err := c.write(ctx, actor, func(w *cwtx) error {
		changed = false
		if err := w.serverOnly(OpPrincipalSet, name, ""); err != nil {
			return err
		}
		cur, err := readPrincipal(ctx, w.tx, name)
		if err != nil {
			return err
		}
		next := cur
		next.Kind = cmp.Or(ch.Kind, cur.Kind)
		if ch.DisplayName != nil {
			next.DisplayName = *ch.DisplayName
		}
		switch {
		case next.Kind == KindPerson && ch.Owner == "":
			next.Owner = ""
		case ch.Owner != "":
			next.Owner = ch.Owner
		}
		if err := w.kindRules(ctx, name, next.Kind, next.Owner, false); err != nil {
			return err
		}
		if next.Kind == KindService && cur.Kind == KindPerson {
			if err := w.mayServe(ctx, name); err != nil {
				return err
			}
		}
		var fields []string
		if next.DisplayName != cur.DisplayName {
			fields = append(fields, "display_name")
		}
		if next.Kind != cur.Kind {
			fields = append(fields, "kind")
		}
		if next.Owner != cur.Owner {
			fields = append(fields, "owner")
		}
		out = cur
		if len(fields) == 0 {
			return nil
		}
		n, err := w.exec(ctx, `UPDATE principals SET kind = $1, owner = $2, display_name = $3, updated_at = $4, rev = rev + 1
  WHERE name = $5 AND rev = $6`, string(next.Kind), nullStr(next.Owner), nullStr(next.DisplayName), w.now, name, cur.Rev)
		if err != nil {
			return fmt.Errorf("%s %s: %w", OpPrincipalSet, name, err)
		}
		if n == 0 {
			return fmt.Errorf("%w: principal %s changed during %s", errRetry, name, OpPrincipalSet)
		}
		next.UpdatedAt, next.Rev = w.now, cur.Rev+1
		out, changed = next, true
		return w.event(ctx, OpPrincipalSet, name, cur, principalSet{Principal: next, Fields: fields})
	})
	if err != nil {
		return Principal{}, false, err
	}
	return out, changed, nil
}

// mayServe refuses, with an *IdentityError, making person name a
// service while it has an identity or owns a service.
func (w *cwtx) mayServe(ctx context.Context, name string) error {
	var ids, owns int
	if err := w.tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM identities WHERE principal = $1),
  (SELECT COUNT(*) FROM principals WHERE owner = $1)`, name).Scan(&ids, &owns); err != nil {
		return fmt.Errorf("read identities and services of %s: %w", name, err)
	}
	switch {
	case ids > 0:
		return &IdentityError{Principal: name, Rule: IdentityRuleService}
	case owns > 0:
		return &IdentityError{Principal: name, Rule: IdentityRuleOwns}
	}
	return nil
}

const identitySelect = `SELECT id, principal, provider, COALESCE(issuer, ''), COALESCE(subject, ''), invited_as, invited_by, invited_at,
  invite_until, bound_at, last_sign_in, rev FROM identities`

func scanIdentity(r scanner) (Identity, error) {
	var i Identity
	var until, bound, last sql.NullTime
	if err := r.Scan(&i.ID, &i.Principal, &i.Provider, &i.Issuer, &i.Subject, &i.InvitedAs, &i.InvitedBy, &i.InvitedAt,
		&until, &bound, &last, &i.Rev); err != nil {
		return Identity{}, err
	}
	i.InvitedAt = i.InvitedAt.UTC()
	i.InviteUntil, i.BoundAt, i.LastSignIn = utcOrZero(until), utcOrZero(bound), utcOrZero(last)
	return i, nil
}

// utcOrZero is t in UTC, or the zero time for NULL.
func utcOrZero(t sql.NullTime) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

// readIdentity reads one identity, or returns ErrNotFound.
func readIdentity(ctx context.Context, q querier, id int64) (Identity, error) {
	i, err := scanIdentity(q.QueryRowContext(ctx, identitySelect+` WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, fmt.Errorf("identity %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Identity{}, fmt.Errorf("identity %d: %w", id, err)
	}
	return i, nil
}

// queryIdentities reads the identities that where, a constant clause
// with placeholders for args, selects. what names them in an error.
func queryIdentities(ctx context.Context, q querier, what, where string, args ...any) ([]Identity, error) {
	rows, err := q.QueryContext(ctx, identitySelect+where, args...) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Identity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// indexedIdentity is an identity in the index, for the status report's
// counts: bound, or an invitation that lapses at until.
type indexedIdentity struct {
	bound bool
	until time.Time
}

// loadIdentities reads every identity for the index.
func loadIdentities(ctx context.Context, q querier) (map[int64]indexedIdentity, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, subject IS NOT NULL, invite_until FROM identities`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]indexedIdentity{}
	for rows.Next() {
		var id int64
		var i indexedIdentity
		var until sql.NullTime
		if err := rows.Scan(&id, &i.bound, &until); err != nil {
			return nil, err
		}
		i.until = utcOrZero(until)
		out[id] = i
	}
	return out, rows.Err()
}
