package main

import (
	"database/sql"
	"io"
	"testing"
)

// The clock check before an upgrade runs while the old daemon serves:
// it migrates nothing, and passes over a control store this binary
// would have to migrate before it could read it, which the restarted
// daemon migrates and checks itself.
func TestUpgradeClockCheckMigratesNothing(t *testing.T) {
	t.Parallel()
	s := newSite(t, "")
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", writeKeys(t)); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	db, err := sql.Open("pgx", s.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	versions := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM aries_control.control_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// One migration behind this binary, as an older ariesd left it.
	if _, err := db.ExecContext(t.Context(), `DELETE FROM aries_control.control_migrations
  WHERE version = (SELECT MAX(version) FROM aries_control.control_migrations)`); err != nil {
		t.Fatal(err)
	}
	before := versions()
	env := adminEnv{stdout: io.Discard, stderr: io.Discard, getenv: func(k string) string { return s.env[k] }}
	if err := clockPreflight(t.Context(), env, s.settings(t), s.cfg); err != nil {
		t.Errorf("the clock check with the control store one migration behind = %v, want it passed over", err)
	}
	if after := versions(); after != before {
		t.Errorf("the clock check took the control store from %d migrations to %d; want it untouched", before, after)
	}
}
