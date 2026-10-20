// Package outcome builds the outcome record of an issue
// (docs/design/outcome-record.md, slice O1): what each outcome of an
// issue (landed, closed, or its change closed unmerged) led to, from
// Aries's event log, comments and usage and from the CI host, computed
// by rule, and carried as an `outcome:` comment on the issue.
//
// The package reads Aries through Store and the CI host through Host;
// the GitHub host (github.go) runs `gh` and is the only part that knows
// a host. Nothing here talks to ariesd but through the ops a client
// already sends, so the record needs no protocol change, and ariesd never
// reads a CI host (decision 67). A record names sessions and the kind of
// each session's principal, never a person or a reviewer (decision 97).
package outcome

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// V is the record's version: it changes only when a field's meaning
// does.
const V = 1

// Prefix begins an outcome comment.
const Prefix = "outcome:"

// Outcomes (§1).
const (
	Landed   = "landed"
	Closed   = "closed"
	PRClosed = "pr-closed"
)

// Capturer names what captured a record.
const Capturer = "aries outcome"

// Record is one outcome of an issue, covering the issue's life up to it
// (§2). Times are RFC 3339 UTC; durations seconds.
type Record struct {
	V          int       `json:"v"`
	Issue      string    `json:"issue"`
	Type       string    `json:"type"`
	Epic       string    `json:"epic,omitempty"`
	Seq        int       `json:"seq"`
	Outcome    string    `json:"outcome"`
	Supersedes string    `json:"supersedes,omitempty"`
	Rev        int64     `json:"rev"`
	CapturedAt time.Time `json:"captured_at"`
	Capturer   string    `json:"capturer"`
	Backfill   bool      `json:"backfill,omitempty"`

	Change        *ChangeRef     `json:"change"`
	Brief         *Brief         `json:"brief"`
	Runs          []RunRecord    `json:"runs"`
	Times         Times          `json:"times"`
	HeldS         int64          `json:"held_s"`
	CycleS        int64          `json:"cycle_s"`
	CI            *CI            `json:"ci"`
	DefaultBranch *DefaultBranch `json:"default_branch"`
	Review        *Review        `json:"review"`
	Reopens       []Reopen       `json:"reopens"`
	Waived        Waived         `json:"waived"`
	Tokens        *Tokens        `json:"tokens"`
	Rework        Rework         `json:"rework"`
	Lessons       int            `json:"lessons"`
	Missing       []Missing      `json:"missing"`
}

// ChangeRef names the change a record is about.
type ChangeRef struct {
	Host string `json:"host"`
	ID   string `json:"id"`
	URL  string `json:"url"`
	Base string `json:"base"`
}

// Brief is the hash of the brief a run was given, and where it was read:
// "comment", a `brief:` comment, until briefs are recorded.
type Brief struct {
	SHA256 string `json:"sha256"`
	From   string `json:"from"`
}

// RunRecord is one session's holds of the issue (§1).
type RunRecord struct {
	Session   string    `json:"session"`
	Kind      string    `json:"kind"`
	Harness   string    `json:"harness,omitempty"`
	Models    []string  `json:"models,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	FirstTake time.Time `json:"first_take"`
	HeldS     int64     `json:"held_s"`
	Handoffs  []string  `json:"handoffs"`
}

// Times are the issue's times (§3); one not reached is left out.
type Times struct {
	Created      time.Time  `json:"created"`
	FirstStart   *time.Time `json:"first_start,omitempty"`
	ChangeOpened *time.Time `json:"change_opened,omitempty"`
	FirstDone    *time.Time `json:"first_done,omitempty"`
	Merged       *time.Time `json:"merged,omitempty"`
	OutcomeAt    time.Time  `json:"outcome_at"`
}

// DefaultBranch counts the change's merges of the default branch, those
// that conflicted, and the sweep's rework: comments (§6).
type DefaultBranch struct {
	Merges     int `json:"merges"`
	Conflicted int `json:"conflicted"`
	Sweeps     int `json:"sweeps"`
}

// Review counts reviews that requested changes, and review comments.
type Review struct {
	ChangesRequested int `json:"changes_requested"`
	Comments         int `json:"comments"`
}

// Reopen is one reopen and its reason.
type Reopen struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// Waived lists the acceptance items and gates waived, and whether a
// forced close overrode open items or checks.
type Waived struct {
	Items  []int    `json:"items"`
	Gates  []string `json:"gates"`
	Forced bool     `json:"forced"`
}

// Tokens are the issue's tokens to its outcome, by kind and, when the
// record is the issue's latest, by model.
type Tokens struct {
	proto.Tokens
	ByModel []proto.ModelTokens `json:"by_model,omitempty"`
	Split   bool                `json:"split"`
	Capped  bool                `json:"capped"`
}

// Rework is §4's: the boundary and its trigger ("done" or "ci"), the
// tokens from it to the merge, those after the first close, and whether
// every commit after the boundary was made under a hold ("full") or not
// ("partial").
type Rework struct {
	From       *time.Time   `json:"from,omitempty"`
	Trigger    string       `json:"trigger,omitempty"`
	PreMerge   proto.Tokens `json:"pre_merge"`
	PostClose  proto.Tokens `json:"post_close"`
	Attributed string       `json:"attributed"`
}

// Missing is a field the capture could not read, with a one-word reason.
type Missing struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// Comment is the record as an issue comment (§8): a line for people and
// a line of JSON.
func (r Record) Comment() (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode the record: %w", err)
	}
	return r.Summary() + "\n" + string(b), nil
}

// Summary is the comment's first line.
func (r Record) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s v%d %s seq %d", Prefix, r.V, r.Outcome, r.Seq)
	if r.CI != nil {
		flakes := 0
		for _, f := range r.CI.Failures {
			if f.Cause == CauseFlake {
				flakes++
			}
		}
		fmt.Fprintf(&b, ", %d CI %s", r.CI.Attempts, plural(r.CI.Attempts, "attempt"))
		if flakes > 0 {
			fmt.Fprintf(&b, " (%d %s)", flakes, plural(flakes, "flake"))
		}
	}
	if r.DefaultBranch != nil {
		fmt.Fprintf(&b, ", %d %s", r.DefaultBranch.Conflicted, plural(r.DefaultBranch.Conflicted, "conflict"))
	}
	if r.Tokens != nil {
		if total := sum(r.Tokens.Tokens); total > 0 {
			fmt.Fprintf(&b, ", rework %d%% pre-merge", sum(r.Rework.PreMerge)*100/total)
		}
	}
	if len(r.Missing) > 0 {
		fmt.Fprintf(&b, ", %d missing", len(r.Missing))
	}
	return b.String()
}

func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// sum adds a count of every kind but the one-hour cache writes, which
// are part of the cache writes.
func sum(t proto.Tokens) int64 {
	var n int64
	for _, p := range []*int64{t.Input, t.Output, t.CacheWrite, t.CacheRead} {
		if p != nil {
			n += *p
		}
	}
	return n
}

// ErrNotRecord is a comment that begins with Prefix and does not parse:
// a failed capture (§8), which a reader counts rather than skips.
var ErrNotRecord = errors.New("an outcome: comment that is no record")

// Parse reads a record from an outcome comment's body. ok is false for a
// comment that is not an outcome comment at all.
func Parse(body string) (r Record, ok bool, err error) {
	if !strings.HasPrefix(body, Prefix) {
		return Record{}, false, nil
	}
	_, rest, found := strings.Cut(body, "\n")
	if !found {
		return Record{}, true, ErrNotRecord
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(strings.TrimSpace(rest))))
	if err := dec.Decode(&r); err != nil || r.V == 0 || r.Issue == "" || r.Seq == 0 {
		return Record{}, true, ErrNotRecord
	}
	return r, true, nil
}
