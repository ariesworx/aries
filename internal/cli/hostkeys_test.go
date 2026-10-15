package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOutboxHostKeyThatIsPinned: `--host-key` reaches what was kept
// under a host key the server no longer has, so naming any key the
// config pins, the first or another, is a usage error that says so.
func TestOutboxHostKeyThatIsPinned(t *testing.T) {
	t.Parallel()
	a := newAway(t)
	const newKey = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	cfgPath := filepath.Join(a.repo, ".aries.yaml")
	b, err := os.ReadFile(cfgPath) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(b), "  host_key: "+awayHostKey+"\n", "  host_keys: ["+awayHostKey+", "+newKey+"]\n", 1)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil { //nolint:gosec // the test's own file
		t.Fatal(err)
	}
	a.reachedAs(t, "alice")
	for _, key := range []string{awayHostKey, newKey} {
		code, out, errb := a.run(t, "outbox", "--host-key", key)
		want := "aries: " + key + " is pinned in .aries.yaml now: leave out --host-key\n"
		if code != ExitUsage || out != "" || !strings.HasPrefix(errb, want) {
			t.Errorf("aries outbox --host-key %s = exit %d, stdout %q, stderr %q; want exit %d and %q", key, code, out, errb, ExitUsage, want)
		}
	}
	if code, out, errb := a.run(t, "outbox"); code != ExitOK || out != "alice's outbox is empty\nno failure to reach the server is recorded\n" {
		t.Errorf("aries outbox, pinning two keys = exit %d, stdout %q, stderr %q; want alice's empty outbox", code, out, errb)
	}
}
