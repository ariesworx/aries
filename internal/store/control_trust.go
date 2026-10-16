package store

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/safetext"
)

// Trust rules (docs/design/identity.md §3.4 and §7, transport.md §7,
// slice N5). A service principal signs in with the token its platform
// already gives it: a CI job's, a cloud workload's. The daemon verifies
// the token against a workload provider of its settings, and gives the
// store the provider and the token's claims; [Control.WorkloadSignIn]
// finds the one service principal whose active rules at that provider
// the claims satisfy, and grants a sign-in for the rule's lifetime. No
// secret is stored anywhere.
//
// A rule's claims are matched exactly, as strings. Which claims a rule
// must pin (a platform's immutable ids) is the daemon's to check, by the
// provider's kind, since only it knows the kinds; the store refuses a
// rule that pins nothing, or only sub, or a value that reads as a
// pattern.

// Event operations of trust rules. Their target is the rule's name, and
// their states the rule's, claims and all: the claims are ids and names
// of the company's own repositories and accounts, not personal data
// (identity.md §3.6).
const (
	OpTrustAdd    Op = "trust.add"
	OpTrustSet    Op = "trust.set"
	OpTrustRemove Op = "trust.remove"
)

// Why a workload's sign-in ended before its end.
const (
	// EndTrustRuleChanged: the rule it matched was disabled, or its
	// claims changed, so that the job it was for may no longer match.
	EndTrustRuleChanged = "trust_rule_changed"
	// EndTrustRuleRemoved: the rule it matched was removed.
	EndTrustRuleRemoved = "trust_rule_removed"
)

// TrustState is whether a rule matches.
type TrustState string

// The states of a trust rule.
const (
	TrustActive   TrustState = "active"
	TrustDisabled TrustState = "disabled"
)

// The bounds of a rule's claims and lifetime.
const (
	// MaxTrustClaims bounds the claims of one rule.
	MaxTrustClaims = 16
	// maxClaimValue bounds a claim's value, in bytes.
	maxClaimValue = 255
	// MinTrustLifetime and MaxTrustLifetime bound what a rule may give;
	// the daemon's workload: max_token_lifetime bounds it further.
	MinTrustLifetime = time.Minute
	MaxTrustLifetime = 24 * time.Hour
)

// claimNamePattern is a claim's name: a token's top-level claim, or a
// path to a nested one with dots, as google.compute_engine.project_id.
var claimNamePattern = lazyregexp.New(`^[A-Za-z0-9_][A-Za-z0-9_.:/-]{0,63}$`)

// TrustRule maps the claims of a workload provider's tokens to a service
// principal.
type TrustRule struct {
	Name      string
	Principal string
	// Provider is the name of a provider under the daemon's workload:
	// providers.
	Provider string
	// Claims each must equal the token's claim of that name.
	Claims map[string]string
	// TokenLifetime is how long a sign-in by the rule lasts.
	TokenLifetime time.Duration
	State         TrustState
	CreatedBy     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Rev           int64
}

// trustState is a rule as its events record it.
type trustState struct {
	Name          string            `json:"name"`
	Principal     string            `json:"principal"`
	Provider      string            `json:"provider"`
	Claims        map[string]string `json:"claims"`
	TokenLifetime int64             `json:"token_lifetime_seconds"`
	State         TrustState        `json:"state"`
	Rev           int64             `json:"rev"`
}

func (r TrustRule) state() trustState {
	return trustState{Name: r.Name, Principal: r.Principal, Provider: r.Provider, Claims: r.Claims,
		TokenLifetime: int64(r.TokenLifetime / time.Second), State: r.State, Rev: r.Rev}
}

// validateClaims refuses, with ErrInvalid, claims a rule may not have.
func validateClaims(rule string, claims map[string]string) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: trust rule %s: "+format, append([]any{ErrInvalid, rule}, a...)...)
	}
	switch {
	case len(claims) == 0:
		return bad("no claims: it would admit every token the provider signs for this server")
	case len(claims) > MaxTrustClaims:
		return bad("%d claims, over %d", len(claims), MaxTrustClaims)
	}
	if _, ok := claims["sub"]; ok && len(claims) == 1 {
		return bad("pins only sub; pin the platform's immutable ids, such as repository_id, as well")
	}
	for _, k := range slices.Sorted(maps.Keys(claims)) {
		v := claims[k]
		switch {
		case !claimNamePattern.MatchString(k):
			return bad("claim %q is not a claim's name: letters, digits and _.:/-, at most 64", safetext.Line(k))
		case v == "" || len(v) > maxClaimValue || !safetext.ValidLine(v):
			return bad("claim %s: a value is one line of 1-%d bytes", k, maxClaimValue)
		case strings.ContainsAny(v, "*?"):
			return bad("claim %s: a value is matched exactly, and may not be a pattern", k)
		}
	}
	return nil
}

// validate refuses, with ErrInvalid, a rule the store would not keep.
func (r TrustRule) validate() error {
	if !PrincipalPattern.MatchString(r.Name) {
		return fmt.Errorf("%w: trust rule %q must be a lowercase letter, then up to 63 lowercase letters, digits, dots, hyphens and underscores", ErrInvalid, r.Name)
	}
	if err := validPrincipal(r.Principal); err != nil {
		return err
	}
	if !providerPattern.MatchString(r.Provider) {
		return fmt.Errorf("%w: provider %q must be a lowercase letter, then up to 31 lowercase letters, digits and hyphens, as the server's workload: settings name it", ErrInvalid, r.Provider)
	}
	if err := validLifetime(r.Name, r.TokenLifetime); err != nil {
		return err
	}
	return validateClaims(r.Name, r.Claims)
}

func validLifetime(rule string, d time.Duration) error {
	if d < MinTrustLifetime || d > MaxTrustLifetime || d%time.Second != 0 {
		return fmt.Errorf("%w: trust rule %s: token lifetime must be whole seconds from %s to %s", ErrInvalid, rule, MinTrustLifetime, MaxTrustLifetime)
	}
	return nil
}

// TrustReason says why a trust rule, or a workload's sign-in, was
// refused.
type TrustReason string

// The reasons a [*TrustError] gives.
const (
	// TrustNotService: Principal is a person, and only a service signs
	// in by a trust rule.
	TrustNotService TrustReason = "not service"
	// TrustNoMatch: no active rule at Provider matches the token.
	TrustNoMatch TrustReason = "no match"
	// TrustAmbiguous: active rules of more than one principal match the
	// token, Rules naming them; the server does not choose.
	TrustAmbiguous TrustReason = "ambiguous"
	// TrustDisabledPrincipal: the rule's principal is disabled.
	TrustDisabledPrincipal TrustReason = "disabled"
	// TrustRules: Principal has trust rules, so it cannot become a
	// person.
	TrustRules TrustReason = "has rules"
)

// TrustError refuses a trust rule or a workload's sign-in. It wraps
// ErrForbidden for a sign-in and ErrInvalid for a rule.
type TrustError struct {
	Reason    TrustReason
	Principal string
	Provider  string
	Rules     []string
}

func (e *TrustError) Error() string {
	switch e.Reason {
	case TrustNotService:
		return fmt.Sprintf("%s is a person; only a service signs in by a trust rule", e.Principal)
	case TrustAmbiguous:
		return fmt.Sprintf("trust rules for more than one principal match this token at %s (%s), and the server does not choose between them",
			e.Provider, strings.Join(e.Rules, ", "))
	case TrustDisabledPrincipal:
		return fmt.Sprintf("%s is disabled on this server", e.Principal)
	case TrustRules:
		return fmt.Sprintf("%s has trust rules, by which it signs in as a service, so it cannot be a person", e.Principal)
	}
	return fmt.Sprintf("no trust rule at %s matches this token", e.Provider)
}

// Unwrap makes errors.Is hold for ErrForbidden, for a refused sign-in,
// or ErrInvalid, for a refused rule.
func (e *TrustError) Unwrap() error {
	switch e.Reason {
	case TrustNoMatch, TrustAmbiguous, TrustDisabledPrincipal:
		return ErrForbidden
	}
	return ErrInvalid
}

// ownTrust refuses a trust rule for principal when the actor is
// principal (decision 19): a rule lets whoever runs the jobs it admits
// sign in as the principal.
func (w *cwtx) ownTrust(op Op, principal string) error {
	if w.actor.Principal == principal {
		return w.denied(op, principal, "", AccessOwnTrust)
	}
	return nil
}

// AddTrustRule adds r, active, as the actor, and returns it. Only a
// server admin may, and not for their own principal: an [*AccessError]
// whose reason is [AccessServerAdmin] or [AccessOwnTrust]. Its principal
// must exist (ErrNotFound) and be a service (a [*TrustError]); a name in
// use is refused with ErrExists; a rule past ControlLimits.TrustRules or
// ControlLimits.TrustRulesPerPrincipal with a [*ControlLimitError]; and
// a rule that is not one with ErrInvalid.
func (c *Control) AddTrustRule(ctx context.Context, actor Actor, r TrustRule) (TrustRule, error) {
	if err := r.validate(); err != nil {
		return TrustRule{}, err
	}
	var out TrustRule
	err := c.write(ctx, actor, func(w *cwtx) error {
		if err := w.ownTrust(OpTrustAdd, r.Principal); err != nil {
			return err
		}
		if err := w.serverOnly(OpTrustAdd, r.Principal, ""); err != nil {
			return err
		}
		p, err := readPrincipal(ctx, w.tx, r.Principal)
		if err != nil {
			return err
		}
		if p.Kind != KindService {
			return &TrustError{Reason: TrustNotService, Principal: p.Name}
		}
		switch _, err := readTrustRule(ctx, w.tx, r.Name); {
		case err == nil:
			return fmt.Errorf("trust rule %s: %w", r.Name, ErrExists)
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if err := w.room(ctx, limitTrustRules, w.lim.TrustRules, `SELECT COUNT(*) FROM trust_rules`); err != nil {
			return err
		}
		if err := w.room(ctx, limitTrustPerPrincipal, w.lim.TrustRulesPerPrincipal, `SELECT COUNT(*) FROM trust_rules WHERE principal = $1`, p.Name); err != nil {
			return err
		}
		claims, err := json.Marshal(r.Claims)
		if err != nil {
			return fmt.Errorf("trust rule %s: %w", r.Name, err)
		}
		out = TrustRule{Name: r.Name, Principal: p.Name, Provider: r.Provider, Claims: maps.Clone(r.Claims), TokenLifetime: r.TokenLifetime,
			State: TrustActive, CreatedBy: w.actor.Principal, CreatedAt: w.now, UpdatedAt: w.now, Rev: 1}
		if _, err := w.exec(ctx, `INSERT INTO trust_rules (name, principal, provider, claims, token_lifetime_seconds, state, created_by, created_at, updated_at, rev)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, 1)`, out.Name, out.Principal, out.Provider, string(claims), int64(out.TokenLifetime/time.Second),
			string(out.State), out.CreatedBy, w.now); err != nil {
			return fmt.Errorf("add trust rule %s: %w", r.Name, err)
		}
		return w.event(ctx, OpTrustAdd, out.Name, nil, out.state())
	})
	if err != nil {
		return TrustRule{}, err
	}
	return out, nil
}

// TrustChange is what [Control.SetTrustRule] changes. A field left at its
// zero value is not changed.
type TrustChange struct {
	// Claims, when set, replace the rule's claims.
	Claims map[string]string
	// TokenLifetime, when set, is the new lifetime of the sign-ins it
	// gives; it does not shorten those already given.
	TokenLifetime time.Duration
	// State, when set, enables or disables the rule.
	State TrustState
}

// SetTrustRule changes rule name's claims, lifetime or state, and
// returns it with the sign-ins it ended; changed is false when it already
// was as asked, and then nothing is recorded. Changing the claims or
// disabling the rule ends the rule's live sign-ins in the same write,
// each with a signin.end event (EndTrustRuleChanged): a job admitted
// before may not be now. Who may is as for [Control.AddTrustRule]; a
// rule that does not exist is refused with ErrNotFound.
func (c *Control) SetTrustRule(ctx context.Context, actor Actor, name string, ch TrustChange) (TrustRule, []Grant, bool, error) {
	if ch.Claims != nil {
		if err := validateClaims(name, ch.Claims); err != nil {
			return TrustRule{}, nil, false, err
		}
	}
	if ch.TokenLifetime != 0 {
		if err := validLifetime(name, ch.TokenLifetime); err != nil {
			return TrustRule{}, nil, false, err
		}
	}
	if ch.State != "" && ch.State != TrustActive && ch.State != TrustDisabled {
		return TrustRule{}, nil, false, fmt.Errorf("%w: trust rule %s: state %q is not active or disabled", ErrInvalid, name, ch.State)
	}
	var out TrustRule
	var ended []Grant
	var changed bool
	err := c.write(ctx, actor, func(w *cwtx) error {
		changed, ended = false, nil
		cur, err := readTrustRule(ctx, w.tx, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// Who may is said before whether the rule exists.
				if err := w.serverOnly(OpTrustSet, "", ""); err != nil {
					return err
				}
			}
			return err
		}
		if err := w.ownTrust(OpTrustSet, cur.Principal); err != nil {
			return err
		}
		if err := w.serverOnly(OpTrustSet, cur.Principal, ""); err != nil {
			return err
		}
		next := cur
		if ch.Claims != nil {
			next.Claims = maps.Clone(ch.Claims)
		}
		next.TokenLifetime = cmp.Or(ch.TokenLifetime, cur.TokenLifetime)
		next.State = cmp.Or(ch.State, cur.State)
		out = cur
		claimsChanged := !maps.Equal(next.Claims, cur.Claims)
		if !claimsChanged && next.TokenLifetime == cur.TokenLifetime && next.State == cur.State {
			return nil
		}
		claims, err := json.Marshal(next.Claims)
		if err != nil {
			return fmt.Errorf("trust rule %s: %w", name, err)
		}
		n, err := w.exec(ctx, `UPDATE trust_rules SET claims = $1, token_lifetime_seconds = $2, state = $3, updated_at = $4, rev = rev + 1
  WHERE name = $5 AND rev = $6`, string(claims), int64(next.TokenLifetime/time.Second), string(next.State), w.now, name, cur.Rev)
		if err != nil {
			return fmt.Errorf("%s %s: %w", OpTrustSet, name, err)
		}
		if n == 0 {
			return fmt.Errorf("%w: trust rule %s changed during %s", errRetry, name, OpTrustSet)
		}
		next.UpdatedAt, next.Rev = w.now, cur.Rev+1
		if err := w.event(ctx, OpTrustSet, name, cur.state(), next.state()); err != nil {
			return err
		}
		if claimsChanged || next.State == TrustDisabled && cur.State != TrustDisabled {
			if ended, err = w.endRuleGrants(ctx, name, EndTrustRuleChanged); err != nil {
				return err
			}
		}
		out, changed = next, true
		return nil
	})
	if err != nil {
		return TrustRule{}, nil, false, err
	}
	return out, ended, changed, nil
}

// RemoveTrustRule removes rule name, and returns it with the sign-ins it
// ended: its live sign-ins end in the same write, each with a signin.end
// event (EndTrustRuleRemoved), and its sign-ins forget it. Who may is as
// for [Control.AddTrustRule]; a rule that does not exist is refused with
// ErrNotFound.
func (c *Control) RemoveTrustRule(ctx context.Context, actor Actor, name string) (TrustRule, []Grant, error) {
	var out TrustRule
	var ended []Grant
	err := c.write(ctx, actor, func(w *cwtx) error {
		ended = nil
		cur, err := readTrustRule(ctx, w.tx, name)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				if err := w.serverOnly(OpTrustRemove, "", ""); err != nil {
					return err
				}
			}
			return err
		}
		if err := w.ownTrust(OpTrustRemove, cur.Principal); err != nil {
			return err
		}
		if err := w.serverOnly(OpTrustRemove, cur.Principal, ""); err != nil {
			return err
		}
		if ended, err = w.endRuleGrants(ctx, name, EndTrustRuleRemoved); err != nil {
			return err
		}
		if _, err := w.exec(ctx, `UPDATE grants SET trust_rule = NULL WHERE trust_rule = $1`, name); err != nil {
			return fmt.Errorf("forget trust rule %s in its sign-ins: %w", name, err)
		}
		if _, err := w.exec(ctx, `DELETE FROM trust_rules WHERE name = $1`, name); err != nil {
			return fmt.Errorf("remove trust rule %s: %w", name, err)
		}
		out = cur
		return w.event(ctx, OpTrustRemove, name, cur.state(), nil)
	})
	if err != nil {
		return TrustRule{}, nil, err
	}
	return out, ended, nil
}

// endRuleGrants ends the live sign-ins of trust rule name for reason, and
// returns them as ended.
func (w *cwtx) endRuleGrants(ctx context.Context, name, reason string) ([]Grant, error) {
	gs, err := queryGrants(ctx, w.tx, ` WHERE trust_rule = $1 AND revoked_at IS NULL ORDER BY created_at, id`, name)
	if err != nil {
		return nil, err
	}
	var out []Grant
	for _, g := range gs {
		e, err := w.endGrant(ctx, g, reason)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// TrustRules returns the rules of principal, or of every principal when
// principal is empty, by name. Reads are not guarded: the caller decides
// who may list them.
func (c *Control) TrustRules(ctx context.Context, principal string) ([]TrustRule, error) {
	if principal == "" {
		return queryTrustRules(ctx, c.s.r, ` ORDER BY name`)
	}
	if err := validPrincipal(principal); err != nil {
		return nil, err
	}
	return queryTrustRules(ctx, c.s.r, ` WHERE principal = $1 ORDER BY name`, principal)
}

// TrustRule returns rule name, or ErrNotFound.
func (c *Control) TrustRule(ctx context.Context, name string) (TrustRule, error) {
	return readTrustRule(ctx, c.s.r, name)
}

// WorkloadSignIn is a verified workload token, for [Control.WorkloadSignIn].
type WorkloadSignIn struct {
	// Provider is the workload provider's name in the server's settings,
	// which a rule names.
	Provider string
	// Claims are the token's claims whose values are strings, nested
	// ones by their path with dots: what a rule's claims are matched
	// against.
	Claims map[string]string
	// Launch are the claims that say which run of which job it was
	// (identity.md §7.5), for the signin.grant event alone.
	Launch map[string]string
	// ClientID, Scopes, JKT, Resource and Machine are as a person's
	// sign-in has them.
	ClientID string
	Scopes   []string
	JKT      string
	Resource string
	Machine  string
	// MaxLifetime bounds the rule's lifetime: the server's
	// workload: max_token_lifetime.
	MaxLifetime time.Duration
	// TokenHash is the SHA-256 of the access token the daemon made, in
	// lowercase hex.
	TokenHash string
}

func (in WorkloadSignIn) validate() error {
	si := SignIn{Provider: in.Provider, Issuer: "workload", Subject: "workload", ClientID: in.ClientID, Scopes: in.Scopes, JKT: in.JKT,
		Resource: in.Resource, Flow: FlowWorkload, Machine: in.Machine, Lifetime: in.MaxLifetime, TokenHash: in.TokenHash}
	return si.validate()
}

// WorkloadSignIn records a verified workload token's sign-in, and returns
// its grant and the rule it matched: the active rules at in.Provider
// whose claims all equal the token's must all be one principal's, who
// is an active service; of that principal's, the one with the most
// claims, then the first by name, is the rule. The grant lasts the
// rule's lifetime, at most in.MaxLifetime, and is written, with a
// signin.grant event naming the rule and the launch, by the principal.
//
// No rule matching is refused with a [*TrustError] (TrustNoMatch), rules
// of two principals with one (TrustAmbiguous), and a disabled principal
// with one (TrustDisabledPrincipal). A principal with
// ControlLimits.Grants live sign-ins, or a rule with
// ControlLimits.GrantsPerTrustRule, is refused with a
// [*ControlLimitError].
func (c *Control) WorkloadSignIn(ctx context.Context, in WorkloadSignIn) (Grant, TrustRule, error) {
	if err := in.validate(); err != nil {
		return Grant{}, TrustRule{}, err
	}
	r, err := matchTrust(ctx, c.s.r, in)
	if err != nil {
		return Grant{}, TrustRule{}, err
	}
	var out Grant
	actor := Actor{Principal: r.Principal, Session: signInSession, Machine: in.Machine}
	err = c.write(ctx, actor, func(w *cwtx) error {
		r, err = matchTrust(ctx, w.tx, in)
		if err != nil {
			return err
		}
		w.actor.Principal = r.Principal
		p, err := readPrincipal(ctx, w.tx, r.Principal)
		if err != nil {
			return err
		}
		switch {
		case p.State != PrincipalActive:
			return &TrustError{Reason: TrustDisabledPrincipal, Principal: p.Name, Provider: in.Provider}
		case p.Kind != KindService:
			return &TrustError{Reason: TrustNotService, Principal: p.Name}
		}
		if err := w.room(ctx, limitGrants, w.lim.Grants, `SELECT COUNT(*) FROM grants WHERE principal = $1 AND revoked_at IS NULL AND ends_at > $2`,
			p.Name, w.now); err != nil {
			return err
		}
		if err := w.room(ctx, limitGrantsPerRule, w.lim.GrantsPerTrustRule, `SELECT COUNT(*) FROM grants WHERE trust_rule = $1 AND revoked_at IS NULL AND ends_at > $2`,
			r.Name, w.now); err != nil {
			return err
		}
		g := Grant{ID: "g-" + strings.ToLower(rand.Text()[:16]), Principal: p.Name, Kind: p.Kind, ClientID: in.ClientID,
			Scopes: slices.Clone(in.Scopes), JKT: in.JKT, Resource: in.Resource, Flow: FlowWorkload, Provider: in.Provider,
			TrustRule: r.Name, Machine: in.Machine, CreatedAt: w.now, EndsAt: w.now.Add(min(r.TokenLifetime, in.MaxLifetime))}
		if _, err := w.exec(ctx, `INSERT INTO grants (id, principal, kind, client_id, scopes, jkt, resource, flow, provider, trust_rule, machine, created_at, ends_at)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`, g.ID, g.Principal, string(g.Kind), g.ClientID, strings.Join(g.Scopes, " "),
			nullStr(g.JKT), g.Resource, g.Flow, g.Provider, g.TrustRule, g.Machine, g.CreatedAt, g.EndsAt); err != nil {
			return fmt.Errorf("grant a sign-in to %s: %w", p.Name, err)
		}
		if _, err := w.exec(ctx, `INSERT INTO tokens (hash, grant_id, kind, expires_at) VALUES ($1, $2, 'access', $3)`,
			in.TokenHash, g.ID, g.EndsAt); err != nil {
			return fmt.Errorf("keep the sign-in's token: %w", err)
		}
		out = g
		w.indexed(func(ix *index) { ix.tokens[in.TokenHash] = g })
		st := g.state()
		st.Launch = launchState(in.Launch)
		return w.event(ctx, OpSignInGrant, p.Name, nil, st)
	})
	if err != nil {
		return Grant{}, TrustRule{}, err
	}
	return out, r, nil
}

// launchState is what of launch an event keeps: each value one line of
// at most maxClaimValue bytes, others dropped.
func launchState(launch map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range launch {
		if claimNamePattern.MatchString(k) && v != "" && len(v) <= maxClaimValue && safetext.ValidLine(v) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// matchTrust finds the rule in matches, as WorkloadSignIn says, reading
// through q.
func matchTrust(ctx context.Context, q querier, in WorkloadSignIn) (TrustRule, error) {
	rules, err := queryTrustRules(ctx, q, ` WHERE provider = $1 AND state = 'active' ORDER BY name`, in.Provider)
	if err != nil {
		return TrustRule{}, err
	}
	var hits []TrustRule
	for _, r := range rules {
		if matchesClaims(r.Claims, in.Claims) {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		return TrustRule{}, &TrustError{Reason: TrustNoMatch, Provider: in.Provider}
	}
	if slices.ContainsFunc(hits, func(r TrustRule) bool { return r.Principal != hits[0].Principal }) {
		names := make([]string, len(hits))
		for i, r := range hits {
			names[i] = r.Name
		}
		return TrustRule{}, &TrustError{Reason: TrustAmbiguous, Provider: in.Provider, Rules: names}
	}
	// The most specific of one principal's rules; hits is by name.
	best := hits[0]
	for _, r := range hits[1:] {
		if len(r.Claims) > len(best.Claims) {
			best = r
		}
	}
	return best, nil
}

// matchesClaims reports whether each of want equals the claim of its
// name in got.
func matchesClaims(want, got map[string]string) bool {
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			return false
		}
	}
	return true
}

const trustSelect = `SELECT name, principal, provider, claims, token_lifetime_seconds, state, created_by, created_at, updated_at, rev FROM trust_rules`

func scanTrustRule(r scanner) (TrustRule, error) {
	var t TrustRule
	var claims []byte
	var lifetime int64
	var state string
	if err := r.Scan(&t.Name, &t.Principal, &t.Provider, &claims, &lifetime, &state, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &t.Rev); err != nil {
		return TrustRule{}, err
	}
	if err := json.Unmarshal(claims, &t.Claims); err != nil {
		return TrustRule{}, fmt.Errorf("trust rule %s: claims: %w", t.Name, err)
	}
	t.TokenLifetime, t.State = time.Duration(lifetime)*time.Second, TrustState(state)
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	return t, nil
}

func readTrustRule(ctx context.Context, q querier, name string) (TrustRule, error) {
	t, err := scanTrustRule(q.QueryRowContext(ctx, trustSelect+` WHERE name = $1`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return TrustRule{}, fmt.Errorf("trust rule %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return TrustRule{}, fmt.Errorf("trust rule %s: %w", name, err)
	}
	return t, nil
}

// queryTrustRules reads the rules that where, a constant clause with
// placeholders for args, selects.
func queryTrustRules(ctx context.Context, q querier, where string, args ...any) ([]TrustRule, error) {
	rows, err := q.QueryContext(ctx, trustSelect+where, args...) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("trust rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TrustRule
	for rows.Next() {
		t, err := scanTrustRule(rows)
		if err != nil {
			return nil, fmt.Errorf("trust rules: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// hasTrustRules refuses, with a *TrustError, making service name a person
// while it has trust rules.
func (w *cwtx) hasTrustRules(ctx context.Context, name string) error {
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM trust_rules WHERE principal = $1`, name).Scan(&n); err != nil {
		return fmt.Errorf("read trust rules of %s: %w", name, err)
	}
	if n > 0 {
		return &TrustError{Reason: TrustRules, Principal: name}
	}
	return nil
}
