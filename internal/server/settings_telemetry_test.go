package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The telemetry: settings: a loopback listen address or none, and
// per_project, on unless it is set false. Nothing else is accepted, so a
// setting that does nothing yet cannot be mistaken for one that works.
func TestResolveSettingsTelemetry(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name       string
		body       string
		env        map[string]string
		listen     string
		perProject bool
		err        string
	}{
		{name: "nothing set", perProject: true},
		{name: "listen on loopback", body: "telemetry:\n  listen: 127.0.0.1:9464\n", listen: "127.0.0.1:9464", perProject: true},
		{name: "listen on IPv6 loopback", body: "telemetry:\n  listen: \"[::1]:9464\"\n", listen: "[::1]:9464", perProject: true},
		{name: "a name is resolved when the daemon starts", body: "telemetry:\n  listen: localhost:9464\n", listen: "localhost:9464", perProject: true},
		{name: "per_project off", body: "telemetry:\n  per_project: false\n", perProject: false},
		{name: "per_project on", body: "telemetry:\n  per_project: true\n", perProject: true},
		{name: "the environment sets the address", env: map[string]string{EnvTelemetryListen: "127.0.0.1:9000"}, listen: "127.0.0.1:9000", perProject: true},
		{name: "the environment wins over the file", body: "telemetry:\n  listen: 127.0.0.1:9464\n  per_project: false\n",
			env: map[string]string{EnvTelemetryListen: "127.0.0.1:9000"}, listen: "127.0.0.1:9000"},
		{name: "every interface", body: "telemetry:\n  listen: 0.0.0.0:9464\n", err: "not a loopback address"},
		{name: "every IPv6 interface", body: "telemetry:\n  listen: \"[::]:9464\"\n", err: "not a loopback address"},
		{name: "a routable address", body: "telemetry:\n  listen: 192.0.2.10:9464\n", err: "not a loopback address"},
		{name: "a routable address from the environment", env: map[string]string{EnvTelemetryListen: "192.0.2.10:9464"}, err: "not a loopback address"},
		{name: "no host", body: "telemetry:\n  listen: \":9464\"\n", err: "not HOST:PORT"},
		{name: "no port", body: "telemetry:\n  listen: 127.0.0.1\n", err: "not HOST:PORT"},
		{name: "port zero", body: "telemetry:\n  listen: 127.0.0.1:0\n", err: "no port from 1 to 65535"},
		// Settings of later slices, and a secret that belongs in the
		// environment, are refused as unknown keys.
		{name: "principals is not built", body: "telemetry:\n  principals: name\n", err: "principals"},
		{name: "audit_log is not built", body: "telemetry:\n  audit_log: true\n", err: "audit_log"},
		{name: "dolt_metrics_url is not built", body: "telemetry:\n  dolt_metrics_url: http://127.0.0.1:11228/metrics\n", err: "dolt_metrics_url"},
		{name: "no headers in the file", body: "telemetry:\n  headers: Authorization=token\n", err: "headers"},
		{name: "no endpoint in the file", body: "telemetry:\n  endpoint: http://127.0.0.1:4318\n", err: "endpoint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := ResolveSettings(Settings{}, path, func(k string) string { return tc.env[k] })
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("ResolveSettings(%q, %v) = %v, want an error containing %q", tc.body, tc.env, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSettings(%q, %v) = %v", tc.body, tc.env, err)
			}
			if s.Telemetry.Listen != tc.listen || s.Telemetry.ByProject() != tc.perProject {
				t.Errorf("ResolveSettings(%q, %v).Telemetry = listen %q, by project %t; want %q, %t",
					tc.body, tc.env, s.Telemetry.Listen, s.Telemetry.ByProject(), tc.listen, tc.perProject)
			}
		})
	}
}
