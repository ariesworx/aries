package store

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// outcomeFixture records, from 12:00 on the test clock, four issues and
// what each cost at 1 USD per million tokens in and 2 out:
//
//	L (task)  alice 12:00-12:05, landed, one acceptance item waived; 100 in, 10 out
//	H (task)  bob 12:00-12:05, closed by hand; 50 in
//	O (task)  alice from 12:10, still open; 7 in
//	R (bug)   bob 12:15-12:18, landed, reopened at 12:20, held again
//	          12:21-12:25 and landed again; 20 in before the first close,
//	          30 in after it (rework)
//
// and 1000 in at 12:08, when alice held nothing. It returns the ids by name.
func outcomeFixture(t *testing.T) (*Store, map[string]IssueID) {
	t.Helper()
	s, clk := clockStore(t)
	ctx := t.Context()
	t0 := clk.now()
	mustSetPrice(t, s, "example-large", day("2026-01-01"), rates(1_000_000, 2_000_000, 0, 0, 0))
	l := mustCreate(t, s, NewIssue{Title: "landed", Acceptance: "- a\n- b"})
	h := mustCreate(t, s, NewIssue{Title: "by hand"})
	o := mustCreate(t, s, NewIssue{Title: "open"})
	r := mustCreate(t, s, NewIssue{Title: "reopened", Type: TypeBug})
	start := func(a Actor, id IssueID) {
		t.Helper()
		if _, _, err := s.StartIssue(ctx, a, id, time.Hour, false); err != nil {
			t.Fatal(err)
		}
	}
	finish := func(a Actor, id IssueID, f Finish) {
		t.Helper()
		if _, _, err := s.FinishIssue(ctx, a, id, 0, f); err != nil {
			t.Fatal(err)
		}
	}
	at := func(m int) { clk.add(t0.Add(time.Duration(m) * time.Minute).Sub(clk.now())) }
	start(alice, l.ID)
	start(bob, h.ID)
	at(5)
	finish(alice, l.ID, Finish{Reason: "landed", Accept: Acceptance{Tick: []int{1}, Waive: map[int]string{2: "not needed"}}})
	finish(bob, h.ID, Finish{Reason: "done by hand"})
	at(10)
	start(alice, o.ID)
	at(15)
	start(bob, r.ID)
	at(18)
	finish(bob, r.ID, Finish{Reason: "landed"})
	at(20)
	if _, err := s.ReopenIssue(ctx, bob, r.ID, 0); err != nil {
		t.Fatal(err)
	}
	at(21)
	start(bob, r.ID)
	at(25)
	finish(bob, r.ID, Finish{Reason: "landed"})
	at(60)
	mustAddUsage(t, s, alice,
		rec("a1", "example-large", t0.Add(2*time.Minute), 100, 10), // L
		rec("a3", "example-large", t0.Add(8*time.Minute), 1000, 0), // nothing held
		rec("a2", "example-large", t0.Add(12*time.Minute), 7, 0),   // O
	)
	mustAddUsage(t, s, bob,
		rec("b1", "example-large", t0.Add(3*time.Minute), 50, 0),  // H
		rec("b2", "example-large", t0.Add(16*time.Minute), 20, 0), // R, before its first close
		rec("b3", "example-large", t0.Add(22*time.Minute), 30, 0), // R, after it
	)
	return s, map[string]IssueID{"L": l.ID, "H": h.ID, "O": o.ID, "R": r.ID}
}

// outcomeText renders a report's groups: "key issues in/out $picos",
// then " per issue $picos" for a group that has one and " rework in/out
// $picos" for one that has rework.
func outcomeText(gs []CostGroup) string {
	var out []string
	f := func(p *int64) string {
		if p == nil {
			return "?"
		}
		return fmt.Sprint(*p)
	}
	for _, g := range gs {
		s := fmt.Sprintf("%s %d %s/%s $%s", g.Key, g.Issues, f(g.Input), f(g.Output), picos(g.Cost))
		if g.PerIssue != nil {
			s += " per issue $" + picos(*g.PerIssue)
		}
		if g.Rework != nil {
			s += fmt.Sprintf(" rework %s/%s $%s", f(g.Rework.Tokens.Input), f(g.Rework.Tokens.Output), picos(g.Rework.Cost))
		}
		out = append(out, s)
	}
	return strings.Join(out, "; ")
}

// An issue's usage is counted under how it ended at the window's end:
// landed, closed otherwise or open, and what nobody held under
// (unattributed); the landed group says what a landed issue cost.
func TestCostReportByOutcome(t *testing.T) {
	t.Parallel()
	s, ids := outcomeFixture(t)
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		until time.Duration
		want  string
	}{
		{"all of it", time.Hour,
			"landed 2 150/10 $170000000 per issue $85000000 rework 30/0 $30000000; " +
				"closed 1 50/0 $50000000; open 1 7/0 $7000000; (unattributed) 0 1000/0 $1000000000"},
		// At 12:06 L and H have ended; R was held but not yet closed, and
		// O not started.
		{"before R's first close", 6 * time.Minute,
			"landed 1 100/10 $120000000 per issue $120000000; closed 1 50/0 $50000000"},
		// R's first close is in the window and its reopen is not: it
		// counts as landed, and what came after is not yet read.
		{"between R's close and reopen", 19 * time.Minute,
			"landed 2 120/10 $140000000 per issue $70000000; closed 1 50/0 $50000000; " +
				"open 1 7/0 $7000000; (unattributed) 0 1000/0 $1000000000"},
		// Reopened: R is open again, and nothing of it is rework until
		// it is closed once more.
		{"after R's reopen", 21 * time.Minute,
			"landed 1 100/10 $120000000 per issue $120000000; closed 1 50/0 $50000000; " +
				"open 2 27/0 $27000000; (unattributed) 0 1000/0 $1000000000"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := s.CostReport(t.Context(), CostFilter{By: CostByOutcome, Since: t0.Add(-time.Hour), Until: t0.Add(tc.until)})
			if err != nil {
				t.Fatal(err)
			}
			if got := outcomeText(r.Groups); got != tc.want {
				t.Errorf("CostReport(by outcome, until +%s).Groups =\n%s\nwant\n%s", tc.until, got, tc.want)
			}
		})
	}

	// However they end, the groups add up to the total.
	r, err := s.CostReport(t.Context(), CostFilter{By: CostByOutcome, Since: t0.Add(-time.Hour), Until: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var in int64
	for _, g := range r.Groups {
		in += *g.Input
	}
	if want := *r.Total.Input; in != want || picos(r.Total.Cost) != "1227000000" {
		t.Errorf("groups hold %d input tokens, total %d $%s; want equal, $1227000000", in, want, picos(r.Total.Cost))
	}
	// The landed group, by type, and each landed issue.
	var landed CostGroup
	for _, g := range r.Groups {
		if g.Key == OutcomeLanded {
			landed = g
		}
	}
	var types []string
	for _, ty := range landed.Types {
		types = append(types, fmt.Sprintf("%s %d $%s per issue $%s", ty.Type, ty.Issues, picos(ty.Cost), picos(ty.PerIssue)))
	}
	if got, want := strings.Join(types, "; "), "bug 1 $50000000 per issue $50000000; task 1 $120000000 per issue $120000000"; got != want {
		t.Errorf("landed types = %q, want %q", got, want)
	}
	var issues []string
	for _, li := range r.Landed {
		issues = append(issues, fmt.Sprintf("%s %s $%s rework $%s held %s cycle %s waivers %d",
			li.ID, li.Type, picos(li.Cost), picos(li.Rework), li.Held, li.Cycle, li.Waivers))
	}
	want := fmt.Sprintf("%s task $120000000 rework $0 held 5m0s cycle 5m0s waivers 1; %s bug $50000000 rework $30000000 held 7m0s cycle 25m0s waivers 0",
		ids["L"], ids["R"])
	if got := strings.Join(issues, "; "); got != want {
		t.Errorf("CostReport(by outcome).Landed =\n%s\nwant\n%s", got, want)
	}
}

// Usage after a reopen is the rework of the issue that was reopened, and
// only what came after its first close.
func TestCostReportReworkAfterReopen(t *testing.T) {
	t.Parallel()
	s, ids := outcomeFixture(t)
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r, err := s.CostReport(t.Context(), CostFilter{By: CostByOutcome, Since: t0.Add(-time.Hour), Until: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, li := range r.Landed {
		if li.ID != ids["R"] {
			if li.Rework.Picodollars != nil && li.Rework.Picodollars.Sign() != 0 {
				t.Errorf("%s was never reopened, rework = $%s", li.ID, picos(li.Rework))
			}
			continue
		}
		got = append(got, fmt.Sprintf("%s total $%s rework %s/%s $%s", li.ID, picos(li.Cost),
			fmt.Sprint(*li.ReworkTokens.Input), fmt.Sprint(*li.ReworkTokens.Output), picos(li.Rework)))
	}
	if want := fmt.Sprintf("%s total $50000000 rework 30/0 $30000000", ids["R"]); strings.Join(got, ";") != want {
		t.Errorf("rework of the reopened issue = %v, want %q", got, want)
	}
}

// Other groupings carry none of it, and an unknown one is refused.
func TestCostReportOutcomeOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	s, _ := outcomeFixture(t)
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	r, err := s.CostReport(t.Context(), CostFilter{By: CostByIssue, Since: t0.Add(-time.Hour), Until: t0.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range r.Groups {
		if g.Issues != 0 || g.Rework != nil || g.PerIssue != nil || len(g.Types) != 0 {
			t.Errorf("group %s of a report by issue has outcome fields: %+v", g.Key, g)
		}
	}
	if len(r.Landed) != 0 {
		t.Errorf("a report by issue lists %d landed issues, want none", len(r.Landed))
	}
}
