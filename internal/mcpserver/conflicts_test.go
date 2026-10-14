package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// TestCommentProposes: comment with a conflict sends the agent's
// proposal, its body the why, for a person to approve, and nothing
// resolves it (decision 42); without one it is a comment.
func TestCommentProposes(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{reply: func(op string, _ any) (any, error) {
		if op == proto.OpComment {
			return proto.CommentResult{ID: "c-1"}, nil
		}
		return proto.Empty{}, nil
	}}
	cs, _ := connect(t, conn)
	for _, tc := range []struct {
		propose string
		want    proto.ProposeArgs
	}{
		{"both titles", proto.ProposeArgs{ID: 3, Choice: "text", Text: "both titles", Why: "keeps both"}},
		{"mine", proto.ProposeArgs{ID: 3, Choice: "mine", Why: "keeps both"}},
		{"theirs", proto.ProposeArgs{ID: 3, Choice: "theirs", Why: "keeps both"}},
		{"accept", proto.ProposeArgs{ID: 3, Choice: "text", Text: "accept", Why: "keeps both"}}, // text; accepting is a person's
	} {
		res := callTool(t, cs, "comment", map[string]any{"id": "sf-1", "conflict": 3, "propose": tc.propose, "body": "keeps both"})
		if res.IsError {
			t.Fatalf("comment proposing %q = %s", tc.propose, text(t, res))
		}
		calls := conn.ops()
		if last := conn.calls[len(calls)-1]; last.op != proto.OpPropose || last.args.(proto.ProposeArgs) != tc.want {
			t.Errorf("comment proposing %q sent %s %+v, want conflict.propose %+v", tc.propose, last.op, last.args, tc.want)
		}
		if out := text(t, res); !strings.Contains(out, "a person") {
			t.Errorf("the proposal answered %q; want it to say a person decides", out)
		}
	}
	n := len(conn.ops())
	if res := callTool(t, cs, "comment", map[string]any{"id": "sf-1", "body": "b", "conflict": 3}); !res.IsError || len(conn.ops()) != n {
		t.Error("comment with a conflict and nothing to propose was sent")
	}
	res := callTool(t, cs, "comment", map[string]any{"id": "sf-1", "body": "plain"})
	if ops := conn.ops(); res.IsError || ops[len(ops)-1] != proto.OpComment || !strings.Contains(text(t, res), "c-1") {
		t.Errorf("a plain comment = %s, calls %v; want a comment", text(t, res), ops)
	}
}

// TestConflictErrorsNameTheNextStep: a refused proposal names the
// agent's next step in tools, and never a resolve.
func TestConflictErrorsNameTheNextStep(t *testing.T) {
	t.Parallel()
	got := explain(proto.Errf(proto.CodeNotFound, proto.FixFindConflict+" with `sfx conflicts`", "conflict 9 not found")).Fix
	if want := "call show on the issue to list its open conflicts and their numbers"; got != want {
		t.Errorf("fix = %q\nwant  %q", got, want)
	}
}

// TestShowConflicts: show gives an issue's open conflicts, each side cut
// to fit, with what an agent needs to propose.
func TestShowConflicts(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 2000)
	conn := &fakeConn{reply: func(op string, _ any) (any, error) {
		return proto.ShowResult{Issue: proto.Issue{ID: "sf-1", Title: "t", Status: "open", Rev: 4},
			Conflicts: []proto.Conflict{{ID: 3, Issue: "sf-1", Field: "body", BaseRev: 2, Base: long, Mine: long + "mine",
				Theirs: long + "theirs", By: "alice", TheirsBy: "bob", Advice: "take theirs",
				Proposal: &proto.Proposal{Choice: "theirs", By: "agent-1", At: time.Now()}}}}, nil
	}}
	cs, _ := connect(t, conn)
	res := callTool(t, cs, "show", map[string]any{"id": "sf-1"})
	var is Issue
	if err := json.Unmarshal([]byte(text(t, res)), &is); err != nil {
		t.Fatal(err)
	}
	if len(is.Conflicts) != 1 {
		t.Fatalf("show's conflicts = %+v, want 1", is.Conflicts)
	}
	c := is.Conflicts[0]
	if c.ID != 3 || c.Field != "body" || c.By != "alice" || c.TheirsBy != "bob" || c.Advice != "take theirs" ||
		c.Proposed != "theirs by agent-1" || !strings.HasSuffix(c.Mine, "…") {
		t.Errorf("show's conflict = %+v; want its id, field, sides cut, advice and proposal", c)
	}
	if n := Tokens([]byte(text(t, res))); n > MaxResultTokens {
		t.Errorf("show with a conflict costs ~%d tokens, over %d", n, MaxResultTokens)
	}
}

// TestDigestListsOldConflicts: the digest tool gives the server's old
// conflicts, as the CLI's digest does.
func TestDigestListsOldConflicts(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{reply: func(string, any) (any, error) {
		return proto.DigestResult{Since: until.Add(-24 * time.Hour), Until: until, Totals: proto.DigestTotals{Conflicts: 1},
			Conflicts: []proto.DigestItem{{ID: "sf-1", Title: "t", By: "alice", At: until.Add(-30 * time.Hour), Note: "title (conflict 3)"}}}, nil
	}}
	cs, _ := connect(t, conn)
	var d Digest
	if err := json.Unmarshal([]byte(text(t, callTool(t, cs, "digest", nil))), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Conflicts) != 1 || d.Conflicts[0].Note != "title (conflict 3)" || d.Totals.Conflicts != 1 {
		t.Errorf("digest's conflicts = %+v, totals %+v", d.Conflicts, d.Totals)
	}
}

// TestPrimeCountsConflicts: prime says how many conflicts concern the
// principal, and that a person resolves them.
func TestPrimeCountsConflicts(t *testing.T) {
	t.Parallel()
	conn := &fakeConn{who: "alice", reply: func(op string, _ any) (any, error) {
		switch op {
		case proto.OpInbox:
			return proto.InboxResult{Items: []proto.InboxItem{}, Conflicts: 2}, nil
		case proto.OpList, proto.OpReady:
			return proto.ListResult{Issues: []proto.Summary{}}, nil
		}
		return proto.Empty{}, nil
	}}
	p, err := BuildPrime(t.Context(), conn, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if p.Conflicts != 2 || !strings.Contains(p.Text(), "conflicts: 2 open") {
		t.Errorf("prime = %d conflicts, text:\n%s\nwant 2, said", p.Conflicts, p.Text())
	}
}
