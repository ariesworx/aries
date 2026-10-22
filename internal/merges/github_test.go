package merges

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeGH answers gh's argv from a table: the joined args to output and
// an error.
type fakeGH map[string]struct {
	out string
	err error
}

func (f fakeGH) run(_ context.Context, _, name string, args ...string) ([]byte, error) {
	if name != "gh" {
		return nil, errors.New("ran " + name)
	}
	for _, a := range args {
		if a == "api" {
			return nil, errors.New("ran gh api")
		}
	}
	r, ok := f[strings.Join(args, " ")]
	if !ok {
		return nil, errors.New("unexpected: gh " + strings.Join(args, " "))
	}
	return []byte(r.out), r.err
}

func TestGH(t *testing.T) {
	t.Parallel()
	f := fakeGH{
		"pr list --state open --limit 200 --json number,url,title,baseRefName,headRefName,headRefOid,isDraft,files": {out: `[
{"number":9,"url":"https://example.com/pr/9","title":"b","baseRefName":"main","headRefName":"docs/ar-a1b2c3d4-x","headRefOid":"def","isDraft":true,"files":[]},
{"number":8,"url":"https://example.com/pr/8","title":"a","baseRefName":"main","headRefName":"fix/ar-b1b2c3d4-y","headRefOid":"abc","isDraft":false,"files":[{"path":"docs/a.md"},{"path":"go.sum"}]}]`},
		"pr checks 8 --required --json name,bucket": {out: `[{"name":"lint","bucket":"pass"},{"name":"test","bucket":"fail"}]`, err: errors.New("exit status 1")},
		"pr diff 8 --color never":                   {out: "diff --git a/docs/a.md b/docs/a.md\n--- a/docs/a.md\n+++ b/docs/a.md\n@@ -1 +1 @@\n-x\n+y\n"},
		"pr checks 9 --required --json name,bucket": {err: errors.New("exit status 1: no required checks reported on the 'docs/ar-a1b2c3d4-x' branch")},
		"pr diff 9 --color never":                   {err: errors.New("exit status 1: HTTP 406: the diff exceeded the maximum number of lines")},
	}
	g := &GH{Run: f.run}
	ctx := context.Background()
	open, err := g.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 || open[0].Number != 8 || !open[1].Draft || open[0].Branch != "fix/ar-b1b2c3d4-y" {
		t.Fatalf("Open = %+v", open)
	}
	got := Read(ctx, g, open[0])
	if want := []Check{{"lint", "pass"}, {"test", "fail"}}; !reflect.DeepEqual(got.Checks, want) || got.ChecksErr != "" {
		t.Errorf("checks %+v %q, want %+v", got.Checks, got.ChecksErr, want)
	}
	// The listed go.sum, absent from the diff, is still classified.
	if want := []Change{{Path: "docs/a.md", Removed: 1}, {Path: "go.sum"}}; !reflect.DeepEqual(got.Files, want) {
		t.Errorf("files %+v, want %+v", got.Files, want)
	}
	got = Read(ctx, g, open[1])
	if got.Checks != nil || got.ChecksErr != "" {
		t.Errorf("no required checks: %+v %q", got.Checks, got.ChecksErr)
	}
	if !strings.Contains(got.FilesErr, "maximum number of lines") {
		t.Errorf("an unreadable diff: %q", got.FilesErr)
	}
	v := Classify(load(t, testRules), Context{Base: "main"}, got)
	if v.Queue {
		t.Errorf("queued an unreadable pull request: %s", v)
	}

	broken := &GH{Run: fakeGH{}.run}
	if _, err := broken.Open(ctx); err == nil {
		t.Error("Open with gh failing: no error")
	}
	if _, err := broken.Required(ctx, 1); err == nil {
		t.Error("Required with gh failing: no error")
	}
}
