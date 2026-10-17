package proto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// exampleWorkflow is design §21.3's example, as aries sends it.
const exampleWorkflow = `{"version": 1, "ci": ["ci-example"], "types": {
  "default": {
    "acceptance": [
      "PR opened from the template, saying what ran and what was left to CI",
      "Tests run as AGENTS.md's Test tiers require for the change",
      {"text": "CI green on the PR", "check": "build"},
      {"text": "Reviewed and merged by the maintainer", "check": "landed"}],
    "checks": ["build", "landed"],
    "closeout": ["Delete files you left outside the repository"]},
  "epic": {"acceptance": [], "checks": [], "approve": {"approvers": ["alice"]}},
  "chore": {"acceptance": ["The result is posted where the issue says"], "checks": []}}}`

func TestParseWorkflow(t *testing.T) {
	t.Parallel()
	w, err := ParseWorkflow([]byte(exampleWorkflow))
	if err != nil {
		t.Fatalf("ParseWorkflow(the example): %v", err)
	}
	def := w.Types["default"]
	if len(def.Acceptance) != 4 || def.Acceptance[3] != (WorkflowItem{Text: "Reviewed and merged by the maintainer", Check: "landed"}) ||
		def.Acceptance[0].Check != "" || w.Types["epic"].Approve == nil || w.Types["epic"].Approve.Approvers[0] != "alice" {
		t.Errorf("ParseWorkflow(the example) = %+v", w)
	}
	// What the server sends back reads as what was sent.
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseWorkflow(b)
	if err != nil || !reflect.DeepEqual(again, w) {
		t.Errorf("ParseWorkflow(Marshal(w)) = %+v, %v; want %+v", again, err, w)
	}
	var lenient Workflow
	if err := json.Unmarshal(b, &lenient); err != nil || !reflect.DeepEqual(lenient, w) {
		t.Errorf("json.Unmarshal(Marshal(w)) = %+v, %v; want %+v", lenient, err, w)
	}

	for _, tc := range []struct{ name, doc, want string }{
		{"not an object", `[]`, "workflow: must be a mapping"},
		{"no version", `{"types": {}}`, "version is missing"},
		{"another version", `{"version": 2}`, "version must be 1"},
		{"an unknown key", `{"version": 1, "type": {}}`, "type: unknown key"},
		{"an unknown type", `{"version": 1, "types": {"story": {}}}`, "types.story: not an issue type"},
		{"a gate", `{"version": 1, "types": {"gate": {}}}`, "types.gate: not an issue type"},
		{"a typo in a profile", `{"version": 1, "types": {"bug": {"acceptence": []}}}`, "types.bug.acceptence: unknown key; a profile takes"},
		{"an item of the wrong kind", `{"version": 1, "types": {"bug": {"acceptance": [3]}}}`, "types.bug.acceptance[0]: an item is a line of text"},
		{"an unknown item key", `{"version": 1, "types": {"bug": {"checks": ["x"], "acceptance": [{"text": "a", "chek": "x"}]}}}`, "types.bug.acceptance[0].chek: unknown key"},
		{"a bound check not required", `{"version": 1, "types": {"bug": {"acceptance": [{"text": "a", "check": "landed"}]}}}`, "check landed is not among types.bug.checks"},
		{"a bad check name", `{"version": 1, "types": {"bug": {"checks": ["Landed"]}}}`, "types.bug.checks[0]: \"Landed\" is not a check's name"},
		{"a check twice", `{"version": 1, "types": {"bug": {"checks": ["a", "a"]}}}`, "types.bug.checks[1]: a is named twice"},
		{"ci not a list", `{"version": 1, "ci": "bob"}`, "ci: must be a list"},
		{"closeout not text", `{"version": 1, "types": {"bug": {"closeout": [{}]}}}`, "types.bug.closeout[0]: must be a line of text"},
		{"approvers unknown key", `{"version": 1, "types": {"epic": {"approve": {"by": ["x"]}}}}`, "types.epic.approve.by: unknown key"},
	} {
		if _, err := ParseWorkflow([]byte(tc.doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: ParseWorkflow(%s) = %v, want an error holding %q", tc.name, tc.doc, err, tc.want)
		}
	}
}

func TestWorkflowSummary(t *testing.T) {
	t.Parallel()
	w, err := ParseWorkflow([]byte(exampleWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	r := WorkflowResult{Rev: 4, Workflow: &w, Effective: w.Effective()}
	if got, want := r.Summary(), "workflow rev 4: bug, feature, task: 4 items, build, landed; epic: approval; chore: 1 item"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	if got := (WorkflowResult{}).Summary(); got != "" {
		t.Errorf("Summary() of no workflow = %q, want empty", got)
	}
	empty := Workflow{Version: 1}
	if got, want := (WorkflowResult{Rev: 2, Workflow: &empty}).Summary(), "workflow rev 2: no defaults"; got != want {
		t.Errorf("Summary() of an empty workflow = %q, want %q", got, want)
	}
	if _, _, ok := w.Profile("gate"); ok {
		t.Error("Profile(gate) found one; a gate gets none")
	}
	if _, name, _ := w.Profile("bug"); name != WorkflowDefault {
		t.Errorf("Profile(bug) is %q's, want default's", name)
	}
}
