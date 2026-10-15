package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/ariesworx/aries/internal/version"
)

// What Open checks of the Postgres server and of the schema before it
// migrates: that the server is one Aries supports, and that the schema
// the DSN names exists and is the role's to use.

// ServerVersionError refuses a Postgres server older than the oldest
// major version Aries supports. Version is what the server calls itself.
// Its message ends with the fix.
type ServerVersionError struct {
	Version string
}

func (e *ServerVersionError) Error() string {
	return fmt.Sprintf("the database server is Postgres %s, older than %d, the oldest ariesd supports; fix: upgrade it to Postgres %d or later (Aries is tested with %s)",
		e.Version, version.PostgresMin, version.PostgresMin, version.Postgres)
}

// checkServer refuses a server older than version.PostgresMin.
func checkServer(ctx context.Context, q querier) error {
	var num int
	var name string
	if err := q.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int, current_setting('server_version')`).
		Scan(&num, &name); err != nil {
		return fmt.Errorf("read the server's version: %w", err)
	}
	return serverVersion(num, name)
}

// serverVersion refuses a server whose server_version_num is num (major
// times 10,000, plus the minor) when it is older than
// version.PostgresMin. name is its server_version, for the refusal.
func serverVersion(num int, name string) error {
	if num < version.PostgresMin*10000 {
		return &ServerVersionError{Version: name}
	}
	return nil
}

// errDenied marks a schema the role may not use, or not make.
var errDenied = errors.New("permission denied")

// DatabaseDenied reports whether err is Postgres refusing the role a
// project's schema: one another role made and did not give it, or, when
// creating one ([Control.CreateDatabase]), a database in which the role
// may not make schemas.
func DatabaseDenied(err error) bool {
	return errors.Is(err, errDenied) || sqlState(err) == codePrivilege
}

// checkSchema checks that the schema d names exists and that the
// connected role may use it and make tables in it, as migrating needs.
// With create, it makes a schema that does not exist instead of refusing
// it.
func checkSchema(ctx context.Context, q querier, d DSN, create bool) error {
	var exists, usable bool
	if err := q.QueryRowContext(ctx, `SELECT n.oid IS NOT NULL,
  COALESCE(has_schema_privilege(n.oid, 'USAGE') AND has_schema_privilege(n.oid, 'CREATE'), false)
FROM (SELECT $1::text AS name) s LEFT JOIN pg_namespace n ON n.nspname = s.name`, d.Schema).Scan(&exists, &usable); err != nil {
		return fmt.Errorf("read schema %s: %w", d.Schema, err)
	}
	switch {
	case !exists && create:
		if _, err := q.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+quoteIdent(d.Schema)); err != nil {
			return fmt.Errorf("create schema %s: %w", d.Schema, err)
		}
	case !exists:
		return fmt.Errorf("%w: schema %s does not exist in database %s", ErrInvalid, d.Schema, d.Database)
	case !usable:
		return fmt.Errorf("%w: role %s may not use schema %s in database %s; fix: as its owner or a superuser, run %s",
			errDenied, d.User, d.Schema, d.Database, GrantSQL(d.Schema, d.User))
	}
	return nil
}
