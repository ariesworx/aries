package merges

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// Forge reads a CI host's open pull requests: their fields and changed
// paths, their required checks on the head commit, and their diff.
// prs-check.md §5's internal/forge, when it exists, takes this over.
type Forge interface {
	Open(ctx context.Context) ([]PR, error)
	Required(ctx context.Context, n int) ([]Check, error)
	Diff(ctx context.Context, n int) ([]Change, error)
}

// Run runs a command in dir and returns its standard output, and an
// error naming the first line of its standard error when it fails; the
// output is returned either way.
type Run func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

// GH is the Forge over the gh on PATH, signed in as whoever gh is, in the
// repository Dir. It runs `gh pr list`, `gh pr checks` and `gh pr diff`
// alone, never `gh api` (decision 67: the client reads the host).
type GH struct {
	Dir string
	// Run runs gh; nil runs the real one. Tests replace it.
	Run Run
}

// MaxOpen is the most open pull requests one run lists.
const MaxOpen = 200

func (g *GH) gh(ctx context.Context, args ...string) ([]byte, error) {
	run := g.Run
	if run == nil {
		run = runCommand
	}
	return run(ctx, g.Dir, "gh", args...)
}

// Open lists the open pull requests, oldest first.
func (g *GH) Open(ctx context.Context) ([]PR, error) {
	out, err := g.gh(ctx, "pr", "list", "--state", "open", "--limit", strconv.Itoa(MaxOpen),
		"--json", "number,url,title,baseRefName,headRefName,headRefOid,isDraft,files")
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []struct {
		Number int    `json:"number"`
		URL    string `json:"url"`
		Title  string `json:"title"`
		Base   string `json:"baseRefName"`
		Branch string `json:"headRefName"`
		Head   string `json:"headRefOid"`
		Draft  bool   `json:"isDraft"`
		Files  []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("gh pr list printed what is not its JSON: %w", err)
	}
	res := make([]PR, 0, len(prs))
	for _, p := range prs {
		pr := PR{Number: p.Number, URL: p.URL, Title: p.Title, Base: p.Base, Branch: p.Branch, Head: p.Head, Draft: p.Draft}
		for _, f := range p.Files {
			pr.Files = append(pr.Files, Change{Path: f.Path})
		}
		res = append(res, pr)
	}
	slices.SortFunc(res, func(a, b PR) int { return a.Number - b.Number })
	return res, nil
}

// Required reads pull request n's required checks on its head commit.
// gh exits non-zero when one failed or is pending, and prints them all
// the same; a pull request with none is an empty list.
func (g *GH) Required(ctx context.Context, n int) ([]Check, error) {
	out, err := g.gh(ctx, "pr", "checks", strconv.Itoa(n), "--required", "--json", "name,bucket")
	var cs []Check
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &cs); jerr == nil && len(bytes.TrimSpace(out)) > 0 {
		return cs, nil
	}
	if err != nil && strings.Contains(err.Error(), "no required checks reported") {
		return nil, nil
	}
	if err == nil {
		err = errors.New("printed what is not its JSON")
	}
	return nil, fmt.Errorf("gh pr checks: %w", err)
}

// Diff reads pull request n's diff against its base.
func (g *GH) Diff(ctx context.Context, n int) ([]Change, error) {
	out, err := g.gh(ctx, "pr", "diff", strconv.Itoa(n), "--color", "never")
	if err != nil {
		return nil, fmt.Errorf("gh pr diff: %w", err)
	}
	cs, err := ParseDiff(out)
	if err != nil {
		return nil, fmt.Errorf("gh pr diff: %w", err)
	}
	return cs, nil
}

// Read reads one open pull request in full: its required checks and its
// diff, whose paths it joins to those the list gave, so that a path
// either names is classified. What it cannot read it records on the
// pull request, for the classifier to hold on.
func Read(ctx context.Context, f Forge, pr PR) PR {
	cs, err := f.Required(ctx, pr.Number)
	if err != nil {
		pr.ChecksErr = err.Error()
	}
	pr.Checks = cs
	listed := pr.Files
	diff, err := f.Diff(ctx, pr.Number)
	if err != nil {
		pr.FilesErr = err.Error()
		return pr
	}
	pr.Files = diff
	known := paths(diff)
	for _, c := range listed {
		if !slices.Contains(known, c.Path) {
			pr.Files = append(pr.Files, c)
		}
	}
	return pr
}

func runCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed binary, explicit argv, no shell
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			first, _, _ := strings.Cut(msg, "\n")
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, first)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}
