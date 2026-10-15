package gitx

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// What a CI job knows of its work (design §20.4): the branch it runs on,
// which names the issue, or after a merge the default branch's commit,
// whose message names the issue or the branch merged.

// ciBranchEnv are the variables that name a CI job's branch, a pull or
// merge request's source branch first: GitHub Actions' GITHUB_HEAD_REF
// (pull requests only) and GITHUB_REF_NAME (when GITHUB_REF_TYPE is
// branch), and GitLab CI's CI_MERGE_REQUEST_SOURCE_BRANCH_NAME and
// CI_COMMIT_BRANCH (branch pipelines only).
var ciBranchEnv = []string{"GITHUB_HEAD_REF", "GITHUB_REF_NAME", "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME", "CI_COMMIT_BRANCH"}

// BranchFromEnv returns the branch a CI job runs on, from the variables
// GitHub Actions and GitLab CI set, or "" when they name none, as on a
// tag. CI systems check out a detached HEAD, so git cannot say.
func BranchFromEnv(getenv func(string) string) string {
	for _, k := range ciBranchEnv {
		if k == "GITHUB_REF_NAME" && getenv("GITHUB_REF_TYPE") != "branch" {
			continue
		}
		if v := getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// mergeSubject is the subject git, GitHub and GitLab give a merge commit,
// capturing the branch merged: "Merge pull request #12 from OWNER/BRANCH",
// and "Merge branch 'BRANCH'" or "Merge remote-tracking branch 'BRANCH'",
// with GitLab's " into 'TARGET'" after it.
var mergeSubject = lazyregexp.New(`^Merge (?:pull request #[0-9]+ from (\S+)|(?:remote-tracking )?branch '([^']+)'(?: into .*)?)$`)

// BranchFromMerge returns the branch a merge commit's message says was
// merged, and whether it says. With OWNER/ before it, from GitHub, the
// branch's last segment still names the issue (IDFromBranch).
func BranchFromMerge(msg string) (string, bool) {
	subject, _, _ := strings.Cut(msg, "\n")
	m := mergeSubject.FindStringSubmatch(strings.TrimRight(subject, "\r"))
	if m == nil {
		return "", false
	}
	return m[1] + m[2], true
}

// CurrentBranch returns the branch checked out in the repository holding
// dir, or "" when HEAD is detached.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	b, err := git(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
		return "", nil // detached
	}
	return b, err
}

// HeadMessage returns the message of HEAD's commit in the repository
// holding dir.
func HeadMessage(ctx context.Context, dir string) (string, error) {
	return gitRaw(ctx, dir, "-c", "log.showSignature=false", "log", "-1", "--no-color", "--format=%B", "HEAD", "--")
}
