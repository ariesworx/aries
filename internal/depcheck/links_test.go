package depcheck

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/links-*.golden from go list")

// targets are the systems the release builds each binary for
// (.github/workflows/release.yml). A module set can differ by system,
// so each binary's is the union over its targets, whatever system the
// test runs on.
var targets = map[string][]string{
	scopeAries:  {"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"},
	scopeAriesd: {"linux/amd64", "linux/arm64"},
}

// linkedModules returns the third-party modules ./cmd/BIN links on its
// targets, sorted: those of its packages that are not the main module's
// or the standard library's.
func linkedModules(t *testing.T, bin string) []string {
	t.Helper()
	main := strings.TrimSpace(string(goCmd(t, nil, "list", "-m")))
	var mods []string
	for _, target := range targets[bin] {
		goos, goarch, _ := strings.Cut(target, "/")
		// CGO off, as the release builds.
		out := goCmd(t, []string{"GOOS=" + goos, "GOARCH=" + goarch, "CGO_ENABLED=0"},
			"list", "-deps", "-f", "{{with .Module}}{{.Path}}{{end}}", "./cmd/"+bin)
		for m := range strings.Lines(string(out)) {
			if m = strings.TrimSpace(m); m != "" && m != main {
				mods = append(mods, m)
			}
		}
	}
	slices.Sort(mods)
	return slices.Compact(mods)
}

// checkLinks refuses a module the binary bin links whose deps.allow line
// scopes it to the other binary, or to tools or actions. A module with
// no line is indirect, and held to the golden file instead.
func checkLinks(bin string, linked []string, allow allowlist) []error {
	var errs []error
	for _, m := range linked {
		if e, ok := allow[m]; ok && !e.permits(bin) {
			errs = append(errs, fmt.Errorf("%s links %s, which deps.allow scopes %s (line %d); change the scope, in a pull request a person reviews, or stop linking it", bin, m, e.scope, e.line))
		}
	}
	return errs
}

// TestLinkSets records which third-party modules each binary links, in
// testdata/links-BIN.golden, so that a pull request that changes the set
// shows it in the diff; and refuses a module whose deps.allow scope does
// not permit the binary.
func TestLinkSets(t *testing.T) {
	t.Parallel()
	allow := readAllow(t)
	for _, bin := range []string{scopeAries, scopeAriesd} {
		t.Run(bin, func(t *testing.T) {
			t.Parallel()
			got := linkedModules(t, bin)
			for _, err := range checkLinks(bin, got, allow) {
				t.Error(err)
			}
			golden := "testdata/links-" + bin + ".golden"
			if *update {
				if err := os.WriteFile(golden, []byte(strings.Join(got, "\n")+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			b, err := os.ReadFile(golden) //nolint:gosec // this package's testdata, named by the test
			if err != nil {
				t.Fatalf("read %s: %v; run go test ./internal/depcheck -run TestLinkSets -update to make it", golden, err)
			}
			want := strings.Fields(string(b))
			for _, m := range got {
				if !slices.Contains(want, m) {
					t.Errorf("%s now links %s, which %s does not list: review what it brings, then record it with go test ./internal/depcheck -run TestLinkSets -update", bin, m, golden)
				}
			}
			for _, m := range want {
				if !slices.Contains(got, m) {
					t.Errorf("%s no longer links %s, which %s lists: record it with go test ./internal/depcheck -run TestLinkSets -update", bin, m, golden)
				}
			}
		})
	}
}

func TestCheckLinks(t *testing.T) {
	t.Parallel()
	allow, err := parseAllow(`
example.com/client  aries  MIT  2026-10-16  aries only
example.com/server  ariesd  MIT  2026-10-16  ariesd only
example.com/shared  both  MIT  2026-10-16  both
example.com/tool  tools  MIT  2026-10-16  a tool
`)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		bin    string
		linked []string
		want   []string
	}{
		{name: "in scope", bin: scopeAries, linked: []string{"example.com/client", "example.com/shared", "example.com/indirect"}},
		{name: "the other binary's", bin: scopeAries, linked: []string{"example.com/shared", "example.com/server"},
			want: []string{"aries links example.com/server, which deps.allow scopes ariesd (line 3)"}},
		{name: "the other binary's, the other way", bin: scopeAriesd, linked: []string{"example.com/client"},
			want: []string{"ariesd links example.com/client, which deps.allow scopes aries (line 2)"}},
		{name: "a tool", bin: scopeAriesd, linked: []string{"example.com/server", "example.com/tool"},
			want: []string{"ariesd links example.com/tool, which deps.allow scopes tools (line 5)"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if m := mismatch(checkLinks(tc.bin, tc.linked, allow), tc.want); m != "" {
				t.Errorf("checkLinks(%s, %q) = %s", tc.bin, tc.linked, m)
			}
		})
	}
}
