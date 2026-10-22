package guard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Pruning removes the snapshots last taken before the cutoff and keeps
// the newer ones and the denial count.
func TestPruneSnapshots(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	old, _ := SnapshotPath(data, "old")
	fresh, _ := SnapshotPath(data, "fresh")
	for _, p := range []string{old, fresh} {
		if err := WriteSnapshot(p, Snapshot(Desktop, nil, false)); err != nil {
			t.Fatal(err)
		}
	}
	count := filepath.Join(SnapshotDir(data), "denials.jsonl")
	if err := os.WriteFile(count, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, p := range []string{old, count} {
		if err := os.Chtimes(p, now.Add(-2*SnapshotAge), now.Add(-2*SnapshotAge)); err != nil {
			t.Fatal(err)
		}
	}
	PruneSnapshots(data, now.Add(-SnapshotAge))
	for p, want := range map[string]bool{old: false, fresh: true, count: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s: kept = %v; want %v", filepath.Base(p), err == nil, want)
		}
	}
}
