package agentsetup

import (
	"slices"
	"strings"
	"testing"
)

// guardOf is the agent's guard target in the project.
func guardOf(t *testing.T, agent string) Target {
	t.Helper()
	for _, tg := range Agents[agent].Targets(false) {
		if tg.Kind == KindGuard {
			return tg
		}
	}
	t.Fatalf("%s has no guard target", agent)
	return Target{}
}

// Claude Code's project settings get the guard as a PreToolUse hook and
// the coarse copy of the policy's denies in permissions.deny, beside the
// person's own rules and hooks, which are kept (docs/design/guard.md §8).
// Applying twice changes nothing; removing takes out what setup wrote
// and nothing else.
func TestGuard(t *testing.T) {
	t.Parallel()
	e := DefaultEntry
	e.Deny = []string{"Bash(gh pr merge *)", "Edit(.mcp.json)"}
	g := guardOf(t, "claude-code")
	in := `{"permissions": {"allow": ["Bash(make *)"], "deny": ["Read(./.env)", "Edit(.mcp.json)"]},
  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "lint"}]}]}}`
	want := `{
  "permissions": {
    "allow": [
      "Bash(make *)"
    ],
    "deny": [
      "Read(./.env)",
      "Edit(.mcp.json)",
      "Bash(gh pr merge *)"
    ]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "lint"
          }
        ]
      },
      {
        "matcher": "Bash|Edit|Write|NotebookEdit",
        "hooks": [
          {
            "type": "command",
            "command": "aries guard --hook",
            "timeout": 5
          }
        ]
      }
    ]
  }
}
`
	if g.Registered([]byte(in), e) {
		t.Fatal("Registered before Apply")
	}
	if got := g.MissingHooks([]byte(in), e); !slices.Equal(got, []string{"PreToolUse"}) {
		t.Errorf("MissingHooks = %v; want [PreToolUse]", got)
	}
	if got := g.MissingDeny([]byte(in), e); !slices.Equal(got, []string{"Bash(gh pr merge *)"}) {
		t.Errorf("MissingDeny = %v", got)
	}
	out, res, err := g.Apply([]byte(in), e)
	if err != nil || res != Added || string(out) != want {
		t.Fatalf("apply: %s %v\n%s\nwant\n%s", res, err, out, want)
	}
	if again, res, err := g.Apply(out, e); err != nil || res != Unchanged || string(again) != string(out) {
		t.Fatalf("second apply: %s %v\n%s", res, err, again)
	}
	if !g.Registered(out, e) || len(g.MissingHooks(out, e)) != 0 || len(g.MissingDeny(out, e)) != 0 {
		t.Fatal("not registered after Apply")
	}

	// A rule the policy gained is out of step until Apply adds it.
	more := e
	more.Deny = append(slices.Clone(e.Deny), "Bash(gh api *)")
	if g.Registered(out, more) {
		t.Error("registered though a deny rule is missing")
	}
	if up, res, err := g.Apply(out, more); err != nil || res != Updated || !strings.Contains(string(up), `"Bash(gh api *)"`) {
		t.Errorf("apply of a new rule: %s %v\n%s", res, err, up)
	}

	rm, res, err := g.Remove(out, e)
	if err != nil || res != Removed {
		t.Fatalf("remove: %s %v", res, err)
	}
	for _, gone := range []string{"guard --hook", "Bash(gh pr merge *)", "Edit(.mcp.json)"} {
		if strings.Contains(string(rm), gone) {
			t.Errorf("remove left %s:\n%s", gone, rm)
		}
	}
	for _, kept := range []string{"Read(./.env)", "Bash(make *)", `"lint"`} {
		if !strings.Contains(string(rm), kept) {
			t.Errorf("remove took the person's %s:\n%s", kept, rm)
		}
	}
	if again, res, _ := g.Remove(rm, e); res != Unchanged || string(again) != string(rm) {
		t.Errorf("second remove: %s", res)
	}
}

// The guard's hook is Claude Code's alone, and the project's: no other
// harness, and no home directory, gets one.
func TestGuardIsClaudeCodeProjects(t *testing.T) {
	t.Parallel()
	for _, name := range Names() {
		for _, global := range []bool{false, true} {
			for _, tg := range Agents[name].Targets(global) {
				if tg.Kind == KindGuard && (name != "claude-code" || global) {
					t.Errorf("%s global=%v has a guard target", name, global)
				}
			}
		}
	}
	snip := guardOf(t, "claude-code").Snippet(Entry{Command: "/opt/star fix/aries", Deny: []string{"Bash(gh api *)"}})
	for _, want := range []string{`"command": "'/opt/star fix/aries' guard --hook"`, `"Bash(gh api *)"`, `"PreToolUse"`} {
		if !strings.Contains(snip, want) {
			t.Errorf("snippet lacks %s:\n%s", want, snip)
		}
	}
}
