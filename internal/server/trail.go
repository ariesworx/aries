package server

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// What an account left behind in the control store: the keys it added,
// the principals it created and the memberships it added. Disabling an
// account ends its own access and none of these (design §15.4, As
// built, the review's fixes): a server admin may add a key to another
// admin's account, or make a principal an admin everywhere, and both go
// on working. So `principal disable` counts them, and three lists show
// them: `key list --added-by`, `principal list --created-by` and `member
// list --added-by`.
//
// The read rule is the lists' own. A server admin sees all of it. A
// project admin sees what is within the projects they administer: the
// keys and the principals of those projects' members, and those
// projects' memberships. Anyone else is refused.

// trail is what one principal left behind, as much of it as a caller
// may see.
type trail struct {
	keys       []store.PrincipalKey // by fingerprint
	principals []store.Principal    // by name
	members    []store.Member       // by project id, then principal
}

// trailOf reads what name left behind and keeps what actor may see. It
// refuses an actor who administers nothing; what says what was asked
// for, in the refusal.
func (s *Server) trailOf(ctx context.Context, actor, name, what string) (trail, *proto.Error, error) {
	ctl := s.cfg.Control
	server := ctl.IsServerAdmin(actor)
	if !server {
		of, err := ctl.ProjectsOf(ctx, actor)
		if err != nil {
			return trail{}, nil, err
		}
		if !slices.ContainsFunc(of, func(m store.Member) bool { return s.administers(actor, m.Project) }) {
			return trail{}, proto.Errf(proto.CodeForbidden, "ask a server admin, or an admin of a project, to run it",
				fmt.Sprintf("%s may not list %s: it is for project admins and server admins", actor, what)), nil
		}
	}
	var t trail
	var err error
	if t.keys, err = ctl.KeysAddedBy(ctx, name); err != nil {
		return trail{}, nil, err
	}
	if t.principals, err = ctl.PrincipalsCreatedBy(ctx, name); err != nil {
		return trail{}, nil, err
	}
	if t.members, err = ctl.MembersAddedBy(ctx, name); err != nil {
		return trail{}, nil, err
	}
	if !server {
		t.keys = slices.DeleteFunc(t.keys, func(k store.PrincipalKey) bool { return !ctl.AdministersProjectOf(actor, k.Principal) })
		t.principals = slices.DeleteFunc(t.principals, func(p store.Principal) bool { return !ctl.AdministersProjectOf(actor, p.Name) })
		t.members = slices.DeleteFunc(t.members, func(m store.Member) bool { return !s.administers(actor, m.Project) })
	}
	// The database's order of fingerprints is its collation's, and the
	// pages are cut by Go's.
	slices.SortFunc(t.keys, func(a, b store.PrincipalKey) int { return strings.Compare(a.Fingerprint, b.Fingerprint) })
	return t, nil, nil
}

// administers reports whether actor has an admin's rights in an active
// project.
func (s *Server) administers(actor, project string) bool {
	acc := s.cfg.Control.Access(actor, project)
	return acc.Active && acc.Role == store.RoleAdmin
}

// left adds to a disable's result what the disabled principal left
// behind, and the warning that says disabling undid none of it. A
// failure to read it is logged and says nothing: the principal is
// disabled.
func (s *Server) left(ctx context.Context, sess *session, out *proto.PrincipalResult) {
	t, perr, err := s.trailOf(ctx, sess.actor.Principal, out.Name, "")
	if err != nil {
		s.cfg.Logger.ErrorContext(ctx, "read what a disabled principal added", "event", logs.RequestPart, "subject", out.Name, "err", err)
		return
	}
	if perr != nil {
		return
	}
	out.KeysAdded, out.PrincipalsCreated, out.MembersAdded = len(t.keys), len(t.principals), len(t.members)
	if out.KeysAdded+out.PrincipalsCreated+out.MembersAdded == 0 {
		return
	}
	warning := fmt.Sprintf("%s added %s that %s still in the store, created %s and added %s, and disabling %s undoes none of them: "+
		"review them with `aries admin key list --added-by %s`, `aries admin principal list --created-by %s` and `aries admin member list --added-by %s`",
		out.Name, plural(out.KeysAdded, "key"), isAre(out.KeysAdded), plural(out.PrincipalsCreated, "principal"), plural(out.MembersAdded, "membership"),
		out.Name, out.Name, out.Name, out.Name)
	// One line: a warning is printed as one.
	if out.Warning != "" {
		warning += "; and " + out.Warning
	}
	out.Warning = warning
}

// plural is "1 key" or "3 keys".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// keysAddedBy lists the keys a principal added, whoever each belongs to.
func (s *Server) keysAddedBy(ctx context.Context, sess *session, in proto.KeysArgs) (any, *proto.Error) {
	t, perr, err := s.trailOf(ctx, sess.actor.Principal, in.AddedBy, "the keys "+in.AddedBy+" added")
	if err != nil {
		return nil, s.ctlErr(ctx, sess, proto.OpKeys, err)
	}
	if perr != nil {
		return nil, perr
	}
	rows, more, perr := page(t.keys, func(k store.PrincipalKey) string { return k.Fingerprint }, in.AdminListArgs)
	if perr != nil {
		return nil, perr
	}
	out := proto.KeysResult{AddedBy: in.AddedBy, Keys: []proto.Key{}, More: more}
	hand := s.byHand()
	for _, k := range rows {
		out.Keys = append(out.Keys, proto.Key{Fingerprint: k.Fingerprint, Type: keyType(k.Key), Comment: k.Comment, AddedBy: k.AddedBy, AddedAt: k.AddedAt,
			InAuthorizedKeys: hand[k.Fingerprint], Principal: k.Principal})
	}
	return out, nil
}

// principalsCreatedBy lists the principals a principal created.
func (s *Server) principalsCreatedBy(ctx context.Context, sess *session, in proto.PrincipalsArgs) (any, *proto.Error) {
	t, perr, err := s.trailOf(ctx, sess.actor.Principal, in.CreatedBy, "the principals "+in.CreatedBy+" created")
	if err != nil {
		return nil, s.ctlErr(ctx, sess, proto.OpPrincipals, err)
	}
	if perr != nil {
		return nil, perr
	}
	rows, more, perr := page(t.principals, func(p store.Principal) string { return p.Name }, in.AdminListArgs)
	if perr != nil {
		return nil, perr
	}
	return wirePrincipals(rows, more), nil
}

func wirePrincipals(rows []store.Principal, more int) proto.PrincipalsResult {
	out := proto.PrincipalsResult{Principals: []proto.Principal{}, More: more}
	for _, p := range rows {
		out.Principals = append(out.Principals, wirePrincipal(p))
	}
	return out
}

// wirePrincipal is a principal in a list.
func wirePrincipal(p store.Principal) proto.Principal {
	return proto.Principal{Name: p.Name, State: string(p.State), Kind: string(p.Kind), Owner: p.Owner, DisplayName: p.DisplayName,
		CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt}
}

// membersAddedBy lists the memberships a principal added: in the
// project the request names, or in every project the caller may list
// the members of. Each row names its project; they are by project name
// and then principal, and a page is cut after PROJECT/PRINCIPAL.
func (s *Server) membersAddedBy(ctx context.Context, sess *session, in proto.MembersArgs) (any, *proto.Error) {
	const op = proto.OpMembers
	actor := sess.actor.Principal
	only := ""
	if in.Project != "" {
		p, perr := s.project(ctx, sess, op, in.Project, "")
		if perr != nil {
			return nil, perr
		}
		if s.cfg.Control.Access(actor, p.ID).Role != store.RoleAdmin {
			return nil, accessErr(&store.AccessError{Actor: actor, Op: store.Op(op), Project: in.Project, Reason: store.AccessAdmin})
		}
		only = p.ID
	}
	t, perr, err := s.trailOf(ctx, actor, in.AddedBy, "the memberships "+in.AddedBy+" added")
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	if perr != nil {
		return nil, perr
	}
	var rows []proto.Member
	projects := map[string]store.Project{}
	for _, m := range t.members {
		if only != "" && m.Project != only {
			continue
		}
		p, read := projects[m.Project]
		if !read {
			var err error
			if p, err = s.cfg.Control.Project(ctx, m.Project); err != nil {
				return nil, s.ctlErr(ctx, sess, op, err)
			}
			projects[m.Project] = p
		}
		rows = append(rows, proto.Member{Principal: m.Principal, Role: string(m.Role), AddedBy: m.AddedBy, AddedAt: m.AddedAt, Project: p.ID, ProjectName: p.Name})
	}
	key := func(m proto.Member) string { return m.ProjectName + "/" + m.Principal }
	slices.SortFunc(rows, func(a, b proto.Member) int { return strings.Compare(key(a), key(b)) })
	rows, more, perr := page(rows, key, in.AdminListArgs)
	if perr != nil {
		return nil, perr
	}
	return proto.MembersResult{Members: append([]proto.Member{}, rows...), More: more}, nil
}
