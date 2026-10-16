package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// A server's connections to Postgres (decision 79, slice P2): one pool
// for its one database, which every store opened from it shares, each
// in a schema of its own. A store does not hold a connection: its writer
// and its readers take one from the pool for each transaction, or each
// read outside one, and give it back. So a project costs the database
// nothing while it is idle, and an open store costs its share of the
// pool while it works.
//
// Each connection's search_path is set to the schema of the store that
// takes it, and left so: a store that takes back a connection it had
// last sets nothing. A statement Postgres prepared under one schema is
// planned again when it runs under another (PREPARE's documentation),
// and the schemas of one binary have the same tables, so a statement's
// result has the same columns in any of them.

// DefaultConns is the most connections a [DB] holds by default.
const DefaultConns = 32

// DBOptions configure a [DB]. The zero value is usable.
type DBOptions struct {
	// Conns caps the pool: the connections to Postgres that every store
	// opened from the DB shares, for its writes and its reads. Default
	// [DefaultConns]. The DB holds one more, which holds the writers'
	// locks (fence.go).
	Conns int
}

// DB is a server's pool of connections to the one Postgres database its
// DSN names, from which it opens the store of each schema in it
// ([DB.Open], [DB.OpenControl]). It is safe for concurrent use. Close it
// once every store opened from it is closed.
type DB struct {
	pool *pgxpool.Pool
	conn driver.Connector
	dsn  DSN
	// addr names the database to the clock: its server's address and
	// its name.
	addr string
	// fence holds the writers' locks of the schemas the DB's stores
	// write (fence.go).
	fence *fence
}

// OpenDB connects to the database dsn names (a postgres:// URL, dsn.go),
// whose search_path it ignores: each store names its own schema. ctx
// bounds only the first connection, which checks that the database
// answers. The caller must Close the DB.
//
// It refuses, with [ErrInvalid], a dsn that is not a postgres:// URL or
// names no database, and a Conns below zero.
func OpenDB(ctx context.Context, dsn string, opts DBOptions) (*DB, error) {
	d, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	if d.Database == "" {
		return nil, fmt.Errorf("%w: dsn names no database", ErrInvalid)
	}
	if opts.Conns < 0 {
		return nil, fmt.Errorf("%w: a pool of %d connections", ErrInvalid, opts.Conns)
	}
	if opts.Conns == 0 {
		opts.Conns = DefaultConns
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx quotes the DSN, which may hold a password.
		return nil, fmt.Errorf("%w: the dsn does not parse as a postgres:// URL", ErrInvalid)
	}
	// Every session works in UTC, and every transaction sees one
	// snapshot (doc.go, Concurrency). The schema is the store's to set.
	params := cfg.ConnConfig.RuntimeParams
	delete(params, "search_path")
	params["timezone"] = "UTC"
	params["default_transaction_isolation"] = "repeatable read"
	cfg.MaxConns = int32(min(opts.Conns, 1<<20)) //nolint:gosec // bounded just above
	cfg.MinConns = 0
	// pgxpool pings only a connection idle for more than a second; this
	// drops one Postgres ended since, without a round trip (idle.go).
	cfg.PrepareConn = usable
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: the dsn does not parse as a postgres:// URL", ErrInvalid)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database %s: %w", d.Database, err)
	}
	c := cfg.ConnConfig
	return &DB{pool: pool, conn: stdlib.GetPoolConnector(pool), dsn: d,
		addr:  fmt.Sprintf("%s:%d/%s", c.Host, c.Port, d.Database),
		fence: newFence(c.Copy())}, nil
}

// Close closes the pool and gives up every writer's lock the DB holds.
// Every store opened from it must be closed first.
func (db *DB) Close() error {
	err := db.fence.close()
	db.pool.Close()
	return err
}

// Database returns the name of the database the DB is connected to.
func (db *DB) Database() string { return db.dsn.Database }

// sqlDB returns a database/sql pool over db's connections for schema,
// opening no connection of its own: each it takes is the pool's, with
// its search_path set to schema, and goes back to the pool when done.
// It holds at most conns at once.
func (db *DB) sqlDB(schema string, conns int) *sql.DB {
	s := sql.OpenDB(schemaConnector{inner: db.conn, schema: schema})
	s.SetMaxOpenConns(conns)
	// Idle connections belong to the pool, where every schema may take
	// them.
	s.SetMaxIdleConns(0)
	return s
}

// schemaConnector is a connector for database/sql over a DB's pool
// whose connections work in one schema.
type schemaConnector struct {
	inner  driver.Connector
	schema string
}

// schemaKey is where a connection's CustomData records the schema its
// search_path names.
const schemaKey = "aries.schema"

// Connect takes a connection from the pool and sets its search_path to
// the connector's schema, unless the connection works there already.
func (c schemaConnector) Connect(ctx context.Context) (driver.Conn, error) {
	dc, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	sc, ok := dc.(*stdlib.Conn)
	if !ok {
		return nil, errors.Join(fmt.Errorf("store: the pool gave a %T, not a pgx connection", dc), dc.Close())
	}
	pc := sc.Conn().PgConn()
	if pc.CustomData()[schemaKey] == c.schema {
		return dc, nil
	}
	// Outside a transaction, so that it lasts: the setting is the
	// session's.
	if _, err := sc.Conn().Exec(ctx, "SET search_path TO "+quoteIdent(c.schema)); err != nil {
		return nil, errors.Join(fmt.Errorf("set the search path to %s: %w", c.schema, err), dc.Close())
	}
	pc.CustomData()[schemaKey] = c.schema
	return dc, nil
}

// Driver returns the pgx driver.
func (c schemaConnector) Driver() driver.Driver { return c.inner.Driver() }
