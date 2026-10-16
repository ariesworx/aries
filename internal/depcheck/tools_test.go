package depcheck

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// unpinned matches a go run or go install of a package at a version: a
// tool fetched by a version string that no go.sum covers.
var unpinned = regexp.MustCompile(`\bgo\s+(?:run|install)\s+(?:-\S+\s+)*\S+@\S+`)

// unpinnedTools returns the numbers of the lines of text that fetch a
// tool by a version string.
func unpinnedTools(text string) []int {
	var lines []int
	n := 0
	for line := range strings.Lines(text) {
		n++
		if unpinned.MatchString(line) {
			lines = append(lines, n)
		}
	}
	return lines
}

// TestToolsArePinned fails on a go run or go install of pkg@version in a
// workflow or a contributor script: the gate's tools are pinned by
// internal/tools/pin/go.mod and its go.sum (supply-chain.md §7.4), and
// run with go tool -modfile=internal/tools/pin/go.mod.
func TestToolsArePinned(t *testing.T) {
	t.Parallel()
	scripts, err := filepath.Glob(filepath.Join(root, "scripts", "*.sh"))
	if err != nil {
		t.Fatal(err)
	}
	files := workflows(t)
	for _, s := range scripts {
		files = append(files, "scripts/"+filepath.Base(s))
	}
	if !slices.Contains(files, "scripts/gate.sh") {
		t.Fatalf("files %q do not include scripts/gate.sh", files)
	}
	for _, file := range files {
		text := readFile(t, file)
		lines := strings.Split(text, "\n")
		for _, n := range unpinnedTools(text) {
			t.Errorf("%s:%d: %s fetches a tool by a version string; pin it in %s and run it with go tool -modfile=%s",
				file, n, strings.TrimSpace(lines[n-1]), toolsModFile, toolsModFile)
		}
	}
}

func TestUnpinnedTools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, text string
		want       []int
	}{
		{"go run at a version", "run: go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...\n", []int{1}},
		{"go run at latest", "steps:\n  - run: go run example.com/tool@latest\n", []int{2}},
		{"go install with a flag", "go install -v example.com/cmd/tool@v1\n", []int{1}},
		{"in a shell function", "gate 'go run x@v1'\n", []int{1}},
		{"the pinned tool", "go tool -modfile=internal/tools/pin/go.mod govulncheck ./...\n", nil},
		{"a local package", "go run ./internal/tools/releasekey sign checksums.txt\n", nil},
		{"an Action's ref", "uses: owner/action@3d3c42e5aac5ba805825da76410c181273ba90b1 # v1\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := unpinnedTools(tc.text); !slices.Equal(got, tc.want) {
				t.Errorf("unpinnedTools(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}
