package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/outbox"
	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrintConflicts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c := proto.Conflict{ID: 3, Issue: "sf-a1", Field: "title", BaseRev: 4, Base: "Fix login", Mine: "Fix the login redirect",
		Theirs: "Fix login\nand logout", By: "alice", TheirsBy: "bob", TheirsAt: now.Add(-3 * time.Hour),
		ParkedAt: now.Add(-2 * time.Hour), Advice: "theirs is from bob, who holds the issue; take theirs"}
	proposed := c
	proposed.Proposal = &proto.Proposal{Choice: "text", Text: "Fix the login and logout redirects", Why: "keeps both", By: "agent-7"}
	tests := []struct {
		name string
		cs   []proto.Conflict
		next int64
		want string
	}{
		{"none", nil, 0, "no open conflicts\n"},
		{"one", []proto.Conflict{c}, 0, `#3 sf-a1 title: alice's update from rev 4, parked 2h ago, against bob's of 3h ago
  base:   Fix login
  mine:   Fix the login redirect
  theirs: Fix login\nand logout
  advice: theirs is from bob, who holds the issue; take theirs
resolve with ` + "`sfx resolve N --mine|--theirs|--text TEXT|--accept`; `sfx conflicts N` shows one whole" + `
`},
		{"a proposal, and more", []proto.Conflict{proposed}, 3, `#3 sf-a1 title: alice's update from rev 4, parked 2h ago, against bob's of 3h ago
  base:   Fix login
  mine:   Fix the login redirect
  theirs: Fix login\nand logout
  advice: theirs is from bob, who holds the issue; take theirs
  proposed by agent-7: text: Fix the login and logout redirects (keeps both)
more: ` + "`sfx conflicts --after 3`" + `
resolve with ` + "`sfx resolve N --mine|--theirs|--text TEXT|--accept`; `sfx conflicts N` shows one whole" + `
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printConflicts(&b, proto.ConflictsResult{Conflicts: tc.cs, Next: tc.next}, now)
			if b.String() != tc.want {
				t.Errorf("printConflicts:\n%s\nwant:\n%s", b.String(), tc.want)
			}
		})
	}
}

// TestPrintConflict: one conflict, asked for by number, shows each side
// whole, on lines of its own.
func TestPrintConflict(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	resolved := now.Add(-time.Hour)
	c := proto.Conflict{ID: 3, Issue: "sf-a1", Field: "body", BaseRev: 4, Base: "one\ntwo", Mine: "one\ntwo, mine",
		Theirs: "one, theirs\ntwo", By: "alice", TheirsBy: "bob", TheirsAt: now.Add(-3 * time.Hour),
		ParkedAt: now.Add(-2 * time.Hour), Resolution: "theirs", ResolvedBy: "carol", ResolvedAt: &resolved}
	var b bytes.Buffer
	printConflict(&b, c, now)
	want := `#3 sf-a1 body: alice's update from rev 4, parked 2h ago, against bob's of 3h ago
resolved: theirs, by carol 1h ago
--- base (rev 4)
one
two
--- mine (alice)
one
two, mine
--- theirs (bob)
one, theirs
two
`
	if b.String() != want {
		t.Errorf("printConflict:\n%s\nwant:\n%s", b.String(), want)
	}
}

// TestShowSaysConflicts: show names an issue's open conflicts, and where
// to see them.
func TestShowSaysConflicts(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	printIssue(&b, proto.ShowResult{Issue: proto.Issue{ID: "sf-a1", Title: "t", Status: "open", Type: "task", Rev: 3},
		Conflicts: []proto.Conflict{{ID: 3, Field: "title"}, {ID: 4, Field: "body"}}})
	if want := "conflicts: 2 open (title, body): `sfx conflicts sf-a1`\n"; !strings.Contains(b.String(), want) {
		t.Errorf("printIssue:\n%s\nwant a line %q", b.String(), want)
	}
}

func TestReplayLineParked(t *testing.T) {
	t.Parallel()
	got := replayLine(outbox.Report{Delivered: 3, Parked: 1})
	if want := "outbox: delivered 3 (1 parked conflicts for a person to resolve: `sfx conflicts`)"; got != want {
		t.Errorf("replayLine = %q, want %q", got, want)
	}
}
