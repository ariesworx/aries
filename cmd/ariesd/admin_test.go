package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/pgtest"
	"github.com/ariesworx/aries/internal/store"
)

// pgs are the package's Postgres servers: a test has one to itself while
// it runs, so the tests can run in parallel (pgtest.Pool).
var pgs = pgtest.NewPool("ariesd-admin-")

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	defer func() { _ = pgs.Close() }()
	pgs.Warm()
	return m.Run()
}

// pgOf returns the Postgres server t's test holds, or skips without
// Postgres.
func pgOf(t testing.TB) *pgtest.Server {
	t.Helper()
	srv, err := pgs.Server(t)
	if errors.Is(err, pgtest.ErrNoPostgres) {
		t.Skip("postgres is not on PATH: run scripts/install-postgres.sh to run the admin command tests")
	}
	if err != nil {
		t.Fatalf("postgres: %v", err)
	}
	return srv
}

// newDSN returns an empty database's DSN in $ARIESD_DSN form, on the
// test's Postgres server, owned by the locked-down role it names. The
// database is dropped when the test ends, after the cleanups of what the
// test opened on it.
func newDSN(t testing.TB) string {
	t.Helper()
	pgOf(t)
	dsn, err := pgs.Database(t)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

type result struct {
	stdout, stderr string
	err            error
}

// admin runs an admin command in process against dsn, with no config file.
func admin(t *testing.T, dsn string, stdin string, cmd func(context.Context, adminEnv, []string) error, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	cfg := filepath.Join(t.TempDir(), "ariesd.yaml")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := adminEnv{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb, getenv: func(k string) string {
		if k == "ARIESD_DSN" {
			return dsn
		}
		return ""
	}}
	err := cmd(t.Context(), env, append([]string{"--config", cfg}, args...))
	return result{out.String(), errb.String(), err}
}

const backlog = "../../internal/bdimport/testdata/backlog.jsonl"

func TestImportBDCommand(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)

	dry := admin(t, dsn, "", importBD, "--dry-run", backlog)
	if dry.err != nil || !strings.HasPrefix(dry.stdout, "would import 10 issues (10 created)") || strings.Count(dry.stdout, "\n") != 1 {
		t.Fatalf("dry run: %+v", dry)
	}

	got := admin(t, dsn, "", importBD, backlog)
	if got.err != nil {
		t.Fatalf("import: %v\n%s", got.err, got.stderr)
	}
	if want := "imported 10 issues (10 created), 8 deps (8 created), 3 comments (3 created), 1 memory (1 created); 0 errors, 16 warnings\n"; got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
	if n, f := strings.Count(got.stderr, "warning: "), strings.Count(got.stderr, "\nfix: "); n != 16 || f != 16 {
		t.Errorf("stderr has %d warnings and %d fix lines:\n%s", n, f, got.stderr)
	}

	again := admin(t, dsn, "", importBD, "--json", backlog)
	if again.err != nil || again.stderr != "" {
		t.Fatalf("json import: %+v", again)
	}
	var doc struct {
		Summary string `json:"summary"`
		Issues  struct {
			Unchanged int `json:"unchanged"`
		} `json:"issues"`
		Problems []struct {
			Level, Kind, Fix string
			IDs              []string
		} `json:"problems"`
	}
	dec := json.NewDecoder(strings.NewReader(again.stdout))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, again.stdout)
	}
	if dec.More() {
		t.Error("more than one JSON document")
	}
	if doc.Issues.Unchanged != 10 || len(doc.Problems) != 16 || !strings.HasPrefix(doc.Summary, "imported 10 issues (10 unchanged)") {
		t.Errorf("json report = %+v", doc)
	}
}

func TestImportBDFailures(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	cases := []struct {
		name   string
		stdin  string
		args   []string
		code   bool // exits 1 after printing the report
		stderr string
		usage  bool
	}{
		{name: "no file", args: nil, usage: true},
		{name: "two files", args: []string{"a", "b"}, usage: true},
		{name: "unknown flag", args: []string{"--colour", "x"}, usage: true},
		{name: "missing file", args: []string{"/nonexistent/bd.jsonl"}, stderr: "fix: give the path of bd's export"},
		{name: "bad line from stdin", args: []string{"-"}, stdin: "{oops\n", code: true,
			stderr: "error: line 1 is not a JSON object"},
		{name: "dangling dependency", args: []string{"-"}, code: true,
			stdin:  `{"id":"ex-a1","title":"A","created_at":"2026-02-01T00:00:00Z","dependencies":[{"depends_on_id":"ex-zz","type":"blocks"}]}`,
			stderr: "error: ex-a1 → ex-zz (blocks): ex-zz is not in the file or the store; skipped (ex-a1, ex-zz)\nfix: import ex-zz too, or remove the dependency in bd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := admin(t, dsn, tc.stdin, importBD, tc.args...)
			switch {
			case tc.usage:
				if _, ok := errors.AsType[usageError](r.err); !ok {
					t.Errorf("err = %v, want a usage error", r.err)
				}
			case tc.code:
				if code, ok := r.err.(exitError); !ok || code != 1 { //nolint:errorlint // the bare value is the contract
					t.Errorf("err = %v, want exit 1", r.err)
				}
				if !strings.Contains(r.stderr, tc.stderr) {
					t.Errorf("stderr = %q, want %q", r.stderr, tc.stderr)
				}
				if strings.Count(r.stdout, "\n") != 1 {
					t.Errorf("stdout is not one summary line: %q", r.stdout)
				}
			default:
				if r.err == nil || !strings.Contains(r.err.Error(), tc.stderr) {
					t.Errorf("err = %v, want %q", r.err, tc.stderr)
				}
			}
		})
	}

	none := admin(t, "", "", importBD, backlog)
	if none.err == nil || !strings.Contains(none.err.Error(), "fix: set $ARIESD_DSN") {
		t.Errorf("no DSN: %v", none.err)
	}
}

func TestExportBDCommand(t *testing.T) {
	t.Parallel()
	src := newDSN(t)
	if r := admin(t, src, "", importBD, backlog); r.err != nil {
		t.Fatal(r.err)
	}
	stdout := admin(t, src, "", exportBD)
	if stdout.err != nil || strings.Count(stdout.stdout, "\n") != 10 {
		t.Fatalf("export to stdout: %+v", stdout)
	}
	file := filepath.Join(t.TempDir(), "out.jsonl")
	r := admin(t, src, "", exportBD, "-o", file)
	if r.err != nil || r.stderr != "exported 10 issues to "+file+"\n" || r.stdout != "" {
		t.Fatalf("export to file: %+v", r)
	}
	b, err := os.ReadFile(file) //nolint:gosec // test temp file
	if err != nil || string(b) != stdout.stdout {
		t.Fatalf("file differs from stdout export: %v", err)
	}

	// The export imports into an empty store with nothing to report but
	// the fields bd has and Aries does not; then it exports the same.
	dst := newDSN(t)
	in := admin(t, dst, "", importBD, file)
	if in.err != nil || !strings.HasPrefix(in.stdout, "imported 10 issues (10 created), 8 deps (8 created), 3 comments (3 created); 0 errors") {
		t.Fatalf("import of the export: %+v", in)
	}
	if again := admin(t, dst, "", exportBD); again.stdout != stdout.stdout {
		t.Errorf("round trip changed the export:\n%s\n%s", stdout.stdout, again.stdout)
	}

	if r := admin(t, src, "", exportBD, "extra"); r.err == nil {
		t.Error("export-bd accepted an argument")
	}
}

// Two daemons on one database: the second refuses to start, saying
// another process writes the project's schema, until the first stops.
func TestSecondDaemonRefusedToStart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, config string
	}{
		{"one project", "project: " + adopted + "\nadmins: [dana]\n"},
		{"many projects", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSite(t, "")
			if tc.config != "" {
				rewrite(t, s, tc.config)
			} else if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", writeKeys(t)); got.err != nil {
				t.Fatalf("control init: %v\n%s", got.err, got.stderr)
			}
			first, err := openDaemon(t.Context(), s.settings(t), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			_, err = openDaemon(t.Context(), s.settings(t), slog.New(slog.DiscardHandler))
			if err == nil || !strings.Contains(err.Error(), "served by another") {
				t.Fatalf("a second daemon on the database = %v, want it refused because the first writes the project's schema", err)
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			second, err := openDaemon(t.Context(), s.settings(t), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("a daemon once the first has stopped = %v", err)
			}
			if err := second.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// import-bd writes, and so is refused while a daemon serves the project,
// with what to do; its dry run and export-bd only read, and run beside
// it.
func TestImportBesideTheDaemon(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	daemon, err := store.Open(t.Context(), dsn, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := admin(t, dsn, "", importBD, backlog)
	if got.err == nil || !strings.Contains(got.err.Error(), "is being written by another process, most likely the running ariesd") ||
		!strings.Contains(got.err.Error(), "fix: stop ariesd") {
		t.Fatalf("import beside the daemon = %+v, want it refused with a fix that says to stop ariesd", got)
	}
	if dry := admin(t, dsn, "", importBD, "--dry-run", backlog); dry.err != nil || !strings.HasPrefix(dry.stdout, "would import 10 issues") {
		t.Errorf("a dry run beside the daemon = %+v, want the report", dry)
	}
	if exp := admin(t, dsn, "", exportBD); exp.err != nil {
		t.Errorf("export beside the daemon = %+v, want it to run", exp)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	if got := admin(t, dsn, "", importBD, backlog); got.err != nil {
		t.Errorf("import once the daemon has stopped = %v\n%s", got.err, got.stderr)
	}
}
