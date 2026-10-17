package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// testWorkflowDoc is a workflow as `aries admin workflow set` sends it.
const testWorkflowDoc = `{"version": 1, "types": {
  "default": {"acceptance": ["PR opened", "Tests run", {"text": "Merged", "check": "landed"}], "checks": ["landed"]},
  "epic": {"approve": {"approvers": ["bob"]}}}}`

func workflowArgs(doc string, rev int64) proto.WorkflowSetArgs {
	return proto.WorkflowSetArgs{Workflow: json.RawMessage(doc), Rev: rev}
}

// The workflow over the protocol: an admin at her terminal sets it, any
// member reads it with each type's profile, and an agent's create gets
// its items, counted in the result. Everyone else's set is refused
// forbidden, with a fix that says who may; a bad document is invalid,
// naming the key; a stale rev a conflict.
func TestWorkflowOps(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	for _, tc := range []struct {
		name string
		by   store.Actor
		args proto.WorkflowSetArgs
		code proto.Code
		says string // in the message or the fix
	}{
		{"a member", aliceCLI, workflowArgs(testWorkflowDoc, 0), proto.CodeForbidden, "ask a project admin"},
		{"an admin's agent", dana, workflowArgs(testWorkflowDoc, 0), proto.CodeForbidden, "your own terminal"},
		{"a typo", danaCLI, workflowArgs(`{"version": 1, "types": {"bug": {"acceptence": []}}}`, 0), proto.CodeInvalid, "types.bug.acceptence"},
		{"not JSON", danaCLI, workflowArgs(`"x"`, 0), proto.CodeInvalid, "workflow: must be a mapping"},
		{"the server's scope", danaCLI, proto.WorkflowSetArgs{Workflow: json.RawMessage(testWorkflowDoc), Scope: "server"}, proto.CodeInvalid, "not built yet"},
		{"a stale rev", danaCLI, workflowArgs(testWorkflowDoc, 3), proto.CodeConflict, "--rev"},
	} {
		_, perr := call[proto.WorkflowSetResult](t, s, tc.by, proto.OpWorkflowSet, tc.args)
		if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message+" "+perr.Fix, tc.says) {
			t.Errorf("%s: workflow.set = %+v, want %s saying %q", tc.name, perr, tc.code, tc.says)
		}
	}
	if r := mustCall[proto.WorkflowResult](t, s, alice, proto.OpWorkflow, proto.WorkflowArgs{}); r.Rev != 0 || r.Workflow != nil {
		t.Fatalf("workflow before any set = %+v, want none", r)
	}

	set := mustCall[proto.WorkflowSetResult](t, s, danaCLI, proto.OpWorkflowSet, workflowArgs(testWorkflowDoc, 0))
	if set.Rev != 1 || !set.Changed {
		t.Errorf("workflow.set = %+v, want rev 1, changed", set)
	}
	if again := mustCall[proto.WorkflowSetResult](t, s, danaCLI, proto.OpWorkflowSet, workflowArgs(testWorkflowDoc, 1)); again.Rev != 1 || again.Changed {
		t.Errorf("workflow.set of the same = %+v, want rev 1, unchanged", again)
	}
	r := mustCall[proto.WorkflowResult](t, s, alice, proto.OpWorkflow, proto.WorkflowArgs{})
	if r.Rev != 1 || r.SetBy != "dana" || r.SetAt == "" || len(r.Effective) != 5 || r.Effective["epic"].Approve == nil ||
		len(r.Effective["bug"].Acceptance) != 3 || r.Effective["bug"].Acceptance[2].Check != "landed" {
		t.Errorf("workflow = %+v, want rev 1 by dana with five types' profiles", r)
	}
	if got, want := r.Summary(), "workflow rev 1: bug, feature, task, chore: 3 items, landed; epic: approval"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}

	c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work", Acceptance: "- mine"})
	if c.Items != 3 {
		t.Errorf("create's items = %d, want 3", c.Items)
	}
	sh := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: c.ID})
	if len(sh.Items) != 4 || sh.Items[0].Text != "mine" || sh.Items[3].Text != "Merged" {
		t.Errorf("show's items = %+v, want mine and the three defaults", sh.Items)
	}
	if e := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "plan", Type: "epic"}); e.Items != 0 {
		t.Errorf("an epic's items = %d, want none", e.Items)
	}
}

// On a server of many projects, a project's admin sets its workflow, and
// the principals it names must be members of that project.
func TestWorkflowMembers(t *testing.T) {
	t.Parallel()
	m := adminWorld(t, Limits{})
	own := mustDialTo(t, m.Server, aliceCLI, projA)
	names := func(who string) proto.WorkflowSetArgs {
		return workflowArgs(`{"version": 1, "types": {"epic": {"approve": {"approvers": ["`+who+`"]}}}}`, 0)
	}
	if _, perr := ask[proto.WorkflowSetResult](own, proto.OpWorkflowSet, names("erin")); perr == nil ||
		perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "erin is not a member") {
		t.Errorf("a workflow naming erin, of another project = %+v, want invalid naming her", perr)
	}
	if set := mustAsk[proto.WorkflowSetResult](own, proto.OpWorkflowSet, names("bob")); set.Rev != 1 {
		t.Errorf("a workflow naming bob = %+v, want rev 1", set)
	}
	bob := mustDialTo(t, m.Server, bobCLI, projA)
	if _, perr := ask[proto.WorkflowSetResult](bob, proto.OpWorkflowSet, names("bob")); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("a member's set = %+v, want forbidden", perr)
	}
}
