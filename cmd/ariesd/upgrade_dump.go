package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// Migrations only go forward, and --rollback swaps only the binary, so
// an upgrade whose release migrates the database takes a dump first:
// with nothing else to go back to, a release that migrated and then
// misbehaves can only be left by restoring it. The running binary does
// not know what the target release carries, so it asks the release
// itself (`ariesd migrations`, run in the staged, verified binary) and
// compares that with how far each schema has run.

// freshDump is how old a dump given with --dump may be.
const freshDump = time.Hour

// dumpsDir is the directory upgrade writes its dumps to, under the home
// directory, when upgrade_dumps does not name one.
const dumpsDir = "upgrade-dumps"

// migrationSet is what a binary's `ariesd migrations` lists: the names
// (NNNN_name) of its migrations of a project's schema and of the
// control store's, in order.
type migrationSet struct {
	project, control []string
}

// migrationsCmd runs `ariesd migrations`: one line for each migration
// this binary carries, "project NNNN_name" or "control NNNN_name". An
// older ariesd runs it in a release it is about to install.
func migrationsCmd(env adminEnv, args []string) error {
	if len(args) > 0 {
		return usageError("migrations takes no arguments")
	}
	for _, set := range []struct {
		kind    string
		control bool
	}{{"project", false}, {"control", true}} {
		names, err := store.Migrations(set.control)
		if err != nil {
			return err
		}
		for _, n := range names {
			_, _ = fmt.Fprintf(env.stdout, "%s %s\n", set.kind, n)
		}
	}
	return nil
}

// parseMigrations reads what `ariesd migrations` printed.
func parseMigrations(out []byte) (migrationSet, error) {
	var m migrationSet
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		kind, name, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		num, _, numOK := strings.Cut(name, "_")
		if _, err := strconv.Atoi(num); !ok || !numOK || err != nil || len(num) != 4 {
			return migrationSet{}, fmt.Errorf("unexpected line %q", sc.Text())
		}
		switch kind {
		case "project":
			m.project = append(m.project, name)
		case "control":
			m.control = append(m.control, name)
		default:
			return migrationSet{}, fmt.Errorf("unexpected line %q", sc.Text())
		}
	}
	if len(m.project) == 0 {
		return migrationSet{}, errors.New("it listed no migration of a project")
	}
	return m, nil
}

// runMigrations runs `<path> migrations`, path being the release's
// staged, verified binary. A release older than the command fails it;
// the caller takes that as migrations it cannot name.
func runMigrations(ctx context.Context, path string) (migrationSet, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "migrations") //nolint:gosec // path is the verified binary just staged
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return migrationSet{}, fmt.Errorf("%w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return parseMigrations(out.Bytes())
}

// schemaAt is a schema the restarted daemon migrates at its start, and
// how far it has run.
type schemaAt struct {
	// label names it for people: "control store", or "project NAME".
	label   string
	schema  string
	control bool
	version int
}

// readSchemas reads how far each schema the restarted daemon would
// migrate has run: the one project's, or the control store's and each
// active project's. An archived project's schema is migrated when it is
// activated, by the binary then running, and is not among them. It
// changes nothing, beside the running daemon.
func readSchemas(ctx context.Context, env adminEnv, fl server.Settings, cfgPath string) ([]schemaAt, error) {
	s, err := server.ResolveSettings(fl, cfgPath, env.getenv)
	if err != nil {
		return nil, err
	}
	if s.DSN == "" {
		return nil, errors.New("no database DSN; fix: set $ARIESD_DSN or dsn: in the config file")
	}
	d, err := store.ParseDSN(s.DSN)
	if err != nil {
		return nil, err
	}
	db, err := store.OpenDB(ctx, s.DSN, store.DBOptions{Conns: 2})
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	var out []schemaAt
	add := func(label, schema string, control bool) error {
		v, err := db.Migrated(ctx, schema, control)
		if err != nil {
			return err
		}
		out = append(out, schemaAt{label: label, schema: schema, control: control, version: v})
		return nil
	}
	if s.ControlSchema == "" {
		return out, add("project "+s.Project, d.Schema, false)
	}
	if err := add("control store", s.ControlSchema, true); err != nil {
		return nil, err
	}
	ctl, err := openControl(ctx, s, db, store.ControlOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	projects, err := ctl.Projects(ctx)
	if err = closeControl(err, ctl); err != nil {
		return nil, fmt.Errorf("control schema %s: %w", s.ControlSchema, err)
	}
	for _, p := range projects {
		if p.State != store.ProjectActive {
			continue
		}
		if err := add("project "+p.Name, p.Schema, false); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// pendingSchema is a schema and the migrations the release will run in
// it.
type pendingSchema struct {
	schemaAt
	names []string
}

// pendingMigrations returns, for each schema with any, the migrations of
// target past the version it has run to.
func pendingMigrations(target migrationSet, schemas []schemaAt) []pendingSchema {
	var out []pendingSchema
	for _, sc := range schemas {
		names := target.project
		if sc.control {
			names = target.control
		}
		if sc.version < len(names) {
			out = append(out, pendingSchema{schemaAt: sc, names: names[max(sc.version, 0):]})
		}
	}
	return out
}

// migrationPlan is what an upgrade to a release will migrate: the
// pending migrations by schema, or, with unknown set, that the release
// could not say, which is taken as migrations to come.
type migrationPlan struct {
	pending []pendingSchema
	unknown error
}

// needsDump reports whether the upgrade may migrate the database.
func (p migrationPlan) needsDump() bool { return p.unknown != nil || len(p.pending) > 0 }

// planMigrations asks the staged release at path what it carries and
// reads how far each schema has run. Failing to read the schemas is an
// error; a release that cannot list its migrations is a plan with
// unknown set.
func planMigrations(ctx context.Context, env adminEnv, d upgradeDeps, fl server.Settings, cfgPath, path string) (migrationPlan, error) {
	schemas, err := d.schemas(ctx, env, fl, cfgPath)
	if err != nil {
		return migrationPlan{}, err
	}
	target, err := d.migrations(ctx, path)
	if err != nil {
		return migrationPlan{unknown: err}, nil
	}
	return migrationPlan{pending: pendingMigrations(target, schemas)}, nil
}

// print writes the plan as `upgrade --check` reports it: a line starting
// "migrations:", then a line for each schema with any to run.
func (p migrationPlan) print(w io.Writer, tag string) {
	switch {
	case p.unknown != nil:
		_, _ = fmt.Fprintf(w, "migrations: %s cannot list its migrations (%v), so upgrade takes a dump first, as for any\n", tag, p.unknown)
	case len(p.pending) == 0:
		_, _ = fmt.Fprintln(w, "migrations: none to run, so upgrade takes no dump")
	default:
		n := 0
		for _, ps := range p.pending {
			n += len(ps.names)
		}
		_, _ = fmt.Fprintf(w, "migrations: %d to run, so upgrade takes a dump first\n", n)
		for _, ps := range p.pending {
			_, _ = fmt.Fprintf(w, "  %s (schema %s): %s\n", ps.label, ps.schema, strings.Join(ps.names, " "))
		}
	}
}

// dumpBeforeMigrations makes sure a fresh dump of the database exists
// before an upgrade that migrates it: the one --dump names, checked,
// or else one it takes into the dumps directory. It returns the dump's
// path. Nothing has been installed when it is called, so each refusal
// says nothing was changed.
func dumpBeforeMigrations(ctx context.Context, env adminEnv, d upgradeDeps, s server.Settings, given, cur, tag string) (string, error) {
	if given != "" {
		if err := checkDump(given, d.now()); err != nil {
			return "", fmt.Errorf("--dump %s: %w; fix: nothing was changed; give a dump taken in the last %s with pg_dump --format=custom, or leave --dump out and upgrade takes one", given, err, freshDump)
		}
		return given, nil
	}
	dir := s.UpgradeDumps
	if dir == "" {
		home := env.getenv("HOME")
		if home == "" || !filepath.IsAbs(home) {
			return "", errors.New("no directory for the dump: HOME is not set; fix: nothing was changed; set upgrade_dumps: in the config file, or run upgrade with sudo -H")
		}
		dir = filepath.Join(home, dumpsDir)
	}
	if err := dumpDir(dir); err != nil {
		return "", fmt.Errorf("%w; fix: nothing was changed; make %s a directory of the user who runs upgrade, mode 0700, or name another in upgrade_dumps", err, dir)
	}
	file := filepath.Join(dir, fmt.Sprintf("ariesd-%s-%s-to-%s.dump", d.now().UTC().Format("20060102T150405Z"), cur, tag))
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dir is the operator's
	if err != nil {
		return "", fmt.Errorf("create the dump: %w; fix: nothing was changed; check %s", err, dir)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("create the dump: %w; fix: nothing was changed; check %s", err, dir)
	}
	if err := d.dump(ctx, s, file); err != nil {
		_ = os.Remove(file)
		return "", fmt.Errorf("dump the database before its migrations: %w; fix: nothing was changed; make pg_dump reach the database as ariesd does (docs/server.md, Upgrades), or take a dump yourself and give it with --dump FILE", err)
	}
	return file, nil
}

// dumpDir makes dir, mode 0700, or checks that the one there is a
// directory open to no one else.
func dumpDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("make %s: %w", dir, err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is open to group or others (mode %04o), and a dump holds every project", dir, fi.Mode().Perm())
	}
	return nil
}

// pgdmpMagic begins every dump in pg_dump's custom format.
const pgdmpMagic = "PGDMP"

// checkDump refuses a file that is not a dump in pg_dump's custom
// format, or was written longer than freshDump before now.
func checkDump(path string, now time.Time) error {
	f, err := os.Open(path) //nolint:gosec // the operator names it
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	head := make([]byte, len(pgdmpMagic))
	if _, err := io.ReadFull(f, head); err != nil || string(head) != pgdmpMagic {
		return errors.New("not a dump in pg_dump's custom format")
	}
	if age := now.Sub(fi.ModTime()); age > freshDump {
		return fmt.Errorf("written %s ago, more than %s", age.Round(time.Minute), freshDump)
	}
	return nil
}

// pgDump dumps the database s.DSN names to file, which exists, empty
// and mode 0600, in pg_dump's custom format, as the role ariesd
// connects as, which owns every schema in it. The URI on pg_dump's
// command line holds no password: a password the DSN holds goes in
// PGPASSWORD. It then reads the dump's table of contents back with
// pg_restore --list, so that a dump that cannot be restored is not taken
// for one.
func pgDump(ctx context.Context, s server.Settings, file string) error {
	uri, password, err := store.DumpTarget(s.DSN)
	if err != nil {
		return err
	}
	db, err := store.OpenDB(ctx, s.DSN, store.DBOptions{Conns: 1})
	if err != nil {
		return err
	}
	major, err := db.ServerMajor(ctx)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	env := os.Environ()
	if password != "" {
		env = append(env, "PGPASSWORD="+password)
	}
	run := func(name string, args ...string) error {
		bin := pgBin(name, major)
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // a Postgres client program, with arguments of ours
		cmd.Env = env
		var errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = io.Discard, &errOut
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errOut.String())
			if password != "" {
				msg = strings.ReplaceAll(msg, password, "REDACTED")
			}
			return fmt.Errorf("%s: %w: %s", bin, err, msg)
		}
		return nil
	}
	if err := run("pg_dump", "--format=custom", "--file="+file, "--dbname="+uri); err != nil {
		return err
	}
	if err := os.Chmod(file, 0o600); err != nil {
		return err
	}
	return run("pg_restore", "--list", file)
}

// pgBin finds a Postgres client program: the one of the server's major
// version where Debian and Ubuntu keep it, since a pg_dump older than
// the server refuses to dump it, or else the one on PATH.
func pgBin(name string, major int) string {
	p := filepath.Join("/usr/lib/postgresql", strconv.Itoa(major), "bin", name)
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// dumpRefusal is an error of the dump step that upgrade returns as it is,
// rather than as a failure to replace the binary.
type dumpRefusal struct{ error }

func (e dumpRefusal) Unwrap() error { return e.error }

// statDump returns the size of the dump at path, for the line that names
// it.
func statDump(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "missing"
		}
		return "unreadable"
	}
	return strconv.FormatInt(fi.Size(), 10) + " bytes"
}
