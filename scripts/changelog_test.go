package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	// changelogHeading is an entry's heading: "## v0.4.0 (2026-10-09)",
	// or "## v0.5.0 (unreleased)" until the release is cut.
	changelogHeading = regexp.MustCompile(`^## (v\d+\.\d+\.\d+) \((\d{4}-\d{2}-\d{2}|unreleased)\)$`)
	// markdownLink matches a link's target.
	markdownLink = regexp.MustCompile(`\]\(([^)\s]+)\)`)
)

// semver orders two plain vMAJOR.MINOR.PATCH versions.
func semver(a, b string) int {
	pa, pb := strings.Split(a[1:], "."), strings.Split(b[1:], ".")
	for i := range pa {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return 0
}

// TestChangelog holds CHANGELOG.md to the tags and the files it links:
// every v* tag has an entry and a notes file, the entries are newest
// first, an entry links its own notes, and every relative link resolves.
func TestChangelog(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	entries := map[string]string{}
	var cur string
	for _, line := range strings.Split(string(b), "\n") {
		if m := changelogHeading.FindStringSubmatch(line); m != nil {
			cur = m[1]
			if _, dup := entries[cur]; dup {
				t.Errorf("CHANGELOG.md has two entries for %s", cur)
			}
			versions = append(versions, cur)
			entries[cur] = ""
			continue
		}
		if strings.HasPrefix(line, "## ") {
			t.Errorf("heading %q is not \"## vX.Y.Z (YYYY-MM-DD)\" or \"(unreleased)\"", line)
		}
		if cur != "" {
			entries[cur] += line + "\n"
		}
		for _, m := range markdownLink.FindAllStringSubmatch(line, -1) {
			target := m[1]
			if strings.Contains(target, "://") {
				continue
			}
			path, _, _ := strings.Cut(target, "#")
			if path == "" {
				continue
			}
			if !filepath.IsLocal(filepath.FromSlash(path)) {
				t.Errorf("CHANGELOG.md links %s, which leaves the repository", target)
				continue
			}
			if _, err := os.Stat(filepath.Join("..", filepath.FromSlash(path))); err != nil { //nolint:gosec // a relative path inside the repository, checked above
				t.Errorf("CHANGELOG.md links %s, which does not resolve", target)
			}
		}
	}
	if len(versions) == 0 {
		t.Fatal("CHANGELOG.md has no entries")
	}
	if !slices.IsSortedFunc(versions, func(a, b string) int { return semver(b, a) }) {
		t.Errorf("entries %v are not newest first", versions)
	}
	for v, body := range entries {
		if !strings.Contains(body, "(docs/releases/"+v+".md)") {
			t.Errorf("the entry for %s does not link docs/releases/%s.md", v, v)
		}
		if _, err := os.Stat("../docs/releases/" + v + ".md"); err != nil {
			t.Errorf("%s has no notes file: %v", v, err)
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cmd := exec.CommandContext(t.Context(), "git", "tag", "--list", "v*")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git tag: %v", err)
	}
	tags := strings.Fields(string(out))
	if len(tags) == 0 {
		t.Skip("no tags here, as in a shallow clone")
	}
	for _, tag := range tags {
		if !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(tag) {
			continue // a prerelease tag has no entry of its own
		}
		if _, ok := entries[tag]; !ok {
			t.Errorf("tag %s has no entry in CHANGELOG.md", tag)
		}
	}
}
