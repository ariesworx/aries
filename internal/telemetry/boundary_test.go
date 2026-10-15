package telemetry

import (
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// The SDK is starfixd's alone (design §16.2, decision 28). These tests
// ask the go tool, which `go test` puts on PATH, what the module's
// packages import.

const (
	module = "github.com/ariesworx/aries"
	// here is the one package that may import the SDK.
	here = module + "/internal/telemetry"
)

// sdkPrefixes are the modules the SDK brings: OpenTelemetry's own, the
// Prometheus client its pull exporter is built on, and its logging
// interface.
var sdkPrefixes = []string{"go.opentelemetry.io/", "github.com/prometheus/", "github.com/go-logr/"}

func isSDK(pkg string) bool {
	for _, p := range sdkPrefixes {
		if strings.HasPrefix(pkg, p) {
			return true
		}
	}
	return false
}

// pkg is what `go list -json` says of a package.
type pkg struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
}

// goList runs `go list -json args...` at the module's root, for goos.
func goList(t *testing.T, goos string, args ...string) []pkg {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", append([]string{"list", "-json=ImportPath,Imports,TestImports,XTestImports"}, args...)...) //nolint:gosec // fixed arguments
	cmd.Dir = "../.."
	cmd.Env = append(cmd.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %s (GOOS=%s): %v\n%s", strings.Join(args, " "), goos, err, stderr.String())
	}
	var pkgs []pkg
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for {
		var p pkg
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("go list: decode: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatalf("go list %s listed no package", strings.Join(args, " "))
	}
	return pkgs
}

// sfx, on every system it is built for, links nothing of the SDK: not
// directly, and not through any package it imports.
func TestSfxLinksNoSDK(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			deps := goList(t, goos, "-deps", "./cmd/sfx")
			found := false
			for _, p := range deps {
				if p.ImportPath == module+"/cmd/sfx" {
					found = true
				}
				if isSDK(p.ImportPath) {
					t.Errorf("cmd/sfx links %s: the OpenTelemetry SDK is for starfixd only (docs/design/starfix.md §16.2)", p.ImportPath)
				}
				if p.ImportPath == here {
					t.Errorf("cmd/sfx links %s, which imports the SDK", here)
				}
			}
			if !found {
				t.Fatalf("go list -deps ./cmd/sfx did not list cmd/sfx among %d packages: the test reads nothing", len(deps))
			}
		})
	}
}

// starfixd does link it: the test above would pass for a tool that
// listed nothing.
func TestStarfixdLinksTheSDK(t *testing.T) {
	t.Parallel()
	for _, p := range goList(t, "linux", "-deps", "./cmd/starfixd") {
		if isSDK(p.ImportPath) {
			return
		}
	}
	t.Errorf("cmd/starfixd links no package of the SDK: isSDK no longer matches what the SDK is")
}

// Only this package imports the SDK directly, in code or in tests, so
// that it can be replaced in one place.
func TestOnlyTelemetryImportsTheSDK(t *testing.T) {
	t.Parallel()
	seen := false
	for _, goos := range []string{"linux", "darwin", "windows"} {
		for _, p := range goList(t, goos, "./...") {
			if p.ImportPath == here {
				seen = true
				continue
			}
			for _, kind := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
				for _, imp := range kind {
					if isSDK(imp) {
						t.Errorf("%s imports %s (GOOS=%s): only internal/telemetry may import the SDK; use its interface", p.ImportPath, imp, goos)
					}
				}
			}
		}
	}
	if !seen {
		t.Fatalf("go list ./... did not list %s", here)
	}
}
