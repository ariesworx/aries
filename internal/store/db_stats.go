package store

import (
	"context"
	"fmt"
)

// Postgres's own figures, for the status report (design §15.4): what it
// says of itself and of the DB's database, and what the DB's pool holds.
//
// Every figure is read from a view or function any role may read, with
// ariesd's role and nothing granted beyond its database: pg_stat_activity
// shows every session, though only its own role's queries, which nothing
// here reads; pg_stat_database every database's counters; and the size
// functions any relation's size. pg_monitor (pg_read_all_stats) would add
// other roles' queries and wait events, which the report does not show,
// so ariesd's role needs none of it.

// DBStats is Postgres's figures for a [DB]. Counters are since Postgres
// last reset its statistics, which a restart of it does not.
type DBStats struct {
	// Version is the server's version, as it names it.
	Version string
	// Pool is what the DB's own pool holds.
	Pool PoolStats
	// Conns counts the client sessions on the Postgres server, of every
	// database and role, which max_connections, ConnsMax, caps.
	// DatabaseConns counts those in the DB's database.
	Conns, ConnsMax, DatabaseConns int
	// Commits and Rollbacks count the database's transactions,
	// Deadlocks the deadlocks Postgres found in it, Conflicts the
	// queries a standby canceled (zero on a primary), and TempBytes what
	// its queries wrote to temporary files.
	Commits, Rollbacks, Deadlocks, Conflicts, TempBytes int64
	// DatabaseBytes is the database's size on disk.
	DatabaseBytes int64
}

// PoolStats is what a [DB]'s pool holds, from memory: Conns is how many
// connections, InUse how many of them a store is using, and Max the most
// it may hold (DBOptions.Conns). Waits counts the times a store waited
// for one because the pool held its most and every one was in use.
type PoolStats struct {
	Conns, InUse, Max int
	Waits             int64
}

// Pool returns what db's pool holds. It reads no database.
func (db *DB) Pool() PoolStats {
	ps := db.pool.Stat()
	// An acquire that found no idle connection either made one or, at the
	// pool's most, waited for one: pgxpool counts both as empty.
	return PoolStats{Conns: int(ps.TotalConns()), InUse: int(ps.AcquiredConns()), Max: int(ps.MaxConns()),
		Waits: max(0, ps.EmptyAcquireCount()-ps.NewConnsCount())}
}

// Stats reads Postgres's figures for db's database, in one query. When
// that fails, the pool's figures, which are the DB's own, are set all the
// same.
func (db *DB) Stats(ctx context.Context) (DBStats, error) {
	st := DBStats{Pool: db.Pool()}
	if err := db.pool.QueryRow(ctx, `SELECT current_setting('server_version'),
  (SELECT COUNT(*) FROM pg_stat_activity WHERE backend_type = 'client backend'),
  current_setting('max_connections')::int,
  d.numbackends, d.xact_commit, d.xact_rollback, d.deadlocks, d.conflicts, d.temp_bytes,
  pg_database_size(d.datid)
FROM pg_stat_database d WHERE d.datname = current_database()`).Scan(&st.Version, &st.Conns, &st.ConnsMax,
		&st.DatabaseConns, &st.Commits, &st.Rollbacks, &st.Deadlocks, &st.Conflicts, &st.TempBytes, &st.DatabaseBytes); err != nil {
		return DBStats{Pool: st.Pool}, fmt.Errorf("read Postgres's figures: %w", err)
	}
	return st, nil
}

// SchemaBytes returns the size on disk of each of schemas that exists,
// by name: its tables', their indexes' and their TOAST tables', in one
// query. A schema that does not exist is not in the map.
func (db *DB) SchemaBytes(ctx context.Context, schemas []string) (map[string]int64, error) {
	rows, err := db.pool.Query(ctx, `SELECT n.nspname, COALESCE(SUM(pg_total_relation_size(c.oid)), 0)::bigint
FROM pg_namespace n LEFT JOIN pg_class c ON c.relnamespace = n.oid AND c.relkind IN ('r', 'm')
WHERE n.nspname = ANY($1) GROUP BY n.nspname`, schemas)
	if err != nil {
		return nil, fmt.Errorf("read the schemas' sizes: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, fmt.Errorf("read the schemas' sizes: %w", err)
		}
		out[name] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the schemas' sizes: %w", err)
	}
	return out, nil
}
