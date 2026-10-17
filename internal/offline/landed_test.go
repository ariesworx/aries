package offline

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/outbox"
)

// What `aries land` last reported is kept per server and project, in the
// directory of the other things kept for them: nothing at first, then
// what was set, replaced whole.
func TestLanded(t *testing.T) {
	t.Parallel()
	l, _ := newLocal(t, outbox.Options{})
	sha1, sha2 := strings.Repeat("a1", 20), strings.Repeat("b2", 20)
	if got, err := l.Landed(); err != nil || got != "" {
		t.Fatalf("Landed before any = %q, %v; want none", got, err)
	}
	if err := l.SetLanded(sha1); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Landed(); err != nil || got != sha1 {
		t.Errorf("Landed = %q, %v; want %s", got, err, sha1)
	}
	if err := l.SetLanded(sha2); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Landed(); got != sha2 {
		t.Errorf("Landed after a second set = %q; want %s", got, sha2)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(filepath.Join(l.Dir(), landedFile))
		if err != nil || st.Mode().Perm()&0o077 != 0 {
			t.Errorf("landed file = %v, %v; want the owner's alone", st, err)
		}
	}
	// Another project's directory has its own.
	other := Open(l.root, hostKey, "7a2d3e4f-5b6c-4d7e-9f80-1b2c3d4e5f60", Options{})
	if got, _ := other.Landed(); got != "" {
		t.Errorf("another project's Landed = %q; want none", got)
	}
}

// Text that is no commit hash is not kept, and a file that holds none is
// read as none: a hash goes to git as an argument.
func TestLandedTakesOnlyAHash(t *testing.T) {
	t.Parallel()
	l, _ := newLocal(t, outbox.Options{})
	for _, bad := range []string{"", "main", "--output=x", "A1B2", strings.Repeat("a", 39), strings.Repeat("a", 41) + "\n"} {
		if err := l.SetLanded(bad); err == nil {
			t.Errorf("SetLanded(%q) succeeded", bad)
		}
	}
	if err := l.replace(landedFile, []byte("--upload-pack=x\n")); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Landed(); err != nil || got != "" {
		t.Errorf("Landed of a file that holds no hash = %q, %v; want none", got, err)
	}
}

// A directory that is not the user's alone is not read or written.
func TestLandedNeedsTheUsersOwnDirectory(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("modes are not checked on Windows")
	}
	l, _ := newLocal(t, outbox.Options{})
	if err := os.Chmod(l.root, 0o755); err != nil { //nolint:gosec // the point of the test
		t.Fatal(err)
	}
	if _, err := l.Landed(); err == nil {
		t.Error("Landed read an open directory")
	}
	if err := l.SetLanded(strings.Repeat("a1", 20)); err == nil {
		t.Error("SetLanded wrote an open directory")
	}
}
