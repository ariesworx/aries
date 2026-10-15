package store

import (
	"cmp"
	"fmt"
	"net/url"
	"strings"
)

// What ariesd needs to know of a data source name and of the database's
// roles, which is the store's to know: the daemon derives each project's
// DSN from the one it is configured with, refuses a password where the
// process list would show it, and names the SQL that lets its role use a
// schema in its fixes.
//
// A DSN is a postgres:// (or postgresql://) URL, as libpq and pgx read
// it: postgres://ROLE@HOST:PORT/DATABASE?search_path=SCHEMA, where HOST
// may instead be a host= parameter naming the directory of a unix
// socket. The store's tables live in the schema search_path names, or in
// public when it names none: a project in a schema of its own, and the
// control store in another, all in one database (decision 79). Anything
// else in the URL, such as sslmode or a password, is pgx's to read; pgx
// also reads the PG* environment variables and the role's .pgpass, which
// is where a password belongs.

// DefaultSchema is the schema a DSN without a search_path selects.
const DefaultSchema = "public"

// DSN is what ariesd reads of a data source name in the format [Open]
// takes.
type DSN struct {
	// User is the role it connects as; empty when it names none.
	User string
	// Database is the database it names; empty when it names none.
	Database string
	// Schema is the schema its search_path names first, or
	// [DefaultSchema] when it names none.
	Schema string
	// Password reports whether it holds a password, in the URL's user
	// information or as a password parameter.
	Password bool
}

// ParseDSN parses dsn, in the format [Open] takes. It reads only the URL:
// a password in the environment or in .pgpass is not the DSN's.
func ParseDSN(dsn string) (DSN, error) {
	u, err := parseURL(dsn)
	if err != nil {
		return DSN{}, err
	}
	d := DSN{Database: strings.TrimPrefix(u.Path, "/"), Schema: DefaultSchema}
	if u.User != nil {
		d.User = u.User.Username()
		_, d.Password = u.User.Password()
	}
	// A parameter wins over the part of the URL it repeats, as pgx reads
	// them.
	q := u.Query()
	d.User = cmp.Or(q.Get("user"), d.User)
	d.Database = cmp.Or(q.Get("dbname"), d.Database)
	d.Password = d.Password || q.Has("password")
	if sp := q.Get("search_path"); sp != "" {
		first, _, _ := strings.Cut(sp, ",")
		d.Schema = strings.Trim(strings.TrimSpace(first), `"`)
	}
	return d, nil
}

// parseURL parses dsn as a postgres:// URL, refusing any other form.
func parseURL(dsn string) (*url.URL, error) {
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		// The DSN may hold a password: it is not quoted back.
		return nil, fmt.Errorf("%w: the dsn is not a postgres:// URL (postgres://ROLE@HOST/DATABASE)", ErrInvalid)
	}
	return u, nil
}

// SchemaDSN returns dsn with its search_path replaced by name: another
// schema in the same database, as the same role. A server of many
// projects reaches its control store and each project's this way, from
// the one dsn it is configured with. name must be a [ValidSchemaName].
func SchemaDSN(dsn, name string) (string, error) {
	if !ValidSchemaName(name) {
		return "", fmt.Errorf("schema name %q must be a lowercase letter, then lowercase letters, digits and underscores, at most 64", name)
	}
	u, err := parseURL(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("search_path", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// defaultRole is the role ariesd's guides create.
const defaultRole = "aries"

// quoteIdent quotes name as an SQL identifier.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// GrantSQL returns the statement that gives role the schema it was
// refused, which another role made: what a fix names when ariesd's role
// cannot use a project's schema ([SchemaDenied]). role is as
// [Control.Account] gives it; empty is the role the guides create.
func GrantSQL(schema, role string) string {
	if role == "" {
		role = defaultRole
	}
	return fmt.Sprintf("ALTER SCHEMA %s OWNER TO %s;", quoteIdent(schema), quoteIdent(role))
}

// CreateSchemaGrantSQL returns the statement that lets role make schemas
// in database db: what a fix names when ariesd's role may not create a
// project's ([Control.CreateSchema]). role is as [Control.Account]
// gives it; empty is the role the guides create.
func CreateSchemaGrantSQL(db, role string) string {
	if role == "" {
		role = defaultRole
	}
	return fmt.Sprintf("GRANT CREATE ON DATABASE %s TO %s;", quoteIdent(db), quoteIdent(role))
}

// CreateDatabaseSQL returns the statements a superuser runs to create
// the database dsn names, owned by the role dsn connects as (or the
// guides' role when dsn names none), which may then make its schemas:
// what a fix names when the control store will not open.
func CreateDatabaseSQL(dsn string) string {
	role, db := defaultRole, defaultRole
	if d, err := ParseDSN(dsn); err == nil {
		role, db = cmp.Or(d.User, role), cmp.Or(d.Database, db)
	}
	return fmt.Sprintf("CREATE DATABASE %s OWNER %s;", quoteIdent(db), quoteIdent(role))
}
