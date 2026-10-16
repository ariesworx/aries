package depcheck

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// workflows returns the repository's workflow files, named from its root.
func workflows(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{"*.yml", "*.yaml"} {
		m, err := filepath.Glob(filepath.Join(root, ".github", "workflows", pattern))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m {
			rel, err := filepath.Rel(root, f)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, filepath.ToSlash(rel))
		}
	}
	if len(files) == 0 {
		t.Fatal("no workflow in .github/workflows")
	}
	return files
}

// usesLine matches a step's or a job's uses: key, and what follows it.
var usesLine = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(.*)$`)

var (
	commitPin  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	versionTag = regexp.MustCompile(`^#\s*v[0-9]+(\.[0-9]+)*\b`)
)

// checkUses holds the value of one uses: key to deps.allow and to a
// commit pin, and returns the Action's owner/repo. A local workflow or
// action (./…) is this commit's own code, and is exempt: it returns "".
// A subpath (actions/cache/restore) is the same repository at the same
// commit, and is held to the repository's line.
func checkUses(value string, allow allowlist) (string, error) {
	ref, comment, _ := strings.Cut(value, "#")
	ref = strings.Trim(strings.TrimSpace(ref), `"'`)
	switch {
	case strings.HasPrefix(ref, "./"):
		return "", nil
	case strings.HasPrefix(ref, "docker://"):
		return "", fmt.Errorf("%s is a container image, which deps.allow cannot list; use an Action, or ask the maintainer", ref)
	}
	path, commit, ok := strings.Cut(ref, "@")
	if !ok {
		return "", fmt.Errorf("%s is not pinned; pin it to a 40-character commit with its version in a comment", ref)
	}
	parts := strings.SplitN(path, "/", 3)
	if len(parts) < 2 {
		return "", fmt.Errorf("%s does not name an owner/repo", ref)
	}
	repo := parts[0] + "/" + parts[1]
	if e, ok := allow[repo]; !ok {
		return "", fmt.Errorf("%s is not in deps.allow; a person adds its line, scope actions (AGENTS.md, rule 4)", repo)
	} else if e.scope != scopeActions {
		return "", fmt.Errorf("%s is not in deps.allow at scope actions; line %d scopes it %s", repo, e.line, e.scope)
	}
	if !commitPin.MatchString(commit) {
		return "", fmt.Errorf("%s is pinned to %q, not to a 40-character commit", repo, commit)
	}
	if !versionTag.MatchString("#" + comment) {
		return "", fmt.Errorf("%s has no version in a comment after its commit (# vX.Y.Z)", repo)
	}
	return repo, nil
}

// TestActionsAreAllowed fails on a uses: in a workflow that deps.allow
// does not list, that is not pinned to a 40-character commit, or whose
// version is not in a comment beside it; and on an Action deps.allow
// lists that no workflow uses.
func TestActionsAreAllowed(t *testing.T) {
	t.Parallel()
	allow := readAllow(t)
	used := map[string]bool{}
	for _, file := range workflows(t) {
		n := 0
		for line := range strings.Lines(readFile(t, file)) {
			n++
			m := usesLine.FindStringSubmatch(strings.TrimRight(line, "\r\n"))
			if m == nil {
				continue
			}
			repo, err := checkUses(m[1], allow)
			if err != nil {
				t.Errorf("%s:%d: %v", file, n, err)
			}
			used[repo] = true
		}
	}
	for _, e := range allow.sorted() {
		if e.scope == scopeActions && !used[e.path] {
			t.Errorf("deps.allow:%d lists %s, which no workflow uses; remove the line", e.line, e.path)
		}
	}
}

func TestCheckUses(t *testing.T) {
	t.Parallel()
	allow, err := parseAllow(`
owner/action  actions  MIT  2026-10-16  an Action
example.com/lib  both  MIT  2026-10-16  a module
`)
	if err != nil {
		t.Fatal(err)
	}
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	tests := []struct {
		name, value, repo, want string // want: in the error; "": no error
	}{
		{name: "pinned", value: "owner/action@" + sha + " # v7.0.1", repo: "owner/action"},
		{name: "a subpath", value: "owner/action/restore@" + sha + " # v6", repo: "owner/action"},
		{name: "quoted", value: `"owner/action@` + sha + `" # v1.2`, repo: "owner/action"},
		{name: "local", value: "./.github/workflows/ci.yml", repo: ""},
		{name: "not listed", value: "other/action@" + sha + " # v1", want: "other/action is not in deps.allow"},
		{name: "a module's line", value: "example.com/lib@" + sha + " # v1", want: "example.com/lib is not in deps.allow at scope actions"},
		{name: "a tag", value: "owner/action@v7 # v7", want: `pinned to "v7", not to a 40-character commit`},
		{name: "a short commit", value: "owner/action@3d3c42e # v7", want: "not to a 40-character commit"},
		{name: "no ref", value: "owner/action", want: "is not pinned"},
		{name: "no version comment", value: "owner/action@" + sha, want: "no version in a comment"},
		{name: "a comment that is not a version", value: "owner/action@" + sha + " # latest", want: "no version in a comment"},
		{name: "a container", value: "docker://example.com/image:1", want: "a container image"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, err := checkUses(tc.value, allow)
			switch {
			case tc.want == "" && (err != nil || repo != tc.repo):
				t.Errorf("checkUses(%q) = %q, %v, want %q, nil", tc.value, repo, err, tc.repo)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("checkUses(%q) = %q, %v, want an error containing %q", tc.value, repo, err, tc.want)
			}
		})
	}
}
