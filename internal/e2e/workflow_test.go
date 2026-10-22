package e2e

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// e2eWorkflow is a project's .aries/workflow.yaml, as design §21.3 has
// it, with dana for the maintainer.
const e2eWorkflow = `version: 1
ci: [dana]
types:
  default:
    acceptance:
      - PR opened from the template
      - Tests run as the tiers require
      - text: Reviewed and merged by the maintainer
        check: landed
    checks: [landed]
  epic:
    approve: {approvers: [dana]}
  chore:
    acceptance: [The result is posted where the issue says]
`

// writeWorkflow writes body as u's .aries/workflow.yaml and returns its
// path.
func (u *user) writeWorkflow(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(u.repo, ".aries", "workflow.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// itemsOf is the texts of an issue's acceptance items, as `aries show
// --json` lists them.
func itemsOf(u *user, id string) []string {
	u.w.t.Helper()
	var out []string
	for _, it := range decode[proto.ShowResult](u.w.t, u.ok("show", id, "--json")).Items {
		out = append(out, it.Text)
	}
	return out
}

// Slice W0's gate (design §21.11): a member's set is refused forbidden,
// and so is an admin's from an agent's session, and no MCP tool can try;
// an admin's set at her terminal applies; an agent's create through MCP,
// and a CLI's, which sends nothing new, get the
// profile's items; a later set changes no existing issue; an agent
// cannot remove an item except by waiving it; and `aries workflow
// check` tells the file from the server's.
func TestWorkflow(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	dana, alice := w.newUser("dana", ""), w.newUser("alice", "")
	file := dana.writeWorkflow(t, e2eWorkflow)
	aliceFile := alice.writeWorkflow(t, e2eWorkflow)

	// Before any set: check fails, show says there is none.
	if r := alice.run("v0.2.0", "workflow", "check", aliceFile); r.code != 1 || !strings.Contains(r.stdout, "has no workflow") {
		t.Errorf("workflow check before a set = exit %d, %q %q; want 1, saying the server has none", r.code, r.stdout, r.stderr)
	}
	if out := alice.ok("workflow", "show"); !strings.Contains(out, "no workflow") {
		t.Errorf("workflow show before a set = %q", out)
	}

	// A member is refused; so is the admin from an agent's session.
	r := alice.run("v0.2.0", "--json", "admin", "workflow", "set", aliceFile)
	if e := cliError(t, r.stdout); r.code != 1 || e.Code != proto.CodeForbidden || !strings.Contains(e.Fix, "ask a project admin") {
		t.Errorf("a member's set = exit %d, %+v; want forbidden, asking an admin", r.code, e)
	}
	dana.env["CLAUDE_CODE_SESSION_ID"] = "cc-dana"
	r = dana.run("v0.2.0", "--json", "admin", "workflow", "set", file)
	if e := cliError(t, r.stdout); r.code != 1 || e.Code != proto.CodeForbidden || !strings.Contains(e.Fix, "own terminal") {
		t.Errorf("the admin's set from an agent's session = exit %d, %+v; want forbidden, saying her own terminal", r.code, e)
	}
	delete(dana.env, "CLAUDE_CODE_SESSION_ID")
	// No tool sets, reads or bypasses it.
	ag := alice.mcp("v0.2.0")
	tools, err := ag.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "workflow") || strings.Contains(tool.Name, "setting") {
			t.Errorf("MCP tool %q reaches the workflow", tool.Name)
		}
	}

	// The admin at her terminal.
	if out := dana.ok("workflow", "set", file); !strings.Contains(out, "workflow rev 1 set") {
		t.Errorf("workflow set = %q, want rev 1", out)
	}
	if out := dana.ok("admin", "workflow", "set", file, "--rev", "1"); !strings.Contains(out, "unchanged at rev 1") {
		t.Errorf("workflow set again = %q, want unchanged", out)
	}
	if out := alice.ok("workflow", "check", aliceFile); !strings.Contains(out, "matches the server's workflow (rev 1)") {
		t.Errorf("workflow check after the set = %q", out)
	}
	show := alice.ok("workflow", "show")
	for _, want := range []string{"workflow rev 1: bug, feature, task: 3 items, landed; epic: approval; chore: 1 item", "set by dana", "check: landed"} {
		if !strings.Contains(show, want) {
			t.Errorf("workflow show lacks %q:\n%s", want, show)
		}
	}

	// A CLI's create: its own item, then the defaults, said on stderr.
	r = alice.run("v0.2.0", "create", "Fix the parser", "-t", "bug", "--acceptance", "- A test reproduces it")
	id := strings.TrimSpace(r.stdout)
	if r.code != 0 || !strings.Contains(r.stderr, "the project's workflow added 3 acceptance items") {
		t.Fatalf("create = exit %d, %q %q; want the id, and the items on stderr", r.code, r.stdout, r.stderr)
	}
	want := []string{"A test reproduces it", "PR opened from the template", "Tests run as the tiers require", "Reviewed and merged by the maintainer"}
	if got := itemsOf(alice, id); !slices.Equal(got, want) {
		t.Errorf("the bug's items = %q, want %q", got, want)
	}

	// An agent's create through MCP gets them, and prime says the
	// workflow in its line.
	p := decode[mcpserver.Prime](t, ag.ok("prime", nil))
	if !strings.HasPrefix(p.Workflow, "workflow rev 1: ") {
		t.Errorf("prime's workflow = %q", p.Workflow)
	}
	made := decode[proto.WriteResult](t, ag.ok("create", map[string]any{"title": "Post the run", "type": "chore"}))
	if got := itemsOf(alice, made.ID); !slices.Equal(got, []string{"The result is posted where the issue says"}) {
		t.Errorf("the agent's chore's items = %q", got)
	}
	task := decode[proto.WriteResult](t, ag.ok("create", map[string]any{"title": "Refactor", "acceptance": "- Same behavior"}))
	if got := itemsOf(alice, task.ID); len(got) != 4 || got[0] != "Same behavior" {
		t.Errorf("the agent's task's items = %q, want its own and the default's three", got)
	}

	// The agent cannot drop a default item, only waive it.
	if out, isErr := ag.call("update", map[string]any{"id": task.ID, "acceptance": "- Same behavior"}); !isErr || !strings.Contains(out, "2, 3, 4") {
		t.Errorf("an update dropping the defaults = %q (error %v), want a refusal naming items 2, 3 and 4", out, isErr)
	}

	// A later set changes no issue made, and the next create takes it.
	before := decode[proto.ShowResult](t, alice.ok("show", id, "--json"))
	dana.writeWorkflow(t, strings.Replace(e2eWorkflow, "      - Tests run as the tiers require\n", "", 1))
	if diff := dana.ok("workflow", "diff", file); !strings.Contains(diff, "types.default.acceptance: - Tests run as the tiers require") {
		t.Errorf("workflow diff = %q", diff)
	}
	if r := alice.run("v0.2.0", "workflow", "check", aliceFile); r.code != 0 {
		t.Errorf("alice's file still matches rev 1: exit %d %q", r.code, r.stdout)
	}
	if out := dana.ok("admin", "workflow", "set", file, "--rev", "1"); !strings.Contains(out, "workflow rev 2 set") {
		t.Errorf("the second set = %q", out)
	}
	if r := alice.run("v0.2.0", "workflow", "check", aliceFile); r.code != 1 || !strings.Contains(r.stdout, "types.default.acceptance: + Tests run as the tiers require") {
		t.Errorf("workflow check of the old file = exit %d, %q; want 1, naming the item", r.code, r.stdout)
	}
	after := decode[proto.ShowResult](t, alice.ok("show", id, "--json"))
	if after.Issue.Rev != before.Issue.Rev || after.Issue.Acceptance != before.Issue.Acceptance {
		t.Errorf("the bug after the second set = rev %d %q, want rev %d unchanged", after.Issue.Rev, after.Issue.Acceptance, before.Issue.Rev)
	}
	if got := itemsOf(alice, strings.TrimSpace(alice.ok("create", "Another bug", "-t", "bug"))); len(got) != 2 {
		t.Errorf("a bug under rev 2 has items %q, want rev 2's two", got)
	}
	// A stale rev is refused, saying how to read the new one.
	r = dana.run("v0.2.0", "--json", "admin", "workflow", "set", file, "--rev", "1")
	if e := cliError(t, r.stdout); r.code != 1 || e.Code != proto.CodeConflict || !strings.Contains(e.Fix, "workflow show") {
		t.Errorf("a set at a stale rev = exit %d, %+v; want conflict", r.code, e)
	}
}
