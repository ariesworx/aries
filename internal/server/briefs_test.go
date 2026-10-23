package server

import (
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// A brief recorded over the op is joined to the claim the next start
// takes; show returns it, its text only with full.
func TestDispatchBrief(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	is := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	coord := store.Actor{Principal: "alice", Session: "s-coord", Machine: "laptop-a"}
	args := proto.BriefArgs{ID: is.ID, Harness: "claude-code-desktop", Model: "claude-opus-5-5", Effort: "high",
		Session: "local_1", Template: "brief@3", Text: "Do the work.\n"}
	got := mustCall[proto.BriefResult](t, s, coord, proto.OpBrief, args)
	if got.ID == "" || got.State != "pending" || got.Claim != nil || len(got.SHA256) != 64 {
		t.Fatalf("brief = %+v", got)
	}
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: is.ID})
	sh := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: is.ID})
	b := sh.Brief
	if b == nil || b.Comment != got.ID || b.By != "alice" || b.BySession != "s-coord" || b.Model != "claude-opus-5-5" ||
		b.Effort != "high" || b.State != "bound" || b.Claim == nil || b.Claim.Session != alice.Session || b.Claim.Epoch != 1 || b.Text != "" {
		t.Fatalf("show's brief = %+v", b)
	}
	full := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: is.ID, Full: true})
	if full.Brief == nil || full.Brief.Text != "Do the work." {
		t.Errorf("show --full's brief = %+v", full.Brief)
	}
}

// Each refusal of a brief is invalid with a Fix naming the next step.
func TestDispatchBriefRefusals(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{Limits: store.Limits{BriefBytes: 32}})
	is := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	held := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "held"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: held.ID})
	ok := proto.BriefArgs{ID: is.ID, Harness: "claude-code", Model: "claude-opus-5-5", Template: "brief@3", Text: "brief"}
	with := func(f func(*proto.BriefArgs)) proto.BriefArgs { a := ok; f(&a); return a }
	coord := store.Actor{Principal: "alice", Session: "s-coord", Machine: "laptop-a"}
	for _, tc := range []struct {
		name     string
		actor    store.Actor
		args     proto.BriefArgs
		msg, fix string
	}{
		{"over the cap", coord, with(func(a *proto.BriefArgs) { a.Text = strings.Repeat("x", 33) }), "brief must be 1-32 bytes", "brief_bytes under limits:"},
		{"a secret", coord, with(func(a *proto.BriefArgs) { a.Text = "key " + "AKIA" + "FAKEFAKEFAKEFAKE" }), "looks like it holds a secret", "never a secret in a brief"},
		{"its own claim", alice, with(func(a *proto.BriefArgs) { a.ID = held.ID }), "never briefs itself", "from its own session"},
		{"a bad effort", coord, with(func(a *proto.BriefArgs) { a.Effort = "huge" }), "effort must be one of", "aries dispatch brief -h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := call[proto.BriefResult](t, s, tc.actor, proto.OpBrief, tc.args)
			if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Fatalf("brief = %+v; want invalid naming %q, fix with %q", perr, tc.msg, tc.fix)
			}
		})
	}
}

// The brief op is the CLI's: a sign-in of scope mcp or ci is refused it,
// so no agent's tool and no CI job records a brief.
func TestBriefIsCLIOnly(t *testing.T) {
	t.Parallel()
	for _, sc := range []string{proto.ScopeMCP, proto.ScopeCI} {
		if perr := mayCall(&signedIn{scopes: []string{sc}}, proto.OpBrief); perr == nil || perr.Code != proto.CodeForbidden {
			t.Errorf("brief to a sign-in of scope %s = %v, want forbidden", sc, perr)
		}
	}
	if perr := mayCall(&signedIn{scopes: []string{proto.ScopeCLI}}, proto.OpBrief); perr != nil {
		t.Errorf("brief to a sign-in of scope cli = %v", perr)
	}
}
