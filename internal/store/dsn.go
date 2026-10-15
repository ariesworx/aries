package store

import (
	"fmt"

	"github.com/go-sql-driver/mysql"
)

// What ariesd needs to know of a data source name and of the database's
// accounts, which is the store's to know: the daemon derives each
// project's DSN from the one it is configured with, refuses a password
// where the process list would show it, and names the SQL that grants a
// database in its fixes.

// DSN is what ariesd reads of a data source name in the format [Open]
// takes.
type DSN struct {
	// User is the account it connects as; empty when it names none.
	User string
	// Database is the database it names; empty when it names none.
	Database string
	// Password reports whether it holds a password.
	Password bool
}

// ParseDSN parses dsn, in the format [Open] takes.
func ParseDSN(dsn string) (DSN, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return DSN{}, err
	}
	return DSN{User: cfg.User, Database: cfg.DBName, Password: cfg.Passwd != ""}, nil
}

// DatabaseDSN returns dsn with its database replaced by name: another
// database on the same server, as the same account. name must be a
// [ValidDBName].
func DatabaseDSN(dsn, name string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	if !ValidDBName(name) {
		return "", fmt.Errorf("database name %q must be a lowercase letter, then lowercase letters, digits and underscores, at most 64", name)
	}
	cfg.DBName = name
	return cfg.FormatDSN(), nil
}

// defaultGrantee is the account ariesd's guides create, as a GRANT
// statement names it.
const defaultGrantee = "'aries'@'localhost'"

// GrantSQL returns the statement the database's root runs to let
// account use database db: what a fix names when the account was never
// granted it. account is as [Control.Account] gives it; empty is the
// account the guides create.
func GrantSQL(db, account string) string {
	if account == "" {
		account = defaultGrantee
	}
	return fmt.Sprintf("GRANT ALL ON %s.* TO %s;", db, account)
}

// CreateDatabaseSQL returns the statements the database's root runs to
// create database db and grant it to the user dsn connects as, or to the
// guides' user when dsn names none: what a fix names when a control
// database will not open.
func CreateDatabaseSQL(db, dsn string) string {
	user := "aries"
	if d, err := ParseDSN(dsn); err == nil && d.User != "" {
		user = d.User
	}
	return fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s; GRANT ALL ON %s.* TO '%s'@'localhost';", db, db, user)
}
