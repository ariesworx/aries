package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The telemetry: settings: a loopback listen address or none,
// per_project, on unless it is set false, and the URL of Dolt's metrics
// endpoint, http:// on loopback or none. Nothing else is accepted, so a
// setting that does nothing yet cannot be mistaken for one that works.
func TestResolveSettingsTelemetry(t *testing.T) {
	// urlSecret stands for a credential someone put in a URL.
	const urlSecret = "hunter2"
	dir := t.TempDir()
	for _, tc := range []struct {
		name       string
		body       string
		env        map[string]string
		listen     string
		perProject bool
		dolt       string
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
		{name: "Dolt's metrics on loopback", body: "telemetry:\n  dolt_metrics_url: http://127.0.0.1:11228/metrics\n", perProject: true,
			dolt: "http://127.0.0.1:11228/metrics"},
		{name: "Dolt's metrics on IPv6 loopback", body: "telemetry:\n  dolt_metrics_url: \"http://[::1]:11228/metrics\"\n", perProject: true,
			dolt: "http://[::1]:11228/metrics"},
		{name: "Dolt's metrics by a name, resolved when the daemon starts", body: "telemetry:\n  dolt_metrics_url: http://localhost:11228/metrics\n",
			perProject: true, dolt: "http://localhost:11228/metrics"},
		{name: "the environment sets Dolt's URL", env: map[string]string{EnvDoltMetricsURL: "http://127.0.0.1:9000/metrics"}, perProject: true,
			dolt: "http://127.0.0.1:9000/metrics"},
		{name: "the environment wins over the file for Dolt's URL", body: "telemetry:\n  dolt_metrics_url: http://127.0.0.1:11228/metrics\n",
			env: map[string]string{EnvDoltMetricsURL: "http://127.0.0.1:9000/metrics"}, perProject: true, dolt: "http://127.0.0.1:9000/metrics"},
		{name: "Dolt's metrics on another machine", body: "telemetry:\n  dolt_metrics_url: http://192.0.2.10:11228/metrics\n", err: "not a loopback address"},
		{name: "Dolt's metrics on another machine, from the environment", env: map[string]string{EnvDoltMetricsURL: "http://192.0.2.10:11228/metrics"},
			err: "not a loopback address"},
		{name: "Dolt's metrics on every interface", body: "telemetry:\n  dolt_metrics_url: http://0.0.0.0:11228/metrics\n", err: "not a loopback address"},
		{name: "Dolt's metrics over TLS", body: "telemetry:\n  dolt_metrics_url: https://127.0.0.1:11228/metrics\n", err: "not an http:// URL"},
		{name: "Dolt's metrics with no path", body: "telemetry:\n  dolt_metrics_url: http://127.0.0.1:11228\n", err: "has no path"},
		{name: "Dolt's metrics with no port", body: "telemetry:\n  dolt_metrics_url: http://127.0.0.1/metrics\n", err: "names no host and port"},
		{name: "Dolt's metrics as an address alone", body: "telemetry:\n  dolt_metrics_url: 127.0.0.1:11228\n", err: "not an http:// URL"},
		{name: "Dolt's metrics with a password", body: "telemetry:\n  dolt_metrics_url: http://dolt:" + urlSecret + "@127.0.0.1:11228/metrics\n",
			err: "holds a user, a query or a fragment"},
		// Settings of later slices, and a secret that belongs in the
		// environment, are refused as unknown keys.
		{name: "principals is not built", body: "telemetry:\n  principals: name\n", err: "principals"},
		{name: "audit_log is not built", body: "telemetry:\n  audit_log: true\n", err: "audit_log"},
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
				if strings.Contains(err.Error(), urlSecret) {
					t.Errorf("ResolveSettings(%q, %v) = %v, which repeats the password", tc.body, tc.env, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSettings(%q, %v) = %v", tc.body, tc.env, err)
			}
			if s.Telemetry.Listen != tc.listen || s.Telemetry.ByProject() != tc.perProject || s.Telemetry.DoltMetricsURL != tc.dolt {
				t.Errorf("ResolveSettings(%q, %v).Telemetry = listen %q, by project %t, Dolt's metrics %q; want %q, %t, %q",
					tc.body, tc.env, s.Telemetry.Listen, s.Telemetry.ByProject(), s.Telemetry.DoltMetricsURL, tc.listen, tc.perProject, tc.dolt)
			}
		})
	}
}
