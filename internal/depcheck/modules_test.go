package depcheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// toolsModFile pins the tools the gate runs, in a module of its own so
// that the product's go.mod lists only what it builds with (supply-chain.md
// §7.4). Not internal/tools/go.mod, which would take releasekey and
// releaseindex out of the main module.
const toolsModFile = "internal/tools/pin/go.mod"

// golangciLint is the one tool no go.mod pins: its Action installs it by
// the version ci.yml names (supply-chain.md §11, question 2).
const golangciLint = "github.com/golangci/golangci-lint/v2"

// modFile is what `go mod edit -json` says of a go.mod.
type modFile struct {
	Require []struct {
		Path     string
		Indirect bool
	}
	Tool []struct{ Path string }
}

// direct returns the modules a go.mod requires directly: those it does
// not mark indirect, and those that provide its tools, which go marks
// indirect.
func (m modFile) direct() []string {
	var mods []string
	for _, r := range m.Require {
		if !r.Indirect {
			mods = append(mods, r.Path)
		}
	}
	for _, tool := range m.Tool {
		best := ""
		for _, r := range m.Require {
			if (tool.Path == r.Path || strings.HasPrefix(tool.Path, r.Path+"/")) && len(r.Path) > len(best) {
				best = r.Path
			}
		}
		if best != "" {
			mods = append(mods, best)
		}
	}
	return mods
}

// readMod returns what go says of the go.mod at path, from the root.
func readMod(t *testing.T, path string) modFile {
	t.Helper()
	var m modFile
	if err := json.Unmarshal(goCmd(t, nil, "mod", "edit", "-json", path), &m); err != nil {
		t.Fatalf("go mod edit -json %s: %v", path, err)
	}
	return m
}

// checkDirect holds the direct modules of go.mod (product) and of the
// tools module (tools) to deps.allow, both ways: each is listed, at a
// scope it may have, and each line names a module one of them requires.
func checkDirect(product, tools []string, allow allowlist) []error {
	var errs []error
	for _, m := range product {
		switch e, ok := allow[m]; {
		case !ok:
			errs = append(errs, fmt.Errorf("go.mod requires %s directly, and deps.allow has no line for it; a person adds one (AGENTS.md, rule 4)", m))
		case e.scope == scopeActions:
			errs = append(errs, fmt.Errorf("go.mod requires %s directly, which deps.allow scopes actions (line %d)", m, e.line))
		}
	}
	for _, m := range tools {
		switch e, ok := allow[m]; {
		case !ok:
			errs = append(errs, fmt.Errorf("%s requires %s, and deps.allow has no line for it; a person adds one (AGENTS.md, rule 4)", toolsModFile, m))
		case e.scope != scopeTools:
			errs = append(errs, fmt.Errorf("%s requires %s, which deps.allow scopes %s, not tools (line %d)", toolsModFile, m, e.scope, e.line))
		}
	}
	for _, e := range allow.sorted() {
		switch {
		case e.linked() && !slices.Contains(product, e.path):
			errs = append(errs, fmt.Errorf("deps.allow:%d lists %s, which go.mod does not require directly; remove the line", e.line, e.path))
		case e.scope == scopeTools && e.path != golangciLint && !slices.Contains(product, e.path) && !slices.Contains(tools, e.path):
			errs = append(errs, fmt.Errorf("deps.allow:%d lists %s, which neither go.mod nor %s requires directly; remove the line", e.line, e.path, toolsModFile))
		}
	}
	return errs
}

// TestDirectModulesAreAllowed fails when go.mod or the tools module
// requires a module deps.allow does not list, or deps.allow lists one
// neither requires.
func TestDirectModulesAreAllowed(t *testing.T) {
	t.Parallel()
	product := readMod(t, "go.mod").direct()
	tools := readMod(t, toolsModFile).direct()
	if len(product) == 0 || len(tools) == 0 {
		t.Fatalf("go.mod requires %d modules directly and %s %d; want some of each", len(product), toolsModFile, len(tools))
	}
	for _, err := range checkDirect(product, tools, readAllow(t)) {
		t.Error(err)
	}
}

func TestCheckDirect(t *testing.T) {
	t.Parallel()
	allow, err := parseAllow(`
example.com/lib  both  MIT  2026-10-16  linked
example.com/testonly  tools  MIT  2026-10-16  used by tests
example.com/vet  tools  MIT  2026-10-16  a pinned tool
` + golangciLint + `  tools  GPL-3.0  2026-10-16  installed by its Action
owner/action  actions  MIT  2026-10-16  an Action
`)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		product, tools []string
		want           []string // in the errors, one each; none: no error
	}{
		{name: "all listed", product: []string{"example.com/lib", "example.com/testonly"}, tools: []string{"example.com/vet"}},
		{name: "a module with no line", product: []string{"example.com/lib", "example.com/testonly", "example.com/new"}, tools: []string{"example.com/vet"},
			want: []string{"go.mod requires example.com/new directly, and deps.allow has no line for it"}},
		{name: "a tool with no line", product: []string{"example.com/lib", "example.com/testonly"}, tools: []string{"example.com/vet", "example.com/lint"},
			want: []string{toolsModFile + " requires example.com/lint, and deps.allow has no line for it"}},
		{name: "a tool scoped to a binary", product: []string{"example.com/lib", "example.com/testonly"}, tools: []string{"example.com/vet", "example.com/lib"},
			want: []string{toolsModFile + " requires example.com/lib, which deps.allow scopes both, not tools"}},
		{name: "an Action in go.mod", product: []string{"example.com/lib", "example.com/testonly", "owner/action"}, tools: []string{"example.com/vet"},
			want: []string{"go.mod requires owner/action directly, which deps.allow scopes actions"}},
		{name: "a line for a module no longer required", product: []string{"example.com/testonly"}, tools: []string{"example.com/vet"},
			want: []string{"deps.allow:2 lists example.com/lib, which go.mod does not require directly"}},
		{name: "a line for a tool no longer required", product: []string{"example.com/lib"}, tools: []string{"example.com/vet"},
			want: []string{"deps.allow:3 lists example.com/testonly, which neither go.mod nor " + toolsModFile + " requires directly"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if m := mismatch(checkDirect(tc.product, tc.tools, allow), tc.want); m != "" {
				t.Errorf("checkDirect(%q, %q) = %s", tc.product, tc.tools, m)
			}
		})
	}
}

// mismatch says how errs differs from one error containing each of
// want, in order, or returns "" when it does not.
func mismatch(errs []error, want []string) string {
	got := errors.Join(errs...)
	if len(errs) != len(want) {
		return fmt.Sprintf("%d errors, want %d:\n%v", len(errs), len(want), got)
	}
	for i, w := range want {
		if !strings.Contains(errs[i].Error(), w) {
			return fmt.Sprintf("%v\nwant error %d to contain %q", got, i+1, w)
		}
	}
	return ""
}
