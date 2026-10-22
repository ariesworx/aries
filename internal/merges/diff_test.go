package merges

import (
	"reflect"
	"testing"
)

const sampleDiff = `diff --git a/docs/guide.md b/docs/guide.md
index 1111111..2222222 100644
--- a/docs/guide.md
+++ b/docs/guide.md
@@ -1,3 +1,3 @@
 # Guide
-old line
+new line
 end
diff --git a/internal/proto/ops.go b/internal/x/ops.go
similarity index 98%
rename from internal/proto/ops.go
rename to internal/x/ops.go
index 3333333..4444444 100644
--- a/internal/proto/ops.go
+++ b/internal/x/ops.go
@@ -1 +1 @@
-package proto
+package x
diff --git a/internal/cli/x_test.go b/internal/cli/x_test.go
index 5555555..6666666 100644
--- a/internal/cli/x_test.go
+++ b/internal/cli/x_test.go
@@ -10,2 +10,4 @@ func TestX(t *testing.T) {
 	t.Parallel()
+	t.Skip("flaky")
+	--- not a header
 	check(t)
diff --git a/internal/cli/gone_test.go b/internal/cli/gone_test.go
deleted file mode 100644
index 7777777..0000000
--- a/internal/cli/gone_test.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package cli
-// gone
diff --git a/docs/new.md b/docs/new.md
new file mode 100644
index 0000000..8888888
--- /dev/null
+++ b/docs/new.md
@@ -0,0 +1 @@
+hello
diff --git a/img/logo.png b/img/logo.png
new file mode 100644
index 0000000..9999999
Binary files /dev/null and b/img/logo.png differ
diff --git "a/docs/caf\303\251.md" "b/docs/caf\303\251.md"
index 1111111..2222222 100644
--- "a/docs/caf\303\251.md"
+++ "b/docs/caf\303\251.md"
@@ -1 +1 @@
-a
+b
diff --git "a/odd" "b/odd"
new file mode 100644
Binary files /dev/null and "b/odd" differ
`

func TestParseDiff(t *testing.T) {
	t.Parallel()
	got, err := ParseDiff([]byte(sampleDiff))
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: "docs/guide.md", Removed: 1},
		{Path: "internal/x/ops.go", Old: "internal/proto/ops.go", Removed: 1},
		{Path: "internal/cli/x_test.go", Skips: 1},
		{Path: "internal/cli/gone_test.go", Removed: 2},
		{Path: "docs/new.md"},
		{Path: "img/logo.png"},
		{Path: "docs/café.md", Removed: 1},
		{Path: `"\"a/odd\" \"b/odd\""`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseDiff\n got: %+v\nwant: %+v", got, want)
	}
	// The last one, whose header cannot be read, holds the pull request.
	pr := good()
	pr.Files = got
	if v := Classify(load(t, testRules), Context{Base: "main"}, pr); v.Queue {
		t.Errorf("queued a diff with an unreadable path: %s", v)
	}
}
