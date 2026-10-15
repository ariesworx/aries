package gitx

import (
	"testing"
)

// A CI job's branch comes from the variables GitHub Actions and GitLab CI
// set, a pull or merge request's source branch first.
func TestBranchFromEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"none", nil, ""},
		{"a GitHub pull request", map[string]string{"GITHUB_HEAD_REF": "feature/sf-a1b2-x", "GITHUB_REF_NAME": "12/merge", "GITHUB_REF_TYPE": "branch"}, "feature/sf-a1b2-x"},
		{"a GitHub push", map[string]string{"GITHUB_HEAD_REF": "", "GITHUB_REF_NAME": "fix/sf-c3d4", "GITHUB_REF_TYPE": "branch"}, "fix/sf-c3d4"},
		{"a GitHub tag", map[string]string{"GITHUB_REF_NAME": "v1.2.0", "GITHUB_REF_TYPE": "tag"}, ""},
		{"a GitLab merge request", map[string]string{"CI_MERGE_REQUEST_SOURCE_BRANCH_NAME": "feature/sf-e5f6", "CI_COMMIT_REF_NAME": "feature/sf-e5f6"}, "feature/sf-e5f6"},
		{"a GitLab branch pipeline", map[string]string{"CI_COMMIT_BRANCH": "main"}, "main"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BranchFromEnv(func(k string) string { return tc.env[k] }); got != tc.want {
				t.Errorf("BranchFromEnv(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

// After a merge, the default branch's commit names the merged branch in
// the subject git, GitHub and GitLab write for a merge commit.
func TestBranchFromMerge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		msg, want string
	}{
		{"Merge pull request #12 from example/feature/sf-a1b2-thing\n\nThe thing", "example/feature/sf-a1b2-thing"},
		{"Merge branch 'fix/sf-c3d4' into 'main'\n\nSee merge request example/repo!7", "fix/sf-c3d4"},
		{"Merge branch 'fix/sf-c3d4'", "fix/sf-c3d4"},
		{"Merge remote-tracking branch 'origin/fix/sf-c3d4'", "origin/fix/sf-c3d4"},
		{"Fix the login redirect (#12)", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got, ok := BranchFromMerge(tc.msg)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("BranchFromMerge(%q) = %q, %v; want %q", tc.msg, got, ok, tc.want)
		}
	}
}

// The checked-out branch, none when HEAD is detached, and HEAD's message.
func TestCurrentBranchAndHeadMessage(t *testing.T) {
	t.Parallel()
	dir := newRepo(t)
	ctx := t.Context()
	if got, err := CurrentBranch(ctx, dir); err != nil || got != "main" {
		t.Errorf("CurrentBranch = %q, %v; want main", got, err)
	}
	if _, err := git(ctx, dir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty",
		"-m", "Do the thing", "-m", "Starfix: sf-a1b2"); err != nil {
		t.Fatal(err)
	}
	msg, err := HeadMessage(ctx, dir)
	if id, ok := IDFromMessage(msg); err != nil || !ok || id != "sf-a1b2" {
		t.Errorf("HeadMessage = %q, %v; want a message naming sf-a1b2", msg, err)
	}
	if _, err := git(ctx, dir, "switch", "-q", "--detach", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if got, err := CurrentBranch(ctx, dir); err != nil || got != "" {
		t.Errorf("CurrentBranch on a detached HEAD = %q, %v; want none", got, err)
	}
}
