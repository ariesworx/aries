package clienttest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEveryConnectingPackageIsIsolated reads the module's tests: a
// package whose tests can make aries choose a key (they import the client,
// or something that connects with it) must call Isolate, which is what
// keeps them from the developer's ~/.ssh and ssh-agent.
func TestEveryConnectingPackageIsIsolated(t *testing.T) {
	t.Parallel()
	const module = `"github.com/ariesworx/aries/internal/`
	connects := []string{module + `client"`, module + `cli"`, module + `mcpserver"`, module + `board"`, module + `offline"`}
	needs, has := map[string]string{}, map[string]bool{}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "spike" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // this module's own tests
		if err != nil {
			return err
		}
		src, dir := string(b), filepath.Dir(path)
		// A package's own tests count too: client, cli, mcpserver, board.
		own := `"github.com/ariesworx/aries/` + filepath.ToSlash(strings.TrimPrefix(dir, root+string(filepath.Separator))) + `"`
		for _, imp := range connects {
			if strings.Contains(src, imp) || own == imp {
				needs[dir] = path
			}
		}
		if strings.Contains(src, "func TestMain(") && strings.Contains(src, "clienttest.Isolate()") {
			has[dir] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) < 5 {
		t.Fatalf("found %d packages whose tests connect (%v), want at least client, cli, mcpserver, board and e2e: the search is broken", len(needs), needs)
	}
	for dir, file := range needs {
		if !has[dir] {
			t.Errorf("%s has tests that can sign in (%s) and no TestMain that calls clienttest.Isolate: they could offer the developer's own keys", dir, filepath.Base(file))
		}
	}
}

// TestEveryTestIsParallelOrSaysWhyNot reads the module's tests: each
// begins with t.Parallel(), or with a comment that starts "Serial:" and
// gives the reason it cannot run beside another (CONTRIBUTING.md, "Tests
// run in parallel"). A test that says nothing is one nobody decided
// about.
func TestEveryTestIsParallelOrSaysWhyNot(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	parallel, serial := 0, 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "spike" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			params := fn.Type.Params.List
			if len(params) != 1 || len(params[0].Names) != 1 {
				continue // not a test: go vet says so
			}
			switch {
			case startsParallel(fn, params[0].Names[0].Name):
				parallel++
			case saysSerial(f, fn):
				serial++
			default:
				t.Errorf("%s: %s neither begins with %s.Parallel() nor says why not: begin it with a comment \"// Serial: the reason.\"",
					fset.Position(fn.Pos()), fn.Name.Name, params[0].Names[0].Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if parallel < 500 || serial < 10 {
		t.Fatalf("found %d parallel tests and %d serial ones, want hundreds and tens: the search is broken", parallel, serial)
	}
}

// startsParallel reports whether fn's first statement is t.Parallel().
func startsParallel(fn *ast.FuncDecl, t string) bool {
	if len(fn.Body.List) == 0 {
		return false
	}
	stmt, ok := fn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Parallel" {
		return false
	}
	x, ok := sel.X.(*ast.Ident)
	return ok && x.Name == t
}

// saysSerial reports whether fn's body begins with a comment that starts
// "Serial:" and goes on to a reason.
func saysSerial(f *ast.File, fn *ast.FuncDecl) bool {
	end := fn.Body.Rbrace
	if len(fn.Body.List) > 0 {
		end = fn.Body.List[0].Pos()
	}
	for _, group := range f.Comments {
		if group.Pos() < fn.Body.Lbrace || group.Pos() > end {
			continue
		}
		reason, ok := strings.CutPrefix(group.Text(), "Serial:")
		return ok && len(strings.TrimSpace(reason)) >= 10
	}
	return false
}

func TestIsolate(t *testing.T) {
	// Serial: sets the process's environment (t.Setenv, Isolate).
	t.Setenv("HOME", "/somebody/else")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/somebody.sock")
	t.Setenv("ARIES_KEY_FILE", "/somebody/else/.ssh/id_ed25519")
	t.Setenv("ARIES_DATA_DIR", "/somebody/else/data")
	t.Setenv("ARIES_NO_QUEUE", "1")
	restore := Isolate()
	home, err := os.UserHomeDir()
	if err != nil || home == "/somebody/else" {
		t.Fatalf("os.UserHomeDir() under Isolate = %q, %v; want a directory of the test's own", home, err)
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Errorf("the isolated home holds %v (%v), want it empty", entries, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
		t.Errorf("the isolated home has a .ssh: %v", err)
	}
	// What a test queues goes to a directory of the test binary's own,
	// never to the outbox of whoever runs the tests.
	data := os.Getenv("ARIES_DATA_DIR")
	if st, err := os.Stat(data); err != nil || !st.IsDir() || data == "/somebody/else/data" || data == home { //nolint:gosec // the directory Isolate just made
		t.Errorf("ARIES_DATA_DIR under Isolate = %q (%v), want a directory of the test's own, apart from its home", data, err)
	}
	for _, k := range []string{"SSH_AUTH_SOCK", "ARIES_KEY_FILE", "ARIES_KEY", "ARIES_NO_QUEUE"} {
		if v, ok := os.LookupEnv(k); ok {
			t.Errorf("%s = %q under Isolate, want it unset", k, v)
		}
	}
	restore()
	if got := os.Getenv("HOME"); got != "/somebody/else" {
		t.Errorf("HOME after restore = %q, want what it was", got)
	}
	if got := os.Getenv("SSH_AUTH_SOCK"); got != "/tmp/somebody.sock" {
		t.Errorf("SSH_AUTH_SOCK after restore = %q, want what it was", got)
	}
	if got := os.Getenv("ARIES_DATA_DIR"); got != "/somebody/else/data" {
		t.Errorf("ARIES_DATA_DIR after restore = %q, want what it was", got)
	}
	for _, dir := range []string{home, data} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) { //nolint:gosec // the directories Isolate made
			t.Errorf("%s is still there after restore: %v", dir, err)
		}
	}
}

// Under IsolateGit the git a test starts reads no configuration but the
// repository's own: not the one in the home directory of whoever runs the
// tests, and not one GIT_CONFIG_GLOBAL already named.
func TestIsolateGit(t *testing.T) {
	// Serial: sets the process's environment (t.Setenv, IsolateGit).
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	home := t.TempDir()
	theirs := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(theirs, []byte("[user]\n\tname = Somebody Else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GIT_CONFIG_GLOBAL", theirs)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")
	name := func() string {
		// git config exits 1 when the key is not set.
		cmd := exec.CommandContext(t.Context(), "git", "config", "--global", "--get", "user.name")
		cmd.Dir = home // not the checkout, whose own repository git would read first
		out, _ := cmd.Output()
		return strings.TrimSpace(string(out))
	}
	if got := name(); got != "Somebody Else" {
		t.Fatalf("git config --global user.name before IsolateGit = %q, want Somebody Else: the test's own setup is wrong", got)
	}
	restore := IsolateGit()
	if got := name(); got != "" {
		t.Errorf("git config --global user.name under IsolateGit = %q, want none", got)
	}
	if got := os.Getenv("GIT_CONFIG_NOSYSTEM"); got != "1" {
		t.Errorf("GIT_CONFIG_NOSYSTEM under IsolateGit = %q, want 1", got)
	}
	restore()
	if got := name(); got != "Somebody Else" {
		t.Errorf("git config --global user.name after restore = %q, want what it was", got)
	}
	if got := os.Getenv("GIT_CONFIG_NOSYSTEM"); got != "" {
		t.Errorf("GIT_CONFIG_NOSYSTEM after restore = %q, want what it was", got)
	}
}

// SilentPort accepts and never answers while the test runs, and resets
// the connections waiting in its backlog when it ends.
func TestSilentPort(t *testing.T) {
	t.Parallel()
	var c net.Conn
	t.Run("open", func(t *testing.T) {
		var err error
		if c, err = net.Dial("tcp", SilentPort(t)); err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if n, err := c.Read(make([]byte, 1)); n != 0 || !isTimeout(err) {
			t.Fatalf("read %d bytes, err %v: want a timeout, as the port never answers", n, err)
		}
	})
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatalf("the connection was not ended when the test ended: %v", err)
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}
