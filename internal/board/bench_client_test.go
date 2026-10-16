package board

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Benchmarks of `aries tui`'s model (docs/design/profile-aries.md). Run with:
//
//	go test -run '^$' -bench Client -benchmem ./internal/board/

// benchSnapshot is the most one refresh reads, whatever the project's
// size: listLimit ready issues, listLimit blocked ones and heldLimit
// claims.
func benchSnapshot(gen int) *Snapshot {
	at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	s := &Snapshot{ServerNow: at, LocalNow: at}
	title := func(i int) string {
		return fmt.Sprintf("Reconciler loses the first of two overlapping writes (case %d, read %d)", i, gen)
	}
	for i := range listLimit {
		s.Ready = append(s.Ready, proto.Summary{ID: fmt.Sprintf("ar-r%07d", i), Title: title(i), Status: "open", Priority: i % 5})
		s.Blocked = append(s.Blocked, proto.BlockedIssue{Summary: proto.Summary{ID: fmt.Sprintf("ar-b%07d", i), Title: title(i), Status: "blocked"},
			BlockedBy: []string{"ar-r0000001", "ar-r0000002"}})
	}
	for i := range heldLimit {
		s.Held = append(s.Held, Held{Title: title(i), Priority: i % 5, Claim: proto.Claim{Issue: fmt.Sprintf("ar-h%07d", i), By: "alice",
			Session: "20000000-0000-4000-8000-000000000001", Machine: "laptop", Epoch: 3, ClaimedAt: at, ExpiresAt: at.Add(15 * time.Minute)}})
	}
	return s
}

func benchEvent(seq int64) proto.Event {
	return proto.Event{Seq: seq, At: time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC), Principal: "alice", Op: "issue.update",
		Issue: "ar-r0000001", After: json.RawMessage(`{"title":"Reconciler loses the first of two overlapping writes","priority":2}`)}
}

// BenchmarkClientModelRefresh is one reread applied to the board: the
// largest snapshot replaces the last, and a frame is drawn.
func BenchmarkClientModelRefresh(b *testing.B) {
	m := New("example as alice")
	m.Resize(160, 50)
	snaps := []*Snapshot{benchSnapshot(0), benchSnapshot(1)}
	now := time.Date(2026, 10, 13, 9, 1, 0, 0, time.UTC)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		m.Apply(snaps[i%2])
		m.Frame(160, 50, now, true)
	}
}

// BenchmarkClientModelEvents is one pushed event put in the tail, which
// holds the last TailMax, and a frame drawn.
func BenchmarkClientModelEvents(b *testing.B) {
	m := New("example as alice")
	m.Resize(160, 50)
	m.Apply(benchSnapshot(0))
	now := time.Date(2026, 10, 13, 9, 1, 0, 0, time.UTC)
	b.ReportAllocs()
	for i := int64(0); b.Loop(); i++ {
		m.Apply(Pushed{benchEvent(i)})
		m.Frame(160, 50, now, true)
	}
	if n := len(m.Tail()); n > TailMax {
		b.Fatalf("the tail holds %d events, want at most %d", n, TailMax)
	}
}
