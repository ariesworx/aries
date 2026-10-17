package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// protoOps reads every op the protocol names: the string constants of
// internal/proto whose names begin with Op.
func protoOps(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "proto", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, "Op") || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					op, _ := strconv.Unquote(lit.Value)
					ops = append(ops, op)
				}
			}
		}
	}
	if len(ops) < 50 {
		t.Fatalf("found %d ops in internal/proto, fewer than the protocol has; the parse is wrong", len(ops))
	}
	return ops
}

// TestEveryOpHasScopes holds the op list to its scopes (transport.md
// §6.8, §12.4 item 2): every op the protocol names is in opScopes, so a
// new op is refused to every token but cli until someone decides who
// may send it.
func TestEveryOpHasScopes(t *testing.T) {
	t.Parallel()
	ops := protoOps(t)
	for _, op := range unscoped(ops) {
		t.Errorf("op %q has no entry in opScopes; add it, with the scopes besides cli whose tokens may send it", op)
	}
	for op := range opScopes() {
		if !slices.Contains(ops, op) {
			t.Errorf("opScopes names %q, which is no op of internal/proto", op)
		}
	}
}

// unscoped returns the ops of ops that opScopes has no entry for.
func unscoped(ops []string) []string {
	var out []string
	for _, op := range ops {
		if _, ok := opScopes()[op]; !ok {
			out = append(out, op)
		}
	}
	return out
}

// TestEveryOpHasScopesFails: the check above reports an op added to the
// protocol without an entry, and only that one; and the parse it reads
// the protocol by finds the ops of every form internal/proto declares
// them in, a block of constants or one alone.
func TestEveryOpHasScopesFails(t *testing.T) {
	t.Parallel()
	ops := protoOps(t)
	if got := unscoped(append(slices.Clone(ops), "made.up")); !slices.Equal(got, []string{"made.up"}) {
		t.Errorf("unscoped with an op added = %q, want only made.up", got)
	}
	for _, op := range []string{proto.OpCreate, proto.OpWatch, proto.OpGateWaive, proto.OpKeyAdd, proto.OpTrustRemove, proto.OpSignInEnd} {
		if !slices.Contains(ops, op) {
			t.Errorf("protoOps misses %q", op)
		}
	}
}

// TestScopeRules holds the table to the rules: an mcp or ci token never
// reaches an admin op, never decides a person's gate, and never waives;
// locks are not an agent's (decision 70).
func TestScopeRules(t *testing.T) {
	t.Parallel()
	for op, scopes := range opScopes() {
		for _, sc := range scopes {
			if sc != proto.ScopeMCP && sc != proto.ScopeCI {
				t.Errorf("opScopes[%q] names scope %q; it lists only mcp and ci, cli allowing every op", op, sc)
			}
		}
		mcp, ci := slices.Contains(scopes, proto.ScopeMCP), slices.Contains(scopes, proto.ScopeCI)
		switch {
		case adminOp(op) && (mcp || ci):
			t.Errorf("admin op %q is open to a token of scope mcp or ci; rule 12 keeps admin to a person's terminal", op)
		case op == proto.OpGateWaive && (mcp || ci):
			t.Errorf("%s is open to scope mcp or ci; a waive is a person's", op)
		case strings.HasPrefix(op, "gate.") && op != proto.OpGateAdd && mcp:
			t.Errorf("%s is open to scope mcp; an agent adds and lists gates, and never decides one (decision 68)", op)
		case strings.HasPrefix(op, "lock.") && mcp:
			t.Errorf("%s is open to scope mcp; locks are taken over the CLI only (decision 70)", op)
		}
	}
}

// signedInWith is the sign-in a table's connection presents: none, a
// key's connection, when byKey is set, and otherwise one holding
// scopes, nil included.
func signedInWith(byKey bool, scopes []string) *signedIn {
	if byKey {
		return nil
	}
	return &signedIn{scopes: scopes}
}

func TestMayCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		byKey  bool
		scopes []string
		op     string
		ok     bool
	}{
		{"a key's connection, any op", true, nil, proto.OpPrincipalAdd, true},
		{"cli, an admin op", false, []string{proto.ScopeCLI}, proto.OpPrincipalAdd, true},
		{"cli and mcp, a gate pass", false, []string{proto.ScopeCLI, proto.ScopeMCP}, proto.OpGatePass, true},
		{"mcp, a tool's op", false, []string{proto.ScopeMCP}, proto.OpReady, true},
		{"mcp, a gate pass", false, []string{proto.ScopeMCP}, proto.OpGatePass, false},
		{"mcp, an admin op", false, []string{proto.ScopeMCP}, proto.OpKeyAdd, false},
		{"mcp, a lock", false, []string{proto.ScopeMCP}, proto.OpLockAcquire, false},
		{"ci, a report", false, []string{proto.ScopeCI}, proto.OpGatePass, true},
		{"ci, a waive", false, []string{proto.ScopeCI}, proto.OpGateWaive, false},
		{"ci, an admin op", false, []string{proto.ScopeCI}, proto.OpStatus, false},
		{"no scope at all", false, []string{}, proto.OpShow, false},
		{"a sign-in with nil scopes, an admin op", false, nil, proto.OpPrincipalAdd, false},
		{"an op no table names", false, []string{proto.ScopeMCP}, "made.up", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := mayCall(signedInWith(tc.byKey, tc.scopes), tc.op)
			if (e == nil) != tc.ok {
				t.Fatalf("mayCall(by key %t, scopes %#v, %s) = %v, want ok %t", tc.byKey, tc.scopes, tc.op, e, tc.ok)
			}
			if e != nil && (e.Code != proto.CodeForbidden || e.Fix == "" || !strings.Contains(e.Message, tc.op)) {
				t.Errorf("mayCall(by key %t, scopes %#v, %s) = %+v, want forbidden, naming the op, with a fix", tc.byKey, tc.scopes, tc.op, e)
			}
		})
	}
}

func TestMayTakeSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		byKey   bool
		scopes  []string
		session string
		ok      bool
	}{
		{"a key's connection, cli", true, nil, proto.CLISession, true},
		{"cli, cli", false, []string{proto.ScopeCLI}, proto.CLISession, true},
		{"mcp, cli", false, []string{proto.ScopeMCP}, proto.CLISession, false},
		{"mcp, an agent's", false, []string{proto.ScopeMCP}, "claude-0000", true},
		{"ci, ci", false, []string{proto.ScopeCI}, proto.CISession, true},
		{"ci, cli", false, []string{proto.ScopeCI}, proto.CLISession, false},
		{"mcp, ci", false, []string{proto.ScopeMCP}, proto.CISession, false},
		{"cli, ci", false, []string{proto.ScopeCLI}, proto.CISession, true},
		{"a sign-in with nil scopes, cli", false, nil, proto.CLISession, false},
		{"a sign-in with nil scopes, ci", false, nil, proto.CISession, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if e := mayTakeSession(signedInWith(tc.byKey, tc.scopes), tc.session); (e == nil) != tc.ok {
				t.Errorf("mayTakeSession(by key %t, scopes %#v, %s) = %v, want ok %t", tc.byKey, tc.scopes, tc.session, e, tc.ok)
			}
		})
	}
}
