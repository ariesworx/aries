package doltmetrics

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
)

// A real Dolt, with its metrics endpoint on and a Go memory limit: every
// figure is read, and says what the process says of itself.
func TestReadRealDolt(t *testing.T) {
	const limit = 3 << 30
	before := time.Now().Add(-2 * time.Second)
	srv, err := dolttest.StartWith(t.Context(), t.TempDir(), dolttest.Options{Metrics: true, Env: []string{"GOMEMLIMIT=3GiB"}})
	if errors.Is(err, dolttest.ErrNoDolt) {
		t.Skip("dolt not installed")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(); err != nil {
			t.Error(err)
		}
	})
	version, err := dolttest.Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// One connection held open, to be counted.
	dsn, err := srv.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	r, err := New(t.Context(), srv.MetricsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.Read(t.Context())
	if err != nil {
		t.Fatalf("Read(%s) = %v", srv.MetricsURL(), err)
	}
	if got.Version != version {
		t.Errorf("Version = %q, want %q, the dolt on PATH", got.Version, version)
	}
	// An empty Dolt holds some tens of mebibytes.
	if got.Resident == nil || *got.Resident < 10<<20 || *got.Resident > 2<<30 {
		t.Errorf("Resident = %s, want between 10 MiB and 2 GiB", show(got))
	}
	if got.MemLimit == nil || *got.MemLimit != limit {
		t.Errorf("MemLimit = %s, want %d", show(got), limit)
	}
	if got.Started == nil || got.Started.Before(before) || got.Started.After(time.Now().Add(2*time.Second)) {
		t.Errorf("Started = %s, want between %s and now", show(got), before.UTC().Format(time.RFC3339))
	}
	if got.Conns == nil || *got.Conns != 1 {
		t.Errorf("Conns = %s, want 1", show(got))
	}
	if got.GCCycles == nil {
		t.Errorf("GCCycles is missing: %s", show(got))
	}
}
