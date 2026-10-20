package e2e

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	outcomerec "github.com/ariesworx/aries/internal/outcome"
)

// e2eHost is a CI host with one merged change for every issue; nothing
// reaches a real host.
type e2eHost struct{ opened, merged time.Time }

func (h e2eHost) Name() string { return "github" }
func (h e2eHost) Changes(_ context.Context, issue string, _ []string) ([]outcomerec.Change, error) {
	return []outcomerec.Change{{ID: "7", URL: "https://example.com/acme/widgets/pull/7", Base: "main",
		Branch: "feature/" + issue + "-x", Opened: h.opened, Merged: h.merged, Closed: h.merged}}, nil
}
func (h e2eHost) Closed(context.Context, time.Time) ([]outcomerec.Change, error) { return nil, nil }
func (h e2eHost) Detail(context.Context, outcomerec.Change) (outcomerec.Detail, error) {
	return outcomerec.Detail{Commits: []outcomerec.Commit{{SHA: "c1", Parents: []string{"b0"}, At: h.opened}},
		Runs: []outcomerec.Run{{ID: "1", Workflow: "ci", Attempt: 1, SHA: "c1", Conclusion: outcomerec.Pass,
			Started: h.opened, Finished: h.opened.Add(time.Minute)}}, Fetched: true}, nil
}

// aries outcome reads a real server through the ops a client sends, so
// that it needs no protocol change: a dry run prints the record of a
// landed issue and posts nothing, a run posts it once as an outcome:
// comment, and a second run posts nothing new.
func TestOutcome(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	id := strings.TrimSpace(alice.ok("create", "Record what landing took", "-t", "task"))
	alice.ok("start", id)
	alice.ok("handoff", id, "done; the change is up", "--state", "done", "--branch", "feature/"+id+"-x")
	host := e2eHost{opened: time.Now().Add(-time.Hour)}
	host.merged = time.Now()
	alice.outcomeHost = func(string) outcomerec.Host { return host }
	if out := alice.ok("outcome", id); !strings.Contains(out, "no outcome yet") {
		t.Errorf("outcome of an open issue: %q", out)
	}
	alice.ok("close", id, "--reason", "landed")

	var doc struct {
		Records []struct {
			Action string            `json:"action"`
			Record outcomerec.Record `json:"record"`
		} `json:"records"`
		DryRun bool `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(alice.ok("--json", "outcome", id, "--dry-run")), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.DryRun || len(doc.Records) != 1 {
		t.Fatalf("dry run: %+v", doc)
	}
	r := doc.Records[0]
	if r.Action != "post" || r.Record.Outcome != outcomerec.Landed || r.Record.Seq != 1 || r.Record.Change == nil ||
		r.Record.Change.ID != "7" || r.Record.CI == nil || !r.Record.CI.FirstPass || len(r.Record.Runs) != 1 ||
		r.Record.Runs[0].Kind != "person" || r.Record.Times.FirstDone == nil || r.Record.Tokens == nil {
		t.Errorf("dry run's record: %+v", r)
	}
	if got := alice.ok("comments", id); strings.Contains(got, "outcome:") {
		t.Errorf("a dry run posted: %s", got)
	}

	if out := alice.ok("outcome", id); !strings.Contains(out, id+" seq 1 landed: posted") {
		t.Errorf("outcome: %q", out)
	}
	if out := alice.ok("outcome", id); !strings.Contains(out, "unchanged, nothing posted") {
		t.Errorf("a second outcome: %q", out)
	}
	if got := alice.ok("comments", id); strings.Count(got, "outcome: v1 landed seq 1") != 1 {
		t.Errorf("comments after two runs: %s", got)
	}
	// --since finds the issue by its close, and posts nothing new.
	if out := alice.ok("outcome", "--since", "1h"); !strings.Contains(out, id+" seq 1 landed: unchanged") {
		t.Errorf("outcome --since: %q", out)
	}
}
