package store

import (
	"testing"
)

// The figures are read as the tests' locked-down role, which is a member
// of no predefined role such as pg_monitor: what ariesd's role can see.
func TestDBStats(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	db := openDB(t, dsn, DBOptions{Conns: 5})
	makeSchema(t, dsn, "web")
	makeSchema(t, dsn, "api")
	s := openIn(t, db, "web", Options{})
	for range 3 {
		mustCreate(t, s, NewIssue{})
	}
	st, err := db.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"pool max is the DB's Conns", st.Pool.Max == 5},
		{"pool in use within its max", st.Pool.InUse >= 0 && st.Pool.InUse <= st.Pool.Conns && st.Pool.Conns <= st.Pool.Max},
		{"a version", st.Version != ""},
		{"connections within their limit", st.Conns >= 1 && st.Conns <= st.ConnsMax},
		{"this database's connections", st.DatabaseConns >= 1 && st.DatabaseConns <= st.Conns},
		{"commits counted", st.Commits >= 3},
		{"rollbacks, deadlocks, conflicts, temporary bytes not negative", st.Rollbacks >= 0 && st.Deadlocks >= 0 && st.Conflicts >= 0 && st.TempBytes >= 0},
		{"the database has a size", st.DatabaseBytes > 0},
	} {
		if !c.ok {
			t.Errorf("Stats() = %+v: want %s", st, c.name)
		}
	}

	sizes, err := db.SchemaBytes(t.Context(), []string{"web", "api", "missing"})
	if err != nil {
		t.Fatalf("SchemaBytes: %v", err)
	}
	if sizes["web"] <= 0 {
		t.Errorf("SchemaBytes(web), a migrated project's = %d, want more than zero", sizes["web"])
	}
	if n, ok := sizes["api"]; !ok || n != 0 {
		t.Errorf("SchemaBytes(api), an empty schema's = %d, %v, want 0, true", n, ok)
	}
	if n, ok := sizes["missing"]; ok {
		t.Errorf("SchemaBytes(missing) = %d, want none for a schema that does not exist", n)
	}
	if sizes["web"] > st.DatabaseBytes {
		t.Errorf("schema web's %d bytes are more than the database's %d", sizes["web"], st.DatabaseBytes)
	}
}
