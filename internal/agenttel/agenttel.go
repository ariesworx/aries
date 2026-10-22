// Package agenttel is the environment that makes Claude Code export its
// own metrics and events over OpenTelemetry, without content, to the
// Collector of the server's observability profiles, under a service name
// of its own, apart from ariesd's (docs/agents.md, Agent metrics).
//
// The variable names are those of Claude Code's monitoring guide,
// https://code.claude.com/docs/en/monitoring-usage, as read on 21 Oct
// 2026. The guide lists what adds content: OTEL_LOG_USER_PROMPTS,
// OTEL_LOG_ASSISTANT_RESPONSES, OTEL_LOG_TOOL_DETAILS,
// OTEL_LOG_TOOL_CONTENT, OTEL_LOG_RAW_API_BODIES, the beta traces and
// detailed tracing. None is ever set here, and a test fails if one is.
package agenttel

import (
	"encoding/json"
	"strings"
)

const (
	// ServiceName is the service name of what Claude Code exports.
	ServiceName = "aries-agents"
	// PathDesktop is the value of the resource attribute aries.path for a
	// person's session.
	PathDesktop = "desktop"
	// PathHeadless is its value for an `aries dispatch` run.
	PathHeadless = "headless"
)

// ContentVars are the variables that add content to what Claude Code
// exports, or turn on traces, which carry more. Env never sets one.
var ContentVars = []string{
	"OTEL_LOG_USER_PROMPTS", "OTEL_LOG_ASSISTANT_RESPONSES", "OTEL_LOG_TOOL_DETAILS",
	"OTEL_LOG_TOOL_CONTENT", "OTEL_LOG_RAW_API_BODIES", "OTEL_LOG_MANAGED_SETTINGS",
	"ENABLE_BETA_TRACING_DETAILED", "BETA_TRACING_ENDPOINT",
	"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", "OTEL_TRACES_EXPORTER",
}

// Series are the Prometheus names of the metrics the agents dashboard
// reads: Claude Code's claude_code.token.usage, cost.usage and the like,
// as the Collector writes them (dots to underscores, the unit, and
// _total for a counter).
var Series = map[string]bool{
	"claude_code_token_usage_tokens_total":        true,
	"claude_code_cost_usage_USD_total":            true,
	"claude_code_session_count_total":             true,
	"claude_code_active_time_total_seconds_total": true,
	"claude_code_lines_of_code_count_total":       true,
	"claude_code_commit_count_total":              true,
	"claude_code_pull_request_count_total":        true,
}

// Labels are the labels the dashboard may use: Claude Code's attributes,
// the resource attributes Env sets, and the labels a backend gives an
// event's log stream.
var Labels = map[string]bool{
	"model": true, "type": true, "session_id": true, "aries_path": true, "aries_issue": true,
	"service_name": true, "query_source": true, "start_type": true,
	"event_name": true, "tool_name": true, "success": true, "error_type": true,
	"status_code": true, "job": true,
}

// Env is the environment that turns the export on at endpoint (OTLP over
// HTTP, e.g. http://127.0.0.1:4318), for a session on path, and for the
// issue it works on when issue is not empty. Without an endpoint it is
// empty. The session id is on every metric and event already
// (session.id), which is how a run is joined to Aries.
func Env(endpoint, path, issue string) []string {
	if endpoint == "" {
		return nil
	}
	attrs := []string{"service.name=" + ServiceName, "aries.path=" + path}
	if v := attrValue(issue); v != "" {
		attrs = append(attrs, "aries.issue="+v)
	}
	return []string{
		"CLAUDE_CODE_ENABLE_TELEMETRY=1",
		"OTEL_METRICS_EXPORTER=otlp",
		"OTEL_LOGS_EXPORTER=otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + endpoint,
		"OTEL_SERVICE_NAME=" + ServiceName,
		"OTEL_RESOURCE_ATTRIBUTES=" + strings.Join(attrs, ","),
		"OTEL_METRICS_INCLUDE_SESSION_ID=true",
		"OTEL_METRICS_INCLUDE_RESOURCE_ATTRIBUTES=true",
		"OTEL_METRICS_INCLUDE_ACCOUNT_UUID=false",
	}
}

// attrValue keeps a value from breaking the list of attributes: only
// letters, digits, '-', '_' and '.' stay.
func attrValue(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return -1
	}, s)
}

// SettingsJSON is the `env` block to merge into the user's
// ~/.claude/settings.json, so that Desktop sessions export.
func SettingsJSON(endpoint string) string {
	m := map[string]string{}
	for _, kv := range Env(endpoint, PathDesktop, "") {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	b, _ := json.MarshalIndent(map[string]any{"env": m}, "", "  ")
	return string(b)
}
