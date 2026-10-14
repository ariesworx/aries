package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The files of deploy/observability are examples that are tested
// (design §16.7). These tests hold them to the code: a rule and a query
// of the dashboard name only series, labels and values that starfixd
// has, the scrape jobs are the ones they select by, and every image of
// the harness is pinned.

const (
	alertsFile     = "../../deploy/observability/alerts.yaml"
	prometheusFile = "../../deploy/observability/prometheus.yaml"
	collectorFile  = "../../deploy/observability/collector.yaml"
	dashboardFile  = "../../deploy/observability/dashboard.json"
	composeFile    = "../../deploy/docker/compose.yaml"
	smokeFile      = "../../deploy/docker/smoke.sh"

	jobStarfixd = "starfixd"
	jobDolt     = "dolt"
)

// doltSeries are the series of Dolt's own metrics endpoint that the
// harness may name, as Dolt 2.4.2 serves them (design §16.5). Dolt is
// not starfix's to rename, so nothing but the Docker smoke test, which
// asks a real Dolt for each, holds this list to it.
var doltSeries = []string{
	"dss_concurrent_connections", "dss_concurrent_queries", "dss_connects", "dss_disconnects",
	"dss_query_duration_bucket", "dss_query_duration_sum", "dss_query_duration_count",
	"process_resident_memory_bytes", "process_cpu_seconds_total", "process_open_fds", "process_max_fds",
	"process_start_time_seconds",
	"go_gc_duration_seconds_count", "go_gc_gomemlimit_bytes", "go_goroutines",
	"sys_cpu_usage", "sys_mem_usage", "sys_disk_usage",
}

// runtimeSeries are the Go runtime's metrics on starfixd's /metrics, as
// the contrib module names them (docs/server.md, What is measured).
var runtimeSeries = []string{
	"go_memory_used_bytes", "go_memory_allocated_bytes_total", "go_memory_allocations_total",
	"go_memory_gc_goal_bytes", "go_memory_limit_bytes", "go_goroutine_count", "go_processor_limit",
	"go_config_gogc_percent",
}

// matcher is one label matcher of a selector: key, =, !=, =~ or !~, and
// the value.
type matcher struct{ key, op, value string }

// selector is one series selector of a PromQL expression.
type selector struct {
	name     string
	matchers []matcher
}

var (
	promIdent = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*`)
	promNum   = regexp.MustCompile(`^[0-9][0-9a-zA-Z_.]*`)
	// promWords are PromQL's operators and modifiers that look like a
	// name; promLists are the ones followed by a list of labels.
	promWords = []string{"and", "or", "unless", "bool", "offset", "inf", "nan"}
	promLists = []string{"by", "without", "on", "ignoring", "group_left", "group_right"}
	// promGrouped is what follows an aggregation written `sum by (…) (…)`.
	promGrouped = regexp.MustCompile(`^(by|without)\s*\(`)
)

// selectors returns the series selectors of the PromQL expression expr.
// It reads the PromQL the harness uses, not all of it: a name followed
// by ( is a function or an aggregation, a name that is none of
// PromQL's words is a series, and a selector with no name is refused.
func selectors(expr string) ([]selector, error) {
	var out []selector
	for i := 0; i < len(expr); {
		rest := expr[i:]
		switch c := rest[0]; {
		case c == '"' || c == '\'' || c == '`':
			end := strings.IndexByte(rest[1:], c)
			if end < 0 {
				return nil, fmt.Errorf("unterminated string at %q", rest)
			}
			i += end + 2
		case c == '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("unterminated range at %q", rest)
			}
			i += end + 1
		case c == '{':
			return nil, fmt.Errorf("a selector with no metric name at %q", rest)
		case c == '.':
			return nil, fmt.Errorf("a dot at %q: a series on /metrics has underscores where its instrument has dots", rest)
		case promNum.MatchString(rest):
			i += len(promNum.FindString(rest))
		case promIdent.MatchString(rest):
			name := promIdent.FindString(rest)
			i += len(name)
			after := strings.TrimLeft(expr[i:], " \t\n")
			switch {
			case slices.Contains(promLists, name) && strings.HasPrefix(after, "("):
				end := strings.IndexByte(expr[i:], ')')
				if end < 0 {
					return nil, fmt.Errorf("unterminated label list after %s", name)
				}
				i += end + 1
			case strings.HasPrefix(after, "("), slices.Contains(promWords, name), promGrouped.MatchString(after):
			default:
				sel := selector{name: name}
				if strings.HasPrefix(after, "{") {
					start := strings.IndexByte(expr[i:], '{') + i
					end := strings.IndexByte(expr[start:], '}')
					if end < 0 {
						return nil, fmt.Errorf("unterminated matchers after %s", name)
					}
					ms, err := matchers(expr[start+1 : start+end])
					if err != nil {
						return nil, fmt.Errorf("%s: %w", name, err)
					}
					sel.matchers, i = ms, start+end+1
				}
				out = append(out, sel)
			}
		default:
			i++
		}
	}
	return out, nil
}

var promMatcher = regexp.MustCompile(`^\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*"([^"]*)"\s*(,|$)`)

// matchers reads the inside of a selector's braces.
func matchers(s string) ([]matcher, error) {
	var out []matcher
	for strings.TrimSpace(s) != "" {
		m := promMatcher.FindStringSubmatch(s)
		if m == nil {
			return nil, fmt.Errorf("cannot read the label matcher at %q", s)
		}
		out = append(out, matcher{key: m[1], op: m[2], value: m[3]})
		s = s[len(m[0]):]
	}
	return out, nil
}

// series is one series of starfixd's /metrics: the label keys it has,
// and the values each may take, nil for a key whose values are not a
// fixed list in this package (an op, a project, a bucket's bound).
type series map[string][]string

// starfixdSeries is every series of starfixd's own instruments, by its
// name on /metrics.
func starfixdSeries() map[string]series {
	out := map[string]series{}
	for _, sp := range specs {
		labels := series{}
		for _, a := range sp.attrs {
			labels[a.key] = a.values
		}
		name := prometheusName(sp)
		if sp.kind != Histogram {
			out[name] = labels
			continue
		}
		out[name+"_sum"], out[name+"_count"] = labels, labels
		bucket := series{"le": nil}
		for k, v := range labels {
			bucket[k] = v
		}
		out[name+"_bucket"] = bucket
	}
	return out
}

// checkExpr returns what is wrong with a PromQL expression of the
// harness: a series that neither starfixd nor Dolt serves, a label a
// series does not have, a value a label never takes, or a series of
// Dolt's or of Prometheus's own that is not held to its job.
func checkExpr(expr string) []string {
	sels, err := selectors(expr)
	if err != nil {
		return []string{err.Error()}
	}
	if len(sels) == 0 {
		return []string{"it selects no series"}
	}
	ours := starfixdSeries()
	var problems []string
	for _, sel := range sels {
		job := ""
		for _, m := range sel.matchers {
			if m.key == "job" && m.op == "=" {
				job = m.value
			}
		}
		labels, isOurs := ours[sel.name]
		switch {
		case isOurs:
		case sel.name == "up":
			if job != jobStarfixd && job != jobDolt {
				problems = append(problems, fmt.Sprintf(`up must be held to job=%q or job=%q`, jobStarfixd, jobDolt))
			}
			continue
		case slices.Contains(doltSeries, sel.name):
			if job != jobDolt {
				problems = append(problems, fmt.Sprintf(`%s is Dolt's, and must be held to job=%q: starfixd's /metrics has Go runtime series too`, sel.name, jobDolt))
			}
			continue
		case slices.Contains(runtimeSeries, sel.name):
			if job != jobStarfixd {
				problems = append(problems, fmt.Sprintf(`%s must be held to job=%q`, sel.name, jobStarfixd))
			}
			continue
		default:
			problems = append(problems, fmt.Sprintf("%s is not a series of starfixd's /metrics (telemetry.Instruments) nor one of Dolt's this test knows", sel.name))
			continue
		}
		for _, m := range sel.matchers {
			if m.key == "job" {
				continue
			}
			values, has := labels[m.key]
			if !has {
				problems = append(problems, fmt.Sprintf("%s has no label %q", sel.name, m.key))
				continue
			}
			if values == nil {
				continue
			}
			given := []string{m.value}
			if m.op == "=~" || m.op == "!~" {
				given = strings.Split(m.value, "|")
			}
			for _, v := range given {
				if !slices.Contains(values, v) {
					problems = append(problems, fmt.Sprintf("%s{%s} is never %q; it is one of %v", sel.name, m.key, v, values))
				}
			}
		}
	}
	return problems
}

// The reader of expressions finds each series and its matchers, and
// passes over functions, aggregations, label lists, ranges and numbers.
func TestSelectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, expr string
		want       []selector
	}{
		{"bare", `starfix_clock_behind_seconds > 0`, []selector{{name: "starfix_clock_behind_seconds"}}},
		{"matcher", `up{job="dolt"} == 0`, []selector{{name: "up", matchers: []matcher{{"job", "=", "dolt"}}}}},
		{"function and range", `increase(starfix_clock_steps_total[15m]) > 0`, []selector{{name: "starfix_clock_steps_total"}}},
		{"aggregation with a label list",
			`histogram_quantile(0.99, sum by (le) (rate(starfix_store_write_duration_seconds_bucket[5m]))) > 1`,
			[]selector{{name: "starfix_store_write_duration_seconds_bucket"}}},
		{"regexp and two matchers", `m{a=~"x|y", b!="z"}`,
			[]selector{{name: "m", matchers: []matcher{{"a", "=~", "x|y"}, {"b", "!=", "z"}}}}},
		{"set operators, offset and a power",
			`a > 0 unless a offset 15m and on() b{job="dolt"} < 2^62 or time() - c > 1e3`,
			[]selector{{name: "a"}, {name: "a"}, {name: "b", matchers: []matcher{{"job", "=", "dolt"}}}, {name: "c"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectors(tc.expr)
			if err != nil {
				t.Fatalf("selectors(%q): %v", tc.expr, err)
			}
			if !slices.EqualFunc(got, tc.want, func(a, b selector) bool {
				return a.name == b.name && slices.Equal(a.matchers, b.matchers)
			}) {
				t.Errorf("selectors(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
	for _, bad := range []string{`{job="dolt"} > 0`, `m{a="x"`, `m{a=x}`, `rate(m[5m`} {
		if got, err := selectors(bad); err == nil {
			t.Errorf("selectors(%q) = %v, want an error", bad, got)
		}
	}
}

// The check of an expression refuses what would make a rule that can
// never fire: a metric that was renamed, a label the series does not
// have, a value the label never takes.
func TestCheckExpr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, expr string
		want       string // part of the one problem, or empty for none
	}{
		{"a counter", `increase(starfix_telemetry_dropped_total{signal="traces"}[15m]) > 0`, ""},
		{"a histogram's buckets", `sum by (le) (rate(starfix_request_duration_seconds_bucket{op="create",le="1"}[5m]))`, ""},
		{"a histogram's count", `rate(starfix_store_write_duration_seconds_count[5m])`, ""},
		{"Dolt's, with its job", `go_gc_duration_seconds_count{job="dolt"}`, ""},
		{"the runtime's, with its job", `go_goroutine_count{job="starfixd"}`, ""},
		{"a regexp of values", `starfix_limit_refusals_total{limit=~"conns|open_stores"}`, ""},
		{"a renamed metric", `starfix_reaper_last_run_seconds > 0`, "is not a series"},
		{"the OpenTelemetry name", `starfix.reaper.last_run.time > 0`, "has underscores where its instrument has dots"},
		{"a histogram with no suffix", `rate(starfix_request_duration_seconds[5m])`, "is not a series"},
		{"a counter with no _total", `rate(starfix_handshakes[5m])`, "is not a series"},
		{"a label of another instrument", `starfix_handshakes_total{op="create"}`, `no label "op"`},
		{"le on a count", `starfix_request_duration_seconds_count{le="1"}`, `no label "le"`},
		{"a value never taken", `starfix_telemetry_dropped_total{signal="events"}`, `is never "events"`},
		{"one bad value of a regexp", `starfix_limit_refusals_total{limit=~"conns|stores"}`, `is never "stores"`},
		{"Dolt's, with no job", `go_gc_duration_seconds_count > 70`, "must be held to job"},
		{"up, with no job", `up == 0`, "up must be held to job"},
		{"no series", `1 > 0`, "selects no series"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := checkExpr(tc.expr)
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("checkExpr(%q) = %q, want no problem", tc.expr, got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("checkExpr(%q) = %q, want one problem containing %q", tc.expr, got, tc.want)
			}
		})
	}
}

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func readAlerts(t *testing.T) []alertRule {
	t.Helper()
	b, err := os.ReadFile(alertsFile)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Groups []struct {
			Name  string      `yaml:"name"`
			Rules []alertRule `yaml:"rules"`
		} `yaml:"groups"`
	}
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		t.Fatalf("%s: %v", alertsFile, err)
	}
	var rules []alertRule
	for _, g := range file.Groups {
		if g.Name == "" || len(g.Rules) == 0 {
			t.Errorf("%s: a group has no name or no rules: %q", alertsFile, g.Name)
		}
		rules = append(rules, g.Rules...)
	}
	return rules
}

// Every alert rule names series, labels and values that exist, so that
// renaming an instrument, or dropping a value from an attribute's
// list, fails here instead of leaving a rule that never fires.
func TestAlertRulesNameWhatExists(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, r := range readAlerts(t) {
		if r.Alert == "" || seen[r.Alert] {
			t.Errorf("%s: the alert name %q is empty or used twice", alertsFile, r.Alert)
		}
		seen[r.Alert] = true
		for _, p := range checkExpr(r.Expr) {
			t.Errorf("%s: %s: %s", alertsFile, r.Alert, p)
		}
		if sev := r.Labels["severity"]; sev != "critical" && sev != "warning" {
			t.Errorf("%s: %s: severity is %q, want critical or warning", alertsFile, r.Alert, sev)
		}
		for _, key := range []string{"summary", "description"} {
			if strings.TrimSpace(r.Annotations[key]) == "" {
				t.Errorf("%s: %s has no %s: an alert says what happened and what to do", alertsFile, r.Alert, key)
			}
		}
	}
}

// selects reports whether some rule's expression has a selector of the
// series name with a matcher that admits value for key; key "" asks
// only for the series.
func selects(rules []alertRule, name, key, value string) bool {
	for _, r := range rules {
		sels, _ := selectors(r.Expr)
		for _, sel := range sels {
			if sel.name != name {
				continue
			}
			if key == "" {
				return true
			}
			for _, m := range sel.matchers {
				if m.key == key && (m.op == "=" && m.value == value || m.op == "=~" && slices.Contains(strings.Split(m.value, "|"), value)) {
					return true
				}
			}
		}
	}
	return false
}

// The rules cover what the design says an operator is told of (§16.7,
// and the notes of slices O2 and O4): each signal's drops, the reaper,
// the keys file, the clock, write latency and retries, the connection
// and store caps, and Dolt's collector against starfixd's writes.
func TestAlertRulesCover(t *testing.T) {
	t.Parallel()
	rules := readAlerts(t)
	tests := []struct{ name, series, key, value string }{
		{"the listener answering", "up", "job", jobStarfixd},
		{"Dolt's endpoint answering", "up", "job", jobDolt},
		{"the reaper, which feeds the watchdog", "starfix_reaper_last_run_time_seconds", "", ""},
		{"a keys file write failing", "starfix_keys_file_writes_total", KeyResult, ResultFailed},
		{"the clock behind history", "starfix_clock_behind_seconds", "", ""},
		{"a step of the clock", "starfix_clock_steps_total", "", ""},
		{"a project that cannot be opened", "starfix_handshakes_total", KeyResult, "unavailable"},
		{"the server's own failures", "starfix_project_requests_total", KeyResultClass, ClassError},
		{"write latency", "starfix_store_write_duration_seconds_bucket", "", ""},
		{"writes retried", "starfix_store_write_retries_total", "", ""},
		{"the connection cap", "starfix_limit_refusals_total", KeyLimit, LimitConns},
		{"the cap on one principal's connections", "starfix_limit_refusals_total", KeyLimit, LimitConnsPerPrincipal},
		{"the store cap", "starfix_limit_refusals_total", KeyLimit, LimitOpenStores},
		{"Dolt's collector", "go_gc_duration_seconds_count", "job", jobDolt},
		{"Dolt's memory", "process_resident_memory_bytes", "job", jobDolt},
		{"Dolt's memory limit", "go_gc_gomemlimit_bytes", "job", jobDolt},
		{"the machine's memory", "sys_mem_usage", "job", jobDolt},
		{"the disk", "sys_disk_usage", "job", jobDolt},
	}
	// Every signal starfixd can drop has its rule: one added to
	// attrSignal's values needs one here.
	for _, signal := range attrSignal.values {
		tests = append(tests, struct{ name, series, key, value string }{
			"dropped " + signal, "starfix_telemetry_dropped_total", KeySignal, signal})
	}
	for _, tc := range tests {
		if !selects(rules, tc.series, tc.key, tc.value) {
			t.Errorf("%s has no rule for %s: want one that selects %s{%s=%q}", alertsFile, tc.name, tc.series, tc.key, tc.value)
		}
	}
	// The thresholds of Dolt's collector are the status report's
	// (internal/statustext: gcRoom and gcTooLow, for each 100 writes of
	// at least 500, and 70 cycles with fewer).
	for _, tc := range []struct{ alert, want string }{
		{"DoltCollectorPastTheCliff", "> 14"}, {"DoltCollectorPastTheCliff", ">= 500"}, {"DoltCollectorPastTheCliff", "> 70"},
		{"DoltCollectorBusy", ">= 3"}, {"DoltCollectorBusy", ">= 500"},
	} {
		i := slices.IndexFunc(rules, func(r alertRule) bool { return r.Alert == tc.alert })
		if i < 0 {
			t.Errorf("%s has no rule named %s", alertsFile, tc.alert)
			continue
		}
		if !strings.Contains(rules[i].Expr, tc.want) {
			t.Errorf("%s: %s does not compare with %q:\n%s", alertsFile, tc.alert, tc.want, rules[i].Expr)
		}
	}
}

type dashboardPanel struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Datasource struct {
		UID string `json:"uid"`
	} `json:"datasource"`
	Targets []struct {
		Expr string `json:"expr"`
	} `json:"targets"`
}

func readDashboard(t *testing.T) []dashboardPanel {
	t.Helper()
	b, err := os.ReadFile(dashboardFile)
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []dashboardPanel `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("%s: %v", dashboardFile, err)
	}
	if len(d.Panels) == 0 {
		t.Fatalf("%s has no panels", dashboardFile)
	}
	return d.Panels
}

// Every query of the dashboard names series, labels and values that
// exist, as every alert rule does, and no panel is tied to one
// installation's data source.
func TestDashboardQueriesNameWhatExists(t *testing.T) {
	t.Parallel()
	ids := map[int]bool{}
	for _, p := range readDashboard(t) {
		if ids[p.ID] || p.Title == "" {
			t.Errorf("%s: panel %d (%q) has no title, or an id used twice", dashboardFile, p.ID, p.Title)
		}
		ids[p.ID] = true
		if p.Datasource.UID != "${datasource}" {
			t.Errorf("%s: panel %q reads the data source %q, want the dashboard's variable, ${datasource}", dashboardFile, p.Title, p.Datasource.UID)
		}
		if len(p.Targets) == 0 {
			t.Errorf("%s: panel %q has no query", dashboardFile, p.Title)
		}
		for _, q := range p.Targets {
			for _, problem := range checkExpr(q.Expr) {
				t.Errorf("%s: panel %q: %s\n%s", dashboardFile, p.Title, problem, q.Expr)
			}
		}
	}
}

// Dolt's series are Dolt's to name, so the one thing that holds the
// harness to them is the Docker smoke test, which waits for each to
// arrive from a real Dolt: every series of Dolt's that a rule or the
// dashboard reads is on its list.
func TestSmokeChecksEveryDoltSeriesUsed(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(smokeFile)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^dolt_series=\(([a-z0-9_ ]+)\)$`).FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s has no line dolt_series=(…)", smokeFile)
	}
	checked := strings.Fields(string(m[1]))
	var exprs []string
	for _, r := range readAlerts(t) {
		exprs = append(exprs, r.Expr)
	}
	for _, p := range readDashboard(t) {
		for _, q := range p.Targets {
			exprs = append(exprs, q.Expr)
		}
	}
	for _, expr := range exprs {
		sels, err := selectors(expr)
		if err != nil {
			t.Fatalf("selectors(%q): %v", expr, err)
		}
		for _, sel := range sels {
			if !slices.Contains(doltSeries, sel.name) {
				continue
			}
			// A histogram's or a summary's series arrive under one name.
			base := sel.name
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				base = strings.TrimSuffix(base, suffix)
			}
			if !slices.Contains(checked, base) {
				t.Errorf("%s does not wait for Dolt's %s, which the harness reads (%s): add it to dolt_series", smokeFile, base, sel.name)
			}
		}
	}
}

type scrapeConfig struct {
	JobName       string `yaml:"job_name"`
	StaticConfigs []struct {
		Targets []string `yaml:"targets"`
	} `yaml:"static_configs"`
}

// The scrape jobs of both examples are the two the rules select by,
// at the loopback addresses the server guide gives starfixd's listener
// and Dolt's metrics; and the collector receives OTLP on loopback, at
// the port the guide gives starfixd.
func TestHarnessScrapesWhatTheRulesSelect(t *testing.T) {
	t.Parallel()
	want := map[string]string{jobStarfixd: "127.0.0.1:9464", jobDolt: "127.0.0.1:11228"}
	check := func(file string, jobs []scrapeConfig) {
		t.Helper()
		got := map[string]string{}
		for _, j := range jobs {
			if len(j.StaticConfigs) != 1 || len(j.StaticConfigs[0].Targets) != 1 {
				t.Errorf("%s: job %s: want one static target", file, j.JobName)
				continue
			}
			got[j.JobName] = j.StaticConfigs[0].Targets[0]
		}
		for job, target := range want {
			if got[job] != target {
				t.Errorf("%s: job %q scrapes %q, want %q", file, job, got[job], target)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: scrape jobs are %v, want only %v", file, got, want)
		}
	}

	var prom struct {
		RuleFiles     []string       `yaml:"rule_files"`
		ScrapeConfigs []scrapeConfig `yaml:"scrape_configs"`
	}
	readYAML(t, prometheusFile, &prom)
	check(prometheusFile, prom.ScrapeConfigs)
	if !slices.Equal(prom.RuleFiles, []string{"alerts.yaml"}) {
		t.Errorf("%s: rule_files is %v, want the alert rules beside it, alerts.yaml", prometheusFile, prom.RuleFiles)
	}

	var col struct {
		Receivers map[string]struct {
			Protocols struct {
				HTTP struct {
					Endpoint string `yaml:"endpoint"`
				} `yaml:"http"`
			} `yaml:"protocols"`
			Config struct {
				ScrapeConfigs []scrapeConfig `yaml:"scrape_configs"`
			} `yaml:"config"`
		} `yaml:"receivers"`
		Service struct {
			Pipelines map[string]struct {
				Receivers []string `yaml:"receivers"`
				Exporters []string `yaml:"exporters"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	readYAML(t, collectorFile, &col)
	var jobs []scrapeConfig
	for _, r := range col.Receivers {
		jobs = append(jobs, r.Config.ScrapeConfigs...)
	}
	check(collectorFile, jobs)
	if got := col.Receivers["otlp"].Protocols.HTTP.Endpoint; got != "127.0.0.1:4318" {
		t.Errorf("%s: the otlp receiver listens on %q, want 127.0.0.1:4318, loopback and OTLP's port for HTTP", collectorFile, got)
	}
	for _, signal := range attrSignal.values {
		p, ok := col.Service.Pipelines[signal]
		if !ok || !slices.Contains(p.Receivers, "otlp") || len(p.Exporters) == 0 {
			t.Errorf("%s: no pipeline carries the %s starfixd pushes from the otlp receiver to an exporter", collectorFile, signal)
		}
	}
}

func readYAML(t *testing.T, file string, into any) {
	t.Helper()
	b, err := os.ReadFile(file) //nolint:gosec // one of the harness files named at the top of this file
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
}

// Every image the compose file runs, but the one it builds, is pinned
// by digest, under a comment giving the version the digest is of.
func TestHarnessImagesArePinned(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`^image: [a-z0-9./-]+@sha256:[0-9a-f]{64}$`)
	versioned := regexp.MustCompile(`^# [a-z0-9./-]+:v?[0-9]+\.[0-9]+\.[0-9]+\b`)
	lines := strings.Split(string(b), "\n")
	n := 0
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "image:") || strings.Contains(line, "STARFIX_IMAGE") {
			continue
		}
		n++
		if !pinned.MatchString(line) {
			t.Errorf("%s:%d: %q is not pinned by digest (name@sha256:…)", composeFile, i+1, line)
		}
		if i == 0 || !versioned.MatchString(strings.TrimSpace(lines[i-1])) {
			t.Errorf("%s:%d: the line above %q does not give the image's version, as in `# name:1.2.3`", composeFile, i+1, line)
		}
	}
	if n != 2 {
		t.Errorf("%s runs %d images besides the one it builds, want 2: the collector and Prometheus", composeFile, n)
	}
}
