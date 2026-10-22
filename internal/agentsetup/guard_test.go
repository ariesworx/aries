package agentsetup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
            "command": ` + string(mustJSON("aries guard --hook || "+guardFallback)) + `,
            "timeout": 5,
            "onFailure": "continue"
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
	for _, want := range []string{`"command": "'/opt/star fix/aries' guard --hook || echo `, `"Bash(gh api *)"`, `"PreToolUse"`} {
		if !strings.Contains(snip, want) {
			t.Errorf("snippet lacks %s:\n%s", want, snip)
		}
	}
}

// guardHookOf is the one guard hook in settings: its command and its
// onFailure.
func guardHookOf(t *testing.T, settings []byte) (command, onFailure string) {
	t.Helper()
	var doc struct {
		Hooks struct {
			PreToolUse []struct {
				Hooks []struct {
					Command   string `json:"command"`
					OnFailure string `json:"onFailure"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settings, &doc); err != nil {
		t.Fatalf("settings: %v\n%s", err, settings)
	}
	if pre := doc.Hooks.PreToolUse; len(pre) != 1 || len(pre[0].Hooks) != 1 {
		t.Fatalf("settings hold %d PreToolUse groups, want one with one hook:\n%s", len(pre), settings)
	}
	h := doc.Hooks.PreToolUse[0].Hooks[0]
	return h.Command, h.OnFailure
}

// The guard's hook, run as Claude Code runs a shell-form hook (`sh -c`),
// never blocks a call when the aries on PATH cannot run the guard: one
// that predates `guard` exits 2, which Claude Code reads as "block", and
// the hook instead exits 0 with a systemMessage, which Claude Code shows
// the person, and no decision, so the call goes on through Claude Code's
// own rules (docs/design/guard.md §6). An aries that runs the guard is
// passed through as it answered.
func TestGuardHookFailsOpen(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("stands in for aries with POSIX shell scripts")
	}
	deny := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}`
	tests := []struct {
		name string
		// script is the aries on PATH, as a shell script's body; "" puts
		// none there.
		script string
		// want is the hook's stdout; "" with note wants the note.
		want string
		note bool
	}{
		{name: "predates guard", note: true,
			script: `echo 'aries: unknown command "guard"' >&2; echo 'fix: run ` + "`aries help`" + ` for the commands' >&2; exit 2`},
		{name: "not on PATH", note: true},
		{name: "crashes", note: true, script: `echo 'panic: runtime error' >&2; exit 2`},
		{name: "denies", script: `printf '%s\n' '` + deny + `'`, want: deny + "\n"},
		{name: "no decision", script: `exit 0`},
	}
	settings, _, err := applyGuard(nil, DefaultEntry)
	if err != nil {
		t.Fatal(err)
	}
	command, onFailure := guardHookOf(t, settings)
	if onFailure != "continue" {
		t.Errorf("the guard's hook has onFailure %q, want continue: a hook that fails must not block a Desktop session", onFailure)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := t.TempDir()
			if tc.script != "" {
				if err := os.WriteFile(filepath.Join(bin, "aries"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0o700); err != nil { //nolint:gosec // a stand-in the test runs
					t.Fatal(err)
				}
			}
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command) //nolint:gosec // the command setup writes, run as Claude Code runs it
			cmd.Env = []string{"PATH=" + bin}
			cmd.Stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("sh -c %q with %s: %v; want exit 0, which Claude Code does not read as a block\nstderr: %s", command, tc.name, err, stderr.String())
			}
			if !tc.note {
				if got := stdout.String(); got != tc.want {
					t.Errorf("sh -c %q printed %q, want %q", command, got, tc.want)
				}
				return
			}
			var out map[string]any
			if err := json.Unmarshal([]byte(stdout.String()), &out); err != nil {
				t.Fatalf("sh -c %q printed %q: %v; want one JSON object", command, stdout.String(), err)
			}
			msg, _ := out["systemMessage"].(string)
			if len(out) != 1 || !strings.Contains(msg, "aries guard did not run") || !strings.Contains(msg, "fix: ") || strings.Contains(msg, "\n") {
				t.Errorf("sh -c %q printed %s; want only a one-line systemMessage saying the guard did not run, with a fix", command, stdout.String())
			}
		})
	}
}

// An install from before the hook's fallback, or with another wording of
// it, is updated in place: the command becomes setup's, onFailure is
// added where the person has none, and the person's other keys stay.
func TestGuardHookUpgrade(t *testing.T) {
	t.Parallel()
	g := guardOf(t, "claude-code")
	want, _, err := applyGuard(nil, DefaultEntry)
	if err != nil {
		t.Fatal(err)
	}
	wantCmd, _ := guardHookOf(t, want)
	hook := func(cmd, extra string) string {
		b, _ := json.Marshal(cmd)
		return `{"hooks": {"PreToolUse": [{"matcher": "Bash|Edit|Write|NotebookEdit", "hooks": [{"type": "command", "command": ` +
			string(b) + `, "timeout": 9` + extra + `}]}]}}`
	}
	tests := []struct {
		name, in, onFailure string
	}{
		{name: "before the fallback", in: hook("aries guard --hook", ""), onFailure: "continue"},
		{name: "another wording", in: hook(`aries guard --hook || echo '{"systemMessage":"older words"}'`, ""), onFailure: "continue"},
		{name: "the person's onFailure", in: hook("aries guard --hook", `, "onFailure": "block"`), onFailure: "block"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, res, err := g.Apply([]byte(tc.in), DefaultEntry)
			if err != nil || res != Updated {
				t.Fatalf("Apply(%s) = %s, %v; want updated", tc.in, res, err)
			}
			cmd, onFailure := guardHookOf(t, out)
			if cmd != wantCmd || onFailure != tc.onFailure || !strings.Contains(string(out), `"timeout": 9`) {
				t.Errorf("Apply(%s) =\n%s\nwant command %q, onFailure %q and timeout 9", tc.in, out, wantCmd, tc.onFailure)
			}
			if !g.Registered(out, DefaultEntry) {
				t.Errorf("not registered after Apply:\n%s", out)
			}
		})
	}
}
