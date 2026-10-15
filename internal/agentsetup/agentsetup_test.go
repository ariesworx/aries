package agentsetup

import (
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	t.Parallel()
	custom := Entry{Command: `C:\tools\aries.exe`, Args: []string{"mcp"}}
	tests := []struct {
		name, agent, in, want string
		entry                 Entry
		result                Result
	}{
		{name: "claude new file", agent: "claude-code", result: Added, want: `{
  "mcpServers": {
    "aries": {
      "type": "stdio",
      "command": "aries",
      "args": [
        "mcp"
      ],
      "env": {
        "ARIES_HARNESS": "claude-code"
      }
    }
  }
}
`},
		{name: "claude keeps other servers and order", agent: "claude-code", result: Added,
			in: `{"z": 1.50, "mcpServers": {"figma": {"type": "http", "url": "https://mcp.example.com"}}, "a": true}`,
			want: `{
  "z": 1.50,
  "mcpServers": {
    "figma": {
      "type": "http",
      "url": "https://mcp.example.com"
    },
    "aries": {
      "type": "stdio",
      "command": "aries",
      "args": [
        "mcp"
      ],
      "env": {
        "ARIES_HARNESS": "claude-code"
      }
    }
  },
  "a": true
}
`},
		{name: "claude replaces the entry wholesale", agent: "claude-code", result: Updated, entry: custom,
			in: `{"mcpServers": {"aries": {"command": "old", "env": {"ARIES_SESSION": "x"}, "args": []}}}`,
			want: `{
  "mcpServers": {
    "aries": {
      "type": "stdio",
      "command": "C:\\tools\\aries.exe",
      "args": [
        "mcp"
      ],
      "env": {
        "ARIES_HARNESS": "claude-code"
      }
    }
  }
}
`},
		{name: "gemini new file", agent: "gemini", result: Added, in: "\n", want: `{
  "mcpServers": {
    "aries": {
      "command": "aries",
      "args": [
        "mcp"
      ],
      "env": {
        "ARIES_HARNESS": "gemini"
      }
    }
  }
}
`},
		{name: "vscode new file", agent: "vscode", result: Added, want: `{
  "servers": {
    "aries": {
      "type": "stdio",
      "command": "aries",
      "args": [
        "mcp"
      ],
      "env": {
        "ARIES_HARNESS": "vscode"
      }
    }
  }
}
`},
		{name: "vscode keeps inputs and other servers", agent: "vscode", result: Added,
			in: `{"inputs": [{"id": "tok", "type": "promptString"}], "servers": {"gh": {"type": "http", "url": "https://mcp.example.com"}}}`,
			want: `{
  "inputs": [
    {
      "id": "tok",
      "type": "promptString"
    }
  ],
  "servers": {
    "gh": {
      "type": "http",
      "url": "https://mcp.example.com"
    },
    "aries": {
      "type": "stdio",
      "command": "aries",
      "args": [
        "mcp"
      ],
      "env": {
        "ARIES_HARNESS": "vscode"
      }
    }
  }
}
`},
		{name: "cursor new file", agent: "cursor", result: Added, want: `{
  "mcpServers": {
    "aries": {
      "command": "aries",
      "args": [
        "mcp"
      ],
      "env": {
        "ARIES_HARNESS": "cursor"
      }
    }
  }
}
`},
		{name: "codex new file", agent: "codex", result: Added,
			want: "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\n"},
		{name: "codex appends after other tables", agent: "codex", result: Added,
			in:   "model = \"o4\"\n\n[mcp_servers.other]\ncommand = \"x\"\n",
			want: "model = \"o4\"\n\n[mcp_servers.other]\ncommand = \"x\"\n\n[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\n"},
		{name: "codex replaces the entry in place, keeping the comment above", agent: "codex", result: Updated, entry: custom,
			in:   "# top\n[mcp_servers.\"aries\"] # ours\nargs = [\n  \"serve\",\n]\nstartup_timeout_sec = 20\n\n[mcp_servers.aries.env]\nA = \"b\"\n",
			want: "# top\n[mcp_servers.aries]\ncommand = \"C:\\\\tools\\\\aries.exe\"\nargs = [\"mcp\"]\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\n"},
		{name: "codex adds the env table after its own, before the next", agent: "codex", result: Updated,
			in:   "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\n\n[other]\ny = 2\n",
			want: "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\n\n[other]\ny = 2\n"},
		{name: "codex corrects a wrong harness", agent: "codex", result: Updated,
			in:   "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\n\n[mcp_servers.aries.env]\nA = \"b\"\nARIES_HARNESS = \"gemini\"\n",
			want: "[mcp_servers.aries]\ncommand = \"aries\"\nargs = [\"mcp\"]\n\n[mcp_servers.aries.env]\nARIES_HARNESS = \"codex\"\n"},
		{name: "claude drops other env variables", agent: "claude-code", result: Updated,
			in: `{"mcpServers": {"aries": {"type": "stdio", "command": "aries", "args": ["mcp"], "env": {"ARIES_HARNESS": "codex", "A": "b"}}}}`,
			want: `{
  "mcpServers": {
    "aries": {
      "type": "stdio",
      "command": "aries",
      "args": [
        "mcp"
      ],
      "env": {
        "ARIES_HARNESS": "claude-code"
      }
    }
  }
}
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := Agents[tc.agent]
			e := tc.entry
			if e.Command == "" {
				e = DefaultEntry
			}
			out, res, err := a.Apply([]byte(tc.in), e)
			if err != nil {
				t.Fatal(err)
			}
			if res != tc.result || string(out) != tc.want {
				t.Fatalf("%s\n%s\nwant %s\n%s", res, out, tc.result, tc.want)
			}
			// Twice changes nothing, byte for byte.
			again, res, err := a.Apply(out, e)
			if err != nil || res != Unchanged || string(again) != string(out) {
				t.Fatalf("second apply: %s %v\n%s", res, err, again)
			}
			if !a.Registered(out, e) || a.Registered(out, Entry{Command: "other", Args: e.Args}) {
				t.Fatal("Registered disagrees")
			}
		})
	}
}

// An already-registered file is left exactly as it is, even when its
// formatting is not ours.
func TestApplyLeavesRegisteredFileAlone(t *testing.T) {
	t.Parallel()
	in := `{"mcpServers":{"aries":{"env":{"ARIES_HARNESS":"claude-code"},"args":["mcp"],"command":"aries","type":"stdio"}}}`
	out, res, err := Agents["claude-code"].Apply([]byte(in), DefaultEntry)
	if err != nil || res != Unchanged || string(out) != in {
		t.Fatalf("%s %v %s", res, err, out)
	}
}

func TestRemove(t *testing.T) {
	t.Parallel()
	tests := []struct{ agent, in, want string }{
		{"claude-code", `{"mcpServers": {"a": {}, "aries": {"command": "aries"}}}`, "{\n  \"mcpServers\": {\n    \"a\": {}\n  }\n}\n"},
		{"vscode", `{"servers": {"aries": {"command": "aries"}}, "mcpServers": {"aries": {}}}`, "{\n  \"servers\": {},\n  \"mcpServers\": {\n    \"aries\": {}\n  }\n}\n"},
		{"codex", "x = 1\n\n[mcp_servers.aries]\ncommand = \"aries\"\n\n[mcp_servers.aries.env]\nA = \"b\"\n\n[other]\ny = 2\n",
			"x = 1\n\n[other]\ny = 2\n"},
	}
	for _, tc := range tests {
		a := Agents[tc.agent]
		out, res, err := a.Remove([]byte(tc.in))
		if err != nil || res != Removed || string(out) != tc.want {
			t.Fatalf("%s: %s %v\n%s\nwant\n%s", tc.agent, res, err, out, tc.want)
		}
		if again, res, _ := a.Remove(out); res != Unchanged || string(again) != string(out) {
			t.Fatalf("%s: second remove %s", tc.agent, res)
		}
	}
}

func TestApplyRefusesBadJSON(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"[1]", "{", `{"mcpServers": []}`, "{} {}", `{"a": 1, "a": 2}`} {
		if _, _, err := Agents["gemini"].Apply([]byte(in), DefaultEntry); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestSnippets(t *testing.T) {
	t.Parallel()
	for _, name := range Names() {
		a := Agents[name]
		s := a.Snippet(DefaultEntry)
		if !strings.Contains(s, "aries") || !strings.Contains(s, "mcp") {
			t.Errorf("%s snippet: %s", name, s)
		}
		if !a.Registered([]byte(s), DefaultEntry) {
			t.Errorf("%s: its own snippet does not register", name)
		}
	}
}
