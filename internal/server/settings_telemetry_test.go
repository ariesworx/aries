package server

import (
	"cmp"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The telemetry: settings: a loopback listen address or none,
// per_project, on unless it is set false, and how a person appears on a
// span, with the file of the key that makes the stand-ins. Nothing else
// is accepted, so a setting that does nothing yet cannot be mistaken for
// one that works.
func TestResolveSettingsTelemetry(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name       string
		body       string
		env        map[string]string
		listen     string
		perProject bool
		principals string // default pseudonym
		key        string // default the file under the home directory
		err        string
	}{
		{name: "nothing set", perProject: true},
		{name: "principals by stand-in", body: "telemetry:\n  principals: pseudonym\n", perProject: true},
		{name: "principals by name", body: "telemetry:\n  principals: name\n", perProject: true, principals: "name"},
		{name: "principals left out", body: "telemetry:\n  principals: omit\n", perProject: true, principals: "omit"},
		{name: "principals of another kind", body: "telemetry:\n  principals: hashed\n", err: "is not pseudonym, name or omit; fix: "},
		{name: "principals in capitals", body: "telemetry:\n  principals: Name\n", err: "is not pseudonym, name or omit; fix: "},
		{name: "a key file of its own", body: "telemetry:\n  principals_key: /srv/starfix/standins.key\n", perProject: true, key: "/srv/starfix/standins.key"},
		{name: "a key file under the home directory", body: "telemetry:\n  principals_key: state/standins.key\n", perProject: true,
			key: "/var/lib/starfix/state/standins.key"},
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
			if want := cmp.Or(tc.principals, "pseudonym"); s.Telemetry.Principals != want {
				t.Errorf("ResolveSettings(%q).Telemetry.Principals = %q, want %q", tc.body, s.Telemetry.Principals, want)
			}
			if got, want := s.PrincipalsKey("/var/lib/starfix"), cmp.Or(tc.key, "/var/lib/starfix/telemetry/principals.key"); got != want {
				t.Errorf("ResolveSettings(%q).PrincipalsKey(/var/lib/starfix) = %q, want %q", tc.body, got, want)
			}
		})
	}
}
