package logs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// root is the module's root, from this package's directory.
const root = "../.."

// logged are the directories whose code writes starfixd's log.
var logged = []string{"cmd/starfixd", "internal/server", "internal/store", "internal/telemetry", "internal/sdnotify", "internal/doltmetrics"}

// parseDir parses the Go files of dir that are not tests.
func parseDir(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	var files []*ast.File
	err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != filepath.Join(root, dir) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no Go file in %s: the test reads nothing", dir)
	}
	return files
}

// declared returns the constants of this package that are an Event, by
// name, and those that are a field's key, as the source declares them.
func declared(t *testing.T) (evs map[string]Event, keyConsts map[string]string) {
	t.Helper()
	evs, keyConsts = map[string]Event{}, map[string]string{}
	for _, f := range parseDir(t, token.NewFileSet(), "internal/logs") {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, _ := strconv.Unquote(lit.Value)
					if id, ok := vs.Type.(*ast.Ident); ok && id.Name == "Event" {
						evs[n.Name] = Event(v)
					} else if strings.HasPrefix(n.Name, "Key") {
						keyConsts[n.Name] = v
					}
				}
			}
		}
	}
	return evs, keyConsts
}

// The list is the contract: every Event constant is in Events, once,
// and every key constant is a field with a kind.
func TestEventsListsEveryConstant(t *testing.T) {
	evs, keyConsts := declared(t)
	if len(evs) < 20 || len(keyConsts) < 10 {
		t.Fatalf("found %d event and %d key constants: the test reads the source wrongly", len(evs), len(keyConsts))
	}
	shape := regexp.MustCompile(`^[a-z][a-z_]*(\.[a-z][a-z_]*)+$`)
	seen := map[Event]string{}
	for name, ev := range evs {
		if !IsEvent(string(ev)) {
			t.Errorf("the constant %s (%q) is not in Events(): add it to the list, and to docs/server.md", name, ev)
		}
		if !shape.MatchString(string(ev)) {
			t.Errorf("the event %q of %s is not lowercase words joined by dots", ev, name)
		}
		if other, dup := seen[ev]; dup {
			t.Errorf("%s and %s are both %q", name, other, ev)
		}
		seen[ev] = name
	}
	if got := len(Events()); got != len(evs) {
		t.Errorf("Events() lists %d events and the source declares %d", got, len(evs))
	}
	for name, k := range keyConsts {
		if !IsKey(k) {
			t.Errorf("the constant %s (%q) is not a field in keys", name, k)
		}
	}
	if !slices.IsSorted(Keys()) || len(Keys()) < len(keyConsts) {
		t.Errorf("Keys() = %v, want every field, sorted", Keys())
	}
}

func TestAudit(t *testing.T) {
	for _, tc := range []struct {
		op   string
		want Event
		ok   bool
	}{
		{"project.add", AuditProjectAdd, true},
		{"project.archive", AuditProjectArchive, true},
		{"project.activate", AuditProjectActivate, true},
		{"principal.add", AuditPrincipalAdd, true},
		{"principal.disable", AuditPrincipalDisable, true},
		{"principal.enable", AuditPrincipalEnable, true},
		{"key.add", AuditKeyAdd, true},
		{"key.remove", AuditKeyRemove, true},
		{"member.set", AuditMemberSet, true},
		{"member.remove", AuditMemberRemove, true},
		{"clock.step", "", false},
		{"read_failed", "", false},
		{"issue.create", "", false},
		{"", "", false},
	} {
		if got, ok := Audit(tc.op); got != tc.want || ok != tc.ok {
			t.Errorf("Audit(%q) = %q, %t, want %q, %t", tc.op, got, ok, tc.want, tc.ok)
		}
	}
}

func TestKindOf(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want Kind
	}{
		{KeyPrincipal, KindPrincipal}, {KeyBy, KindPrincipal}, {KeySubject, KindPrincipal}, {KeySession, KindSession},
		{KeyErr, KindLocal}, {KeyTarget, KindLocal}, {"machine", KindLocal}, {"no such field", KindLocal},
		{KeyProject, KindProject}, {KeyOp, KindOp}, {KeyCode, KindCode}, {KeyReq, KindReq}, {KeyEvent, KindWord}, {KeyTraceID, KindTrace},
	} {
		if got := KindOf(tc.key); got != tc.want {
			t.Errorf("KindOf(%q) = %d, want %d", tc.key, got, tc.want)
		}
	}
}

// slogCalls are the methods of *slog.Logger that write a line, with the
// index of the message among their arguments.
var slogCalls = map[string]int{
	"Debug": 0, "Info": 0, "Warn": 0, "Error": 0,
	"DebugContext": 1, "InfoContext": 1, "WarnContext": 1, "ErrorContext": 1,
	"Log": 2,
}

// wrappers are the daemon's own functions that write a line for their
// caller, with the index of the event and of the message among their
// arguments. Inside one, the line is written from its parameters.
var wrappers = map[string]struct{ ev, msg int }{
	"line":   {5, 6}, // (*sampler).line in internal/server
	"failed": {0, 1}, // (*errlog).failed in internal/telemetry
}

// Every line the daemon logs names an event of the list, as its first
// field, under a message that is fixed text, and every field it names
// is one of the schema's (design §16.4).
func TestLogCallsFollowTheSchema(t *testing.T) {
	evs, keyConsts := declared(t)
	fset := token.NewFileSet()
	calls := 0
	for _, dir := range logged {
		for _, f := range parseDir(t, fset, dir) {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				_, inWrapper := wrappers[fn.Name.Name]
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					at := fset.Position(call.Pos())
					isEvent := func(e ast.Expr) bool {
						s, ok := e.(*ast.SelectorExpr)
						if !ok {
							return false
						}
						pkg, ok := s.X.(*ast.Ident)
						_, known := evs[s.Sel.Name]
						return ok && pkg.Name == "logs" && known
					}
					isText := func(e ast.Expr) bool {
						lit, ok := e.(*ast.BasicLit)
						return ok && lit.Kind == token.STRING
					}
					if w, ok := wrappers[sel.Sel.Name]; ok && len(call.Args) > w.msg {
						calls++
						if !isEvent(call.Args[w.ev]) {
							t.Errorf("%s: %s is not given an event of internal/logs", at, sel.Sel.Name)
						}
						if !isText(call.Args[w.msg]) {
							t.Errorf("%s: the message given to %s is not a string literal", at, sel.Sel.Name)
						}
						return true
					}
					msg, ok := slogCalls[sel.Sel.Name]
					if !ok || len(call.Args) <= msg || inWrapper {
						return true
					}
					calls++
					args := call.Args[msg:]
					if !isText(args[0]) {
						t.Errorf("%s: the message of %s is not a string literal: put what varies in a field", at, sel.Sel.Name)
					}
					if len(args) == 2 && call.Ellipsis.IsValid() {
						return true // a slice of fields, event first: the tests of the lines themselves check it
					}
					if len(args) < 3 || !isText(args[1]) || args[1].(*ast.BasicLit).Value != `"event"` || !isEvent(args[2]) {
						t.Errorf(`%s: %s does not name its event first: want msg, "event", logs.NAME, …`, at, sel.Sel.Name)
						return true
					}
					for i := 3; i < len(args); i += 2 {
						if call.Ellipsis.IsValid() && i == len(args)-1 {
							break // a slice of fields, which the tests of the lines themselves check
						}
						var key string
						switch k := args[i].(type) {
						case *ast.BasicLit:
							key, _ = strconv.Unquote(k.Value)
						case *ast.SelectorExpr:
							key = keyConsts[k.Sel.Name]
						}
						if !IsKey(key) {
							t.Errorf("%s: field %d of %s is not a field of internal/logs (schema.go)", at, (i-1)/2, sel.Sel.Name)
						}
					}
					return true
				})
			}
		}
	}
	if calls < 60 {
		t.Errorf("found %d log calls in %v: the test reads the source wrongly", calls, logged)
	}
}

// The guide lists every event and every field (rule 17).
func TestGuideListsEventsAndFields(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(root, "docs", "server.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, ev := range Events() {
		if !strings.Contains(doc, "`"+string(ev)+"`") {
			t.Errorf("docs/server.md does not list the event `%s`", ev)
		}
	}
	for _, k := range Keys() {
		if !strings.Contains(doc, "`"+k+"`") {
			t.Errorf("docs/server.md does not list the field `%s`", k)
		}
	}
}
