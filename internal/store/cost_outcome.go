package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Cost by outcome (design productivity-metrics.md §6, F3): the usage
// attributed to each issue, grouped by how the issue stood at the end of
// the window. Everything here is read from the event log and the usage
// records when asked, priced then; none of it is stored or measured.

// The groups of a report by outcome, besides proto's CostUnattributed
// for usage nobody held. An issue's outcome is how it stood at the end
// of the window, from its close and reopen events before then: landed
// when its last close gave the reason ReasonLanded, closed when it gave
// another, open when it was never closed or was reopened since.
const (
	OutcomeLanded = "landed"
	OutcomeClosed = "closed"
	OutcomeOpen   = "open"
)

// outcomeOrder lists the groups of a report by outcome.
var outcomeOrder = []string{OutcomeLanded, OutcomeClosed, OutcomeOpen, proto.CostUnattributed}

// CostRework is the part of a group's usage that came after the first
// close of the issue it was attributed to.
type CostRework struct {
	Tokens Tokens
	Cost   Cost
}

// CostType is the landed issues of one type, as a group by outcome
// gives them.
type CostType struct {
	Type   IssueType
	Issues int
	Tokens
	Cost     Cost
	PerIssue Cost
}

// LandedIssue is one landed issue of a report by outcome: what was
// attributed to it in the window, the part of that after its first
// close (rework), the time it was held over the time from its creation
// to its last close, and the acceptance items and gates waived on it.
type LandedIssue struct {
	ID           IssueID
	Title        string
	Type         IssueType
	Tokens       Tokens
	Cost         Cost
	ReworkTokens Tokens
	Rework       Cost
	Held, Cycle  time.Duration
	Waivers      int
	Split        bool
}

// outcome is how an issue stood at the end of a window.
type outcome struct {
	kind                  string
	typ                   IssueType
	title                 string
	created               time.Time
	firstClose, lastClose time.Time
}

// loadOutcomes reads how each of ids stood just before until. The close
// and reopen events decide; an issue with none (imported closed) stands
// as its row says.
func loadOutcomes(ctx context.Context, q querier, ids []IssueID, until time.Time) (map[IssueID]*outcome, error) {
	out := map[IssueID]*outcome{}
	for chunk := range slices.Chunk(ids, 1000) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = string(id)
		}
		query := `SELECT id, type, title, status, close_reason, closed_at, created_at FROM issues WHERE id IN (` + placeholders(1, len(args)) + `)` //nolint:gosec // placeholders only; values are arguments
		err := scanAll(ctx, q, "issue outcomes", query, args, func(rs *sql.Rows) error {
			var id IssueID
			var status string
			var reason sql.NullString
			var closedAt sql.NullTime
			o := &outcome{kind: OutcomeOpen}
			if err := rs.Scan(&id, &o.typ, &o.title, &status, &reason, &closedAt, &o.created); err != nil {
				return err
			}
			o.created = o.created.UTC()
			if status == string(StatusClosed) && closedAt.Valid && closedAt.Time.Before(until) {
				o.kind = outcomeKind(reason.String)
				o.firstClose, o.lastClose = closedAt.Time.UTC(), closedAt.Time.UTC()
			}
			out[id] = o
			return nil
		})
		if err != nil {
			return nil, err
		}
		evArgs := append(slices.Clone(args), string(OpIssueClose), string(OpIssueReopen), until)
		n := len(args)
		evs, err := queryEvents(ctx, q, `WHERE target IN (`+placeholders(1, n)+`) AND op IN (`+placeholders(n+1, 2)+
			`) AND at < `+placeholder(n+3)+` ORDER BY seq`, evArgs...)
		if err != nil {
			return nil, err
		}
		fromEvents := map[IssueID]bool{}
		for _, e := range evs {
			id := IssueID(e.Target)
			o := out[id]
			if o == nil {
				continue
			}
			if !fromEvents[id] { // the log decides over the row
				fromEvents[id] = true
				*o = outcome{kind: OutcomeOpen, typ: o.typ, title: o.title, created: o.created}
			}
			if e.Op == OpIssueReopen {
				o.kind = OutcomeOpen
				continue
			}
			var a struct {
				Reason string `json:"close_reason"`
			}
			_ = json.Unmarshal(e.After, &a) // a malformed state has no reason
			o.kind = outcomeKind(a.Reason)
			if o.firstClose.IsZero() {
				o.firstClose = e.At
			}
			o.lastClose = e.At
		}
	}
	return out, nil
}

// outcomeKind is the group of an issue closed for reason.
func outcomeKind(reason string) string {
	if reason == ReasonLanded {
		return OutcomeLanded
	}
	return OutcomeClosed
}

// issueSum is what one issue was attributed in a window, and the part
// of it after the issue's first close.
type issueSum struct {
	tokens, reworkTokens modelSum
	cost, rework         costSum
	split                bool
}

// outcomes accumulates a report by outcome beside its groups.
type outcomes struct {
	states map[IssueID]*outcome
	issues map[IssueID]*issueSum
}

func newOutcomes(states map[IssueID]*outcome) *outcomes {
	return &outcomes{states: states, issues: map[IssueID]*issueSum{}}
}

// note adds part, of the record r, to issue id; part is rework when the
// record is later than the issue's first close.
func (o *outcomes) note(book priceBook, id IssueID, r usageRow, part Tokens, split bool) {
	s := o.issues[id]
	if s == nil {
		s = &issueSum{}
		o.issues[id] = s
	}
	s.tokens.add(part)
	s.cost.add(book, r, part)
	s.split = s.split || split
	if fc := o.states[id].firstClose; !fc.IsZero() && r.to.After(fc) {
		s.reworkTokens.add(part)
		s.rework.add(book, r, part)
	}
}

// finish adds the issues to their groups, as counts, rework and (for
// landed ones) types, and returns the landed issues, dearest first, at
// most limit of them.
func (o *outcomes) finish(ctx context.Context, q querier, groups map[string]*costAcc, now time.Time, limit int) ([]LandedIssue, error) {
	var landed []IssueID
	for id, s := range o.issues {
		a := groups[o.states[id].kind]
		a.issues++
		a.reworkTokens.add(s.reworkTokens.tokens())
		a.rework.merge(s.rework)
		if o.states[id].kind == OutcomeLanded {
			landed = append(landed, id)
			if a.types == nil {
				a.types = map[IssueType]*costAcc{}
			}
			t := a.types[o.states[id].typ]
			if t == nil {
				t = &costAcc{}
				a.types[o.states[id].typ] = t
			}
			t.issues++
			t.tokens.add(s.tokens.tokens())
			t.cost.merge(s.cost)
		}
	}
	slices.Sort(landed)
	holds, err := loadHolds(ctx, q, landed, now)
	if err != nil {
		return nil, err
	}
	held := map[IssueID]time.Duration{}
	for _, h := range holds {
		held[h.issue] += h.end.Sub(h.start)
	}
	waivers, err := countWaivers(ctx, q, landed)
	if err != nil {
		return nil, err
	}
	out := make([]LandedIssue, 0, len(landed))
	for _, id := range landed {
		s, st := o.issues[id], o.states[id]
		out = append(out, LandedIssue{
			ID: id, Title: st.title, Type: st.typ,
			Tokens: s.tokens.tokens(), Cost: s.cost.cost(),
			ReworkTokens: s.reworkTokens.tokens(), Rework: s.rework.cost(),
			Held: held[id], Cycle: st.lastClose.Sub(st.created), Waivers: waivers[id], Split: s.split,
		})
	}
	slices.SortFunc(out, func(a, b LandedIssue) int {
		return cmp.Or(b.Cost.Picodollars.Cmp(a.Cost.Picodollars), strings.Compare(string(a.ID), string(b.ID)))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// countWaivers counts, for each of ids, the acceptance items waived and
// the gates waived that hold it.
func countWaivers(ctx context.Context, q querier, ids []IssueID) (map[IssueID]int, error) {
	out := map[IssueID]int{}
	for chunk := range slices.Chunk(ids, 1000) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = string(id)
		}
		for _, c := range []struct{ what, query string }{
			{"waived items", `SELECT issue_id, COUNT(*) FROM acceptance_state WHERE state = 'waived' AND issue_id IN (` + placeholders(1, len(args)) + `) GROUP BY issue_id`},
			{"waived gates", `SELECT subject, COUNT(*) FROM gates WHERE outcome = 'waived' AND subject IN (` + placeholders(1, len(args)) + `) GROUP BY subject`},
		} {
			err := scanAll(ctx, q, c.what, c.query, args, func(rs *sql.Rows) error {
				var id IssueID
				var n int
				if err := rs.Scan(&id, &n); err != nil {
					return err
				}
				out[id] += n
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("count %s: %w", c.what, err)
			}
		}
	}
	return out, nil
}

// perIssue is c divided among n issues, rounded to the picodollar.
func perIssue(c Cost, n int) Cost {
	out := Cost{Picodollars: new(big.Int), Unpriced: c.Unpriced}
	if n > 0 && c.Picodollars != nil {
		num := new(big.Int).Mul(c.Picodollars, big.NewInt(2))
		num.Add(num, big.NewInt(int64(n)))
		out.Picodollars.Div(num, big.NewInt(int64(2*n)))
	}
	return out
}

// typeList lists the landed issues' types, by type.
func (a *costAcc) typeList() []CostType {
	var out []CostType
	for _, t := range slices.Sorted(maps.Keys(a.types)) {
		x := a.types[t]
		c := x.cost.cost()
		out = append(out, CostType{Type: t, Issues: x.issues, Tokens: x.tokens.tokens(), Cost: c, PerIssue: perIssue(c, x.issues)})
	}
	return out
}
