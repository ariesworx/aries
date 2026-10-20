package gitx

import (
	"context"
	"errors"
	"os/exec"
)

// MergeConflicts reports whether merging commits a and b conflicts, as
// `git merge-tree --write-tree` (git 2.38 or later) finds it: it writes
// the merge's tree to the object store and touches no worktree or index.
// An outcome record counts the catch-up merges of the default branch
// that conflicted with it (docs/design/outcome-record.md §6).
func MergeConflicts(ctx context.Context, dir, a, b string) (bool, error) {
	// merge-tree exits 1 for a commit it cannot read as well as for a
	// conflict, so both are checked first.
	for _, c := range []string{a, b} {
		if _, err := git(ctx, dir, "rev-parse", "--quiet", "--verify", c+"^{commit}"); err != nil {
			return false, &unknownCommit{rev: c}
		}
	}
	_, err := git(ctx, dir, "merge-tree", "--write-tree", "--no-messages", a, b)
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
		return true, nil
	}
	return false, err
}

// Fetch fetches refspec from remote into the object store alone, with no
// tags and no ref updated: what a check of a change's commits needs
// when its branch is gone but the host keeps a ref for it.
func Fetch(ctx context.Context, dir, remote, refspec string) error {
	_, err := git(ctx, dir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", remote, refspec)
	return err
}
