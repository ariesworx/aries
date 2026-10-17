package server

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ariesworx/aries/internal/store"
)

// checkDatabaseFiles refuses a file holding a secret of the database's
// that pgx would read for dsn and that group or others may read: the
// password file, unless a password is given otherwise, and the key of
// the client certificate. Each is named as pgx finds it: by the DSN's
// parameter, else the environment's variable, else at its place in the
// home directory, $HOME. libpq refuses either file when it is loose;
// pgx reads it whatever its mode, so ariesd refuses it here, as it does
// a config file that holds a password. A file that is not there is not
// refused: pgx reads none. Modes are not checked on Windows.
func checkDatabaseFiles(dsn string, getenv func(string) string) error {
	if dsn == "" || runtime.GOOS == "windows" {
		return nil
	}
	d, err := store.ParseDSN(dsn)
	if err != nil {
		// Refused, with its cause, where the DSN is opened.
		return nil
	}
	home := getenv("HOME")
	inHome := func(parts ...string) string {
		if home == "" {
			return ""
		}
		return filepath.Join(append([]string{home}, parts...)...)
	}
	if !d.Password && getenv("PGPASSWORD") == "" {
		if err := privateFile("passfile", "the database password", cmp.Or(d.Passfile, getenv("PGPASSFILE"), inHome(".pgpass"))); err != nil {
			return err
		}
	}
	key := cmp.Or(d.SSLKey, getenv("PGSSLKEY"))
	if key == "" && home != "" {
		// pgx takes the key at its default place only beside its
		// certificate.
		if _, err := os.Stat(inHome(".postgresql", "postgresql.crt")); err == nil {
			key = inHome(".postgresql", "postgresql.key")
		}
	}
	return privateFile("sslkey", "the key of the database's client certificate", key)
}

// privateFile refuses path, the file of setting, which holds what,
// when group or others have any access to it. An empty path, or a file
// that is not there, is not refused.
func privateFile(setting, what, path string) error {
	if path == "" {
		return nil
	}
	fi, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("dsn: %s: %w", setting, err)
	case fi.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("dsn: %s %s holds %s but is mode %04o, open to group or others; fix: chmod 600 %s", setting, path, what, fi.Mode().Perm(), path)
	}
	return nil
}
