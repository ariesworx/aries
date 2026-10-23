package server

import (
	"context"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// brief records the brief a session is started with (design §22). The
// recorder is the connection's principal and session, never an argument.
func brief(ctx context.Context, pr *served, a store.Actor, in proto.BriefArgs) (any, *proto.Error) {
	f := store.BriefFields{Harness: in.Harness, Model: in.Model, Effort: in.Effort, Session: in.Session, Template: in.Template}
	b, err := pr.store.RecordBrief(ctx, a, store.IssueID(in.ID), f, in.Text, in.Idem)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpBrief, in.ID, 0, err)
	}
	return proto.BriefResult{ID: b.Comment, SHA256: b.SHA256, State: b.State, Claim: wireBriefClaim(b.Claim)}, nil
}

// wireBrief is b as show returns it, its text only with full.
func wireBrief(b store.Brief, full bool) proto.Brief {
	w := proto.Brief{Comment: b.Comment, By: b.By, BySession: b.BySession, Harness: b.Harness, Model: b.Model,
		Effort: b.Effort, Session: b.Session, Template: b.Template, SHA256: b.SHA256, At: b.At.UTC(), State: b.State,
		Claim: wireBriefClaim(b.Claim)}
	if full {
		w.Text = b.Text
	}
	return w
}

func wireBriefClaim(c *store.BriefClaim) *proto.BriefClaim {
	if c == nil {
		return nil
	}
	return &proto.BriefClaim{By: c.Principal, Session: c.Session, Epoch: c.Epoch, At: c.At.UTC()}
}

// briefErr is the refusal of a brief, with what to do instead.
func briefErr(e *store.BriefError) *proto.Error {
	msg := e.Error()[len(store.ErrInvalid.Error())+2:]
	switch e.Reason {
	case store.BriefSecret:
		return proto.Errf(proto.CodeInvalid, "take the secret out of the brief and record it again: never a secret in a brief", msg)
	case store.BriefOwnClaim:
		return proto.Errf(proto.CodeInvalid,
			"the brief is recorded by whoever starts the session, from its own session, before the session starts the issue", msg)
	default:
		return proto.Errf(proto.CodeInvalid,
			"a claim has one brief: record the next session's brief before it starts the issue, naming its session", msg)
	}
}
