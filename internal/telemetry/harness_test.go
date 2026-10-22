package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The files of deploy/observability are examples that are tested
// (design §16.7). These tests hold them to the code: a rule and a query
// of the dashboard name only series, labels and values that ariesd
// has, the scrape jobs are the ones they select by, and every image of
// the harness is pinned.

const (
	alertsFile     = "../../deploy/observability/alerts.yaml"
	prometheusFile = "../../deploy/observability/prometheus.yaml"
	collectorFile  = "../../deploy/observability/collector.yaml"
	dashboardFile  = "../../deploy/observability/dashboard.json"
	composeFile    = "../../deploy/docker/compose.yaml"

	jobAriesd  = "ariesd"
	jobSextant = "sextant"
)

// runtimeSeries are the Go runtime's metrics on ariesd's /metrics, as
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

// series is one series of ariesd's /metrics: the label keys it has,
// and the values each may take, nil for a key whose values are not a
// fixed list in this package (an op, a project, a bucket's bound).
type series map[string][]string

// ariesdSeries is every series of ariesd's own instruments, by its
// name on /metrics.
func ariesdSeries() map[string]series {
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
// harness: a series that ariesd does not serve, a label a series does
// not have, a value a label never takes, or a series of the Go
// runtime's or of Prometheus's own that is not held to its job.
func checkExpr(expr string) []string {
	sels, err := selectors(expr)
	if err != nil {
		return []string{err.Error()}
	}
	if len(sels) == 0 {
		return []string{"it selects no series"}
	}
	ours := ariesdSeries()
	var problems []string
	for _, sel := range sels {
		job := ""
		for _, m := range sel.matchers {
			if m.key == "job" && m.op == "=" {
				job = m.value
			}
		}
		labels, isOurs := ours[sel.name]
		if l, ok := sextantSeries()[sel.name]; ok && !isOurs {
			labels, isOurs = l, true
			if job != jobSextant {
				problems = append(problems, fmt.Sprintf(`%s must be held to job=%q`, sel.name, jobSextant))
			}
		}
		switch {
		case isOurs:
		case sel.name == "up":
			if job != jobAriesd {
				problems = append(problems, fmt.Sprintf(`up must be held to job=%q`, jobAriesd))
			}
			continue
		case slices.Contains(runtimeSeries, sel.name):
			if job != jobAriesd {
				problems = append(problems, fmt.Sprintf(`%s must be held to job=%q`, sel.name, jobAriesd))
			}
			continue
		default:
			problems = append(problems, fmt.Sprintf("%s is not a series of ariesd's /metrics (telemetry.Instruments)", sel.name))
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
		{"bare", `aries_clock_behind_seconds > 0`, []selector{{name: "aries_clock_behind_seconds"}}},
		{"matcher", `up{job="ariesd"} == 0`, []selector{{name: "up", matchers: []matcher{{"job", "=", "ariesd"}}}}},
		{"function and range", `increase(aries_clock_steps_total[15m]) > 0`, []selector{{name: "aries_clock_steps_total"}}},
		{"aggregation with a label list",
			`histogram_quantile(0.99, sum by (le) (rate(aries_store_write_duration_seconds_bucket[5m]))) > 1`,
			[]selector{{name: "aries_store_write_duration_seconds_bucket"}}},
		{"regexp and two matchers", `m{a=~"x|y", b!="z"}`,
			[]selector{{name: "m", matchers: []matcher{{"a", "=~", "x|y"}, {"b", "!=", "z"}}}}},
		{"set operators, offset and a power",
			`a > 0 unless a offset 15m and on() b{job="ariesd"} < 2^62 or time() - c > 1e3`,
			[]selector{{name: "a"}, {name: "a"}, {name: "b", matchers: []matcher{{"job", "=", "ariesd"}}}, {name: "c"}}},
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
	for _, bad := range []string{`{job="ariesd"} > 0`, `m{a="x"`, `m{a=x}`, `rate(m[5m`} {
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
		{"a counter", `increase(aries_telemetry_dropped_total{signal="traces"}[15m]) > 0`, ""},
		{"a histogram's buckets", `sum by (le) (rate(aries_request_duration_seconds_bucket{op="create",le="1"}[5m]))`, ""},
		{"a histogram's count", `rate(aries_store_write_duration_seconds_count[5m])`, ""},
		{"the runtime's, with its job", `go_goroutine_count{job="ariesd"}`, ""},
		{"a regexp of values", `aries_limit_refusals_total{limit=~"conns_per_server|open_stores_per_server"}`, ""},
		{"a renamed metric", `aries_reaper_last_run_seconds > 0`, "is not a series"},
		{"the OpenTelemetry name", `aries.reaper.last_run.time > 0`, "has underscores where its instrument has dots"},
		{"a histogram with no suffix", `rate(aries_request_duration_seconds[5m])`, "is not a series"},
		{"a counter with no _total", `rate(aries_handshakes[5m])`, "is not a series"},
		{"a label of another instrument", `aries_handshakes_total{op="create"}`, `no label "op"`},
		{"le on a count", `aries_request_duration_seconds_count{le="1"}`, `no label "le"`},
		{"a value never taken", `aries_telemetry_dropped_total{signal="events"}`, `is never "events"`},
		{"one bad value of a regexp", `aries_limit_refusals_total{limit=~"conns_per_server|stores"}`, `is never "stores"`},
		{"the runtime's, with no job", `go_goroutine_count > 70`, "must be held to job"},
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

func readAlerts(t *testing.T) []alertRule { return readAlertsFile(t, alertsFile) }

func readAlertsFile(t *testing.T, path string) []alertRule {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a rules file of deploy/observability
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
		t.Fatalf("%s: %v", path, err)
	}
	var rules []alertRule
	for _, g := range file.Groups {
		if g.Name == "" || len(g.Rules) == 0 {
			t.Errorf("%s: a group has no name or no rules: %q", path, g.Name)
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
// and store caps, and the pool of database connections; and the HTTPS
// listener's certificate files and its connection cap.
func TestAlertRulesCover(t *testing.T) {
	t.Parallel()
	rules := readAlerts(t)
	tests := []struct{ name, series, key, value string }{
		{"the listener answering", "up", "job", jobAriesd},
		{"the reaper, which feeds the watchdog", "aries_reaper_last_run_time_seconds", "", ""},
		{"a keys file write failing", "aries_keys_file_writes_total", KeyResult, ResultFailed},
		{"a certificate's files that cannot be read", "aries_https_cert_reloads_total", KeyResult, ResultFailed},
		{"the clock behind history", "aries_clock_behind_seconds", "", ""},
		{"a step of the clock", "aries_clock_steps_total", "", ""},
		{"a project that cannot be opened", "aries_handshakes_total", KeyResult, "unavailable"},
		{"the server's own failures", "aries_project_requests_total", KeyResultClass, ClassError},
		{"write latency", "aries_store_write_duration_seconds_bucket", "", ""},
		{"writes retried", "aries_store_write_retries_total", "", ""},
		{"the connection cap", "aries_limit_refusals_total", KeyLimit, LimitConns},
		{"the cap on one principal's connections", "aries_limit_refusals_total", KeyLimit, LimitConnsPerPrincipal},
		{"the HTTPS listener's connection cap", "aries_limit_refusals_total", KeyLimit, LimitHTTPSConns},
		{"the store cap", "aries_limit_refusals_total", KeyLimit, LimitOpenStores},
		{"requests waiting for a database connection", "aries_db_pool_waits_total", "", ""},
	}
	// Every signal ariesd can drop has its rule: one added to
	// attrSignal's values needs one here.
	for _, signal := range attrSignal.values {
		tests = append(tests, struct{ name, series, key, value string }{
			"dropped " + signal, "aries_telemetry_dropped_total", KeySignal, signal})
	}
	for _, tc := range tests {
		if !selects(rules, tc.series, tc.key, tc.value) {
			t.Errorf("%s has no rule for %s: want one that selects %s{%s=%q}", alertsFile, tc.name, tc.series, tc.key, tc.value)
		}
	}
}

type dashboardPanel struct {
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Datasource struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	} `json:"datasource"`
	Targets []struct {
		Expr  string `json:"expr"`
		Query string `json:"query"`
	} `json:"targets"`
}

// dashboardFiles are the dashboards of deploy/observability: the
// server's, and those the Docker test server's observe profile loads
// beside it.
func dashboardFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(filepath.Dir(dashboardFile), "dashboard*.json"))
	if err != nil || !slices.Contains(files, filepath.Clean(dashboardFile)) {
		t.Fatalf("dashboards: %v, %v; want %s among them", files, err, dashboardFile)
	}
	return files
}

func readDashboard(t *testing.T, file string) []dashboardPanel {
	t.Helper()
	b, err := os.ReadFile(file) //nolint:gosec // a dashboard of deploy/observability, from dashboardFiles
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []dashboardPanel `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if len(d.Panels) == 0 {
		t.Fatalf("%s has no panels", file)
	}
	return d.Panels
}

// The variable each kind of data source is read through, so that no
// panel is tied to one installation's data source.
var dashboardSources = map[string]string{"prometheus": "${datasource}", "tempo": "${traces}", "loki": "${logs}"}

// Every query of every dashboard names series, labels and values that
// exist, as every alert rule does, and no panel is tied to one
// installation's data source. A panel of traces or logs is held to its
// variable; its query names no metric.
func TestDashboardQueriesNameWhatExists(t *testing.T) {
	t.Parallel()
	for _, file := range dashboardFiles(t) {
		ids := map[int]bool{}
		for _, p := range readDashboard(t, file) {
			if ids[p.ID] || p.Title == "" {
				t.Errorf("%s: panel %d (%q) has no title, or an id used twice", file, p.ID, p.Title)
			}
			ids[p.ID] = true
			want, known := dashboardSources[p.Datasource.Type]
			if !known || p.Datasource.UID != want {
				t.Errorf("%s: panel %q reads the data source %q of type %q, want a dashboard variable: %v", file, p.Title, p.Datasource.UID, p.Datasource.Type, dashboardSources)
			}
			if len(p.Targets) == 0 {
				t.Errorf("%s: panel %q has no query", file, p.Title)
			}
			for _, q := range p.Targets {
				if p.Datasource.Type != "prometheus" {
					if q.Expr == "" && q.Query == "" {
						t.Errorf("%s: panel %q has an empty query", file, p.Title)
					}
					continue
				}
				for _, problem := range checkExpr(q.Expr) {
					t.Errorf("%s: panel %q: %s\n%s", file, p.Title, problem, q.Expr)
				}
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

// The scrape job of both examples is the one the rules select by, at
// the loopback address the server guide gives ariesd's listener; and
// the collector receives OTLP on loopback, at
// the port the guide gives ariesd.
func TestHarnessScrapesWhatTheRulesSelect(t *testing.T) {
	t.Parallel()
	// Prometheus also scrapes Sextant, at its default listen address;
	// the collector is ariesd's alone.
	want := map[string]string{jobAriesd: "127.0.0.1:9464"}
	wantProm := map[string]string{jobAriesd: "127.0.0.1:9464", jobSextant: "127.0.0.1:9466"}
	check := func(file string, jobs []scrapeConfig, want map[string]string) {
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
	check(prometheusFile, prom.ScrapeConfigs, wantProm)
	if !slices.Equal(prom.RuleFiles, []string{"alerts.yaml", "alerts-sextant.yaml"}) {
		t.Errorf("%s: rule_files is %v, want the alert rules beside it, alerts.yaml and alerts-sextant.yaml", prometheusFile, prom.RuleFiles)
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
	check(collectorFile, jobs, want)
	if got := col.Receivers["otlp"].Protocols.HTTP.Endpoint; got != "127.0.0.1:4318" {
		t.Errorf("%s: the otlp receiver listens on %q, want 127.0.0.1:4318, loopback and OTLP's port for HTTP", collectorFile, got)
	}
	for _, signal := range attrSignal.values {
		p, ok := col.Service.Pipelines[signal]
		if !ok || !slices.Contains(p.Receivers, "otlp") || len(p.Exporters) == 0 {
			t.Errorf("%s: no pipeline carries the %s ariesd pushes from the otlp receiver to an exporter", collectorFile, signal)
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
// by tag and digest (name:tag@sha256:…): the tag a version of three
// parts, or two for an image tagged so, as Postgres's are, and in the
// reference, where Dependabot reads and updates it.
func TestHarnessImagesArePinned(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(composeFile)
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`^image: [a-z0-9./-]+:v?[0-9]+\.[0-9]+(\.[0-9]+)?@sha256:[0-9a-f]{64}$`)
	lines := strings.Split(string(b), "\n")
	n := 0
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "image:") || strings.Contains(line, "ARIES_IMAGE") {
			continue
		}
		n++
		if !pinned.MatchString(line) {
			t.Errorf("%s:%d: %q is not pinned by version and digest (name:1.2.3@sha256:…)", composeFile, i+1, line)
		}
	}
	if n != 6 {
		t.Errorf("%s runs %d images besides the one it builds, want 6: Postgres, the collector, Prometheus, Keycloak, the observe profile's otel-lgtm and the proxy profile's nginx", composeFile, n)
	}
}
