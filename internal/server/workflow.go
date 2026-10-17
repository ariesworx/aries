package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// workflowSet applies a project's workflow (design §21.3): a project
// admin's act, from the session cli, refused to anyone else, and never
// over MCP, which has no tool for it (rule 12).
func workflowSet(ctx context.Context, pr *served, a store.Actor, in proto.WorkflowSetArgs) (any, *proto.Error) {
	switch in.Scope {
	case "", "project":
	case "server":
		return nil, proto.Errf(proto.CodeInvalid, "apply it to each project for now, with `aries admin workflow set FILE` in the project's repository",
			"a server's default workflow is not built yet")
	default:
		return nil, proto.Errf(proto.CodeInvalid, "give scope project, or leave it out", fmt.Sprintf("scope %q is not a workflow's scope", in.Scope))
	}
	w, err := proto.ParseWorkflow(in.Workflow)
	if err != nil {
		return nil, proto.Errf(proto.CodeInvalid, workflowFix, "workflow: "+err.Error())
	}
	rev, changed, err := pr.store.SetWorkflow(ctx, a, w, in.Rev)
	if err != nil {
		return nil, pr.workflowErr(ctx, err)
	}
	return proto.WorkflowSetResult{Rev: rev, Changed: changed}, nil
}

// workflowFix is the fix of a document the server refused.
const workflowFix = "correct the file and apply it again; `aries workflow check FILE` reads it the same way"

// workflowErr words a refused workflow.set.
func (pr *served) workflowErr(ctx context.Context, err error) *proto.Error {
	var person *store.InPersonError
	var forbidden *store.ForbiddenError
	text := strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": ")
	switch {
	case errors.As(err, &person):
		return proto.Errf(proto.CodeForbidden,
			"run `aries admin workflow set FILE` yourself, in your own terminal; an agent never changes the workflow it works under",
			person.Error())
	case errors.As(err, &forbidden):
		return proto.Errf(proto.CodeForbidden,
			"ask a project admin to apply it with `aries admin workflow set FILE`; `aries workflow check FILE` says whether it differs",
			"setting the workflow is for the project's admins")
	case errors.Is(err, store.ErrConflict):
		return proto.Errf(proto.CodeConflict,
			"read it again with `aries admin workflow show`, check the change with `aries admin workflow diff FILE`, and apply it with the new --rev",
			text)
	case errors.Is(err, store.ErrInvalid):
		fix := workflowFix
		for _, l := range []string{"workflow_bytes", "workflow_items_per_type", "workflow_checks_per_type", "workflow_closeout_per_type", "approvers_per_gate"} {
			if strings.Contains(text, "("+l+")") {
				fix = fmt.Sprintf("shorten the file, or ask the server admin to raise %s under limits: in ariesd's config", l)
			}
		}
		return proto.Errf(proto.CodeInvalid, fix, text)
	}
	return pr.mapErr(ctx, proto.OpWorkflowSet, "", 0, err)
}

// workflow reads the project's workflow, for `aries admin workflow` and
// prime's line; any member may.
func workflow(ctx context.Context, pr *served, _ store.Actor, _ proto.WorkflowArgs) (any, *proto.Error) {
	ws, err := pr.store.Workflow(ctx)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpWorkflow, "", 0, err)
	}
	out := proto.WorkflowResult{Rev: ws.Rev, Workflow: ws.Workflow, SetBy: ws.SetBy}
	if ws.Workflow != nil {
		out.Effective = ws.Workflow.Effective()
		if !ws.SetAt.IsZero() {
			out.SetAt = ws.SetAt.Format(time.RFC3339)
		}
	}
	return out, nil
}
