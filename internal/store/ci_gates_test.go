package store

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// A CI job: a principal of its own, in the session CI's sfx takes.
var (
	ciJob   = Actor{Principal: "ci-example", Session: proto.CISession, Machine: "runner-1"}
	ciOther = Actor{Principal: "ci-other", Session: proto.CISession, Machine: "runner-2"}
)

// ciGate adds a ci gate for check on subject, reported by ci-example,
// holding blocks back (none when empty).
func ciGate(t *testing.T, s *Store, subject, blocks IssueID, check string) Gate {
	t.Helper()
	return mustGate(t, s, alice, NewGate{Kind: GateCI, Check: check, Subject: subject, Blocks: blocks,
		Approvers: []string{"ci-example"}})
}

// withPassword is a link to a run with an invented user and password in
// it, which a report must not carry.
var withPassword = (&url.URL{Scheme: "https", User: url.UserPassword("bob", "invented"), Host: "ci.example.com", Path: "/runs/1"}).String()

// inboxOf is principal a's unread items of kind k.
func inboxOf(t *testing.T, s *Store, a Actor, k InboxKind) []InboxItem {
	t.Helper()
	page, err := s.Inbox(t.Context(), a, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []InboxItem
	for _, it := range page.Items {
		if it.Kind == k {
			out = append(out, it)
		}
	}
	return out
}

// A ci gate holds what depends on it until the CI principal it names
// reports its check passed on the subject; the report keeps the
// evidence, and the dependent becomes ready.
func TestCIGatePassMakesDependentReady(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	impl := mustCreate(t, s, NewIssue{Title: "implement"})
	next := mustCreate(t, s, NewIssue{Title: "build on it"})
	g := ciGate(t, s, impl.ID, next.ID, "build")
	if g.Kind != GateCI || g.Check != "build" || g.Subject != impl.ID || g.Title != "build of "+string(impl.ID) {
		t.Fatalf("AddGate(ci) = %+v, want a ci gate for build of %s", g, impl.ID)
	}
	if got := readyIDs(t, s); slices.Contains(got, next.ID) || !slices.Contains(got, impl.ID) {
		t.Fatalf("Ready behind the ci gate = %v, want %s and not %s", got, impl.ID, next.ID)
	}
	r := Report{Subject: impl.ID, Check: "build", SHA: "0123abcd0123abcd0123abcd0123abcd0123abcd",
		URL: "https://ci.example.com/runs/42", Reason: "all green"}
	passed, err := s.PassCheck(ctx, ciJob, r)
	if err != nil {
		t.Fatalf("PassCheck(%+v) by ci-example: %v", r, err)
	}
	if passed.ID != g.ID || passed.Outcome != GatePassed || passed.By != "ci-example" {
		t.Errorf("PassCheck = %+v, want %s passed by ci-example", passed, g.ID)
	}
	if rep := passed.Report; rep == nil || rep.Result != CheckPassed || rep.SHA != r.SHA || rep.URL != r.URL || rep.Reason != r.Reason {
		t.Errorf("PassCheck report = %+v, want passed with the evidence of %+v", passed.Report, r)
	}
	if got := readyIDs(t, s); !slices.Contains(got, next.ID) {
		t.Errorf("Ready after the pass = %v, want %s", got, next.ID)
	}
	// A rerun that passes again changes nothing.
	again, err := s.PassCheck(ctx, ciJob, r)
	if err != nil || again.Outcome != GatePassed {
		t.Errorf("PassCheck of a passed gate = %+v, %v, want it left passed", again, err)
	}
	evs, err := s.History(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ops []Op
	for _, e := range evs {
		ops = append(ops, e.Op)
	}
	if want := []Op{OpIssueCreate, OpGateAdd, OpGatePass, OpIssueClose}; !slices.Equal(ops, want) {
		t.Errorf("ci gate history = %v, want %v", ops, want)
	}
}

// Who reports a ci gate: only the principals it names, from the session
// of a CI job or a person's terminal. Not an admin it does not name, who
// waives it instead, nor its creator, nor anyone from an agent's session.
// A human gate is unchanged: the session ci does not pass it.
func TestCIGateWho(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	tests := []struct {
		name string
		by   Actor
		want GateReason // "" reports
	}{
		{"the named CI principal from its job", ciJob, ""},
		{"the named CI principal from a terminal", Actor{Principal: "ci-example", Session: proto.CLISession, Machine: "laptop"}, ""},
		{"the named CI principal from an agent's session", Actor{Principal: "ci-example", Session: "s1", Machine: "m"}, GateNotCI},
		{"another CI principal", ciOther, GateNotApprover},
		{"an admin not named", danaCLI, GateNotApprover},
		{"its creator", aliceCLI, GateNotApprover},
	}
	for _, tc := range tests {
		for _, fail := range []bool{false, true} {
			name := tc.name + " passes"
			report := s.PassCheck
			if fail {
				name, report = tc.name+" fails", s.FailCheck
			}
			t.Run(name, func(t *testing.T) {
				is := mustCreate(t, s, NewIssue{Title: tc.name})
				g := ciGate(t, s, is.ID, "", "build")
				_, err := report(ctx, tc.by, Report{Gate: g.ID, URL: "https://ci.example.com/runs/1"})
				switch {
				case tc.want == "" && err != nil:
					t.Errorf("report by %s/%s = %v, want it taken", tc.by.Principal, tc.by.Session, err)
				case tc.want != "" && !gateErr(err, tc.want):
					t.Errorf("report by %s/%s = %v, want a GateError (%s)", tc.by.Principal, tc.by.Session, err, tc.want)
				case tc.want != "" && !errors.Is(err, ErrForbidden):
					t.Errorf("report by %s/%s = %v, want it to wrap ErrForbidden", tc.by.Principal, tc.by.Session, err)
				}
			})
		}
	}
	t.Run("a human gate's approver from the session ci", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "human"})
		g := mustGate(t, s, alice, NewGate{Blocks: is.ID, Approvers: []string{"carol"}})
		carolCI := Actor{Principal: "carol", Session: proto.CISession, Machine: "runner"}
		if _, err := s.PassGate(ctx, carolCI, g.ID, ""); !gateErr(err, GateNotPerson) {
			t.Errorf("PassGate(human) by carol from ci = %v, want a GateError (not a person)", err)
		}
		if _, err := s.FailCheck(ctx, carolCLI, Report{Gate: g.ID}); !errors.Is(err, ErrInvalid) {
			t.Errorf("FailCheck(human gate) = %v, want ErrInvalid", err)
		}
	})
	t.Run("pass by id of a ci gate", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "by id"})
		g := ciGate(t, s, is.ID, "", "test")
		if out, err := s.PassGate(ctx, ciJob, g.ID, "rerun by hand"); err != nil || out.Outcome != GatePassed || out.Report == nil {
			t.Errorf("PassGate(ci gate) by ci-example = %+v, %v, want it passed with a report", out, err)
		}
	})
	t.Run("an admin waives a ci gate", func(t *testing.T) {
		is := mustCreate(t, s, NewIssue{Title: "waived"})
		g := ciGate(t, s, is.ID, "", "build")
		if out, err := s.WaiveGate(ctx, danaCLI, g.ID, "CI is down"); err != nil || out.Outcome != GateWaived {
			t.Errorf("WaiveGate(ci gate) by dana = %+v, %v, want it waived", out, err)
		}
		if _, err := s.FailCheck(ctx, ciJob, Report{Gate: g.ID}); !errors.Is(err, ErrInvalid) {
			t.Errorf("FailCheck of a waived gate = %v, want ErrInvalid", err)
		}
	})
}

// A failed check tells the subject's holder, in the holder's session, or
// its assignee when nobody holds it, with the evidence; the gate stays
// open. A later pass closes it and tells them it passed. A failure after
// a pass, a new commit's, reopens the gate and holds the dependent again.
func TestCIGateFailTellsHolder(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	held := mustCreate(t, s, NewIssue{Title: "held work"})
	if _, _, err := s.StartIssue(ctx, bob, held.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	assigned := mustCreate(t, s, NewIssue{Title: "assigned work", Assignee: "carol"})
	tests := []struct {
		name    string
		subject Issue
		told    Actor
	}{
		{"the holder", held, bob},
		{"the assignee", assigned, carol},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := mustCreate(t, s, NewIssue{Title: "after " + tc.name})
			g := ciGate(t, s, tc.subject.ID, next.ID, "build")
			r := Report{Subject: tc.subject.ID, Check: "build", SHA: "abc1234", URL: "https://ci.example.com/runs/7", Reason: "2 tests failed"}
			failed, err := s.FailCheck(ctx, ciJob, r)
			if err != nil {
				t.Fatalf("FailCheck(%+v): %v", r, err)
			}
			if failed.Outcome != "" || failed.Report == nil || failed.Report.Result != CheckFailed || failed.Report.URL != r.URL {
				t.Errorf("FailCheck = %+v, want an open gate with the failed report", failed)
			}
			items := inboxOf(t, s, tc.told, InboxGateFailed)
			if len(items) != 1 || items[0].Issue != tc.subject.ID || !strings.Contains(items[0].Body, r.URL) ||
				!strings.Contains(items[0].Body, r.Reason) || !strings.Contains(items[0].Body, "abc1234") {
				t.Errorf("%s's gate.failed items = %+v, want one for %s with the evidence", tc.told.Principal, items, tc.subject.ID)
			}
			if got := readyIDs(t, s); slices.Contains(got, next.ID) {
				t.Errorf("Ready after the failure = %v, want %s held", got, next.ID)
			}
			if _, err := s.PassCheck(ctx, ciJob, Report{Subject: tc.subject.ID, Check: "build", SHA: "def5678"}); err != nil {
				t.Fatal(err)
			}
			if items := inboxOf(t, s, tc.told, InboxGatePassed); !slices.ContainsFunc(items, func(it InboxItem) bool {
				return it.Issue == tc.subject.ID && strings.Contains(it.Body, "build")
			}) {
				t.Errorf("%s's gate.passed items after the fix = %+v, want one for %s", tc.told.Principal, items, tc.subject.ID)
			}
			reopened, err := s.FailCheck(ctx, ciJob, Report{Subject: tc.subject.ID, Check: "build", SHA: "0ff1ce0"})
			if err != nil || reopened.Outcome != "" {
				t.Fatalf("FailCheck after a pass = %+v, %v, want the gate open again", reopened, err)
			}
			if is, _ := s.GetIssue(ctx, g.ID); is.Status != StatusOpen {
				t.Errorf("gate after a later failure is %s, want open", is.Status)
			}
			if got := readyIDs(t, s); slices.Contains(got, next.ID) {
				t.Errorf("Ready after a later failure = %v, want %s held again", got, next.ID)
			}
		})
	}
}

// What a ci gate must name, and that a subject has one gate per check.
func TestCIGateAdd(t *testing.T) {
	t.Parallel()
	s := openStore(t, newDSN(t), Options{Limits: Limits{GateChecks: 2}})
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})
	by := []string{"ci-example"}
	tests := []struct {
		name string
		in   NewGate
		want error
	}{
		{"no check", NewGate{Kind: GateCI, Subject: a.ID, Blocks: b.ID, Approvers: by}, ErrInvalid},
		{"a check that is no name", NewGate{Kind: GateCI, Check: "Build All", Subject: a.ID, Approvers: by}, ErrInvalid},
		{"no subject", NewGate{Kind: GateCI, Check: "build", Blocks: b.ID, Approvers: by}, ErrInvalid},
		{"nobody to report it", NewGate{Kind: GateCI, Check: "build", Subject: a.ID, Blocks: b.ID}, ErrInvalid},
		{"its subject held by itself", NewGate{Kind: GateCI, Check: "build", Subject: a.ID, Blocks: a.ID, Approvers: by}, ErrInvalid},
		{"a subject that does not exist", NewGate{Kind: GateCI, Check: "build", Subject: "tst-zzzz", Approvers: by}, ErrNotFound},
		{"an unknown kind", NewGate{Kind: "timer", Blocks: b.ID}, ErrInvalid},
		{"a check on a human gate", NewGate{Check: "build", Blocks: b.ID}, ErrInvalid},
		{"a build gate holding b", NewGate{Kind: GateCI, Check: "build", Subject: a.ID, Blocks: b.ID, Approvers: by}, nil},
		{"a second build gate on a", NewGate{Kind: GateCI, Check: "build", Subject: a.ID, Approvers: by}, ErrInvalid},
		{"a landed gate holding nothing", NewGate{Kind: GateCI, Check: "landed", Subject: a.ID, Approvers: by}, nil},
		{"checks past the cap", NewGate{Kind: GateCI, Check: "test", Subject: a.ID, Approvers: by}, ErrInvalid},
	}
	for _, tc := range tests {
		_, err := s.AddGate(ctx, alice, tc.in)
		if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: AddGate(%+v) = %v, want %v", tc.name, tc.in, err, tc.want)
		}
	}
	if _, err := s.PassCheck(ctx, ciJob, Report{Subject: b.ID, Check: "build"}); !gateErr(err, GateNoCheck) || !errors.Is(err, ErrNotFound) {
		t.Errorf("PassCheck of a subject with no such check = %v, want a GateError (no check) wrapping ErrNotFound", err)
	}
	if _, err := s.PassCheck(ctx, ciJob, Report{Check: "build"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("PassCheck naming no gate and no subject = %v, want ErrInvalid", err)
	}
	build, err := s.ChecksOf(ctx, a.ID)
	if err != nil || len(build) == 0 || build[0].Check != "build" {
		t.Fatalf("ChecksOf(%s) = %+v, %v; want its build gate first", a.ID, build, err)
	}
	for _, r := range []Report{{Gate: build[0].ID, Check: "landed"}, {Gate: build[0].ID, Subject: b.ID}} {
		if _, err := s.PassCheck(ctx, ciJob, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("PassCheck(%+v), a gate and another check or subject = %v, want ErrInvalid", r, err)
		}
	}
}

// Evidence is bounded (gate_evidence) and holds no secret (rule 2): a URL
// is http or https with no user or password, a commit is hex, and the
// reason is one line.
func TestCheckEvidence(t *testing.T) {
	t.Parallel()
	s := openStore(t, newDSN(t), Options{Limits: Limits{GateEvidence: 120}})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "subject"})
	ciGate(t, s, is.ID, "", "build")
	tests := []struct {
		name string
		r    Report
		ok   bool
	}{
		{"a run's URL, a commit and a reason", Report{URL: "https://ci.example.com/example/repo/actions/runs/1234567890", SHA: "0123abc", Reason: "lint failed"}, true},
		{"no evidence", Report{}, true},
		{"a URL with a password", Report{URL: withPassword}, false},
		{"a URL with a user", Report{URL: "https://bob@ci.example.com/runs/1"}, false},
		{"a URL of another scheme", Report{URL: "file:///etc/passwd"}, false},
		{"a URL that is no URL", Report{URL: "not a url"}, false},
		{"a commit that is not hex", Report{SHA: "main"}, false},
		{"a reason with a token", Report{Reason: "token ghp_" + strings.Repeat("a1B2", 9)}, false},
		{"a reason of two lines", Report{Reason: "one\ntwo"}, false},
		{"evidence past the cap", Report{URL: "https://ci.example.com/" + strings.Repeat("r", 100), Reason: "x"}, false},
	}
	for _, tc := range tests {
		r := tc.r
		r.Subject, r.Check = is.ID, "build"
		_, err := s.FailCheck(ctx, ciJob, r)
		if tc.ok && err != nil || !tc.ok && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: FailCheck(%+v) = %v, want ok %v", tc.name, r, err, tc.ok)
		}
	}
}

// ChecksOf lists the ci gates whose subject an issue is, in every state:
// how a closed issue shows whether it landed.
func TestChecksOf(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "subject"})
	build := ciGate(t, s, is.ID, "", "build")
	landed := ciGate(t, s, is.ID, "", "landed")
	if _, err := s.PassCheck(ctx, ciJob, Report{Subject: is.ID, Check: "build"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CloseIssue(ctx, alice, is.ID, 0, "done"); err != nil {
		t.Fatal(err)
	}
	cs, err := s.ChecksOf(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].ID != build.ID || cs[0].Outcome != GatePassed || cs[1].ID != landed.ID || cs[1].Outcome != "" {
		t.Fatalf("ChecksOf(closed, not landed) = %+v, want build passed and landed open", cs)
	}
	if _, err := s.PassCheck(ctx, ciJob, Report{Subject: is.ID, Check: "landed", SHA: "fedcba9"}); err != nil {
		t.Fatalf("PassCheck(landed) of a closed subject: %v", err)
	}
	cs, err = s.ChecksOf(ctx, is.ID)
	if err != nil || len(cs) != 2 || cs[1].Outcome != GatePassed || cs[1].Report.SHA != "fedcba9" {
		t.Errorf("ChecksOf(landed) = %+v, %v, want landed passed at fedcba9", cs, err)
	}
}
