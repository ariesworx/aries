package agentsetup

import (
	"strings"
	"testing"
)

// A shadowing duplicate key decides what the harness runs, since
// JSON.parse keeps the last one; setup refuses such a file rather than
// edit the first copy and call it registered (T-2). Inside the Aries
// entry a duplicate is not registered either, and Apply's wholesale
// replacement leaves one copy of each key.
func TestJSONRefusesDuplicateKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in string
		refused  bool
	}{
		{"top level", `{"mcpServers":{},"mcpServers":{"aries":{"command":"/tmp/evil","args":["mcp"]}}}`, true},
		{"servers", `{"mcpServers":{"aries":{"command":"aries","args":["mcp"]},"aries":{"command":"/tmp/evil"}}}`, true},
		{"escaped twin", `{"mcpServers":{},"mcp\u0053ervers":{}}`, true},
		{"entry", `{"mcpServers":{"aries":{"command":"aries","args":["mcp"],"command":"/tmp/evil"}}}`, false},
		{"env", `{"mcpServers":{"aries":{"command":"aries","args":["mcp"],"env":{"ARIES_HARNESS":"gemini","ARIES_HARNESS":"x"}}}}`, false},
	}
	a := Agents["gemini"]
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if a.Registered([]byte(tc.in), DefaultEntry) {
				t.Errorf("Registered(%s) = true, want false", tc.in)
			}
			out, _, err := a.Apply([]byte(tc.in), DefaultEntry)
			if !tc.refused {
				if err != nil || string(out) != a.Snippet(DefaultEntry) {
					t.Errorf("Apply(%s) = %s, %v; want the snippet", tc.in, out, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Errorf("Apply(%s) = %s, %v; want a duplicate-key error", tc.in, out, err)
			}
			if _, _, err := a.Remove([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), "duplicate") {
				t.Errorf("Remove(%s) = %v; want a duplicate-key error", tc.in, err)
			}
		})
	}
}

// Text after the object makes the file unreadable to the harness, so
// setup refuses it rather than call it registered, or rewrite the file
// without it.
func TestJSONRefusesTextAfterTheObject(t *testing.T) {
	t.Parallel()
	a := Agents["gemini"]
	tests := []struct{ name, in string }{
		{"a word", `{"mcpServers":{}} x`},
		{"a stray brace", `{"mcpServers":{}}}`},
		{"a stray bracket", `{"mcpServers":{}} ]`},
		{"a comment", "{\"mcpServers\":{}}\n// keep this\n"},
		{"a second object", `{"mcpServers":{}} {}`},
		{"a registered entry, then a brace", a.Snippet(DefaultEntry) + "}"},
	}
	const want = "not JSON: text after the object"
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if a.Registered([]byte(tc.in), DefaultEntry) {
				t.Errorf("Registered(%q) = true, want false", tc.in)
			}
			if out, res, err := a.Apply([]byte(tc.in), DefaultEntry); err == nil || err.Error() != want {
				t.Errorf("Apply(%q) = %q, %s, %v; want error %q", tc.in, out, res, err, want)
			}
			if out, res, err := a.Remove([]byte(tc.in)); err == nil || err.Error() != want {
				t.Errorf("Remove(%q) = %q, %s, %v; want error %q", tc.in, out, res, err, want)
			}
		})
	}
}

// An entry with the right command but extra keys runs something else:
// --check reports it, and Apply replaces the entry wholesale (C-8).
func TestRegisteredRefusesExtras(t *testing.T) {
	t.Parallel()
	const good = `"command": "aries", "args": ["mcp"]`
	tests := []struct {
		name, agent, in string
	}{
		{"json PATH", "gemini", `{"mcpServers": {"aries": {` + good + `, "env": {"ARIES_HARNESS": "gemini", "PATH": "./.tools:/usr/bin"}}}}`},
		{"json LD_PRELOAD", "claude-code", `{"mcpServers": {"aries": {"type": "stdio", ` + good + `, "env": {"ARIES_HARNESS": "claude-code", "LD_PRELOAD": "./x.so"}}}}`},
		{"json NODE_OPTIONS", "cursor", `{"mcpServers": {"aries": {` + good + `, "env": {"ARIES_HARNESS": "cursor", "NODE_OPTIONS": "--require ./x.js"}}}}`},
		{"json cwd", "gemini", `{"mcpServers": {"aries": {` + good + `, "cwd": "./tools", "env": {"ARIES_HARNESS": "gemini"}}}}`},
		{"json envFile", "vscode", `{"servers": {"aries": {"type": "stdio", ` + good + `, "envFile": ".env", "env": {"ARIES_HARNESS": "vscode"}}}}`},
		{"json session", "gemini", `{"mcpServers": {"aries": {` + good + `, "env": {"ARIES_HARNESS": "gemini", "ARIES_SESSION": "x"}}}}`},
		{"toml env", "codex", "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\nLD_PRELOAD = \"./x.so\"\n"},
		{"toml cwd", "codex", "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\ncwd = \"./tools\"\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\n"},
		{"toml dotted env", "codex", "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\nenv.PATH = \"./.tools\"\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := Agents[tc.agent]
			if a.Registered([]byte(tc.in), DefaultEntry) {
				t.Fatalf("Registered(%s) = true, want false", tc.in)
			}
			out, res, err := a.Apply([]byte(tc.in), DefaultEntry)
			if err != nil || res != Updated {
				t.Fatalf("Apply = %s, %v; want updated", res, err)
			}
			for _, extra := range []string{"PATH", "LD_PRELOAD", "NODE_OPTIONS", "cwd", "envFile", "ARIES_SESSION"} {
				if strings.Contains(string(out), extra) {
					t.Errorf("Apply kept %s:\n%s", extra, out)
				}
			}
			if want := a.Snippet(DefaultEntry); string(out) != want {
				t.Errorf("Apply = \n%s\nwant the snippet\n%s", out, want)
			}
			if !a.Registered(out, DefaultEntry) {
				t.Errorf("Apply's output is not registered:\n%s", out)
			}
		})
	}
}

// The Codex editor understands TOML's strings, arrays and comments:
// valid TOML in gives valid TOML out with Aries registered, and a
// second run changes nothing (T-3, C-11).
func TestApplyTOMLStructure(t *testing.T) {
	t.Parallel()
	const block = "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\n"
	tests := []struct{ name, in, want string }{
		{"nested array",
			"[mcp_servers.aries]\ncommand = \"old\"\nargs = [[\"a\"],\n[\"b\"]]\n",
			block},
		{"bracket in a string",
			"[mcp_servers.aries]\nargs = [\"a]\",\n \"b\"]\n",
			block},
		{"header inside a basic multi-line string",
			"x = \"\"\"\n[mcp_servers.aries]\n\"\"\"\n",
			"x = \"\"\"\n[mcp_servers.aries]\n\"\"\"\n\n" + block},
		{"header inside a literal multi-line string",
			"x = '''\n[mcp_servers.aries]\ncommand = \"evil\"\n'''\n",
			"x = '''\n[mcp_servers.aries]\ncommand = \"evil\"\n'''\n\n" + block},
		{"escaped quotes in a multi-line string",
			"x = \"\"\"a\\\"\"\"\n[mcp_servers.aries]\n\"\"\"\n",
			"x = \"\"\"a\\\"\"\"\n[mcp_servers.aries]\n\"\"\"\n\n" + block},
		{"comment with a bracket in an array",
			"[mcp_servers.aries]\nargs = [ # ]\n  \"serve\", # [\n]\nstartup_timeout_sec = 20\n\n[other]\ny = 2\n",
			block + "\n[other]\ny = 2\n"},
		{"keeps comments before the table and after it",
			"# servers\n[mcp_servers.\"aries\"] # ours\ncommand = \"x\"\n# about other\n[other]\n",
			"# servers\n" + block + "\n# about other\n[other]\n"},
		{"env table elsewhere moves beside the table",
			"[mcp_servers.aries.env]\nA = \"b\"\n\n[other]\ny = 2\n\n[mcp_servers.aries]\ncommand = \"x\"\n",
			block + "\n[other]\ny = 2\n"},
		{"subtables are aries's too",
			"[mcp_servers.aries]\ncommand = \"x\"\n\n[mcp_servers.aries.tools.a]\nz = 1\n",
			block},
		{"inline env is replaced",
			"[mcp_servers.aries]\ncommand = \"aries\"\nenv = { A = \"b\" }\n",
			block},
		{"other servers stay",
			"[mcp_servers.other]\ncommand = \"o\"\nargs = [\n  \"x\",\n]\n",
			"[mcp_servers.other]\ncommand = \"o\"\nargs = [\n  \"x\",\n]\n\n" + block},
		{"date time and inline table values",
			"when = 1979-05-27 07:32:00Z\npt = { x = 1, y = \"}\" }\n",
			"when = 1979-05-27 07:32:00Z\npt = { x = 1, y = \"}\" }\n\n" + block},
	}
	a := Agents["codex"]
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, _, err := a.Apply([]byte(tc.in), DefaultEntry)
			if err != nil {
				t.Fatalf("Apply(%q) = %v", tc.in, err)
			}
			if string(out) != tc.want {
				t.Fatalf("Apply(%q) =\n%s\nwant\n%s", tc.in, out, tc.want)
			}
			if _, err := scanTOML(out); err != nil {
				t.Fatalf("Apply's output does not scan: %v", err)
			}
			again, res, err := a.Apply(out, DefaultEntry)
			if err != nil || res != Unchanged || string(again) != string(out) {
				t.Fatalf("second Apply = %s, %v\n%s", res, err, again)
			}
			if !a.Registered(out, DefaultEntry) {
				t.Fatal("not registered after Apply")
			}
		})
	}
}

// What the editor cannot place safely, it refuses; setup then prints
// the snippet to add by hand.
func TestApplyTOMLRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct{ name, in string }{
		{"unterminated multi-line string", "x = \"\"\"\n[mcp_servers.aries]\n"},
		{"unterminated array", "args = [\"a\",\n"},
		{"newline in a basic string", "x = \"a\nb\"\n"},
		{"aries as an inline table", "[mcp_servers]\naries = { command = \"evil\" }\n"},
		{"aries by dotted keys", "mcp_servers.aries.command = \"evil\"\n"},
		{"servers as an inline table", "mcp_servers = { other = { command = \"x\" } }\n"},
		{"servers as an array", "[[mcp_servers]]\nx = 1\n"},
		{"twice", "[mcp_servers.aries]\ncommand = \"a\"\n[mcp_servers.aries]\ncommand = \"b\"\n"},
		{"bad header", "[mcp_servers.aries\n"},
		{"text after a value", "x = 1 2\n"},
		{"bad escape in a key", "\"a\\q\" = 1\n"},
	}
	a := Agents["codex"]
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if out, _, err := a.Apply([]byte(tc.in), DefaultEntry); err == nil {
				t.Errorf("Apply(%q) =\n%s\nwant an error", tc.in, out)
			}
			if a.Registered([]byte(tc.in), DefaultEntry) {
				t.Errorf("Registered(%q) = true", tc.in)
			}
		})
	}
	if _, _, err := a.Remove([]byte("x = \"\"\"\n[mcp_servers.aries]\n")); err == nil {
		t.Error("Remove accepted an unterminated string")
	}
}

// TOML basic strings take only TOML's escapes; Go's \x.. and \a are not
// among them.
func TestTOMLString(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{`C:\tools\aries.exe`, `"C:\\tools\\aries.exe"`},
		{"a\"b", `"a\"b"`},
		{"\a\x01\x7f", `"\u0007\u0001\u007F"`},
		{"tab\tnl\n", `"tab\tnl\n"`},
		{"é", `"é"`},
	}
	for _, tc := range tests {
		if got := tomlString(tc.in); got != tc.want {
			t.Errorf("tomlString(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// A hook is Aries's only when its whole command is one program token
// running prime --hook, or usage --hook: a person's compound command that
// ends the same way is theirs (C-10).
func TestOurHookExact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cmd  string
		want bool
	}{
		{"aries prime --hook", true},
		{"aries prime --hook=codex", true},
		{"/usr/local/bin/aries prime --hook", true},
		{"'/opt/star fix/aries' prime --hook", true},
		{"'/opt/it'\\''s/aries' prime --hook=gemini", true},
		{`C:\tools\aries.exe prime --hook`, false}, // not shell-safe unquoted, so not what setup writes
		{`'C:\tools\aries.exe' prime --hook`, true},
		{"make env; ./bin/aries prime --hook", false},
		{"cd x && aries prime --hook", false},
		{"$(curl x) aries prime --hook", false},
		{"/tmp/evil aries prime --hook", false},
		{"bd prime --hook", false},
		{"aries prime --hook --x", false},
		{"'aries prime --hook", false},
	}
	for _, tc := range tests {
		h := object{{"type", mustJSON("command")}, {"command", mustJSON(tc.cmd)}}
		if got := claudeHook.ours(h, DefaultEntry, "claude-code"); got != tc.want {
			t.Errorf("prime hook ours(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
	}
	// The usage hooks are owned the same way, and each verb owns only its
	// own command.
	usage := []struct {
		cmd         string
		prime, want bool
	}{
		{cmd: "aries usage --hook", want: true},
		{cmd: "aries usage --hook=claude-code", want: true},
		{cmd: "/usr/local/bin/aries usage --hook", want: true},
		{cmd: "'/opt/star fix/aries' usage --hook", want: true},
		{cmd: "aries prime --hook", prime: true},
		{cmd: "bd usage --hook"},
		{cmd: "cd x && aries usage --hook"},
		{cmd: "aries usage --hook --x"},
		{cmd: "aries usage"},
	}
	for _, tc := range usage {
		h := object{{"type", mustJSON("command")}, {"command", mustJSON(tc.cmd)}}
		if got := claudeUsage("Stop", true, UsageTimeout).ours(h, DefaultEntry, "claude-code"); got != tc.want {
			t.Errorf("usage hook ours(%q) = %v, want %v", tc.cmd, got, tc.want)
		}
		if got := claudeHook.ours(h, DefaultEntry, "claude-code"); got != tc.prime {
			t.Errorf("prime hook ours(%q) = %v, want %v", tc.cmd, got, tc.prime)
		}
	}
	in := `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": "make env; ./bin/aries prime --hook"}]}]}}`
	h := hookOf(t, "claude-code", false)
	if out, res, err := h.Remove([]byte(in), DefaultEntry); err != nil || res != Unchanged || string(out) != in {
		t.Errorf("Remove(compound hook) = %s, %v\n%s", res, err, out)
	}
	out, res, err := h.Apply([]byte(in), DefaultEntry)
	if err != nil || res != Added || !strings.Contains(string(out), "make env; ./bin/aries prime --hook") {
		t.Errorf("Apply beside a compound hook = %s, %v\n%s", res, err, out)
	}
}

// FuzzApplyTOML: whatever the editor accepts, its output scans, holds
// the entry, and a second run changes nothing.
func FuzzApplyTOML(f *testing.F) {
	for _, s := range []string{"model = \"o3\"\n", "[mcp_servers.aries]\ncommand = \"old\"\nargs = [\n  \"a\",\n  \"b\",\n]\n",
		"x = '''\n[mcp_servers.aries]\n'''\n", "[mcp_servers.aries.env]\nA = 1\n\n\n[b]\n", "a = [1, # ]\n 2]\r\n"} {
		f.Add([]byte(s))
	}
	a := Agents["codex"]
	f.Fuzz(func(t *testing.T, in []byte) {
		out, _, err := a.Apply(in, DefaultEntry)
		if err != nil {
			return
		}
		if _, err := scanTOML(out); err != nil {
			t.Fatalf("output does not scan: %v\nin %q\nout %q", err, in, out)
		}
		if !a.Registered(out, DefaultEntry) {
			t.Fatalf("not registered:\nin %q\nout %q", in, out)
		}
		if rm, _, err := a.Remove(out); err != nil || tomlHas(rm) {
			t.Fatalf("Remove(out) = %q, %v", rm, err)
		}
	})
}
