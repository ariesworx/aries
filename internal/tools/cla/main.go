// Command cla is the contributor license agreement check (CLA.md, design
// decision 84), run by .github/workflows/cla.yml. It is not part of aries
// or ariesd, and it replaced a third-party action that was no longer
// maintained.
//
//	cla [--allow LIST] [--branch NAME] [--file PATH] [--document URL]
//
// It reads the event GitHub Actions gives it (GITHUB_EVENT_NAME and
// GITHUB_EVENT_PATH) and talks to GitHub's API with GITHUB_TOKEN, nothing
// else. It never reads a pull request's code: the workflow runs it from
// the base branch.
//
// On a pull request (pull_request_target, opened, reopened or
// synchronize) it lists everyone who must have signed: the pull request's
// author and the GitHub account of every commit's author, less the
// allowed. It passes when each has signed, and otherwise fails, with one
// comment on the pull request, kept up to date, that says who must sign
// and how. A commit whose author email belongs to no GitHub account
// cannot be signed for, so it fails until the commit is rewritten.
//
// On a comment (issue_comment, created) on a pull request, the exact
// sentence of CLA.md section 13 from someone who must sign records their
// signature in FILE on BRANCH, as the signatures recorded before this
// command were; then, as `recheck` does alone, it reruns the pull
// request's latest check, which then reads the signature.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Sentence is what a contributor comments to sign (CLA.md, section 13).
const Sentence = "I have read the CLA Document and I hereby sign the CLA"

// marker starts the one comment this command keeps on a pull request.
const marker = "<!-- aries-cla -->"

// workflowPath is the workflow whose runs a signature reruns.
const workflowPath = ".github/workflows/cla.yml"

// env is what run needs from the process, so tests can supply their own.
type env struct {
	getenv         func(string) string
	readFile       func(string) ([]byte, error)
	client         *http.Client
	stdout, stderr io.Writer
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], env{
		getenv: os.Getenv, readFile: os.ReadFile,
		client: &http.Client{Timeout: 30 * time.Second},
		stdout: os.Stdout, stderr: os.Stderr,
	}))
}

// config is the command line.
type config struct {
	allow    []string
	branch   string
	file     string
	document string
}

// person is a GitHub account.
type person struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

// event is the part of a pull_request_target or issue_comment event read.
type event struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest *struct {
		Number int    `json:"number"`
		User   person `json:"user"`
		Head   struct {
			SHA string `json:"sha"`
		} `json:"head"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Comment *struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      person `json:"user"`
	} `json:"comment"`
	Repository struct {
		ID int64 `json:"id"`
	} `json:"repository"`
}

// signature is one entry of the signatures file, in the shape the action
// before this command wrote, so that no one signs twice.
type signature struct {
	Name          string `json:"name"`
	ID            int64  `json:"id"`
	CommentID     int64  `json:"comment_id"`
	CreatedAt     string `json:"created_at"`
	RepoID        int64  `json:"repoId"`
	PullRequestNo int    `json:"pullRequestNo"`
}

// signatures is the signatures file.
type signatures struct {
	SignedContributors []signature `json:"signedContributors"`
}

// errUnsigned fails the check without being a failure of the command.
var errUnsigned = errors.New("unsigned")

func run(ctx context.Context, args []string, e env) int {
	fs := flag.NewFlagSet("cla", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	allow := fs.String("allow", "", "comma-separated accounts that need not sign; *SUFFIX matches by suffix, as *[bot]")
	branch := fs.String("branch", "cla-signatures", "the branch that holds the signatures file")
	file := fs.String("file", "signatures/cla.json", "the signatures file")
	document := fs.String("document", "", "the agreement's URL, linked from the comment")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *document == "" {
		_, _ = fmt.Fprintln(e.stderr, "usage: cla --document URL [--allow LIST] [--branch NAME] [--file PATH]")
		return 2
	}
	cfg := config{branch: *branch, file: *file, document: *document}
	for a := range strings.SplitSeq(*allow, ",") {
		if a = strings.TrimSpace(a); a != "" {
			cfg.allow = append(cfg.allow, a)
		}
	}
	gh, err := newGitHub(e)
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "::error::%v\n", err)
		return 1
	}
	raw, err := e.readFile(e.getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		_, _ = fmt.Fprintf(e.stderr, "::error::read the event: %v\n", err)
		return 1
	}
	var ev event
	if err := json.Unmarshal(raw, &ev); err != nil {
		_, _ = fmt.Fprintf(e.stderr, "::error::read the event: %v\n", err)
		return 1
	}
	switch name := e.getenv("GITHUB_EVENT_NAME"); name {
	case "pull_request_target":
		err = check(ctx, gh, cfg, ev, e.stdout)
	case "issue_comment":
		err = comment(ctx, gh, cfg, ev, e.stdout)
	default:
		_, _ = fmt.Fprintf(e.stdout, "nothing to do for %s\n", name)
	}
	switch {
	case errors.Is(err, errUnsigned):
		return 1
	case err != nil:
		_, _ = fmt.Fprintf(e.stderr, "::error::%v\n", err)
		return 1
	}
	return 0
}

// check passes a pull request whose contributors have all signed.
func check(ctx context.Context, gh *github, cfg config, ev event, out io.Writer) error {
	if ev.PullRequest == nil {
		return errors.New("the event names no pull request")
	}
	switch ev.Action {
	case "opened", "reopened", "synchronize":
	default:
		_, _ = fmt.Fprintf(out, "nothing to check when a pull request is %s\n", ev.Action)
		return nil
	}
	n := ev.PullRequest.Number
	must, unlinked, err := gh.contributors(ctx, n, ev.PullRequest.User, cfg.allow)
	if err != nil {
		return err
	}
	signed, _, err := gh.signatures(ctx, cfg)
	if err != nil {
		return err
	}
	var unsigned []person
	for _, p := range must {
		if !hasSigned(signed, p) {
			unsigned = append(unsigned, p)
		}
	}
	if len(unsigned) == 0 && len(unlinked) == 0 {
		_, _ = fmt.Fprintln(out, "every contributor has signed the CLA")
		return gh.note(ctx, n, marker+"\nEvery contributor to this pull request has signed the [Contributor License Agreement]("+cfg.document+"). Thank you.", false)
	}
	if err := gh.note(ctx, n, request(cfg, unsigned, unlinked), true); err != nil {
		return err
	}
	var who []string
	for _, p := range unsigned {
		who = append(who, "@"+p.Login)
	}
	who = append(who, unlinked...)
	_, _ = fmt.Fprintf(out, "::error::the CLA is not yet signed by %s; the pull request's comment says how\n", strings.Join(who, ", "))
	return errUnsigned
}

// comment records a signature, or reruns the check for `recheck`.
func comment(ctx context.Context, gh *github, cfg config, ev event, out io.Writer) error {
	if ev.Issue == nil || ev.Issue.PullRequest == nil || ev.Comment == nil || ev.Action != "created" {
		_, _ = fmt.Fprintln(out, "not a new comment on a pull request")
		return nil
	}
	n := ev.Issue.Number
	body := strings.TrimSpace(ev.Comment.Body)
	if body != Sentence && body != "recheck" {
		_, _ = fmt.Fprintln(out, "neither the signing sentence nor recheck")
		return nil
	}
	pr, err := gh.pull(ctx, n)
	if err != nil {
		return err
	}
	if body == Sentence {
		must, _, err := gh.contributors(ctx, n, pr.User, cfg.allow)
		if err != nil {
			return err
		}
		who := ev.Comment.User
		if !slices.ContainsFunc(must, func(p person) bool { return p.ID == who.ID }) {
			_, _ = fmt.Fprintf(out, "@%s need not sign for pull request %d; nothing recorded\n", who.Login, n)
			return nil
		}
		s := signature{Name: who.Login, ID: who.ID, CommentID: ev.Comment.ID, CreatedAt: ev.Comment.CreatedAt,
			RepoID: ev.Repository.ID, PullRequestNo: n}
		if err := gh.record(ctx, cfg, s, out); err != nil {
			return err
		}
	}
	return gh.rerun(ctx, pr.Head.SHA, out)
}

// request is the comment that asks for signatures.
func request(cfg config, unsigned []person, unlinked []string) string {
	var b strings.Builder
	b.WriteString(marker + "\n")
	b.WriteString("Thank you for your pull request. Before it can be merged, each person whose commits are in it signs the [Contributor License Agreement](" + cfg.document + ") once.\n")
	if len(unsigned) > 0 {
		b.WriteString("\nTo sign, read the agreement, then comment on this pull request, exactly:\n\n> " + Sentence + "\n\nStill to sign:")
		for _, p := range unsigned {
			b.WriteString(" @" + p.Login)
		}
		b.WriteString("\n")
	}
	if len(unlinked) > 0 {
		b.WriteString("\nSome commits name an author whose email belongs to no GitHub account, who therefore cannot sign: " +
			strings.Join(unlinked, ", ") + ". Add the email to your GitHub account, or rewrite the commits with an email it has, and push again.\n")
	}
	b.WriteString("\nThe check runs again by itself once you sign. To run it again by hand, comment `recheck`.")
	return b.String()
}

func hasSigned(s signatures, p person) bool {
	return slices.ContainsFunc(s.SignedContributors, func(c signature) bool { return c.ID == p.ID })
}

// allowed says whether login need not sign.
func allowed(login string, allow []string) bool {
	for _, a := range allow {
		if suffix, ok := strings.CutPrefix(a, "*"); ok && strings.HasSuffix(login, suffix) || a == login {
			return true
		}
	}
	return false
}

// github is GitHub's REST API for one repository.
type github struct {
	client    *http.Client
	api, repo string
	token     string
}

func newGitHub(e env) (*github, error) {
	g := &github{client: e.client, api: strings.TrimSuffix(e.getenv("GITHUB_API_URL"), "/"),
		repo: e.getenv("GITHUB_REPOSITORY"), token: e.getenv("GITHUB_TOKEN")}
	if g.api == "" {
		g.api = "https://api.github.com"
	}
	if g.repo == "" || g.token == "" {
		return nil, errors.New("GITHUB_REPOSITORY and GITHUB_TOKEN must be set, as GitHub Actions sets them")
	}
	return g, nil
}

// statusError is an answer GitHub gave with a status it did not want.
type statusError struct {
	method, path string
	status       int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GitHub answered %s %s with %d", e.method, e.path, e.status)
}

// do sends a request to the repository's path and decodes the answer into
// out, which may be nil.
func (g *github) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api+"/repos/"+g.repo+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return &statusError{method: method, path: path, status: resp.StatusCode}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

// pr is the part of a pull request read.
type pr struct {
	User person `json:"user"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

func (g *github) pull(ctx context.Context, n int) (pr, error) {
	var p pr
	err := g.do(ctx, http.MethodGet, "/pulls/"+strconv.Itoa(n), nil, &p)
	return p, err
}

// contributors lists who must sign for pull request n: its author and each
// commit's author, less the allowed, once each; and, as "name <email>",
// the commit authors with no GitHub account. GitHub lists at most 250 of
// a pull request's commits.
func (g *github) contributors(ctx context.Context, n int, author person, allow []string) (must []person, unlinked []string, err error) {
	add := func(p person) {
		if !allowed(p.Login, allow) && !slices.ContainsFunc(must, func(q person) bool { return q.ID == p.ID }) {
			must = append(must, p)
		}
	}
	add(author)
	for page := 1; page <= 3; page++ {
		var commits []struct {
			Author *person `json:"author"`
			Commit struct {
				Author struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"author"`
			} `json:"commit"`
		}
		path := fmt.Sprintf("/pulls/%d/commits?per_page=100&page=%d", n, page)
		if err := g.do(ctx, http.MethodGet, path, nil, &commits); err != nil {
			return nil, nil, err
		}
		for _, c := range commits {
			if c.Author == nil || c.Author.ID == 0 {
				u := fmt.Sprintf("%s <%s>", c.Commit.Author.Name, c.Commit.Author.Email)
				if !slices.Contains(unlinked, u) {
					unlinked = append(unlinked, u)
				}
				continue
			}
			add(*c.Author)
		}
		if len(commits) < 100 {
			break
		}
	}
	return must, unlinked, nil
}

// signatures reads the signatures file and the blob sha that replacing it
// names. A file not there yet is no signatures, and sha "".
func (g *github) signatures(ctx context.Context, cfg config) (signatures, string, error) {
	var f struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	path := "/contents/" + cfg.file + "?ref=" + url.QueryEscape(cfg.branch)
	var se *statusError
	if err := g.do(ctx, http.MethodGet, path, nil, &f); errors.As(err, &se) && se.status == http.StatusNotFound {
		return signatures{}, "", nil
	} else if err != nil {
		return signatures{}, "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil {
		return signatures{}, "", fmt.Errorf("decode %s: %w", cfg.file, err)
	}
	var s signatures
	if err := json.Unmarshal(raw, &s); err != nil {
		return signatures{}, "", fmt.Errorf("read %s: %w", cfg.file, err)
	}
	return s, f.SHA, nil
}

// record adds s to the signatures file, unless its account has signed,
// retrying when another signature was written between the read and the
// write.
func (g *github) record(ctx context.Context, cfg config, s signature, out io.Writer) error {
	for attempt := 1; ; attempt++ {
		cur, sha, err := g.signatures(ctx, cfg)
		if err != nil {
			return err
		}
		if hasSigned(cur, person{ID: s.ID}) {
			_, _ = fmt.Fprintf(out, "@%s has signed already\n", s.Name)
			return nil
		}
		cur.SignedContributors = append(cur.SignedContributors, s)
		b, err := json.MarshalIndent(cur, "", "   ")
		if err != nil {
			return err
		}
		put := map[string]string{
			"message": fmt.Sprintf("@%s has signed the CLA in #%d", s.Name, s.PullRequestNo),
			"content": base64.StdEncoding.EncodeToString(append(b, '\n')),
			"branch":  cfg.branch,
		}
		if sha != "" {
			put["sha"] = sha
		}
		var se *statusError
		err = g.do(ctx, http.MethodPut, "/contents/"+cfg.file, put, nil)
		if errors.As(err, &se) && (se.status == http.StatusConflict || se.status == http.StatusUnprocessableEntity) && attempt < 5 {
			continue
		}
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "recorded @%s's signature\n", s.Name)
		return nil
	}
}

// note keeps this command's one comment on pull request n: it edits the
// comment there, or adds one when create is set.
func (g *github) note(ctx context.Context, n int, body string, create bool) error {
	for page := 1; page <= 10; page++ {
		var cs []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
			User struct {
				Type string `json:"type"`
			} `json:"user"`
		}
		path := fmt.Sprintf("/issues/%d/comments?per_page=100&page=%d", n, page)
		if err := g.do(ctx, http.MethodGet, path, nil, &cs); err != nil {
			return err
		}
		for _, c := range cs {
			if c.User.Type == "Bot" && strings.HasPrefix(c.Body, marker) {
				if c.Body == body {
					return nil
				}
				return g.do(ctx, http.MethodPatch, fmt.Sprintf("/issues/comments/%d", c.ID), map[string]string{"body": body}, nil)
			}
		}
		if len(cs) < 100 {
			break
		}
	}
	if !create {
		return nil
	}
	return g.do(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", n), map[string]string{"body": body}, nil)
}

// rerun reruns the latest check of commit sha, which reads the signatures
// afresh. A check still running is left to finish.
func (g *github) rerun(ctx context.Context, sha string, out io.Writer) error {
	var runs struct {
		WorkflowRuns []struct {
			ID     int64  `json:"id"`
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"workflow_runs"`
	}
	path := "/actions/runs?event=pull_request_target&per_page=100&head_sha=" + url.QueryEscape(sha)
	if err := g.do(ctx, http.MethodGet, path, nil, &runs); err != nil {
		return err
	}
	for _, r := range runs.WorkflowRuns { // newest first
		if r.Path != workflowPath && !strings.HasPrefix(r.Path, workflowPath+"@") {
			continue
		}
		if r.Status != "completed" {
			_, _ = fmt.Fprintf(out, "the check of %s is still running, and reads the signatures when it does\n", sha)
			return nil
		}
		if err := g.do(ctx, http.MethodPost, fmt.Sprintf("/actions/runs/%d/rerun", r.ID), nil, nil); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "reran the check of %s\n", sha)
		return nil
	}
	_, _ = fmt.Fprintf(out, "no check of %s to rerun; push, or comment recheck once it has run\n", sha)
	return nil
}
