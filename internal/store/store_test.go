package store

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

func TestMigrations(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	s := openStore(t, dsn, Options{})
	if n, err := migrate(t.Context(), s.w, s.now(), projectSchema, true); err != nil || n != 0 {
		t.Errorf("second migrate ran %d (err %v), want 0", n, err)
	}
	if _, err := s.w.ExecContext(t.Context(), `INSERT INTO schema_migrations VALUES (99, 'future', NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := testDB(t, dsn).Open(t.Context(), DefaultSchema, Options{}); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("open newer schema: %v, want ErrSchemaTooNew", err)
	}
}

// TestOpenNoMigrate pins Options.NoMigrate: a database this binary's
// migrations have all been applied to opens, and one that is behind is
// refused with ErrSchemaOld and left exactly as it was.
func TestOpenNoMigrate(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	// A database nothing has migrated: it is behind, and stays empty.
	if _, err := testDB(t, dsn).Open(t.Context(), DefaultSchema, Options{NoMigrate: true}); !errors.Is(err, ErrSchemaOld) {
		t.Fatalf("Open(NoMigrate) of an empty database: %v, want ErrSchemaOld", err)
	}
	s := openStore(t, dsn, Options{})
	count := func(q string) int {
		t.Helper()
		var n int
		if err := s.r.QueryRowContext(t.Context(), q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const tables = `SELECT COUNT(*) FROM pg_tables WHERE schemaname = current_schema()`
	const version = `SELECT COUNT(*) FROM schema_migrations`

	cur, err := testDB(t, dsn).Open(t.Context(), DefaultSchema, Options{NoMigrate: true})
	if err != nil {
		t.Fatalf("Open(NoMigrate) of a migrated database: %v", err)
	}
	if err := cur.Close(); err != nil {
		t.Fatal(err)
	}

	// One migration behind, as an older binary left it.
	last := count(version)
	if _, err := s.w.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE version = $1`, last); err != nil {
		t.Fatal(err)
	}
	before := count(tables)
	if _, err := testDB(t, dsn).Open(t.Context(), DefaultSchema, Options{NoMigrate: true}); !errors.Is(err, ErrSchemaOld) {
		t.Errorf("Open(NoMigrate) of a database one migration behind: %v, want ErrSchemaOld", err)
	}
	if got := count(version); got != last-1 {
		t.Errorf("after the refused Open the database is at version %d, want %d: it must not migrate", got, last-1)
	}
	if got := count(tables); got != before {
		t.Errorf("after the refused Open the database has %d tables, want %d", got, before)
	}
	// Newer is still its own refusal.
	if _, err := s.w.ExecContext(t.Context(), `INSERT INTO schema_migrations VALUES (99, 'future', NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := testDB(t, dsn).Open(t.Context(), DefaultSchema, Options{NoMigrate: true}); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("Open(NoMigrate) of a newer schema: %v, want ErrSchemaTooNew", err)
	}
}

func TestLoadMigrations(t *testing.T) {
	t.Parallel()
	ms, err := loadMigrations(migrationFiles, "migrations")
	if err != nil || len(ms) == 0 || ms[0].version != 1 || len(ms[0].stmts) != 63 {
		t.Fatalf("embedded migrations: %v %+v", err, ms)
	}
	bad := []struct {
		name string
		fs   fstest.MapFS
	}{
		{"gap", fstest.MapFS{"migrations/0001_a.sql": {}, "migrations/0003_c.sql": {}}},
		{"bad name", fstest.MapFS{"migrations/init.sql": {}}},
		{"zero", fstest.MapFS{"migrations/0000_a.sql": {}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadMigrations(tc.fs, "migrations"); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestSplitSQL(t *testing.T) {
	t.Parallel()
	got := splitSQL("-- comment\nCREATE TABLE a (\n  x INT\n);\n\nCREATE TABLE b (y INT);\nSELECT 1")
	if len(got) != 3 || !strings.HasPrefix(got[0], "CREATE TABLE a") || got[2] != "SELECT 1" {
		t.Errorf("splitSQL = %q", got)
	}
}

// TestDefaultPrefix pins the prefix of the ids a store makes when it is
// given none: "ar", which every issue of a new project carries for good.
func TestDefaultPrefix(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	s, err := testDB(t, dsn).Open(t.Context(), DefaultSchema, Options{})
	if err != nil {
		t.Fatalf("Open(no prefix): %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	is := mustCreate(t, s, NewIssue{})
	if !strings.HasPrefix(string(is.ID), "ar-") {
		t.Errorf("CreateIssue with no prefix set made id %q, want one starting ar-", is.ID)
	}
}
