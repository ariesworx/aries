package sextant

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := filepath.Join(dir, "sextant.yaml")
	if err := os.WriteFile(good, []byte("server:\n  url: https://aries.example.com\nkey: id_ed25519\nprojects: ["+projectA+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("server:\n  url: https://aries.example.com\nprojects: ["+projectA+"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name         string
		args         []string
		code         int
		stdout, errs string
	}{
		{"version", []string{"-version"}, ExitOK, "sextant v9.9.9\n", ""},
		{"check", []string{"-config", good, "-check"}, ExitOK, "sextant: " + good + " is good: 1 project, every 1m0s, a window of 24h0m0s, listening on 127.0.0.1:9466\n", ""},
		{"check a bad config", []string{"-config", bad, "-check"}, ExitFailure, "", "neither key: nor workload: is set"},
		{"no such flag", []string{"-colour"}, ExitUsage, "", "flag provided but not defined"},
		{"an argument", []string{"run"}, ExitUsage, "", "sextant takes no argument"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errb bytes.Buffer
			code := Command(t.Context(), tc.args, CommandEnv{Stdout: &out, Stderr: &errb, Version: "v9.9.9"})
			if code != tc.code || out.String() != tc.stdout || !strings.Contains(errb.String(), tc.errs) {
				t.Errorf("Command(%q) = %d\nstdout %q\nstderr %q\nwant %d, %q and stderr holding %q", tc.args, code, out.String(), errb.String(), tc.code, tc.stdout, tc.errs)
			}
		})
	}
}
