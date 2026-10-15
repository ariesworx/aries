package store

import (
	"context"
	"fmt"
	"strings"
)

// The account check (S-2). ariesd runs as a role of its own that may use
// its database and nothing beyond it. A superuser, or a role that can
// make roles or databases, replicate, skip row security, or read and
// write the server's files and every table, could skip ariesd: rewrite
// the event log as anyone, read another database, or run a program as
// the server's user. Open refuses such a role unless
// Options.AllowUnsafeAccount says otherwise.

// UnsafeAccountError refuses a database role with more rights than Aries
// needs. User is the role, and Problems lists what is unsafe, in words.
// Its message ends with the fix.
type UnsafeAccountError struct {
	User     string
	Problems []string
}

func (e *UnsafeAccountError) Error() string {
	role := quoteIdent(e.User)
	return fmt.Sprintf("postgres role %s is unsafe for ariesd: %s; fix: as a superuser, create a role of its own and a database it owns, "+
		"CREATE ROLE aries LOGIN; CREATE DATABASE aries OWNER aries; and put that role in the DSN, or take the rights away: "+
		"ALTER ROLE %s NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS; and REVOKE each role named above FROM %s; "+
		"for local development only, pass --dev --allow-unsafe-account",
		e.User, strings.Join(e.Problems, "; "), role, role)
}

// riskyRoles are the predefined roles whose members reach beyond their
// own database: the server's files and programs, and every table of
// every database.
var riskyRoles = []string{"pg_execute_server_program", "pg_read_all_data", "pg_read_server_files", "pg_write_all_data", "pg_write_server_files"}

// roleRights is what checkAccount reads of the connected role from
// pg_roles: its attributes, and which of riskyRoles it is a member of,
// directly or through another role.
type roleRights struct {
	name                                                    string
	superuser, createRole, createDB, replication, bypassRLS bool
	members                                                 []string
}

// checkAccount refuses the connected role when it holds any right beyond
// its own database (accountProblems).
func checkAccount(ctx context.Context, q querier) error {
	var r roleRights
	var members string
	if err := q.QueryRowContext(ctx, `SELECT r.rolname, r.rolsuper, r.rolcreaterole, r.rolcreatedb, r.rolreplication, r.rolbypassrls,
  array_to_string(ARRAY(SELECT b.rolname FROM pg_roles b
    WHERE b.rolname = ANY(string_to_array($1, ',')) AND pg_has_role(r.oid, b.oid, 'MEMBER') ORDER BY b.rolname), ',')
FROM pg_roles r WHERE r.rolname = current_user`, strings.Join(riskyRoles, ",")).
		Scan(&r.name, &r.superuser, &r.createRole, &r.createDB, &r.replication, &r.bypassRLS, &members); err != nil {
		return fmt.Errorf("account check: %w", err)
	}
	if members != "" {
		r.members = strings.Split(members, ",")
	}
	if p := accountProblems(r); len(p) > 0 {
		return &UnsafeAccountError{User: r.name, Problems: p}
	}
	return nil
}

// accountProblems lists what is unsafe about a role with rights r. A
// superuser is a member of every role, which says nothing more, so its
// memberships are not listed.
func accountProblems(r roleRights) []string {
	var out []string
	if r.superuser {
		return append(out, "it is a superuser")
	}
	for _, a := range []struct {
		has  bool
		what string
	}{
		{r.createRole, "it can create roles (CREATEROLE)"},
		{r.createDB, "it can create databases (CREATEDB)"},
		{r.replication, "it can replicate (REPLICATION)"},
		{r.bypassRLS, "it bypasses row security (BYPASSRLS)"},
	} {
		if a.has {
			out = append(out, a.what)
		}
	}
	if len(r.members) > 0 {
		out = append(out, "it is a member of "+strings.Join(r.members, ", "))
	}
	return out
}
