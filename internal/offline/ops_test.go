package offline

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/outbox"
	"github.com/ariesworx/aries/internal/proto"
)

// protoOps reads every op the protocol names, by the name of its
// constant: the string constants of internal/proto whose names begin
// with Op, as internal/server's scopes test reads them.
func protoOps(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "proto", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]string{}
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
					ops[name.Name], _ = strconv.Unquote(lit.Value)
				}
			}
		}
	}
	if len(ops) < 50 {
		t.Fatalf("found %d ops in internal/proto, fewer than the protocol has; the parse is wrong", len(ops))
	}
	return ops
}

// serverReads reads the ops the server counts as writing nothing, its
// readOps (internal/server/limits.go), by parsing it: ops names each
// constant's op. Every op it does not name is a write.
func serverReads(t *testing.T, ops map[string]string) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "server", "limits.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	reads := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "readOps" || len(vs.Values) != 1 {
			return true
		}
		lit, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatalf("readOps in %s is no composite literal; change serverReads to read it", path)
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			sel, ok2 := kv.Key.(*ast.SelectorExpr)
			if !ok || !ok2 {
				t.Fatalf("readOps in %s has a key that is no proto.Op constant; change serverReads to read it", path)
			}
			op, ok := ops[sel.Sel.Name]
			if !ok {
				t.Fatalf("readOps in %s names %s, which is no op of internal/proto", path, sel.Sel.Name)
			}
			reads[op] = true
		}
		return false
	})
	if len(reads) < 20 {
		t.Fatalf("found %d reads in %s's readOps, fewer than the server has; the parse is wrong", len(reads), path)
	}
	return reads
}

// What becomes of an op while the server cannot be reached: a write is
// queued, or never queued, with why (internal/outbox); a read is
// answered from this machine's copy, refused as what others are doing
// now, or not kept, with why (this package).
const (
	ruleQueued   = "queued"
	ruleNever    = "never queued"
	ruleAnswered = "answered from the cache"
	ruleRefused  = "refused offline"
	ruleUnkept   = "not kept"
)

// offlineRules returns every rule that names op.
func offlineRules(op string) []string {
	var rules []string
	if outbox.Queueable(op) {
		rules = append(rules, ruleQueued)
	}
	if why, ok := outbox.Unqueued(op); ok && why != "" {
		rules = append(rules, ruleNever)
	}
	if AnsweredOffline(op) {
		rules = append(rules, ruleAnswered)
	}
	if RefusedOffline(op) {
		rules = append(rules, ruleRefused)
	}
	if unkept(op) != "" {
		rules = append(rules, ruleUnkept)
	}
	return rules
}

// unruled returns what is wrong with the offline rules of ops, given
// which of them the server counts as reads: each op has exactly one
// rule, a write's a write's and a read's a read's.
func unruled(ops []string, reads map[string]bool) []string {
	var out []string
	for _, op := range ops {
		rules := offlineRules(op)
		switch {
		case len(rules) == 0 && reads[op]:
			out = append(out, "read "+strconv.Quote(op)+" has no offline rule; add it to AnsweredOffline, RefusedOffline or unkept with why (internal/offline/cache.go)")
		case len(rules) == 0:
			out = append(out, "write "+strconv.Quote(op)+" has no offline rule; add it to queued, or to Unqueued with why (internal/outbox/entry.go)")
		case len(rules) > 1:
			out = append(out, "op "+strconv.Quote(op)+" has rules "+strings.Join(rules, ", ")+"; give it one")
		case reads[op] && (rules[0] == ruleQueued || rules[0] == ruleNever):
			out = append(out, "read "+strconv.Quote(op)+" is "+rules[0]+", a write's rule; the server counts it as a read")
		case !reads[op] && rules[0] != ruleQueued && rules[0] != ruleNever:
			out = append(out, "write "+strconv.Quote(op)+" is "+rules[0]+", a read's rule; the server counts it as a write")
		}
	}
	return out
}

// TestEveryOpHasAnOfflineRule holds the offline lists to the protocol
// (design §18.3, §18.6): every write of internal/proto is queued or
// never queued with why, and every read is answered from the cache,
// refused offline or not kept with why, so that a new op is decided
// rather than silently not queued and not kept.
func TestEveryOpHasAnOfflineRule(t *testing.T) {
	t.Parallel()
	byName := protoOps(t)
	ops := slices.Sorted(maps.Values(byName))
	for _, msg := range unruled(ops, serverReads(t, byName)) {
		t.Error(msg)
	}
}

// TestEveryOpHasAnOfflineRuleFails: the check above reports an op added
// to the protocol with no rule, read or write, and only that one, and
// the parses it reads by find what they should.
func TestEveryOpHasAnOfflineRuleFails(t *testing.T) {
	t.Parallel()
	byName := protoOps(t)
	ops := slices.Sorted(maps.Values(byName))
	reads := serverReads(t, byName)
	for _, tc := range []struct {
		name  string
		reads map[string]bool
		want  string
	}{
		{"a write", reads, `write "made.up" has no offline rule`},
		{"a read", map[string]bool{"made.up": true}, `read "made.up" has no offline rule`},
	} {
		all := maps.Clone(reads)
		maps.Copy(all, tc.reads)
		got := unruled(append(slices.Clone(ops), "made.up"), all)
		if len(got) != 1 || !strings.HasPrefix(got[0], tc.want) {
			t.Errorf("%s: unruled with made.up added = %q, want only %q", tc.name, got, tc.want)
		}
	}
	for _, op := range []string{proto.OpCreate, proto.OpBrief, proto.OpWatch, proto.OpTrustRemove} {
		if !slices.Contains(ops, op) {
			t.Errorf("protoOps misses %q", op)
		}
	}
	for _, op := range []string{proto.OpShow, proto.OpWatch, proto.OpTrustShow} {
		if !reads[op] {
			t.Errorf("serverReads misses %q", op)
		}
	}
	for _, op := range []string{proto.OpCreate, proto.OpRenew, proto.OpBrief} {
		if reads[op] {
			t.Errorf("serverReads has write %q", op)
		}
	}
}
