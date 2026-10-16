package server

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// limitNames are the keys of limits: as v1.0.0 settled them (decision
// 88): a count is THING_per_SCOPE, a size ends in _bytes, a rate names
// its unit, and a duration says what the time is for.
var limitNames = []string{
	// store.Limits
	"labels_per_issue", "acceptance_items_per_issue", "deps_per_issue", "sessions_per_principal",
	"unread_per_principal", "notices_per_minute", "records_per_usage_call", "usage_per_day", "paths_per_issue",
	"memory_body_bytes", "tags_per_memory", "memory_tag_bytes", "memories_per_scope", "memory_key_bytes",
	"prices_per_project", "hours_per_day", "hours_note_bytes", "plans_per_project", "principals_per_plan",
	"conflicts_per_issue", "conflicts_per_principal", "approvers_per_gate", "open_gates_per_project",
	"checks_per_issue", "gate_evidence_bytes", "locks_per_project", "holders_per_lock", "waiters_per_lock",
	"lock_waits_per_principal",
	// store.ControlLimits
	"projects_per_server", "principals_per_server", "principals_per_creator", "keys_per_principal",
	"members_per_project", "identities_per_principal", "invites_per_server", "grants_per_principal",
	// server.Limits
	"conns_per_server", "conns_per_principal", "handshakes_per_principal", "idle_timeout",
	"db_conns_per_server", "db_conns_per_project", "writes_per_second", "write_burst",
	"refusal_logs_per_minute", "agent_keep", "inbox_keep", "idem_window", "away_max",
	"open_stores_per_server", "store_idle", "control_writes_per_second", "control_write_burst",
	// server.Limits, the HTTPS listener's
	"https_conns_per_server", "https_conns_per_addr", "https_header_bytes", "https_header_timeout", "https_body_bytes",
	"upgrade_timeout", "ws_message_bytes", "auth_failures_per_minute", "auth_failure_burst",
	// server.Limits, the sign-in's, in the daemon's memory
	"signins_pending_per_server", "signins_pending_per_addr", "dpop_proofs_per_server",
}

// yamlKeys returns the YAML keys of t's fields, inlined structs' among
// them.
func yamlKeys(t reflect.Type) []string {
	var keys []string
	for f := range t.Fields() {
		name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		switch {
		case opts == "inline":
			keys = append(keys, yamlKeys(f.Type)...)
		case name != "" && name != "-":
			keys = append(keys, name)
		}
	}
	return keys
}

// TestLimitNames pins the keys of limits: and their rule. conflict_age
// and due_soon bound nothing and are settings of their own, and
// telemetry_requests is telemetry: max_requests, beside the listener it
// bounds.
func TestLimitNames(t *testing.T) {
	t.Parallel()
	got, want := yamlKeys(reflect.TypeFor[Limits]()), slices.Clone(limitNames)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the keys of limits: are\n%q\nwant\n%q", got, want)
	}
	durations := map[string]bool{"idle_timeout": true, "agent_keep": true, "inbox_keep": true, "idem_window": true,
		"away_max": true, "store_idle": true, "https_header_timeout": true, "upgrade_timeout": true}
	for _, k := range got {
		switch {
		case durations[k], strings.HasSuffix(k, "_bytes"), strings.HasSuffix(k, "_burst"):
		case strings.Contains(k, "_per_"):
		default:
			t.Errorf("limit %s is neither THING_per_SCOPE, a size in _bytes, a burst nor a duration", k)
		}
	}
}

// TestLimitsTable holds the Limits table of docs/server.md to the keys
// of limits: (rule 18): a row for each, and none for a key there is not.
func TestLimitsTable(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(b), "\n## Limits\n")
	if !ok {
		t.Fatal("docs/server.md has no section Limits")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	var rows []string
	for line := range strings.Lines(section) {
		cell, ok := strings.CutPrefix(line, "| `")
		if !ok {
			continue
		}
		cell, _, _ = strings.Cut(cell, " |")
		for name := range strings.SplitSeq(cell, ", ") {
			rows = append(rows, strings.Trim(name, "`"))
		}
	}
	want := yamlKeys(reflect.TypeFor[Limits]())
	slices.Sort(rows)
	slices.Sort(want)
	if !slices.Equal(rows, want) {
		t.Errorf("docs/server.md's Limits table has rows for\n%q\nwant one for each key of limits:\n%q", rows, want)
	}
}

// TestResolveSettingsThresholds: conflict_age and due_soon are read at
// the top of the config file, and telemetry: max_requests under
// telemetry:, each with its default; under limits: each is an unknown
// key.
func TestResolveSettingsThresholds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	resolve := func(name, body string) (Settings, error) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return ResolveSettings(Settings{}, p, func(string) string { return "" })
	}
	s, err := resolve("set.yaml", "conflict_age: 3d\ndue_soon: 2d\ntelemetry:\n  max_requests: 3\n")
	if err != nil || s.ConflictAge != Duration(3*24*time.Hour) || s.DueSoon != Duration(2*24*time.Hour) || s.Telemetry.MaxRequests != 3 {
		t.Errorf("ResolveSettings(set) = conflict_age %v, due_soon %v, max_requests %d, %v; want 3d, 2d, 3",
			time.Duration(s.ConflictAge), time.Duration(s.DueSoon), s.Telemetry.MaxRequests, err)
	}
	s, err = resolve("defaults.yaml", "")
	if err != nil || s.ConflictAge != Duration(24*time.Hour) || s.DueSoon != Duration(24*time.Hour) || s.Telemetry.MaxRequests != 8 {
		t.Errorf("ResolveSettings(defaults) = conflict_age %v, due_soon %v, max_requests %d, %v; want 1d, 1d, 8",
			time.Duration(s.ConflictAge), time.Duration(s.DueSoon), s.Telemetry.MaxRequests, err)
	}
	for _, tc := range []struct{ name, body, err string }{
		{"conflict_age under limits:", "limits:\n  conflict_age: 3d\n", "conflict_age"},
		{"due_soon under limits:", "limits:\n  due_soon: 3d\n", "due_soon"},
		{"telemetry_requests under limits:", "limits:\n  telemetry_requests: 3\n", "telemetry_requests"},
		{"due_soon past a year", "due_soon: 367d\n", "due_soon"},
		{"negative max_requests", "telemetry:\n  max_requests: -1\n", "max_requests"},
		{"a former name", "limits:\n  memory_body: 2048\n", "memory_body"},
	} {
		if _, err := resolve(strings.ReplaceAll(tc.name, " ", "-")+".yaml", tc.body); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("ResolveSettings(%q) = %v, want an error naming %s", tc.body, err, tc.err)
		}
	}
}
