package server

import (
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// ciJob is a CI job: a principal of its own, in the session CI's sfx
// takes.
var ciJob = store.Actor{Principal: "ci-example", Session: proto.CISession, Machine: "runner-1"}

// A CI job reports a check by its branch: the server finds the issue the
// branch names, by the project's prefix, and that issue's gate for the
// check. A branch that names no issue, an issue with no such gate, a
// principal the gate does not name, and an agent's session are each
// refused with a fix; the pass makes the dependent ready, and a failure
// shows on the subject with its evidence.
func TestCIGateFromBranch(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	impl := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "implement"})
	next := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "build on it"})
	g := mustCall[proto.Gate](t, s, alice, proto.OpGateAdd, proto.GateAddArgs{Kind: "ci", Check: "build", Subject: impl.ID,
		Blocks: next.ID, Approvers: []string{"ci-example"}})
	if g.Kind != "ci" || g.Check != "build" || g.Subject != impl.ID || !slices.Equal(g.Holds, []string{next.ID}) {
		t.Fatalf("gate.add(ci) = %+v, want a build gate on %s holding %s", g, impl.ID, next.ID)
	}
	branch := "feature/" + impl.ID + "-implement"
	refused := []struct {
		name string
		by   store.Actor
		args proto.GateArgs
		code proto.Code
		// msg is in the message, and fix in the fix.
		msg, fix string
	}{
		{"a branch that names no issue", ciJob, proto.GateArgs{Check: "build", Branch: "main"}, proto.CodeNotFound,
			"branch main names no issue", "sfx start --branch"},
		{"a branch naming an issue with no such gate", ciJob, proto.GateArgs{Check: "build", Branch: "fix/" + next.ID}, proto.CodeNotFound,
			next.ID + " has no ci gate for check build", "sfx gate add --kind ci --check build --subject " + next.ID},
		{"a check of no gate", ciJob, proto.GateArgs{Check: "test", Branch: branch}, proto.CodeNotFound,
			"check test", "sfx gate list"},
		{"a principal the gate does not name", bob, proto.GateArgs{Check: "build", Branch: branch}, proto.CodeForbidden,
			"ci-example", proto.FixGate},
		{"an agent's session", store.Actor{Principal: "ci-example", Session: "s-1", Machine: "m"},
			proto.GateArgs{Check: "build", Branch: branch}, proto.CodeForbidden, "agent's session", "CI"},
		{"a branch and another issue", ciJob, proto.GateArgs{Check: "build", Branch: branch, Subject: next.ID}, proto.CodeInvalid,
			"names " + impl.ID, "one of"},
		{"evidence with a password", ciJob, proto.GateArgs{Check: "build", Branch: branch,
			URL: (&url.URL{Scheme: "https", User: url.UserPassword("u", "invented"), Host: "ci.example.com"}).String()},
			proto.CodeInvalid, "user or a password", ""},
	}
	for _, tc := range refused {
		_, perr := call[proto.Gate](t, s, tc.by, proto.OpGatePass, tc.args)
		if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
			t.Errorf("%s: gate.pass(%+v) by %s/%s = %+v, want %s naming %q, its fix %q",
				tc.name, tc.args, tc.by.Principal, tc.by.Session, perr, tc.code, tc.msg, tc.fix)
		}
	}

	failed := mustCall[proto.Gate](t, s, ciJob, proto.OpGateFail, proto.GateArgs{Check: "build", Branch: branch,
		SHA: "abc1234", URL: "https://ci.example.com/runs/7", Reason: "2 tests failed"})
	if failed.Outcome != "" || failed.Report == nil || failed.Report.Result != "failed" || failed.Report.URL != "https://ci.example.com/runs/7" {
		t.Errorf("gate.fail = %+v, want an open gate with the failed report", failed)
	}
	sh := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: impl.ID})
	if len(sh.Checks) != 1 || sh.Checks[0].ID != g.ID || sh.Checks[0].Report == nil || sh.Checks[0].Report.Reason != "2 tests failed" {
		t.Errorf("show(subject).checks = %+v, want build with the failure", sh.Checks)
	}
	passed := mustCall[proto.Gate](t, s, ciJob, proto.OpGatePass, proto.GateArgs{Check: "build", Branch: branch, SHA: "def5678"})
	if passed.Outcome != "passed" || passed.Report == nil || passed.Report.SHA != "def5678" {
		t.Errorf("gate.pass = %+v, want passed at def5678", passed)
	}
	ready := mustCall[proto.ListResult](t, s, alice, proto.OpReady, proto.LimitArgs{})
	if !slices.ContainsFunc(ready.Issues, func(sm proto.Summary) bool { return sm.ID == next.ID }) {
		t.Errorf("ready after the pass = %+v, want %s", ready.Issues, next.ID)
	}
	// A human gate is passed in person; the session ci does not pass it.
	h := mustCall[proto.Gate](t, s, alice, proto.OpGateAdd, proto.GateAddArgs{Blocks: next.ID, Approvers: []string{"carol"}})
	carolCI := store.Actor{Principal: "carol", Session: proto.CISession, Machine: "runner"}
	if _, perr := call[proto.Gate](t, s, carolCI, proto.OpGatePass, proto.GateArgs{ID: h.ID}); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("gate.pass(human) from the session ci = %+v, want forbidden", perr)
	}
	if _, perr := call[proto.Gate](t, s, carolCLI, proto.OpGateFail, proto.GateArgs{ID: h.ID}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("gate.fail(human) = %+v, want invalid", perr)
	}
}

// A project whose prefix has a hyphen: the branch's issue is found by it.
func TestCIGateBranchWithHyphenatedPrefix(t *testing.T) {
	t.Parallel()
	dsn := newDatabase(t)
	st, err := store.Open(t.Context(), dsn, store.Options{Prefix: "web-app", CommitInterval: -1, Admins: []string{dana.Principal}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(Config{Store: st, Project: project, Version: "v0.2.0", PeerCheck: func(net.Conn) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	impl := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "implement"})
	mustCall[proto.Gate](t, s, alice, proto.OpGateAdd, proto.GateAddArgs{Kind: "ci", Check: "landed", Subject: impl.ID,
		Approvers: []string{"ci-example"}})
	if out, perr := call[proto.Gate](t, s, ciJob, proto.OpGatePass, proto.GateArgs{Check: "landed",
		Branch: "alice/feature/" + impl.ID + "-implement"}); perr != nil || out.Outcome != "passed" {
		t.Errorf("gate.pass by a branch of prefix web-app = %+v, %+v; want passed", out, perr)
	}
}
