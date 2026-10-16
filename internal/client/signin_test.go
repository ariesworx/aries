package client

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/proto"
)

func newSignIn(t *testing.T, server string) *SignIn {
	t.Helper()
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	return &SignIn{Server: server, Token: "tok-" + strings.Repeat("a", 48), ExpiresAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC),
		Scopes: []string{"cli", "mcp"}, Principal: "alice", Provider: "corp", Grant: "g-1", Key: k}
}

func TestSignInRoundTrip(t *testing.T) {
	t.Parallel()
	data := filepath.Join(t.TempDir(), "aries")
	const server = "https://aries.example.com"
	if s, err := LoadSignIn(data, server); s != nil || err != nil {
		t.Fatalf("LoadSignIn(nothing saved) = %v, %v; want none", s, err)
	}
	in := newSignIn(t, server)
	if err := SaveSignIn(data, in); err != nil {
		t.Fatalf("SaveSignIn = %v", err)
	}
	out, err := LoadSignIn(data, server)
	if err != nil || out == nil {
		t.Fatalf("LoadSignIn = %v, %v", out, err)
	}
	if out.Token != in.Token || !out.ExpiresAt.Equal(in.ExpiresAt) || out.Principal != "alice" || out.Provider != "corp" ||
		out.Grant != "g-1" || strings.Join(out.Scopes, " ") != "cli mcp" || !out.Key.Equal(in.Key) {
		t.Errorf("LoadSignIn = %+v, want what was saved, %+v", out, in)
	}
	// Another server's is another directory.
	if s, err := LoadSignIn(data, "https://other.example.com"); s != nil || err != nil {
		t.Errorf("LoadSignIn(another server) = %v, %v; want none", s, err)
	}
	if runtime.GOOS != "windows" {
		dir := SignInDir(data, server)
		for _, p := range []string{dir, filepath.Join(dir, signInTokenFile), filepath.Join(dir, signInKeyFile)} {
			st, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm()&0o077 != 0 {
				t.Errorf("%s is mode %04o, want the user's alone", p, st.Mode().Perm())
			}
		}
	}
	if err := RemoveSignIn(data, server); err != nil {
		t.Fatalf("RemoveSignIn = %v", err)
	}
	if s, err := LoadSignIn(data, server); s != nil || err != nil {
		t.Errorf("LoadSignIn(after RemoveSignIn) = %v, %v; want none", s, err)
	}
	if err := RemoveSignIn(data, server); err != nil {
		t.Errorf("RemoveSignIn(nothing there) = %v, want nil", err)
	}
}

// TestSignInFilesMustBeTheUsersOwn: a token and its key in a directory
// others can read are refused, never used and never chmodded.
func TestSignInFilesMustBeTheUsersOwn(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits; its ACLs are not checked")
	}
	data := filepath.Join(t.TempDir(), "aries")
	const server = "https://aries.example.com"
	if err := SaveSignIn(data, newSignIn(t, server)); err != nil {
		t.Fatal(err)
	}
	dir := SignInDir(data, server)
	for _, tc := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{"the directory", dir, 0o755},
		{"the token", filepath.Join(dir, signInTokenFile), 0o644},
		{"the key", filepath.Join(dir, signInKeyFile), 0o640},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Serial: each case changes and restores a mode of the one
			// sign-in the test saved.
			old, err := os.Stat(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(tc.path, tc.mode); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Chmod(tc.path, old.Mode().Perm()) }()
			_, err = LoadSignIn(data, server)
			pe, ok := errors.AsType[*proto.Error](err)
			if !ok || pe.Code != proto.CodeAuth || !strings.Contains(pe.Message, tc.path) || !strings.Contains(pe.Fix, "aries login") {
				t.Errorf("LoadSignIn(%s mode %04o) = %v, want a refusal naming it, with a fix to sign in again", tc.name, tc.mode, err)
			}
		})
	}
}

func TestSignInLive(t *testing.T) {
	t.Parallel()
	s := newSignIn(t, "https://aries.example.com")
	if !s.Live(s.ExpiresAt.Add(-time.Second)) || s.Live(s.ExpiresAt) {
		t.Errorf("Live: a sign-in ending at %s is live a second before and not at its end", s.ExpiresAt)
	}
}
