package mcpserver

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/aries/internal/proto"
)

// Every create-type tool sends an idempotency key, the same one on the
// retry after a dropped connection; keys differ between calls.
func TestWritesRetryWithTheirKey(t *testing.T) {
	t.Parallel()
	key := func(args any) string {
		switch a := args.(type) {
		case proto.CreateArgs:
			return a.Idem
		case proto.CommentArgs:
			return a.Idem
		case proto.HandoffArgs:
			return a.Idem
		case proto.FinishArgs:
			return a.Idem
		case proto.RememberArgs:
			return a.Idem
		case proto.ForgetArgs:
			return a.Idem
		}
		return ""
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"create", map[string]any{"title": "x"}},
		{"comment", map[string]any{"id": "ar-1", "body": "hi"}},
		{"handoff", map[string]any{"id": "ar-1", "note": "later"}},
		{"finish", map[string]any{"id": "ar-1"}},
		{"remember", map[string]any{"key": "k", "body": "b"}},
		{"forget", map[string]any{"key": "k"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			var keys []string
			record := func(fail bool) *fakeConn {
				return &fakeConn{reply: func(_ string, args any) (any, error) {
					keys = append(keys, key(args))
					if fail {
						return nil, errLost
					}
					return proto.WriteResult{ID: "ar-1", Rev: 2}, nil
				}}
			}
			cs, d := connect(t, record(true), record(false))
			if res := callTool(t, cs, tc.tool, tc.args); res.IsError {
				t.Fatalf("%s after a drop: %s", tc.tool, text(t, res))
			}
			if res := callTool(t, cs, tc.tool, tc.args); res.IsError {
				t.Fatalf("%s again: %s", tc.tool, text(t, res))
			}
			if len(keys) != 3 || keys[0] != keys[1] || !strings.HasPrefix(keys[0], "mcp-") || keys[2] == keys[0] || d.n != 2 {
				t.Fatalf("keys %q over %d dials; want the first two equal, the third new", keys, d.n)
			}
		})
	}
}

// dep sends its idempotency key whichever way it goes: dep.rm honors one
// as dep.add does (decision 88), so a remove that reaches the server
// twice applies once.
func TestDepSendsItsKey(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"add", "remove"} {
		t.Run(action, func(t *testing.T) {
			f := &fakeConn{reply: func(string, any) (any, error) { return proto.Empty{}, nil }}
			cs, _ := connect(t, f)
			if res := callTool(t, cs, "dep", map[string]any{"action": action, "id": "ar-1", "depends_on": "ar-2"}); res.IsError {
				t.Fatalf("dep %s: %s", action, text(t, res))
			}
			if a, ok := f.calls[0].args.(proto.DepArgs); !ok || !strings.HasPrefix(a.Idem, "mcp-") {
				t.Errorf("dep %s sent %+v, want an idempotency key", action, f.calls[0].args)
			}
		})
	}
}

// finish passes ticked and waived items; a waived key that is not an item
// number is refused before anything is sent.
func TestFinishAcceptance(t *testing.T) {
	t.Parallel()
	f := &fakeConn{reply: func(string, any) (any, error) { return proto.FinishResult{ID: "ar-1", Rev: 3}, nil }}
	cs, d := connect(t, f)
	res := callTool(t, cs, "finish", map[string]any{"id": "ar-1", "ticked": []any{1, 3}, "waived": map[string]any{"2": "covered elsewhere"}})
	if res.IsError {
		t.Fatal(text(t, res))
	}
	got := f.calls[0].args.(proto.FinishArgs)
	if !slices.Equal(got.Ticked, []int{1, 3}) || !maps.Equal(got.Waived, map[int]string{2: "covered elsewhere"}) {
		t.Errorf("finish args %+v", got)
	}
	for _, bad := range []map[string]any{{"two": "x"}, {"0": "x"}, {"-1": "x"}} {
		if res := callTool(t, cs, "finish", map[string]any{"id": "ar-1", "waived": bad}); !res.IsError {
			t.Errorf("waived %v accepted", bad)
		}
	}
	if d.n != 1 || len(f.calls) != 1 {
		t.Errorf("refused calls reached the server: %d dials, %d calls", d.n, len(f.calls))
	}
}

// start and show list acceptance items in place of the prose, one line
// each; show and create name similar closed issues in one line.
func TestItemsAndSimilar(t *testing.T) {
	t.Parallel()
	items := []proto.AcceptanceItem{{N: 1, Text: "survives restart", State: "ticked", By: "alice"},
		{N: 2, Text: "docs", State: "waived", Reason: "none needed"}, {N: 3, Text: "has tests"}}
	similar := []proto.Summary{{ID: "ar-old1", Title: "Login fails", Status: "closed"}, {ID: "ar-old2", Title: "Token expiry", Status: "closed"}}
	f := &fakeConn{reply: func(op string, _ any) (any, error) {
		is := proto.Issue{ID: "ar-1", Title: "Login", Type: "bug", Status: "in_progress", Acceptance: "- [x] survives restart\n- docs\n- has tests"}
		switch op {
		case proto.OpStart:
			return proto.StartResult{Issue: is, Items: items}, nil
		case proto.OpShow:
			return proto.ShowResult{Issue: is, Items: items, Similar: similar}, nil
		}
		return proto.CreateResult{ID: "ar-2", Rev: 1, Similar: similar}, nil
	}}
	cs, _ := connect(t, f)
	want := []string{"1 [x] survives restart", "2 [waived: none needed] docs", "3 [ ] has tests"}

	var st Started
	if err := json.Unmarshal([]byte(text(t, callTool(t, cs, "start", map[string]any{"id": "ar-1"}))), &st); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Items, want) || st.Acceptance != "" {
		t.Errorf("start items %q, acceptance %q", st.Items, st.Acceptance)
	}
	var sh Issue
	if err := json.Unmarshal([]byte(text(t, callTool(t, cs, "show", map[string]any{"id": "ar-1"}))), &sh); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sh.Items, want) || sh.Acceptance != "" || sh.Similar != "ar-old1 Login fails; ar-old2 Token expiry" {
		t.Errorf("show: items %q, acceptance %q, similar %q", sh.Items, sh.Acceptance, sh.Similar)
	}
	if got := text(t, callTool(t, cs, "create", map[string]any{"title": "Login again"})); got != `{"untrusted":"`+untrustedNote+`","id":"ar-2","rev":1,"similar":"ar-old1 Login fails; ar-old2 Token expiry"}` {
		t.Errorf("create = %s", got)
	}
}

func TestAcceptanceErrorNamesTheArguments(t *testing.T) {
	t.Parallel()
	res := toolError(proto.Errf(proto.CodeAcceptance, "tick what is met with `aries accept ar-1 2`, or waive an item with `aries accept ar-1 N --waive REASON`",
		"issue ar-1 has acceptance items neither ticked nor waived: 2"))
	want := "acceptance: issue ar-1 has acceptance items neither ticked nor waived: 2\n" +
		"fix: call finish with ticked: [numbers met] and waived: {number: reason} for the rest; show lists the items"
	if got := text(t, res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// The descriptions tell the tools that end a session apart, and waiving
// apart from leaving an item open (aries-pidxyzub): finish is for the
// issue the agent holds and names handoff for when a check or a review
// still has to pass; handoff says it stops without closing; close names
// finish for one's own issue; waived is for an item that does not apply,
// and the instructions say an item that is a person's or CI's stays open;
// take says it is for the agent's own stopped session.
func TestDescriptionsSteer(t *testing.T) {
	t.Parallel()
	cs, _ := connect(t)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	param := func(tool, field string) string {
		t.Helper()
		var s struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		b, err := json.Marshal(tools[tool].InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		return s.Properties[field].Description
	}
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{"finish", tools["finish"].Description, []string{"hold", "handoff"}},
		{"handoff", tools["handoff"].Description, []string{"without closing", "done"}},
		{"close", tools["close"].Description, []string{"finish"}},
		{"finish.waived", param("finish", "waived"), []string{"does not apply"}},
		{"handoff.waived", param("handoff", "waived"), []string{"does not apply"}},
		{"start.take", param("start", "take"), []string{"stopped"}},
		{"instructions", Instructions, []string{"does not apply", "stays open", "handoff with state done"}},
	} {
		for _, want := range tc.want {
			if !strings.Contains(tc.text, want) {
				t.Errorf("%s = %q, want it to say %q", tc.name, tc.text, want)
			}
		}
	}
}
