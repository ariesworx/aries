package gitx

import (
	"os"
	"path/filepath"
	"testing"
)

// writeCommit writes name with text and commits it, returning the commit.
func writeCommit(t *testing.T, dir, name, text, msg string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := git(t.Context(), dir, "add", name); err != nil {
		t.Fatal(err)
	}
	return commitHash(t, dir, msg)
}

// MergeConflicts says whether two commits merge cleanly, as a catch-up
// merge of the default branch would, without touching the worktree.
func TestMergeConflicts(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	ctx := t.Context()
	writeCommit(t, dir, "a.txt", "one\n", "base")
	if _, err := git(ctx, dir, "switch", "-q", "-c", "feature/ar-a1b2-x"); err != nil {
		t.Fatal(err)
	}
	side := writeCommit(t, dir, "a.txt", "side\n", "side")
	clean := writeCommit(t, dir, "b.txt", "b\n", "unrelated")
	if _, err := git(ctx, dir, "switch", "-q", "main"); err != nil {
		t.Fatal(err)
	}
	main := writeCommit(t, dir, "a.txt", "main\n", "main")
	other := writeCommit(t, dir, "c.txt", "c\n", "other")
	if _, err := git(ctx, dir, "switch", "-q", "-c", "fix/ar-c3d4-y", side+"~1"); err != nil {
		t.Fatal(err)
	}
	apart := writeCommit(t, dir, "d.txt", "d\n", "apart")
	if _, err := git(ctx, dir, "switch", "-q", "main"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"same line changed on both sides", side, main, true},
		{"a descendant of a conflicting commit", clean, other, true},
		{"different files", apart, other, false},
		{"a commit with its ancestor", main, other, false},
	}
	for _, tc := range tests {
		got, err := MergeConflicts(ctx, dir, tc.a, tc.b)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: MergeConflicts = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := MergeConflicts(ctx, dir, "0123456789abcdef0123456789abcdef01234567", main); err == nil {
		t.Error("MergeConflicts of an unknown commit: no error")
	}
	if st, _ := git(ctx, dir, "status", "--porcelain"); st != "" {
		t.Errorf("the worktree changed: %q", st)
	}
}
