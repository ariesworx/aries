package dispatch

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/merges"
)

// GH's Open lists the open PRs, maps each to its issue by its commits'
// trailer before its branch, leaves out forks, and reads required checks
// only for a PR an issue names that does not conflict.
func TestGHOpen(t *testing.T) {
	t.Parallel()
	list := `[
 {"number":7,"url":"https://example.com/pr/7","headRefName":"feature/ar-1-x","headRefOid":"aaa","mergeable":"CONFLICTING","labels":[],"commits":[{"messageHeadline":"x","messageBody":"why\n\nAries: ar-1"}],"isCrossRepository":false},
 {"number":8,"url":"https://example.com/pr/8","headRefName":"fix/ar-2-y","headRefOid":"bbb","mergeable":"MERGEABLE","labels":[{"name":"dispatch:rework"}],"commits":[{"messageHeadline":"y","messageBody":""}],"isCrossRepository":false},
 {"number":9,"url":"https://example.com/pr/9","headRefName":"feature/ar-3-z","headRefOid":"ccc","mergeable":"MERGEABLE","labels":[],"commits":[{"messageHeadline":"z","messageBody":"Aries: ar-4"}],"isCrossRepository":true},
 {"number":10,"url":"https://example.com/pr/10","headRefName":"main-copy","headRefOid":"ddd","mergeable":"UNKNOWN","labels":[],"commits":[],"isCrossRepository":false}
]`
	var asked []string
	run := func(_ context.Context, _, name string, args ...string) ([]byte, error) {
		asked = append(asked, name+" "+strings.Join(args, " "))
		switch {
		case args[0] == "pr" && args[1] == "list":
			return []byte(list), nil
		case args[0] == "pr" && args[1] == "checks" && args[2] == "8":
			return []byte(`[{"name":"test","bucket":"fail"},{"name":"lint","bucket":"pass"}]`), errors.New("exit status 1")
		}
		return nil, errors.New("unexpected")
	}
	prs, err := (&GH{Dir: t.TempDir(), Run: run}).Open(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]OpenPR{}
	for _, pr := range prs {
		got[pr.Number] = pr
	}
	if _, ok := got[9]; ok || len(prs) != 3 {
		t.Errorf("prs = %+v; want 7, 8 and 10, without the fork", prs)
	}
	if pr := got[7]; pr.Issue != "ar-1" || pr.Mergeable != MergeConflicting || pr.Checks != "" || pr.Head != "aaa" {
		t.Errorf("PR 7 = %+v", pr)
	}
	if pr := got[8]; pr.Issue != "ar-2" || pr.Checks != ChecksFail || !slices.Contains(pr.Labels, ReworkLabel) {
		t.Errorf("PR 8 = %+v", pr)
	}
	if pr := got[10]; pr.Mergeable != MergeUnknown {
		t.Errorf("PR 10 = %+v", pr)
	}
	for _, a := range asked {
		if strings.Contains(a, "checks 7") {
			t.Errorf("read the checks of a conflicted PR: %s", a)
		}
		if strings.HasPrefix(a, "gh api") || strings.HasPrefix(a, "gh pr merge") {
			t.Errorf("gh was asked to %s", a)
		}
	}
}

func TestRequiredRollup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		buckets []string
		want    string
	}{
		{nil, ""},
		{[]string{"pass", "skipping"}, ChecksPass},
		{[]string{"pass", "pending"}, ChecksPending},
		{[]string{"pending", "fail"}, ChecksFail},
		{[]string{"cancel"}, ChecksFail},
	}
	for _, tc := range tests {
		var cs []merges.Check
		for _, b := range tc.buckets {
			cs = append(cs, merges.Check{Name: b, Bucket: b})
		}
		if got := requiredRollup(cs); got != tc.want {
			t.Errorf("requiredRollup(%v) = %q; want %q", tc.buckets, got, tc.want)
		}
	}
}

// A rework resumes the run's own session rather than naming a new one.
func TestClaudeArgvResume(t *testing.T) {
	t.Parallel()
	c := &Claude{Aries: "aries"}
	sp := Spec{Session: "0f6e5c1a-0000-4000-8000-000000000001", Model: "claude-opus-5-5", Dir: "/repo/wt", Capacity: 4, Lease: 10 * time.Minute, Resume: true}
	line := strings.Join(c.Argv(sp), " ")
	if !strings.Contains(line, "claude -p --resume "+sp.Session) || strings.Contains(line, "--session-id") {
		t.Errorf("argv = %s", line)
	}
}

// Comment posts the body on stdin, from the dispatcher's own session.
func TestExecComment(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is started as a program by its path")
	}
	env, record := standin(t)
	e := &Exec{Aries: testBinary(t), Dir: t.TempDir(), Environ: env, Session: "dispatch-1"}
	if err := e.Comment(t.Context(), "ar-1", "rework: pr=7\n\nwhy"); err != nil {
		t.Fatal(err)
	}
	got := records(t, record)
	if len(got) != 1 || !slices.Equal(got[0].Args, []string{"comment", "ar-1", "-"}) || got[0].Session != "dispatch-1" || got[0].Stdin != "rework: pr=7\n\nwhy" {
		t.Errorf("aries was started as %+v", got)
	}
}

// ReworkWorktree makes a worktree on a PR's branch as the remote has it,
// not on the default branch; a clean one already there is brought up to
// the remote's branch, and one with work of its own is kept aside.
func TestExecGitReworkWorktree(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	origin, top, other := filepath.Join(root, "origin.git"), filepath.Join(root, "repo"), filepath.Join(root, "other")
	gitIn := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // git, with the test's arguments
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=alice", "GIT_AUTHOR_EMAIL=alice@example.com",
			"GIT_COMMITTER_NAME=alice", "GIT_COMMITTER_EMAIL=alice@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(dir, name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(root, "init", "--quiet", "--bare", "--initial-branch=main", origin)
	gitIn(root, "clone", "--quiet", origin, top)
	write(top, "README.md", "one\n")
	gitIn(top, "add", ".")
	gitIn(top, "commit", "--quiet", "-m", "first")
	gitIn(top, "push", "--quiet", "origin", "HEAD:main")
	// Someone else's PR branch, pushed from another clone.
	const branch = "fix/ar-2-y"
	gitIn(root, "clone", "--quiet", origin, other)
	gitIn(other, "switch", "--quiet", "-c", branch)
	write(other, "fix.txt", "fix\n")
	gitIn(other, "add", ".")
	gitIn(other, "commit", "--quiet", "-m", "fix")
	gitIn(other, "push", "--quiet", "origin", branch)
	prHead := gitIn(other, "rev-parse", "HEAD")

	g := &ExecGit{Top: top, Remote: "origin", Base: "main"}
	ctx := t.Context()
	wt := filepath.Join(top, ".aries-dispatch", "worktrees", "ar-2")
	if found, err := g.ReworkWorktree(ctx, wt, branch); err != nil || found != "" {
		t.Fatalf("ReworkWorktree = %q, %v", found, err)
	}
	if got := gitIn(wt, "rev-parse", "HEAD"); got != prHead {
		t.Errorf("the worktree is at %s; want the PR's head %s", got, prHead)
	}
	if got := gitIn(wt, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Errorf("the worktree is on %s", got)
	}
	if st := gitIn(top, "status", "--porcelain"); st != "" {
		t.Errorf("the checkout shows the worktrees' directory: %q", st)
	}

	// The PR moves on: a clean worktree there follows it.
	write(other, "fix.txt", "fix 2\n")
	gitIn(other, "commit", "--quiet", "-am", "fix 2")
	gitIn(other, "push", "--quiet", "origin", branch)
	if found, err := g.ReworkWorktree(ctx, wt, branch); err != nil || found != "" {
		t.Fatalf("again = %q, %v", found, err)
	}
	if got, want := gitIn(wt, "rev-parse", "HEAD"), gitIn(other, "rev-parse", "HEAD"); got != want {
		t.Errorf("the worktree stayed at %s; want %s", got, want)
	}

	// Work of its own there is kept aside, not reset.
	write(wt, "wip.txt", "mine\n")
	found, err := g.ReworkWorktree(ctx, wt, branch)
	if err != nil || !strings.Contains(found, "kept, not reset") {
		t.Fatalf("with work of its own = %q, %v", found, err)
	}
	if got, want := gitIn(wt, "rev-parse", "HEAD"), gitIn(other, "rev-parse", "HEAD"); got != want {
		t.Errorf("the new worktree is at %s; want %s", got, want)
	}

	if _, err := g.ReworkWorktree(ctx, filepath.Join(top, ".aries-dispatch", "worktrees", "ar-9"), "fix/ar-9-gone"); err == nil {
		t.Error("a branch the remote lacks gave a worktree")
	}
}
