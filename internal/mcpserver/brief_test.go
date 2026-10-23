package mcpserver

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// show gives the issue's brief as one line, the record and its claim,
// and never its text, which an agent reads with comments.
func TestShowCarriesTheBrief(t *testing.T) {
	t.Parallel()
	b := &proto.Brief{Comment: "c1", By: "alice", BySession: "s-coord", Harness: "claude-code-desktop", Model: "claude-opus-5-5",
		Effort: "high", Session: "local_1", Template: "brief@3", State: "bound", Text: "the whole brief",
		Claim: &proto.BriefClaim{By: "alice", Session: "s-work", Epoch: 2}}
	f := &fakeConn{reply: func(op string, _ any) (any, error) {
		if op == proto.OpShow {
			return proto.ShowResult{Issue: proto.Issue{ID: "ar-1", Title: "t"}, Brief: b}, nil
		}
		return nil, errors.New("unexpected " + op)
	}}
	cs, _ := connect(t, f)
	raw := text(t, callTool(t, cs, "show", map[string]any{"id": "ar-1", "full": true}))
	var is Issue
	if err := json.Unmarshal([]byte(raw), &is); err != nil {
		t.Fatal(err)
	}
	const want = "harness=claude-code-desktop model=claude-opus-5-5 effort=high session=local_1 template=brief@3; by alice/s-coord; started alice/s-work epoch 2"
	if is.Brief != want {
		t.Errorf("show's brief = %q\nwant %q", is.Brief, want)
	}
	if strings.Contains(raw, "the whole brief") {
		t.Errorf("show carries the brief's text: %s", raw)
	}
}
