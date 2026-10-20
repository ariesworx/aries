package dispatch

import (
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Action is what a wake does with a ready issue.
type Action string

const (
	// ActStart starts a run on it (or, in a dry run, would).
	ActStart Action = "start"
	// ActSkip passes it over, by Rule.
	ActSkip Action = "skip"
	// ActQueue would start it, but no slot is free.
	ActQueue Action = "queue"
)

// Decision is what a wake does with one issue, and why.
type Decision struct {
	Action Action
	Issue  string
	// Model is the run's model, for a start or a queue.
	Model string
	// Rule names what skipped or queued it; Why says it for a person, in
	// the dispatcher's words, never the issue's.
	Rule string
	Why  string
}

// The rules that skip or queue an issue (dispatcher.md §4; the memory
// `dispatch`, rules 1 and 2).
const (
	RuleRunning      = "running"
	RuleNotOpen      = "not-open"
	RuleEpic         = "epic"
	RuleHuman        = "human"
	RuleWaiting      = "waiting:maintainer"
	RuleHandedOff    = "handed-off"
	RuleSigning      = "release-signing"
	RuleClaimed      = "claimed"
	RuleModels       = "models"
	RuleNoModel      = "no-model"
	RuleFailedBefore = "failed-before"
	RuleUnreadable   = "unreadable"
	RuleHeld         = "held"
	RuleSlots        = "slots"
	RuleLaunch       = "launch"
)

// why says each rule for a person.
var why = map[string]string{
	RuleRunning:      "one of this dispatcher's runs works it",
	RuleNotOpen:      "it is not open",
	RuleEpic:         "an epic is planned by a person, not run",
	RuleHuman:        "labeled human: a person's to do",
	RuleWaiting:      "labeled waiting:maintainer: it waits on the maintainer's answers",
	RuleHandedOff:    "its last handoff is done: it waits on review or landing",
	RuleSigning:      "it names release signing, which agents never touch (rule 3)",
	RuleClaimed:      "another session holds its claim",
	RuleModels:       "it has more than one model label; a person picks one",
	RuleNoModel:      "it has no model label (model:haiku, model:sonnet or model:opus); a person labels it",
	RuleFailedBefore: "a run of this dispatcher on it failed or was stopped; a person looks first",
	RuleUnreadable:   "show failed, so it could not be judged",
	RuleHeld:         "another session claimed it first",
	RuleSlots:        "no slot of the lock " + RunsLock + " is free",
	RuleLaunch:       "the run could not be started",
}

// models maps a model label to the harness's model alias.
var models = map[string]string{
	"model:haiku":  "haiku",
	"model:sonnet": "sonnet",
	"model:opus":   "opus",
}

// signing are the words that tie an issue to release signing, which holds
// it for a person rather than guessing (dispatcher.md §4).
var signing = []string{"internal/release", "keys.go", "release workflow", ".github/workflows/release.yml"}

// Judge applies the picking rules to one ready issue: the first rule that
// fits skips it; an issue no rule skips is started with the model its
// label names. running reports whether one of the dispatcher's runs works
// an issue. It reads nothing but its arguments.
func Judge(sr proto.ShowResult, running func(id string) bool, now time.Time) Decision {
	is := sr.Issue
	skip := func(rule string) Decision {
		return Decision{Action: ActSkip, Issue: is.ID, Rule: rule, Why: why[rule]}
	}
	switch {
	case running(is.ID):
		return skip(RuleRunning)
	case is.Status != "open":
		return skip(RuleNotOpen)
	case is.Type == "epic":
		return skip(RuleEpic)
	case slices.Contains(is.Labels, "human"):
		return skip(RuleHuman)
	case slices.Contains(is.Labels, "waiting:maintainer"):
		return skip(RuleWaiting)
	case sr.Handoff != nil && sr.Handoff.State == "done":
		return skip(RuleHandedOff)
	case namesSigning(is):
		return skip(RuleSigning)
	case sr.Claim != nil && sr.Claim.ExpiresAt.After(now):
		return skip(RuleClaimed)
	}
	var found []string
	for _, l := range is.Labels {
		if m, ok := models[l]; ok && !slices.Contains(found, m) {
			found = append(found, m)
		}
	}
	switch len(found) {
	case 0:
		return skip(RuleNoModel)
	case 1:
		return Decision{Action: ActStart, Issue: is.ID, Model: found[0]}
	}
	return skip(RuleModels)
}

// namesSigning reports whether the issue's text names release signing.
func namesSigning(is proto.Issue) bool {
	text := strings.ToLower(strings.Join([]string{is.Title, is.Body, is.Design, is.Acceptance, is.Notes}, "\n"))
	for _, w := range signing {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}
