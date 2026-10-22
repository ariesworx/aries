package merges

import (
	"strings"
	"testing"
)

// testRules is the shape of this repository's file: rule 4's class and
// the memory `merges`'s.
const testRules = `version: 1
classes:
  docs-and-tests:
    checks: green
    paths:
      allow: ["docs/**/*.md", "**/*_test.go", "**/testdata/**"]
      except: ["docs/design/**", "**/*.golden", "scripts/**"]
      deny: ["internal/tools/**"]
    tests: no-deletions
  approved-epic:
    checks: green
    epic: approved
    opener: {harness: [claude-code-desktop], started_by: [alice]}
    paths:
      deny: ["internal/proto/**"]
holds: draft
`

// epicOnly passes a pull request on its checks, its epic and its paths
// alone, with no opener condition, so that each of those can be held and
// released in turn.
const epicOnly = `version: 1
classes:
  epic:
    checks: green
    epic: approved
    paths:
      deny: ["internal/proto/**"]
`

func load(t *testing.T, s string) *Rules {
	t.Helper()
	r, err := Load([]byte(s))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

// good is a pull request every condition passes: green, open, on main,
// its issue under an approved epic, touching one guide.
func good() PR {
	return PR{
		Number: 7, Base: "main", Branch: "docs/ar-a1b2c3d4-guide", Head: "abc",
		Checks: []Check{{Name: "lint", Bucket: "pass"}, {Name: "test", Bucket: "pass"}},
		Files:  []Change{{Path: "docs/guide.md"}},
		Issue:  Issue{ID: "ar-a1b2c3d4", Epic: Epic{Approved: true, Detail: "ar-e1e2e3e4"}},
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		rules string
		edit  func(*PR)
		// want is "queue CLASS" or "hold" with the reasons' text, each
		// "class condition: detail", joined by "; ".
		want string
	}{
		{"docs pass rule 4", testRules, nil, "queue docs-and-tests"},
		{"a test passes rule 4", testRules, func(p *PR) { p.Files = []Change{{Path: "internal/cli/x_test.go"}} }, "queue docs-and-tests"},

		// Checks: each state of a required check, held and released.
		{"a failing check", testRules, func(p *PR) { p.Checks[1].Bucket = "fail" },
			"hold approved-epic checks: test fail; docs-and-tests checks: test fail"},
		{"a pending check", epicOnly, func(p *PR) { p.Checks[0].Bucket = "pending" }, "hold epic checks: lint pending"},
		{"a skipped check is not green", epicOnly, func(p *PR) { p.Checks[0].Bucket = "skipping" }, "hold epic checks: lint skipping"},
		{"no required checks", epicOnly, func(p *PR) { p.Checks = nil }, "hold epic checks: no required check reported"},
		{"checks unreadable", epicOnly, func(p *PR) { p.ChecksErr = "gh pr checks: timeout" }, "hold epic checks: unreadable: gh pr checks: timeout"},
		{"checks released", epicOnly, func(p *PR) { p.Files = []Change{{Path: "internal/cli/cli.go"}} }, "queue epic"},

		// Epic: held and released.
		{"no approved epic", epicOnly, func(p *PR) { p.Issue.Epic = Epic{Detail: "epic ar-e1e2e3e4 has no passed approval gate"} },
			"hold epic epic: epic ar-e1e2e3e4 has no passed approval gate"},
		{"no issue", epicOnly, func(p *PR) { p.Issue = Issue{} }, "hold epic epic: no issue: the branch names none"},
		{"issue unreadable", epicOnly, func(p *PR) { p.Issue = Issue{ID: "ar-a1b2c3d4", Err: "not found"} },
			"hold epic epic: issue ar-a1b2c3d4 unreadable: not found"},

		// Paths: allow, except and deny, held and released.
		{"a path outside allow", testRules, func(p *PR) { p.Files = []Change{{Path: "internal/cli/cli.go"}} },
			"hold approved-epic opener: unverified (no brief: comment); docs-and-tests paths: internal/cli/cli.go (not allowed)"},
		{"a path in except", testRules, func(p *PR) { p.Files = []Change{{Path: "docs/design/x.md"}} },
			"hold approved-epic opener: unverified (no brief: comment); docs-and-tests paths: docs/design/x.md (not allowed)"},
		{"a golden in except", testRules, func(p *PR) { p.Files = []Change{{Path: "internal/cli/testdata/help.golden"}} },
			"hold approved-epic opener: unverified (no brief: comment); docs-and-tests paths: internal/cli/testdata/help.golden (not allowed)"},
		{"a path in deny", testRules, func(p *PR) { p.Files = []Change{{Path: "internal/tools/x_test.go"}} },
			"hold approved-epic opener: unverified (no brief: comment); docs-and-tests paths: internal/tools/x_test.go (denied)"},
		{"deny holds the epic class", epicOnly, func(p *PR) { p.Files = []Change{{Path: "internal/proto/ops.go"}} },
			"hold epic paths: internal/proto/ops.go (denied)"},
		{"a rename's old side counts", epicOnly, func(p *PR) { p.Files = []Change{{Path: "internal/x/ops.go", Old: "internal/proto/ops.go"}} },
			"hold epic paths: internal/proto/ops.go (denied)"},
		{"no paths read", epicOnly, func(p *PR) { p.Files = nil }, "hold epic paths: no changed path read"},
		{"paths unreadable", epicOnly, func(p *PR) { p.FilesErr = "diff too large" }, "hold epic paths: unreadable: diff too large"},
		{"a path it cannot classify", epicOnly, func(p *PR) { p.Files = []Change{{Path: "../etc/passwd"}} },
			`hold paths: "../etc/passwd" cannot be classified`},

		// Tests: no-deletions.
		{"a test loses lines", testRules, func(p *PR) { p.Files = []Change{{Path: "a/x_test.go", Removed: 3}} },
			"hold approved-epic opener: unverified (no brief: comment); docs-and-tests tests: a/x_test.go loses 3 lines"},
		{"a test gains t.Skip", testRules, func(p *PR) { p.Files = []Change{{Path: "a/x_test.go", Skips: 1}} },
			"hold approved-epic opener: unverified (no brief: comment); docs-and-tests tests: a/x_test.go adds t.Skip"},

		// Draft and base.
		{"a draft", testRules, func(p *PR) { p.Draft = true }, "hold draft: a draft is a hold"},
		{"a stacked pull request", testRules, func(p *PR) { p.Base = "feature/x" }, "hold base: feature/x, not main"},

		// The opener never satisfies a condition, whatever the brief says.
		{"an opener the brief names", testRules, func(p *PR) {
			p.Files = []Change{{Path: "internal/cli/cli.go"}}
			p.Issue.Brief = "harness=claude-code-desktop session=local_1"
		}, "hold approved-epic opener: unverified (brief harness=claude-code-desktop session=local_1); docs-and-tests paths: internal/cli/cli.go (not allowed)"},

		// Unreadable.
		{"an unreadable pull request", testRules, func(p *PR) { p.Err = "gh pr list: no such field" },
			"hold read: gh pr list: no such field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pr := good()
			if tt.edit != nil {
				tt.edit(&pr)
			}
			v := Classify(load(t, tt.rules), Context{Base: "main"}, pr)
			if got := v.String(); got != tt.want {
				t.Errorf("verdict\n got: %s\nwant: %s", got, tt.want)
			}
			if v.Opener.Verified {
				t.Error("an opener was verified")
			}
		})
	}
}

// TestFloorHoldsWhateverTheFile: every path of every floor area is held
// by a rule whose one class allows everything.
func TestFloorHoldsWhateverTheFile(t *testing.T) {
	t.Parallel()
	open := load(t, "version: 1\nclasses:\n  all:\n    checks: green\n    paths:\n      allow: [\"**\"]\n")
	examples := map[string]string{
		"install*.sh":            "install-server.sh",
		".claude/settings*.json": ".claude/settings.local.json",
		"**/go.mod":              "internal/tools/pin/go.mod",
		"**/go.sum":              "go.sum",
	}
	for _, a := range Floor {
		for _, p := range a.Paths {
			rel := examples[p]
			if rel == "" {
				rel = strings.ReplaceAll(p, "**", "x")
			}
			pr := good()
			pr.Files = []Change{{Path: "docs/ok.md"}, {Path: rel}}
			v := Classify(open, Context{Base: "main"}, pr)
			want := "hold floor: " + rel + " (" + a.Role + ")"
			if got := v.String(); got != want {
				t.Errorf("%s: got %s, want %s", p, got, want)
			}
		}
	}
	// With the file's own floor and the rule's own path.
	r := load(t, "version: 1\nfloor: [\"internal/keyproof/**\"]\nclasses:\n  all:\n    checks: green\n    paths:\n      allow: [\"**\"]\n")
	pr := good()
	pr.Files = []Change{{Path: "internal/keyproof/proof.go"}}
	if got, want := Classify(r, Context{Base: "main"}, pr).String(), "hold floor: internal/keyproof/proof.go (the rule's floor)"; got != want {
		t.Errorf("file floor: got %s, want %s", got, want)
	}
	pr.Files = []Change{{Path: "ops/merges.yaml"}}
	if got, want := Classify(r, Context{Base: "main", RulesPath: "ops/merges.yaml"}, pr).String(), "hold floor: ops/merges.yaml (the merge rule)"; got != want {
		t.Errorf("rules path: got %s, want %s", got, want)
	}
	pr.Files = []Change{{Path: "docs/x.md"}}
	if got := Classify(r, Context{Base: "main"}, pr).String(); got != "queue all" {
		t.Errorf("an open rule: got %s", got)
	}
}

func TestNoRule(t *testing.T) {
	t.Parallel()
	if got, want := Classify(nil, Context{Base: "main"}, good()).String(), "hold rule: no rule file (.aries/merges.yaml on the default branch, or --rules FILE)"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestOpenerAlwaysUnverified(t *testing.T) {
	t.Parallel()
	only := load(t, "version: 1\nclasses:\n  opener:\n    checks: green\n    opener: {harness: [claude-code-desktop]}\n    paths:\n      allow: [\"**\"]\n")
	for _, brief := range []string{"", "harness=claude-code-desktop session=local_1", "harness=claude-code session=s"} {
		pr := good()
		pr.Issue.Brief = brief
		v := Classify(only, Context{Base: "main"}, pr)
		if v.Queue || v.Opener.Verified {
			t.Errorf("brief %q: queued on an unverified opener: %s", brief, v)
		}
	}
}

func TestMatch(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		pat, rel string
		want     bool
	}{
		{"docs/**/*.md", "docs/a.md", true},
		{"docs/**/*.md", "docs/x/y/a.md", true},
		{"docs/**/*.md", "README.md", false},
		{"**/*_test.go", "a_test.go", true},
		{"**/*_test.go", "internal/x/a_test.go", true},
		{"**/testdata/**", "internal/x/testdata/a/b.txt", true},
		{"*.md", "docs/a.md", false},
		{"*.md", "README.md", true},
		{"internal/proto/**", "internal/proto", true},
		{"internal/proto/**", "internal/protox/a.go", false},
	} {
		if got := Match(tt.pat, tt.rel); got != tt.want {
			t.Errorf("Match(%q, %q) = %v", tt.pat, tt.rel, got)
		}
	}
}
