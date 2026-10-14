package server

import (
	"context"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// conflicts lists open conflicts, of an issue or all, a page at a time,
// their sides cut as show cuts long text; or the one conflict asked for
// by id, open or resolved, whole.
func conflicts(ctx context.Context, pr *served, _ store.Actor, in proto.ConflictsArgs) (any, *proto.Error) {
	if in.ID != 0 {
		c, err := pr.store.GetConflict(ctx, in.ID)
		if err != nil {
			return nil, pr.mapErr(ctx, proto.OpConflicts, "", 0, err)
		}
		return proto.ConflictsResult{Conflicts: []proto.Conflict{wireConflict(c, false)}}, nil
	}
	cs, more, err := pr.store.Conflicts(ctx, store.ConflictFilter{Issue: store.IssueID(in.Issue), Limit: in.Limit, After: in.After})
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpConflicts, in.Issue, 0, err)
	}
	out := proto.ConflictsResult{Conflicts: []proto.Conflict{}}
	if more {
		out.Next = cs[len(cs)-1].ID
	}
	for _, c := range cs {
		out.Conflicts = append(out.Conflicts, wireConflict(c, true))
	}
	return out, nil
}

func propose(ctx context.Context, pr *served, a store.Actor, in proto.ProposeArgs) (any, *proto.Error) {
	if _, err := pr.store.ProposeResolution(ctx, a, in.ID, store.Choice(in.Choice), in.Text, in.Why); err != nil {
		return nil, pr.mapErr(ctx, proto.OpPropose, "", 0, err)
	}
	return proto.Empty{}, nil
}

func resolve(ctx context.Context, pr *served, a store.Actor, in proto.ResolveArgs) (any, *proto.Error) {
	is, err := pr.store.ResolveConflict(ctx, a, in.ID, store.Choice(in.Choice), in.Text)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpResolve, "", 0, err)
	}
	return proto.WriteResult{ID: string(is.ID), Rev: int64(is.Rev)}, nil
}

// wireConflict is c on the wire; cut cuts each side, and a proposal's
// text, to show's long text.
func wireConflict(c store.Conflict, cut bool) proto.Conflict {
	out := proto.Conflict{ID: c.ID, Issue: string(c.Issue), Field: c.Field, BaseRev: int64(c.BaseRev),
		Base: c.Base, Mine: c.Mine, Theirs: c.Theirs, By: c.Author, Session: c.Session, TheirsBy: c.TheirsBy,
		TheirsAt: c.TheirsAt.UTC(), ParkedAt: c.ParkedAt.UTC(), Resolution: c.Resolution, ResolvedBy: c.ResolvedBy,
		ResolvedAt: c.ResolvedAt, Advice: c.Advice}
	if p := c.Proposal; p != nil {
		out.Proposal = &proto.Proposal{Choice: string(p.Choice), Text: p.Text, Why: p.Why, By: p.By, At: p.At.UTC()}
	}
	if cut {
		fields := []*string{&out.Base, &out.Mine, &out.Theirs}
		if out.Proposal != nil {
			fields = append(fields, &out.Proposal.Text)
		}
		for _, f := range fields {
			if s, ok := truncate(*f, compactText); ok {
				*f, out.Truncated = s, true
			}
		}
	}
	return out
}
