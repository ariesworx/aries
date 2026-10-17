package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub is GitHub's API for one repository, ariesworx/aries, in
// memory: one pull request, its commits and comments, the signatures
// file, and the workflow runs.
type fakeGitHub struct {
	mu        sync.Mutex
	author    person
	commits   []map[string]any
	comments  []map[string]any
	file      *signatures // nil: not there yet
	sha       int         // the file's blob, bumped by every write
	conflicts int         // PUTs to answer 409 before one succeeds
	runs      []map[string]any
	reruns    []int64
	puts      int
	auth      string
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	const repo = "/repos/ariesworx/aries"
	path := strings.TrimPrefix(r.URL.Path, repo)
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodGet && path == "/pulls/7":
		write(map[string]any{"user": f.author, "head": map[string]any{"sha": "abc123"}})
	case r.Method == http.MethodGet && path == "/pulls/7/commits":
		if r.URL.Query().Get("page") != "1" {
			write([]any{})
			return
		}
		write(f.commits)
	case r.Method == http.MethodGet && path == "/contents/signatures/cla.json":
		if r.URL.Query().Get("ref") != "cla-signatures" {
			http.Error(w, "wrong branch", http.StatusBadRequest)
			return
		}
		if f.file == nil {
			http.NotFound(w, r)
			return
		}
		b, _ := json.Marshal(f.file)
		write(map[string]any{"content": base64.StdEncoding.EncodeToString(b), "sha": strconv.Itoa(f.sha)})
	case r.Method == http.MethodPut && path == "/contents/signatures/cla.json":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if f.conflicts > 0 {
			f.conflicts--
			f.sha++ // someone else wrote meanwhile
			http.Error(w, "sha does not match", http.StatusConflict)
			return
		}
		if (f.file != nil && in["sha"] != strconv.Itoa(f.sha)) || in["branch"] != "cla-signatures" {
			http.Error(w, "sha does not match", http.StatusConflict)
			return
		}
		raw, _ := base64.StdEncoding.DecodeString(in["content"])
		var s signatures
		if err := json.Unmarshal(raw, &s); err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		f.file, f.sha, f.puts = &s, f.sha+1, f.puts+1
		write(map[string]any{})
	case r.Method == http.MethodGet && path == "/issues/7/comments":
		write(f.comments)
	case r.Method == http.MethodPost && path == "/issues/7/comments":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.comments = append(f.comments, map[string]any{"id": len(f.comments) + 1, "body": in["body"], "user": map[string]any{"type": "Bot"}})
		write(map[string]any{})
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/issues/comments/"):
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/issues/comments/"))
		f.comments[id-1]["body"] = in["body"]
		write(map[string]any{})
	case r.Method == http.MethodGet && path == "/actions/runs":
		if r.URL.Query().Get("head_sha") != "abc123" || r.URL.Query().Get("event") != "pull_request_target" {
			write(map[string]any{"workflow_runs": []any{}})
			return
		}
		write(map[string]any{"workflow_runs": f.runs})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/actions/runs/") && strings.HasSuffix(path, "/rerun"):
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "/actions/runs/"), "/rerun"), 10, 64)
		f.reruns = append(f.reruns, id)
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusTeapot)
	}
}

var (
	alice = person{Login: "alice", ID: 101}
	bob   = person{Login: "bob", ID: 102}
	owner = person{Login: "edwardfward", ID: 1}
	bot   = person{Login: "dependabot[bot]", ID: 900}
)

func commitBy(p *person, name, email string) map[string]any {
	c := map[string]any{"commit": map[string]any{"author": map[string]any{"name": name, "email": email}}}
	if p != nil {
		c["author"] = p
	}
	return c
}

func signed(ps ...person) *signatures {
	s := &signatures{SignedContributors: []signature{}}
	for _, p := range ps {
		s.SignedContributors = append(s.SignedContributors, signature{Name: p.Login, ID: p.ID, CommentID: 1, CreatedAt: "2026-10-16T00:00:00Z", RepoID: 5, PullRequestNo: 1})
	}
	return s
}

// invoke runs the command on one event against f.
func invoke(t *testing.T, f *fakeGitHub, eventName string, ev map[string]any) (status int, stdout, stderr string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"GITHUB_API_URL": srv.URL, "GITHUB_REPOSITORY": "ariesworx/aries", "GITHUB_TOKEN": "t0ken",
		"GITHUB_EVENT_NAME": eventName, "GITHUB_EVENT_PATH": "/event.json",
	}
	var out, errb bytes.Buffer
	status = run(context.Background(), []string{"--allow", "edwardfward,*[bot]", "--document", "https://example.com/CLA.md"}, env{
		getenv: func(k string) string { return vars[k] },
		readFile: func(p string) ([]byte, error) {
			if p != "/event.json" {
				return nil, fmt.Errorf("unexpected file %s", p)
			}
			return raw, nil
		},
		client: srv.Client(), stdout: &out, stderr: &errb,
	})
	return status, out.String(), errb.String()
}

func prEvent(action string, author person) map[string]any {
	return map[string]any{"action": action, "pull_request": map[string]any{"number": 7, "user": author, "head": map[string]any{"sha": "abc123"}},
		"repository": map[string]any{"id": 5}}
}

func commentEvent(by person, body string) map[string]any {
	return map[string]any{"action": "created",
		"issue":      map[string]any{"number": 7, "pull_request": map[string]any{}},
		"comment":    map[string]any{"id": 4242, "body": body, "created_at": "2026-10-16T23:00:00Z", "user": by},
		"repository": map[string]any{"id": 5}}
}

func TestCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		author     person
		commits    []map[string]any
		file       *signatures
		comments   []map[string]any
		action     string
		want       int
		wantNote   string // a substring of the one comment, or "" for none
		wantNotNot string // a substring the comment must not hold
	}{
		{name: "the author and every committer signed", author: alice, commits: []map[string]any{commitBy(&alice, "Alice", "a@example.com"), commitBy(&bob, "Bob", "b@example.com")},
			file: signed(alice, bob), want: 0},
		{name: "a committer has not signed", author: alice, commits: []map[string]any{commitBy(&alice, "Alice", "a@example.com"), commitBy(&bob, "Bob", "b@example.com")},
			file: signed(alice), want: 1, wantNote: "Still to sign: @bob", wantNotNot: "@alice"},
		{name: "no signatures file yet", author: alice, commits: []map[string]any{commitBy(&alice, "Alice", "a@example.com")},
			want: 1, wantNote: "> " + Sentence},
		{name: "the allowed need not sign", author: owner, commits: []map[string]any{commitBy(&owner, "Ed", "e@example.com"), commitBy(&bot, "dependabot", "bot@example.com")},
			file: signed(), want: 0},
		{name: "a commit with no GitHub account fails", author: alice, commits: []map[string]any{commitBy(nil, "Carol", "carol@example.com")},
			file: signed(alice), want: 1, wantNote: "Carol <carol@example.com>"},
		{name: "the comment is edited, not added again", author: alice, commits: []map[string]any{commitBy(&bob, "Bob", "b@example.com")},
			file: signed(alice), comments: []map[string]any{{"id": 1, "body": marker + "\nold", "user": map[string]any{"type": "Bot"}}},
			want: 1, wantNote: "Still to sign: @bob"},
		{name: "a person's comment that quotes the marker is left alone", author: alice, commits: []map[string]any{commitBy(&bob, "Bob", "b@example.com")},
			file: signed(alice), comments: []map[string]any{{"id": 1, "body": marker + "\nmine", "user": map[string]any{"type": "User"}}},
			want: 1, wantNote: "Still to sign: @bob"},
		{name: "once all have signed, the comment says so", author: alice, commits: []map[string]any{commitBy(&alice, "Alice", "a@example.com")},
			file: signed(alice), comments: []map[string]any{{"id": 1, "body": marker + "\nStill to sign: @alice", "user": map[string]any{"type": "Bot"}}},
			want: 0, wantNote: "has signed"},
		{name: "a closed pull request is not checked", author: alice, action: "closed", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeGitHub{author: tt.author, commits: tt.commits, file: tt.file, comments: tt.comments}
			action := tt.action
			if action == "" {
				action = "synchronize"
			}
			status, out, errb := invoke(t, f, "pull_request_target", prEvent(action, tt.author))
			if status != tt.want {
				t.Fatalf("status %d, want %d\nstdout: %s\nstderr: %s", status, tt.want, out, errb)
			}
			if tt.action != "closed" && f.auth != "Bearer t0ken" {
				t.Errorf("Authorization %q", f.auth)
			}
			var bots []string
			for _, c := range f.comments {
				if c["user"].(map[string]any)["type"] == "Bot" {
					bots = append(bots, c["body"].(string))
				}
			}
			switch {
			case tt.wantNote == "" && len(bots) != 0:
				t.Errorf("comments %q, want none", bots)
			case tt.wantNote != "" && len(bots) != 1:
				t.Fatalf("%d comments of the check, want 1: %q", len(bots), bots)
			case tt.wantNote != "" && !strings.Contains(bots[0], tt.wantNote):
				t.Errorf("comment %q lacks %q", bots[0], tt.wantNote)
			case tt.wantNotNot != "" && strings.Contains(bots[0], tt.wantNotNot):
				t.Errorf("comment %q holds %q", bots[0], tt.wantNotNot)
			}
			if f.puts != 0 {
				t.Errorf("the check wrote the signatures file %d times", f.puts)
			}
		})
	}
}

func TestComment(t *testing.T) {
	t.Parallel()
	runs := func() []map[string]any {
		return []map[string]any{
			{"id": 31, "path": ".github/workflows/ci.yml", "status": "completed"},
			{"id": 30, "path": workflowPath, "status": "completed"},
			{"id": 29, "path": workflowPath, "status": "completed"},
		}
	}
	tests := []struct {
		name       string
		by         person
		body       string
		file       *signatures
		conflicts  int
		runs       []map[string]any
		wantSigned []int64
		wantPuts   int
		wantReruns []int64
		wantOut    string
	}{
		{name: "a committer signs", by: bob, body: Sentence, file: signed(alice), runs: runs(),
			wantSigned: []int64{101, 102}, wantPuts: 1, wantReruns: []int64{30}},
		{name: "the first signature makes the file", by: bob, body: "  " + Sentence + "\n", runs: runs(),
			wantSigned: []int64{102}, wantPuts: 1, wantReruns: []int64{30}},
		{name: "a signature written meanwhile is kept", by: bob, body: Sentence, file: signed(alice), conflicts: 2, runs: runs(),
			wantSigned: []int64{101, 102}, wantPuts: 1, wantReruns: []int64{30}},
		{name: "signing twice records once", by: bob, body: Sentence, file: signed(bob), runs: runs(),
			wantSigned: []int64{102}, wantPuts: 0, wantReruns: []int64{30}},
		{name: "someone with no commits here records nothing", by: person{Login: "mallory", ID: 666}, body: Sentence, file: signed(), runs: runs(),
			wantSigned: []int64{}, wantPuts: 0, wantOut: "need not sign"},
		{name: "recheck reruns alone", by: alice, body: "recheck", file: signed(), runs: runs(),
			wantSigned: []int64{}, wantReruns: []int64{30}},
		{name: "a check still running is left to finish", by: bob, body: Sentence, file: signed(), runs: []map[string]any{{"id": 40, "path": workflowPath, "status": "in_progress"}},
			wantSigned: []int64{102}, wantPuts: 1, wantOut: "still running"},
		{name: "any other comment does nothing", by: bob, body: "I have read the CLA Document and I hereby sign the CLA, mostly", file: signed(), runs: runs(),
			wantSigned: []int64{}, wantOut: "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeGitHub{author: alice, file: tt.file, conflicts: tt.conflicts, runs: tt.runs,
				commits: []map[string]any{commitBy(&alice, "Alice", "a@example.com"), commitBy(&bob, "Bob", "b@example.com")}}
			status, out, errb := invoke(t, f, "issue_comment", commentEvent(tt.by, tt.body))
			if status != 0 {
				t.Fatalf("status %d\nstdout: %s\nstderr: %s", status, out, errb)
			}
			var ids []int64
			if f.file != nil {
				for _, s := range f.file.SignedContributors {
					ids = append(ids, s.ID)
				}
			}
			if fmt.Sprint(ids) != fmt.Sprint(tt.wantSigned) && !(len(ids) == 0 && len(tt.wantSigned) == 0) {
				t.Errorf("signed %v, want %v", ids, tt.wantSigned)
			}
			if f.puts != tt.wantPuts {
				t.Errorf("%d writes, want %d", f.puts, tt.wantPuts)
			}
			if fmt.Sprint(f.reruns) != fmt.Sprint(tt.wantReruns) && !(len(f.reruns) == 0 && len(tt.wantReruns) == 0) {
				t.Errorf("reran %v, want %v", f.reruns, tt.wantReruns)
			}
			if tt.wantOut != "" && !strings.Contains(out, tt.wantOut) {
				t.Errorf("stdout %q lacks %q", out, tt.wantOut)
			}
			if tt.wantPuts == 1 {
				last := f.file.SignedContributors[len(f.file.SignedContributors)-1]
				want := signature{Name: "bob", ID: 102, CommentID: 4242, CreatedAt: "2026-10-16T23:00:00Z", RepoID: 5, PullRequestNo: 7}
				if last != want {
					t.Errorf("recorded %+v, want %+v", last, want)
				}
			}
		})
	}
}

// TestSignaturesKeepTheirShape: the file is written in the shape the
// action before this command wrote, so its signatures still count.
func TestSignaturesKeepTheirShape(t *testing.T) {
	t.Parallel()
	b, err := json.MarshalIndent(signed(alice), "", "   ")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"signedContributors"`, `"name"`, `"id"`, `"comment_id"`, `"created_at"`, `"repoId"`, `"pullRequestNo"`} {
		if !bytes.Contains(b, []byte(k)) {
			t.Errorf("%s lacks %s", b, k)
		}
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	var errb bytes.Buffer
	if got := run(context.Background(), nil, env{getenv: func(string) string { return "" }, stdout: io.Discard, stderr: &errb}); got != 2 {
		t.Errorf("no --document: status %d, want 2", got)
	}
	if got := run(context.Background(), []string{"--document", "https://example.com/CLA.md"},
		env{getenv: func(string) string { return "" }, stdout: io.Discard, stderr: &errb}); got != 1 || !strings.Contains(errb.String(), "GITHUB_TOKEN") {
		t.Errorf("no token: status %d, stderr %q", got, errb.String())
	}
}

func TestAllowed(t *testing.T) {
	t.Parallel()
	allow := []string{"edwardfward", "*[bot]"}
	for login, want := range map[string]bool{"edwardfward": true, "dependabot[bot]": true, "alice": false, "edwardfward2": false, "[bot]x": false} {
		if got := allowed(login, allow); got != want {
			t.Errorf("allowed(%q) = %t, want %t", login, got, want)
		}
	}
}
