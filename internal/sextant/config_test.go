package sextant

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	projectA = "6f1c2b3a-4d5e-4f60-8a7b-9c0d1e2f3a4b"
	projectB = "0a1b2c3d-4e5f-4a6b-8c7d-8e9f0a1b2c3d"
	hostKey  = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

// A config with only what it must have takes the defaults of
// sextant.md §3 and names each project's server as .aries.yaml would.
func TestParseDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Parse([]byte("server:\n  url: https://aries.example.com\nkey: id_ed25519\nprojects:\n  - "+projectA+"\n"), dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Interval != DefaultInterval || cfg.Window != DefaultWindow || cfg.Listen != DefaultListen {
		t.Errorf("interval %s, window %s, listen %q; want the defaults", cfg.Interval, cfg.Window, cfg.Listen)
	}
	if cfg.Limits.Projects != DefaultProjects || cfg.Limits.Models != DefaultModels || cfg.Limits.RequestTimeout != DefaultRequestTimeout {
		t.Errorf("limits %+v, want the defaults", cfg.Limits)
	}
	if want := filepath.Join(dir, "id_ed25519"); cfg.KeyFile() != want {
		t.Errorf("KeyFile() = %q, want %q", cfg.KeyFile(), want)
	}
	cc, err := cfg.Client(projectA)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if cc.Project != projectA || cc.Server.URL != "https://aries.example.com" || cc.Root != dir || cc.Key != "" {
		t.Errorf("Client(%s) = project %q, url %q, root %q, key %q", projectA, cc.Project, cc.Server.URL, cc.Root, cc.Key)
	}
}

func TestParseFull(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Parse([]byte(`server:
  host: aries.example.com
  port: 2222
  host_key: `+hostKey+`
key: /etc/sextant/id_ed25519
projects: [`+projectA+`, `+projectB+`]
interval: 30s
window: 168h
listen: "[::1]:9466"
limits:
  projects: 2
  models: 5
  request_timeout: 10s
`), dir)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Interval != 30*time.Second || cfg.Window != 168*time.Hour || cfg.Listen != "[::1]:9466" || len(cfg.Projects) != 2 {
		t.Errorf("Parse = %+v", cfg)
	}
	if cfg.KeyFile() != "/etc/sextant/id_ed25519" {
		t.Errorf("KeyFile() = %q", cfg.KeyFile())
	}
	cc, err := cfg.Client(projectB)
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if cc.Server.Host != "aries.example.com" || cc.Server.Port != 2222 || cc.Server.User != "aries" || cc.Project != projectB {
		t.Errorf("Client(%s).Server = %+v", projectB, cc.Server)
	}
}

func TestParseWorkload(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte("server:\n  url: https://aries.example.com\nworkload:\n  kind: google\n  provider: gcp\ndata_dir: /var/lib/sextant\nprojects: ["+projectA+"]\n"), t.TempDir())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Workload == nil || cfg.Workload.Kind != "google" || cfg.Workload.Provider != "gcp" || cfg.DataDir != "/var/lib/sextant" || cfg.KeyFile() != "" {
		t.Errorf("Parse = %+v, workload %+v", cfg, cfg.Workload)
	}
}

// Every refusal names what to change.
func TestParseRefusals(t *testing.T) {
	t.Parallel()
	const server = "server:\n  url: https://aries.example.com\n"
	const key = "key: id_ed25519\n"
	const one = "projects: [" + projectA + "]\n"
	tests := []struct {
		name, yaml, want string
	}{
		{"no server", key + one, "server: is not set"},
		{"no project", server + key, "projects: lists no project"},
		{"not a project's id", server + key + "projects: [acme]\n", `projects: "acme" is not a project's lowercase UUID`},
		{"a project twice", server + key + "projects: [" + projectA + ", " + projectA + "]\n", "listed twice"},
		{"more projects than the limit", server + key + "projects: [" + projectA + ", " + projectB + "]\nlimits:\n  projects: 1\n", "more than limits.projects (1)"},
		{"no way to sign in", server + one, "neither key: nor workload: is set"},
		{"both ways to sign in", server + key + one + "workload:\n  kind: google\ndata_dir: /var/lib/sextant\n", "both key: and workload: are set"},
		{"a workload with no data_dir", server + one + "workload:\n  kind: google\n", "data_dir: is not set"},
		{"a workload of no kind", server + one + "workload:\n  kind: jenkins\ndata_dir: /d\n", `workload.kind "jenkins" is not one of azure, github-actions, google`},
		{"a workload over SSH", "server:\n  host: aries.example.com\n  host_key: " + hostKey + "\n" + one + "workload:\n  kind: google\ndata_dir: /d\n", "workload: signs in over HTTPS"},
		{"an interval too short", server + key + one + "interval: 1s\n", "interval: 1s is shorter than 10s"},
		{"a window too long", server + key + one + "window: 1000h\n", "window: 1000h0m0s is longer than 744h0m0s"},
		{"a window too short", server + key + one + "window: 1s\n", "window: 1s is shorter than 1m0s"},
		{"too many models", server + key + one + "limits:\n  models: 501\n", "limits.models: 501 is not from 1 to 500"},
		{"a request timeout too long", server + key + one + "limits:\n  request_timeout: 10m\n", "limits.request_timeout: 10m0s is not from 1s to 2m0s"},
		{"a listener off loopback", server + key + one + "listen: 0.0.0.0:9466\n", "not a loopback address"},
		{"an unknown key", server + key + one + "pepole: name\n", "field pepole not found"},
		{"an unknown server key", "server:\n  url: https://aries.example.com\n  colour: red\n" + key + one, "server.colour is not a key sextant knows"},
		{"a bad server", "server:\n  url: http://aries.example.com\n" + key + one, "server.url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.yaml), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) = %v, want an error holding %q", tc.yaml, err, tc.want)
			}
			if !strings.Contains(err.Error(), "fix: ") {
				t.Errorf("Parse(%q) = %v, which names no fix", tc.yaml, err)
			}
		})
	}
}

// Load reads the file and takes its directory as the one relative paths
// start from; a file others can write is refused, since it says which
// key Sextant offers.
func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sextant.yaml")
	if err := os.WriteFile(path, []byte("server:\n  url: https://aries.example.com\nkey: id_ed25519\nprojects: ["+projectA+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.KeyFile() != filepath.Join(dir, "id_ed25519") {
		t.Errorf("KeyFile() = %q", cfg.KeyFile())
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err == nil || !strings.Contains(err.Error(), "fix: ") {
		t.Errorf("Load of a missing file = %v, want a refusal with a fix", err)
	}
	if runtime.GOOS == "windows" {
		return // no mode bits to check
	}
	if err := os.Chmod(path, 0o666); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm()&0o022 == 0 {
		t.Skip("this system keeps no group or world write bit")
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "others can write") {
		t.Errorf("Load of a file others can write = %v, want refused", err)
	}
}
