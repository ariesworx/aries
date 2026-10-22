package merges

import (
	"os"
	"strings"
	"testing"
)

func TestLoadRefuses(t *testing.T) {
	t.Parallel()
	const cls = "classes:\n  a:\n    checks: green\n    paths:\n      allow: [\"docs/**\"]\n"
	for _, tt := range []struct{ name, doc, want string }{
		{"no version", cls, "no version"},
		{"a later version", "version: 2\n" + cls, "version 2: this aries reads version 1"},
		{"no classes", "version: 1\n", "no classes"},
		{"an unknown key", "version: 1\nqueue: all\n" + cls, `unknown key "queue"`},
		{"an unknown class key", "version: 1\nclasses:\n  a:\n    checks: green\n    merge: yes\n    paths:\n      allow: [x]\n", `unknown key "merge"`},
		{"no checks", "version: 1\nclasses:\n  a:\n    paths:\n      allow: [x]\n", "no checks"},
		{"checks not green", "version: 1\nclasses:\n  a:\n    checks: any\n    paths:\n      allow: [x]\n", `checks: "any"`},
		{"an unknown epic", "version: 1\nclasses:\n  a:\n    checks: green\n    epic: maybe\n    paths:\n      allow: [x]\n", `epic: "maybe"`},
		{"an unknown tests", "version: 1\nclasses:\n  a:\n    checks: green\n    tests: some\n    paths:\n      allow: [x]\n", `tests: "some"`},
		{"an empty opener", "version: 1\nclasses:\n  a:\n    checks: green\n    opener: {}\n    paths:\n      allow: [x]\n", "opener: name a harness"},
		{"no paths", "version: 1\nclasses:\n  a:\n    checks: green\n", "no paths"},
		{"except alone", "version: 1\nclasses:\n  a:\n    checks: green\n    paths:\n      except: [x]\n", "paths: name allow or deny"},
		{"a bad pattern", "version: 1\nclasses:\n  a:\n    checks: green\n    paths:\n      deny: [\"/etc/**\"]\n", "leading /"},
		{"a half **", "version: 1\nclasses:\n  a:\n    checks: green\n    paths:\n      deny: [\"a**/b\"]\n", "whole segment"},
		{"a bad name", "version: 1\nclasses:\n  A_B:\n    checks: green\n    paths:\n      allow: [x]\n", "a class's name"},
		{"an unknown hold", "version: 1\nholds: none\n" + cls, `holds: "none"`},
		{"a bad floor", "version: 1\nfloor: [\"../x\"]\n" + cls, "floor 1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load: %v, want %q", err, tt.want)
			}
		})
	}
}

// TestRepositoryFile: this repository's rule loads, and holds what the
// memories `dispatch` (rule 4) and `merges` hold.
func TestRepositoryFile(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../" + File)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Load(b)
	if err != nil {
		t.Fatalf("%s: %v", File, err)
	}
	for _, tt := range []struct {
		path string
		want string
	}{
		{"docs/cli.md", "queue docs-and-tests"},
		{"internal/cli/cli_test.go", "queue docs-and-tests"},
		{"internal/cli/testdata/x.txt", "queue docs-and-tests"},
		{"docs/design/merges.md", "hold"},
		{"docs/server.md", "hold"},
		{"docs/security-model.md", "hold"},
		{"internal/cli/testdata/help.golden", "hold"},
		{"internal/telemetry/names_test.go", "hold"},
		{"scripts/docs_test.go", "hold"},
		{"internal/release/release_test.go", "hold floor"},
		{"internal/tools/cla/cla_test.go", "hold"},
		{"deploy/docker/x_test.go", "hold"},
		{"internal/cli/cli.go", "hold"},
		{"README.md", "hold"},
	} {
		pr := good()
		pr.Files = []Change{{Path: tt.path}}
		if got := Classify(r, Context{Base: "main"}, pr).String(); !strings.HasPrefix(got, tt.want) {
			t.Errorf("%s: %s, want %s…", tt.path, got, tt.want)
		}
	}
	// The epic class names each of the memory `merges`'s areas.
	for _, p := range []string{"internal/proto/ops.go", "internal/store/migrations/0002_x.sql", "internal/store/controlmigrations/0002_x.sql",
		"internal/keyproof/x.go", "internal/dpop/x.go", "internal/jose/x.go", "internal/oidc/x.go", "internal/credential/x.go",
		"internal/server/signin.go", "internal/server/exchange.go", "internal/server/trust.go", "internal/server/https.go",
		"internal/secretscan/x.go", "internal/safetext/x.go", "internal/dispatch/claude.go"} {
		pr := good()
		pr.Files = []Change{{Path: p}}
		got := Classify(r, Context{Base: "main"}, pr).String()
		if !strings.Contains(got, "approved-epic paths: "+p+" (denied)") {
			t.Errorf("%s: %s, want the epic class to deny it", p, got)
		}
	}
}
