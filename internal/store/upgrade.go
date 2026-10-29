package store

import (
	"context"
	"fmt"
	"net/url"
)

// What `ariesd upgrade` needs of the store before it installs a release:
// the migrations a binary carries, how far each schema has run, and how
// to hand the database to pg_dump.

// Migrations returns the names (NNNN_name) of this binary's migrations
// of a project's schema, or with control those of the control store's,
// in order. The release that carries them prints them with `ariesd
// migrations`, which an older binary reads to say what an upgrade will
// run.
func Migrations(control bool) ([]string, error) {
	sc := projectSchema
	if control {
		sc = controlSchema
	}
	ms, err := loadMigrations(sc.fsys, sc.dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.name
	}
	return names, nil
}

// Migrated returns the version the schema has run to: of a project's
// migrations, or with control of the control store's. A schema none has
// run in is at 0. It reads and changes nothing else, takes no writer's
// lock, and so runs beside the daemon that serves the schema.
func (db *DB) Migrated(ctx context.Context, schema string, control bool) (int, error) {
	sc := projectSchema
	if control {
		sc = controlSchema
	}
	sdb := db.sqlDB(schema, 1)
	defer func() { _ = sdb.Close() }()
	n, err := countTables(ctx, sdb, sc.table)
	if err != nil {
		return 0, fmt.Errorf("schema %s: read tables: %w", schema, err)
	}
	if n == 0 {
		return 0, nil
	}
	v, err := lastKey(ctx, sdb, `SELECT version FROM `+sc.table+` ORDER BY version DESC LIMIT 1`) //nolint:gosec // sc.table is a constant
	if err != nil {
		return 0, fmt.Errorf("schema %s: read schema version: %w", schema, err)
	}
	return int(v), nil
}

// ServerMajor returns the major version of the Postgres db is connected
// to, such as 18: a pg_dump must be at least as new.
func (db *DB) ServerMajor(ctx context.Context) (int, error) {
	var num int
	if err := db.pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&num); err != nil {
		return 0, fmt.Errorf("read the server's version: %w", err)
	}
	return num / 10000, nil
}

// libpqParam reports whether libpq, and so pg_dump, accepts the
// parameter k in a connection URI. pgx accepts others (search_path among
// them), which libpq refuses. A switch, not a map: a package-level map
// is built at every start of ariesd, which sshd runs for each connection.
func libpqParam(k string) bool {
	switch k {
	case "host", "hostaddr", "port", "dbname", "user",
		"passfile", "channel_binding", "connect_timeout",
		"client_encoding", "options", "application_name",
		"fallback_application_name", "keepalives", "keepalives_idle",
		"keepalives_interval", "keepalives_count", "tcp_user_timeout",
		"sslmode", "sslnegotiation", "sslcompression", "sslcert",
		"sslkey", "sslcertmode", "sslrootcert", "sslcrl",
		"sslcrldir", "sslsni", "requirepeer",
		"ssl_min_protocol_version", "ssl_max_protocol_version",
		"krbsrvname", "gsslib", "gssdelegation", "gssencmode",
		"service", "target_session_attrs", "load_balance_hosts",
		"require_auth", "min_protocol_version", "max_protocol_version":
		return true
	}
	return false
}

// DumpTarget splits dsn into what pg_dump is given: a connection URI to
// put on its command line, which holds no password, and the password
// the dsn held, if any, for PGPASSWORD in its environment. The URI
// keeps the role, the host, the database and libpq's own parameters,
// and drops the search_path and any other parameter libpq would refuse:
// a dump is of the whole database. sslpassword, which no variable can
// carry, is dropped too; a key that needs one is the operator's to dump
// with by hand.
func DumpTarget(dsn string) (uri, password string, err error) {
	u, err := parseURL(dsn)
	if err != nil {
		return "", "", err
	}
	if u.User != nil {
		password, _ = u.User.Password()
		u.User = url.User(u.User.Username())
		if u.User.Username() == "" {
			u.User = nil
		}
	}
	q := u.Query()
	if p := q.Get("password"); p != "" {
		password = p
	}
	out := url.Values{}
	for k, v := range q {
		if libpqParam(k) {
			out[k] = v
		}
	}
	u.RawQuery = out.Encode()
	return u.String(), password, nil
}
