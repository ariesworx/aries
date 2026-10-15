package store

// Spike only (spike/postgres): what lets this copy of the store run on
// Postgres as well as on Dolt. A connection string that starts with
// postgres:// opens the store through pgx, behind a driver.Connector that
// rewrites each statement from the store's MySQL dialect as it is sent:
//
//   - `?` placeholders become $1, $2, ...;
//   - INSERT IGNORE INTO becomes INSERT INTO ... ON CONFLICT DO NOTHING;
//   - JSON_UNQUOTE(JSON_EXTRACT(x, '$.k')) becomes (x->>'k');
//   - DATABASE() becomes current_schema(), for information_schema;
//   - CAST(x AS CHAR(n)) becomes CAST(x AS VARCHAR(n));
//   - the CASE that refreshes issue_paths.last_at casts its times;
//   - `quoted` identifiers become "quoted".
//
// A real port would change the SQL where it is written instead; the
// rewrite is here so that one copy of the store, with its tests, runs on
// both backends and shows what differs. RESULTS.md counts the call sites.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// isPostgres reports whether dsn names a Postgres database.
func isPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// pgConnector opens dsn through pgx with the session settings the store
// assumes: UTC, and snapshot reads and writes, as Dolt's repeatable read.
func pgConnector(dsn string) (driver.Connector, string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, "", fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.Database == "" {
		return nil, "", fmt.Errorf("%w: dsn names no database", ErrInvalid)
	}
	cfg.RuntimeParams["timezone"] = "UTC"
	if _, ok := cfg.RuntimeParams["default_transaction_isolation"]; !ok {
		cfg.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	}
	where := fmt.Sprintf("%s:%d/%s", cfg.Host, cfg.Port, cfg.Database)
	if sp := cfg.RuntimeParams["search_path"]; sp != "" {
		where += "." + sp
	}
	return pgRewriter{stdlib.GetConnector(*cfg)}, where, nil
}

type pgRewriter struct{ base driver.Connector }

func (c pgRewriter) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pgConn{conn.(*stdlib.Conn)}, nil
}

func (c pgRewriter) Driver() driver.Driver { return c.base.Driver() }

// pgConn forwards to pgx's connection every interface database/sql looks
// for, rewriting the statement on the way.
type pgConn struct{ c *stdlib.Conn }

var (
	_ driver.ConnPrepareContext = (*pgConn)(nil)
	_ driver.ExecerContext      = (*pgConn)(nil)
	_ driver.QueryerContext     = (*pgConn)(nil)
	_ driver.ConnBeginTx        = (*pgConn)(nil)
	_ driver.Pinger             = (*pgConn)(nil)
	_ driver.NamedValueChecker  = (*pgConn)(nil)
	_ driver.SessionResetter    = (*pgConn)(nil)
)

func (p *pgConn) Prepare(q string) (driver.Stmt, error) { return p.c.Prepare(pgRewrite(q)) }
func (p *pgConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	return p.c.PrepareContext(ctx, pgRewrite(q))
}
func (p *pgConn) Close() error              { return p.c.Close() }
func (p *pgConn) Begin() (driver.Tx, error) { return p.c.Begin() } //nolint:staticcheck // driver.Conn requires it
func (p *pgConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return p.c.BeginTx(ctx, o)
}
func (p *pgConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	return p.c.ExecContext(ctx, pgRewrite(q), args)
}
func (p *pgConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return p.c.QueryContext(ctx, pgRewrite(q), args)
}
func (p *pgConn) Ping(ctx context.Context) error             { return p.c.Ping(ctx) }
func (p *pgConn) CheckNamedValue(v *driver.NamedValue) error { return p.c.CheckNamedValue(v) }
func (p *pgConn) ResetSession(ctx context.Context) error     { return p.c.ResetSession(ctx) }

var (
	rewrites   sync.Map // MySQL statement -> Postgres statement
	jsonGet    = regexp.MustCompile(`JSON_UNQUOTE\(JSON_EXTRACT\(([\w.]+), '\$\.(\w+)'\)\)`)
	insertIgn  = regexp.MustCompile(`^(\s*)INSERT IGNORE INTO`)
	databaseFn = regexp.MustCompile(`\bDATABASE\(\)`)
	castChar   = regexp.MustCompile(`\bAS CHAR\((\d+)\)`)
	thenTime   = regexp.MustCompile(`THEN \?`)
	pathsCase  = regexp.MustCompile(`^UPDATE\s+issue_paths SET last_at = CASE`)
)

// pgRewrite turns one statement of the store's MySQL dialect into
// Postgres's (see the file's comment).
func pgRewrite(q string) string {
	if v, ok := rewrites.Load(q); ok {
		return v.(string)
	}
	out := q
	if insertIgn.MatchString(out) {
		out = insertIgn.ReplaceAllString(out, "${1}INSERT INTO") + " ON CONFLICT DO NOTHING"
	}
	out = jsonGet.ReplaceAllString(out, "($1->>'$2')")
	out = databaseFn.ReplaceAllString(out, "current_schema()")
	// A CHAR cast pads in Postgres, and a recursive CTE's two terms then
	// differ in type; MySQL has no CAST AS VARCHAR.
	out = castChar.ReplaceAllString(out, "AS VARCHAR($1)")
	// Postgres types a CASE's THEN placeholders as text; this one sets a time.
	if pathsCase.MatchString(out) {
		out = thenTime.ReplaceAllString(out, "THEN CAST(? AS TIMESTAMP(6))")
	}
	out = rebind(out)
	rewrites.Store(q, out)
	return out
}

// rebind numbers the placeholders and turns backquotes into double
// quotes, outside string literals and comments.
func rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 16)
	n := 0
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			j := i + 1
			for j < len(q) {
				if q[j] == '\'' {
					if j+1 < len(q) && q[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			b.WriteString(q[i:min(j+1, len(q))])
			i = j
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			j := strings.IndexByte(q[i:], '\n')
			if j < 0 {
				j = len(q) - i
			}
			b.WriteString(q[i : i+j])
			i += j - 1
		case c == '`':
			b.WriteByte('"')
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// pgCode returns the SQLSTATE of a Postgres error, or "".
func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// checkPGAccount is checkAccount's counterpart: the role must not be a
// superuser, create roles or create databases.
func checkPGAccount(ctx context.Context, q querier) error {
	var user string
	var super, createRole, createDB bool
	if err := q.QueryRowContext(ctx, `SELECT rolname, rolsuper, rolcreaterole, rolcreatedb FROM pg_roles WHERE rolname = current_user`).
		Scan(&user, &super, &createRole, &createDB); err != nil {
		return fmt.Errorf("check account: %w", err)
	}
	var problems []string
	if super {
		problems = append(problems, "it is a superuser")
	}
	if createRole {
		problems = append(problems, "it can create roles")
	}
	if createDB {
		problems = append(problems, "it can create databases")
	}
	if len(problems) > 0 {
		return &UnsafeAccountError{User: user, Problems: problems}
	}
	return nil
}
