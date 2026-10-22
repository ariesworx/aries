package gitx

import (
	"testing"
)

// ShowFile reads a file as a commit has it, byte for byte, says when the
// commit has none, and fails on a commit the repository lacks.
func TestShowFile(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	write(t, dir, ".aries/guard.yaml", "\n\nversion: 1\n")
	repoGit(t, dir, "add", "--all")
	repoGit(t, dir, "commit", "-q", "-m", "policy")
	write(t, dir, ".aries/guard.yaml", "edited in the worktree\n")

	b, ok, err := ShowFile(t.Context(), dir, "main", ".aries/guard.yaml")
	if err != nil || !ok || string(b) != "\n\nversion: 1\n" {
		t.Errorf("ShowFile = %q, %v, %v; want the committed bytes", b, ok, err)
	}
	if b, ok, err := ShowFile(t.Context(), dir, "main", ".aries/none.yaml"); err != nil || ok || b != nil {
		t.Errorf("ShowFile of a missing file = %q, %v, %v; want none", b, ok, err)
	}
	if _, _, err := ShowFile(t.Context(), dir, "origin/main", ".aries/guard.yaml"); err == nil {
		t.Errorf("ShowFile of a commit the repository lacks did not fail")
	}
}
