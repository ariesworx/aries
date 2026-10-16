package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// Identities and invitations over the protocol
// (docs/design/identity.md §6.5, slice N1): what `aries admin invite`,
// `identity` and `principal set` and `show` ask. The control store
// decides who may write (only a server admin, and nobody onto their own
// principal); the reads are a server admin's, and a principal's own.
// Nothing here binds an identity to a sign-in: that is slice T2's.

// wireIdentity is an identity on the wire, its state at now. The subject
// is never sent.
func wireIdentity(i store.Identity, now time.Time) proto.Identity {
	out := proto.Identity{ID: i.ID, Principal: i.Principal, Provider: i.Provider, Address: i.InvitedAs, State: string(i.State(now)),
		Issuer: i.Issuer, InvitedBy: i.InvitedBy, InvitedAt: i.InvitedAt}
	for _, t := range []struct {
		at  time.Time
		out **time.Time
	}{{i.InviteUntil, &out.ExpiresAt}, {i.BoundAt, &out.BoundAt}, {i.LastSignIn, &out.LastSignIn}} {
		if !t.at.IsZero() {
			at := t.at
			*t.out = &at
		}
	}
	return out
}

// wireIdentities is identities on the wire, never nil.
func wireIdentities(ids []store.Identity, now time.Time) []proto.Identity {
	out := make([]proto.Identity, 0, len(ids))
	for _, i := range ids {
		out = append(out, wireIdentity(i, now))
	}
	return out
}

// inviteAdd invites an address at a provider for a person, or renews
// its invitation there.
func inviteAdd(ctx context.Context, s *Server, sess *session, in proto.InviteAddArgs) (any, *proto.Error) {
	ctl := s.cfg.Control
	id, _, err := ctl.Invite(ctx, sess.actor, in.Name, store.Invitation{Provider: in.Provider, Address: in.Address})
	if err != nil {
		return nil, s.identityErr(ctx, sess, proto.OpInviteAdd, in.Name, err)
	}
	out := proto.IdentityResult{Identity: wireIdentity(id, ctl.Now()), Change: "added"}
	if id.Rev > 1 {
		out.Change = "renewed"
	}
	return out, nil
}

// invites lists the invitations no sign-in has bound, for server admins:
// the addresses are the whole server's.
func invites(ctx context.Context, s *Server, sess *session, in proto.InvitesArgs) (any, *proto.Error) {
	const op = proto.OpInvites
	if perr := s.serverOnly(sess, op, ""); perr != nil {
		return nil, perr
	}
	if in.Limit < 0 || in.Limit > proto.AdminListMax {
		return nil, proto.Errf(proto.CodeInvalid, fmt.Sprintf("give -n from 1 to %d", proto.AdminListMax),
			fmt.Sprintf("limit %d is out of range", in.Limit))
	}
	var after int64
	if in.After != "" {
		var err error
		if after, err = strconv.ParseInt(in.After, 10, 64); err != nil || after < 0 {
			return nil, proto.Errf(proto.CodeInvalid, "--after takes the id of the last invitation listed, a number",
				fmt.Sprintf("%q is no invitation's id", in.After))
		}
	}
	ctl := s.cfg.Control
	rows, more, err := ctl.Invites(ctx, store.InviteQuery{After: after, Limit: cmp.Or(in.Limit, proto.AdminListDefault), Pending: in.Pending})
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	return proto.InvitesResult{Invites: wireIdentities(rows, ctl.Now()), More: more}, nil
}

// identities lists a principal's identities: for a server admin, and for
// the principal itself. Anyone else is refused the same way whether or
// not the principal exists.
func identities(ctx context.Context, s *Server, sess *session, in proto.IdentitiesArgs) (any, *proto.Error) {
	const op = proto.OpIdentities
	ctl, actor := s.cfg.Control, sess.actor.Principal
	admin := ctl.IsServerAdmin(actor)
	if !admin && actor != in.Name {
		return nil, proto.Errf(proto.CodeForbidden,
			"ask a server admin; `aries admin identity list "+actor+"` lists your own",
			fmt.Sprintf("%s may not list the identities of %s: it is for the principal and for server admins", actor, in.Name))
	}
	all, err := ctl.Identities(ctx, in.Name)
	if err == nil && admin && len(all) == 0 {
		// A server admin is told when there is no such principal.
		_, err = ctl.Principal(ctx, in.Name)
	}
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	return proto.IdentitiesResult{Principal: in.Name, Identities: wireIdentities(all, ctl.Now())}, nil
}

// inviteRemove removes an invitation no sign-in has bound.
func inviteRemove(ctx context.Context, s *Server, sess *session, in proto.IdentityArgs) (any, *proto.Error) {
	id, err := s.cfg.Control.RemoveInvite(ctx, sess.actor, in.ID)
	if err != nil {
		return nil, s.identityErr(ctx, sess, proto.OpInviteRemove, "", err)
	}
	return proto.IdentityResult{Identity: wireIdentity(id, s.cfg.Control.Now())}, nil
}

// identityRemove removes an identity, bound or not: no sign-in matches
// it from then on. Nothing a sign-in made with it exists before T2.
func identityRemove(ctx context.Context, s *Server, sess *session, in proto.IdentityArgs) (any, *proto.Error) {
	id, err := s.cfg.Control.RemoveIdentity(ctx, sess.actor, in.ID)
	if err != nil {
		return nil, s.identityErr(ctx, sess, proto.OpIdentityRemove, "", err)
	}
	return proto.IdentityResult{Identity: wireIdentity(id, s.cfg.Control.Now())}, nil
}

// principalSet changes a principal's display name, kind or owner.
func principalSet(ctx context.Context, s *Server, sess *session, in proto.PrincipalSetArgs) (any, *proto.Error) {
	const op = proto.OpPrincipalSet
	if in.DisplayName == nil && in.Kind == "" && in.Owner == "" {
		return nil, proto.Errf(proto.CodeInvalid, "give --display-name, --kind or --owner",
			fmt.Sprintf("principal set changes nothing of %s", in.Name))
	}
	p, changed, err := s.cfg.Control.SetPrincipal(ctx, sess.actor, in.Name,
		store.PrincipalChange{DisplayName: in.DisplayName, Kind: store.PrincipalKind(in.Kind), Owner: in.Owner})
	if err != nil {
		return nil, s.identityErr(ctx, sess, op, in.Name, err)
	}
	out := wirePrincipalResult(p)
	out.Change = "unchanged"
	if changed {
		out.Change = "changed"
	}
	return out, nil
}

// wirePrincipalResult is a principal after a write.
func wirePrincipalResult(p store.Principal) proto.PrincipalResult {
	return proto.PrincipalResult{Name: p.Name, Rev: int64(p.Rev), State: string(p.State), Kind: string(p.Kind), Owner: p.Owner,
		DisplayName: p.DisplayName}
}

// principalShow shows a principal with its identities and keys: to a
// server admin, and to the principal itself. Anyone else is refused the
// same way whether or not the principal exists.
func principalShow(ctx context.Context, s *Server, sess *session, in proto.PrincipalArgs) (any, *proto.Error) {
	const op = proto.OpPrincipalShow
	ctl, actor := s.cfg.Control, sess.actor.Principal
	if !ctl.IsServerAdmin(actor) && actor != in.Name {
		return nil, proto.Errf(proto.CodeForbidden,
			"ask a server admin; `aries admin principal show "+actor+"` shows your own",
			fmt.Sprintf("%s may not show %s: it is for the principal and for server admins", actor, in.Name))
	}
	p, err := ctl.Principal(ctx, in.Name)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	ids, err := ctl.Identities(ctx, in.Name)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	ks, err := ctl.Keys(ctx, in.Name)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := proto.PrincipalShowResult{Principal: wirePrincipal(p), Identities: wireIdentities(ids, ctl.Now()), Keys: []proto.Key{}}
	hand := s.byHand()
	for _, k := range ks {
		out.Keys = append(out.Keys, wireKey(k, hand))
	}
	return out, nil
}

// identityErr is ctlErr for an op on identities or a principal's kind,
// with the fix of each rule's refusal; name is the principal the request
// names, when it names one.
func (s *Server) identityErr(ctx context.Context, sess *session, op, name string, err error) *proto.Error {
	if ie, ok := errors.AsType[*store.IdentityError](err); ok {
		return identityRuleErr(op, ie)
	}
	if le, ok := errors.AsType[*store.ControlLimitError](err); ok && le.Limit == "identities_per_principal" {
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("remove an identity or invitation %s no longer uses (`aries admin identity list %s`, then `aries admin identity remove ID`), or ask the server's operator to raise identities_per_principal under limits: in ariesd's config",
				name, name),
			strings.TrimPrefix(le.Error(), store.ErrInvalid.Error()+": "))
	}
	return s.ctlErr(ctx, sess, op, err)
}

// identityRuleErr refuses what breaks a rule of a principal's kind and
// identities, with the next step for the rule.
func identityRuleErr(op string, e *store.IdentityError) *proto.Error {
	msg := strings.TrimPrefix(e.Error(), store.ErrInvalid.Error()+": ")
	fix := "correct it and retry"
	switch e.Rule {
	case store.IdentityRuleService:
		fix = fmt.Sprintf("a service signs in by a key or a trust rule: invite an address for a person, or make %s a person with `aries admin principal set %s --kind person`",
			e.Principal, e.Principal)
		if op == proto.OpPrincipalSet {
			fix = fmt.Sprintf("remove its identities first: `aries admin identity list %s` lists them, and `aries admin identity remove ID` removes one", e.Principal)
		}
	case store.IdentityRuleOwner:
		fix = "give --owner NAME, a person other than the service itself, who answers for it"
	case store.IdentityRulePersonOwner:
		fix = "leave --owner out for a person, or make it a service with --kind service --owner NAME"
	case store.IdentityRuleOwns:
		fix = fmt.Sprintf("give %s's services another owner first, with `aries admin principal set NAME --owner OWNER`; `aries admin principal list` shows each one's owner", e.Principal)
	case store.IdentityRuleBound:
		fix = fmt.Sprintf("remove it as an identity, with `aries admin identity remove %d`", e.ID)
	}
	return proto.Errf(proto.CodeInvalid, fix, msg)
}
