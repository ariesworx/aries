package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aries tui refuses, before it dials, anything but a terminal on both
// standard input and output, and a terminal that says it cannot move
// the cursor (TERM=dumb), and says what to run instead.
func TestTUIRefusals(t *testing.T) {
	t.Parallel()
	file := func(name string) *os.File {
		f, err := os.Create(filepath.Join(t.TempDir(), name)) //nolint:gosec // the test's own temporary file
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	tests := []struct {
		name   string
		args   []string
		stdout func() any
		term   string
		code   int
		want   []string
	}{
		{"output to a buffer", []string{"tui"}, func() any { return nil }, "xterm", ExitFailure,
			[]string{"aries: aries tui needs a terminal, and standard input or output is not one\n",
				"fix: run it in a terminal; for a list that does not update, `aries ready`, `aries blocked` or `aries list`\n"}},
		{"output to a file", []string{"tui"}, func() any { return file("out") }, "", ExitFailure,
			[]string{"aries tui needs a terminal"}},
		{"dumb terminal", []string{"tui"}, func() any { return nil }, "dumb", ExitFailure,
			[]string{"aries: aries tui needs a terminal that can move the cursor, and TERM is dumb\n",
				"fix: run it in a terminal; for a list that does not update, `aries ready`, `aries blocked` or `aries list`\n"}},
		{"json", []string{"tui", "--json"}, func() any { return nil }, "", ExitUsage,
			[]string{`{"error":{"code":"invalid","message":"tui draws a board and prints no JSON; ` + "`aries ready --json` and `aries list --json` do" + `","fix":"usage: aries tui"}}`}},
		{"arguments", []string{"tui", "ar-a1"}, func() any { return nil }, "", ExitUsage, []string{"tui takes no arguments"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf, errb bytes.Buffer
			getenv := func(k string) string {
				if k == "TERM" {
					return tc.term
				}
				return ""
			}
			env := Env{Stdout: &buf, Stderr: &errb, Getenv: getenv, Version: "v0.3.0"}
			if f, ok := tc.stdout().(*os.File); ok {
				env.Stdout, env.Stdin = f, file("in")
			}
			// -C names a directory with no .aries.yaml: a refusal that
			// came after dialing would be about the config instead.
			code := Run(t.Context(), append([]string{"-C", t.TempDir()}, tc.args...), env)
			// --json reports on standard output, as one document.
			got := buf.String() + errb.String()
			if f, ok := env.Stdout.(*os.File); ok {
				b, err := os.ReadFile(f.Name())
				if err != nil {
					t.Fatal(err)
				}
				got = string(b) + errb.String()
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("aries %s printed %q, want it to contain %q", strings.Join(tc.args, " "), got, want)
				}
			}
			if code != tc.code {
				t.Errorf("aries %s = exit %d, want %d", strings.Join(tc.args, " "), code, tc.code)
			}
		})
	}
}
