package depcheck

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The cooldown (AGENTS.md, rule 4; supply-chain.md, §7.3): a version is
// taken no sooner than 14 days after it was published, 30 for a new
// major version, unless it fixes a vulnerability.
const (
	cooldown      = 14 * 24 * time.Hour
	cooldownMajor = 30 * 24 * time.Hour
)

// Environment of TestCooldown, set by .github/workflows/deps.yml. Without
// the first the test skips: it needs the pull request's base and the
// module proxy.
const (
	envBase        = "ARIES_DEPS_BASE"         // a commit or ref of the pull request's base
	envSecurityFix = "ARIES_DEPS_SECURITY_FIX" // "1" when the pull request carries the label security-fix
)

// moduleFiles are the go.mod files whose versions the cooldown covers.
var moduleFiles = []string{"go.mod", toolsModFile}

// versionedMod is a module at a version, as a go.mod requires it.
type versionedMod struct{ path, version string }

// majorSuffix matches a module path's major version element, /v2 and on.
var majorSuffix = regexp.MustCompile(`/v([2-9]|[1-9][0-9]+)$`)

// stem is a module path without its major version element.
func stem(path string) string { return majorSuffix.ReplaceAllString(path, "") }

// taken returns the requirements of head that base did not have at that
// version: a module added, a version moved, or a new major version.
// Indirect modules count: the cooldown holds what the build links.
func taken(base, head []versionedMod) []versionedMod {
	have := map[versionedMod]bool{}
	for _, m := range base {
		have[m] = true
	}
	var out []versionedMod
	for _, m := range head {
		if !have[m] {
			out = append(out, m)
		}
	}
	return out
}

// newMajor reports whether m is a major version of a module that base
// required at another major version (a path with another /vN, or none).
func newMajor(base []versionedMod, m versionedMod) bool {
	for _, b := range base {
		if b.path != m.path && stem(b.path) == stem(m.path) {
			return true
		}
	}
	return false
}

// checkCooldown holds each version in changes to the wait, given when it
// was published and the time now, and returns one error for each that is
// too new or whose publication time is unknown.
func checkCooldown(base, changes []versionedMod, published map[versionedMod]time.Time, now time.Time) []error {
	var errs []error
	for _, m := range changes {
		at, ok := published[m]
		if !ok || at.IsZero() {
			errs = append(errs, fmt.Errorf("%s %s: the module proxy gives no publication time, so the cooldown cannot be checked", m.path, m.version))
			continue
		}
		wait, which := cooldown, "14"
		if newMajor(base, m) {
			wait, which = cooldownMajor, "30, for a new major version,"
		}
		if age := now.Sub(at); age < wait {
			errs = append(errs, fmt.Errorf("%s %s was published %s ago, inside the cooldown of %s days (AGENTS.md, rule 4); take an older version, or label the pull request security-fix if it fixes a vulnerability govulncheck or an advisory names",
				m.path, m.version, age.Round(time.Hour), which))
		}
	}
	return errs
}

// requirements returns what the go.mod at path requires, direct and
// indirect.
func requirements(t *testing.T, path string) []versionedMod {
	t.Helper()
	var m struct {
		Require []struct{ Path, Version string }
	}
	if err := json.Unmarshal(goCmd(t, nil, "mod", "edit", "-json", path), &m); err != nil {
		t.Fatalf("go mod edit -json %s: %v", path, err)
	}
	var out []versionedMod
	for _, r := range m.Require {
		out = append(out, versionedMod{r.Path, r.Version})
	}
	return out
}

// baseRequirements returns what the go.mod at name (from the root)
// required at the commit base; none if the file did not exist there.
func baseRequirements(t *testing.T, base, name string) []versionedMod {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "show", base+":"+name) //nolint:gosec // the pull request's base and fixed names
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	file := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(file, out, 0o600); err != nil { //nolint:gosec // a file in the test's own temporary directory
		t.Fatal(err)
	}
	return requirements(t, file)
}

func TestCooldown(t *testing.T) {
	t.Parallel()
	base := os.Getenv(envBase)
	if base == "" {
		t.Skipf("%s is not set: the cooldown is checked against a pull request's base, by .github/workflows/deps.yml", envBase)
	}
	if os.Getenv(envSecurityFix) == "1" {
		t.Skip("labeled security-fix: a fix for a vulnerability skips the wait, never the review (AGENTS.md, rule 4)")
	}
	var oldReqs, changes []versionedMod
	for _, name := range moduleFiles {
		oldReqs = append(oldReqs, baseRequirements(t, base, name)...)
		changes = append(changes, taken(baseRequirements(t, base, name), requirements(t, name))...)
	}
	published := map[versionedMod]time.Time{}
	for _, m := range changes {
		var info struct{ Time time.Time }
		if err := json.Unmarshal(goCmd(t, nil, "list", "-m", "-json", m.path+"@"+m.version), &info); err != nil {
			t.Fatalf("go list -m -json %s@%s: %v", m.path, m.version, err)
		}
		published[m] = info.Time
	}
	for _, err := range checkCooldown(oldReqs, changes, published, time.Now()) { //nolint:forbidigo // the cooldown counts days from now, not from the store's clock
		t.Error(err)
	}
}

func TestCheckCooldown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	base := []versionedMod{{"example.com/a", "v1.0.0"}, {"example.com/b/v2", "v2.1.0"}, {"example.com/same", "v1.0.0"}}
	tests := []struct {
		name      string
		head      []versionedMod
		published map[versionedMod]time.Time
		wantTaken int
		want      string // in the one error; empty: none
	}{
		{name: "nothing changed", head: base},
		{name: "an old version", head: []versionedMod{{"example.com/a", "v1.1.0"}}, wantTaken: 1,
			published: map[versionedMod]time.Time{{"example.com/a", "v1.1.0"}: now.Add(-15 * day)}},
		{name: "a new version", head: []versionedMod{{"example.com/a", "v1.1.0"}}, wantTaken: 1,
			published: map[versionedMod]time.Time{{"example.com/a", "v1.1.0"}: now.Add(-3 * day)},
			want:      "inside the cooldown of 14 days"},
		{name: "a module added too soon", head: []versionedMod{{"example.com/new", "v0.1.0"}}, wantTaken: 1,
			published: map[versionedMod]time.Time{{"example.com/new", "v0.1.0"}: now.Add(-day)},
			want:      "example.com/new v0.1.0 was published"},
		{name: "a new major version is 30 days", head: []versionedMod{{"example.com/a/v2", "v2.0.0"}}, wantTaken: 1,
			published: map[versionedMod]time.Time{{"example.com/a/v2", "v2.0.0"}: now.Add(-20 * day)},
			want:      "30, for a new major version"},
		{name: "a new major version old enough", head: []versionedMod{{"example.com/b/v3", "v3.0.0"}}, wantTaken: 1,
			published: map[versionedMod]time.Time{{"example.com/b/v3", "v3.0.0"}: now.Add(-31 * day)}},
		{name: "a minor of a v2 module is 14 days", head: []versionedMod{{"example.com/b/v2", "v2.2.0"}}, wantTaken: 1,
			published: map[versionedMod]time.Time{{"example.com/b/v2", "v2.2.0"}: now.Add(-15 * day)}},
		{name: "no publication time", head: []versionedMod{{"example.com/a", "v1.1.0"}}, wantTaken: 1,
			want: "gives no publication time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			changes := taken(base, tt.head)
			if len(changes) != tt.wantTaken {
				t.Fatalf("taken = %v, want %d", changes, tt.wantTaken)
			}
			errs := checkCooldown(base, changes, tt.published, now)
			switch {
			case tt.want == "" && len(errs) > 0:
				t.Errorf("errors %v, want none", errs)
			case tt.want != "" && (len(errs) != 1 || !strings.Contains(errs[0].Error(), tt.want)):
				t.Errorf("errors %v, want one containing %q", errs, tt.want)
			}
		})
	}
}
