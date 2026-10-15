package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/ariesworx/starfix/internal/logs"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// Dispatch runs one operation for actor. args is the raw JSON of the op's
// *Args type; unknown fields are refused, since every field of every
// protocol version in range is known here. It returns the op's *Result,
// or a refusal: invalid for an unknown op or bad arguments, else the
// store's error as mapErr words it. Dispatch is safe for concurrent use.
// It applies no write rate limit, and refuses watch, which needs a
// connection to push to, and the ops that manage access (proto's
// admin.go), which need a server of many projects.
//
// Dispatch is for a server of one project (Config.Store). A server of
// many (Config.Control) knows a request's project only from its
// connection, and refuses here with unavailable.
func (s *Server) Dispatch(ctx context.Context, actor store.Actor, op string, args json.RawMessage) (any, *proto.Error) {
	if s.single == nil {
		return nil, proto.Errf(proto.CodeUnavailable, "send the request on a connection, which names its project",
			"this server holds many projects, and the request names none")
	}
	if adminOp(op) {
		return nil, oneProject()
	}
	return s.single.dispatch(ctx, actor, op, args)
}

// dispatch runs one operation for actor in pr, as Dispatch describes.
func (pr *served) dispatch(ctx context.Context, actor store.Actor, op string, args json.RawMessage) (any, *proto.Error) {
	h, ok := handlers[op]
	if !ok {
		return nil, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" starfix to match the server, or check the operation name",
			fmt.Sprintf("unknown operation %q", op))
	}
	return h(ctx, pr, actor, args)
}

// handler runs one op, as Dispatch does, for actor a in project pr.
type handler func(ctx context.Context, pr *served, a store.Actor, args json.RawMessage) (any, *proto.Error)

// handlers maps each op to its handler.
var handlers = map[string]handler{
	proto.OpCreate:      typed(create),
	proto.OpShow:        typed(show),
	proto.OpList:        typed(list),
	proto.OpReady:       typed(ready),
	proto.OpBlocked:     typed(blocked),
	proto.OpUpdate:      typed(update),
	proto.OpClose:       typed(closeIssue),
	proto.OpReopen:      typed(reopen),
	proto.OpDepAdd:      typed(depAdd),
	proto.OpDepRm:       typed(depRm),
	proto.OpLabelAdd:    typed(labelAdd),
	proto.OpLabelRm:     typed(labelRm),
	proto.OpComment:     typed(comment),
	proto.OpComments:    typed(comments),
	proto.OpHistory:     typed(history),
	proto.OpStart:       typed(start),
	proto.OpFinish:      typed(finish),
	proto.OpHandoff:     typed(handoff),
	proto.OpDigest:      typed(digest),
	proto.OpRenew:       typed(renew),
	proto.OpWho:         typed(who),
	proto.OpInbox:       typed(inbox),
	proto.OpAck:         typed(ack),
	proto.OpWatch:       typed(watchOp),
	proto.OpAccept:      typed(accept),
	proto.OpUsage:       typed(usage),
	proto.OpClaims:      typed(claims),
	proto.OpRemember:    typed(remember),
	proto.OpRecall:      typed(recall),
	proto.OpForget:      typed(forget),
	proto.OpPin:         typed(pin),
	proto.OpPriceSet:    typed(priceSet),
	proto.OpPrices:      typed(prices),
	proto.OpCost:        typed(cost),
	proto.OpHoursLog:    typed(hoursLog),
	proto.OpHoursDelete: typed(hoursDelete),
	proto.OpHours:       typed(hours),
	proto.OpPlanSet:     typed(planSet),
	proto.OpPlans:       typed(plans),
	proto.OpConflicts:   typed(conflicts),
	proto.OpPropose:     typed(propose),
	proto.OpResolve:     typed(resolve),
	proto.OpGateAdd:     typed(gateAdd),
	proto.OpGatePass:    typed(gatePass),
	proto.OpGateWaive:   typed(gateWaive),
	proto.OpGateRearm:   typed(gateRearm),
	proto.OpGateFail:    typed(gateFail),
	proto.OpGates:       typed(gates),
	proto.OpDates:       typed(dates),
}

// typed decodes args strictly into A and calls fn.
func typed[A any](fn func(ctx context.Context, pr *served, a store.Actor, args A) (any, *proto.Error)) handler {
	return func(ctx context.Context, pr *served, a store.Actor, raw json.RawMessage) (any, *proto.Error) {
		args, perr := decodeArgs[A](raw)
		if perr != nil {
			return nil, perr
		}
		return fn(ctx, pr, a, args)
	}
}

func create(ctx context.Context, pr *served, a store.Actor, in proto.CreateArgs) (any, *proto.Error) {
	n := store.NewIssue{
		ID: store.IssueID(in.ID), IdempotencyKey: in.Idem, ParentID: store.IssueID(in.Parent),
		Title: in.Title, Body: in.Body, Design: in.Design, Acceptance: in.Acceptance, Notes: in.Notes,
		Status: store.Status(in.Status), Type: store.IssueType(in.Type),
		Assignee: in.Assignee, Owner: in.Owner, Labels: in.Labels, Account: in.Account, Paths: in.Paths,
	}
	if in.Priority != nil {
		p := store.Priority(*in.Priority)
		n.Priority = &p
	}
	var perr *proto.Error
	if n.DeferUntil, perr = issueDate("defer_until", in.DeferUntil); perr != nil {
		return nil, perr
	}
	if n.DueAt, perr = issueDate("due", in.Due); perr != nil {
		return nil, perr
	}
	is, err := pr.store.CreateIssue(ctx, a, n)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpCreate, in.ID, 0, err)
	}
	return proto.CreateResult{WriteResult: proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)},
		Similar: pr.similar(ctx, is)}, nil
}

// similar lists the closed issues like is, for create and show. A failure
// is logged and lists none: the request itself succeeded.
func (pr *served) similar(ctx context.Context, is store.Issue) []proto.Summary {
	sim, err := pr.store.SimilarClosed(ctx, is.Title, is.ID, store.MaxSimilar)
	if err != nil {
		pr.log.ErrorContext(ctx, "similar issues", "event", logs.RequestPart, "issue", is.ID, "err", err)
		return nil
	}
	var out []proto.Summary
	for _, x := range sim {
		out = append(out, proto.Summary{ID: string(x.ID), Title: x.Title, Status: string(store.StatusClosed), Priority: int(x.Priority)})
	}
	return out
}

// items reads is's acceptance items for start and show.
func (pr *served) items(ctx context.Context, is store.Issue) ([]proto.AcceptanceItem, error) {
	items, err := pr.store.AcceptanceItems(ctx, is.ID)
	if err != nil {
		return nil, err
	}
	return wireItems(items), nil
}

func wireItems(items []store.AcceptanceItem) []proto.AcceptanceItem {
	var out []proto.AcceptanceItem
	for _, it := range items {
		out = append(out, proto.AcceptanceItem{N: it.N, Text: it.Text, State: string(it.State), Reason: it.Reason,
			By: it.By, At: it.At})
	}
	return out
}

// compactText is how much of each long text field compact show returns.
const compactText = 500

func show(ctx context.Context, pr *served, a store.Actor, in proto.ShowArgs) (any, *proto.Error) {
	v, err := pr.store.ShowIssue(ctx, a, store.IssueID(in.ID), proto.MaxShowPaths)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	deps := v.Deps
	out := proto.ShowResult{Issue: wireIssue(v.Issue), Items: wireItems(v.Items), Similar: pr.similar(ctx, v.Issue),
		Usage: pr.wireIssueUsage(v.Usage), Files: wireFiles(v.Files)}
	if v.Claim != nil {
		c := wireClaim(*v.Claim)
		out.Claim = &c
	}
	if v.Handoff != nil {
		w := wireHandoff(*v.Handoff, a)
		out.Handoff = &w
	}
	if !in.Full {
		for _, f := range []*string{&out.Issue.Body, &out.Issue.Design, &out.Issue.Acceptance, &out.Issue.Notes} {
			if cut, ok := truncate(*f, compactText); ok {
				*f = cut
				out.Issue.Truncated = true
			}
		}
	}
	cs, _, err := pr.store.Conflicts(ctx, store.ConflictFilter{Issue: v.Issue.ID, Limit: proto.MaxShowConflicts})
	if err != nil {
		pr.log.ErrorContext(ctx, "read conflicts", "event", logs.RequestPart, "issue", v.Issue.ID, "err", err)
	}
	for _, c := range cs {
		out.Conflicts = append(out.Conflicts, wireConflict(c, !in.Full))
	}
	if v.Issue.Type == store.TypeGate {
		g, err := pr.store.GateOf(ctx, v.Issue.ID)
		if err != nil {
			return nil, pr.mapErr(ctx, proto.OpShow, in.ID, 0, err)
		}
		if g != nil {
			w := wireGate(*g)
			out.Gate = &w
		}
	}
	holding, err := pr.store.GatesHolding(ctx, v.Issue.ID)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	if len(holding) > 0 {
		out.Gates = wireGates(holding)
	}
	checks, err := pr.store.ChecksOf(ctx, v.Issue.ID)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpShow, in.ID, 0, err)
	}
	if len(checks) > 0 {
		out.Checks = wireGates(checks)
	}
	if len(deps) > proto.MaxShowDeps {
		deps, out.DepsMore = deps[:proto.MaxShowDeps], len(deps)-proto.MaxShowDeps
	}
	for _, d := range deps {
		out.Deps = append(out.Deps, proto.Dep{From: string(d.From), To: string(d.To), Type: string(d.Type),
			CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt.UTC()})
	}
	return out, nil
}

// truncate cuts s to at most n bytes, on a rune boundary, and appends "…".
// It reports whether it cut anything.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…", true
}

func list(ctx context.Context, pr *served, _ store.Actor, in proto.ListArgs) (any, *proto.Error) {
	f := store.Filter{Assignee: in.Assignee, ParentID: store.IssueID(in.Parent), Labels: in.Labels,
		Limit: in.Limit, Cursor: store.Cursor(in.Cursor)}
	for _, st := range in.Status {
		f.Statuses = append(f.Statuses, store.Status(st))
	}
	for _, t := range in.Type {
		f.Types = append(f.Types, store.IssueType(t))
	}
	for _, p := range in.Priority {
		f.Priorities = append(f.Priorities, store.Priority(p))
	}
	page, err := pr.store.ListSummaries(ctx, f)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpList, "", 0, err)
	}
	return proto.ListResult{Issues: summaries(page.Issues), Next: string(page.Next)}, nil
}

func ready(ctx context.Context, pr *served, a store.Actor, in proto.LimitArgs) (any, *proto.Error) {
	rs, err := pr.store.Ready(ctx, a, in.Limit)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpReady, "", 0, err)
	}
	out := proto.ListResult{Issues: make([]proto.Summary, 0, len(rs))}
	for _, r := range rs {
		sm := summary(r.Summary)
		for _, id := range r.Overlaps {
			sm.Overlaps = append(sm.Overlaps, string(id))
		}
		out.Issues = append(out.Issues, sm)
	}
	return out, nil
}

// wireFiles is show's files, or nil when the issue has no paths and
// overlaps nothing.
func wireFiles(f store.Files) *proto.Files {
	if len(f.Paths) == 0 && len(f.Overlaps) == 0 {
		return nil
	}
	out := &proto.Files{More: f.More}
	for _, p := range f.Paths {
		out.Paths = append(out.Paths, proto.FilePath{Path: p.Path, Source: string(p.Source)})
	}
	for _, o := range f.Overlaps {
		out.Overlaps = append(out.Overlaps, proto.Overlap{ID: string(o.Issue), By: o.Holder.Principal, Session: o.Holder.Session})
	}
	return out
}

func blocked(ctx context.Context, pr *served, _ store.Actor, in proto.LimitArgs) (any, *proto.Error) {
	bs, err := pr.store.Blocked(ctx, in.Limit)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpBlocked, "", 0, err)
	}
	out := proto.BlockedResult{Issues: []proto.BlockedIssue{}}
	var blockers []store.IssueID
	for _, b := range bs {
		blockers = append(blockers, b.BlockedBy...)
	}
	gs, err := pr.gatesOf(ctx, blockers)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpBlocked, "", 0, err)
	}
	if len(gs) > 0 {
		out.Gates = gs
	}
	for _, b := range bs {
		bi := proto.BlockedIssue{Summary: summary(b.Summary), Via: string(b.Via), BlockedBy: []string{}}
		if len(b.BlockedBy) > proto.MaxBlockers {
			b.BlockedBy, bi.More = b.BlockedBy[:proto.MaxBlockers], len(b.BlockedBy)-proto.MaxBlockers
		}
		for _, id := range b.BlockedBy {
			bi.BlockedBy = append(bi.BlockedBy, string(id))
		}
		out.Issues = append(out.Issues, bi)
	}
	return out, nil
}

func update(ctx context.Context, pr *served, a store.Actor, in proto.UpdateArgs) (any, *proto.Error) {
	p := store.IssuePatch{Title: in.Title, Body: in.Body, Design: in.Design, Acceptance: in.Acceptance,
		Notes: in.Notes, Assignee: in.Assignee, Owner: in.Owner, Account: in.Account, IdempotencyKey: in.Idem}
	if in.Status != nil {
		st := store.Status(*in.Status)
		p.Status = &st
	}
	if in.Priority != nil {
		prio := store.Priority(*in.Priority)
		p.Priority = &prio
	}
	if in.Type != nil {
		t := store.IssueType(*in.Type)
		p.Type = &t
	}
	if in.Parent != nil {
		pid := store.IssueID(*in.Parent)
		p.ParentID = &pid
	}
	p.Paths = in.Paths
	var perr *proto.Error
	if in.DeferUntil != nil {
		if p.DeferUntil, perr = issueDate("defer_until", *in.DeferUntil); perr != nil {
			return nil, perr
		}
	}
	if in.Due != nil {
		if p.DueAt, perr = issueDate("due", *in.Due); perr != nil {
			return nil, perr
		}
	}
	if !in.Park {
		is, err := pr.store.UpdateIssue(ctx, a, store.IssueID(in.ID), store.Rev(in.Rev), p)
		if err != nil {
			return nil, pr.mapErr(ctx, proto.OpUpdate, in.ID, in.Rev, err)
		}
		return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
	}
	m, err := pr.store.MergeIssue(ctx, a, store.IssueID(in.ID), store.Rev(in.Rev), p)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpUpdate, in.ID, in.Rev, err)
	}
	out := proto.UpdateResult{WriteResult: proto.WriteResult{ID: string(m.ID), Rev: int64(m.Rev)},
		Dropped: m.Dropped, Comment: m.Comment}
	for _, pk := range m.Parked {
		out.Parked = append(out.Parked, proto.ParkedField{Field: pk.Field, Conflict: pk.Conflict})
	}
	return out, nil
}

func closeIssue(ctx context.Context, pr *served, a store.Actor, in proto.CloseArgs) (any, *proto.Error) {
	closer := pr.store.CloseIssue
	if in.Force {
		closer = pr.store.ForceClose
	}
	is, err := closer(ctx, a, store.IssueID(in.ID), store.Rev(in.Rev), in.Reason)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpClose, in.ID, in.Rev, err)
	}
	return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
}

func reopen(ctx context.Context, pr *served, a store.Actor, in proto.ReopenArgs) (any, *proto.Error) {
	is, err := pr.store.ReopenIssue(ctx, a, store.IssueID(in.ID), store.Rev(in.Rev))
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpReopen, in.ID, in.Rev, err)
	}
	return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
}

// depType is a request's dependency type, blocks when it gives none.
func depType(t string) store.DepType {
	if t == "" {
		return store.DepBlocks
	}
	return store.DepType(t)
}

func depAdd(ctx context.Context, pr *served, a store.Actor, in proto.DepArgs) (any, *proto.Error) {
	if err := pr.store.AddDep(ctx, a, store.IssueID(in.From), store.IssueID(in.To), depType(in.Type), in.Idem); err != nil {
		return nil, pr.mapErr(ctx, proto.OpDepAdd, in.From, 0, err)
	}
	return proto.Empty{}, nil
}

func depRm(ctx context.Context, pr *served, a store.Actor, in proto.DepArgs) (any, *proto.Error) {
	if err := pr.store.RemoveDep(ctx, a, store.IssueID(in.From), store.IssueID(in.To), depType(in.Type)); err != nil {
		return nil, pr.mapErr(ctx, proto.OpDepRm, in.From, 0, err)
	}
	return proto.Empty{}, nil
}

func labelAdd(ctx context.Context, pr *served, a store.Actor, in proto.LabelArgs) (any, *proto.Error) {
	if err := pr.store.AddLabel(ctx, a, store.IssueID(in.ID), in.Label, in.Idem); err != nil {
		return nil, pr.mapErr(ctx, proto.OpLabelAdd, in.ID, 0, err)
	}
	return proto.Empty{}, nil
}

func labelRm(ctx context.Context, pr *served, a store.Actor, in proto.LabelArgs) (any, *proto.Error) {
	if err := pr.store.RemoveLabel(ctx, a, store.IssueID(in.ID), in.Label, in.Idem); err != nil {
		return nil, pr.mapErr(ctx, proto.OpLabelRm, in.ID, 0, err)
	}
	return proto.Empty{}, nil
}

func comment(ctx context.Context, pr *served, a store.Actor, in proto.CommentArgs) (any, *proto.Error) {
	c, err := pr.store.AddComment(ctx, a, store.IssueID(in.ID), in.Body, in.Idem)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpComment, in.ID, 0, err)
	}
	return proto.CommentResult{ID: c.ID}, nil
}

func comments(ctx context.Context, pr *served, _ store.Actor, in proto.PageArgs) (any, *proto.Error) {
	id := store.IssueID(in.ID)
	page, err := pr.store.CommentsPage(ctx, id, store.Cursor(in.Before), in.Limit)
	if err == nil && page.Total == 0 {
		_, err = pr.store.GetIssue(ctx, id) // no comments, or no issue?
	}
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpComments, in.ID, 0, err)
	}
	out := proto.CommentsResult{Comments: []proto.Comment{}, Earlier: string(page.Earlier), Total: page.Total}
	for _, c := range page.Comments {
		out.Comments = append(out.Comments, wireComment(c))
	}
	return out, nil
}

func history(ctx context.Context, pr *served, a store.Actor, in proto.PageArgs) (any, *proto.Error) {
	page, err := pr.store.HistoryPage(ctx, store.IssueID(in.ID), store.Cursor(in.Before), in.Limit)
	if err == nil && page.Total == 0 {
		_, err = pr.store.GetIssue(ctx, store.IssueID(in.ID))
	}
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpHistory, in.ID, 0, err)
	}
	out := proto.HistoryResult{Events: []proto.Event{}, Earlier: string(page.Earlier), Total: page.Total}
	for _, e := range page.Events {
		out.Events = append(out.Events, proto.Event{Seq: e.Seq, At: e.At.UTC(), Principal: e.Actor.Principal,
			Session: e.Actor.Session, Machine: e.Actor.Machine, Op: string(e.Op),
			Before: eventState(e, e.Before, a), After: eventState(e, e.After, a)})
	}
	return out, nil
}

// lease parses the lease a start or a renew of one session's claims
// asks for. Empty means store.DefaultLease; anything else must be from
// store.MinLease to store.MaxClaimLease.
func lease(op, s string) (time.Duration, *proto.Error) {
	if s == "" {
		return store.DefaultLease, nil
	}
	d, err := proto.ParseDuration(s)
	if err != nil || d < store.MinLease || d > store.MaxClaimLease {
		return 0, proto.Errf(proto.CodeInvalid, fmt.Sprintf("give a lease from 1m to 24h; `sfx %s -h` lists the options", command(op)),
			fmt.Sprintf("lease %q is not a duration from 1m to 24h", s))
	}
	return d, nil
}

// awayLease parses the duration of `sfx away`, a renew with All: from
// store.MinLease to the store's AwayMax, the server's away_max.
func awayLease(s string, most time.Duration) (time.Duration, *proto.Error) {
	d, err := proto.ParseDuration(s)
	if err != nil || d < store.MinLease || d > most {
		upTo := store.LeaseText(most)
		return 0, proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("give `sfx away` a duration from 1m to at most %s, this server's away_max; a server admin can raise away_max under limits:", upTo),
			fmt.Sprintf("away %q is not a duration from 1m to %s", s, upTo))
	}
	return d, nil
}

func start(ctx context.Context, pr *served, a store.Actor, in proto.StartArgs) (any, *proto.Error) {
	d, perr := lease(proto.OpStart, in.Lease)
	if perr != nil {
		return nil, perr
	}
	is, claim, err := pr.store.StartIssue(ctx, a, store.IssueID(in.ID), d, in.Take)
	if errors.Is(err, store.ErrNothingReady) {
		return nil, proto.Errf(proto.CodeNotFound, proto.FixSeeBlocked+" with `sfx blocked`, or create an issue",
			"nothing is ready to start")
	}
	if held, ok := errors.AsType[*store.HeldError](err); ok {
		return nil, pr.held(ctx, a, held)
	}
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpStart, in.ID, 0, err)
	}
	c := wireClaim(claim)
	out := proto.StartResult{Issue: wireIssue(is), Claim: &c}
	// The issue is taken: a failed read of its items or its handoff is
	// logged rather than failing the start.
	if out.Items, err = pr.items(ctx, is); err != nil {
		pr.log.ErrorContext(ctx, "read acceptance items", "event", logs.RequestPart, "issue", is.ID, "err", err)
	}
	h, err := pr.store.LastHandoff(ctx, is.ID)
	if err != nil {
		pr.log.ErrorContext(ctx, "read handoff", "event", logs.RequestPart, "issue", is.ID, "err", err)
	} else if h != nil {
		w := wireHandoff(*h, a)
		out.Handoff = &w
	}
	out.Memories = pr.issueMemories(ctx, a, is.ID)
	return out, nil
}

func finish(ctx context.Context, pr *served, a store.Actor, in proto.FinishArgs) (any, *proto.Error) {
	f := store.Finish{Reason: in.Reason, Handoff: storeHandoff(in.Handoff, in.HandoffFields), IdempotencyKey: in.Idem,
		Accept: store.Acceptance{Tick: in.Ticked, Waive: in.Waived}, Paths: in.Paths}
	for _, d := range in.Discovered {
		n := store.NewIssue{Title: d.Title, Type: store.IssueType(d.Type)}
		if d.Priority != nil {
			p := store.Priority(*d.Priority)
			n.Priority = &p
		}
		f.Discovered = append(f.Discovered, n)
	}
	var done store.Finished
	var err error
	if in.Park {
		// A replay from an outbox: kept as a comment where the issue
		// moved on, rather than refused (design §18.6).
		done, err = pr.store.ReplayFinish(ctx, a, store.IssueID(in.ID), in.Epoch, f)
	} else {
		done.Issue, done.IDs, err = pr.store.FinishIssue(ctx, a, store.IssueID(in.ID), in.Epoch, f)
	}
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpFinish, in.ID, 0, err)
	}
	out := proto.FinishResult{ID: string(done.ID), Rev: int64(done.Rev), Lapsed: done.Lapsed, Comment: done.Comment}
	for _, id := range done.IDs {
		out.Created = append(out.Created, string(id))
	}
	return out, nil
}

func handoff(ctx context.Context, pr *served, a store.Actor, in proto.HandoffArgs) (any, *proto.Error) {
	h := storeHandoff(in.Note, in.HandoffFields)
	if in.Park {
		done, err := pr.store.ReplayHandoff(ctx, a, store.IssueID(in.ID), in.Epoch, h, in.Release, in.Idem, in.Paths)
		if err != nil {
			return nil, pr.mapErr(ctx, proto.OpHandoff, in.ID, 0, err)
		}
		return proto.HandoffResult{WriteResult: proto.WriteResult{ID: string(done.ID), Rev: int64(done.Rev)},
			Lapsed: done.Lapsed, Comment: done.Comment}, nil
	}
	is, err := pr.store.HandoffIssue(ctx, a, store.IssueID(in.ID), in.Epoch, h, in.Release, in.Idem, in.Paths)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpHandoff, in.ID, 0, err)
	}
	return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
}

func accept(ctx context.Context, pr *served, a store.Actor, in proto.AcceptArgs) (any, *proto.Error) {
	items, err := pr.store.Accept(ctx, a, store.IssueID(in.ID), store.Acceptance{Tick: in.Tick, Untick: in.Untick, Waive: in.Waive, IdempotencyKey: in.Idem})
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpAccept, in.ID, 0, err)
	}
	out := proto.AcceptResult{ID: in.ID, Items: wireItems(items)}
	for _, it := range items {
		if it.State == store.ItemOpen {
			out.Open = append(out.Open, it.N)
		}
	}
	return out, nil
}

func renew(ctx context.Context, pr *served, a store.Actor, in proto.RenewArgs) (any, *proto.Error) {
	var d time.Duration
	var perr *proto.Error
	if in.All {
		// Renewing every session's claims is a person's call before going
		// away (`sfx away`), not an agent's: only their own terminal's
		// session may ask (S-12).
		if a.Session != proto.CLISession {
			return nil, proto.Errf(proto.CodeInvalid,
				"run `sfx away` from your own terminal, outside an agent session; an agent renews only its own claims",
				fmt.Sprintf("session %s may renew only its own claims", a.Session))
		}
		d, perr = awayLease(in.Lease, pr.store.AwayMax())
	} else {
		d, perr = lease(proto.OpRenew, in.Lease)
	}
	if perr != nil {
		return nil, perr
	}
	var paths map[store.IssueID][]string
	if len(in.Paths) > 0 {
		paths = make(map[store.IssueID][]string, len(in.Paths))
		for id, ps := range in.Paths {
			paths[store.IssueID(id)] = ps
		}
	}
	cs, err := pr.store.RenewClaims(ctx, a, d, in.All, paths)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpRenew, "", 0, err)
	}
	// A running agent renews every minute, which keeps it in `who`. The
	// harness was recorded at the handshake; empty keeps it.
	pr.touch(ctx, pr.log.With("principal", a.Principal, "session", a.Session), a, "")
	out := proto.ClaimsResult{Claims: []proto.Claim{}}
	for _, c := range cs {
		out.Claims = append(out.Claims, wireClaim(c))
	}
	return out, nil
}

// claims lists every live claim, for a board of who holds what.
func claims(ctx context.Context, pr *served, _ store.Actor, in proto.LimitArgs) (any, *proto.Error) {
	now := pr.store.Now()
	cs, more, err := pr.store.ActiveClaims(ctx, in.Limit)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpClaims, "", 0, err)
	}
	out := proto.ClaimsResult{Claims: []proto.Claim{}, Now: now, More: more}
	for _, c := range cs {
		out.Claims = append(out.Claims, wireClaim(c))
	}
	return out, nil
}

func wireClaim(c store.Claim) proto.Claim {
	return proto.Claim{ID: string(c.Issue), By: c.Holder.Principal, Session: c.Holder.Session,
		Machine: c.Holder.Machine, Epoch: c.Epoch, ClaimedAt: c.ClaimedAt.UTC(), ExpiresAt: c.ExpiresAt.UTC()}
}

func wireComment(c store.Comment) proto.Comment {
	return proto.Comment{ID: c.ID, Author: c.Author, Session: c.Session, Kind: string(c.Kind), Body: c.Body,
		CreatedAt: c.CreatedAt.UTC()}
}

func summary(sm store.Summary) proto.Summary {
	return proto.Summary{ID: string(sm.ID), Title: sm.Title, Status: string(sm.Status), Priority: int(sm.Priority),
		DueAt: timeOrNil(sm.DueAt), DeferUntil: timeOrNil(sm.DeferUntil)}
}

// timeOrNil is t, or nil for the zero time.
func timeOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func summaries(sms []store.Summary) []proto.Summary {
	out := make([]proto.Summary, 0, len(sms))
	for _, sm := range sms {
		out = append(out, summary(sm))
	}
	return out
}

func wireIssue(is store.Issue) proto.Issue {
	return proto.Issue{
		ID: string(is.ID), ParentID: string(is.ParentID), Title: is.Title, Body: is.Body, Design: is.Design,
		Acceptance: is.Acceptance, Notes: is.Notes, Status: string(is.Status), Priority: int(is.Priority),
		Type: string(is.Type), Assignee: is.Assignee, Owner: is.Owner, DueAt: is.DueAt, DeferUntil: is.DeferUntil,
		ExpiresAt: is.ExpiresAt, Ephemeral: is.Ephemeral, Pinned: is.Pinned, Template: is.Template,
		Metadata: is.Metadata, CloseReason: is.CloseReason, CreatedBy: is.CreatedBy, CreatedAt: is.CreatedAt.UTC(),
		UpdatedAt: is.UpdatedAt.UTC(), ClosedAt: is.ClosedAt, Rev: int64(is.Rev), Labels: is.Labels,
		Account: is.Account,
	}
}
