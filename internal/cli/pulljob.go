package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
)

const pullJobUsage = "pull-job --run [--branch NAME] [--remote NAME] [--key-file PATH]"

// pullJobLog is the file in the data directory that holds one line for
// each run of the job, and pullJobLogMax bounds it: past it, the oldest
// half is dropped.
const (
	pullJobLog    = "pull-job.log"
	pullJobLogMax = 128 << 10
)

// pullJobEnv are the variables that name another session than the
// person's own: the job clears them, so that it reports from the session
// `cli`, as `aries land` does by itself.
var pullJobEnv = map[string]bool{"ARIES_SESSION": true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_SESSION_ID": true, "CI": true}

// cmdPullJob is what the job `aries setup pull-job` schedules runs: on the
// default branch and with no change to a tracked file, it fetches and
// fast-forwards (git's post-merge hook runs as for a pull), and then, on
// every run, reports landed for the commits not yet reported, so that a
// checkout moved by something else than the job is still landed. It is a
// person's act, from the session cli, with the person's key; it never
// merges, rebases or resets, and writes one line to the log in the data
// directory for each run. CLI only (rule 12).
func cmdPullJob(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("pull-job")
	run := fs.Bool("run", false, "do one run: pull, then report landed")
	branch := fs.String("branch", "", "the default branch's name, when git cannot tell")
	remote := fs.String("remote", "origin", "the remote to fetch the default branch from")
	keyFile := fs.String("key-file", "", "the key file the job signs in with (ARIES_KEY_FILE)")
	pos, err := parse(fs, args, pullJobUsage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(pullJobUsage, "pull-job takes no arguments")
	}
	if !*run {
		return usagef(pullJobUsage, "pull-job is the job's own command: give --run to do one run, or `aries setup pull-job` to schedule it")
	}
	get := r.env.Getenv
	r.env.Getenv = func(k string) string {
		switch {
		case pullJobEnv[k]:
			return ""
		case k == client.EnvKeyFile && *keyFile != "":
			return *keyFile
		}
		return get(k)
	}
	r.session = client.CLISession

	line, err := r.pullJob(ctx, *branch, *remote)
	if err != nil {
		line = "failed: " + esc(err.Error())
	}
	r.logPullJob(line)
	if r.json && err == nil {
		r.emit(struct {
			Result string `json:"result"`
		}{line})
	} else if err == nil {
		_, _ = fmt.Fprintln(r.env.Stdout, line)
	}
	return err
}

// pullJob does one run and returns the line that says what it did.
func (r *runner) pullJob(ctx context.Context, branch, remote string) (string, error) {
	cur, err := gitx.CurrentBranch(ctx, r.dir)
	if err != nil {
		return "", proto.Errf(proto.CodeInvalid, "run it in a checkout of the project's repository", fmt.Sprintf("read the branch: %v", err))
	}
	def := branch
	if def == "" {
		if def, err = gitx.DefaultBranch(ctx, r.dir); err != nil {
			return "", proto.Errf(proto.CodeInvalid, "give it with --branch NAME", fmt.Sprintf("read the default branch: %v", err))
		}
	}
	switch {
	case def == "":
		return "", proto.Errf(proto.CodeInvalid, "give it with --branch NAME", "git does not say which branch is the default")
	case cur != def:
		return fmt.Sprintf("skipped: the checkout is on %s, not %s", branchText(cur), def), nil
	}
	var pulled string
	switch dirty, err := gitx.Dirty(ctx, r.dir); {
	case err != nil:
		return "", fmt.Errorf("read the checkout's state: %w", err)
	case dirty:
		pulled = "skipped the pull: the checkout has uncommitted changes"
	default:
		before, after, err := gitx.FastForward(ctx, r.dir, remote, def)
		switch {
		case errors.Is(err, gitx.ErrNotFastForward):
			pulled = "skipped the pull: " + def + " has commits that " + remote + " lacks"
		case err != nil:
			return "", proto.Errf(proto.CodeUnavailable, "check that git can fetch from "+remote+" without asking anything: its credentials, or ssh-agent, in the job's environment", err.Error())
		case before == after:
			pulled = "nothing to pull"
		default:
			pulled = "pulled " + short(before) + ".." + short(after)
		}
	}
	// Land on every run, not only after a pull of its own: the checkout
	// may have been moved by another program, and the post-merge hook's
	// land never ran. A rerun reports nothing twice.
	out, errb := r.env.Stdout, r.env.Stderr
	var buf bytes.Buffer
	r.env.Stdout, r.env.Stderr = &buf, &bytes.Buffer{}
	err = r.land(ctx, "", def, false, false)
	r.env.Stdout, r.env.Stderr = out, errb
	if err != nil {
		return "", fmt.Errorf("%s; land: %w", pulled, err)
	}
	n := 0
	for l := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(l, " landed") {
			n++
		}
	}
	return fmt.Sprintf("%s; landed %d", pulled, n), nil
}

// logPullJob appends line, dated, to the job's log in the data directory.
// A log that cannot be written is said on stderr and nothing more.
func (r *runner) logPullJob(line string) {
	data, err := offline.DataDir(r.env.GOOS, r.env.Getenv, r.env.UserHomeDir)
	if err == nil {
		err = appendBounded(filepath.Join(data, pullJobLog), r.env.Now().UTC().Format(time.RFC3339)+" "+line+"\n", pullJobLogMax)
	}
	if err != nil {
		r.notef("could not write the job's log: %s", esc(err.Error()))
	}
}

// appendBounded appends text to the file at path, creating it 0600 and its
// directory 0700, and keeps the newest half of it once it passes max.
func appendBounded(path, text string, limit int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := os.ReadFile(path) //nolint:gosec // the job's own log in the data directory
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b = append(b, text...)
	if len(b) > limit {
		b = b[len(b)/2:]
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return os.WriteFile(path, b, 0o600) //nolint:gosec // the job's own log in the data directory
}

// RunProgram runs name with args and waits, for Env.RunProgram: what
// `aries setup pull-job --write` starts a scheduler with. A failure
// carries the program's output.
func RunProgram(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // a fixed scheduler program with explicit argv, no shell
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
