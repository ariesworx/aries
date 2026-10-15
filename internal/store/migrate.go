package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

//go:embed controlmigrations/*.sql
var controlMigrationFiles embed.FS

// schema is one embedded migration set and where a schema records how
// far it has run: a project's ([Open]) or the control store's
// ([OpenControl]).
type schema struct {
	// kind names what the database holds, for refusals.
	kind string
	fsys fs.FS
	// dir is the directory of fsys holding NNNN_name.sql.
	dir string
	// table records the versions applied.
	table string
	// foreign is the other set's table. A schema that has it holds the
	// other kind, and migrating it as this one would mix the two.
	foreign string
}

var (
	projectSchema = schema{kind: "a project", fsys: migrationFiles, dir: "migrations",
		table: "schema_migrations", foreign: "control_migrations"}
	controlSchema = schema{kind: "the control store", fsys: controlMigrationFiles, dir: "controlmigrations",
		table: "control_migrations", foreign: "schema_migrations"}
)

// migration is one NNNN_name.sql file of a schema: its version (NNNN),
// its file name without .sql, and its statements.
type migration struct {
	version int
	name    string
	stmts   []string
}

// loadMigrations parses dir/NNNN_name.sql in version order.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	names, err := fs.Glob(fsys, path.Join(dir, "*.sql"))
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	var ms []migration
	for _, n := range names {
		base := path.Base(n)
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v < 1 {
			return nil, fmt.Errorf("migration %s: name must be NNNN_name.sql", base)
		}
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", base, err)
		}
		ms = append(ms, migration{version: v, name: strings.TrimSuffix(base, ".sql"), stmts: splitSQL(string(b))})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s: versions must run 1..n without gaps", m.name)
		}
	}
	return ms, nil
}

// splitSQL drops "--" comment lines and splits on semicolons that end a line.
func splitSQL(src string) []string {
	var stmts []string
	var cur strings.Builder
	for line := range strings.SplitSeq(src, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(t, ";") {
			stmts = append(stmts, strings.TrimSuffix(strings.TrimSpace(cur.String()), ";"))
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

// migrate applies sc's pending migrations and returns how many ran. It
// refuses, with ErrInvalid and before changing anything, a schema that
// holds the other set. Each migration runs in one transaction with the
// row that records it, since Postgres's DDL is transactional: one that
// fails leaves the schema as the one before left it.
//
// With apply false it changes nothing: a schema with migrations
// pending, or with none ever applied, is refused with ErrSchemaOld.
func migrate(ctx context.Context, db *sql.DB, now time.Time, sc schema, apply bool) (int, error) {
	ms, err := loadMigrations(sc.fsys, sc.dir)
	if err != nil {
		return 0, err
	}
	foreign, err := countTables(ctx, db, sc.foreign)
	if err != nil {
		return 0, fmt.Errorf("read tables: %w", err)
	}
	if foreign > 0 {
		return 0, fmt.Errorf("%w: the schema does not hold %s: it has the table %s; check the dsn", ErrInvalid, sc.kind, sc.foreign)
	}
	if !apply {
		versioned, err := countTables(ctx, db, sc.table)
		if err != nil {
			return 0, fmt.Errorf("read tables: %w", err)
		}
		if versioned == 0 {
			return 0, fmt.Errorf("%w: schema was never migrated, binary knows %d", ErrSchemaOld, len(ms))
		}
	} else if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+sc.table+` (
  version    INT          NOT NULL PRIMARY KEY,
  name       VARCHAR(255) NOT NULL,
  applied_at TIMESTAMP(6) NOT NULL)`); err != nil {
		return 0, fmt.Errorf("create %s: %w", sc.table, err)
	}
	cur, err := lastKey(ctx, db, `SELECT version FROM `+sc.table+` ORDER BY version DESC LIMIT 1`)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if cur > int64(len(ms)) {
		return 0, fmt.Errorf("%w: schema at version %d, binary knows %d", ErrSchemaTooNew, cur, len(ms))
	}
	if !apply && cur < int64(len(ms)) {
		return 0, fmt.Errorf("%w: schema at version %d, binary knows %d", ErrSchemaOld, cur, len(ms))
	}
	n := 0
	for _, m := range ms[cur:] { //nolint:gosec // 0 <= cur <= len(ms)
		if err := applyMigration(ctx, db, now, sc, m); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// applyMigration runs m's statements and records it in sc's table, in one
// transaction.
func applyMigration(ctx context.Context, db *sql.DB, now time.Time, sc schema, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	for _, s := range m.stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return errors.Join(fmt.Errorf("migration %s: %w", m.name, err), rollback(tx))
		}
	}
	record := `INSERT INTO ` + sc.table + ` (version, name, applied_at) VALUES ($1, $2, $3)` //nolint:gosec // sc.table is a constant
	if _, err := tx.ExecContext(ctx, record, m.version, m.name, now); err != nil {
		return errors.Join(fmt.Errorf("record migration %s: %w", m.name, err), rollback(tx))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	return nil
}

// countTables counts the tables named name in the connection's schema,
// the first of its search_path.
func countTables(ctx context.Context, db *sql.DB, name string) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_tables WHERE schemaname = current_schema() AND tablename = $1`, name).Scan(&n)
	return n, err
}
