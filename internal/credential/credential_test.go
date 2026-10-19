package credential

import (
	"context"
	"encoding/hex"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// testSecret looks like an API key and is no one's.
const testSecret = "sk-test-0123456789abcdefABCDEF_-xyz"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "secret-tool" {
		os.Exit(fakeSecretTool(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// recorder starts processes for a store and keeps every argv it started,
// so a test can hold them to the secret.
type recorder struct {
	mu    sync.Mutex
	argvs [][]string
	// fake, when set, runs the test binary as secret-tool, keeping its
	// secrets under this directory; mode is the fake's behavior.
	fake, mode string
}

func (r *recorder) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	r.mu.Lock()
	r.argvs = append(r.argvs, append([]string{name}, args...))
	r.mu.Unlock()
	if name == "secret-tool" && r.fake != "" {
		cmd := exec.CommandContext(ctx, os.Args[0], args...) //nolint:gosec // the test binary itself
		cmd.Env = append(os.Environ(), fakeEnv+"=secret-tool", fakeDirEnv+"="+r.fake, fakeModeEnv+"="+r.mode)
		return cmd
	}
	return exec.CommandContext(ctx, name, args...) //nolint:gosec // the store's commands, which these tests check
}

func (r *recorder) lookPath(name string) (string, error) {
	if name == "secret-tool" && r.fake != "" {
		return name, nil
	}
	if name == "secret-tool" {
		return "", exec.ErrNotFound
	}
	return exec.LookPath(name)
}

// noSecretIn fails the test if any argv the recorder started, or any of
// texts, carries the secret or its hexadecimal form.
func (r *recorder) noSecretIn(t *testing.T, texts ...string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	forms := []string{testSecret, hex.EncodeToString([]byte(testSecret)), strings.ToUpper(hex.EncodeToString([]byte(testSecret)))}
	for _, argv := range r.argvs {
		for _, a := range argv {
			for _, f := range forms {
				if strings.Contains(a, f) {
					t.Errorf("argv %q carries the secret", argv[0])
				}
			}
		}
	}
	for _, s := range texts {
		for _, f := range forms {
			if strings.Contains(s, f) {
				t.Errorf("output %q carries the secret", s)
			}
		}
	}
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func noHome() (string, error) { return "", errors.New("no home in tests") }

// fileOptions opens the file store under a data directory of the test's.
func fileOptions(t *testing.T, r *recorder) (Options, string) {
	t.Helper()
	data := filepath.Join(t.TempDir(), "data")
	return Options{
		GOOS:     "linux",
		Getenv:   env(map[string]string{"ARIES_DATA_DIR": data}),
		Home:     noHome,
		Command:  r.command,
		LookPath: r.lookPath,
	}, data
}

// roundTrip sets, reads, replaces and removes a secret in s, and returns
// every error text it saw for the caller to hold to the secret.
func roundTrip(t *testing.T, s Store) []string {
	t.Helper()
	ctx := t.Context()
	var texts []string
	if _, ok, err := s.Get(ctx, "api-key"); err != nil || ok {
		t.Fatalf("Get before Set = %v, %v; want not found", ok, err)
	}
	if err := s.Set(ctx, "api-key", testSecret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := s.Get(ctx, "api-key")
	if err != nil || !ok || got != testSecret {
		t.Fatalf("Get = %v, %v; want the secret", ok, err)
	}
	if err := s.Set(ctx, "api-key", testSecret+"2"); err != nil {
		t.Fatalf("Set again: %v", err)
	}
	if got, _, _ := s.Get(ctx, "api-key"); got != testSecret+"2" {
		t.Fatal("Set did not replace the secret")
	}
	if err := s.Set(ctx, "api-key", "bad secret with spaces"); err == nil {
		t.Error("Set took a secret with a space")
	} else {
		texts = append(texts, err.Error())
	}
	if err := s.Remove(ctx, "api-key"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, err := s.Get(ctx, "api-key"); err != nil || ok {
		t.Fatalf("Get after Remove = %v, %v; want not found", ok, err)
	}
	if err := s.Remove(ctx, "api-key"); err != nil {
		t.Fatalf("Remove of nothing: %v", err)
	}
	return append(texts, s.Name())
}

func TestFileStore(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	o, data := fileOptions(t, r)
	s, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind() != KindFile || !strings.Contains(s.Name(), filepath.Join(data, "credentials")) {
		t.Fatalf("store = %s %q; want the file under the data directory", s.Kind(), s.Name())
	}
	texts := roundTrip(t, s)
	if err := s.Set(t.Context(), "api-key", testSecret); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "credentials", "aries.api-key")
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil || string(b) != testSecret {
		t.Fatalf("file holds %d bytes, %v; want the secret", len(b), err)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{path, filepath.Dir(path)} {
			st, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			want := fs.FileMode(0o600)
			if st.IsDir() {
				want = 0o700
			}
			if st.Mode().Perm() != want {
				t.Errorf("%s has mode %04o, want %04o", p, st.Mode().Perm(), want)
			}
		}
	}
	r.noSecretIn(t, texts...)
	if len(r.argvs) != 0 {
		t.Errorf("the file store started %d processes", len(r.argvs))
	}
}

func TestFileStoreRefuses(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("modes are not checked on Windows, where the file store is refused")
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, s *fileStore, path string)
		want  string
	}{
		{"file mode 0644", func(t *testing.T, _ *fileStore, path string) {
			chmod(t, path, 0o644)
		}, "chmod 600"},
		{"file mode 0400", func(t *testing.T, _ *fileStore, path string) {
			chmod(t, path, 0o400)
		}, "chmod 600"},
		{"directory mode 0755", func(t *testing.T, _ *fileStore, path string) {
			chmod(t, filepath.Dir(path), 0o755)
		}, "chmod 700"},
		{"another user's file", func(_ *testing.T, s *fileStore, _ string) {
			s.owned = func(fi fs.FileInfo) bool { return fi.IsDir() }
		}, "belongs to another user"},
		{"another user's directory", func(_ *testing.T, s *fileStore, _ string) {
			s.owned = func(fi fs.FileInfo) bool { return !fi.IsDir() }
		}, "belongs to another user"},
		{"a symbolic link", func(t *testing.T, _ *fileStore, path string) {
			other := filepath.Join(t.TempDir(), "other")
			if err := os.WriteFile(other, []byte(testSecret), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, path); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &recorder{}
			o, data := fileOptions(t, r)
			st, err := Open(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			s := st.(*fileStore)
			if err := s.Set(t.Context(), "api-key", testSecret); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(data, "credentials", "aries.api-key")
			tt.setup(t, s, path)
			_, ok, err := s.Get(t.Context(), "api-key")
			var pe *proto.Error
			if !errors.As(err, &pe) || pe.Code != proto.CodeUnavailable || !strings.Contains(pe.Error(), tt.want) || pe.Fix == "" {
				t.Fatalf("Get = %v, %v; want unavailable naming %q, with a fix", ok, err, tt.want)
			}
			texts := []string{err.Error()}
			if err := s.Set(t.Context(), "api-key", testSecret); err == nil {
				t.Error("Set wrote over a file it refuses to read")
			} else {
				texts = append(texts, err.Error())
			}
			r.noSecretIn(t, texts...)
		})
	}
}

func chmod(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o700) }) //nolint:gosec // so that TempDir can remove it
}

func TestSecretService(t *testing.T) {
	t.Parallel()
	r := &recorder{fake: t.TempDir()}
	o, _ := fileOptions(t, r)
	s, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind() != KindSecretService || s.Name() != "Secret Service" {
		t.Fatalf("store = %s %q; want the Secret Service", s.Kind(), s.Name())
	}
	texts := roundTrip(t, s)
	r.noSecretIn(t, texts...)
	if !slices.ContainsFunc(r.argvs, func(a []string) bool { return len(a) > 1 && a[1] == "store" }) {
		t.Error("secret-tool store never ran")
	}
}

// TestSecretServiceNotRunning is a Linux machine with secret-tool and no
// service answering: the file is used, and said so.
func TestSecretServiceNotRunning(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"absent", "hang"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r := &recorder{fake: t.TempDir(), mode: mode}
			o, _ := fileOptions(t, r)
			o.probeTimeout = 200e6
			s, err := Open(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			if s.Kind() != KindFile {
				t.Fatalf("store = %s; want the file when no Secret Service answers", s.Kind())
			}
		})
	}
}

func TestSecretServiceErrorsKeepTheSecretOut(t *testing.T) {
	t.Parallel()
	r := &recorder{fake: t.TempDir()}
	o, _ := fileOptions(t, r)
	s, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	r.mode = "echo" // the fake now fails, printing what it was given
	err = s.Set(t.Context(), "api-key", testSecret)
	if err == nil {
		t.Fatal("Set succeeded against a failing secret-tool")
	}
	r.noSecretIn(t, err.Error())
}

func TestKeychain(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "darwin" {
		t.Skip("the keychain is macOS's")
	}
	kc := tempKeychain(t)
	r := &recorder{}
	s, err := Open(t.Context(), Options{GOOS: "darwin", Getenv: env(nil), Home: noHome, Command: r.command, LookPath: r.lookPath, Keychain: kc})
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind() != KindKeychain || s.Name() != "macOS keychain" {
		t.Fatalf("store = %s %q; want the keychain", s.Kind(), s.Name())
	}
	texts := roundTrip(t, s)
	r.noSecretIn(t, texts...)
}

// tempKeychain makes a keychain file of the test's, unlocked, so that no
// test reads or writes the user's own; its password is the test's too.
func tempKeychain(t *testing.T) string {
	t.Helper()
	kc := filepath.Join(t.TempDir(), "test.keychain-db")
	for _, args := range [][]string{
		{"create-keychain", "-p", "test", kc},
		{"unlock-keychain", "-p", "test", kc},
		{"set-keychain-settings", kc},
	} {
		out, err := exec.CommandContext(t.Context(), "security", args...).CombinedOutput() //nolint:gosec // a keychain of the test's
		if err != nil {
			t.Fatalf("security %s: %v: %s", args[0], err, out)
		}
	}
	t.Cleanup(func() {
		_ = exec.Command("security", "delete-keychain", kc).Run() //nolint:noctx,gosec // cleanup, after the test's context ended
	})
	return kc
}

func TestKindRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		goos string
		kind Kind
	}{
		{"windows", KindAuto},
		{"windows", KindFile},
		{"linux", KindKeychain},
		{"darwin", KindSecretService},
		{"windows", KindWindows},
	}
	for _, tt := range tests {
		t.Run(tt.goos+"/"+string(tt.kind), func(t *testing.T) {
			t.Parallel()
			r := &recorder{}
			o, _ := fileOptions(t, r)
			o.GOOS, o.Kind = tt.goos, tt.kind
			s, err := Open(t.Context(), o)
			if tt.goos == "windows" && (tt.kind == KindAuto || tt.kind == KindWindows) {
				// Windows has its store, which refuses each call until it
				// is built.
				if err != nil || s.Kind() != KindWindows {
					t.Fatalf("Open = %v, %v; want the Windows store", s, err)
				}
				for _, err := range []error{
					s.Set(t.Context(), "api-key", testSecret),
					s.Remove(t.Context(), "api-key"),
					func() error { _, _, err := s.Get(t.Context(), "api-key"); return err }(),
				} {
					var pe *proto.Error
					if !errors.As(err, &pe) || pe.Code != proto.CodeUnavailable || pe.Fix == "" {
						t.Errorf("Windows store: %v; want unavailable with a fix", err)
					}
				}
				return
			}
			var pe *proto.Error
			if !errors.As(err, &pe) || pe.Code != proto.CodeInvalid || pe.Fix == "" {
				t.Fatalf("Open = %v; want invalid with a fix", err)
			}
		})
	}
}

func TestNames(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	o, _ := fileOptions(t, r)
	s, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "Api", "a b", "../x", ".x", "a/b", strings.Repeat("a", 65), "-x"} {
		if err := s.Set(t.Context(), name, testSecret); err == nil {
			t.Errorf("Set took the name %q", name)
		}
		if _, _, err := s.Get(t.Context(), name); err == nil {
			t.Errorf("Get took the name %q", name)
		}
	}
	o.Service = "Bad Service"
	if _, err := Open(t.Context(), o); err == nil {
		t.Error("Open took a service with a space and capitals")
	}
}

func TestSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		secret string
		ok     bool
	}{
		{testSecret, true},
		{strings.Repeat("a", minSecret), true},
		{strings.Repeat("a", minSecret-1), false},
		{strings.Repeat("a", maxSecret), true},
		{strings.Repeat("a", maxSecret+1), false},
		{"", false},
		{"aaaaaaaa bbbbbbbbb", false},
		{"aaaaaaaabbbbbbbbb\n", false},
		{"aaaaaaaabbbbbbbbb\x00", false},
		{"aaaaaaaabbbbbbbbbé", false},
	}
	for i, tt := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			err := checkSecret(tt.secret)
			if (err == nil) != tt.ok {
				t.Fatalf("checkSecret = %v; want ok %v", err, tt.ok)
			}
			if err != nil && tt.secret != "" && strings.Contains(err.Error(), tt.secret) {
				t.Errorf("refusal %q quotes the secret", err)
			}
		})
	}
}

// TestRunRefusesSecretInArgv: a command line that would carry the secret
// is refused, and nothing is started.
func TestRunRefusesSecretInArgv(t *testing.T) {
	t.Parallel()
	r := &recorder{}
	x := execer{command: r.command}
	for _, arg := range []string{testSecret, "-w" + testSecret, hex.EncodeToString([]byte(testSecret))} {
		_, err := x.run(t.Context(), testSecret, "", "security", "add-generic-password", arg)
		if err == nil {
			t.Errorf("run took an argv carrying the secret")
		} else {
			r.noSecretIn(t, err.Error())
		}
	}
	if len(r.argvs) != 0 {
		t.Errorf("run started %d processes", len(r.argvs))
	}
}

// TestNoLogsOrOutput holds the package to writing nowhere: it imports no
// logger and names none of the process's streams. What it says, it says
// in errors, which the tests above hold to the secret.
func TestNoLogsOrOutput(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if path, _ := strconv.Unquote(imp.Path.Value); path == "log" || path == "log/slog" {
				t.Errorf("%s imports %s", name, path)
			}
		}
		b, err := os.ReadFile(name) //nolint:gosec // the package's own source
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{"os.Stdout", "os.Stderr", "fmt.Print"} {
			if strings.Contains(string(b), s) {
				t.Errorf("%s names %s", name, s)
			}
		}
	}
}

// TestRealSecretService runs against the Secret Service of the machine,
// with ARIES_TEST_SECRET_SERVICE=1, under a service of the test's own,
// which it removes: the other tests use a fake secret-tool, and a
// person's keyring is not touched unasked.
func TestRealSecretService(t *testing.T) {
	t.Parallel()
	if os.Getenv("ARIES_TEST_SECRET_SERVICE") != "1" {
		t.Skip("set ARIES_TEST_SECRET_SERVICE=1 to use this machine's Secret Service")
	}
	r := &recorder{}
	o, _ := fileOptions(t, r)
	o.GOOS, o.LookPath, o.Service = runtime.GOOS, exec.LookPath, "aries-test-"+strconv.Itoa(os.Getpid())
	s, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind() != KindSecretService {
		t.Fatalf("store = %s; no Secret Service answered", s.Kind())
	}
	t.Cleanup(func() { _ = s.Remove(context.Background(), "api-key") })
	r.noSecretIn(t, roundTrip(t, s)...)
}
