package gitx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// ErrNotFastForward is returned by FastForward when the local branch has
// commits the remote's does not.
var ErrNotFastForward = errors.New("the local branch has commits the remote's lacks")

// Top returns the top-level directory of the working tree holding dir.
func Top(ctx context.Context, dir string) (string, error) {
	return git(ctx, dir, "rev-parse", "--show-toplevel")
}

// HasRemote reports whether the repository holding dir has a remote of
// that name.
func HasRemote(ctx context.Context, dir, remote string) bool {
	_, err := git(ctx, dir, "remote", "get-url", remote)
	return err == nil
}

// Dirty reports whether the working tree has changes to tracked files,
// staged or not. Untracked files do not count: they do not stop a
// fast-forward unless git itself refuses it.
func Dirty(ctx context.Context, dir string) (bool, error) {
	out, err := git(ctx, dir, "status", "--porcelain", "--untracked-files=no")
	return out != "", err
}

// FastForward fetches branch from remote and moves the checked-out branch
// to it when that is a fast-forward, and only then. It returns the commit
// before and after; they are equal when there was nothing to pull. A
// branch with commits of its own is left alone, with ErrNotFastForward.
// The merge runs git's post-merge hook, as a pull does.
func FastForward(ctx context.Context, dir, remote, branch string) (before, after string, err error) {
	if before, err = git(ctx, dir, "rev-parse", "HEAD"); err != nil {
		return "", "", err
	}
	ref := "refs/remotes/" + remote + "/" + branch
	if _, err = git(ctx, dir, "fetch", "--quiet", "--no-tags", remote, "+refs/heads/"+branch+":"+ref); err != nil {
		return before, before, err
	}
	_, err = git(ctx, dir, "merge-base", "--is-ancestor", "HEAD", ref)
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
		if _, err = git(ctx, dir, "merge-base", "--is-ancestor", ref, "HEAD"); err == nil {
			return before, before, nil // the remote is behind: nothing to pull
		}
		return before, before, ErrNotFastForward
	}
	if err != nil {
		return before, before, err
	}
	if _, err = git(ctx, dir, "merge", "--ff-only", "--quiet", ref); err != nil {
		return before, before, fmt.Errorf("fast-forward: %w", err)
	}
	after, err = git(ctx, dir, "rev-parse", "HEAD")
	return before, after, err
}
