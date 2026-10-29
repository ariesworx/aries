package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/release"
	"github.com/ariesworx/aries/internal/release/releasetest"
	"github.com/ariesworx/aries/internal/server"
)

const upgradeProject = "00000000-0000-4000-8000-0000000000aa"

// upgradeRig is an ariesd install, a fake release and fake systemd: the
// "daemon" runs whatever the exe file holds, so a probe after a restart
// reports the version written in it.
type upgradeRig struct {
	gh       *releasetest.Server
	exe      string
	cfg      string
	running  string
	restarts []string
	d        upgradeDeps
	dsn      string
	// vars is the rest of the environment upgrade sees.
	vars map[string]string
	// healthy reports whether a daemon running version v answers.
	healthy func(v string) bool
	// home is the HOME upgrade sees, and dumps the files dump was asked
	// to write.
	home  string
	dumps []string
}

func newRig(t *testing.T, current, latest, configExtra string) *upgradeRig {
	t.Helper()
	dir := t.TempDir()
	r := &upgradeRig{gh: releasetest.New(t, latest), exe: filepath.Join(dir, "ariesd"), running: current,
		healthy: func(string) bool { return true }, home: filepath.Join(dir, "home")}
	r.vars = map[string]string{"HOME": r.home}
	r.gh.AddBinary(t, "ariesd", "linux", "amd64", []byte(latest))
	if err := os.WriteFile(r.exe, []byte(current), 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	r.cfg = filepath.Join(dir, "ariesd.yaml")
	cfg := "project: " + upgradeProject + "\nsocket: " + filepath.Join(dir, "d.sock") + "\n" + configExtra
	if err := os.WriteFile(r.cfg, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	r.d = upgradeDeps{
		current: current, goos: "linux", goarch: "amd64", euid: func() int { return 1000 },
		releases: r.gh.Client(r.gh.Verifier()), exe: r.exe,
		check: func(_ context.Context, p, tag string) error {
			if b, err := os.ReadFile(p); err != nil || string(b) != tag { //nolint:gosec // test temp dir
				return errors.New("staged binary does not report " + tag)
			}
			return nil
		},
		// By default the release carries the migrations the database has
		// run: there is nothing to dump.
		migrations: func(context.Context, string) (migrationSet, error) {
			return migrationSet{project: []string{"0001_init"}, control: []string{"0001_init"}}, nil
		},
		schemas: func(context.Context, adminEnv, server.Settings, string) ([]schemaAt, error) {
			return []schemaAt{{label: "project " + upgradeProject, schema: "public", version: 1}}, nil
		},
		dump: func(_ context.Context, _ server.Settings, file string) error {
			r.dumps = append(r.dumps, file)
			return os.WriteFile(file, []byte("PGDMP fake"), 0o600)
		},
		now: time.Now,
		restart: func(_ context.Context, unit string) error {
			r.restarts = append(r.restarts, unit)
			b, err := os.ReadFile(r.exe)
			r.running = string(b)
			return err
		},
		probe: func(_ context.Context, socket, project string) (string, error) {
			if project != upgradeProject || !strings.HasSuffix(socket, "d.sock") {
				return "", errors.New("probe of the wrong daemon")
			}
			if !r.healthy(r.running) {
				return "", errors.New("connection refused")
			}
			return r.running, nil
		},
	}
	return r
}

func (r *upgradeRig) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	env := adminEnv{stdout: &out, stderr: &out, getenv: func(k string) string {
		if k == "ARIESD_DSN" {
			return r.dsn
		}
		return r.vars[k]
	}}
	err := upgradeCmd(t.Context(), env, r.d, append([]string{"--config", r.cfg}, args...))
	return out.String(), err
}

func (r *upgradeRig) exeHolds(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(r.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeServerRestarts(t *testing.T) {
	t.Parallel()
	r := newRig(t, "v0.2.0", "v0.3.0", "systemd_unit: ariesd.service\n")
	out, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	want := "ariesd upgraded v0.2.0 -> v0.3.0 (ariesd.service restarted and healthy)\n"
	if out != want {
		t.Errorf("stdout %q, want %q", out, want)
	}
	if r.exeHolds(t) != "v0.3.0" || r.running != "v0.3.0" {
		t.Errorf("exe %q, running %q", r.exeHolds(t), r.running)
	}
	if strings.Join(r.restarts, ",") != "ariesd.service" {
		t.Errorf("restarts %v", r.restarts)
	}

	// The new binary reports the new version: a second run changes nothing.
	r.d.current = "v0.3.0"
	out, err = r.run(t)
	if err != nil || out != "ariesd v0.3.0 is up to date\n" || len(r.restarts) != 1 {
		t.Fatalf("second run: %q, %v, restarts %v", out, err, r.restarts)
	}

	if _, err := r.run(t, "--rollback"); err != nil {
		t.Fatal(err)
	}
	if r.exeHolds(t) != "v0.2.0" || r.running != "v0.2.0" {
		t.Errorf("after rollback: exe %q, running %q", r.exeHolds(t), r.running)
	}
	if _, err := r.run(t, "--rollback"); err == nil || !strings.Contains(err.Error(), "fix: nothing to roll back") {
		t.Errorf("second rollback: %v", err)
	}
}

func TestUpgradeServerWithoutUnit(t *testing.T) {
	t.Parallel()
	r := newRig(t, "v0.2.0", "v0.3.0", "")
	out, err := r.run(t)
	if err != nil || !strings.Contains(out, "installed ariesd v0.3.0") || !strings.Contains(out, "systemctl restart") {
		t.Fatalf("%q, %v", out, err)
	}
	if len(r.restarts) != 0 || r.exeHolds(t) != "v0.3.0" || r.running != "v0.2.0" {
		t.Errorf("restarts %v, exe %q, running %q", r.restarts, r.exeHolds(t), r.running)
	}

	r = newRig(t, "v0.2.0", "v0.3.0", "")
	if _, err := r.run(t, "--restart"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.restarts, ",") != "ariesd" {
		t.Errorf("--restart restarted %v, want the default unit", r.restarts)
	}
}

func TestUpgradeServerUnhealthyRestores(t *testing.T) {
	t.Parallel()
	r := newRig(t, "v0.2.0", "v0.3.0", "systemd_unit: ariesd\n")
	r.healthy = func(v string) bool { return v != "v0.3.0" }
	_, err := r.run(t)
	if err == nil || !strings.Contains(err.Error(), "restored v0.2.0, which is running") || !strings.Contains(err.Error(), "fix: check `journalctl -u ariesd`") {
		t.Fatalf("err = %v", err)
	}
	if r.exeHolds(t) != "v0.2.0" || r.running != "v0.2.0" || len(r.restarts) != 2 {
		t.Errorf("exe %q, running %q, restarts %v", r.exeHolds(t), r.running, r.restarts)
	}

	r = newRig(t, "v0.2.0", "v0.3.0", "systemd_unit: ariesd\n")
	r.healthy = func(string) bool { return false }
	if _, err := r.run(t); err == nil || !strings.Contains(err.Error(), "not healthy either") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpgradeServerCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, current, latest string
		args                  []string
		want                  string
	}{
		{"behind", "v0.2.0", "v0.3.0", nil, "ariesd v0.2.0, latest v0.3.0: run ariesd upgrade\nmigrations: none to run, so upgrade takes no dump\n"},
		{"current", "v0.3.0", "v0.3.0", nil, "ariesd v0.3.0, latest v0.3.0: up to date\n"},
		{"target", "v0.2.0", "v0.3.0", []string{"--to", "v0.3.0"}, "ariesd v0.2.0, target v0.3.0: run ariesd upgrade\nmigrations: none to run, so upgrade takes no dump\n"},
		{"dev", "dev", "v0.3.0", nil, "ariesd dev, latest v0.3.0: dev build; install a release to upgrade\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, tt.current, tt.latest, "systemd_unit: ariesd\n")
			r.d.euid = func() int { return 0 } // --check changes nothing, so root may run it
			out, err := r.run(t, append([]string{"--check"}, tt.args...)...)
			if err != nil || out != tt.want {
				t.Fatalf("%q, %v; want %q", out, err, tt.want)
			}
			if r.exeHolds(t) != tt.current || len(r.restarts) != 0 {
				t.Error("--check changed something")
			}
		})
	}
}

func TestUpgradeServerRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		current string
		args    []string
		edit    func(t *testing.T, r *upgradeRig)
		usage   bool
		want    string
	}{
		{name: "dev build", current: "dev", want: "development build"},
		{name: "root", current: "v0.2.0", edit: func(_ *testing.T, r *upgradeRig) { r.d.euid = func() int { return 0 } }, want: "refuses to run as root"},
		{name: "not linux", current: "v0.2.0", edit: func(_ *testing.T, r *upgradeRig) { r.d.goos = "darwin" }, want: "Linux server only"},
		{name: "downgrade", current: "v0.4.0", args: []string{"--to", "v0.3.0"}, want: "migrations only go forward"},
		{name: "no such release", current: "v0.2.0", args: []string{"--to", "v0.9.0"}, want: "no release v0.9.0"},
		{name: "bad --to", current: "v0.2.0", args: []string{"--to", "latest"}, usage: true, want: "not a release version"},
		{name: "rollback with --to", current: "v0.2.0", args: []string{"--rollback", "--to", "v0.3.0"}, usage: true, want: "cannot be combined"},
		{name: "unsigned", current: "v0.2.0", edit: func(_ *testing.T, r *upgradeRig) { r.gh.Omit[release.SignatureName] = true }, want: "failed verification"},
		{name: "no release key", current: "v0.2.0", edit: func(_ *testing.T, r *upgradeRig) {
			r.d.releases = r.gh.Client(release.Ed25519{Keys: []ed25519.PublicKey{}}) // a build with no keys
		}, want: "this build cannot verify releases"},
		{name: "tampered archive", current: "v0.2.0", edit: func(t *testing.T, r *upgradeRig) {
			s := r.gh.Checksums()
			r.gh.Sums = &s
			r.gh.AddBinary(t, "ariesd", "linux", "amd64", []byte("evil"))
		}, want: "checksum mismatch"},
		{name: "staged binary fails", current: "v0.2.0", edit: func(t *testing.T, r *upgradeRig) {
			r.gh.AddBinary(t, "ariesd", "linux", "amd64", []byte("v9.9.9"))
		}, want: "does not report v0.3.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, tt.current, "v0.3.0", "systemd_unit: ariesd\n")
			if tt.edit != nil {
				tt.edit(t, r)
			}
			_, err := r.run(t, tt.args...)
			var ue usageError
			if err == nil || !strings.Contains(err.Error(), tt.want) || errors.As(err, &ue) != tt.usage {
				t.Fatalf("err = %v, want %q (usage %v)", err, tt.want, tt.usage)
			}
			if !tt.usage && !strings.Contains(err.Error(), "fix: ") {
				t.Errorf("no fix: %v", err)
			}
			if r.exeHolds(t) != tt.current || len(r.restarts) != 0 {
				t.Errorf("refused upgrade: exe %q, restarts %v", r.exeHolds(t), r.restarts)
			}
			if _, err := os.Stat(r.exe + ".prev"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("refused upgrade left a .prev: %v", err)
			}
		})
	}
}
