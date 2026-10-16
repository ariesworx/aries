package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// Trust rules over the protocol (docs/design/identity.md §7, slice N5):
// what `aries admin trust` asks. The control store decides who may write
// (a server admin, never for their own principal); the server checks
// what only it knows: that the provider is one of its workload: providers,
// and that the rule pins the claims the provider's kind requires. The
// reads are a server admin's, and a service's owner's for its rules.
// Changing a rule's claims, disabling or removing it ends the sign-ins
// it gave, and closes their connections once the write has committed.

// wireTrust is r on the wire.
func wireTrust(r store.TrustRule) proto.TrustRule {
	return proto.TrustRule{Name: r.Name, Principal: r.Principal, Provider: r.Provider, Claims: r.Claims, TokenLifetime: lifetime(r.TokenLifetime),
		State: string(r.State), CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Rev: r.Rev}
}

// lifetime is d as proto.ParseDuration reads it back, whole: in hours,
// minutes or seconds, the largest that divides it.
func lifetime(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

// workloadProvider is the workload provider named name, or a refusal
// that names those there are.
func (s *Server) workloadProvider(name string) (*oidc.Workload, *proto.Error) {
	var names []string
	if a := s.auth; a != nil && a.workload != nil {
		for _, p := range a.workload.Providers {
			if p.Name() == name {
				return p, nil
			}
			names = append(names, p.Name())
		}
	}
	if len(names) == 0 {
		return nil, proto.Errf(proto.CodeInvalid,
			"ask the server's operator to set up the platform under workload: providers in ariesd's config (docs/server.md, Service principals)",
			"this server signs in no service principal by a workload's token: workload: names no provider")
	}
	return nil, proto.Errf(proto.CodeInvalid,
		fmt.Sprintf("give --provider one of %s, or ask the server's operator to add %s under workload: providers in ariesd's config", strings.Join(names, ", "), name),
		fmt.Sprintf("this server has no workload provider %s", name))
}

// requiredClaims refuses claims that leave out one the kind of provider
// p requires.
func requiredClaims(p *oidc.Workload, claims map[string]string) *proto.Error {
	for _, k := range oidc.RequiredClaims(p.Kind()) {
		if _, ok := claims[k]; !ok {
			return proto.Errf(proto.CodeInvalid,
				fmt.Sprintf("add --claim %s=VALUE, the value from the platform's token; a %s rule pins %s", k, p.Kind(), strings.Join(oidc.RequiredClaims(p.Kind()), " and ")),
				fmt.Sprintf("a rule at %s must pin %s, an immutable id of the platform's", p.Name(), k))
		}
	}
	return nil
}

// ruleLifetime reads raw, a rule's lifetime as the request gives it, or
// the server's default when it is empty, and refuses one past the
// server's workload: max_token_lifetime.
func (s *Server) ruleLifetime(raw string) (time.Duration, *proto.Error) {
	most, def := store.MaxTrustLifetime, DefaultWorkloadLifetime
	if a := s.auth; a != nil && a.workload != nil {
		most, def = a.workload.MaxTokenLifetime, a.workload.TokenLifetime
	}
	if raw == "" {
		return def, nil
	}
	d, err := proto.ParseDuration(raw)
	if err != nil || d < store.MinTrustLifetime || d > most || d%time.Second != 0 {
		return 0, proto.Errf(proto.CodeInvalid, fmt.Sprintf("give --token-lifetime from %s to %s, such as 1h", lifetime(store.MinTrustLifetime), lifetime(most)),
			fmt.Sprintf("token lifetime %q is not a duration within the server's workload: max_token_lifetime, %s", raw, lifetime(most)))
	}
	return d, nil
}

// trustAdd adds a trust rule.
func trustAdd(ctx context.Context, s *Server, sess *session, in proto.TrustAddArgs) (any, *proto.Error) {
	const op = proto.OpTrustAdd
	if perr := s.serverOnly(sess, op, ""); perr != nil {
		return nil, perr
	}
	p, perr := s.workloadProvider(in.Provider)
	if perr != nil {
		return nil, perr
	}
	if perr := requiredClaims(p, in.Claims); perr != nil {
		return nil, perr
	}
	d, perr := s.ruleLifetime(in.TokenLifetime)
	if perr != nil {
		return nil, perr
	}
	r, err := s.cfg.Control.AddTrustRule(ctx, sess.actor, store.TrustRule{Name: in.Name, Principal: in.Principal, Provider: in.Provider,
		Claims: in.Claims, TokenLifetime: d})
	if err != nil {
		return nil, s.trustErr(ctx, sess, op, in.Name, err)
	}
	s.Audit(ctx)
	return proto.TrustResult{TrustRule: wireTrust(r), Change: "added", Required: oidc.RequiredClaims(p.Kind())}, nil
}

// trustSet changes a trust rule's claims, lifetime or state, and closes
// the connections of the sign-ins that ended.
func trustSet(ctx context.Context, s *Server, sess *session, in proto.TrustSetArgs) (any, *proto.Error) {
	const op = proto.OpTrustSet
	if in.Claims == nil && in.TokenLifetime == "" && in.State == "" {
		return nil, proto.Errf(proto.CodeInvalid, "give --claim, --token-lifetime, or use `aries admin trust disable` or `enable`", "nothing to set")
	}
	if perr := s.serverOnly(sess, op, ""); perr != nil {
		return nil, perr
	}
	ctl := s.cfg.Control
	var ch store.TrustChange
	if in.Claims != nil {
		cur, err := ctl.TrustRule(ctx, in.Name)
		if err != nil {
			return nil, s.trustErr(ctx, sess, op, in.Name, err)
		}
		// The rule's provider may have left the settings; its claims
		// then cannot be checked, and are not changed.
		p, perr := s.workloadProvider(cur.Provider)
		if perr != nil {
			return nil, perr
		}
		if perr := requiredClaims(p, in.Claims); perr != nil {
			return nil, perr
		}
		ch.Claims = in.Claims
	}
	if in.TokenLifetime != "" {
		d, perr := s.ruleLifetime(in.TokenLifetime)
		if perr != nil {
			return nil, perr
		}
		ch.TokenLifetime = d
	}
	switch in.State {
	case "", proto.TrustActive, proto.TrustDisabled:
		ch.State = store.TrustState(in.State)
	default:
		return nil, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" aries to match the server", fmt.Sprintf("state %q is not active or disabled", in.State))
	}
	r, ended, changed, err := ctl.SetTrustRule(ctx, sess.actor, in.Name, ch)
	if err != nil {
		return nil, s.trustErr(ctx, sess, op, in.Name, err)
	}
	out := proto.TrustResult{TrustRule: wireTrust(r), Ended: len(ended)}
	if changed {
		out.Change = "changed"
		s.Audit(ctx)
		s.endGrants(ctx, ended)
	}
	return out, nil
}

// trustRemove removes a trust rule, and closes the connections of the
// sign-ins that ended.
func trustRemove(ctx context.Context, s *Server, sess *session, in proto.TrustArgs) (any, *proto.Error) {
	const op = proto.OpTrustRemove
	if perr := s.serverOnly(sess, op, ""); perr != nil {
		return nil, perr
	}
	r, ended, err := s.cfg.Control.RemoveTrustRule(ctx, sess.actor, in.Name)
	if err != nil {
		return nil, s.trustErr(ctx, sess, op, in.Name, err)
	}
	s.Audit(ctx)
	s.endGrants(ctx, ended)
	return proto.TrustResult{TrustRule: wireTrust(r), Change: "removed", Ended: len(ended)}, nil
}

// endGrants closes the connections of each sign-in of gs, which ended.
func (s *Server) endGrants(ctx context.Context, gs []store.Grant) {
	for _, g := range gs {
		s.endGrant(ctx, g.ID, g.EndReason)
	}
}

// mayReadTrust refuses a read of principal's trust rules to anyone but a
// server admin and principal's owner, the same way whether or not it
// exists.
func (s *Server) mayReadTrust(ctx context.Context, sess *session, principal string) *proto.Error {
	ctl, actor := s.cfg.Control, sess.actor.Principal
	if ctl.IsServerAdmin(actor) {
		return nil
	}
	if principal != "" {
		if p, err := ctl.Principal(ctx, principal); err == nil && p.Kind == store.KindService && p.Owner == actor {
			return nil
		}
	}
	whose := "every service's"
	if principal != "" {
		whose = principal + "'s"
	}
	return proto.Errf(proto.CodeForbidden, "ask a server admin; a service's owner lists its rules with `aries admin trust list SERVICE`",
		fmt.Sprintf("%s may not read %s trust rules: they are for server admins, and a service's owner", actor, whose))
}

// trustRules lists the trust rules of a service, or of the server.
func trustRules(ctx context.Context, s *Server, sess *session, in proto.TrustRulesArgs) (any, *proto.Error) {
	const op = proto.OpTrustRules
	if perr := s.mayReadTrust(ctx, sess, in.Principal); perr != nil {
		return nil, perr
	}
	ctl := s.cfg.Control
	rs, err := ctl.TrustRules(ctx, in.Principal)
	if err == nil && in.Principal != "" && len(rs) == 0 {
		// A server admin, or the owner, is told when there is no such
		// principal.
		_, err = ctl.Principal(ctx, in.Principal)
	}
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := proto.TrustRulesResult{Rules: make([]proto.TrustRule, 0, len(rs))}
	for _, r := range rs {
		out.Rules = append(out.Rules, wireTrust(r))
	}
	return out, nil
}

// trustShow shows a trust rule, and the claims its provider's kind
// requires.
func trustShow(ctx context.Context, s *Server, sess *session, in proto.TrustArgs) (any, *proto.Error) {
	const op = proto.OpTrustShow
	ctl := s.cfg.Control
	r, err := ctl.TrustRule(ctx, in.Name)
	principal := r.Principal
	if err != nil && !ctl.IsServerAdmin(sess.actor.Principal) {
		// Refused as for a rule of a service the asker does not own.
		principal = ""
	}
	if perr := s.mayReadTrust(ctx, sess, principal); perr != nil {
		return nil, perr
	}
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := proto.TrustResult{TrustRule: wireTrust(r)}
	if p, perr := s.workloadProvider(r.Provider); perr == nil {
		out.Required = oidc.RequiredClaims(p.Kind())
	}
	return out, nil
}

// trustErr words a refusal of a trust rule's write.
func (s *Server) trustErr(ctx context.Context, sess *session, op, name string, err error) *proto.Error {
	if te, ok := errors.AsType[*store.TrustError](err); ok && te.Reason == store.TrustNotService {
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("name a service, or make %s one with `aries admin principal set %s --kind service --owner NAME`", te.Principal, te.Principal),
			te.Error())
	}
	if errors.Is(err, store.ErrExists) {
		return proto.Errf(proto.CodeExists, fmt.Sprintf("choose another name, or change that rule with `aries admin trust set %s`", name),
			fmt.Sprintf("a trust rule named %q already exists", name))
	}
	if le, ok := errors.AsType[*store.ControlLimitError](err); ok && slices.Contains([]string{"trust_rules_per_principal", "trust_rules_per_server"}, le.Limit) {
		sess.proj.observeLimit(op, err)
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("remove a rule no longer used (`aries admin trust list`, then `aries admin trust remove NAME`), or ask the server's operator to raise %s under limits: in ariesd's config", le.Limit),
			strings.TrimPrefix(le.Error(), store.ErrInvalid.Error()+": "))
	}
	return s.ctlErr(ctx, sess, op, err)
}
