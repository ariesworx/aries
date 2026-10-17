package mcpserver

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/aries/internal/proto"
)

// contents returns the text of each block of res.
func contents(t *testing.T, res *mcp.CallToolResult) []string {
	t.Helper()
	var out []string
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			t.Fatalf("content is %T, want text", c)
		}
		out = append(out, tc.Text)
	}
	return out
}

// closeoutReply answers finish, handoff and close with the close-out
// lines the server gave.
func closeoutReply(lines []string) func(string, any) (any, error) {
	return func(op string, _ any) (any, error) {
		switch op {
		case proto.OpFinish:
			return proto.FinishResult{ID: "ar-1", Rev: 3, Closeout: lines}, nil
		case proto.OpHandoff:
			return proto.HandoffResult{WriteResult: proto.WriteResult{ID: "ar-1", Rev: 3}, Closeout: lines}, nil
		case proto.OpClose:
			return proto.CloseResult{WriteResult: proto.WriteResult{ID: "ar-1", Rev: 3}, Closeout: lines}, nil
		}
		return nil, errors.New("unexpected " + op)
	}
}

// The close-out checklist (design §21.7) is a short list after the
// result line, from finish, handoff and close; a result with none is the
// result line alone, as before.
func TestCloseoutAfterTheResultLine(t *testing.T) {
	t.Parallel()
	lines := []string{"Stop your compose stack", "Archive the session"}
	const list = "closeout:\n- Stop your compose stack\n- Archive the session"
	for _, tc := range []struct {
		tool  string
		args  map[string]any
		lines []string
		want  []string
	}{
		{"finish", map[string]any{"id": "ar-1"}, lines, []string{`{"id":"ar-1","rev":3}`, list}},
		{"handoff", map[string]any{"id": "ar-1", "note": "n", "state": "done"}, lines, []string{`{"id":"ar-1","rev":3}`, list}},
		{"close", map[string]any{"id": "ar-1"}, lines, []string{`{"id":"ar-1","rev":3}`, list}},
		{"finish", map[string]any{"id": "ar-1"}, nil, []string{`{"id":"ar-1","rev":3}`}},
		{"handoff", map[string]any{"id": "ar-1", "note": "n"}, nil, []string{`{"id":"ar-1","rev":3}`}},
		{"close", map[string]any{"id": "ar-1"}, nil, []string{`{"id":"ar-1","rev":3}`}},
	} {
		t.Run(fmt.Sprintf("%s/%d lines", tc.tool, len(tc.lines)), func(t *testing.T) {
			t.Parallel()
			cs, _ := connect(t, &fakeConn{reply: closeoutReply(tc.lines)})
			res := callTool(t, cs, tc.tool, tc.args)
			if res.IsError {
				t.Fatal(text(t, res))
			}
			if got := contents(t, res); strings.Join(got, "\n--\n") != strings.Join(tc.want, "\n--\n") {
				t.Errorf("%s result = %q; want %q", tc.tool, got, tc.want)
			}
		})
	}
}

// A long checklist is cut by whole lines to keep the result under its
// cap, the result line first and whole, and says how many it left out.
func TestCloseoutFitsTheCap(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("%02d %s", i, strings.Repeat("x", 490)))
	}
	cs, _ := connect(t, &fakeConn{reply: closeoutReply(lines)})
	res := callTool(t, cs, "finish", map[string]any{"id": "ar-1"})
	got := contents(t, res)
	if len(got) != 2 || got[0] != `{"id":"ar-1","rev":3}` {
		t.Fatalf("finish result = %.80q; want the result line, then the list", got)
	}
	if n := Tokens([]byte(strings.Join(got, "\n"))); n > MaxResultTokens {
		t.Errorf("finish result is %d tokens; cap %d", n, MaxResultTokens)
	}
	kept := strings.Count(got[1], "\n- ") // lines after the heading
	if kept < 2 || !strings.HasPrefix(got[1], "closeout:\n- 00 ") || !strings.HasSuffix(got[1], fmt.Sprintf("- … %d more", 20-(kept-1))) {
		t.Errorf("list = %d lines, ends %q; want the first lines and a count of the rest", kept, got[1][max(0, len(got[1])-30):])
	}
}

// digest lists an epic whose last child closed, with its close-out lines
// as the note, and stays under its cap with many.
func TestDigestCloseout(t *testing.T) {
	t.Parallel()
	var items []proto.DigestItem
	for i := range 40 {
		items = append(items, proto.DigestItem{ID: fmt.Sprintf("ar-%08d", i), Title: strings.Repeat("t", 80), Priority: 2,
			By: "alice", Note: strings.Repeat("Prune worktrees; ", 40)})
	}
	f := &fakeConn{reply: func(op string, _ any) (any, error) {
		if op != proto.OpDigest {
			return nil, errors.New("unexpected " + op)
		}
		return proto.DigestResult{Totals: proto.DigestTotals{Closeout: 40}, Closeout: items}, nil
	}}
	cs, _ := connect(t, f)
	res := callTool(t, cs, "digest", nil)
	got := text(t, res)
	if res.IsError || Tokens([]byte(got)) > MaxDigestTokens || !strings.Contains(got, `"closeout":[{"id":"ar-00000000"`) ||
		!strings.Contains(got, "Prune worktrees;") || !strings.Contains(got, `"truncated":true`) {
		t.Errorf("digest = %d tokens (cap %d), error %v: %.300s", Tokens([]byte(got)), MaxDigestTokens, res.IsError, got)
	}
}
