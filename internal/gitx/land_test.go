package gitx

import (
	"path/filepath"
	"strings"
	"testing"
)

// A commit names its issue by an Aries: trailer, else by the branch a
// merge commit merged; neither names none.
func TestIssueOfMessage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, msg, id, branch string
		ok                    bool
	}{
		{"trailer", "feature: x\n\nwhy\n\nAries: ar-a1b2\n", "ar-a1b2", "", true},
		{"trailer beats a merge subject", "Merge branch 'feature/ar-c3d4-y'\n\nAries: ar-a1b2\n", "ar-a1b2", "", true},
		{"github merge", "Merge pull request #12 from example/feature/ar-c3d4-y\n\nfeature: y\n", "", "example/feature/ar-c3d4-y", true},
		{"gitlab merge", "Merge branch 'fix/ar-e5f6-z' into 'main'\n", "", "fix/ar-e5f6-z", true},
		{"neither", "chore: bump a thing\n", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			id, branch, ok := IssueOfMessage(tc.msg)
			if id != tc.id || branch != tc.branch || ok != tc.ok {
				t.Errorf("IssueOfMessage = %q, %q, %v; want %q, %q, %v", id, branch, ok, tc.id, tc.branch, tc.ok)
			}
		})
	}
}

func commitHash(t *testing.T, dir string, msg ...string) string {
	t.Helper()
	args := []string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty"}
	for _, m := range msg {
		args = append(args, "-m", m)
	}
	if _, err := git(t.Context(), dir, args...); err != nil {
		t.Fatal(err)
	}
	h, err := git(t.Context(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Since walks the default branch's first parents, oldest first, without
// the commit a merge brought in; with no commit to start from it is HEAD
// alone.
func TestSince(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	ctx := t.Context()
	base, _ := git(ctx, dir, "rev-parse", "HEAD")
	one := commitHash(t, dir, "one", "Aries: ar-a1b2")
	if _, err := git(ctx, dir, "switch", "-q", "-c", "feature/ar-c3d4-x"); err != nil {
		t.Fatal(err)
	}
	side := commitHash(t, dir, "side", "Aries: ar-zzzz")
	if _, err := git(ctx, dir, "switch", "-q", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "merge", "-q", "--no-ff",
		"-m", "Merge pull request #3 from example/feature/ar-c3d4-x", "feature/ar-c3d4-x"); err != nil {
		t.Fatal(err)
	}
	merge, _ := git(ctx, dir, "rev-parse", "HEAD")

	got, err := Since(ctx, dir, base, 10)
	if err != nil || len(got) != 2 || got[0].SHA != one || got[1].SHA != merge {
		t.Fatalf("Since(base) = %+v, %v; want %s then %s, not %s", got, err, one, merge, side)
	}
	if !strings.Contains(got[1].Message, "ar-c3d4-x") {
		t.Errorf("merge message = %q", got[1].Message)
	}
	if got, err := Since(ctx, dir, "", 10); err != nil || len(got) != 1 || got[0].SHA != merge {
		t.Errorf("Since(none) = %+v, %v; want HEAD alone", got, err)
	}
	if got, err := Since(ctx, dir, merge, 10); err != nil || len(got) != 0 {
		t.Errorf("Since(HEAD) = %+v, %v; want none", got, err)
	}
	if _, err := Since(ctx, dir, base, 1); err == nil {
		t.Error("Since past its limit succeeded")
	}
	if _, err := Since(ctx, dir, strings.Repeat("a", 40), 10); !IsUnknownCommit(err) {
		t.Errorf("Since an unknown commit = %v; want IsUnknownCommit", err)
	}
}

func TestDefaultBranch(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	if got, err := DefaultBranch(t.Context(), dir); err != nil || got != "main" {
		t.Errorf("DefaultBranch = %q, %v; want main", got, err)
	}
	if _, err := git(t.Context(), dir, "branch", "-m", "trunk"); err != nil {
		t.Fatal(err)
	}
	if got, err := DefaultBranch(t.Context(), dir); err != nil || got != "" {
		t.Errorf("DefaultBranch with none = %q, %v; want none", got, err)
	}
	if _, err := git(t.Context(), dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk"); err != nil {
		t.Fatal(err)
	}
	if got, err := DefaultBranch(t.Context(), dir); err != nil || got != "trunk" {
		t.Errorf("DefaultBranch from origin/HEAD = %q, %v; want trunk", got, err)
	}
}

// A hook's path is where git runs it from: the repository's hooks
// directory, shared by its worktrees, or core.hooksPath.
func TestHookPath(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	ctx := t.Context()
	got, err := HookPath(ctx, dir, "post-merge")
	if err != nil || !strings.HasSuffix(filepath.ToSlash(got), "/.git/hooks/post-merge") || !filepath.IsAbs(got) {
		t.Errorf("HookPath = %q, %v; want the repository's hooks/post-merge, absolute", got, err)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := git(ctx, dir, "worktree", "add", "-q", "-b", "other", wt); err != nil {
		t.Fatal(err)
	}
	if again, err := HookPath(ctx, wt, "post-merge"); err != nil || realPath(t, again) != realPath(t, got) {
		t.Errorf("HookPath in a worktree = %q, %v; want %q", again, err, got)
	}
	if _, err := git(ctx, dir, "config", "core.hooksPath", "/elsewhere/hooks"); err != nil {
		t.Fatal(err)
	}
	if got, err := HookPath(ctx, dir, "post-merge"); err != nil || filepath.ToSlash(got) != "/elsewhere/hooks/post-merge" {
		t.Errorf("HookPath with core.hooksPath = %q, %v", got, err)
	}
	if _, err := HookPath(ctx, t.TempDir(), "post-merge"); err == nil {
		t.Error("HookPath outside a repository succeeded")
	}
}

// realPath resolves the links in the directory of p, as git does.
func realPath(t *testing.T, p string) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(d, filepath.Base(p))
}
