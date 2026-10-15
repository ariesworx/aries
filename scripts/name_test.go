package scripts_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// oldName matches the project's former name and its client's, in any
// case. The brackets keep this file from matching itself.
var oldName = regexp.MustCompile(`(?i)st[a]rfix|\bs[f]x\b`)

// TestNoOldName fails if the path or the text of any file git tracks
// names the project or its client by their former names. It allows no
// exception: the rename to Aries left nothing that reads an old name.
func TestNoOldName(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cmd := exec.CommandContext(t.Context(), "git", "ls-files", "-z")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(files) < 100 {
		t.Fatalf("git ls-files listed %d files; want the whole repository", len(files))
	}
	for _, f := range files {
		if m := oldName.FindString(f); m != "" {
			t.Errorf("path %s names %q", f, m)
		}
		b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(f))) //nolint:gosec // a file git tracks in this repository
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		// One error a file, at its first line that matches, keeps a
		// failure readable.
		var first string
		n := 0
		for i, line := range bytes.Split(b, []byte("\n")) {
			if m := oldName.Find(line); m != nil {
				if n == 0 {
					first = fmt.Sprintf("%s:%d names %q", f, i+1, m)
				}
				n++
			}
		}
		if n > 0 {
			t.Errorf("%s, the first of %d such lines", first, n)
		}
	}
}
