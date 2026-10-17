package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/proto"
)

// postMerge is the name of the setup that is no agent's: git's own hook,
// which reports a pull's merges with `aries land` (design §21.8).
const postMerge = "post-merge"

// postMergeScript is the hook aries writes, running command: nothing
// else is in the file, so that a file with more is someone else's.
func postMergeScript(command string) string {
	return "#!/bin/sh\n# Written by `aries setup post-merge`: after a pull, report landed for the merges it brought in.\n# Remove it with `aries setup post-merge --remove`.\n" +
		postMergeLine(command) + "\n"
}

func postMergeLine(command string) string { return command + " land --hook" }

// setupPostMerge prints, checks, writes or removes the post-merge hook of
// the checkout's repository. It writes only when asked to, and only a
// hook that is not there: one of someone else's is never replaced, and
// only a hook that is all aries's is removed.
func (r *runner) setupPostMerge(ctx context.Context, command string, mode setupMode, global, all bool, usage string) error {
	switch {
	case global:
		return usagef(usage, "post-merge is a repository's hook: it takes no --global")
	case all:
		return usagef(usage, "--all sets up agents, and the post-merge hook is set up by name: `aries setup post-merge`")
	}
	path, err := gitx.HookPath(ctx, r.dir, postMerge)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "run it in a checkout of the project's repository", fmt.Sprintf("find git's hooks: %v", err))
	}
	line := postMergeLine(command)
	cur, err := os.ReadFile(path) //nolint:gosec // git's hook for this repository, named by git
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	has := hasLine(cur, line)
	ours := bytes.Equal(cur, []byte(postMergeScript(command))) || (has && onlyOurs(cur, line))
	out := r.env.Stdout
	switch {
	case mode.check:
		if !has {
			return proto.Errf(proto.CodeNotFound, "run `aries setup post-merge --write`", fmt.Sprintf("%s does not run `%s`", path, line))
		}
		_, _ = fmt.Fprintf(out, "%s runs %s\n", esc(path), line)
	case mode.remove:
		switch {
		case !has:
			_, _ = fmt.Fprintf(out, "%s does not run aries: nothing to remove\n", esc(path))
		case !ours:
			return proto.Errf(proto.CodeInvalid, fmt.Sprintf("delete the line `%s` from %s by hand", line, path),
				path+" holds more than aries's line, so aries leaves it")
		default:
			if err := os.Remove(path); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "removed %s\n", esc(path))
		}
	case mode.write:
		switch {
		case has:
			_, _ = fmt.Fprintf(out, "%s already runs `%s`\n", esc(path), line)
		case len(cur) > 0:
			return proto.Errf(proto.CodeInvalid, fmt.Sprintf("add the line `%s` to it", line),
				path+" is a hook of someone else's, and aries does not replace it")
		default:
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(postMergeScript(command)), 0o755); err != nil { //nolint:gosec // a hook must be executable
				return err
			}
			_, _ = fmt.Fprintf(out, "wrote %s\n", esc(path))
		}
	default:
		_, _ = fmt.Fprintf(out, "git runs %s after each pull that merges; it would hold:\n\n%saries setup post-merge --write  writes it\n", esc(path), postMergeScript(command))
	}
	return nil
}

// hasLine reports whether script has line as one of its lines.
func hasLine(script []byte, line string) bool {
	for l := range strings.SplitSeq(string(script), "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// onlyOurs reports whether script holds nothing but the shebang,
// comments, blank lines and line.
func onlyOurs(script []byte, line string) bool {
	for l := range strings.SplitSeq(string(script), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") && l != line {
			return false
		}
	}
	return true
}
