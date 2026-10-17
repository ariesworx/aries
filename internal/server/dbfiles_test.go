package server

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A file Postgres's client reads a secret from, the password file or
// the client certificate's key, is refused at start when group or
// others may read it, with the fix chmod 600, wherever it is named: in
// the DSN, in the environment, or at its default place in the home
// directory. libpq refuses both; pgx, which ariesd connects with, reads
// either whatever its mode (aries-l6emugw5).
func TestDatabaseSecretFilesRefuseLooseModes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	const base = "postgres://aries@db.example.com/aries"
	for _, tc := range []struct {
		name string
		// file is the secret file's path under the test's directory;
		// others are files beside it that make it the one read.
		file   string
		others []string
		dsn    string // %s is the test's directory
		env    map[string]string
	}{
		{name: "passfile= in the DSN", file: "pgpass", dsn: base + "?passfile=%s/pgpass"},
		{name: "PGPASSFILE", file: "pgpass", dsn: base, env: map[string]string{"PGPASSFILE": "pgpass"}},
		{name: "~/.pgpass", file: ".pgpass", dsn: base},
		{name: "sslkey= in the DSN", file: "client.key", dsn: base + "?sslmode=verify-full&sslkey=%s/client.key"},
		{name: "PGSSLKEY", file: "client.key", dsn: base, env: map[string]string{"PGSSLKEY": "client.key"}},
		{name: "~/.postgresql/postgresql.key", file: ".postgresql/postgresql.key", others: []string{".postgresql/postgresql.crt"}, dsn: base},
	} {
		for _, mode := range []os.FileMode{0o640, 0o604} {
			t.Run(fmt.Sprintf("%s %04o", tc.name, mode), func(t *testing.T) {
				t.Parallel()
				home := t.TempDir()
				for _, f := range append([]string{tc.file}, tc.others...) {
					p := filepath.Join(home, f)
					if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(p, []byte("*:*:*:*:pw\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				secret := filepath.Join(home, tc.file)
				if err := os.Chmod(secret, mode); err != nil { //nolint:gosec // the mode under test
					t.Fatal(err)
				}
				env := map[string]string{"HOME": home, EnvDSN: strings.ReplaceAll(tc.dsn, "%s", home)}
				for k, v := range tc.env {
					env[k] = filepath.Join(home, v)
				}
				_, err := ResolveSettings(Settings{}, "", func(k string) string { return env[k] })
				if err == nil || !strings.Contains(err.Error(), "chmod 600 "+secret) {
					t.Errorf("ResolveSettings with %s of mode %04o = %v, want a refusal naming chmod 600 %s", tc.name, mode, err, secret)
				}
				if err != nil && strings.Contains(err.Error(), "pw") {
					t.Errorf("the refusal quotes the file: %v", err)
				}
				// At 0600 the same settings resolve.
				if err := os.Chmod(secret, 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := ResolveSettings(Settings{}, "", func(k string) string { return env[k] }); err != nil {
					t.Errorf("ResolveSettings with %s of mode 0600 = %v, want nil", tc.name, err)
				}
			})
		}
	}
}

// What pgx would not read is not checked: a DSN with a password of its
// own reads no password file, a client key at its default place is
// read only beside its certificate, and a file that is not there is no
// refusal.
func TestDatabaseSecretFilesNotRead(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not checked on Windows")
	}
	home := t.TempDir()
	for _, f := range []string{".pgpass", ".postgresql/postgresql.key"} {
		p := filepath.Join(home, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil { //nolint:gosec // loose on purpose
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"a password in the DSN", map[string]string{"HOME": home, EnvDSN: "postgres://aries:pw@db.example.com/aries"}}, //nolint:gosec // an invented password
		{"a password in PGPASSWORD", map[string]string{"HOME": home, "PGPASSWORD": "pw", EnvDSN: "postgres://aries@db.example.com/aries"}},
		{"no such passfile", map[string]string{"HOME": t.TempDir(), EnvDSN: "postgres://aries@db.example.com/aries?passfile=" + filepath.Join(home, "none")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ResolveSettings(Settings{}, "", func(k string) string { return tc.env[k] }); err != nil {
				t.Errorf("ResolveSettings = %v, want nil", err)
			}
		})
	}
}
