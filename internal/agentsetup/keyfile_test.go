package agentsetup

import (
	"bytes"
	"strings"
	"testing"
)

// TestEntryKeyFile pins the one variable beside the harness's name that
// a starfix entry may hold: STARFIX_KEY_FILE, the key file the agent's
// sfx signs in with, written only when the entry's KeyFile is set. The
// entry is still replaced wholesale and may hold nothing else.
func TestEntryKeyFile(t *testing.T) {
	const path = `/home/alice/.ssh/starfix "agent" key`
	for _, name := range Names() {
		a := Agents[name]
		if a.ManualMCP() {
			continue // no file of its own to write
		}
		t.Run(name, func(t *testing.T) {
			with := DefaultEntry
			with.KeyFile = path
			out, res, err := a.Apply(nil, with)
			if err != nil || res != Added {
				t.Fatalf("Apply with a key file = %v, %v; want Added", res, err)
			}
			if !bytes.Contains(out, []byte(KeyFileEnv)) {
				t.Fatalf("the entry does not set %s:\n%s", KeyFileEnv, out)
			}
			if got := a.KeyFile(out, with); got != path {
				t.Errorf("KeyFile of the entry = %q, want %q", got, path)
			}
			if !a.Registered(out, with) {
				t.Errorf("the entry Apply wrote is not Registered with the same key file:\n%s", out)
			}
			// It is exactly what was asked for: not the entry without a
			// key file, and not the entry with another.
			other := with
			other.KeyFile = "/home/alice/.ssh/other"
			if a.Registered(out, DefaultEntry) || a.Registered(out, other) {
				t.Errorf("an entry with key file %q is Registered for none, or for another", path)
			}
			// Without one, the variable is not written, and an entry that
			// has it is replaced by one that does not.
			plain, res, err := a.Apply(out, DefaultEntry)
			if err != nil || res != Updated || bytes.Contains(plain, []byte(KeyFileEnv)) || a.KeyFile(plain, DefaultEntry) != "" {
				t.Errorf("Apply without a key file over one that has it = %v, %v; want Updated and no %s:\n%s", res, err, KeyFileEnv, plain)
			}
			if again, res, _ := a.Apply(out, with); res != Unchanged || !bytes.Equal(again, out) {
				t.Errorf("Apply again with the same key file = %v, want Unchanged", res)
			}
			// Removing the entry takes the path with it.
			gone, res, err := a.Remove(out)
			if err != nil || res != Removed || strings.Contains(string(gone), "starfix") {
				t.Errorf("Remove = %v, %v; want Removed and nothing of the path left:\n%s", res, err, gone)
			}
		})
	}
	if a := Agents["claude-code"]; a.KeyFile([]byte("not json"), DefaultEntry) != "" || a.KeyFile(nil, DefaultEntry) != "" {
		t.Error("KeyFile of a file with no entry is not empty")
	}
}
