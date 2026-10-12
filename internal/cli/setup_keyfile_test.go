package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSetupKeyFile pins `sfx setup --key-file`: it puts STARFIX_KEY_FILE
// into the starfix entry of a file that is the person's own (--global,
// or a desktop app's config), and never into one a repository commits.
func TestSetupKeyFile(t *testing.T) {
	root, sub := repoWithConfig(t)
	home := t.TempDir()
	key := filepath.Join(home, ".ssh", "starfix_agent")
	if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("a key file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mcp := filepath.Join(home, ".claude.json")

	// Written, with the path made absolute.
	code, out, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--global", "--write", "--key-file", "~/.ssh/starfix_agent")
	if code != ExitOK {
		t.Fatalf("setup --global --write --key-file: exit %d\n%s%s", code, out, errb)
	}
	if got := readFile(t, mcp); !strings.Contains(got, `"STARFIX_KEY_FILE": `+jsonString(key)) {
		t.Errorf("%s does not set STARFIX_KEY_FILE to %s:\n%s", mcp, key, got)
	}
	// Claude Code's hooks run with its own environment, not the entry's.
	if !strings.Contains(errb, "hooks") || !strings.Contains(errb, "STARFIX_KEY_FILE") {
		t.Errorf("setup did not say that the hooks need the variable in the agent's own environment:\n%s", errb)
	}
	// --check with the same file passes, and with none says what the
	// entry holds.
	if code, out, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--global", "--check", "--key-file", key); code != ExitOK {
		t.Errorf("setup --check --key-file with the file in place: exit %d\n%s%s", code, out, errb)
	}
	code, out, errb = runIn(t, home, "-C", sub, "setup", "claude-code", "--global", "--check")
	if code == ExitOK || !strings.Contains(out+errb, "STARFIX_KEY_FILE="+key) || !strings.Contains(out+errb, "--key-file") {
		t.Errorf("setup --check without --key-file, on an entry that has one: exit %d, want a failure that reports the variable and names the option\n%s%s", code, out, errb)
	}
	// Written again without the option, the entry is replaced whole.
	if code, _, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--global", "--write"); code != ExitOK || strings.Contains(readFile(t, mcp), "STARFIX_KEY_FILE") {
		t.Errorf("setup --write without --key-file left the variable: exit %d\n%s\n%s", code, errb, readFile(t, mcp))
	}
	// And --remove takes it out with the entry.
	runIn(t, home, "-C", sub, "setup", "claude-code", "--global", "--write", "--key-file", key)
	if code, _, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--global", "--remove"); code != ExitOK || strings.Contains(readFile(t, mcp), "starfix_agent") {
		t.Errorf("setup --remove left the key file's path: exit %d\n%s\n%s", code, errb, readFile(t, mcp))
	}

	before := snapshot(t, root)
	refused := []struct {
		name string
		args []string
		want string
	}{
		{"a project's files", []string{"setup", "claude-code", "--write", "--key-file", key}, "only for your own files"},
		{"every agent's project files", []string{"setup", "--all", "--write", "--key-file", key}, "only for your own files"},
		{"with --remove", []string{"setup", "claude-code", "--global", "--remove", "--key-file", key}, "--remove takes no --key-file"},
		{"a file that is not there", []string{"setup", "claude-code", "--global", "--write", "--key-file", filepath.Join(home, "nope")}, "is not a file"},
		{"a directory", []string{"setup", "claude-code", "--global", "--write", "--key-file", home}, "is not a file"},
		{"a key in place of a path", []string{"setup", "claude-code", "--global", "--write", "--key-file", "-----BEGIN OPENSSH PRIVATE KEY-----"}, "never the key itself"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runIn(t, home, append([]string{"-C", sub}, tc.args...)...)
			if code == ExitOK || !strings.Contains(errb, tc.want) {
				t.Errorf("sfx %s: exit %d, want a refusal that says %q\n%s%s", strings.Join(tc.args, " "), code, tc.want, out, errb)
			}
			if strings.Contains(out+errb, "BEGIN OPENSSH") {
				t.Errorf("the refusal repeats what was given as a path:\n%s%s", out, errb)
			}
		})
	}
	sameFiles(t, before, snapshot(t, root))
	if strings.Contains(readFile(t, mcp), "STARFIX_KEY_FILE") {
		t.Errorf("a refused setup wrote the variable:\n%s", readFile(t, mcp))
	}
}

// A desktop app starts sfx with no shell environment at all, so its
// entry is the only place the variable can come from.
func TestSetupKeyFileClaudeDesktop(t *testing.T) {
	_, sub := repoWithConfig(t)
	home := t.TempDir()
	key := filepath.Join(home, "agent_key")
	if err := os.WriteFile(key, []byte("a key file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := desktopEnv{goos: "darwin", home: home, exe: "/opt/starfix/bin/sfx"}
	code, out, errb := d.run(t, "-C", sub, "setup", "claude-desktop", "--write", "--key-file", key)
	cfg := filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	if code != ExitOK || !strings.Contains(readFile(t, cfg), `"STARFIX_KEY_FILE": `+jsonString(key)) {
		t.Fatalf("setup claude-desktop --write --key-file: exit %d\n%s%s\n%s", code, out, errb, readFile(t, cfg))
	}
	if code, out, errb := d.run(t, "-C", sub, "setup", "claude-desktop", "--check", "--key-file", key); code != ExitOK {
		t.Errorf("setup claude-desktop --check --key-file: exit %d\n%s%s", code, out, errb)
	}
	if code, _, _ := d.run(t, "-C", sub, "setup", "claude-desktop", "--check"); code == ExitOK {
		t.Error("setup claude-desktop --check without --key-file passed on an entry that has one")
	}
}
