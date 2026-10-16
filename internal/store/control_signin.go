package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// Sign-ins (docs/design/transport.md §6, slice T2). A person signs in at
// a provider the operator configured; the daemon verifies the provider's
// token and gives the store who it names: the provider, and the issuer,
// subject and address the token says. [Control.SignIn] finds the person
// by (issuer, subject), or binds an open invitation whose address the
// token says is verified, and grants a sign-in for the day, whose token
// the daemon made and of which the store keeps only the hash. Nobody is
// made a principal by signing in (decision 57).
//
// A grant ends at its end, at a logout or an admin's word
// ([Control.EndSignIn]), or when the identity it was made by is removed.
// The index holds the live grants by their tokens' hashes, so that a
// connection presenting a token costs no database read.

// Event operations of sign-ins. identity.bind's target is the
// principal, and its state the identity's, with no address or subject;
// signin.grant's and signin.end's target is the principal, and their
// state the grant's, with no token or key.
const (
	OpIdentityBind Op = "identity.bind"
	OpSignInGrant  Op = "signin.grant"
	OpSignInEnd    Op = "signin.end"
)

// The flows a sign-in came by (transport.md §6.9).
const (
	FlowLoopback = "loopback"
	FlowURL      = "url"
	FlowDevice   = "device"
	FlowWorkload = "workload"
	FlowCard     = "card"
)

// signInFlows are the flows, as the grants table's check has them.
var signInFlows = []string{FlowLoopback, FlowURL, FlowDevice, FlowWorkload, FlowCard}

// Why a sign-in ended before its end.
const (
	EndLogout          = "logout"
	EndRevoked         = "revoked"
	EndIdentityRemoved = "identity_removed"
)

// The scopes a grant may hold (proto's ScopeCLI, ScopeMCP and ScopeCI).
var grantScopes = []string{"cli", "mcp", "ci"}

// tokenHashPattern is a token's hash as the store keeps it: SHA-256 in
// lowercase hex.
var tokenHashPattern = lazyregexp.New(`^[0-9a-f]{64}$`)

// SignIn is a verified sign-in at a provider, for [Control.SignIn].
type SignIn struct {
	// Provider is the provider's name in the server's settings, which an
	// invitation names.
	Provider string
	// Issuer and Subject are who the provider's token names: what an
	// identity is bound to and matched by.
	Issuer, Subject string
	// Address is the token's address, and Verified whether the provider
	// says it is the person's. Only a verified address binds an
	// invitation. It is not stored.
	Address  string
	Verified bool
	// ClientID is the client the grant is for (aries-cli), Scopes its
	// scopes, JKT the thumbprint of the DPoP key its token is bound to,
	// and Resource the server's URL it is for.
	ClientID string
	Scopes   []string
	JKT      string
	Resource string
	// Flow is how the sign-in came (FlowLoopback, FlowURL), and Machine
	// what the client says it is: shown, and trusted for nothing.
	Flow    string
	Machine string
	// Lifetime is how long the grant lasts from now.
	Lifetime time.Duration
	// TokenHash is the SHA-256 of the access token the daemon made, in
	// lowercase hex.
	TokenHash string
}

// validate refuses, with ErrInvalid, a sign-in the store would not keep.
func (in SignIn) validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: sign-in: "+format, append([]any{ErrInvalid}, a...)...)
	}
	switch {
	case !providerPattern.MatchString(in.Provider):
		return bad("provider %q is not a provider's name", in.Provider)
	case in.Issuer == "" || in.Subject == "" || len(in.Subject) > 255:
		return bad("an issuer and a subject of at most 255 bytes are required")
	case !tokenHashPattern.MatchString(in.TokenHash):
		return bad("the token's hash is not SHA-256 in hex")
	case in.Lifetime <= 0:
		return bad("a lifetime is required")
	case !slices.Contains(signInFlows, in.Flow):
		return bad("flow %q is not one", in.Flow)
	case in.ClientID == "" || len(in.ClientID) > 64 || len(in.JKT) > 64 || in.Resource == "":
		return bad("a client id, a resource and a key thumbprint of at most 64 bytes are required")
	case len(in.Scopes) == 0:
		return bad("no scope")
	}
	for _, s := range in.Scopes {
		if !slices.Contains(grantScopes, s) {
			return bad("scope %q is not cli, mcp or ci", s)
		}
	}
	return checkLine("machine", in.Machine, maxName, true)
}

// Grant is one sign-in of a principal.
type Grant struct {
	ID        string
	Principal string
	Kind      PrincipalKind
	ClientID  string
	Scopes    []string
	// JKT is the thumbprint of the DPoP key its tokens are bound to;
	// empty for a bearer grant.
	JKT      string
	Resource string
	Flow     string
	Provider string
	// Identity is the identity it was made by, or 0 once that is
	// removed.
	Identity  int64
	Machine   string
	CreatedAt time.Time
	EndsAt    time.Time
	// RevokedAt and EndReason are set when it ended before EndsAt.
	RevokedAt time.Time
	EndReason string
}

// Live reports whether g has neither ended nor been ended at now.
func (g Grant) Live(now time.Time) bool { return g.EndReason == "" && now.Before(g.EndsAt) }

// grantState is a grant as its events record it: no token and no key.
type grantState struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Provider  string     `json:"provider,omitempty"`
	Identity  int64      `json:"identity,omitempty"`
	ClientID  string     `json:"client_id"`
	Scopes    []string   `json:"scopes"`
	Flow      string     `json:"flow"`
	Machine   string     `json:"machine"`
	EndsAt    time.Time  `json:"ends_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
}

func (g Grant) state() grantState {
	s := grantState{ID: g.ID, Kind: string(g.Kind), Provider: g.Provider, Identity: g.Identity, ClientID: g.ClientID, Scopes: g.Scopes,
		Flow: g.Flow, Machine: g.Machine, EndsAt: g.EndsAt, EndReason: g.EndReason}
	if !g.RevokedAt.IsZero() {
		at := g.RevokedAt
		s.RevokedAt = &at
	}
	return s
}

// SignInReason says why [Control.SignIn] refused a sign-in.
type SignInReason string

// The reasons a [*SignInError] gives.
const (
	// SignInUninvited: no identity and no open invitation matches.
	SignInUninvited SignInReason = "uninvited"
	// SignInLapsed: the invitation of the address lapsed at Until.
	SignInLapsed SignInReason = "lapsed"
	// SignInUnverified: an invitation is open for the address, and the
	// provider does not say the address is verified.
	SignInUnverified SignInReason = "unverified"
	// SignInRebound: the address is bound to another account at the
	// provider; the account signing in is not the one bound.
	SignInRebound SignInReason = "rebound"
	// SignInDisabled: the identity's principal is disabled.
	SignInDisabled SignInReason = "disabled"
)

// SignInError refuses a sign-in. Provider and Address are what the
// token gave, for the person's own refusal; neither is logged. It wraps
// ErrForbidden.
type SignInError struct {
	Reason    SignInReason
	Provider  string
	Address   string
	Principal string
	Until     time.Time
}

func (e *SignInError) Error() string {
	switch e.Reason {
	case SignInLapsed:
		return fmt.Sprintf("the invitation of %s at %s lapsed at %s", e.Address, e.Provider, e.Until.Format(time.RFC3339))
	case SignInUnverified:
		return fmt.Sprintf("%s does not say %s is verified, and only a verified address takes an invitation", e.Provider, e.Address)
	case SignInRebound:
		return fmt.Sprintf("%s is bound to another account at %s; this account is not the one that signed in before", e.Address, e.Provider)
	case SignInDisabled:
		return fmt.Sprintf("%s is disabled on this server", e.Principal)
	}
	return fmt.Sprintf("no invitation for %s at %s", e.Address, e.Provider)
}

// Unwrap makes errors.Is(err, ErrForbidden) hold.
func (e *SignInError) Unwrap() error { return ErrForbidden }

// signInSession is the session a sign-in's events are recorded under.
const signInSession = "signin"

// SignIn records a verified sign-in and returns its grant: by the
// identity bound to (in.Issuer, in.Subject), or by binding the open
// invitation at in.Provider whose address, without regard to the case of
// ASCII letters, is in.Address, which the provider says is verified.
// Binding and granting are one write, with an identity.bind event and a
// signin.grant event, by the principal; a later sign-in matches
// (issuer, subject) alone, and moves the identity's last_sign_in as
// bookkeeping.
//
// A sign-in no identity or open invitation matches, of an invitation
// that lapsed, of an unverified address, of an address bound to
// another subject, or of a disabled principal is refused with a
// [*SignInError]. A principal with ControlLimits.Grants live sign-ins is
// refused with a [*ControlLimitError]. A sign-in that is not one is
// refused with ErrInvalid.
func (c *Control) SignIn(ctx context.Context, in SignIn) (Grant, error) {
	if err := in.validate(); err != nil {
		return Grant{}, err
	}
	// Who it is decides whose write it is: read it once to name the
	// actor, and again inside the write, which is what counts.
	m, err := c.matchSignIn(ctx, c.s.r, in, c.Now())
	if err != nil {
		return Grant{}, err
	}
	var out Grant
	actor := Actor{Principal: m.identity.Principal, Session: signInSession, Machine: in.Machine}
	err = c.write(ctx, actor, func(w *cwtx) error {
		m, err := c.matchSignIn(ctx, w.tx, in, w.now)
		if err != nil {
			return err
		}
		w.actor.Principal = m.identity.Principal
		p, err := readPrincipal(ctx, w.tx, m.identity.Principal)
		if err != nil {
			return err
		}
		if p.State != PrincipalActive {
			return &SignInError{Reason: SignInDisabled, Provider: in.Provider, Address: in.Address, Principal: p.Name}
		}
		if err := w.room(ctx, limitGrants, w.lim.Grants, `SELECT COUNT(*) FROM grants WHERE principal = $1 AND revoked_at IS NULL AND ends_at > $2`,
			p.Name, w.now); err != nil {
			return err
		}
		if m.bind {
			if err := w.bindIdentity(ctx, m.identity, in); err != nil {
				return err
			}
		} else if err := w.touch(ctx, `UPDATE identities SET last_sign_in = $1 WHERE id = $2`, w.now, m.identity.ID); err != nil {
			return fmt.Errorf("note the sign-in of identity %d: %w", m.identity.ID, err)
		}
		g := Grant{ID: "g-" + strings.ToLower(rand.Text()[:16]), Principal: p.Name, Kind: p.Kind, ClientID: in.ClientID,
			Scopes: slices.Clone(in.Scopes), JKT: in.JKT, Resource: in.Resource, Flow: in.Flow, Provider: in.Provider,
			Identity: m.identity.ID, Machine: in.Machine, CreatedAt: w.now, EndsAt: w.now.Add(in.Lifetime)}
		if _, err := w.exec(ctx, `INSERT INTO grants (id, principal, kind, client_id, scopes, jkt, resource, flow, provider, identity, machine, created_at, ends_at)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, g.ID, g.Principal, string(g.Kind), g.ClientID, strings.Join(g.Scopes, " "),
			nullStr(g.JKT), g.Resource, g.Flow, g.Provider, g.Identity, g.Machine, g.CreatedAt, g.EndsAt); err != nil {
			return fmt.Errorf("grant a sign-in to %s: %w", p.Name, err)
		}
		if _, err := w.exec(ctx, `INSERT INTO tokens (hash, grant_id, kind, expires_at) VALUES ($1, $2, 'access', $3)`,
			in.TokenHash, g.ID, g.EndsAt); err != nil {
			return fmt.Errorf("keep the sign-in's token: %w", err)
		}
		out = g
		w.indexed(func(ix *index) { ix.tokens[in.TokenHash] = g })
		return w.event(ctx, OpSignInGrant, p.Name, nil, g.state())
	})
	if err != nil {
		return Grant{}, err
	}
	return out, nil
}

// signInMatch is the identity a sign-in is by, and whether the sign-in
// binds it.
type signInMatch struct {
	identity Identity
	bind     bool
}

// matchSignIn finds the identity in names, at now: bound to its issuer
// and subject, or else an open invitation of its verified address at its
// provider; or it says, with a *SignInError, why there is none.
func (c *Control) matchSignIn(ctx context.Context, q querier, in SignIn, now time.Time) (signInMatch, error) {
	refuse := func(r SignInReason, until time.Time) (signInMatch, error) {
		return signInMatch{}, &SignInError{Reason: r, Provider: in.Provider, Address: in.Address, Until: until}
	}
	id, err := scanIdentity(q.QueryRowContext(ctx, identitySelect+` WHERE issuer = $1 AND subject = $2`, in.Issuer, in.Subject))
	switch {
	case err == nil:
		return signInMatch{identity: id}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return signInMatch{}, fmt.Errorf("find the identity of a sign-in at %s: %w", in.Provider, err)
	}
	inv, err := scanIdentity(q.QueryRowContext(ctx, identitySelect+` WHERE subject IS NULL AND provider = $1 AND lower(invited_as) = lower($2::text COLLATE "C")`,
		in.Provider, in.Address))
	switch {
	case err == nil:
		if inv.State(now) == IdentityLapsed {
			return refuse(SignInLapsed, inv.InviteUntil)
		}
		if !in.Verified {
			return refuse(SignInUnverified, time.Time{})
		}
		return signInMatch{identity: inv, bind: true}, nil
	case !errors.Is(err, sql.ErrNoRows):
		return signInMatch{}, fmt.Errorf("find the invitation of a sign-in at %s: %w", in.Provider, err)
	}
	var bound int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities WHERE subject IS NOT NULL AND provider = $1 AND lower(invited_as) = lower($2::text COLLATE "C")`,
		in.Provider, in.Address).Scan(&bound); err != nil {
		return signInMatch{}, fmt.Errorf("find the identities of a sign-in at %s: %w", in.Provider, err)
	}
	if bound > 0 {
		return refuse(SignInRebound, time.Time{})
	}
	return refuse(SignInUninvited, time.Time{})
}

// bindIdentity binds invitation inv to in's issuer and subject, as of
// this sign-in, by compare-and-swap on its rev.
func (w *cwtx) bindIdentity(ctx context.Context, inv Identity, in SignIn) error {
	n, err := w.exec(ctx, `UPDATE identities SET issuer = $1, subject = $2, bound_at = $3, last_sign_in = $3, invite_until = NULL, rev = rev + 1
  WHERE id = $4 AND rev = $5 AND subject IS NULL`, in.Issuer, in.Subject, w.now, inv.ID, inv.Rev)
	if err != nil {
		// A unique violation is a sign-in beside this one that bound the
		// subject first: the write is run again, and decides again.
		return fmt.Errorf("bind identity %d: %w", inv.ID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: invitation %d changed during %s", errRetry, inv.ID, OpIdentityBind)
	}
	bound := inv
	bound.Issuer, bound.Subject, bound.BoundAt, bound.LastSignIn, bound.InviteUntil, bound.Rev = in.Issuer, in.Subject, w.now, w.now, time.Time{}, inv.Rev+1
	w.indexed(func(ix *index) { ix.identities[inv.ID] = indexedIdentity{bound: true} })
	return w.event(ctx, OpIdentityBind, inv.Principal, inv.state(), bound.state())
}

// EndSignIn ends sign-in id now for reason (EndLogout, EndRevoked):
// its tokens are deleted, and it is marked ended, with a signin.end
// event. changed is false, and nothing is written, when it had ended
// already. Its holder may end it, and a server admin may end anyone's;
// anyone else is refused with an [*AccessError]. One that does not exist
// is refused with ErrNotFound.
func (c *Control) EndSignIn(ctx context.Context, actor Actor, id, reason string) (Grant, bool, error) {
	if reason != EndLogout && reason != EndRevoked {
		return Grant{}, false, fmt.Errorf("%w: a sign-in ends by logout or revoked, not %q", ErrInvalid, reason)
	}
	var out Grant
	var changed bool
	err := c.write(ctx, actor, func(w *cwtx) error {
		changed = false
		g, err := readGrant(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if g.Principal != w.actor.Principal && !w.admin {
			return w.denied(OpSignInEnd, g.Principal, "", AccessServerAdmin)
		}
		out = g
		if g.EndReason != "" {
			return nil
		}
		out, err = w.endGrant(ctx, g, reason)
		changed = err == nil
		return err
	})
	if err != nil {
		return Grant{}, false, err
	}
	return out, changed, nil
}

// endGrant ends g now for reason: its tokens deleted, the index told,
// and a signin.end event.
func (w *cwtx) endGrant(ctx context.Context, g Grant, reason string) (Grant, error) {
	if _, err := w.exec(ctx, `DELETE FROM tokens WHERE grant_id = $1`, g.ID); err != nil {
		return Grant{}, fmt.Errorf("end sign-in %s: %w", g.ID, err)
	}
	if _, err := w.exec(ctx, `UPDATE grants SET revoked_at = $1, end_reason = $2 WHERE id = $3`, w.now, reason, g.ID); err != nil {
		return Grant{}, fmt.Errorf("end sign-in %s: %w", g.ID, err)
	}
	ended := g
	ended.RevokedAt, ended.EndReason = w.now, reason
	w.indexed(func(ix *index) { ix.dropGrant(g.ID) })
	return ended, w.event(ctx, OpSignInEnd, g.Principal, g.state(), ended.state())
}

// endIdentityGrants ends the live sign-ins made by identity id, which is
// being removed, and lets them forget it.
func (w *cwtx) endIdentityGrants(ctx context.Context, id int64) error {
	gs, err := queryGrants(ctx, w.tx, ` WHERE identity = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return err
	}
	for _, g := range gs {
		if g.EndReason == "" {
			if _, err := w.endGrant(ctx, g, EndIdentityRemoved); err != nil {
				return err
			}
		}
	}
	if _, err := w.exec(ctx, `UPDATE grants SET identity = NULL WHERE identity = $1`, id); err != nil {
		return fmt.Errorf("forget identity %d in its sign-ins: %w", id, err)
	}
	return nil
}

// Grants returns principal's sign-ins that have not been pruned, live
// and ended, oldest first. Reads are not guarded: the caller decides who
// may list them.
func (c *Control) Grants(ctx context.Context, principal string) ([]Grant, error) {
	if err := validPrincipal(principal); err != nil {
		return nil, err
	}
	return queryGrants(ctx, c.s.r, ` WHERE principal = $1 ORDER BY created_at, id`, principal)
}

// Grant returns sign-in id, or ErrNotFound.
func (c *Control) Grant(ctx context.Context, id string) (Grant, error) {
	return readGrant(ctx, c.s.r, id)
}

// TokenGrant returns the live sign-in whose token's hash is hash, from
// memory: what a connection presenting a token is checked against. ok is
// false for a token never issued, or whose sign-in was ended or pruned.
// A sign-in past its end is still returned until pruned; the caller
// compares EndsAt with the store's time.
func (c *Control) TokenGrant(hash string) (Grant, bool) {
	c.ix.mu.RLock()
	defer c.ix.mu.RUnlock()
	g, ok := c.ix.tokens[hash]
	return g, ok
}

// PruneSignIns deletes the sign-ins that ended, or were ended, before
// before, with their tokens: bookkeeping, which records no event (rule
// 7), since each end was history when it came. It returns how many.
func (c *Control) PruneSignIns(ctx context.Context, before time.Time) (int, error) {
	var n int64
	err := c.write(ctx, Actor{Principal: OperatorPrincipal, Session: signInSession, Machine: "ariesd"}, func(w *cwtx) error {
		w.quiet = true
		var err error
		n, err = w.exec(ctx, `DELETE FROM grants WHERE ends_at < $1 OR revoked_at < $1`, before)
		if err != nil {
			return fmt.Errorf("prune sign-ins: %w", err)
		}
		w.indexed(func(ix *index) {
			for h, g := range ix.tokens {
				if g.EndsAt.Before(before) {
					delete(ix.tokens, h)
				}
			}
		})
		return nil
	})
	return int(n), err
}

// dropGrant forgets the tokens of grant id.
func (ix *index) dropGrant(id string) {
	for h, g := range ix.tokens {
		if g.ID == id {
			delete(ix.tokens, h)
		}
	}
}

const grantSelect = `SELECT id, principal, kind, client_id, scopes, COALESCE(jkt, ''), resource, flow, COALESCE(provider, ''),
  COALESCE(identity, 0), machine, created_at, ends_at, revoked_at, COALESCE(end_reason, '') FROM grants`

func scanGrant(r scanner) (Grant, error) {
	var g Grant
	var kind, scopes string
	var revoked sql.NullTime
	if err := r.Scan(&g.ID, &g.Principal, &kind, &g.ClientID, &scopes, &g.JKT, &g.Resource, &g.Flow, &g.Provider, &g.Identity, &g.Machine,
		&g.CreatedAt, &g.EndsAt, &revoked, &g.EndReason); err != nil {
		return Grant{}, err
	}
	g.Kind, g.Scopes = PrincipalKind(kind), strings.Fields(scopes)
	g.CreatedAt, g.EndsAt, g.RevokedAt = g.CreatedAt.UTC(), g.EndsAt.UTC(), utcOrZero(revoked)
	return g, nil
}

func readGrant(ctx context.Context, q querier, id string) (Grant, error) {
	g, err := scanGrant(q.QueryRowContext(ctx, grantSelect+` WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, fmt.Errorf("sign-in %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Grant{}, fmt.Errorf("sign-in %s: %w", id, err)
	}
	return g, nil
}

// queryGrants reads the grants that where, a constant clause with
// placeholders for args, selects.
func queryGrants(ctx context.Context, q querier, where string, args ...any) ([]Grant, error) {
	rows, err := q.QueryContext(ctx, grantSelect+where, args...) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("sign-ins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("sign-ins: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// loadTokens reads the access tokens of the sign-ins that have not
// ended, for the index.
func loadTokens(ctx context.Context, q querier) (map[string]Grant, error) {
	rows, err := q.QueryContext(ctx, `SELECT t.hash, g.id, g.principal, g.kind, g.client_id, g.scopes, COALESCE(g.jkt, ''), g.resource, g.flow,
  COALESCE(g.provider, ''), COALESCE(g.identity, 0), g.machine, g.created_at, g.ends_at, g.revoked_at, COALESCE(g.end_reason, '')
  FROM tokens t JOIN grants g ON g.id = t.grant_id WHERE t.kind = 'access' AND g.revoked_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]Grant{}
	for rows.Next() {
		var hash string
		g, err := scanGrant(scannerFunc(func(dest ...any) error { return rows.Scan(append([]any{&hash}, dest...)...) }))
		if err != nil {
			return nil, err
		}
		out[hash] = g
	}
	return out, rows.Err()
}

// scannerFunc is a func as a scanner.
type scannerFunc func(dest ...any) error

func (f scannerFunc) Scan(dest ...any) error { return f(dest...) }
