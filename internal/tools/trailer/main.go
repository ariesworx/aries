// Command trailer is the check that a pull request names its Aries issue,
// run by .github/workflows/trailer.yml. It is not part of aries or ariesd.
//
//	trailer [--exempt-label NAME] [--dir DIR]
//
// main takes squash merges whose message is the pull request's commit
// messages, so a squash has no branch for `aries land` to read and names
// its issue only through an `Aries: ID` trailer in one of those commits.
// trailer reads the pull_request event GitHub Actions gives it
// (GITHUB_EVENT_NAME and GITHUB_EVENT_PATH) and the commits from the
// event's base to its head in the checkout at DIR (full history), without
// the merge commits, and reads each by internal/gitx, as `aries land`
// does. It fails unless at least one commit carries a trailer naming an
// issue and every trailer names the same one.
//
// A pull request with the label NAME (default no-issue) is exempt, said on
// the output: only someone with write access can add a label. It talks to
// nothing but git, and never to GitHub's API.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/ariesworx/aries/internal/gitx"
)

// env is what run needs from the process, so tests can supply their own.
type env struct {
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	// commits returns the message of each commit from base to head in dir,
	// merge commits left out, oldest first.
	commits        func(ctx context.Context, dir, base, head string) ([]commit, error)
	stdout, stderr io.Writer
}

// commit is one commit of the pull request.
type commit struct {
	SHA     string
	Subject string
	Message string
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], env{
		getenv: os.Getenv, readFile: os.ReadFile, commits: gitCommits,
		stdout: os.Stdout, stderr: os.Stderr,
	}))
}

// event is the part of a pull_request event read.
type event struct {
	PullRequest *struct {
		Base   struct{ SHA string } `json:"base"`
		Head   struct{ SHA string } `json:"head"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	} `json:"pull_request"`
}

func run(ctx context.Context, args []string, e env) int {
	fs := flag.NewFlagSet("trailer", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	label := fs.String("exempt-label", "no-issue", "the label that exempts a pull request from the check")
	dir := fs.String("dir", ".", "a checkout of the pull request with its full history")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *label == "" {
		_, _ = fmt.Fprintln(e.stderr, "usage: trailer [--exempt-label NAME] [--dir DIR]")
		return 2
	}
	if name := e.getenv("GITHUB_EVENT_NAME"); name != "pull_request" {
		_, _ = fmt.Fprintf(e.stderr, "trailer: runs on pull_request events, not %q\n", name)
		return 2
	}
	raw, err := e.readFile(e.getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "trailer: read the event: %v\n", err)
		return 1
	}
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil || ev.PullRequest == nil ||
		ev.PullRequest.Base.SHA == "" || ev.PullRequest.Head.SHA == "" {
		_, _ = fmt.Fprintln(e.stderr, "trailer: the event is not a pull_request event with a base and a head")
		return 1
	}
	pr := ev.PullRequest
	for _, l := range pr.Labels {
		if l.Name == *label {
			_, _ = fmt.Fprintf(e.stdout, "trailer: exempt: the pull request has the label %q, which only someone with write access can add\n", *label)
			return 0
		}
	}
	commits, err := e.commits(ctx, *dir, pr.Base.SHA, pr.Head.SHA)
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "trailer: read the commits: %v\n", err)
		return 1
	}
	msg, ok := check(commits, *label)
	if !ok {
		_, _ = fmt.Fprintln(e.stderr, msg)
		return 1
	}
	_, _ = fmt.Fprintln(e.stdout, msg)
	return 0
}

// check reads commits, and returns what to say and whether they pass.
func check(commits []commit, label string) (string, bool) {
	var ids []string
	var named []string // the commit of each trailer, aligned with ids
	for _, c := range commits {
		id, _, ok := gitx.IssueOfMessage(c.Message)
		if !ok || id == "" {
			continue
		}
		ids = append(ids, id)
		named = append(named, short(c.SHA))
	}
	distinct := slices.Compact(slices.Sorted(slices.Values(ids)))
	switch {
	case len(commits) == 0:
		return "trailer: the pull request has no commits of its own", false
	case len(distinct) == 0:
		return "trailer: none of the pull request's " + count(len(commits), "commit") + " has an `Aries: ID` trailer.\n" +
			"A squash merge has no branch name for `aries land` to read, so the pull request must name its issue.\n" +
			"fix: amend a commit to end with the trailer (`git commit --amend --trailer \"Aries: ID\"`), or add an empty one\n" +
			"(`git commit --allow-empty -m \"Name the issue\" --trailer \"Aries: ID\"`), and push; the squash keeps it.\n" +
			"A pull request with no Aries issue takes the label `" + label + "`, which someone with write access adds.", false
	case len(distinct) > 1:
		var b strings.Builder
		b.WriteString("trailer: the pull request's commits name different issues:\n")
		for i, id := range ids {
			fmt.Fprintf(&b, "  %s  Aries: %s\n", named[i], id)
		}
		b.WriteString("fix: a pull request closes one issue; reword the commits (or split the pull request) so that every `Aries:` trailer names the same one.")
		return b.String(), false
	}
	return fmt.Sprintf("trailer: ok: %d of %s name %s", len(ids), count(len(commits), "commit"), distinct[0]), true
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// gitCommits is env.commits over git: the commits reachable from head and
// not from base, without merge commits (a merge of main into the branch
// is not the author's), oldest first.
func gitCommits(ctx context.Context, dir, base, head string) ([]commit, error) {
	const sep, fieldSep = "\x1e", "\x1f"
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log", "--no-merges", "--reverse", //nolint:gosec // fixed binary, explicit argv, no shell
		"--format=%H"+fieldSep+"%s"+fieldSep+"%B"+sep, base+".."+head)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := errors.AsType[*exec.ExitError](err); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git log %s..%s: %s", short(base), short(head), strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("git log %s..%s: %w", short(base), short(head), err)
	}
	var commits []commit
	for rec := range strings.SplitSeq(string(out), sep) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, fieldSep, 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("git log: unexpected record %q", rec)
		}
		commits = append(commits, commit{SHA: f[0], Subject: f[1], Message: f[2]})
	}
	return commits, nil
}
