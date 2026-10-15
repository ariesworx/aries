package server

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
	"github.com/ariesworx/aries/internal/store"
)

// Gates and dates (design §20.3; stage 6, slice G0). Who may pass, waive
// and rearm a gate, and that only from the session cli, is the store's
// to decide (store/gates.go), so a request that reaches it any other way
// is held to the same.

func gateAdd(ctx context.Context, pr *served, a store.Actor, in proto.GateAddArgs) (any, *proto.Error) {
	g, err := pr.store.AddGate(ctx, a, store.NewGate{Kind: store.GateKind(in.Kind), Blocks: store.IssueID(in.Blocks), Title: in.Title,
		Approvers: in.Approvers, Check: in.Check, Subject: store.IssueID(in.Subject), IdempotencyKey: in.Idem})
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpGateAdd, cmp.Or(in.Blocks, in.Subject), 0, err)
	}
	return wireGate(g), nil
}

// gatePass passes a gate: a person's decision by the gate's id, or a CI
// job's report of a check (proto.GateArgs.Reported).
func gatePass(ctx context.Context, pr *served, a store.Actor, in proto.GateArgs) (any, *proto.Error) {
	if in.Reported() {
		return gateReport(ctx, pr, a, proto.OpGatePass, in, pr.store.PassCheck)
	}
	return gateDecide(ctx, pr, a, proto.OpGatePass, in, pr.store.PassGate)
}

func gateFail(ctx context.Context, pr *served, a store.Actor, in proto.GateArgs) (any, *proto.Error) {
	return gateReport(ctx, pr, a, proto.OpGateFail, in, pr.store.FailCheck)
}

// gateReport runs a CI job's report of a check, passed or failed.
func gateReport(ctx context.Context, pr *served, a store.Actor, op string, in proto.GateArgs,
	report func(context.Context, store.Actor, store.Report) (store.Gate, error)) (any, *proto.Error) {
	subject, perr := pr.reportSubject(in)
	if perr != nil {
		return nil, perr
	}
	g, err := report(ctx, a, store.Report{Gate: store.IssueID(in.ID), Subject: subject, Check: in.Check,
		SHA: in.SHA, URL: in.URL, Reason: in.Reason})
	if err != nil {
		return nil, pr.mapErr(ctx, op, cmp.Or(in.ID, string(subject)), 0, err)
	}
	return wireGate(g), nil
}

// maxBranch bounds a branch name a report gives, as git's refs are in
// practice.
const maxBranch = 255

// reportSubject is the issue a report names: in.Subject, or the issue
// in.Branch names with the project's prefix, as `sfx start --branch`
// names a branch (gitx.Branch). A branch that names none, or another
// issue than in.Subject, is refused.
func (pr *served) reportSubject(in proto.GateArgs) (store.IssueID, *proto.Error) {
	if in.Branch == "" {
		return store.IssueID(in.Subject), nil
	}
	if len(in.Branch) > maxBranch || !safetext.ValidLine(in.Branch) {
		return "", proto.Errf(proto.CodeInvalid, "give the branch's name as git prints it",
			fmt.Sprintf("branch must be one line of at most %d bytes", maxBranch))
	}
	id, ok := gitx.IDFromBranch(in.Branch, pr.store.Prefix())
	if !ok {
		// not_found, as a subject with no gate for the check is: there is
		// nothing to report on, which `sfx gate pass --optional` allows.
		return "", proto.Errf(proto.CodeNotFound,
			"report on a branch named for its issue, as `sfx start --branch` names it ("+gitx.Branch("feature", pr.store.Prefix()+"-a1b2", "title")+
				"), or name the issue with --issue ID",
			fmt.Sprintf("branch %s names no issue of this project", in.Branch))
	}
	if in.Subject != "" && in.Subject != id {
		return "", proto.Errf(proto.CodeInvalid, "give one of --branch and --issue",
			fmt.Sprintf("branch %s names %s, not %s", in.Branch, id, in.Subject))
	}
	return store.IssueID(id), nil
}

func gateWaive(ctx context.Context, pr *served, a store.Actor, in proto.GateArgs) (any, *proto.Error) {
	return gateDecide(ctx, pr, a, proto.OpGateWaive, in, pr.store.WaiveGate)
}

func gateRearm(ctx context.Context, pr *served, a store.Actor, in proto.GateArgs) (any, *proto.Error) {
	return gateDecide(ctx, pr, a, proto.OpGateRearm, in, pr.store.RearmGate)
}

// gateDecide runs a pass, waive or rearm of the gate in.ID.
func gateDecide(ctx context.Context, pr *served, a store.Actor, op string, in proto.GateArgs,
	decide func(context.Context, store.Actor, store.IssueID, string) (store.Gate, error)) (any, *proto.Error) {
	g, err := decide(ctx, a, store.IssueID(in.ID), in.Reason)
	if err != nil {
		return nil, pr.mapErr(ctx, op, in.ID, 0, err)
	}
	return wireGate(g), nil
}

func gates(ctx context.Context, pr *served, _ store.Actor, in proto.GatesArgs) (any, *proto.Error) {
	gs, err := pr.store.Gates(ctx, store.GateFilter{All: in.All, Limit: in.Limit})
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpGates, "", 0, err)
	}
	return proto.GatesResult{Gates: wireGates(gs)}, nil
}

func dates(ctx context.Context, pr *served, _ store.Actor, in proto.DatesArgs) (any, *proto.Error) {
	since, window, err := parseSince(in.Since)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpDates, "", 0, err)
	}
	if since.IsZero() {
		since = pr.store.Now().Add(-window)
	}
	d, err := pr.store.Dates(ctx, store.DatesFilter{Since: since, DueSoon: pr.dueSoon})
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpDates, "", 0, err)
	}
	return proto.DatesResult{Since: d.Since.UTC(), Until: d.Now.UTC(), Ready: digestItems(d.Ready),
		DueSoon: digestItems(d.DueSoon), Overdue: digestItems(d.Overdue), Truncated: d.Capped,
		Totals: proto.DateTotals{Ready: d.Ready.Total, DueSoon: d.DueSoon.Total, Overdue: d.Overdue.Total}}, nil
}

// issueDate reads a create's or update's date argument named name: nil
// for none, and a zero time, which clears it, for "".
func issueDate(name, s string) (*time.Time, *proto.Error) {
	t, err := proto.ParseDate(s)
	if err != nil {
		return nil, proto.Errf(proto.CodeInvalid, fmt.Sprintf("give %s as a date, 2026-11-02, or a time, 2026-11-02T09:00:00Z; an empty one clears it", name),
			fmt.Sprintf("%s: %v", name, err))
	}
	return &t, nil
}

// gatesOf returns the open gates among the issues ids, for blocked.
func (pr *served) gatesOf(ctx context.Context, ids []store.IssueID) ([]proto.Gate, error) {
	slices.Sort(ids)
	gs, err := pr.store.GatesOf(ctx, slices.Compact(ids))
	if err != nil {
		return nil, err
	}
	return wireGates(gs), nil
}

func wireGates(gs []store.Gate) []proto.Gate {
	out := make([]proto.Gate, 0, len(gs))
	for _, g := range gs {
		out = append(out, wireGate(g))
	}
	return out
}

func wireGate(g store.Gate) proto.Gate {
	out := proto.Gate{ID: string(g.ID), Title: g.Title, Kind: string(g.Kind), Approvers: g.Approvers,
		Outcome: string(g.Outcome), By: g.By, At: g.At, Reason: g.Reason, CreatedBy: g.CreatedBy, CreatedAt: g.CreatedAt.UTC(),
		Check: g.Check, Subject: string(g.Subject)}
	if out.Approvers == nil {
		out.Approvers = []string{}
	}
	if r := g.Report; r != nil {
		out.Report = &proto.GateReport{Result: string(r.Result), By: r.By, At: r.At.UTC(), SHA: r.SHA, URL: r.URL, Reason: r.Reason}
	}
	for _, id := range g.Holds {
		out.Holds = append(out.Holds, string(id))
	}
	return out
}

// gateErr refuses what a gate does not allow, naming who may pass it and
// the command a person runs.
func gateErr(e *store.GateError) *proto.Error {
	who := e.ApproverText()
	switch e.Reason {
	case store.GateHeld:
		msg := fmt.Sprintf("%s is held by gate %s, which %s may pass", e.ID, e.Gate, who)
		if e.Op == "start" {
			msg = fmt.Sprintf("%s waits behind gate %s, which %s may pass", e.ID, e.Gate, who)
		}
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf(proto.FixGate+" %s to pass gate %s (`sfx gate pass %s`); `sfx ready` lists the work nothing holds", who, e.Gate, e.Gate), msg)
	case store.GateIsGate:
		if e.Gate == "" {
			return proto.Errf(proto.CodeInvalid, proto.FixGateAdd+" `sfx gate add --blocks ID`, which makes the gate and what it holds",
				"an issue becomes a gate only through gate add")
		}
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf(proto.FixGate+" %s to pass it (`sfx gate pass %s`); a gate is opened and closed only by the gate commands", who, e.Gate),
			fmt.Sprintf("%s is a gate; %s may pass it", e.Gate, who))
	case store.GateNotPerson:
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf(proto.FixGate+" a person to run `sfx gate %s %s` at their own terminal, outside an agent session", e.Op, e.Gate),
			fmt.Sprintf("gate %s is decided by a person at their own terminal, and this request came from an agent's session", e.Gate))
	case store.GateNotAdmin:
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf(proto.FixGate+" %s to pass gate %s, or a project admin to waive it", who, e.Gate),
			fmt.Sprintf("waiving gate %s is for the project's admins", e.Gate))
	case store.GateNotCI:
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf(proto.FixGate+" CI to report check %s as %s, from its job; a person named by the gate may run `sfx gate %s %s` at their own terminal", e.Check, who, e.Op, e.Gate),
			fmt.Sprintf("gate %s takes reports from a CI job or a person's terminal, and this request came from an agent's session", e.Gate))
	case store.GateNoCheck:
		return proto.Errf(proto.CodeNotFound,
			fmt.Sprintf("add one with `sfx gate add --kind ci --check %s --subject %s --by PRINCIPAL`; `sfx gate list` lists the gates", e.Check, e.ID),
			fmt.Sprintf("%s has no ci gate for check %s", e.ID, e.Check))
	}
	if e.Check != "" {
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf(proto.FixGate+" CI to report check %s as %s, the principal gate %s names; an admin may waive it (`sfx gate waive %s --reason TEXT`)", e.Check, who, e.Gate, e.Gate),
			fmt.Sprintf("only %s may %s gate %s", who, e.Op, e.Gate))
	}
	return proto.Errf(proto.CodeForbidden,
		fmt.Sprintf(proto.FixGate+" %s to %s gate %s (`sfx gate %s %s`)", who, e.Op, e.Gate, e.Op, e.Gate),
		fmt.Sprintf("only %s may %s gate %s", who, e.Op, e.Gate))
}
