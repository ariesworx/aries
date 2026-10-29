package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// pending makes r's release carry a third migration of a project's
// schema, which the database has not run.
func (r *upgradeRig) pending() {
	r.d.migrations = func(context.Context, string) (migrationSet, error) {
		return migrationSet{project: []string{"0001_init", "0002_b", "0003_c"}, control: []string{"0001_init"}}, nil
	}
	r.d.schemas = func(context.Context, adminEnv, server.Settings, string) ([]schemaAt, error) {
		return []schemaAt{{label: "project " + upgradeProject, schema: "public", version: 2}}, nil
	}
}

func TestUpgradeDumpsBeforeMigrations(t *testing.T) {
	t.Parallel()
	r := newRig(t, "v0.2.0", "v0.3.0", "systemd_unit: ariesd\n")
	r.pending()
	out, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.dumps) != 1 {
		t.Fatalf("dumps %v, want one", r.dumps)
	}
	file := r.dumps[0]
	dir := filepath.Join(r.home, dumpsDir)
	if filepath.Dir(file) != dir || !strings.HasSuffix(file, "-v0.2.0-to-v0.3.0.dump") {
		t.Errorf("dump %s, want one in %s named for the versions", file, dir)
	}
	for _, want := range []string{
		"migrations: 1 to run, so upgrade takes a dump first\n",
		"  project " + upgradeProject + " (schema public): 0003_c\n",
		"dump: " + file + " (10 bytes)",
		"ariesd upgraded v0.2.0 -> v0.3.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	for p, mode := range map[string]os.FileMode{dir: 0o700, file: 0o600} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v; want mode %04o", p, fi.Mode(), err, mode)
		}
	}
	if r.exeHolds(t) != "v0.3.0" {
		t.Errorf("exe %q", r.exeHolds(t))
	}
}

func TestUpgradeDumpsWhenMigrationsUnknown(t *testing.T) {
	t.Parallel()
	r := newRig(t, "v0.2.0", "v0.3.0", "")
	r.d.migrations = func(context.Context, string) (migrationSet, error) {
		return migrationSet{}, errors.New(`unknown command "migrations"`)
	}
	out, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.dumps) != 1 || !strings.Contains(out, "v0.3.0 cannot list its migrations") {
		t.Errorf("dumps %v, stdout %q", r.dumps, out)
	}
}

func TestUpgradeNoDumpWithoutMigrations(t *testing.T) {
	t.Parallel()
	r := newRig(t, "v0.2.0", "v0.3.0", "")
	out, err := r.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.dumps) != 0 || strings.Contains(out, "dump") {
		t.Errorf("dumps %v, stdout %q", r.dumps, out)
	}
	if _, err := os.Stat(filepath.Join(r.home, dumpsDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("made the dumps directory: %v", err)
	}
}

func TestUpgradeGivenDump(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, body string, age time.Duration) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "before.dump")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r := newRig(t, "v0.2.0", "v0.3.0", "")
	r.pending()
	fresh := write(t, "PGDMP rows", 10*time.Minute)
	out, err := r.run(t, "--dump", fresh)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.dumps) != 0 || !strings.Contains(out, "dump: "+fresh) || r.exeHolds(t) != "v0.3.0" {
		t.Errorf("dumps %v, exe %q, stdout %q", r.dumps, r.exeHolds(t), out)
	}

	for name, tt := range map[string]struct {
		body string
		age  time.Duration
		want string
	}{
		"stale":   {"PGDMP rows", 2 * time.Hour, "written 2h0m0s ago"},
		"not one": {"-- plain SQL", 0, "not a dump in pg_dump's custom format"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, "v0.2.0", "v0.3.0", "")
			r.pending()
			_, err := r.run(t, "--dump", write(t, tt.body, tt.age))
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "fix: nothing was changed") {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if r.exeHolds(t) != "v0.2.0" || len(r.dumps) != 0 {
				t.Errorf("exe %q, dumps %v", r.exeHolds(t), r.dumps)
			}
		})
	}
}

func TestUpgradeDumpRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(t *testing.T, r *upgradeRig)
		want string
	}{
		{"dump fails", func(_ *testing.T, r *upgradeRig) {
			r.d.dump = func(_ context.Context, _ server.Settings, file string) error {
				r.dumps = append(r.dumps, file)
				return errors.New("pg_dump: server version mismatch")
			}
		}, "server version mismatch"},
		{"schemas unread", func(_ *testing.T, r *upgradeRig) {
			r.d.schemas = func(context.Context, adminEnv, server.Settings, string) ([]schemaAt, error) {
				return nil, errors.New("connection refused")
			}
		}, "make sure Postgres is running"},
		{"no home", func(_ *testing.T, r *upgradeRig) { r.vars["HOME"] = "" }, "set upgrade_dumps:"},
		{"loose directory", func(t *testing.T, r *upgradeRig) {
			dir := filepath.Join(r.home, dumpsDir)
			if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the loose directory under test
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // the loose directory under test
				t.Fatal(err)
			}
		}, "open to group or others"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, "v0.2.0", "v0.3.0", "systemd_unit: ariesd\n")
			r.pending()
			tt.edit(t, r)
			_, err := r.run(t)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "fix: nothing was changed") {
				t.Fatalf("err = %v, want %q and nothing changed", err, tt.want)
			}
			if strings.Contains(err.Error(), "replace ") {
				t.Errorf("reported as a failure to replace the binary: %v", err)
			}
			if r.exeHolds(t) != "v0.2.0" || len(r.restarts) != 0 {
				t.Errorf("exe %q, restarts %v", r.exeHolds(t), r.restarts)
			}
			if _, err := os.Stat(r.exe + ".prev"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("left a .prev: %v", err)
			}
			for _, f := range r.dumps {
				if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("left the failed dump %s: %v", f, err)
				}
			}
			assertOnlyExe(t, r)
		})
	}
}

// assertOnlyExe fails unless the binary's directory holds the binary
// alone, apart from the config file and the socket's path: no staged
// release is left behind.
func assertOnlyExe(t *testing.T, r *upgradeRig) {
	t.Helper()
	ents, err := os.ReadDir(filepath.Dir(r.exe))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.Contains(e.Name(), ".new-") {
			t.Errorf("left the staged release %s", e.Name())
		}
	}
}

func TestUpgradeCheckListsMigrations(t *testing.T) {
	t.Parallel()
	r := newRig(t, "v0.2.0", "v0.3.0", "")
	r.pending()
	r.d.schemas = func(context.Context, adminEnv, server.Settings, string) ([]schemaAt, error) {
		return []schemaAt{
			{label: "control store", schema: "aries_control", control: true, version: 1},
			{label: "project web", schema: "project_web", version: 1},
			{label: "project api", schema: "project_api", version: 3},
		}, nil
	}
	r.d.euid = func() int { return 0 }
	out, err := r.run(t, "--check")
	if err != nil {
		t.Fatal(err)
	}
	want := "ariesd v0.2.0, latest v0.3.0: run ariesd upgrade\n" +
		"migrations: 2 to run, so upgrade takes a dump first\n" +
		"  project web (schema project_web): 0002_b 0003_c\n"
	if out != want {
		t.Errorf("stdout\n%s\nwant\n%s", out, want)
	}
	if r.exeHolds(t) != "v0.2.0" || len(r.dumps) != 0 {
		t.Error("--check changed something")
	}
	assertOnlyExe(t, r)

	r.d.migrations = func(context.Context, string) (migrationSet, error) {
		return migrationSet{}, errors.New("exit status 2")
	}
	if out, err = r.run(t, "--check"); err != nil || !strings.Contains(out, "migrations: v0.3.0 cannot list its migrations (exit status 2)") {
		t.Errorf("%q, %v", out, err)
	}
	r.d.schemas = func(context.Context, adminEnv, server.Settings, string) ([]schemaAt, error) {
		return nil, errors.New("connection refused")
	}
	if out, err = r.run(t, "--check"); err != nil || !strings.Contains(out, "migrations: not checked: connection refused") {
		t.Errorf("%q, %v", out, err)
	}
	for _, args := range [][]string{{"--check", "--dump", "x"}, {"--rollback", "--dump", "x"}} {
		var ue usageError
		if _, err := r.run(t, args...); !errors.As(err, &ue) {
			t.Errorf("%v: %v, want a usage error", args, err)
		}
	}
}

func TestMigrationsCommand(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := migrationsCmd(adminEnv{stdout: &out}, nil); err != nil {
		t.Fatal(err)
	}
	m, err := parseMigrations(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	for control, got := range map[bool][]string{false: m.project, true: m.control} {
		want, err := store.Migrations(control)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("control %v: %v, want %v (%v)", control, got, want, err)
		}
	}
	for _, bad := range []string{"", "project init\n", "other 0001_init\n", "control 0001_init\n"} {
		if _, err := parseMigrations([]byte(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestPendingMigrations(t *testing.T) {
	t.Parallel()
	target := migrationSet{project: []string{"0001_a", "0002_b"}, control: []string{"0001_a", "0002_b", "0003_c"}}
	got := pendingMigrations(target, []schemaAt{
		{schema: "c", control: true, version: 1},
		{schema: "p1", version: 2},
		{schema: "p2", version: 0},
		{schema: "p3", version: 5}, // newer than the release: never an upgrade's
	})
	var lines []string
	for _, p := range got {
		lines = append(lines, p.schema+":"+strings.Join(p.names, ","))
	}
	if want := "c:0002_b,0003_c p2:0001_a,0002_b"; strings.Join(lines, " ") != want {
		t.Errorf("got %v, want %s", lines, want)
	}
}

// TestPgDumpRealDatabase dumps a real project's database as the
// locked-down role ariesd connects as, over the unix socket, and reads
// how far its schema has run.
func TestPgDumpRealDatabase(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	st, err := store.Open(t.Context(), dsn, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "ariesd.yaml")
	if err := os.WriteFile(cfg, []byte("project: "+upgradeProject+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := adminEnv{getenv: func(k string) string {
		if k == server.EnvDSN {
			return dsn
		}
		return ""
	}}
	schemas, err := readSchemas(t.Context(), env, server.Settings{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	names, err := store.Migrations(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != 1 || schemas[0].version != len(names) || schemas[0].control {
		t.Errorf("schemas %+v, want one at %d", schemas, len(names))
	}

	file := filepath.Join(t.TempDir(), "before.dump")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pgDump(t.Context(), server.Settings{DSN: dsn}, file); err != nil {
		t.Fatal(err)
	}
	if err := checkDump(file, time.Now()); err != nil {
		t.Errorf("checkDump: %v", err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() < 1000 {
		t.Errorf("dump: %v, %v", fi, err)
	}
}
