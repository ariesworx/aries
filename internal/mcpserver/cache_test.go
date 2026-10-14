package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/proto"
)

// heldWork is a server with one issue alice holds, sf-1, and another she
// looked at, sf-2, whose title the test changes.
type heldWork struct{ title2 string }

func (h *heldWork) reply(op string, args any) (any, error) {
	working := proto.Summary{ID: "sf-1", Title: "Held work", Status: "in_progress", Priority: 1}
	switch op {
	case proto.OpStart:
		return proto.StartResult{Issue: proto.Issue{ID: "sf-1", Title: "Held work", Status: "in_progress", Body: "the plan"},
			Claim: &proto.Claim{ID: "sf-1", By: "alice", Epoch: 1},
			Items: []proto.AcceptanceItem{{N: 1, Text: "tests pass"}}}, nil
	case proto.OpShow:
		if a := args.(proto.ShowArgs); a.ID == "sf-2" {
			return proto.ShowResult{Issue: proto.Issue{ID: "sf-2", Title: h.title2, Status: "open"}}, nil
		}
		return proto.ShowResult{}, proto.Errf(proto.CodeNotFound, "find the id with `sfx list`", "issue not found")
	case proto.OpList:
		return proto.ListResult{Issues: []proto.Summary{working}}, nil
	case proto.OpReady:
		return proto.ListResult{Issues: []proto.Summary{{ID: "sf-3", Title: "Up next", Status: "open"}}}, nil
	case proto.OpInbox:
		return proto.InboxResult{Unread: 1, Items: []proto.InboxItem{{ID: 7, Kind: "handoff", Issue: "sf-1", From: "bob", Body: "over to you"}}}, nil
	case proto.OpRecall:
		return proto.RecallResult{Memories: []proto.Memory{{Key: "style", Body: "tabs", Scope: "project", Pinned: true}}}, nil
	case proto.OpDigest:
		return proto.DigestResult{Totals: proto.DigestTotals{Closed: 3}}, nil
	}
	return proto.Empty{}, nil
}

// lastLine is the last text of a result: the cached line, when it has
// one.
func lastLine(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	tc, ok := res.Content[len(res.Content)-1].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[len(res.Content)-1])
	}
	return tc.Text
}

// TestCachedToolCalls is slice F3's gate for an agent (design §18.10):
// with the server down, prime and show of the work it holds answer from
// this machine's copy and say so in one terse line; inbox and digest do
// too; ready, blocked, list and who are refused, saying when the server
// was last reached; and a server that answers is always asked.
func TestCachedToolCalls(t *testing.T) {
	t.Parallel()
	h := &heldWork{title2: "Looked at"}
	conn := &fakeConn{}
	conn.reply = h.reply
	a := newAway(t, conn)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"prime", nil}, {"start", nil}, {"show", map[string]any{"id": "sf-2"}}, {"inbox", nil}, {"digest", nil},
	} {
		if res := callTool(t, a.cs, c.tool, c.args); res.IsError {
			t.Fatalf("%s with the server up: %s", c.tool, text(t, res))
		}
	}
	// A server that answers is asked, whatever is kept.
	h.title2 = "Renamed"
	if got := decodeText[Issue](t, callTool(t, a.cs, "show", map[string]any{"id": "sf-2"})); got.Title != "Renamed" {
		t.Errorf("show with the server up = %+v, want the server's answer", got)
	}

	drop(conn)
	a.net.set(true)
	isCached := func(tool string, res *mcp.CallToolResult) {
		t.Helper()
		line := lastLine(t, res)
		if res.IsError || !strings.HasPrefix(line, "cached: starfix cannot reach its server, so this is this machine's copy from ") ||
			!strings.HasSuffix(line, "and may be out of date") {
			t.Errorf("%s with the server down = %q (error %v); want the cached answer, and the line that says so", tool, line, res.IsError)
		}
		if n := Tokens([]byte(line)); n > 50 {
			t.Errorf("%s: the cached line is %d tokens, want it terse", tool, n)
		}
	}

	res := callTool(t, a.cs, "prime", nil)
	p := decodeText[Prime](t, res)
	if res.IsError || len(p.Notices) == 0 || !strings.HasPrefix(p.Notices[0], "cached: ") || p.You != "alice" ||
		len(p.Working) != 1 || p.Working[0].ID != "sf-1" || len(p.Ready) != 0 || len(p.Memories) != 1 || p.Unread != 1 {
		t.Errorf("prime with the server down = %+v (error %v); want alice's work, memories and inbox from the cache, no ready issues, and a notice that says so", p, res.IsError)
	}
	if len(res.Content) != 1 {
		t.Errorf("prime with the server down has %d texts; want the notice inside it, and no line after", len(res.Content))
	}

	res = callTool(t, a.cs, "show", map[string]any{"id": "sf-1"})
	isCached("show of the held issue", res)
	if got := decodeText[Issue](t, res); got.Title != "Held work" || got.Body != "the plan" || len(got.Items) != 1 {
		t.Errorf("show of the held issue = %+v; want what start returned", got)
	}
	res = callTool(t, a.cs, "show", map[string]any{"id": "sf-2"})
	isCached("show of an issue looked at", res)
	if got := decodeText[Issue](t, res); got.Title != "Renamed" {
		t.Errorf("show of an issue looked at = %+v; want the newest answer kept", got)
	}
	res = callTool(t, a.cs, "inbox", nil)
	isCached("inbox", res)
	res = callTool(t, a.cs, "digest", nil)
	isCached("digest", res)

	if res := callTool(t, a.cs, "show", map[string]any{"id": "sf-9"}); !res.IsError || !strings.HasPrefix(text(t, res), "unavailable: ") {
		t.Errorf("show of an issue never looked at = %q (error %v); want the server's absence, as before", text(t, res), res.IsError)
	}
	for _, tool := range []string{"ready", "blocked", "list", "who"} {
		res := callTool(t, a.cs, tool, nil)
		got := text(t, res)
		if !res.IsError || !strings.Contains(got, "is not answered from this machine's copy") ||
			!strings.Contains(got, "server last reached ") || !strings.Contains(got, "carry on with the work you hold") {
			t.Errorf("%s with the server down = %q (error %v); want it refused, saying when the server was last reached, with a fix", tool, got, res.IsError)
		}
	}
}

// decodeText decodes a result's first text, its JSON.
func decodeText[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(text(t, res)), &v); err != nil {
		t.Fatalf("decode %q: %v", text(t, res), err)
	}
	return v
}
