package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// danaAdminCLI is the test store's admin at her own terminal.
var danaAdminCLI = Actor{Principal: "dana", Session: proto.CLISession, Machine: "laptop-d"}

// testWorkflow is design §21.3's example, with bob as the epic's
// approver.
func testWorkflow(t *testing.T) proto.Workflow {
	t.Helper()
	w, err := proto.ParseWorkflow([]byte(`{"version": 1, "ci": ["alice"], "types": {
  "default": {
    "acceptance": [
      "PR opened from the template",
      "Tests run as the tiers require",
      {"text": "Reviewed and merged by the maintainer", "check": "landed"}],
    "checks": ["landed"],
    "closeout": ["Delete files you left outside the repository"]},
  "epic": {"acceptance": ["The plan is written down"], "approve": {"approvers": ["bob"]}},
  "chore": {"acceptance": ["The result is posted where the issue says"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func mustSetWorkflow(t *testing.T, s *Store, w proto.Workflow) int64 {
	t.Helper()
	rev, _, err := s.SetWorkflow(t.Context(), danaAdminCLI, w, 0)
	if err != nil {
		t.Fatalf("SetWorkflow: %v", err)
	}
	return rev
}

// itemTexts lists an issue's acceptance items' texts.
func itemTexts(t *testing.T, s *Store, id IssueID) []string {
	t.Helper()
	items, err := s.AcceptanceItems(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

// stampOf returns the workflow stamp on the last event of op on id, and
// whether it has one.
func stampOf(t *testing.T, s *Store, id IssueID, op Op) (WorkflowStamp, bool) {
	t.Helper()
	evs, err := s.History(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range slices.Backward(evs) {
		if e.Op != op {
			continue
		}
		var after struct {
			Workflow *WorkflowStamp `json:"workflow"`
		}
		if err := json.Unmarshal(e.After, &after); err != nil {
			t.Fatal(err)
		}
		if after.Workflow == nil {
			return WorkflowStamp{}, false
		}
		return *after.Workflow, true
	}
	t.Fatalf("no %s event on %s", op, id)
	return WorkflowStamp{}, false
}

// An admin at her terminal sets the workflow; it reads back as set, a
// set of the same document changes nothing, and each change is a
// workflow.set event with the document before and after.
func TestSetWorkflow(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	got, err := s.Workflow(ctx)
	if err != nil || got.Rev != 0 || got.Workflow != nil {
		t.Fatalf("Workflow() of a new project = %+v, %v; want rev 0 and none", got, err)
	}
	w := testWorkflow(t)
	seq := lastSeq(t, s)
	rev, changed, err := s.SetWorkflow(ctx, danaAdminCLI, w, 0)
	if err != nil || rev != 1 || !changed {
		t.Fatalf("SetWorkflow = %d, %v, %v; want rev 1, changed", rev, changed, err)
	}
	got, err = s.Workflow(ctx)
	if err != nil || got.Rev != 1 || !reflect.DeepEqual(*got.Workflow, w) || got.SetBy != "dana" || got.SetAt.IsZero() {
		t.Fatalf("Workflow() = %+v, %v; want rev 1 as set, by dana", got, err)
	}
	evs, err := s.Events(ctx, seq, 10)
	if err != nil || len(evs) != 1 || evs[0].Op != OpWorkflowSet || evs[0].Target != WorkflowTarget || evs[0].Before != nil {
		t.Fatalf("events of the set = %+v, %v; want one workflow.set on %s with no before", evs, err, WorkflowTarget)
	}
	var after struct {
		Rev      int64          `json:"rev"`
		Workflow proto.Workflow `json:"workflow"`
	}
	if err := json.Unmarshal(evs[0].After, &after); err != nil || after.Rev != 1 || !reflect.DeepEqual(after.Workflow, w) {
		t.Errorf("workflow.set's after state = %s, %v; want rev 1 and the document", evs[0].After, err)
	}

	// The same document again: nothing written.
	seq = lastSeq(t, s)
	if rev, changed, err := s.SetWorkflow(ctx, danaAdminCLI, w, 1); err != nil || rev != 1 || changed {
		t.Errorf("SetWorkflow(the same) = %d, %v, %v; want rev 1, unchanged", rev, changed, err)
	}
	if lastSeq(t, s) != seq {
		t.Error("SetWorkflow of the document in force recorded an event")
	}

	// A stale rev is refused; the current one applies.
	w2 := w
	w2.Types = map[string]proto.WorkflowProfile{"bug": {Acceptance: []proto.WorkflowItem{{Text: "A test reproduces it"}}}}
	if _, _, err := s.SetWorkflow(ctx, danaAdminCLI, w2, 7); !errors.Is(err, ErrConflict) {
		t.Errorf("SetWorkflow at rev 7 = %v, want ErrConflict", err)
	}
	if rev, changed, err := s.SetWorkflow(ctx, danaAdminCLI, w2, 1); err != nil || rev != 2 || !changed {
		t.Errorf("SetWorkflow at rev 1 = %d, %v, %v; want rev 2, changed", rev, changed, err)
	}
	evs, err = s.Events(ctx, seq, 10)
	if err != nil || len(evs) != 1 || evs[0].Before == nil {
		t.Fatalf("events of the second set = %+v, %v; want one with a before state", evs, err)
	}
}

// Only an admin, at her own terminal, sets the workflow: a member is
// refused forbidden, and so is an admin from an agent's session.
func TestSetWorkflowWho(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	w := testWorkflow(t)
	for _, tc := range []struct {
		name string
		by   Actor
	}{
		{"a member from her terminal", aliceCLI},
		{"a member from an agent's session", alice},
		{"an admin from an agent's session", Actor{Principal: "dana", Session: "sess-d", Machine: "laptop-d"}},
		{"an admin from a CI job", Actor{Principal: "dana", Session: proto.CISession, Machine: "runner"}},
	} {
		if _, _, err := s.SetWorkflow(ctx, tc.by, w, 0); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: SetWorkflow = %v, want ErrForbidden", tc.name, err)
		}
	}
	if got, _ := s.Workflow(ctx); got.Rev != 0 {
		t.Errorf("after the refusals the workflow is at rev %d, want none", got.Rev)
	}
}

// A document is checked at set: its lines are safe text with no secret,
// each item reads back as one item, the principals it names are
// members, and it is within the workflow limits.
func TestSetWorkflowChecks(t *testing.T) {
	t.Parallel()
	members := []string{"alice", "bob", "dana"}
	s := openStore(t, newDSN(t), Options{
		Limits:   Limits{WorkflowItems: 3, WorkflowChecks: 2, WorkflowCloseout: 2, WorkflowBytes: 1024, GateApprovers: 2},
		IsMember: func(p string) bool { return slices.Contains(members, p) },
	})
	profile := func(p proto.WorkflowProfile) proto.Workflow {
		return proto.Workflow{Version: 1, Types: map[string]proto.WorkflowProfile{"bug": p}}
	}
	items := func(texts ...string) []proto.WorkflowItem {
		var out []proto.WorkflowItem
		for _, t := range texts {
			out = append(out, proto.WorkflowItem{Text: t})
		}
		return out
	}
	for _, tc := range []struct {
		name string
		w    proto.Workflow
		want string
	}{
		{"a control character", profile(proto.WorkflowProfile{Acceptance: items("a\x07b")}), "types.bug.acceptance[0]"},
		{"two lines", profile(proto.WorkflowProfile{Closeout: []string{"a\nb"}}), "types.bug.closeout[0]"},
		{"a blank item", profile(proto.WorkflowProfile{Acceptance: items("  ")}), "types.bug.acceptance[0]"},
		{"an item that ticks itself", profile(proto.WorkflowProfile{Acceptance: items("[x] done already")}), "reads as another item"},
		{"an item with a box", profile(proto.WorkflowProfile{Acceptance: items("[ ] open")}), "reads as another item"},
		{"a secret", profile(proto.WorkflowProfile{Closeout: []string{"export GITHUB_TOKEN=ghp_" + strings.Repeat("a1B2", 9)}}), "looks like it holds a secret"},
		{"a principal not a member", profile(proto.WorkflowProfile{Review: &proto.WorkflowApprovers{Approvers: []string{"erin"}}}), "erin is not a member"},
		{"a ci principal not a member", proto.Workflow{Version: 1, CI: []string{"ci-bot"}}, "ci-bot is not a member"},
		{"a reserved principal", proto.Workflow{Version: 1, CI: []string{"import"}}, "is not a principal name, or is reserved"},
		{"too many items", profile(proto.WorkflowProfile{Acceptance: items("a", "b", "c", "d")}), "workflow_items_per_type"},
		{"too many checks", profile(proto.WorkflowProfile{Checks: []string{"a", "b", "c"}}), "workflow_checks_per_type"},
		{"too many close-out lines", profile(proto.WorkflowProfile{Closeout: []string{"a", "b", "c"}}), "workflow_closeout_per_type"},
		{"too many approvers", profile(proto.WorkflowProfile{Approve: &proto.WorkflowApprovers{Approvers: []string{"alice", "bob", "dana"}}}), "approvers_per_gate"},
		{"too long", profile(proto.WorkflowProfile{Closeout: []string{strings.Repeat("x", 500), strings.Repeat("y", 500)}}), "workflow_bytes"},
		{"another version", proto.Workflow{Version: 2}, "version"},
		{"an unknown type", proto.Workflow{Version: 1, Types: map[string]proto.WorkflowProfile{"story": {}}}, "types.story"},
		{"a bad check", profile(proto.WorkflowProfile{Checks: []string{"Landed"}}), "types.bug.checks[0]"},
		{"a bound check not required", profile(proto.WorkflowProfile{Acceptance: []proto.WorkflowItem{{Text: "a", Check: "landed"}}}), "types.bug.checks"},
	} {
		_, _, err := s.SetWorkflow(t.Context(), danaAdminCLI, tc.w, 0)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: SetWorkflow = %v, want ErrInvalid naming %q", tc.name, err, tc.want)
		}
	}
	// Item text is kept with its whitespace collapsed, as an item is read.
	rev, _, err := s.SetWorkflow(t.Context(), danaAdminCLI, profile(proto.WorkflowProfile{Acceptance: items("  spaced \t out  ")}), 0)
	if err != nil {
		t.Fatalf("SetWorkflow(spaced item) = %v", err)
	}
	got, _ := s.Workflow(t.Context())
	if got.Rev != rev || got.Workflow.Types["bug"].Acceptance[0].Text != "spaced out" {
		t.Errorf("Workflow() = %+v, want the item collapsed to %q", got, "spaced out")
	}
}

// Each limit has a default and a most, as every limit does.
func TestWorkflowLimits(t *testing.T) {
	t.Parallel()
	l := Limits{}.withDefaults()
	if l.WorkflowBytes != 16384 || l.WorkflowItems != 20 || l.WorkflowChecks != 5 || l.WorkflowCloseout != 20 {
		t.Errorf("default workflow limits = %d, %d, %d, %d; want 16384, 20, 5, 20",
			l.WorkflowBytes, l.WorkflowItems, l.WorkflowChecks, l.WorkflowCloseout)
	}
	for _, tc := range []struct {
		l    Limits
		want string
	}{
		{Limits{WorkflowBytes: 65537}, "workflow_bytes"},
		{Limits{WorkflowItems: 101}, "workflow_items_per_type"},
		{Limits{WorkflowChecks: 21}, "workflow_checks_per_type"},
		{Limits{WorkflowChecks: 6, GateChecks: 5}, "workflow_checks_per_type"},
		{Limits{WorkflowCloseout: 101}, "workflow_closeout_per_type"},
		{Limits{WorkflowItems: -1}, "workflow_items_per_type"},
	} {
		if err := tc.l.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%+v) = %v, want an error naming %s", tc.l, err, tc.want)
		}
	}
	if err := (Limits{WorkflowBytes: 65536, WorkflowItems: 100, WorkflowChecks: 20, WorkflowCloseout: 100}).Validate(); err != nil {
		t.Errorf("Validate(the most of each) = %v", err)
	}
}

// A create gets its type's profile's items after its own, each once, and
// its event names the workflow's rev and the profile.
func TestWorkflowDefaultsAtCreate(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	none, err := s.Create(ctx, alice, NewIssue{Title: "before any workflow", Acceptance: "- mine"})
	if err != nil || none.Items != 0 || !slices.Equal(itemTexts(t, s, none.ID), []string{"mine"}) {
		t.Fatalf("Create with no workflow = %+v, %v; want only its own item", none, err)
	}
	if _, ok := stampOf(t, s, none.ID, OpIssueCreate); ok {
		t.Error("a create with no workflow set records one")
	}
	rev := mustSetWorkflow(t, s, testWorkflow(t))
	defaults := []string{"PR opened from the template", "Tests run as the tiers require", "Reviewed and merged by the maintainer"}

	for _, tc := range []struct {
		name       string
		in         NewIssue
		want       []string
		items      int
		profile    string
		acceptance string
	}{
		{"a bug with no items", NewIssue{Type: TypeBug}, defaults, 3, "default", ""},
		{"a task with its own items, one a default", NewIssue{Acceptance: "- [ ] mine\n- Tests run as  the tiers require\n"},
			[]string{"mine", "Tests run as the tiers require", "PR opened from the template", "Reviewed and merged by the maintainer"}, 2, "default", ""},
		{"prose acceptance", NewIssue{Type: TypeFeature, Acceptance: "It works\non both systems."},
			append([]string{"It works on both systems."}, defaults...), 3, "default",
			"- It works on both systems.\n- PR opened from the template\n- Tests run as the tiers require\n- Reviewed and merged by the maintainer"},
		{"an epic", NewIssue{Type: TypeEpic}, []string{"The plan is written down"}, 1, "epic", "- The plan is written down"},
		{"a chore", NewIssue{Type: TypeChore, Acceptance: "- The result is posted where the issue says"},
			[]string{"The result is posted where the issue says"}, 0, "chore", ""},
		{"a template", NewIssue{Template: true}, nil, 0, "", ""},
	} {
		tc.in.Title = tc.name
		c, err := s.Create(ctx, alice, tc.in)
		if err != nil {
			t.Fatalf("%s: Create: %v", tc.name, err)
		}
		if got := itemTexts(t, s, c.ID); !slices.Equal(got, tc.want) || c.Items != tc.items {
			t.Errorf("%s: items %q, %d added; want %q, %d", tc.name, got, c.Items, tc.want, tc.items)
		}
		if tc.acceptance != "" && c.Acceptance != tc.acceptance {
			t.Errorf("%s: acceptance text %q, want %q", tc.name, c.Acceptance, tc.acceptance)
		}
		stamp, ok := stampOf(t, s, c.ID, OpIssueCreate)
		if tc.profile == "" {
			if ok {
				t.Errorf("%s: a template's create records the workflow %+v", tc.name, stamp)
			}
			continue
		}
		if !ok || stamp != (WorkflowStamp{Rev: rev, Type: tc.profile}) {
			t.Errorf("%s: the create's workflow = %+v, %v; want rev %d, %s", tc.name, stamp, ok, rev, tc.profile)
		}
	}

	// A gate gets nothing, whatever the default.
	g := mustGate(t, s, alice, NewGate{Blocks: none.ID, Title: "approve", Approvers: []string{"carol"}})
	if got := itemTexts(t, s, g.ID); len(got) != 0 {
		t.Errorf("a gate's items = %q, want none", got)
	}
}

// The items an issue ends with are capped as its own are: a create whose
// own items and the defaults together pass the cap is refused, naming
// both counts.
func TestWorkflowDefaultsCapped(t *testing.T) {
	t.Parallel()
	s := openStore(t, newDSN(t), Options{Limits: Limits{AcceptanceItems: 4}})
	mustSetWorkflow(t, s, testWorkflow(t))
	if _, err := s.Create(t.Context(), alice, NewIssue{Title: "fits", Acceptance: "- one"}); err != nil {
		t.Errorf("Create with 1 + 3 items = %v, want it made", err)
	}
	_, err := s.Create(t.Context(), alice, NewIssue{Title: "too many", Acceptance: "- one\n- two"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "2 of its own and 3 from the workflow") {
		t.Errorf("Create with 2 + 3 items = %v, want ErrInvalid naming both counts", err)
	}
}

// A later set changes no issue already made, and the new rev applies to
// the next create.
func TestWorkflowLaterSetChangesNoIssue(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	mustSetWorkflow(t, s, testWorkflow(t))
	first := mustCreate(t, s, NewIssue{Title: "under rev 1", Type: TypeEpic})
	w := testWorkflow(t)
	w.Types["epic"] = proto.WorkflowProfile{Acceptance: []proto.WorkflowItem{{Text: "Children filed"}}}
	rev, _, err := s.SetWorkflow(ctx, danaAdminCLI, w, 1)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.GetIssue(ctx, first.ID)
	if err != nil || after.Rev != first.Rev || after.Acceptance != first.Acceptance {
		t.Errorf("the epic after the set = rev %d %q, %v; want rev %d %q, unchanged", after.Rev, after.Acceptance, err, first.Rev, first.Acceptance)
	}
	second, err := s.Create(ctx, alice, NewIssue{Title: "under rev 2", Type: TypeEpic})
	if err != nil || !slices.Equal(itemTexts(t, s, second.ID), []string{"Children filed"}) {
		t.Errorf("an epic under rev 2 = %+v, %v; want rev 2's item", second, err)
	}
	if stamp, _ := stampOf(t, s, second.ID, OpIssueCreate); stamp.Rev != rev {
		t.Errorf("the second epic's workflow rev = %d, want %d", stamp.Rev, rev)
	}
}

// An item the workflow added leaves the issue only ticked or waived, on
// the record: an update that drops it while open is refused.
func TestWorkflowItemLeavesOnlyByWaiver(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	mustSetWorkflow(t, s, testWorkflow(t))
	is := mustCreate(t, s, NewIssue{Title: "work", Acceptance: "- mine"})
	_, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Acceptance: ptr("- mine")})
	var unmet *AcceptanceError
	if !errors.As(err, &unmet) || !unmet.Dropped || !slices.Equal(unmet.Open, []int{2, 3, 4}) {
		t.Fatalf("an update dropping the defaults = %v, want an AcceptanceError (dropped) naming 2, 3 and 4", err)
	}
	if _, err := s.Accept(ctx, alice, is.ID, Acceptance{Waive: map[int]string{2: "no PR for this", 3: "docs only", 4: "not merged here"}}); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.GetIssue(ctx, is.ID)
	if _, err := s.UpdateIssue(ctx, alice, is.ID, cur.Rev, IssuePatch{Acceptance: ptr("- mine")}); err != nil {
		t.Errorf("an update dropping the waived defaults = %v, want it applied", err)
	}
}

// The discovered issues of a finish get their type's items, recorded as
// a create's are.
func TestWorkflowDefaultsAtFinish(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	parent := mustCreate(t, s, NewIssue{Title: "parent"})
	if _, _, err := s.StartIssue(ctx, alice, parent.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	rev := mustSetWorkflow(t, s, testWorkflow(t))
	_, ids, err := s.FinishIssue(ctx, alice, parent.ID, 0, Finish{Discovered: []NewIssue{
		{Title: "a follow-up"}, {Title: "a run", Type: TypeChore}}})
	if err != nil {
		t.Fatalf("FinishIssue: %v", err)
	}
	if got := itemTexts(t, s, ids[0]); len(got) != 3 {
		t.Errorf("the discovered task's items = %q, want the default's three", got)
	}
	if got := itemTexts(t, s, ids[1]); !slices.Equal(got, []string{"The result is posted where the issue says"}) {
		t.Errorf("the discovered chore's items = %q, want the chore's", got)
	}
	if stamp, ok := stampOf(t, s, ids[1], OpIssueCreate); !ok || stamp != (WorkflowStamp{Rev: rev, Type: "chore"}) {
		t.Errorf("the discovered chore's workflow = %+v, %v; want rev %d, chore", stamp, ok, rev)
	}
}

// A change of type adds, in the same write, the new profile's items the
// issue lacks, and keeps the old ones; the update's event names the
// workflow. A closed issue's change of type adds nothing.
func TestWorkflowTypeChange(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	rev := mustSetWorkflow(t, s, testWorkflow(t))
	is := mustCreate(t, s, NewIssue{Title: "grows into an epic"})
	up, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Type: ptr(TypeEpic), Title: ptr("an epic now")})
	if err != nil {
		t.Fatalf("UpdateIssue to epic: %v", err)
	}
	want := []string{"PR opened from the template", "Tests run as the tiers require", "Reviewed and merged by the maintainer", "The plan is written down"}
	if got := itemTexts(t, s, is.ID); !slices.Equal(got, want) || up.Rev != is.Rev+1 {
		t.Errorf("items after the change = %q at rev %d; want %q at rev %d", got, up.Rev, want, is.Rev+1)
	}
	if stamp, ok := stampOf(t, s, is.ID, OpIssueUpdate); !ok || stamp != (WorkflowStamp{Rev: rev, Type: "epic"}) {
		t.Errorf("the update's workflow = %+v, %v; want rev %d, epic", stamp, ok, rev)
	}
	// Back to a task: its items are all there already.
	back, err := s.UpdateIssue(ctx, alice, is.ID, up.Rev, IssuePatch{Type: ptr(TypeTask)})
	if err != nil || back.Acceptance != up.Acceptance {
		t.Errorf("UpdateIssue back to task = %q, %v; want the text unchanged", back.Acceptance, err)
	}
	// With new text in the same update, the profile's items follow it.
	other := mustCreate(t, s, NewIssue{Title: "another", Type: TypeChore})
	up, err = s.UpdateIssue(ctx, alice, other.ID, other.Rev, IssuePatch{Type: ptr(TypeEpic),
		Acceptance: ptr("- The result is posted where the issue says\n- new")})
	if err != nil {
		t.Fatal(err)
	}
	if got := itemTexts(t, s, other.ID); !slices.Equal(got, []string{"The result is posted where the issue says", "new", "The plan is written down"}) {
		t.Errorf("items after a change of type and text = %q", got)
	}
	// A closed issue takes none.
	closed := mustCreate(t, s, NewIssue{Title: "done", Type: TypeChore, Acceptance: "- [x] The result is posted where the issue says"})
	closed, err = s.CloseIssue(ctx, alice, closed.ID, closed.Rev, "done")
	if err != nil {
		t.Fatal(err)
	}
	if after, err := s.UpdateIssue(ctx, alice, closed.ID, closed.Rev, IssuePatch{Type: ptr(TypeBug)}); err != nil || after.Acceptance != closed.Acceptance {
		t.Errorf("a closed issue's change of type = %q, %v; want its text unchanged", after.Acceptance, err)
	}
}

// withItems appends what text lacks, as list entries, turning prose into
// one entry first.
func TestWithItems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		text  string
		items []string
		want  string
		n     int
	}{
		{"", []string{"a", "b"}, "- a\n- b", 2},
		{"- a", []string{"a", "b"}, "- a\n- b", 1},
		{"- a\n", []string{"b"}, "- a\n- b", 1},
		{"1. a\n2. b", nil, "1. a\n2. b", 0},
		{"Done when\nit works.", []string{"b"}, "- Done when it works.\n- b", 1},
		{"Intro\n- a\n\nOutro", []string{"a", "c"}, "Intro\n- a\n\nOutro\n- c", 1},
		{"- a", []string{"b", "b"}, "- a\n- b", 1},
	} {
		got, n := withItems(tc.text, tc.items)
		if got != tc.want || n != tc.n {
			t.Errorf("withItems(%q, %q) = %q, %d; want %q, %d", tc.text, tc.items, got, n, tc.want, tc.n)
		}
		var texts []string
		for _, p := range parseAcceptance(got) {
			texts = append(texts, p.text)
		}
		if want := parseAcceptance(tc.want); len(texts) != len(want) {
			t.Errorf("withItems(%q, %q) parses to %q", tc.text, tc.items, texts)
		}
	}
}
