package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/clienttest"
)

// TestMain keeps the git these tests start from the configuration of
// whoever runs them (clienttest), for the whole process.
func TestMain(m *testing.M) {
	restore := clienttest.IsolateGit()
	code := m.Run()
	restore()
	os.Exit(code)
}

func msg(subject string, trailers ...string) string {
	if len(trailers) == 0 {
		return subject + "\n"
	}
	return subject + "\n\nWhy it changed.\n\n" + strings.Join(trailers, "\n") + "\n"
}

// prEvent returns a pull_request event with the given labels.
func prEvent(labels ...string) []byte {
	var ls []map[string]string
	for _, l := range labels {
		ls = append(ls, map[string]string{"name": l})
	}
	b, _ := json.Marshal(map[string]any{"pull_request": map[string]any{
		"base": map[string]string{"sha": "base0000"}, "head": map[string]string{"sha": "head0000"}, "labels": ls}})
	return b
}

// TestRun runs the command against commits it is given.
func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		event   []byte
		eventNm string // GITHUB_EVENT_NAME; "" is pull_request
		commits []commit
		err     error
		args    []string
		code    int
		stdout  string
		stderr  string
	}{
		{name: "no trailer fails", event: prEvent(), code: 1,
			commits: []commit{{SHA: "a1", Message: msg("feature: x")}, {SHA: "a2", Message: msg("fix: y")}},
			stderr:  "none of the pull request's 2 commits has an `Aries: ID` trailer"},
		{name: "the failure names the fix", event: prEvent(), code: 1,
			commits: []commit{{SHA: "a1", Message: msg("feature: x")}},
			stderr:  "git commit --allow-empty -m \"Name the issue\" --trailer \"Aries: ID\"`), and push; the squash keeps it"},
		{name: "the failure names the exemption", event: prEvent(), code: 1,
			commits: []commit{{SHA: "a1", Message: msg("feature: x")}},
			stderr:  "takes the label `no-issue`"},
		{name: "one commit with it passes", event: prEvent(), code: 0,
			commits: []commit{{SHA: "a1", Message: msg("feature: x")}, {SHA: "a2", Message: msg("fix: y", "Aries: aries-4s6jxn44", "Co-Authored-By: A <a@example.com>")}},
			stdout:  "ok: 1 of 2 commits name aries-4s6jxn44"},
		{name: "every commit with the same one passes", event: prEvent(), code: 0,
			commits: []commit{{SHA: "a1", Message: msg("a", "Aries: ar-a1b2c3d4")}, {SHA: "a2", Message: msg("b", "Aries: ar-a1b2c3d4")}},
			stdout:  "ok: 2 of 2 commits name ar-a1b2c3d4"},
		{name: "two different issues fail", event: prEvent(), code: 1,
			commits: []commit{{SHA: "a1b2c3d4e5f6a7b8", Message: msg("a", "Aries: ar-a1b2c3d4")}, {SHA: "c2", Message: msg("b", "Aries: ar-e5f6a7b8")}},
			stderr:  "a1b2c3d4e5f6  Aries: ar-a1b2c3d4\n  c2  Aries: ar-e5f6a7b8"},
		{name: "a value that is no issue id is no trailer", event: prEvent(), code: 1,
			commits: []commit{{SHA: "a1", Message: msg("a", "Aries: not an id!")}},
			stderr:  "has an `Aries: ID` trailer"},
		{name: "a merge line in a message names no issue", event: prEvent(), code: 1,
			commits: []commit{{SHA: "a1", Message: "Merge pull request #12 from example/feature/ar-a1b2c3d4-x\n"}},
			stderr:  "has an `Aries: ID` trailer"},
		{name: "the label exempts", event: prEvent("bug", "no-issue"), code: 0,
			commits: []commit{{SHA: "a1", Message: msg("feature: x")}},
			stdout:  "exempt: the pull request has the label \"no-issue\""},
		{name: "another label does not", event: prEvent("bug", "no-issues"), code: 1,
			commits: []commit{{SHA: "a1", Message: msg("feature: x")}},
			stderr:  "has an `Aries: ID` trailer"},
		{name: "the label is a setting", event: prEvent("docs-only"), args: []string{"--exempt-label", "docs-only"}, code: 0,
			commits: []commit{{SHA: "a1", Message: msg("feature: x")}},
			stdout:  "exempt"},
		{name: "no commits fails", event: prEvent(), code: 1, stderr: "no commits of its own"},
		{name: "git fails", event: prEvent(), code: 1, err: errors.New("bad object"), stderr: "read the commits: bad object"},
		{name: "not a pull request", event: prEvent(), eventNm: "merge_group", code: 2, stderr: `runs on pull_request events, not "merge_group"`},
		{name: "no pull request in the event", event: []byte(`{}`), code: 1, stderr: "not a pull_request event"},
		{name: "a bad flag", event: prEvent(), args: []string{"--nope"}, code: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errb bytes.Buffer
			name := tc.eventNm
			if name == "" {
				name = "pull_request"
			}
			e := env{
				getenv: func(k string) string {
					return map[string]string{"GITHUB_EVENT_NAME": name, "GITHUB_EVENT_PATH": "event.json"}[k]
				},
				readFile: func(p string) ([]byte, error) {
					if p != "event.json" {
						return nil, os.ErrNotExist
					}
					return tc.event, nil
				},
				commits: func(_ context.Context, _, base, head string) ([]commit, error) {
					if base != "base0000" || head != "head0000" {
						t.Errorf("commits read from %s to %s; want the event's base and head", base, head)
					}
					return tc.commits, tc.err
				},
				stdout: &out, stderr: &errb,
			}
			if code := run(t.Context(), tc.args, e); code != tc.code {
				t.Errorf("exit %d, want %d\nstdout %q\nstderr %q", code, tc.code, out.String(), errb.String())
			}
			if !strings.Contains(out.String(), tc.stdout) || !strings.Contains(errb.String(), tc.stderr) {
				t.Errorf("stdout %q, stderr %q; want them to contain %q and %q", out.String(), errb.String(), tc.stdout, tc.stderr)
			}
			if tc.code == 1 && tc.err == nil && len(tc.commits) > 0 && !strings.Contains(errb.String(), "fix: ") {
				t.Errorf("a failure without a fix: %q", errb.String())
			}
		})
	}
}

// gitIn runs git in dir and returns its output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...) //nolint:gosec // fixed binary, explicit argv, no shell
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGitCommits reads a real pull request: the commits from main's tip to
// the branch's head, oldest first, without the merge of main into the
// branch and without the commits that merge brought in.
func TestGitCommits(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "root")
	gitIn(t, dir, "checkout", "-q", "-b", "feature/x")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "first", "-m", "Why.", "--trailer", "Aries: ar-a1b2c3d4")
	gitIn(t, dir, "checkout", "-q", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "someone else's work", "--trailer", "Aries: ar-ffffffff")
	base := gitIn(t, dir, "rev-parse", "HEAD")
	gitIn(t, dir, "checkout", "-q", "feature/x")
	gitIn(t, dir, "merge", "-q", "--no-ff", "-m", "Merge branch 'main' into feature/x", "main")
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "second")
	head := gitIn(t, dir, "rev-parse", "HEAD")

	got, err := gitCommits(t.Context(), dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, c := range got {
		subjects = append(subjects, c.Subject)
	}
	if strings.Join(subjects, "|") != "first|second" {
		t.Fatalf("commits = %v; want first and second, with neither the merge of main nor main's own commit", subjects)
	}
	if msg, ok := check(got, "no-issue"); !ok || !strings.Contains(msg, "ar-a1b2c3d4") {
		t.Errorf("check of the real commits = %q, %v; want it passing on the branch's own trailer", msg, ok)
	}

	if _, err := gitCommits(t.Context(), dir, "0000000000000000000000000000000000000001", head); err == nil ||
		!strings.Contains(err.Error(), "git log") {
		t.Errorf("gitCommits of an unknown base = %v; want an error naming git log", err)
	}
}
