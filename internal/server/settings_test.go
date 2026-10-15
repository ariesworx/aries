package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// latest, the newest release an operator typed in by hand for prime to
// compare with, is no setting (decision 88): it went stale silently,
// and `ariesd upgrade --check` asks the release source. ariesd.yaml is
// strict, so a file that still sets it is refused, naming it.
func TestLatestIsNoSetting(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "ariesd.yaml")
	if err := os.WriteFile(p, []byte("latest: v1.2.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSettings(Settings{}, p, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "latest") {
		t.Errorf("ResolveSettings with latest: %v, want a refusal naming latest", err)
	}
}
