package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"
)

// A query of information_schema while other connections create and drop
// databases on the same Dolt failed with "no root value found in
// session" within a few reads (docs/design/database.md). countTables
// reads only the current database, so none of these reads fails.
func TestCountTablesBesideCreateAndDrop(t *testing.T) {
	t.Parallel()
	srv := doltOf(t)
	dsn, err := srv.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.DropDatabase(dsn) })
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for ctx.Err() == nil {
				other, err := srv.NewDatabase(ctx)
				if err != nil {
					continue
				}
				_ = srv.DropDatabase(other)
			}
		})
	}
	defer func() { cancel(); wg.Wait() }()
	for i := range 100 {
		if _, err := countTables(t.Context(), db, "schema_migrations"); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
}
