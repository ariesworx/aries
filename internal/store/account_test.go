package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/pgtest"
)

func TestAccountProblems(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		r    roleRights
		want []string // substrings, one per problem
	}{
		{name: "least privilege", r: roleRights{name: "aries"}},
		{name: "superuser", r: roleRights{name: "postgres", superuser: true, createRole: true, createDB: true, members: []string{"pg_read_server_files"}},
			want: []string{"superuser"}},
		{name: "createrole", r: roleRights{name: "aries", createRole: true}, want: []string{"CREATEROLE"}},
		{name: "createdb", r: roleRights{name: "aries", createDB: true}, want: []string{"CREATEDB"}},
		{name: "replication", r: roleRights{name: "aries", replication: true}, want: []string{"REPLICATION"}},
		{name: "bypassrls", r: roleRights{name: "aries", bypassRLS: true}, want: []string{"BYPASSRLS"}},
		{name: "file access", r: roleRights{name: "aries", members: []string{"pg_read_server_files", "pg_write_server_files"}},
			want: []string{"pg_read_server_files, pg_write_server_files"}},
		{name: "several", r: roleRights{name: "aries", createDB: true, members: []string{"pg_read_all_data"}},
			want: []string{"CREATEDB", "pg_read_all_data"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := accountProblems(tc.r)
			if len(got) != len(tc.want) {
				t.Fatalf("accountProblems(%+v) = %q, want %d problems", tc.r, got, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("accountProblems(%+v) problem %d = %q, want it to mention %q", tc.r, i, got[i], w)
				}
			}
		})
	}
}

// asRole returns dsn with its role replaced by role.
func asRole(t *testing.T, dsn, role string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.User(role)
	return u.String()
}

// superuserDSN is dsn's database reached as the server's superuser.
func superuserDSN(t *testing.T, dsn string) string {
	t.Helper()
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return pgOf(t).RootDSN(d.Database)
}

// S-2: a store opened as a superuser, or as a role with a right that
// reaches beyond its database, which could rewrite the event log behind
// ariesd's back or read and write the server's files, is refused unless
// the caller explicitly allows it (ariesd allows it only with --dev).
func TestOpenRefusesUnsafeAccount(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	root, err := sql.Open("pgx", superuserDSN(t, dsn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	d, err := ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		role string
		make []string // run as the superuser first; the pool drops the roles
		want string   // in the problems
	}{
		{name: "superuser", role: "postgres", want: "superuser"},
		{name: "createdb", role: "can_createdb", make: []string{"CREATE ROLE can_createdb LOGIN CREATEDB"}, want: "CREATEDB"},
		{name: "createrole", role: "can_createrole", make: []string{"CREATE ROLE can_createrole LOGIN CREATEROLE"}, want: "CREATEROLE"},
		{name: "file access through another role", role: "reads_files", make: []string{
			"CREATE ROLE file_readers", "GRANT pg_read_server_files TO file_readers",
			"CREATE ROLE reads_files LOGIN IN ROLE file_readers"}, want: "pg_read_server_files"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, q := range append(tc.make, fmt.Sprintf("GRANT ALL ON SCHEMA public TO %s", tc.role)) {
				if _, err := root.ExecContext(t.Context(), q); err != nil {
					t.Fatalf("%s: %v", q, err)
				}
			}
			role := asRole(t, dsn, tc.role)
			_, err := Open(t.Context(), role, Options{})
			ua, ok := errors.AsType[*UnsafeAccountError](err)
			if !ok {
				t.Fatalf("Open as %s = %v, want an *UnsafeAccountError", tc.role, err)
			}
			if ua.User != tc.role || !strings.Contains(strings.Join(ua.Problems, "; "), tc.want) {
				t.Errorf("Open as %s: UnsafeAccountError = %+v, want role %s with %q among the problems", tc.role, ua, tc.role, tc.want)
			}
			for _, w := range []string{"CREATE ROLE aries LOGIN", "ALTER ROLE", "--allow-unsafe-account"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("Open as %s: error %q does not name %q", tc.role, err, w)
				}
			}
		})
	}
	// Allowed, the superuser's store opens, on a database of its own.
	s, err := Open(t.Context(), superuserDSN(t, newDSN(t)), Options{AllowUnsafeAccount: true})
	if err != nil {
		t.Fatalf("Open as the superuser with AllowUnsafeAccount: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The locked-down role pgtest hands out opens: the refusals above
	// changed nothing in its database.
	if d.User != pgtest.User {
		t.Fatalf("newDSN names role %q, want %s", d.User, pgtest.User)
	}
	openStore(t, dsn, Options{})
}
