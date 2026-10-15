package store

import (
	"context"
	"database/sql"
	"embed"
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

// schema is one embedded migration set and where a database records how
// far it has run: a project's database ([Open]) or the control database
// ([OpenControl]).
type schema struct {
	// kind names what the database holds, for refusals.
	kind string
	fsys fs.FS
	// dir is the directory of fsys holding NNNN_name.sql.
	dir string
	// table records the versions applied.
	table string
	// foreign is the other schema's table. A database that has it holds
	// the other kind, and migrating it as this one would mix the two.
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
// refuses, with ErrInvalid and before changing anything, a database that
// holds the other schema. DDL is not transactional, so a migration that
// fails halfway needs repair by hand; each is small and runs once.
//
// With apply false it changes nothing: a database with migrations
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
		return 0, fmt.Errorf("%w: the database does not hold %s: it has the table %s; check the dsn", ErrInvalid, sc.kind, sc.foreign)
	}
	if !apply {
		versioned, err := countTables(ctx, db, sc.table)
		if err != nil {
			return 0, fmt.Errorf("read tables: %w", err)
		}
		if versioned == 0 {
			return 0, fmt.Errorf("%w: database was never migrated, binary knows %d", ErrSchemaOld, len(ms))
		}
	} else if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+sc.table+` (
  version    INT          NOT NULL PRIMARY KEY,
  name       VARCHAR(255) NOT NULL,
  applied_at DATETIME(6)  NOT NULL)`); err != nil {
		return 0, fmt.Errorf("create %s: %w", sc.table, err)
	}
	cur, err := lastKey(ctx, db, `SELECT version FROM `+sc.table+` ORDER BY version DESC LIMIT 1`)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if cur > int64(len(ms)) {
		return 0, fmt.Errorf("%w: database at version %d, binary knows %d", ErrSchemaTooNew, cur, len(ms))
	}
	if !apply && cur < int64(len(ms)) {
		return 0, fmt.Errorf("%w: database at version %d, binary knows %d", ErrSchemaOld, cur, len(ms))
	}
	n := 0
	for _, m := range ms[cur:] { //nolint:gosec // 0 <= cur <= len(ms)
		for _, s := range m.stmts {
			if _, err := db.ExecContext(ctx, s); err != nil {
				return n, fmt.Errorf("migration %s: %w", m.name, err)
			}
		}
		record := `INSERT INTO ` + sc.table + ` (version, name, applied_at) VALUES (?, ?, ?)` //nolint:gosec // sc.table is a constant
		if _, err := db.ExecContext(ctx, record, m.version, m.name, now); err != nil {
			return n, fmt.Errorf("record migration %s: %w", m.name, err)
		}
		n++
	}
	return n, nil
}

// countTables counts the tables named name in db's current database.
//
// It lists them with SHOW TABLES, which reads that database alone. A
// query of information_schema reads every database on the server, and
// Dolt (2.4) fails it with "no root value found in session" or "no state
// for database" when it meets one that another connection is creating
// or dropping: in tests, where databases come and go beside each other,
// and on a server, where an admin adds a project while the daemon opens
// another's store (docs/design/database.md). A database holds a few
// dozen tables, so they are listed and compared here.
func countTables(ctx context.Context, db *sql.DB, name string) (int, error) {
	rows, err := db.QueryContext(ctx, `SHOW TABLES`)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return 0, err
		}
		if t == name {
			n++
		}
	}
	return n, rows.Err()
}
