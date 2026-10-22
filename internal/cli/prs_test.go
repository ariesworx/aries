package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/merges"
	"github.com/ariesworx/aries/internal/proto"
)

func TestPrsUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"prs"}, "prs needs merges"},
		{[]string{"prs", "queue"}, `unknown prs action "queue"`},
		{[]string{"prs", "merges", "x12"}, `"x12" is not a pull request's number`},
		{[]string{"prs", "merges", "--queue"}, "flag provided but not defined: -queue"},
	} {
		code, _, stderr := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
		if code != ExitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("aries %s: exit %d, %q; want a usage error with %q", strings.Join(tc.args, " "), code, stderr, tc.want)
		}
	}
	if !strings.Contains(helpText(t, "prs"), "usage: aries "+prsUsage) {
		t.Errorf("help prs does not give its usage")
	}
}

type fakeForge struct {
	open []merges.PR
	err  error
}

func (f *fakeForge) Open(context.Context) ([]merges.PR, error) { return f.open, f.err }
func (f *fakeForge) Required(_ context.Context, n int) ([]merges.Check, error) {
	if n == 3 {
		return nil, errors.New("gh pr checks: HTTP 502")
	}
	return []merges.Check{{Name: "test", Bucket: "pass"}}, nil
}
func (f *fakeForge) Diff(context.Context, int) ([]merges.Change, error) { return nil, nil }

type fakeMergeStore struct{}

func (fakeMergeStore) Show(_ context.Context, id string) (proto.ShowResult, error) {
	return proto.ShowResult{}, errors.New("no issue " + id)
}
func (fakeMergeStore) Comments(context.Context, string) ([]proto.Comment, error) { return nil, nil }

// mergeVerdicts reads each open pull request and its issue, filters to
// the numbers asked, and holds a number that is not open.
func TestMergeVerdicts(t *testing.T) {
	t.Parallel()
	rules, err := merges.Load([]byte("version: 1\nclasses:\n  docs:\n    checks: green\n    paths:\n      allow: [\"docs/**\"]\n  epic:\n    checks: green\n    epic: approved\n    paths:\n      deny: [\"internal/proto/**\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeForge{open: []merges.PR{
		{Number: 1, Base: "main", Branch: "docs/ar-a1b2c3d4-guide", Files: []merges.Change{{Path: "docs/a.md"}}},
		{Number: 2, Base: "main", Branch: "fix/ar-b1b2c3d4-bug", Files: []merges.Change{{Path: "internal/cli/cli.go"}}},
		{Number: 3, Base: "main", Branch: "fix/no-issue", Files: []merges.Change{{Path: "docs/a.md"}}},
	}}
	ctx := context.Background()
	c := merges.Context{Base: "main"}
	vs, err := mergeVerdicts(ctx, f, fakeMergeStore{}, rules, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range vs {
		got = append(got, v.String())
	}
	want := []string{
		"queue docs",
		"hold docs paths: internal/cli/cli.go (not allowed); epic epic: issue ar-b1b2c3d4 unreadable: no issue ar-b1b2c3d4",
		"hold docs checks: unreadable: gh pr checks: HTTP 502; epic checks: unreadable: gh pr checks: HTTP 502",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("verdicts\n got: %q\nwant: %q", got, want)
	}
	vs, err = mergeVerdicts(ctx, f, fakeMergeStore{}, rules, c, []int{2, 9})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || vs[0].Number != 2 || vs[1].Number != 9 || vs[1].String() != "hold read: not an open pull request" {
		t.Errorf("filtered verdicts: %+v", vs)
	}
	if _, err := mergeVerdicts(ctx, &fakeForge{err: errors.New("gh: not signed in")}, fakeMergeStore{}, rules, c, nil); err == nil {
		t.Error("an unreadable list: no error")
	}
}

func TestPrintMerges(t *testing.T) {
	t.Parallel()
	rules, err := merges.Load([]byte("version: 1\nclasses:\n  e:\n    checks: green\n    opener: {harness: [claude-code-desktop]}\n    paths:\n      deny: [x]\n"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	r := &runner{env: Env{Stdout: &out}}
	r.printMerges(mergesDoc{Rule: &mergesRule{Source: ".aries/merges.yaml at origin/main", Version: 1, SHA256: rules.SHA},
		PRs: []merges.Verdict{
			{Number: 7, Queue: true, Class: "docs"},
			{Number: 8, Holds: []merges.Reason{{Condition: "floor", Detail: "go.mod (dependency files)"}}},
			{Number: 9, Holds: []merges.Reason{{Condition: "paths", Detail: "\x1b[31mevil.md cannot be classified"}}},
		}}, rules)
	for _, w := range []string{
		"rule .aries/merges.yaml at origin/main, version 1, sha256 " + rules.SHA[:12] + "\n",
		"queue #7 docs\n",
		"hold #8 floor: go.mod (dependency files)\n",
		"the opener is unverified",
	} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("printed %q, want %q in it", out.String(), w)
		}
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("printed a control character: %q", out.String())
	}
	out.Reset()
	r.printMerges(mergesDoc{}, nil)
	if want := "no rule: .aries/merges.yaml is not on the default branch; every pull request is held\nno open pull request\n"; out.String() != want {
		t.Errorf("no rule printed %q, want %q", out.String(), want)
	}
}

// --rules reads the file named, and a file in the repository is held by
// the floor as the rule's own.
func TestLoadMergeRulesFile(t *testing.T) {
	t.Parallel()
	top := t.TempDir()
	if err := os.MkdirAll(filepath.Join(top, "ops"), 0o750); err != nil {
		t.Fatal(err)
	}
	doc := "version: 1\nclasses:\n  d:\n    checks: green\n    paths:\n      allow: [\"docs/**\"]\n"
	if err := os.WriteFile(filepath.Join(top, "ops", "m.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &runner{dir: top}
	rules, rule, rel, err := loadMergeRules(context.Background(), r, top, "main", "ops/m.yaml")
	if err != nil || rules == nil || rule.Source != "ops/m.yaml" || rel != "ops/m.yaml" || len(rule.SHA256) != 64 {
		t.Fatalf("loadMergeRules = %v %+v %q %v", rules, rule, rel, err)
	}
	if err := os.WriteFile(filepath.Join(top, "ops", "bad.yaml"), []byte("version: 1\nmerge: all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := loadMergeRules(context.Background(), r, top, "main", "ops/bad.yaml"); err == nil || !strings.Contains(err.Error(), `unknown key "merge"`) {
		t.Errorf("a bad file: %v", err)
	}
	if _, _, _, err := loadMergeRules(context.Background(), r, top, "main", "ops/none.yaml"); err == nil {
		t.Error("a missing file: no error")
	}
}
