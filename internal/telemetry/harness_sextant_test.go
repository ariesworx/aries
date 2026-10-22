package telemetry

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The Sextant half of deploy/observability: alerts-sextant.yaml and
// dashboard-sextant.json are held to the series Sextant exports, as the
// ariesd half is held to ariesd's (harness_test.go).

const alertsSextantFile = "../../deploy/observability/alerts-sextant.yaml"
const dashboardSextantFile = "../../deploy/observability/dashboard-sextant.json"

// sextantSeries is every series of Sextant's /metrics by its name there,
// with the label keys it has and the values each takes, nil for a key
// whose values are not a fixed list (a project, a model, an account, a
// code). TestSextantSeriesAreTheExporters holds it to the exporter.
func sextantSeries() map[string]series {
	kinds := []string{TokenInput, TokenOutput, TokenCacheWrite, TokenCacheRead}
	return map[string]series{
		"sextant_cost_usd":             {KeyProject: nil, KeyModel: nil},
		"sextant_account_cost_usd":     {KeyProject: nil, KeyAccount: nil},
		"sextant_hours":                {KeyProject: nil, KeyAccount: nil},
		"sextant_tokens":               {KeyProject: nil, KeyModel: nil, KeyKind: kinds},
		"sextant_cost_unpriced_tokens": {KeyProject: nil, KeyModel: nil},
		"sextant_scrape_errors_total":  {KeyProject: nil, KeyOp: sextantOps, "code": nil},
	}
}

// sample is one line of a /metrics text.
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

var promLabel = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="([^"]*)"`)

// samples reads the series lines of a Prometheus text exposition.
func samples(t *testing.T, text string) []sample {
	t.Helper()
	var out []sample
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		head, val, ok := strings.Cut(line, "} ")
		if !ok {
			continue // a series with no labels is none of Sextant's
		}
		name, inside, _ := strings.Cut(head, "{")
		v, err := strconv.ParseFloat(strings.Fields(val)[0], 64)
		if err != nil {
			t.Fatalf("a sample with no value: %q", line)
		}
		s := sample{name: name, labels: map[string]string{}, value: v}
		for _, m := range promLabel.FindAllStringSubmatch(inside, -1) {
			s.labels[m[1]] = m[2]
		}
		out = append(out, s)
	}
	return out
}

// newSextantWithFigures is a Sextant that has every figure set, over
// pull.
func newSextantWithFigures(t *testing.T) *Sextant {
	t.Helper()
	s, err := NewSextant(t.Context(), SextantConfig{Pull: true})
	if err != nil {
		t.Fatalf("NewSextant: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	s.SetCost(sextantProject, []SextantCost{{Model: "example-large", USD: 8.35}})
	s.SetAccounts(sextantProject, []SextantAccount{{Account: "team-a", USD: 150, Hours: 1.5}, {Account: "team-b", USD: 40}})
	s.SetTokens(sextantProject, []SextantTokens{{Model: "example-large", Input: 1, Output: 2, CacheWrite: 3, CacheRead: 4, Unpriced: 6}})
	s.ScrapeError(sextantProject, SextantOpCost, "unavailable")
	return s
}

// The table the harness checks expressions against is what Sextant
// exports: every series in it is served with just its labels and values,
// and Sextant serves no other.
func TestSextantSeriesAreTheExporters(t *testing.T) {
	t.Parallel()
	s := newSextantWithFigures(t)
	got := map[string]bool{}
	for _, sm := range samples(t, promText(t, s.t)) {
		want, ok := sextantSeries()[sm.name]
		if sm.name == "target_info" { // the resource, which no rule or panel reads
			continue
		}
		if !ok {
			t.Errorf("Sextant exports %s, which the harness's table does not know", sm.name)
			continue
		}
		got[sm.name] = true
		for k, v := range sm.labels {
			values, has := want[k]
			if !has {
				t.Errorf("%s has the label %q, which the table does not list", sm.name, k)
			} else if values != nil && !slices.Contains(values, v) {
				t.Errorf("%s{%s=%q}: the table lists %v", sm.name, k, v, values)
			}
		}
		if len(sm.labels) != len(want) {
			t.Errorf("%s has labels %v, the table lists %d keys", sm.name, sm.labels, len(want))
		}
	}
	for name := range sextantSeries() {
		if !got[name] {
			t.Errorf("the table lists %s, which Sextant does not export", name)
		}
	}
}

// Account figures are bounded and never put text on a series: an account
// past the cap, or of the wrong shape, is (other), summed; and a poll
// replaces a project's accounts.
func TestSextantAccountsAreBounded(t *testing.T) {
	t.Parallel()
	s, err := NewSextant(t.Context(), SextantConfig{Pull: true, Accounts: 2})
	if err != nil {
		t.Fatalf("NewSextant: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	s.SetAccounts(sextantProject, []SextantAccount{{Account: "a", USD: 1, Hours: 1}, {Account: "b", USD: 2}, {Account: "c", USD: 3}, {Account: "has space", USD: 4}})
	text := promText(t, s.t)
	for _, want := range []string{
		`sextant_account_cost_usd{account="a",project="` + sextantProject + `"} 1`,
		`sextant_account_cost_usd{account="b",project="` + sextantProject + `"} 2`,
		`sextant_account_cost_usd{account="(other)",project="` + sextantProject + `"} 7`,
		`sextant_hours{account="a",project="` + sextantProject + `"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics lacks %s:\n%s", want, text)
		}
	}
	if strings.Contains(text, "has space") || strings.Contains(text, `account="c"`) {
		t.Errorf("/metrics holds an account past the cap or of the wrong shape:\n%s", text)
	}
	s.SetAccounts(sextantProject, nil)
	if text = promText(t, s.t); strings.Contains(text, "sextant_account_cost_usd{") || strings.Contains(text, "sextant_hours{") {
		t.Errorf("after the project's accounts were cleared, /metrics =\n%s", text)
	}
}

// Tokens are exported by model and kind, with the part no price covers.
func TestSextantTokens(t *testing.T) {
	t.Parallel()
	s := newSextantWithFigures(t)
	text := promText(t, s.t)
	for _, want := range []string{
		`sextant_tokens{kind="input",model="example-large",project="` + sextantProject + `"} 1`,
		`sextant_tokens{kind="cache_read",model="example-large",project="` + sextantProject + `"} 4`,
		`sextant_cost_unpriced_tokens{model="example-large",project="` + sextantProject + `"} 6`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics lacks %s:\n%s", want, text)
		}
	}
	s.SetTokens(sextantProject, nil)
	if text = promText(t, s.t); strings.Contains(text, "sextant_tokens{") || strings.Contains(text, "sextant_cost_unpriced_tokens{") {
		t.Errorf("after the project's tokens were cleared, /metrics =\n%s", text)
	}
}

// Every rule of alerts-sextant.yaml names series, labels and values that
// Sextant exports, and says what to do.
func TestSextantAlertRulesNameWhatExists(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, r := range readAlertsFile(t, alertsSextantFile) {
		if r.Alert == "" || seen[r.Alert] {
			t.Errorf("%s: the alert name %q is empty or used twice", alertsSextantFile, r.Alert)
		}
		seen[r.Alert] = true
		for _, p := range checkExpr(r.Expr) {
			t.Errorf("%s: %s: %s", alertsSextantFile, r.Alert, p)
		}
		if sev := r.Labels["severity"]; sev != "critical" && sev != "warning" {
			t.Errorf("%s: %s: severity is %q, want critical or warning", alertsSextantFile, r.Alert, sev)
		}
		for _, key := range []string{"summary", "description"} {
			if strings.TrimSpace(r.Annotations[key]) == "" {
				t.Errorf("%s: %s has no %s", alertsSextantFile, r.Alert, key)
			}
		}
		if strings.Contains(r.Expr+r.Annotations["description"], "person") {
			t.Errorf("%s: %s names a person: Sextant has no per-person series (decision 98)", alertsSextantFile, r.Alert)
		}
	}
	if len(seen) == 0 {
		t.Fatalf("%s has no rules", alertsSextantFile)
	}
}

// The rules cover what the design asks for (sextant.md §5, X3): spend
// over a budget per account, unpriced tokens and the exporter failing.
func TestSextantAlertRulesCover(t *testing.T) {
	t.Parallel()
	rules := readAlertsFile(t, alertsSextantFile)
	for _, tc := range []struct{ name, series, key, value string }{
		{"spend over a budget per account", "sextant_account_cost_usd", "", ""},
		{"unpriced tokens", "sextant_cost_unpriced_tokens", "", ""},
		{"the exporter failing", "sextant_scrape_errors_total", "", ""},
	} {
		if !selects(rules, tc.series, tc.key, tc.value) {
			t.Errorf("%s has no rule for %s: want one that selects %s", alertsSextantFile, tc.name, tc.series)
		}
	}
}

// Every series of X1 has a panel on the dashboard, and the panels are
// held to the series by TestDashboardQueriesNameWhatExists.
func TestSextantDashboardCovers(t *testing.T) {
	t.Parallel()
	var exprs []string
	for _, p := range readDashboard(t, dashboardSextantFile) {
		for _, q := range p.Targets {
			exprs = append(exprs, q.Expr)
		}
	}
	all := strings.Join(exprs, "\n")
	for name := range sextantSeries() {
		if !strings.Contains(all, name) {
			t.Errorf("%s has no panel for %s", dashboardSextantFile, name)
		}
	}
}

var budgetRule = regexp.MustCompile(`^sum by \(project, account\) \(sextant_account_cost_usd\{job="sextant"\}\) > ([0-9]+(?:\.[0-9]+)?)$`)

// firing evaluates the budget rule's expression over a /metrics text:
// the accounts whose spend, summed by project and account, is over the
// rule's threshold, by "project/account". It reads the one form the rule
// is written in, and fails if the rule is written another way, so that
// a change to the rule is a change here.
func firing(t *testing.T, expr, text string) map[string]float64 {
	t.Helper()
	m := budgetRule.FindStringSubmatch(expr)
	if m == nil {
		t.Fatalf("the budget rule %q is not in the form this test evaluates: %s", expr, budgetRule)
	}
	limit, _ := strconv.ParseFloat(m[1], 64)
	sums := map[string]float64{}
	for _, sm := range samples(t, text) {
		if sm.name == "sextant_account_cost_usd" {
			sums[sm.labels[KeyProject]+"/"+sm.labels[KeyAccount]] += sm.value
		}
	}
	out := map[string]float64{}
	for k, v := range sums {
		if v > limit {
			out[k] = v
		}
	}
	return out
}

// A budget alert fires when an account's spend passes its threshold, over
// what a real Sextant exports, and stops when the next poll is under it;
// an account under it never fires.
func TestSextantBudgetAlertFires(t *testing.T) {
	t.Parallel()
	var rule alertRule
	for _, r := range readAlertsFile(t, alertsSextantFile) {
		if r.Alert == "SextantAccountOverBudget" {
			rule = r
		}
	}
	if rule.Expr == "" {
		t.Fatal("no rule SextantAccountOverBudget")
	}
	s := newSextantWithFigures(t) // team-a at 150, team-b at 40: the rule's threshold is 100
	got := firing(t, rule.Expr, promText(t, s.t))
	if len(got) != 1 || got[sextantProject+"/team-a"] != 150 {
		t.Errorf("firing = %v, want team-a alone at 150", got)
	}

	s.SetAccounts(sextantProject, []SextantAccount{{Account: "team-a", USD: 60}, {Account: "team-b", USD: 40}})
	if got := firing(t, rule.Expr, promText(t, s.t)); len(got) != 0 {
		t.Errorf("firing = %v after the spend fell under the threshold, want none", got)
	}

	// Past the cap, accounts are summed as (other), and the sum is held to
	// the same threshold.
	s.SetAccounts(sextantProject, []SextantAccount{{Account: "has space", USD: 70}, {Account: "also bad", USD: 60}})
	if got := firing(t, rule.Expr, promText(t, s.t)); got[sextantProject+"/(other)"] != 130 {
		t.Errorf("firing = %v, want (other) at 130", got)
	}
}
