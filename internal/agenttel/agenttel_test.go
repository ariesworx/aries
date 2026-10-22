package agenttel

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("%q is not NAME=value", kv)
		}
		if _, dup := m[k]; dup {
			t.Fatalf("%s set twice", k)
		}
		m[k] = v
	}
	return m
}

func TestEnvExportsWithoutContent(t *testing.T) {
	t.Parallel()
	for _, path := range []string{PathDesktop, PathHeadless} {
		m := envMap(t, Env("http://127.0.0.1:4318", path, "aries-abc"))
		if m["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" || m["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://127.0.0.1:4318" ||
			m["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" || m["OTEL_METRICS_EXPORTER"] != "otlp" || m["OTEL_LOGS_EXPORTER"] != "otlp" {
			t.Errorf("%s: export not on: %v", path, m)
		}
		for _, k := range ContentVars {
			if v, ok := m[k]; ok {
				t.Errorf("%s: %s=%q is set; it adds content", path, k, v)
			}
		}
		for k := range m {
			if strings.HasPrefix(k, "OTEL_LOG_") {
				t.Errorf("%s: %s is set: every OTEL_LOG_ variable adds content", path, k)
			}
		}
		attrs := m["OTEL_RESOURCE_ATTRIBUTES"]
		if !strings.Contains(attrs, "service.name="+ServiceName) || !strings.Contains(attrs, "aries.path="+path) {
			t.Errorf("%s: resource attributes %q", path, attrs)
		}
		if m["OTEL_SERVICE_NAME"] != ServiceName {
			t.Errorf("%s: service name %q", path, m["OTEL_SERVICE_NAME"])
		}
		if m["OTEL_METRICS_INCLUDE_SESSION_ID"] != "true" || m["OTEL_METRICS_INCLUDE_RESOURCE_ATTRIBUTES"] != "true" {
			t.Errorf("%s: the session id and resource attributes must reach metrics: %v", path, m)
		}
	}
}

func TestEnvIssueAttribute(t *testing.T) {
	t.Parallel()
	attrs := func(issue string) string {
		return envMap(t, Env("http://127.0.0.1:4318", PathHeadless, issue))["OTEL_RESOURCE_ATTRIBUTES"]
	}
	if a := attrs("aries-abc"); !strings.Contains(a, "aries.issue=aries-abc") {
		t.Errorf("attributes %q lack the issue", a)
	}
	if a := attrs(""); strings.Contains(a, "aries.issue") {
		t.Errorf("attributes %q name an issue none was given", a)
	}
	// Nothing that would break the list of attributes goes into it.
	if a := attrs("a,b=c d"); strings.Count(a, ",") != 2 || strings.Contains(a, " ") {
		t.Errorf("attributes %q: an issue id broke the list", a)
	}
}

func TestEnvOffWithoutEndpoint(t *testing.T) {
	t.Parallel()
	if env := Env("", PathHeadless, "aries-abc"); len(env) != 0 {
		t.Errorf("no endpoint, but %v", env)
	}
}

// What the docs tell a person to put in settings.json is what a
// dispatched run gets, less the issue.
func TestSettingsJSONIsEnv(t *testing.T) {
	t.Parallel()
	var got struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(SettingsJSON("http://127.0.0.1:4318")), &got); err != nil {
		t.Fatal(err)
	}
	want := envMap(t, Env("http://127.0.0.1:4318", PathDesktop, ""))
	if len(got.Env) != len(want) {
		t.Errorf("settings env %v, want %v", got.Env, want)
	}
	for k, v := range want {
		if got.Env[k] != v {
			t.Errorf("%s: %q, want %q", k, got.Env[k], v)
		}
	}
}

// The block docs/agents.md gives a person is what SettingsJSON writes.
func TestDocsHoldSettingsJSON(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../docs/agents.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "```json\n"+SettingsJSON("http://127.0.0.1:4318")+"\n```") {
		t.Errorf("docs/agents.md, Agent metrics, lacks the block SettingsJSON writes:\n%s", SettingsJSON("http://127.0.0.1:4318"))
	}
}

// The agents dashboard reads only series Claude Code exports, only
// labels that exist, and nothing of content.
func TestDashboardNamesWhatExists(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../deploy/observability/agents-dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Title      string `json:"title"`
			Datasource struct {
				Type string `json:"type"`
				UID  string `json:"uid"`
			} `json:"datasource"`
			Targets []struct {
				Expr  string `json:"expr"`
				Query string `json:"query"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(b, &d); err != nil || len(d.Panels) == 0 {
		t.Fatalf("dashboard: %v", err)
	}
	ident := regexp.MustCompile(`claude_code_[a-zA-Z_]+`)
	group := regexp.MustCompile(`by \(([^)]*)\)`)
	matcher := regexp.MustCompile(`([a-z_]+)\s*(?:=~|!=|=)\s*"`)
	for _, p := range d.Panels {
		if len(p.Targets) == 0 {
			t.Errorf("%q has no query", p.Title)
		}
		want := map[string]string{"prometheus": "${datasource}", "loki": "${logs}"}[p.Datasource.Type]
		if want == "" || p.Datasource.UID != want {
			t.Errorf("%q reads data source %q of type %q", p.Title, p.Datasource.UID, p.Datasource.Type)
		}
		for _, q := range p.Targets {
			text := q.Expr + q.Query
			if text == "" {
				t.Errorf("%q has an empty query", p.Title)
			}
			for _, name := range ident.FindAllString(text, -1) {
				if !Series[name] {
					t.Errorf("%q reads %s, which is not in Series", p.Title, name)
				}
			}
			for _, bad := range []string{"prompt", "tool_input", "tool_parameters", "body", "tool_output", "response"} {
				if strings.Contains(text, bad) {
					t.Errorf("%q names %q, which is content", p.Title, bad)
				}
			}
			var labels []string
			for _, m := range group.FindAllStringSubmatch(text, -1) {
				labels = append(labels, strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' })...)
			}
			for _, m := range matcher.FindAllStringSubmatch(text, -1) {
				labels = append(labels, m[1])
			}
			for _, l := range labels {
				if !Labels[l] {
					t.Errorf("%q uses label %q, which is not in Labels", p.Title, l)
				}
			}
		}
	}
}
