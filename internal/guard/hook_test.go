package guard

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookEnv is a hook's surroundings: a repository with the starting
// policy, a data directory, and the session's environment.
type hookEnv struct {
	top, data string
	env       map[string]string
	policy    []byte
}

func newHookEnv(t *testing.T) *hookEnv {
	t.Helper()
	top, _ := repo(t)
	b, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(top, "internal", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &hookEnv{top: top, data: t.TempDir(), env: map[string]string{"GOTOOLCHAIN": "local", "GOFLAGS": "-mod=readonly"}, policy: b}
}

func (h *hookEnv) run(t *testing.T, in any) map[string]any {
	t.Helper()
	b, ok := in.([]byte)
	if !ok {
		var err error
		if b, err = json.Marshal(in); err != nil {
			t.Fatal(err)
		}
	}
	out := Hook{Stdin: bytes.NewReader(b), Getenv: func(k string) string { return h.env[k] }, DataDir: h.data,
		Now: func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }}.Run()
	if out == nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("hook output %q is not JSON: %v", out, err)
	}
	return doc
}

// input is Claude Code's PreToolUse input for a call (Hooks reference,
// "PreToolUse input").
func (h *hookEnv) input(session, tool string, toolInput map[string]any) map[string]any {
	return map[string]any{
		"session_id": session, "transcript_path": "/x/t.jsonl", "cwd": filepath.Join(h.top, "internal"),
		"permission_mode": "default", "hook_event_name": "PreToolUse",
		"tool_name": tool, "tool_input": toolInput, "tool_use_id": "toolu_01",
	}
}

func decision(doc map[string]any) (answer, reason string) {
	hso, _ := doc["hookSpecificOutput"].(map[string]any)
	if hso == nil {
		return "", ""
	}
	if hso["hookEventName"] != "PreToolUse" {
		return "bad event name", ""
	}
	a, _ := hso["permissionDecision"].(string)
	r, _ := hso["permissionDecisionReason"].(string)
	return a, r
}

func (h *hookEnv) snapshot(t *testing.T, session string, m Mode, policy []byte, present bool) string {
	t.Helper()
	p, err := SnapshotPath(h.data, session)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSnapshot(p, Snapshot(m, policy, present)); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHookDesktop(t *testing.T) {
	t.Parallel()
	h := newHookEnv(t)
	h.snapshot(t, "s1", Desktop, h.policy, true)
	for _, tc := range []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"Bash", map[string]any{"command": "go test ./...", "description": "x"}, "allow"},
		{"Bash", map[string]any{"command": "gh pr merge 1"}, "deny"},
		{"Bash", map[string]any{"command": "eval x"}, "ask"},
		{"Bash", map[string]any{"command": "curl example.com"}, ""},
		{"Edit", map[string]any{"file_path": filepath.Join(h.top, ".mcp.json"), "old_string": "a", "new_string": "b"}, "deny"},
		{"Write", map[string]any{"file_path": filepath.Join(h.top, "internal", "x", "y.go"), "content": "package x"}, ""},
		{"Read", map[string]any{"file_path": filepath.Join(h.top, ".mcp.json")}, ""},
	} {
		doc := h.run(t, h.input("s1", tc.tool, tc.input))
		if got, reason := decision(doc); got != tc.want {
			t.Errorf("%s %v: %q (%s); want %q", tc.tool, tc.input, got, reason, tc.want)
		}
		if doc != nil && doc["systemMessage"] != nil {
			t.Errorf("%s %v: a system message with a good snapshot: %v", tc.tool, tc.input, doc["systemMessage"])
		}
	}
}

// Headless: the snapshot ARIES_GUARD_POLICY names, marked headless,
// denies what it cannot read.
func TestHookHeadless(t *testing.T) {
	t.Parallel()
	h := newHookEnv(t)
	p := filepath.Join(t.TempDir(), "run.yaml")
	if err := WriteSnapshot(p, Snapshot(Headless, h.policy, true)); err != nil {
		t.Fatal(err)
	}
	h.env["ARIES_GUARD_POLICY"] = p
	if a, _ := decision(h.run(t, h.input("r1", "Bash", map[string]any{"command": "eval x"}))); a != "deny" {
		t.Errorf("eval x headless: %q; want deny", a)
	}
	if a, _ := decision(h.run(t, h.input("r1", "Bash", map[string]any{"command": "GOTOOLCHAIN=local go test ./..."}))); a != "allow" {
		t.Errorf("GOTOOLCHAIN=local go test headless: %q; want allow", a)
	}
	// The snapshot's own file cannot be written.
	if a, _ := decision(h.run(t, h.input("r1", "Write", map[string]any{"file_path": p}))); a != "deny" {
		t.Errorf("writing the run's snapshot: %q; want deny", a)
	}
}

// When the guard cannot read its policy or its input it fails open in a
// Desktop session, with the built-in denies and one line for the
// person, and closed in a headless run (docs/design/guard.md §6).
func TestHookFails(t *testing.T) {
	t.Parallel()
	h := newHookEnv(t)
	h.snapshot(t, "bad", Desktop, []byte("version: 1\nalow: []\n"), true)
	for _, tc := range []struct {
		name, session, cmd, want string
	}{
		{"no snapshot", "none", "go test ./...", ""},
		{"no snapshot", "none", "gh pr merge 1", "deny"},
		{"invalid policy", "bad", "go test ./...", ""},
		{"invalid policy", "bad", "git push --force", "deny"},
	} {
		doc := h.run(t, h.input(tc.session, "Bash", map[string]any{"command": tc.cmd}))
		if a, _ := decision(doc); a != tc.want {
			t.Errorf("%s, %q: %q; want %q", tc.name, tc.cmd, a, tc.want)
		}
		msg, _ := doc["systemMessage"].(string)
		if !strings.HasPrefix(msg, "aries guard: policy not applied: ") || strings.Contains(msg, "\n") || strings.Contains(msg, tc.cmd) {
			t.Errorf("%s: system message %q", tc.name, msg)
		}
	}
	if doc := h.run(t, h.input("bad", "Bash", map[string]any{"command": "x"})); !strings.Contains(doc["systemMessage"].(string), `line 2: unknown key "alow"`) {
		t.Errorf("invalid policy's message: %v", doc["systemMessage"])
	}
	if doc := h.run(t, []byte("not json")); doc == nil || doc["systemMessage"] == nil || doc["hookSpecificOutput"] != nil {
		t.Errorf("Desktop, input not JSON: %v", doc)
	}

	// Headless: every call is denied, with a fix.
	h.env["ARIES_GUARD_POLICY"] = filepath.Join(h.data, "missing.yaml")
	for _, in := range []any{
		h.input("r", "Bash", map[string]any{"command": "go test ./..."}),
		h.input("r", "Edit", map[string]any{"file_path": filepath.Join(h.top, "x.go")}),
		[]byte("not json"),
	} {
		a, reason := decision(h.run(t, in))
		if a != "deny" || !strings.Contains(reason, "\nfix: ") {
			t.Errorf("headless without its policy: %q, %q; want deny with a fix", a, reason)
		}
	}
}

// Outside a git repository the hook prints nothing.
func TestHookOutsideRepository(t *testing.T) {
	t.Parallel()
	h := newHookEnv(t)
	in := h.input("s", "Bash", map[string]any{"command": "gh pr merge 1"})
	in["cwd"] = t.TempDir()
	if doc := h.run(t, in); doc != nil {
		t.Errorf("outside a repository: %v", doc)
	}
}

// With no policy file on origin/main, the snapshot says so and the
// built-in denies apply, quietly.
func TestHookNoPolicyFile(t *testing.T) {
	t.Parallel()
	h := newHookEnv(t)
	h.snapshot(t, "s", Desktop, nil, false)
	doc := h.run(t, h.input("s", "Bash", map[string]any{"command": "gh api user"}))
	if a, _ := decision(doc); a != "deny" || doc["systemMessage"] != nil {
		t.Errorf("gh api with no policy file: %v", doc)
	}
	if doc := h.run(t, h.input("s", "Bash", map[string]any{"command": "go test ./..."})); doc != nil {
		t.Errorf("go test with no policy file: %v", doc)
	}
}

// Each denial is one line of the count, with the rule and the session
// and never the command; nothing else writes it.
func TestHookCountsDenials(t *testing.T) {
	t.Parallel()
	h := newHookEnv(t)
	h.snapshot(t, "s9", Desktop, h.policy, true)
	h.run(t, h.input("s9", "Bash", map[string]any{"command": "gh pr merge 4242"}))
	h.run(t, h.input("s9", "Bash", map[string]any{"command": "go test ./..."}))
	h.run(t, h.input("s9", "Bash", map[string]any{"command": "eval x"}))
	h.run(t, h.input("s9", "Edit", map[string]any{"file_path": filepath.Join(h.top, ".mcp.json")}))
	f := filepath.Join(h.data, "guard", "denials.jsonl")
	b, err := os.ReadFile(f) //nolint:gosec // the test's own temporary file
	if err != nil {
		t.Fatal(err)
	}
	want := `{"at":"2026-10-21T12:00:00Z","session":"s9","mode":"desktop","tool":"Bash","rule":"deny 1"}
{"at":"2026-10-21T12:00:00Z","session":"s9","mode":"desktop","tool":"Edit","rule":"deny 11"}
`
	if string(b) != want {
		t.Errorf("denials.jsonl:\n%s\nwant:\n%s", b, want)
	}
	if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("denials.jsonl's mode: %v, %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(f)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("guard directory's mode: %v, %v", fi, err)
	}
}

// The count is bounded: past its size, the file is moved aside and a
// new one begun.
func TestCountBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "guard", "denials.jsonl")
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, bytes.Repeat([]byte("x"), maxCount), 0o600); err != nil {
		t.Fatal(err)
	}
	count(dir, time.Now(), "s", Desktop, "Bash", "deny 1")
	if fi, err := os.Stat(f + ".1"); err != nil || fi.Size() != maxCount {
		t.Errorf("old count: %v, %v", fi, err)
	}
	if b, err := os.ReadFile(f); err != nil || bytes.Count(b, []byte("\n")) != 1 { //nolint:gosec // the test's own temporary file
		t.Errorf("new count: %q, %v", b, err)
	}
}

func TestSnapshot(t *testing.T) {
	t.Parallel()
	pol := []byte("version: 1\nallow:\n  - run: [go]\n")
	for _, tc := range []struct {
		m       Mode
		present bool
	}{{Desktop, true}, {Headless, true}, {Desktop, false}, {Headless, false}} {
		p, m, err := ReadSnapshot(Snapshot(tc.m, pol, tc.present))
		if err != nil || m != tc.m {
			t.Fatalf("%v %v: %v, %v", tc.m, tc.present, m, err)
		}
		if (p.Source == File) != tc.present {
			t.Errorf("%v %v: source %q", tc.m, tc.present, p.Source)
		}
	}
	for _, bad := range []string{"", "version: 1\n", "# aries guard snapshot 2 mode=desktop policy=present\n", "# aries guard snapshot 1 mode=x policy=present\n"} {
		if _, _, err := ReadSnapshot([]byte(bad)); err == nil {
			t.Errorf("ReadSnapshot(%q) read", bad)
		}
	}
	// A policy's error names its line in the file, not in the snapshot.
	if _, _, err := ReadSnapshot(Snapshot(Desktop, []byte("version: 1\nalow: []\n"), true)); err == nil || !strings.Contains(err.Error(), File+": line 2:") {
		t.Errorf("error: %v", err)
	}
	for _, s := range []string{"", ".", "..", "a/b", `a\b`, strings.Repeat("a", 129), ".hidden"} {
		if _, err := SnapshotPath("/d", s); err == nil {
			t.Errorf("SnapshotPath(%q) accepted", s)
		}
	}
	if p, err := SnapshotPath("/d", "8d6f-ab_1.x"); err != nil || p != filepath.Join("/d", "guard", "8d6f-ab_1.x.yaml") {
		t.Errorf("SnapshotPath: %q, %v", p, err)
	}
}
