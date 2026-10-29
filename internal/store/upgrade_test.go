package store

import (
	"fmt"
	"strings"
	"testing"
)

func TestMigrationNames(t *testing.T) {
	t.Parallel()
	for _, control := range []bool{false, true} {
		names, err := Migrations(control)
		if err != nil {
			t.Fatal(err)
		}
		if len(names) == 0 || names[0] != "0001_init" {
			t.Fatalf("control %v: %v", control, names)
		}
		for i, n := range names {
			if !strings.HasPrefix(n, fmt.Sprintf("%04d_", i+1)) {
				t.Errorf("control %v: migration %d is %q", control, i+1, n)
			}
		}
	}
}

func TestMigrated(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	openStore(t, dsn, Options{})
	db := testDB(t, dsn)
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	names, err := Migrations(false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.Migrated(t.Context(), d.Schema, false)
	if err != nil || got != len(names) {
		t.Errorf("Migrated = %d, %v; want %d", got, err, len(names))
	}
	// The project's schema has never run the control store's migrations.
	if got, err := db.Migrated(t.Context(), d.Schema, true); err != nil || got != 0 {
		t.Errorf("Migrated(control) = %d, %v; want 0", got, err)
	}
	major, err := db.ServerMajor(t.Context())
	if err != nil || major < 16 {
		t.Errorf("ServerMajor = %d, %v", major, err)
	}
}

func TestDumpTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		dsn, uri, password string
	}{
		{"postgres://aries@/aries?host=/var/run/postgresql&search_path=project_web",
			"postgres://aries@/aries?host=%2Fvar%2Frun%2Fpostgresql", ""},
		{"postgres://aries:s3cret@db.example.com:5432/aries?sslmode=verify-full&sslrootcert=/etc/aries/postgres/ca.pem",
			"postgres://aries@db.example.com:5432/aries?sslmode=verify-full&sslrootcert=%2Fetc%2Faries%2Fpostgres%2Fca.pem", "s3cret"},
		{"postgresql://db.example.com/aries?user=aries&password=hunter2&passfile=/etc/aries/postgres/pgpass&pool_max_conns=4",
			"postgresql://db.example.com/aries?passfile=%2Fetc%2Faries%2Fpostgres%2Fpgpass&user=aries", "hunter2"},
	}
	for _, tt := range tests {
		uri, pw, err := DumpTarget(tt.dsn)
		if err != nil || uri != tt.uri || pw != tt.password {
			t.Errorf("DumpTarget(%q) = %q, %q, %v; want %q, %q", tt.dsn, uri, pw, err, tt.uri, tt.password)
		}
		if tt.password != "" && strings.Contains(uri, tt.password) {
			t.Errorf("the URI holds the password: %q", uri)
		}
	}
	if _, _, err := DumpTarget("host=/tmp dbname=aries"); err == nil || strings.Contains(err.Error(), "host=") {
		t.Errorf("a non-URL: %v", err)
	}
}
