package gitx

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// What `aries land` reads of a checkout of the default branch (design
// §21.8): the issue a commit names, and the commits since the last one
// reported.

// IssueOfMessage returns what a commit's message says of the issue it
// landed: the id in its Aries: trailer, else the branch a merge commit
// merged, whose last segment names the issue (IDFromBranch). `gate pass
// --from-commit` and `aries land` both read a commit by it. ok is false
// when the message names neither.
func IssueOfMessage(msg string) (id, branch string, ok bool) {
	if id, ok := IDFromMessage(msg); ok {
		return id, "", true
	}
	if b, ok := BranchFromMerge(msg); ok {
		return "", b, true
	}
	return "", "", false
}

// DefaultBranch returns the name of the default branch of the repository
// holding dir: origin's HEAD, else main or master, whether origin's or
// local. It returns "" when none of them is there.
func DefaultBranch(ctx context.Context, dir string) (string, error) {
	if ref, err := git(ctx, dir, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimPrefix(ref, "refs/remotes/origin/"); name != ref && name != "" {
			return name, nil
		}
	} else if ee, ok := errors.AsType[*exec.ExitError](err); !ok || ee.ExitCode() != 1 {
		return "", err
	}
	for _, name := range []string{"main", "master"} {
		for _, ref := range []string{"refs/remotes/origin/" + name, "refs/heads/" + name} {
			if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
				return name, nil
			}
		}
	}
	return "", nil
}

// LogEntry is one commit: its hash and its whole message.
type LogEntry struct {
	SHA, Message string
}

// unknownCommit is the error of a commit the repository does not have.
type unknownCommit struct{ rev string }

func (e *unknownCommit) Error() string { return fmt.Sprintf("this repository has no commit %s", e.rev) }

// IsUnknownCommit reports whether err says Since was given a commit the
// repository does not have.
func IsUnknownCommit(err error) bool {
	_, ok := errors.AsType[*unknownCommit](err)
	return ok
}

// Since returns the commits of HEAD's first-parent line after since,
// oldest first, so that a merge counts once and the commits it brought in
// not at all. With since empty it is HEAD alone. It fails when there are
// more than limit, so that a first run in a long history reports no
// backlog by accident.
func Since(ctx context.Context, dir, since string, limit int) ([]LogEntry, error) {
	count, rev := limit+1, "HEAD"
	if since == "" {
		count = 1
	} else {
		if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", since+"^{commit}"); err != nil {
			if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
				return nil, &unknownCommit{since}
			}
			return nil, err
		}
		rev = since + "..HEAD"
	}
	out, err := gitRaw(ctx, dir, "-c", "log.showSignature=false", "log", "--no-color", "--first-parent", "--reverse", "-z",
		"--max-count="+strconv.Itoa(count), "--format=%H%n%B", rev, "--")
	if err != nil {
		return nil, err
	}
	var es []LogEntry
	for rec := range strings.SplitSeq(out, "\x00") {
		sha, msg, _ := strings.Cut(rec, "\n")
		if sha = strings.TrimSpace(sha); sha != "" {
			es = append(es, LogEntry{SHA: sha, Message: msg})
		}
	}
	if len(es) > limit {
		return nil, fmt.Errorf("more than %d commits since %s", limit, since)
	}
	return es, nil
}

// HookPath returns the absolute path of the hook name of the repository
// holding dir, where git runs it from: in the hooks directory of the
// repository, which its worktrees share, or core.hooksPath's. It reads
// the path and makes nothing.
func HookPath(ctx context.Context, dir, name string) (string, error) {
	p, err := git(ctx, dir, "rev-parse", "--git-path", "hooks/"+name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p, nil
}
