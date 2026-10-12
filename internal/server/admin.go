package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// Access management over the protocol (design §15.4, protocols 5 and 6):
// what `sfx admin` asks of a server of many projects. Each op rides the
// caller's connection to a project it may reach, and acts on that
// project unless it names another.
//
// Who may write is the control store's to decide (store/control_authz.go):
// the handlers pass the connection's principal as the actor and word the
// store's refusal. The store does not guard reads, so the lists apply
// the read rules here. When a write ends someone's access, their open
// connections are closed once it has committed (drop), and the claims
// they held are ended (release, decision 21). A write that changes
// which keys sign in is followed by a write of the keys file sshd reads
// (syncKeys), before the admin is answered.

// adminHandler runs one admin op for the session sess.
type adminHandler func(ctx context.Context, s *Server, sess *session, args json.RawMessage) (any, *proto.Error)

// adminHandlers maps each admin op to its handler.
var adminHandlers = map[string]adminHandler{
	proto.OpProjectAdd:       adminTyped(projectAdd),
	proto.OpProjects:         adminTyped(projects),
	proto.OpProjectArchive:   adminTyped(projectArchive),
	proto.OpProjectActivate:  adminTyped(projectActivate),
	proto.OpPrincipalAdd:     adminTyped(principalAdd),
	proto.OpPrincipals:       adminTyped(principals),
	proto.OpPrincipalDisable: adminTyped(principalDisable),
	proto.OpPrincipalEnable:  adminTyped(principalEnable),
	proto.OpMemberSet:        adminTyped(memberSet),
	proto.OpMembers:          adminTyped(members),
	proto.OpMemberRemove:     adminTyped(memberRemove),
	proto.OpKeyAdd:           adminTyped(keyAdd),
	proto.OpKeys:             adminTyped(keys),
	proto.OpKeyRemove:        adminTyped(keyRemove),
	proto.OpStatus:           adminTyped(status),
}

// adminTyped decodes args strictly into A and calls fn.
func adminTyped[A any](fn func(ctx context.Context, s *Server, sess *session, args A) (any, *proto.Error)) adminHandler {
	return func(ctx context.Context, s *Server, sess *session, raw json.RawMessage) (any, *proto.Error) {
		args, perr := decodeArgs[A](raw)
		if perr != nil {
			return nil, perr
		}
		return fn(ctx, s, sess, args)
	}
}

// decodeArgs decodes an op's arguments into A, refusing unknown fields.
func decodeArgs[A any](raw json.RawMessage) (A, *proto.Error) {
	var args A
	if len(raw) == 0 {
		return args, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return args, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" starfix to match the server",
			fmt.Sprintf("bad arguments: %v", err))
	}
	return args, nil
}

// oneProject refuses an admin op on a server of one project, which has
// no control store to change.
func oneProject() *proto.Error {
	return proto.Errf(proto.CodeInvalid,
		"the server's operator opts in by setting control_db (docs/server.md, Serving many projects, Opt in); until then people are added as lines of authorized_keys",
		"this server holds one project, so it has no projects, principals or members to manage")
}

// admin runs one admin op for sess. The caller has checked adminOp(op).
func (s *Server) admin(ctx context.Context, sess *session, op string, args json.RawMessage) (any, *proto.Error) {
	if s.cfg.Control == nil {
		return nil, oneProject()
	}
	return adminHandlers[op](ctx, s, sess, args)
}

// ctlErr words a control store error for op.
func (s *Server) ctlErr(ctx context.Context, sess *session, op string, err error) *proto.Error {
	return sess.proj.mapErr(ctx, op, "", 0, err)
}

// serverOnly refuses op to anyone but a server admin, in the control
// store's own terms, before the handler reads anything: so that the
// refusal does not depend on whether what the request names exists.
func (s *Server) serverOnly(sess *session, op, project string) *proto.Error {
	if s.cfg.Control.IsServerAdmin(sess.actor.Principal) {
		return nil
	}
	return accessErr(&store.AccessError{Actor: sess.actor.Principal, Op: store.Op(op), Project: project, Reason: store.AccessServerAdmin})
}

// project resolves the project an admin op acts on: ref, a project's
// UUID or its name, or the connection's own when ref is empty. principal
// is the principal the request names, for the refusal's wording.
//
// A server admin is told when no such project exists. Anyone else is
// refused the same way for a project that does not exist as for one they
// have no role in, so the answer does not say which names are taken.
func (s *Server) project(ctx context.Context, sess *session, op, ref, principal string) (store.Project, *proto.Error) {
	ctl, actor := s.cfg.Control, sess.actor.Principal
	var p store.Project
	var err error
	switch {
	case ref == "":
		p, err = ctl.Project(ctx, sess.proj.id)
	case uuidPattern.MatchString(ref):
		p, err = ctl.Project(ctx, ref)
	default:
		var ok bool
		if p, ok = ctl.ProjectNamed(ref); !ok {
			err = fmt.Errorf("project %q: %w", ref, store.ErrNotFound)
		}
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return p, s.ctlErr(ctx, sess, op, err)
	}
	if ctl.IsServerAdmin(actor) {
		if err != nil {
			return p, proto.Errf(proto.CodeNotFound, "list the projects with `sfx admin project list`", fmt.Sprintf("project %q not found", ref))
		}
		return p, nil
	}
	if err != nil || ctl.Access(actor, p.ID).Role == "" {
		return p, accessErr(&store.AccessError{Actor: actor, Op: store.Op(op), Principal: principal, Project: ref, Reason: store.AccessAdmin})
	}
	return p, nil
}

// open opens p's store, which migrates its database, and gives it back:
// what makes a project ready to be served before it is marked active.
// The store stays open, idle, for the connection likely to follow.
func (s *Server) open(ctx context.Context, op string, p store.Project) *proto.Error {
	held, err := s.stores.acquire(ctx, p)
	if err == nil {
		s.stores.release(held)
		return nil
	}
	switch {
	case errors.Is(err, errStoresFull):
		return proto.Errf(proto.CodeBusy,
			"retry in a minute; the server admin can raise open_stores under limits: in starfixd's config",
			fmt.Sprintf("the server has its limit of %d projects open, each with a connection", s.cfg.Limits.OpenStores))
	case store.DatabaseDenied(err):
		account, aerr := s.cfg.Control.Account(ctx)
		if aerr != nil {
			account = "'starfix'@'localhost'"
		}
		return proto.Errf(proto.CodeUnavailable,
			fmt.Sprintf("ask the server's operator to run this as Dolt's root, then run `sfx %s %s` again: GRANT ALL ON %s.* TO %s; (docs/server.md, Serving many projects)",
				command(op), p.Name, p.DBName, account),
			fmt.Sprintf("starfixd's Dolt account was never granted database %s, so project %s cannot use it", p.DBName, p.Name))
	}
	if ctx.Err() == nil {
		s.cfg.Logger.Error("open project store", "project", p.ID, "database", p.DBName, "err", err)
	}
	return proto.Errf(proto.CodeUnavailable,
		fmt.Sprintf("run `sfx %s %s` again; if it persists, the server admin should check the starfixd log", command(op), p.Name),
		fmt.Sprintf("the server could not open the database of project %s", p.Name))
}

// newProjectID returns a random version 4 UUID, in lowercase.
func newProjectID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// projectAdd creates a project: its row, its database from the block of
// names the operator granted, its schema, and then its activation. Every
// step is safe to repeat, so a request for a project still being created
// picks up where the last one stopped (design §15.5, 1).
func projectAdd(ctx context.Context, s *Server, sess *session, in proto.ProjectAddArgs) (any, *proto.Error) {
	const op = proto.OpProjectAdd
	ctl, a := s.cfg.Control, sess.actor
	if perr := s.serverOnly(sess, op, ""); perr != nil {
		return nil, perr
	}
	s.adding.Lock()
	defer s.adding.Unlock()
	all, err := ctl.Projects(ctx)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	used := map[string]bool{}
	var np store.NewProject
	resumed := false
	for _, p := range all {
		used[p.DBName] = true
		if p.Name != in.Name {
			continue
		}
		if p.State != store.ProjectCreating {
			return nil, takenErr(&store.TakenError{Kind: store.TakenProjectName, Value: in.Name})
		}
		if (in.Prefix != "" && in.Prefix != p.Prefix) || (in.Account != "" && in.Account != p.Account) {
			return nil, proto.Errf(proto.CodeInvalid,
				fmt.Sprintf("run `sfx admin project add %s` without --prefix and --account to finish creating it", p.Name),
				fmt.Sprintf("project %s is being created with prefix %s and account %s, which cannot change", p.Name, p.Prefix, p.Account))
		}
		np, resumed = store.NewProject{ID: p.ID, Name: p.Name, Prefix: p.Prefix, Account: p.Account, DBName: p.DBName}, true
	}
	if !resumed {
		b := s.cfg.ProjectDBs
		if b.Count == 0 {
			return nil, proto.Errf(proto.CodeInvalid,
				"ask the server's operator to grant starfixd's Dolt account a block of database names and set project_dbs: in its config (docs/server.md, Serving many projects)",
				"the server has no database names for new projects")
		}
		name, ok := b.free(used)
		if !ok {
			return nil, proto.Errf(proto.CodeInvalid,
				fmt.Sprintf("ask the server's operator to grant starfixd's Dolt account more names, from %s on, and raise count under project_dbs: in its config (docs/server.md, Serving many projects)",
					b.Name(b.Count+1)),
				fmt.Sprintf("every database name for new projects is in use (%s to %s)", b.Name(1), b.Name(b.Count)))
		}
		np = store.NewProject{ID: newProjectID(), Name: in.Name, Prefix: cmp.Or(in.Prefix, in.Name), Account: cmp.Or(in.Account, DefaultAccount), DBName: name}
	}
	p, err := ctl.AddProject(ctx, a, np)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	if err := ctl.CreateDatabase(ctx, a, p.ID); err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	if perr := s.open(ctx, op, p); perr != nil {
		return nil, perr
	}
	if p, err = ctl.ActivateProject(ctx, a, p.ID); err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	s.cfg.Logger.Info("project added", "project", p.ID, "name", p.Name, "database", p.DBName, "by", a.Principal, "resumed", resumed)
	return proto.ProjectAddResult{ProjectResult: wireProjectResult(p), Database: p.DBName, Resumed: resumed}, nil
}

func wireProjectResult(p store.Project) proto.ProjectResult {
	return proto.ProjectResult{ID: p.ID, Rev: int64(p.Rev), Name: p.Name, State: string(p.State)}
}

// page returns the rows of an admin list a request asks for: those
// whose name sorts after in.After, at most in.Limit, and how many come
// after them. rows are sorted by name.
func page[T any](rows []T, name func(T) string, in proto.AdminListArgs) ([]T, int, *proto.Error) {
	if in.Limit < 0 || in.Limit > proto.AdminListMax {
		return nil, 0, proto.Errf(proto.CodeInvalid, fmt.Sprintf("give -n from 1 to %d", proto.AdminListMax),
			fmt.Sprintf("limit %d is out of range", in.Limit))
	}
	limit := cmp.Or(in.Limit, proto.AdminListDefault)
	if in.After != "" {
		i, found := slices.BinarySearchFunc(rows, in.After, func(r T, after string) int { return strings.Compare(name(r), after) })
		if found {
			i++
		}
		rows = rows[i:]
	}
	n := min(len(rows), limit)
	return rows[:n], len(rows) - n, nil
}

// projects lists every project for a server admin, and for anyone else
// the active projects they belong to.
func projects(ctx context.Context, s *Server, sess *session, in proto.AdminListArgs) (any, *proto.Error) {
	const op = proto.OpProjects
	ctl, actor := s.cfg.Control, sess.actor.Principal
	var rows []proto.Project
	if ctl.IsServerAdmin(actor) {
		all, err := ctl.Projects(ctx)
		if err != nil {
			return nil, s.ctlErr(ctx, sess, op, err)
		}
		for _, p := range all {
			rows = append(rows, proto.Project{ID: p.ID, Name: p.Name, Prefix: p.Prefix, Account: p.Account, State: string(p.State),
				Database: p.DBName, CreatedBy: p.CreatedBy})
		}
	} else {
		of, err := ctl.ProjectsOf(ctx, actor)
		if err != nil {
			return nil, s.ctlErr(ctx, sess, op, err)
		}
		for _, m := range of {
			if acc := ctl.Access(actor, m.Project); acc.Active && acc.Role != "" {
				p := acc.Project
				rows = append(rows, proto.Project{ID: p.ID, Name: p.Name, Prefix: p.Prefix, Account: p.Account, State: string(p.State),
					Role: string(acc.Role)})
			}
		}
		slices.SortFunc(rows, func(a, b proto.Project) int { return strings.Compare(a.Name, b.Name) })
	}
	rows, more, perr := page(rows, func(p proto.Project) string { return p.Name }, in)
	if perr != nil {
		return nil, perr
	}
	return proto.ProjectsResult{Projects: append([]proto.Project{}, rows...), More: more}, nil
}

// projectArchive archives a project, then closes every connection to it
// and its store: an archived project is served to no one. It refuses the
// server's last active project.
func projectArchive(ctx context.Context, s *Server, sess *session, in proto.ProjectArgs) (any, *proto.Error) {
	const op = proto.OpProjectArchive
	if perr := s.serverOnly(sess, op, in.Project); perr != nil {
		return nil, perr
	}
	p, perr := s.named(ctx, sess, op, in.Project)
	if perr != nil {
		return nil, perr
	}
	// Every admin op rides a connection to an active project, so with
	// none left nobody could connect to activate one again.
	if p.State == store.ProjectActive && s.cfg.Control.ActiveProjects() == 1 {
		return nil, proto.Errf(proto.CodeInvalid,
			"add another project, or bring one back with `sfx admin project activate PROJECT`, before archiving this one",
			fmt.Sprintf("%s is the server's only active project; without one, nobody could connect to manage the server", p.Name))
	}
	was := p.State
	p, err := s.cfg.Control.ArchiveProject(ctx, sess.actor, p.ID)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	s.mu.Lock()
	delete(s.unopened, p.ID)
	s.mu.Unlock()
	s.drop(sess, "project archived", func(o owner) bool { return o.project == p.ID })
	out := wireProjectResult(p)
	if was != store.ProjectActive {
		// Never served, or archived before, when its claims ended.
		s.stores.retire(p.ID)
		return out, nil
	}
	// Every claim in it ends now (decisions 21 and 24). A store that is
	// closed is opened for it, through the registry so that the project
	// never has two writers: nobody can connect to an archived project,
	// so nothing else would open it before its activation or the
	// daemon's next start. The opening ends the claims as every opening
	// does, under the server's name; the count is of the admin's own.
	// One project is opened for one admin's request, which the control
	// write bucket bounds. The archive is committed, so the release goes
	// on if the admin's connection drops. A store that cannot be opened
	// is logged, and its claims end at the daemon's next start
	// (ReleaseArchived) or when the project is activated.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RequestTimeout)
	defer cancel()
	held, err := s.stores.acquire(rctx, p)
	if err != nil {
		s.cfg.Logger.Error("open the store of an archived project to end its claims", "project", p.ID, "database", p.DBName, "err", err)
	}
	out.Released = s.release(rctx, sess.actor, p.ID, "", lostArchived)
	s.stores.retire(p.ID)
	if err == nil {
		s.stores.release(held)
	}
	return out, nil
}

// named resolves the project a server admin's op must name.
func (s *Server) named(ctx context.Context, sess *session, op, ref string) (store.Project, *proto.Error) {
	if ref == "" {
		return store.Project{}, proto.Errf(proto.CodeInvalid, fmt.Sprintf("name the project: `sfx %s PROJECT`, a UUID or a name from `sfx admin project list`", command(op)),
			"no project named")
	}
	return s.project(ctx, sess, op, ref, "")
}

// projectActivate serves an archived project again. Its store is opened
// first, which migrates a database archived under an older starfixd, so
// that an active project is always one this server can open.
func projectActivate(ctx context.Context, s *Server, sess *session, in proto.ProjectArgs) (any, *proto.Error) {
	const op = proto.OpProjectActivate
	if perr := s.serverOnly(sess, op, in.Project); perr != nil {
		return nil, perr
	}
	p, perr := s.named(ctx, sess, op, in.Project)
	if perr != nil {
		return nil, perr
	}
	switch p.State {
	case store.ProjectActive:
		return wireProjectResult(p), nil
	case store.ProjectCreating:
		// It may have no database yet; only project add finishes one.
		return nil, proto.Errf(proto.CodeInvalid, fmt.Sprintf("run `sfx admin project add %s` again, which finishes creating it", p.Name),
			fmt.Sprintf("project %s is still being created", p.Name))
	}
	if perr := s.open(ctx, op, p); perr != nil {
		return nil, perr
	}
	p, err := s.cfg.Control.ActivateProject(ctx, sess.actor, p.ID)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	return wireProjectResult(p), nil
}

// principalAdd adds a principal, as a member of a project unless a
// server admin asks for none.
func principalAdd(ctx context.Context, s *Server, sess *session, in proto.PrincipalAddArgs) (any, *proto.Error) {
	const op = proto.OpPrincipalAdd
	var proj store.Project
	switch {
	case in.Unassigned && in.Project != "":
		return nil, proto.Errf(proto.CodeInvalid, "give --project or --no-project, not both", "a principal is added to a project or to none")
	case !in.Unassigned:
		var perr *proto.Error
		if proj, perr = s.project(ctx, sess, op, in.Project, in.Name); perr != nil {
			return nil, perr
		}
	}
	if len(in.Keys) > proto.MaxKeysPerAdd {
		return nil, proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("add the principal with up to %d keys, then the rest with `sfx admin key add %s FILE`", proto.MaxKeysPerAdd, in.Name),
			fmt.Sprintf("%d keys is more than one principal add carries (%d)", len(in.Keys), proto.MaxKeysPerAdd))
	}
	p, err := s.cfg.Control.AddPrincipal(ctx, sess.actor, in.Name, proj.ID, in.Keys...)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := proto.PrincipalAddResult{PrincipalResult: proto.PrincipalResult{ID: p.Name, Rev: int64(p.Rev), State: string(p.State),
		Project: proj.ID, ProjectName: proj.Name}}
	if len(in.Keys) > 0 {
		out.Warning = s.syncKeys()
		// A failure here shows no fingerprints: the keys were added.
		added, err := s.cfg.Control.Keys(ctx, p.Name)
		if err != nil {
			s.cfg.Logger.Error("read the keys of a new principal", "principal", p.Name, "err", err)
		}
		for _, k := range added {
			out.Keys = append(out.Keys, k.Fingerprint)
		}
	}
	return out, nil
}

// principals lists every principal, for server admins only: the names
// are the whole server's, whatever project the caller administers.
func principals(ctx context.Context, s *Server, sess *session, in proto.AdminListArgs) (any, *proto.Error) {
	const op = proto.OpPrincipals
	if perr := s.serverOnly(sess, op, ""); perr != nil {
		return nil, perr
	}
	all, err := s.cfg.Control.Principals(ctx)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	all, more, perr := page(all, func(p store.Principal) string { return p.Name }, in)
	if perr != nil {
		return nil, perr
	}
	out := proto.PrincipalsResult{Principals: []proto.Principal{}, More: more}
	for _, p := range all {
		out.Principals = append(out.Principals, proto.Principal{Name: p.Name, State: string(p.State), CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt})
	}
	return out, nil
}

// principalDisable disables a principal, then takes its keys out of the
// keys file, closes every connection it has, to every project, and ends
// the claims it holds in every store that is open. A server admin's
// too: disabled wins over the config file (decision 20).
func principalDisable(ctx context.Context, s *Server, sess *session, in proto.PrincipalArgs) (any, *proto.Error) {
	p, err := s.cfg.Control.DisablePrincipal(ctx, sess.actor, in.Name)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, proto.OpPrincipalDisable, err)
	}
	warning := s.syncKeys()
	s.drop(sess, "principal disabled", func(o owner) bool { return o.principal == p.Name })
	released := s.release(ctx, sess.actor, "", p.Name, lostDisabled)
	return proto.PrincipalResult{ID: p.Name, Rev: int64(p.Rev), State: string(p.State), Released: released, Warning: warning}, nil
}

// principalEnable makes a disabled principal active again, and its keys
// with it.
func principalEnable(ctx context.Context, s *Server, sess *session, in proto.PrincipalArgs) (any, *proto.Error) {
	p, err := s.cfg.Control.EnablePrincipal(ctx, sess.actor, in.Name)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, proto.OpPrincipalEnable, err)
	}
	return proto.PrincipalResult{ID: p.Name, Rev: int64(p.Rev), State: string(p.State), Warning: s.syncKeys()}, nil
}

// memberSet adds a membership or changes its role. Taking the admin
// role away closes nothing: the member stays a member.
func memberSet(ctx context.Context, s *Server, sess *session, in proto.MemberSetArgs) (any, *proto.Error) {
	const op = proto.OpMemberSet
	ctl, a := s.cfg.Control, sess.actor
	p, perr := s.project(ctx, sess, op, in.Project, in.Name)
	if perr != nil {
		return nil, perr
	}
	role := store.Role(cmp.Or(in.Role, string(store.RoleMember)))
	// Read before the write, to say what the write did. Another admin's
	// write may come between; then only the word is off.
	had, was, err := ctl.Role(ctx, in.Name, p.ID)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	m, err := ctl.SetMember(ctx, a, in.Name, p.ID, role)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := proto.MemberSetResult{ID: m.Principal, Rev: int64(m.Rev), Project: p.ID, ProjectName: p.Name, Role: string(m.Role), Change: "unchanged"}
	switch {
	case !was:
		out.Change = "added"
		out.Projects, out.Keys = s.keysCarried(ctx, a.Principal, m.Principal)
	case had != m.Role:
		out.Change = "changed"
	}
	return out, nil
}

// keysCarried returns, for a principal just added to a project, how
// many projects it now belongs to and the fingerprints of the keys
// recorded for it, when that is more than one project and admin may see
// its keys: a server admin, or a project admin with reach over it. A
// key reaches every project its principal is in, so the admin who adds
// someone from another project should see what comes with them. A
// failure is logged and shows nothing: the membership was added.
func (s *Server) keysCarried(ctx context.Context, admin, principal string) (int, []string) {
	ctl := s.cfg.Control
	of, err := ctl.ProjectsOf(ctx, principal)
	if err != nil {
		s.cfg.Logger.Error("read the projects of a new member", "principal", principal, "err", err)
		return 0, nil
	}
	if len(of) < 2 || !ctl.Reaches(admin, principal) {
		return 0, nil
	}
	keys, err := ctl.Keys(ctx, principal)
	if err != nil {
		s.cfg.Logger.Error("read the keys of a new member", "principal", principal, "err", err)
		return 0, nil
	}
	fps := make([]string, 0, len(keys))
	for _, k := range keys {
		fps = append(fps, k.Fingerprint)
	}
	return len(of), fps
}

// members lists a project's members, for its admins and server admins.
func members(ctx context.Context, s *Server, sess *session, in proto.MembersArgs) (any, *proto.Error) {
	const op = proto.OpMembers
	ctl, actor := s.cfg.Control, sess.actor.Principal
	p, perr := s.project(ctx, sess, op, in.Project, "")
	if perr != nil {
		return nil, perr
	}
	if ctl.Access(actor, p.ID).Role != store.RoleAdmin {
		return nil, accessErr(&store.AccessError{Actor: actor, Op: store.Op(op), Project: cmp.Or(in.Project, p.ID), Reason: store.AccessAdmin})
	}
	all, err := ctl.Members(ctx, p.ID)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	all, more, perr := page(all, func(m store.Member) string { return m.Principal }, in.AdminListArgs)
	if perr != nil {
		return nil, perr
	}
	out := proto.MembersResult{Project: p.ID, ProjectName: p.Name, Members: []proto.Member{}, More: more}
	for _, m := range all {
		out.Members = append(out.Members, proto.Member{Principal: m.Principal, Role: string(m.Role), AddedBy: m.AddedBy, AddedAt: m.AddedAt})
	}
	return out, nil
}

// memberRemove ends a membership, then closes the principal's
// connections to that project and ends the claims it holds there,
// unless they are a server admin, who needs no membership to be there.
func memberRemove(ctx context.Context, s *Server, sess *session, in proto.MemberArgs) (any, *proto.Error) {
	const op = proto.OpMemberRemove
	ctl := s.cfg.Control
	p, perr := s.project(ctx, sess, op, in.Project, in.Name)
	if perr != nil {
		return nil, perr
	}
	m, err := ctl.RemoveMember(ctx, sess.actor, in.Name, p.ID)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := proto.MemberRemoveResult{ID: m.Principal, Project: p.ID, ProjectName: p.Name}
	if !ctl.IsServerAdmin(m.Principal) {
		s.drop(sess, "membership removed", func(o owner) bool { return o.principal == m.Principal && o.project == p.ID })
		out.Released = s.release(ctx, sess.actor, p.ID, m.Principal, lostMembership)
	}
	// The removal may have brought the principal within reach of the
	// admins of what it still belongs to (store.Control.ReachGained).
	// They are counted, not named: they may be admins of projects the
	// caller knows nothing of.
	if n := len(ctl.ReachGained(m.Principal, p.ID)); n > 0 {
		who := "a project admin who could not manage " + m.Principal + " before, and now can"
		if n > 1 {
			who = fmt.Sprintf("%d project admins who could not manage %s before, and now can", n, m.Principal)
		}
		out.Warning = fmt.Sprintf("every project %s still belongs to is administered by %s: disable or enable %s, and manage its keys",
			m.Principal, who, m.Principal)
	}
	return out, nil
}

// keyAdd adds a public key to a principal, and writes the keys file, so
// that the key signs in once the admin is answered. The control store
// decides who may, and refuses anyone their own account's (decision 19).
func keyAdd(ctx context.Context, s *Server, sess *session, in proto.KeyAddArgs) (any, *proto.Error) {
	const op = proto.OpKeyAdd
	ctl := s.cfg.Control
	// Read before the write, to say what the write did. Another admin's
	// write may come between; then only the word is off.
	before, err := ctl.Keys(ctx, in.Name)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	k, err := ctl.AddKey(ctx, sess.actor, in.Name, in.Key)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := wireKeyResult(k)
	out.Change, out.Warning = "added", s.syncKeys()
	if slices.ContainsFunc(before, func(b store.PrincipalKey) bool { return b.Fingerprint == k.Fingerprint }) {
		out.Change = "unchanged"
	}
	return out, nil
}

// wireKeyResult is a key after a write. The type is the first word of
// the stored key.
func wireKeyResult(k store.PrincipalKey) proto.KeyResult {
	return proto.KeyResult{ID: k.Fingerprint, Principal: k.Principal, Type: keyType(k.Key), Comment: k.Comment}
}

// keyType is the type of a key in authorized_keys form: its first word.
func keyType(key string) string {
	t, _, _ := strings.Cut(key, " ")
	return t
}

// keys lists a principal's keys: for a server admin, for an admin with
// reach over the principal, and for the principal itself. Anyone else
// is refused the same way whether or not the principal exists.
func keys(ctx context.Context, s *Server, sess *session, in proto.KeysArgs) (any, *proto.Error) {
	const op = proto.OpKeys
	ctl, actor := s.cfg.Control, sess.actor.Principal
	admin := ctl.IsServerAdmin(actor)
	if !admin && actor != in.Name && !ctl.Reaches(actor, in.Name) {
		return nil, proto.Errf(proto.CodeForbidden,
			"ask a server admin, or a project admin who administers every project the principal belongs to; `sfx admin key list "+actor+"` lists your own",
			fmt.Sprintf("%s may not list the keys of %s: it is for the principal, for an admin who manages it, and for server admins", actor, in.Name))
	}
	all, err := ctl.Keys(ctx, in.Name)
	if err == nil && admin && len(all) == 0 {
		// A server admin is told when there is no such principal.
		_, err = ctl.Principal(ctx, in.Name)
	}
	if err != nil {
		return nil, s.ctlErr(ctx, sess, op, err)
	}
	out := proto.KeysResult{Principal: in.Name, Keys: []proto.Key{}, FileConns: s.fileConns(in.Name)}
	hand := s.byHand()
	for _, k := range all {
		out.Keys = append(out.Keys, proto.Key{Fingerprint: k.Fingerprint, Type: keyType(k.Key), Comment: k.Comment, AddedBy: k.AddedBy, AddedAt: k.AddedAt,
			InAuthorizedKeys: hand[k.Fingerprint]})
	}
	return out, nil
}

// keyRemove removes a key, takes it out of the keys file, and closes
// the connections it had authenticated: those whose bridge named its
// fingerprint, because sshd took the key's line from that file.
//
// A key that is also a line of the hand-maintained authorized_keys file
// is not kept out by this: sshd still accepts it there, by a line that
// names no fingerprint, so its connections are not closed either. The
// result says so, and that the line must be deleted on the server.
func keyRemove(ctx context.Context, s *Server, sess *session, in proto.KeyRemoveArgs) (any, *proto.Error) {
	k, err := s.cfg.Control.RemoveKey(ctx, sess.actor, in.Fingerprint)
	if err != nil {
		return nil, s.ctlErr(ctx, sess, proto.OpKeyRemove, err)
	}
	out := wireKeyResult(k)
	out.Warning = s.syncKeys()
	out.Dropped = s.drop(sess, "key removed", func(o owner) bool { return o.key == k.Fingerprint })
	if s.byHand()[k.Fingerprint] {
		out.InAuthorizedKeys = true
		// One line: a warning is printed as one.
		if out.Warning != "" {
			out.Warning = "; and " + out.Warning
		}
		out.Warning = handWarning(k.Fingerprint) + out.Warning
	}
	return out, nil
}

// OpenProjects opens the store of every active project in turn and
// closes it again, which applies this starfixd's migrations to each
// database (about 3 ms for one already migrated). A daemon runs it once,
// before it listens, so that an upgrade migrates every project while
// `starfixd upgrade` still watches, not each at its next connection.
//
// A project that cannot be opened does not stop the others, or the
// server: it is logged, tried again by its next connection, and held
// against the upgrade's health probe, which is refused until every such
// project has opened or been archived. So a failed migration fails the
// upgrade, and a restart with one broken database still serves the rest.
// OpenProjects returns an error only when the projects cannot be read.
// A server of one project has opened its store already.
func (s *Server) OpenProjects(ctx context.Context) error {
	if s.cfg.Control == nil {
		return nil
	}
	all, err := s.cfg.Control.Projects(ctx)
	if err != nil {
		return fmt.Errorf("open projects: %w", err)
	}
	for _, p := range all {
		if p.State != store.ProjectActive {
			continue
		}
		octx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		st, err := s.cfg.Open(octx, p)
		cancel()
		if err == nil {
			err = st.Close()
		}
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("open projects: %w", ctx.Err())
			}
			s.cfg.Logger.Error("open project store at start", "project", p.ID, "name", p.Name, "database", p.DBName, "err", err)
			s.mu.Lock()
			s.unopened[p.ID] = p
			s.mu.Unlock()
		}
	}
	return nil
}

// ReleaseArchived ends every claim in every archived project (decision
// 24). A daemon runs it once, after OpenProjects and before it listens.
// An archive ends its project's claims as it commits (projectArchive),
// but a daemon that stopped between the control write and the release
// left them in a project nobody may connect to, whose store therefore
// never opens and is never swept. Each claim ends as the sweep ends one:
// a claim.release of the server's own for "project archived", the
// holder's claim.lost, and the issue open again.
//
// open opens an archived project's database as it is, without migrating
// it ([store.Options.NoMigrate]): a database an older starfixd last
// migrated is left untouched, with a line in the log, and any claim in
// it ends when the project is activated, which migrates it and sweeps
// it before its first request. So an upgrade changes no database that
// nobody can use, and a rollback finds the archived ones as they were.
//
// A project that cannot be opened is logged and does not stop the
// others, or the server. ReleaseArchived returns an error only when the
// projects cannot be read or ctx ends. It opens each store beside the
// registry, so it must not run once the server accepts connections.
//
// A project that holds no claim costs one read through the control
// store's connection ([store.Control.HeldClaims]) and opens no store, so
// a server with hundreds of archived projects starts as fast as one with
// none. A server of one project has nothing archived.
func (s *Server) ReleaseArchived(ctx context.Context, open func(context.Context, store.Project) (*store.Store, error)) error {
	if s.cfg.Control == nil {
		return nil
	}
	all, err := s.cfg.Control.Projects(ctx)
	if err != nil {
		return fmt.Errorf("release the claims of archived projects: %w", err)
	}
	for _, p := range all {
		if p.State != store.ProjectArchived {
			continue
		}
		err := s.releaseArchived(ctx, p, open)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return fmt.Errorf("release the claims of archived projects: %w", ctx.Err())
		case errors.Is(err, store.ErrSchemaOld):
			s.cfg.Logger.Info("archived project not opened at start: its database awaits this starfixd's migrations, and any claim in it ends when the project is activated",
				"project", p.ID, "name", p.Name, "database", p.DBName)
		default:
			s.cfg.Logger.Error("end the claims of an archived project at start", "project", p.ID, "name", p.Name, "database", p.DBName, "err", err)
		}
	}
	return nil
}

// releaseArchived ends every claim in the archived project p, whose
// store it opens with open and closes again.
func (s *Server) releaseArchived(ctx context.Context, p store.Project, open func(context.Context, store.Project) (*store.Store, error)) error {
	octx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	// Nearly every archived project holds no claim, and one read says
	// so without a store. When the read fails, opening says why.
	if n, err := s.cfg.Control.HeldClaims(octx, p.DBName); err == nil && n == 0 {
		return nil
	}
	st, err := open(octx, p)
	if err != nil {
		return err
	}
	cs, err := st.EndClaims(octx, accessActor, func(string) string { return lostArchived })
	for _, c := range cs {
		s.cfg.Logger.Info("claim released: access ended", "project", p.ID, "issue", c.Issue, "principal", c.Holder.Principal,
			"session", c.Holder.Session, "epoch", c.Epoch, "by", accessActor.Principal, "reason", lostArchived)
	}
	return errors.Join(err, st.Close())
}

// unopenedErr refuses the upgrade's health probe while a project that
// OpenProjects could not open is still unopened.
func (s *Server) unopenedErr() *proto.Error {
	bad := s.unopenedProjects()
	if len(bad) == 0 {
		return nil
	}
	slices.SortFunc(bad, func(a, b store.Project) int { return strings.Compare(a.Name, b.Name) })
	names := make([]string, 0, 5)
	for _, p := range bad[:min(len(bad), 5)] {
		names = append(names, fmt.Sprintf("%s (database %s)", p.Name, p.DBName))
	}
	msg := fmt.Sprintf("starfixd could not open the database of %d active projects: %s", len(bad), strings.Join(names, ", "))
	if len(bad) == 1 {
		msg = "starfixd could not open the database of an active project: " + names[0]
	}
	if len(bad) > len(names) {
		msg += fmt.Sprintf(" and %d more", len(bad)-len(names))
	}
	return proto.Errf(proto.CodeUnavailable, "check the starfixd log for each; fix or archive the project, then restart starfixd", msg)
}
