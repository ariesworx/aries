package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/oidc/oidctest"
)

// The profile a process runs under is read from what the kernel says of
// it, and an unconfined process has none.
func TestParseAppArmorLabel(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"ariesd-serve (enforce)\n", "ariesd-serve"},
		{"ariesd-admin (complain)", "ariesd-admin"},
		{"unconfined\n", ""},
		{"", ""},
		{"  \n", ""},
	}
	for _, tc := range tests {
		if got := parseAppArmorLabel(tc.in); got != tc.want {
			t.Errorf("parseAppArmorLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A provider's file refused for want of permission, under an AppArmor
// profile, is most likely outside the directory the profile reads; the
// fix names it. Unconfined, or for another error, there is no such fix.
func TestProviderFileFix(t *testing.T) {
	t.Parallel()
	denied := fmt.Errorf("client_secret_file: %w", &fs.PathError{Op: "open", Path: "/etc/aries/corp-secret", Err: fs.ErrPermission})
	missing := fmt.Errorf("client_secret_file: %w", &fs.PathError{Op: "open", Path: "/etc/aries/login/corp-secret", Err: fs.ErrNotExist})
	tests := []struct {
		name    string
		err     error
		profile string
		want    bool
	}{
		{"denied, confined", denied, "ariesd-serve", true},
		{"denied, unconfined", denied, "", false},
		{"missing, confined", missing, "ariesd-serve", false},
		{"another error", errors.New("issuer is required"), "ariesd-serve", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fix := providerFileFix(tc.err, tc.profile)
			if !tc.want {
				if fix != "" {
					t.Errorf("providerFileFix = %q, want none", fix)
				}
				return
			}
			for _, want := range []string{"fix:", tc.profile, AppArmorLoginDir, "0600"} {
				if !strings.Contains(fix, want) {
					t.Errorf("providerFileFix = %q, want it to name %q", fix, want)
				}
			}
		})
	}
}

// OpenLogin refuses a secret it may not open, and under AppArmor says to
// move it where the profile reads; unconfined, it says what it said before.
func TestOpenLoginDenied(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test's user cannot enter, which root and Windows do not refuse")
	}
	iss := oidctest.New(t)
	dir := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret")
	if err := os.WriteFile(secret, []byte(oidctest.Secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // put back, so the directory can be removed
	s, err := resolveFile(t, "login:\n  providers:\n    - name: corp\n      kind: oidc\n      issuer: "+iss.URL+
		"\n      client_id: "+oidctest.ClientID+"\n      client_secret_file: "+secret+"\n      ca_file: "+iss.CAFile+"\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = openLogin(s.Login, time.Now, "ariesd-serve")
	if err == nil || !strings.Contains(err.Error(), AppArmorLoginDir) || !strings.Contains(err.Error(), "ariesd-serve") {
		t.Errorf("openLogin(confined, a secret it may not read) = %v, want a refusal naming %s and the profile", err, AppArmorLoginDir)
	}
	_, err = openLogin(s.Login, time.Now, "")
	if err == nil || strings.Contains(err.Error(), "AppArmor") || !strings.Contains(err.Error(), "fix:") {
		t.Errorf("openLogin(unconfined, a secret it may not read) = %v, want a refusal with a fix that does not name AppArmor", err)
	}
}
