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
	for _, op := range ops {
		if _, ok := opScopes()[op]; !ok {
			t.Errorf("op %q has no entry in opScopes; add it, with the scopes besides cli whose tokens may send it", op)
		}
	}
	for op := range opScopes() {
		if !slices.Contains(ops, op) {
			t.Errorf("opScopes names %q, which is no op of internal/proto", op)
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

func TestMayCall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		scopes []string
		op     string
		ok     bool
	}{
		{"a key's connection, any op", nil, proto.OpPrincipalAdd, true},
		{"cli, an admin op", []string{proto.ScopeCLI}, proto.OpPrincipalAdd, true},
		{"cli and mcp, a gate pass", []string{proto.ScopeCLI, proto.ScopeMCP}, proto.OpGatePass, true},
		{"mcp, a tool's op", []string{proto.ScopeMCP}, proto.OpReady, true},
		{"mcp, a gate pass", []string{proto.ScopeMCP}, proto.OpGatePass, false},
		{"mcp, an admin op", []string{proto.ScopeMCP}, proto.OpKeyAdd, false},
		{"mcp, a lock", []string{proto.ScopeMCP}, proto.OpLockAcquire, false},
		{"ci, a report", []string{proto.ScopeCI}, proto.OpGatePass, true},
		{"ci, a waive", []string{proto.ScopeCI}, proto.OpGateWaive, false},
		{"ci, an admin op", []string{proto.ScopeCI}, proto.OpStatus, false},
		{"no scope at all", []string{}, proto.OpShow, false},
		{"an op no table names", []string{proto.ScopeMCP}, "made.up", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := mayCall(tc.scopes, tc.op)
			if (e == nil) != tc.ok {
				t.Fatalf("mayCall(%v, %s) = %v, want ok %t", tc.scopes, tc.op, e, tc.ok)
			}
			if e != nil && (e.Code != proto.CodeForbidden || e.Fix == "" || !strings.Contains(e.Message, tc.op)) {
				t.Errorf("mayCall(%v, %s) = %+v, want forbidden, naming the op, with a fix", tc.scopes, tc.op, e)
			}
		})
	}
}

func TestMayTakeSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		scopes  []string
		session string
		ok      bool
	}{
		{"a key's connection, cli", nil, proto.CLISession, true},
		{"cli, cli", []string{proto.ScopeCLI}, proto.CLISession, true},
		{"mcp, cli", []string{proto.ScopeMCP}, proto.CLISession, false},
		{"mcp, an agent's", []string{proto.ScopeMCP}, "claude-0000", true},
		{"ci, ci", []string{proto.ScopeCI}, proto.CISession, true},
		{"ci, cli", []string{proto.ScopeCI}, proto.CLISession, false},
		{"mcp, ci", []string{proto.ScopeMCP}, proto.CISession, false},
		{"cli, ci", []string{proto.ScopeCLI}, proto.CISession, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if e := mayTakeSession(tc.scopes, tc.session); (e == nil) != tc.ok {
				t.Errorf("mayTakeSession(%v, %s) = %v, want ok %t", tc.scopes, tc.session, e, tc.ok)
			}
		})
	}
}
