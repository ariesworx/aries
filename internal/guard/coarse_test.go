package guard

import (
	"slices"
	"testing"
)

// The coarse copy holds the built-in denies first, then the file's, each
// rule once, in Claude Code's form; a path rule is an Edit rule alone.
func TestCoarse(t *testing.T) {
	t.Parallel()
	p, err := Load([]byte(`version: 1
deny:
  - run: gh pr merge
    fix: "hand off"
  - run: find
    flags: [-exec]
    fix: "list, then act"
  - run: git
    config: [alias.*]
    fix: "write it out"
  - path: [.mcp.json, .claude/**]
    fix: "the maintainer's"
`))
	if err != nil {
		t.Fatal(err)
	}
	got := Coarse(p)
	want := []string{
		"Bash(gh pr merge *)", "Bash(gh api *)",
		"Bash(git push --force*)", "Bash(git push * --force*)", "Bash(git push * +*)",
		"Bash(git push -f*)", "Bash(git push * -f*)",
		"Bash(git push --force-with-lease*)", "Bash(git push * --force-with-lease*)",
		"Bash(git push --force-if-includes*)", "Bash(git push * --force-if-includes*)",
		"Bash(git push --mirror*)", "Bash(git push * --mirror*)",
		"Bash(git push --delete*)", "Bash(git push * --delete*)",
		"Bash(git push -d*)", "Bash(git push * -d*)",
		"Bash(git push * main)", "Bash(git push * main *)", "Bash(git push *:main)", "Bash(git push *:main *)",
		"Bash(git push *:refs/heads/main)", "Bash(git push *:refs/heads/main *)",
		"Bash(find -exec*)", "Bash(find * -exec*)",
		"Bash(git -c alias.*)", "Bash(git * -c alias.*)",
		"Edit(.mcp.json)", "Edit(.claude/**)",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Coarse =\n%q\nwant\n%q", got, want)
	}
	if b := Coarse(BuiltinPolicy()); !slices.Equal(b, want[:23]) {
		t.Errorf("Coarse(builtin) = %q; want the built-in rules alone", b)
	}
}

// The allow list is the tools, then each run: rule as a Bash rule.
func TestAllowed(t *testing.T) {
	t.Parallel()
	p, err := Load([]byte("version: 1\nallow:\n  - tools: [Read, mcp__aries]\n  - run: [git, gh pr view, scripts/gate.sh]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Read", "mcp__aries", "Bash(git *)", "Bash(gh pr view *)", "Bash(scripts/gate.sh *)"}
	if got := Allowed(p); !slices.Equal(got, want) {
		t.Errorf("Allowed = %q; want %q", got, want)
	}
}
