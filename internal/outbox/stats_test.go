package outbox_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/outbox"
	"github.com/ariesworx/starfix/internal/proto"
)

func mustStats(t *testing.T, o *outbox.Outbox) outbox.Stats {
	t.Helper()
	s, err := o.Stats()
	if err != nil {
		t.Fatalf("Stats() = %v", err)
	}
	return s
}

func comment(i int) outbox.Entry {
	return outbox.Entry{Op: proto.OpComment, Args: fmt.Appendf(nil, `{"id":"sfx-1","body":"body %d","idem":"c-%d"}`, i, i)}
}

// wait is a WaitStats of one wait of d, which falls in bucket.
func wait(d time.Duration, bucket int) outbox.WaitStats {
	w := outbox.WaitStats{Count: 1, Total: d, Max: d}
	w.Buckets[bucket] = 1
	return w
}

func TestStatsCountsEachOp(t *testing.T) {
	clk := newClock()
	o, _ := open(t, outbox.Options{}, clk)
	if got := mustStats(t, o); len(got.Ops) != 0 || got.Outages != (outbox.OutageStats{}) {
		t.Errorf("Stats() of a new outbox = %+v, want nothing counted", got)
	}

	mustAppend(t, o, handoff(1))
	mustAppend(t, o, handoff(2))
	mustAppend(t, o, comment(3))
	clk.Advance(30 * time.Second)
	mustReplay(t, o, &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: e.Seq == 2, Code: "conflict"}, nil
	}})
	got := mustStats(t, o)
	if want := (outbox.OpStats{Queued: 2, Delivered: 1, Refused: 1, Wait: wait(30*time.Second, 2)}); got.Ops[proto.OpHandoff] != want {
		t.Errorf("Stats() handoff = %+v, want %+v", got.Ops[proto.OpHandoff], want)
	}
	if want := (outbox.OpStats{Queued: 1, Delivered: 1, Wait: wait(30*time.Second, 2)}); got.Ops[proto.OpComment] != want {
		t.Errorf("Stats() comment = %+v, want %+v", got.Ops[proto.OpComment], want)
	}
	if len(got.Ops) != 2 {
		t.Errorf("Stats() has %d ops, want the 2 that were queued", len(got.Ops))
	}

	// Dropped by hand, pending and refused alike; and stale.
	e := mustAppend(t, o, comment(4))
	if err := o.Drop(e.Seq); err != nil {
		t.Fatalf("Drop(%d) = %v", e.Seq, err)
	}
	if err := o.Drop(2); err != nil {
		t.Fatalf("Drop(2) = %v", err)
	}
	mustAppend(t, o, comment(5))
	clk.Advance(25 * time.Hour)
	mustReplay(t, o, &server{})
	got = mustStats(t, o)
	if want := (outbox.OpStats{Queued: 2, Delivered: 1, Refused: 1, Dropped: 1, Wait: wait(30*time.Second, 2)}); got.Ops[proto.OpHandoff] != want {
		t.Errorf("Stats() handoff = %+v, want %+v", got.Ops[proto.OpHandoff], want)
	}
	if want := (outbox.OpStats{Queued: 3, Delivered: 1, Dropped: 1, Stale: 1, Wait: wait(30*time.Second, 2)}); got.Ops[proto.OpComment] != want {
		t.Errorf("Stats() comment = %+v, want %+v", got.Ops[proto.OpComment], want)
	}
}

func TestStatsWaitBuckets(t *testing.T) {
	tests := []struct {
		waited time.Duration
		bucket int
	}{
		{0, 0},
		{time.Second, 0},
		{time.Second + time.Nanosecond, 1},
		{10 * time.Second, 1},
		{59 * time.Second, 2},
		{10 * time.Minute, 3},
		{time.Hour, 4},
		{6 * time.Hour, 5},
		{23 * time.Hour, 6},
		{24 * time.Hour, 6},
		{25 * time.Hour, 7},
		{90 * 24 * time.Hour, 7},
	}
	for _, tc := range tests {
		t.Run(tc.waited.String(), func(t *testing.T) {
			clk := newClock()
			o, _ := open(t, outbox.Options{MaxAge: 100 * 24 * time.Hour}, clk)
			mustAppend(t, o, handoff(1))
			clk.Advance(tc.waited)
			mustReplay(t, o, &server{})
			if got, want := mustStats(t, o).Ops[proto.OpHandoff].Wait, wait(tc.waited, tc.bucket); got != want {
				t.Errorf("after a wait of %s, Stats() wait = %+v, want %+v", tc.waited, got, want)
			}
		})
	}
	if n := len(outbox.WaitBounds) + 1; n != len(outbox.WaitStats{}.Buckets) {
		t.Errorf("WaitStats has %d buckets, want one for each of WaitBounds and one beyond: %d", len(outbox.WaitStats{}.Buckets), n)
	}
}

func TestStatsWaitSummary(t *testing.T) {
	clk := newClock()
	o, _ := open(t, outbox.Options{}, clk)
	for i, waited := range []time.Duration{5 * time.Second, 2 * time.Minute, 8 * time.Second} {
		mustAppend(t, o, handoff(i))
		clk.Advance(waited)
		mustReplay(t, o, &server{})
	}
	want := outbox.WaitStats{Count: 3, Total: 2*time.Minute + 13*time.Second, Max: 2 * time.Minute}
	want.Buckets[1], want.Buckets[3] = 2, 1
	if got := mustStats(t, o).Ops[proto.OpHandoff].Wait; got != want {
		t.Errorf("Stats() wait = %+v, want %+v", got, want)
	}
}

func TestStatsOutages(t *testing.T) {
	clk := newClock()
	o, _ := open(t, outbox.Options{}, clk)
	down := &server{answer: func(outbox.Entry) (outbox.Outcome, error) { return outbox.Outcome{}, errors.New("down") }}

	// The first outage: 10 minutes, two entries, one failed replay.
	clk.Advance(time.Hour)
	began := clk.Now()
	mustAppend(t, o, handoff(1))
	clk.Advance(4 * time.Minute)
	mustAppend(t, o, handoff(2))
	mustReplay(t, o, down)
	if got, want := mustStats(t, o).Outages, (outbox.OutageStats{Since: began}); !sameOutages(got, want) {
		t.Errorf("during the first outage, Stats() outages = %+v, want %+v", got, want)
	}
	clk.Advance(6 * time.Minute)
	mustReplay(t, o, &server{})
	if got, want := mustStats(t, o).Outages, (outbox.OutageStats{Count: 1, Total: 10 * time.Minute, Longest: 10 * time.Minute}); !sameOutages(got, want) {
		t.Errorf("after the first outage, Stats() outages = %+v, want %+v", got, want)
	}

	// The second: 3 minutes, and ended by a refusal, which is the
	// server answering too.
	clk.Advance(time.Hour)
	mustAppend(t, o, handoff(3))
	clk.Advance(3 * time.Minute)
	mustReplay(t, o, &server{answer: func(outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: true, Code: "forbidden"}, nil
	}})
	if got, want := mustStats(t, o).Outages, (outbox.OutageStats{Count: 2, Total: 13 * time.Minute, Longest: 10 * time.Minute}); !sameOutages(got, want) {
		t.Errorf("after the second outage, Stats() outages = %+v, want %+v", got, want)
	}

	// The third is still going: an entry that goes stale was never
	// answered, so it ends nothing.
	clk.Advance(time.Hour)
	began = clk.Now()
	mustAppend(t, o, handoff(4))
	clk.Advance(25 * time.Hour)
	mustReplay(t, o, &server{})
	if got, want := mustStats(t, o).Outages, (outbox.OutageStats{Count: 2, Total: 13 * time.Minute, Longest: 10 * time.Minute, Since: began}); !sameOutages(got, want) {
		t.Errorf("during the third outage, Stats() outages = %+v, want %+v", got, want)
	}
}

func sameOutages(a, b outbox.OutageStats) bool {
	return a.Count == b.Count && a.Total == b.Total && a.Longest == b.Longest && a.Since.Equal(b.Since)
}

func TestStatsFileIsSafeToLose(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "not JSON", content: "{{{"},
		{name: "empty", content: ""},
		{name: "JSON of another shape", content: `{"ops":[1,2,3],"outages":"many"}`},
		{name: "half a file", content: `{"ops":{"handoff":{"queued":3,"deliv`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, dir := open(t, outbox.Options{}, nil)
			if err := os.WriteFile(filepath.Join(dir, "stats.json"), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := mustStats(t, o); len(got.Ops) != 0 {
				t.Errorf("Stats() of a file that does not parse = %+v, want nothing counted", got)
			}
			mustAppend(t, o, handoff(1))
			if got := mustStats(t, o).Ops[proto.OpHandoff].Queued; got != 1 {
				t.Errorf("after an append over a file that does not parse, Stats() handoff queued = %d, want 1", got)
			}
		})
	}
	t.Run("removed", func(t *testing.T) {
		o, dir := open(t, outbox.Options{}, nil)
		mustAppend(t, o, handoff(1))
		if err := os.Remove(filepath.Join(dir, "stats.json")); err != nil {
			t.Fatal(err)
		}
		if rep := mustReplay(t, o, &server{}); rep.Delivered != 1 {
			t.Errorf("Replay() without the counts = %+v, want 1 delivered", rep)
		}
		if got := mustStats(t, o).Ops[proto.OpHandoff]; got.Queued != 0 || got.Delivered != 1 {
			t.Errorf("Stats() = %+v, want what was counted since the file was lost", got)
		}
	})
}

func TestStatsHoldNoText(t *testing.T) {
	clk := newClock()
	o, dir := open(t, outbox.Options{}, clk)
	texts := []string{"zebra-note", "quagga-key", "okapi-session", "tapir-machine", "sfx-4242", "gnu-message", "ibex-fix"}
	for i := range 3 {
		mustAppend(t, o, outbox.Entry{
			Op:      proto.OpHandoff,
			Args:    fmt.Appendf(nil, `{"id":"sfx-4242","note":"zebra-note","idem":"quagga-key-%d"}`, i),
			Session: "okapi-session", Machine: "tapir-machine",
		})
	}
	clk.Advance(time.Minute)
	mustReplay(t, o, &server{answer: func(e outbox.Entry) (outbox.Outcome, error) {
		return outbox.Outcome{Refused: e.Seq == 1, Code: "conflict", Message: "gnu-message", Fix: "ibex-fix"}, nil
	}})
	if err := o.Drop(1); err != nil {
		t.Fatalf("Drop(1) = %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "stats.json")) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range texts {
		if strings.Contains(string(b), text) {
			t.Errorf("the counts file holds %q, an entry's text:\n%s", text, b)
		}
	}
}

func TestStatsFileIsBounded(t *testing.T) {
	// Only the ops the outbox queues are kept, so the file has a fixed
	// size however it is used, and whatever was put in it.
	o, dir := open(t, outbox.Options{}, nil)
	planted := `{"ops":{"handoff":{"queued":4},"frobnicate":{"queued":9},"start":{"queued":1}},"outages":{"count":2}}`
	path := filepath.Join(dir, "stats.json")
	if err := os.WriteFile(path, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	got := mustStats(t, o)
	if len(got.Ops) != 1 || got.Ops[proto.OpHandoff].Queued != 4 || got.Outages.Count != 2 {
		t.Errorf("Stats() = %+v, want handoff's 4 and the 2 outages, and no op the outbox does not queue", got)
	}
	mustAppend(t, o, handoff(1))
	b, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "frobnicate") || strings.Contains(string(b), "start") {
		t.Errorf("after an append the counts file still names ops the outbox does not queue:\n%s", b)
	}
	if got := mustStats(t, o).Ops[proto.OpHandoff].Queued; got != 5 {
		t.Errorf("Stats() handoff queued = %d, want 5", got)
	}
}
