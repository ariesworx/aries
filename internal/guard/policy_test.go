package guard

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	t.Parallel()
	p, err := Load([]byte(`version: 1
env:
  GOFLAGS: {fix: "drop it"}
  ARIES_E2E_TRANSPORT: {any: true}
wrappers: [scripts/leftovers.sh]
options:
  git: [-C, -c]
allow:
  - tools: [Read, Edit]
  - run: [git, "gh pr create"]
deny:
  - run: gh pr merge
    fix: "hand off"
  - run: git push
    flags: [--force, -f]
    fix: "no force"
  - run: git push
    refs: [main]
    fix: "not main"
  - run: git
    config: [alias.*]
    fix: "no aliases"
  - path: [.claude/**, .mcp.json]
    fix: "the maintainer's"
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Source != File {
		t.Errorf("Source = %q", p.Source)
	}
	if !slices.Equal(p.Tools, []string{"Read", "Edit"}) || len(p.Allow) != 2 || !slices.Equal(p.Allow[1], []string{"gh", "pr", "create"}) {
		t.Errorf("allow: tools %q, run %q", p.Tools, p.Allow)
	}
	if len(p.Deny) != 5 || p.Deny[0].N != 1 || !slices.Equal(p.Deny[0].Words, []string{"gh", "pr", "merge"}) || p.Deny[4].N != 5 {
		t.Errorf("deny = %+v", p.Deny)
	}
	if !p.Env["ARIES_E2E_TRANSPORT"].Any || p.Env["GOFLAGS"].Fix != "drop it" {
		t.Errorf("env = %+v", p.Env)
	}
}

// Every mistake in the file is refused with where it is, in one line.
func TestLoadRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ doc, want string }{
		{"", "no version"},
		{"version: 2\n", "version 2"},
		{"version: 1\nalow: []\n", `line 2: unknown key "alow"`},
		{"version: 1\ndeny:\n  - run: gh api\n", "deny 1: no fix"},
		{"version: 1\ndeny:\n  - run: gh api\n    fix: x\n    path: [a]\n", "deny 1: either run or path"},
		{"version: 1\ndeny:\n  - fix: x\n", "deny 1: either run or path"},
		{"version: 1\ndeny:\n  - path: [a]\n    flags: [-f]\n    fix: x\n", "deny 1: flags, refs and config need run"},
		{"version: 1\ndeny:\n  - run: gh\n    refs: [main]\n    fix: x\n", "deny 1: refs are for git push"},
		{"version: 1\ndeny:\n  - run: gh\n    config: [x]\n    fix: x\n", "deny 1: config is for git"},
		{"version: 1\ndeny:\n  - run: ' '\n    fix: x\n", "deny 1: run is empty"},
		{"version: 1\nallow:\n  - tools: [a]\n    run: [b]\n", "allow 1: either tools or run"},
		{"version: 1\nallow:\n  - run: ['']\n", "allow 1: an empty run"},
		{"version: 1\nenv:\n  X: {}\n", "env X: either fix or any"},
		{"version: 1\nenv:\n  X: {fix: a, any: true}\n", "env X: either fix or any"},
		{"version: 1\nwrappers: ['']\n", "wrappers: an empty name"},
		{"version: 1\n\tbad", "line 2"},
	} {
		_, err := Load([]byte(tc.doc))
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "\n") {
			t.Errorf("Load(%q) = %v; want one line holding %q", tc.doc, err, tc.want)
		}
	}
}

// The repository's own policy loads, and holds what the dispatcher's
// lists held (docs/design/guard.md §4).
func TestStartingFile(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("..", "..", File))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load(b)
	if err != nil {
		t.Fatalf("%s: %v", File, err)
	}
	for _, tool := range []string{"Read", "Edit", "Write", "Glob", "Grep", "TodoWrite", "mcp__aries"} {
		if !slices.Contains(p.Tools, tool) {
			t.Errorf("%s: tools lack %s", File, tool)
		}
	}
}

func TestMatchPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{".claude/**", ".claude/settings.json", true},
		{".claude/**", ".claude/agents/x.md", true},
		{".claude/**", "x/.claude/settings.json", false},
		{".claude/", ".claude/settings.json", true},
		{".mcp.json", ".mcp.json", true},
		{".mcp.json", "sub/.mcp.json", true},
		{".mcp.json", ".mcp.json.bak", false},
		{"/.mcp.json", "sub/.mcp.json", false},
		{"internal/release/keys.go", "internal/release/keys.go", true},
		{"internal/release/keys.go", "x/internal/release/keys.go", false},
		{"**/keys.go", "a/b/keys.go", true},
		{"*.pem", "a/b/x.pem", true},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/b/c", false},
		{"a/**/c", "a/c", true},
	} {
		if got := matchPath(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchPath(%q, %q) = %v; want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}
