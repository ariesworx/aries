package server

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// Gates and dates (design §20.3; stage 6, slice G0). Who may pass, waive
// and rearm a gate, and that only from the session cli, is the store's
// to decide (store/gates.go), so a request that reaches it any other way
// is held to the same.

func gateAdd(ctx context.Context, pr *served, a store.Actor, in proto.GateAddArgs) (any, *proto.Error) {
	g, err := pr.store.AddGate(ctx, a, store.NewGate{Blocks: store.IssueID(in.Blocks), Title: in.Title,
		Approvers: in.Approvers, IdempotencyKey: in.Idem})
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpGateAdd, in.Blocks, 0, err)
	}
	return wireGate(g), nil
}

func gatePass(ctx context.Context, pr *served, a store.Actor, in proto.GateArgs) (any, *proto.Error) {
	return gateDecide(ctx, pr, a, proto.OpGatePass, in, pr.store.PassGate)
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
		Outcome: string(g.Outcome), By: g.By, At: g.At, Reason: g.Reason, CreatedBy: g.CreatedBy, CreatedAt: g.CreatedAt.UTC()}
	if out.Approvers == nil {
		out.Approvers = []string{}
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
	}
	return proto.Errf(proto.CodeForbidden,
		fmt.Sprintf(proto.FixGate+" %s to %s gate %s (`sfx gate %s %s`)", who, e.Op, e.Gate, e.Op, e.Gate),
		fmt.Sprintf("only %s may %s gate %s", who, e.Op, e.Gate))
}
