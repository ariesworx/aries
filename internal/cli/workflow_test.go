package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// repoWorkflow is this repository's workflow template,
// .aries/workflow.example.yaml, which names alice where the applied copy
// names the server's principals.
func repoWorkflow(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", ".aries", "workflow.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// This repository's workflow template reads as a workflow: the one its
// maintainer copies, names the server's principals in, and applies
// (design §21.3). It names no real principal.
func TestRepoWorkflow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "workflow.yaml"), repoWorkflow(t), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner{dir: dir}
	_, w, err := r.readWorkflow("workflow.yaml")
	if err != nil {
		t.Fatalf("readWorkflow(.aries/workflow.example.yaml): %v", err)
	}
	def := w.Types[proto.WorkflowDefault]
	if len(def.Acceptance) != 4 || !slices.Equal(def.Checks, []string{"landed"}) || w.Types["epic"].Approve == nil ||
		!slices.Equal(w.Types["epic"].Approve.Approvers, []string{"alice"}) || !slices.Equal(w.CI, []string{"alice"}) ||
		len(w.Types["chore"].Acceptance) != 1 {
		t.Errorf("the repository's workflow = %+v", w)
	}
}

// What show prints reads back as the document it shows.
func TestWorkflowYAMLRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "w.yaml"), repoWorkflow(t), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner{dir: dir}
	_, w, err := r.readWorkflow("w.yaml")
	if err != nil {
		t.Fatal(err)
	}
	b, err := workflowYAML(w)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "{") {
		t.Errorf("workflowYAML holds flow style:\n%s", b)
	}
	if err := os.WriteFile(filepath.Join(dir, "again.yaml"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, again, err := r.readWorkflow("again.yaml")
	if err != nil || !reflect.DeepEqual(again, w) {
		t.Errorf("readWorkflow(workflowYAML(w)) = %+v, %v; want %+v\n%s", again, err, w, b)
	}
}

func TestReadWorkflowRefuses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"typo.yaml": "version: 1\ntypes:\n  bug:\n    acceptence: [x]\n",
		"bad.yaml":  "version: [1\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := &runner{dir: dir}
	for _, tc := range []struct{ file, want string }{
		{"typo.yaml", "typo.yaml: types.bug.acceptence: unknown key"},
		{"bad.yaml", "bad.yaml is not YAML or JSON"},
		{"missing.yaml", "cannot read missing.yaml"},
	} {
		_, _, err := r.readWorkflow(tc.file)
		var perr *proto.Error
		if !errors.As(err, &perr) || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, tc.want) {
			t.Errorf("readWorkflow(%s) = %v, want invalid saying %q", tc.file, err, tc.want)
		}
	}
}

func TestWorkflowDiff(t *testing.T) {
	t.Parallel()
	parse := func(doc string) *proto.Workflow {
		w, err := proto.ParseWorkflow([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		return &w
	}
	was := parse(`{"version": 1, "ci": ["alice"], "types": {
  "default": {"acceptance": ["a", "b"], "checks": ["landed"]},
  "epic": {"approve": {"approvers": ["bob"]}}}}`)
	to := parse(`{"version": 1, "ci": ["alice", "carol"], "types": {
  "default": {"acceptance": ["b", "a", {"text": "c", "check": "landed"}], "checks": ["landed"], "review": {"approvers": ["dana"]}},
  "chore": {"acceptance": ["posted"]}}}`)
	want := []string{
		"ci: + carol",
		"types.default.acceptance: + c (check landed)",
		"+ types.default.review",
		"types.default.review.approvers: + dana",
		"- types.epic",
		"- types.epic.approve",
		"+ types.chore",
		"types.chore.acceptance: + posted",
	}
	if got := workflowDiff(was, to); !slices.Equal(got, want) {
		t.Errorf("workflowDiff =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := workflowDiff(was, was); len(got) != 0 {
		t.Errorf("workflowDiff(w, w) = %q, want nothing", got)
	}
	if got := workflowDiff(parse(`{"version": 1, "ci": ["a", "b"]}`), parse(`{"version": 1, "ci": ["b", "a"]}`)); !slices.Equal(got, []string{"ci: reordered"}) {
		t.Errorf("workflowDiff of a reordered list = %q", got)
	}
	if got := workflowDiff(nil, parse(`{"version": 1}`)); !slices.Equal(got, []string{"version: 0 -> 1"}) {
		t.Errorf("workflowDiff(none, empty) = %q", got)
	}
	// A list's order is reported once, not item by item.
	b, _ := json.Marshal(to)
	if !strings.Contains(string(b), `"check":"landed"`) {
		t.Errorf("Marshal(to) = %s, want the bound item as an object", b)
	}
}

func TestWorkflowUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"admin", "workflow"}, "admin workflow needs show, set, diff or check"},
		{[]string{"admin", "workflow", "set"}, "workflow set needs one file"},
		{[]string{"admin", "workflow", "show", "x"}, "workflow show takes no arguments"},
		{[]string{"admin", "workflow", "diff", "f", "--rev", "2"}, "only workflow set takes --rev"},
		{[]string{"admin", "workflow", "apply", "f"}, `unknown workflow action "apply"`},
		{[]string{"workflow"}, "workflow needs show, set, diff or check"},
		{[]string{"workflow", "set"}, "workflow set needs one file"},
		{[]string{"workflow", "apply", "f"}, `unknown workflow action "apply"`},
		{[]string{"workflow", "show", "x"}, "workflow show takes no arguments"},
		{[]string{"admin", "workflow", "check"}, "workflow check needs one file"},
		{[]string{"workflow", "check"}, "workflow check needs one file"},
	} {
		code, out, errb := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
		if code != ExitUsage || out != "" || !strings.Contains(errb, tc.want) {
			t.Errorf("aries %s: exit %d, stdout %q, stderr %q; want usage saying %q", strings.Join(tc.args, " "), code, out, errb, tc.want)
		}
	}
}
