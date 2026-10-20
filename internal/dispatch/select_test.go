package dispatch

import (
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Each rule of the memory `dispatch` that picking applies (design §4),
// in the order Judge applies them: the first that fits is the one named.
func TestJudge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	issue := func(typ string, labels ...string) proto.ShowResult {
		return proto.ShowResult{Issue: proto.Issue{ID: "ar-a1", Title: "Fix the login redirect", Type: typ, Status: "open", Labels: labels}}
	}
	with := func(sr proto.ShowResult, f func(*proto.ShowResult)) proto.ShowResult {
		f(&sr)
		return sr
	}
	tests := []struct {
		name    string
		sr      proto.ShowResult
		running bool
		action  Action
		rule    string
		model   string
	}{
		{name: "opus", sr: issue("feature", "model:opus"), action: ActStart, model: "opus"},
		{name: "sonnet among other labels", sr: issue("bug", "ui", "model:sonnet"), action: ActStart, model: "sonnet"},
		{name: "haiku", sr: issue("task", "model:haiku"), action: ActStart, model: "haiku"},
		{name: "no model label waits for a person", sr: issue("bug", "ui"), action: ActSkip, rule: RuleNoModel},
		{name: "an unknown model label is none", sr: issue("bug", "model:gpt"), action: ActSkip, rule: RuleNoModel},
		{name: "two model labels are a person's to settle", sr: issue("bug", "model:opus", "model:haiku"), action: ActSkip, rule: RuleModels},
		{name: "the same model twice is one", sr: issue("bug", "model:opus", "model:opus"), action: ActStart, model: "opus"},
		{name: "human", sr: issue("bug", "model:opus", "human"), action: ActSkip, rule: RuleHuman},
		{name: "epic", sr: issue("epic", "model:opus"), action: ActSkip, rule: RuleEpic},
		{name: "waiting on the maintainer", sr: issue("task", "model:opus", "waiting:maintainer"), action: ActSkip, rule: RuleWaiting},
		{name: "handed off done waits on a person or landing",
			sr: with(issue("feature", "model:opus"), func(sr *proto.ShowResult) {
				sr.Handoff = &proto.Handoff{HandoffFields: proto.HandoffFields{State: "done"}}
			}), action: ActSkip, rule: RuleHandedOff},
		{name: "handed off partial is work to continue",
			sr: with(issue("feature", "model:opus"), func(sr *proto.ShowResult) {
				sr.Handoff = &proto.Handoff{HandoffFields: proto.HandoffFields{State: "partial"}}
			}), action: ActStart, model: "opus"},
		{name: "release signing in the body",
			sr:     with(issue("task", "model:opus"), func(sr *proto.ShowResult) { sr.Issue.Body = "Rotate the key in internal/release/keys.go" }),
			action: ActSkip, rule: RuleSigning},
		{name: "release workflow in the title",
			sr:     with(issue("task", "model:opus"), func(sr *proto.ShowResult) { sr.Issue.Title = "Pin an action in the Release workflow" }),
			action: ActSkip, rule: RuleSigning},
		{name: "release signing in the notes",
			sr:     with(issue("task", "model:opus"), func(sr *proto.ShowResult) { sr.Issue.Notes = "touches .github/workflows/release.yml" }),
			action: ActSkip, rule: RuleSigning},
		{name: "a live claim",
			sr: with(issue("task", "model:opus"), func(sr *proto.ShowResult) {
				sr.Claim = &proto.Claim{By: "bob", Session: "s1", ExpiresAt: now.Add(time.Minute)}
			}), action: ActSkip, rule: RuleClaimed},
		{name: "a lapsed claim holds nothing",
			sr: with(issue("task", "model:opus"), func(sr *proto.ShowResult) {
				sr.Claim = &proto.Claim{By: "bob", Session: "s1", ExpiresAt: now.Add(-time.Minute)}
			}), action: ActStart, model: "opus"},
		{name: "closed", sr: with(issue("task", "model:opus"), func(sr *proto.ShowResult) { sr.Issue.Status = "closed" }),
			action: ActSkip, rule: RuleNotOpen},
		{name: "one of its runs works it", sr: issue("task", "model:opus"), running: true, action: ActSkip, rule: RuleRunning},
		// Precedence: the first rule that fits is named.
		{name: "running before human", sr: issue("task", "human"), running: true, action: ActSkip, rule: RuleRunning},
		{name: "epic before no model", sr: issue("epic"), action: ActSkip, rule: RuleEpic},
		{name: "human before waiting", sr: issue("task", "waiting:maintainer", "human"), action: ActSkip, rule: RuleHuman},
		{name: "signing before no model",
			sr:     with(issue("task"), func(sr *proto.ShowResult) { sr.Issue.Body = "see internal/release" }),
			action: ActSkip, rule: RuleSigning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := Judge(tc.sr, func(string) bool { return tc.running }, now)
			if d.Action != tc.action || d.Rule != tc.rule || d.Model != tc.model {
				t.Fatalf("Judge = %+v; want action %s rule %q model %q", d, tc.action, tc.rule, tc.model)
			}
			if d.Issue != "ar-a1" {
				t.Errorf("Judge names %q, not the issue", d.Issue)
			}
			if d.Action == ActSkip && d.Why == "" {
				t.Errorf("a skip by %s says no why", d.Rule)
			}
		})
	}
}

// Every rule says why in words of its own, never the issue's text.
func TestRulesSayWhy(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{RuleRunning, RuleNotOpen, RuleEpic, RuleHuman, RuleWaiting, RuleHandedOff,
		RuleSigning, RuleClaimed, RuleModels, RuleNoModel, RuleFailedBefore, RuleUnreadable, RuleHeld, RuleSlots, RuleLaunch} {
		if why[rule] == "" {
			t.Errorf("rule %s has no why", rule)
		}
	}
}
