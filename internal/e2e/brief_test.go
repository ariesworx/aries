package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/brief"
)

// aries dispatch brief renders the repository's brief template for an
// issue read from a real server: a Desktop brief with its hand-written
// section and its record's first line, the same text the dispatcher's
// runs get less what only a headless run is told, and nothing written.
func TestDispatchBrief(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	gitInit(t, alice.repo)
	tmpl, err := os.ReadFile(filepath.Join("..", "..", brief.File))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(alice.repo, ".aries"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alice.repo, brief.File), tmpl, 0o600); err != nil { //nolint:gosec // the test user's repository, a temporary directory
		t.Fatal(err)
	}
	id := strings.TrimSpace(alice.ok("create", "Export to CSV", "-t", "feature", "--body", "Write the rows as CSV."))

	out := alice.ok("dispatch", "brief", id, "--note", "Leave the permission list alone.")
	for _, want := range []string{
		"started on issue " + id + ".", "title: Export to CSV", "Write the rows as CSV.",
		brief.HandBegin + "\nLeave the permission list alone.\n" + brief.HandEnd,
		"turn on Auto-fix", "feature/" + id + "-export-to-csv", "Aries: " + id,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the brief lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "headless") || strings.Contains(out, "[desktop]") {
		t.Errorf("the Desktop brief holds a headless step or a tag:\n%s", out)
	}

	var doc struct {
		Template, SHA256, Brief, Comment string
	}
	if err := json.Unmarshal([]byte(alice.ok("--json", "dispatch", "brief", id, "--record", "--model", "claude-sonnet-5-5", "--session", "local_0000")), &doc); err != nil {
		t.Fatal(err)
	}
	rec, text, ok := brief.ParseComment(doc.Comment)
	if !ok || rec.Harness != brief.Desktop || rec.Model != "claude-sonnet-5-5" || rec.Effort != "default" ||
		rec.Session != "local_0000" || rec.Template != doc.Template || !strings.HasPrefix(doc.Template, "brief@") {
		t.Errorf("record %+v from %q", rec, doc.Comment)
	}
	if doc.SHA256 != brief.Hash(text) || doc.SHA256 != brief.Hash(doc.Brief) {
		t.Errorf("the hash %s is not the record's", doc.SHA256)
	}
	if got := alice.ok("comments", id); strings.Contains(got, "brief:") {
		t.Errorf("dispatch brief posted: %s", got)
	}
}
