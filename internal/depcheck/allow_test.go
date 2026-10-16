package depcheck

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// root is the repository's root, from this package's directory.
const root = "../.."

// The scopes of deps.allow: which may link or run what a line names.
const (
	scopeAries   = "aries"
	scopeAriesd  = "ariesd"
	scopeBoth    = "both"
	scopeTools   = "tools"
	scopeActions = "actions"
)

// linkable are the licenses the AGPL combines with (decision 82): what
// aries or ariesd links carries only these. A tool or an Action is run,
// never linked, and may carry another.
var linkable = []string{"Apache-2.0", "BSD-2-Clause", "BSD-3-Clause", "ISC", "MIT", "MPL-2.0"}

// entry is one line of deps.allow.
type entry struct {
	path, scope, reason string
	licenses            []string
	since               time.Time
	line                int
}

// linked reports whether the entry's scope is a binary's.
func (e entry) linked() bool {
	return e.scope == scopeAries || e.scope == scopeAriesd || e.scope == scopeBoth
}

// permits reports whether the entry may be linked into the binary bin.
func (e entry) permits(bin string) bool {
	return e.scope == scopeBoth || e.scope == bin
}

// allowlist is deps.allow, by path.
type allowlist map[string]entry

// sorted returns the entries in the order of their lines.
func (a allowlist) sorted() []entry {
	return slices.SortedFunc(maps.Values(a), func(x, y entry) int { return cmp.Compare(x.line, y.line) })
}

// parseAllow reads deps.allow's text. It returns every bad line at
// once, each named by its number.
func parseAllow(text string) (allowlist, error) {
	allow := allowlist{}
	var errs []error
	n := 0
	for line := range strings.Lines(text) {
		n++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		e, err := parseEntry(line)
		if err != nil {
			errs = append(errs, fmt.Errorf("deps.allow:%d: %w", n, err))
			continue
		}
		e.line = n
		if prev, ok := allow[e.path]; ok {
			errs = append(errs, fmt.Errorf("deps.allow:%d: %s is listed again; line %d lists it first", n, e.path, prev.line))
			continue
		}
		allow[e.path] = e
	}
	return allow, errors.Join(errs...)
}

// actionPath is an Action's owner/repo, as GitHub names a repository.
var actionPath = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// parseEntry parses one line that is not a comment.
func parseEntry(line string) (entry, error) {
	f := strings.Fields(line)
	if len(f) < 5 {
		return entry{}, fmt.Errorf("%q has %d fields, want path, scope, license, since and a reason", line, len(f))
	}
	e := entry{path: f[0], scope: f[1], licenses: strings.Split(f[2], ","), reason: strings.Join(f[4:], " ")}
	switch e.scope {
	case scopeAries, scopeAriesd, scopeBoth, scopeTools:
		if !strings.Contains(strings.Split(e.path, "/")[0], ".") {
			return entry{}, fmt.Errorf("%s is scoped %s but is not a module path", e.path, e.scope)
		}
	case scopeActions:
		if !actionPath.MatchString(e.path) {
			return entry{}, fmt.Errorf("%s is scoped actions but is not an owner/repo", e.path)
		}
	default:
		return entry{}, fmt.Errorf("%s has scope %q, want aries, ariesd, both, tools or actions", e.path, e.scope)
	}
	for _, l := range e.licenses {
		if l == "" {
			return entry{}, fmt.Errorf("%s has license %q, with an empty identifier", e.path, f[2])
		}
		if e.linked() && !slices.Contains(linkable, l) {
			return entry{}, fmt.Errorf("%s is linked (scope %s) and carries %s, which is not one of %s; ask the maintainer",
				e.path, e.scope, l, strings.Join(linkable, ", "))
		}
	}
	since, err := time.Parse(time.DateOnly, f[3])
	if err != nil {
		return entry{}, fmt.Errorf("%s has since %q, want YYYY-MM-DD", e.path, f[3])
	}
	e.since = since
	return e, nil
}

// readAllow returns the repository's deps.allow, failing the test if a
// line of it is bad.
func readAllow(t *testing.T) allowlist {
	t.Helper()
	allow, err := parseAllow(readFile(t, "deps.allow"))
	if err != nil {
		t.Fatal(err)
	}
	return allow
}

// readFile returns a file of the repository, named from its root.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path))) //nolint:gosec // a file of this repository, named by the test
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// goCmd runs go at the repository's root with env added to the test's
// environment, and returns its standard output.
func goCmd(t *testing.T, env []string, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", args...) //nolint:gosec // fixed arguments
	cmd.Dir = root
	// What the gate and CI set (scripts/gate.sh), whatever the caller's
	// environment says.
	cmd.Env = append(cmd.Environ(), "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly")
	cmd.Env = append(cmd.Env, env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %s (%s): %v\n%s", strings.Join(args, " "), strings.Join(env, " "), err, stderr.String())
	}
	return out
}

func TestParseAllow(t *testing.T) {
	t.Parallel()
	good, err := parseAllow(`# a comment
example.com/a  aries  MIT  2026-10-16  the reason, in words

example.com/b  both  MIT,Apache-2.0  2026-10-16  two licenses
example.com/tool  tools  GPL-3.0  2026-10-16  a tool may carry another license
owner/repo  actions  MIT  2026-10-16  an Action
`)
	if err != nil {
		t.Fatalf("parseAllow(good lines) = %v, want no error", err)
	}
	if got := good["example.com/a"]; got.scope != scopeAries || got.reason != "the reason, in words" || got.line != 2 {
		t.Errorf("parseAllow(good lines)[example.com/a] = %+v, want scope aries, the whole reason and line 2", got)
	}
	if got := good["example.com/b"].licenses; !slices.Equal(got, []string{"MIT", "Apache-2.0"}) {
		t.Errorf("parseAllow(good lines)[example.com/b].licenses = %q, want MIT and Apache-2.0", got)
	}
	if len(good) != 4 {
		t.Errorf("parseAllow(good lines) has %d entries, want 4", len(good))
	}

	tests := []struct {
		name, text, want string
	}{
		{"no reason", "example.com/a  aries  MIT  2026-10-16\n", "deps.allow:1: "},
		{"unknown scope", "example.com/a  tests  MIT  2026-10-16  why\n", `scope "tests"`},
		{"linked under another license", "example.com/a  ariesd  GPL-3.0  2026-10-16  why\n", "carries GPL-3.0"},
		{"one of two licenses not linkable", "example.com/a  both  MIT,SSPL-1.0  2026-10-16  why\n", "carries SSPL-1.0"},
		{"empty license", "example.com/a  both  MIT,  2026-10-16  why\n", "empty identifier"},
		{"bad date", "example.com/a  aries  MIT  16 Oct  why\n", "want YYYY-MM-DD"},
		{"action that is a module", "example.com/a/b  actions  MIT  2026-10-16  why\n", "not an owner/repo"},
		{"module that is an action", "owner/repo  tools  MIT  2026-10-16  why\n", "not a module path"},
		{"listed twice", "example.com/a  aries  MIT  2026-10-16  why\nexample.com/a  both  MIT  2026-10-16  why\n", "deps.allow:2: example.com/a is listed again; line 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseAllow(tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseAllow(%q) = %v, want an error containing %q", tc.text, err, tc.want)
			}
		})
	}
}
