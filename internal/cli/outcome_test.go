package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/outcome"
)

// outcome refuses a mistake on its command line before it connects.
func TestOutcomeUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"outcome"}, "outcome needs issue ids or --since"},
		{[]string{"outcome", "ar-a1b2", "--since", "7d"}, "give issue ids or --since, not both"},
		{[]string{"outcome", "ar-a1b2", "--backfill"}, "--backfill needs --since"},
		{[]string{"outcome", "--since", "lately"}, "--since:"},
	} {
		code, _, stderr := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
		if code != ExitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("aries %s: exit %d, %q; want a usage error with %q", strings.Join(tc.args, " "), code, stderr, tc.want)
		}
	}
	if !strings.Contains(helpText(t, "outcome"), "usage: aries "+outcomeUsage) {
		t.Errorf("help outcome does not give its usage")
	}
}

func TestSinceTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"30d", now.AddDate(0, 0, -30)},
		{"36h", now.Add(-36 * time.Hour)},
		{"2026-10-01", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-10-01T09:30:00Z", time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)},
	} {
		got, err := sinceTime(tc.in, now)
		if err != nil || !got.Equal(tc.want) {
			t.Errorf("sinceTime(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

// Each record's line says what was done with it; a dry run prints the
// comment it would post.
func TestPrintOutcomes(t *testing.T) {
	t.Parallel()
	rec := outcome.Record{V: 1, Issue: "ar-a1b2", Seq: 1, Outcome: outcome.Landed}
	for _, tc := range []struct {
		name string
		doc  outcomeDoc
		want []string
	}{
		{"none", outcomeDoc{}, []string{"no outcome to record\n"}},
		{"posted", outcomeDoc{Records: []outcomeRow{{Issue: "ar-a1b2", Seq: 1, Action: "post", Comment: "c9", Record: rec}}},
			[]string{"ar-a1b2 seq 1 landed: posted c9\n"}},
		{"superseded", outcomeDoc{Records: []outcomeRow{{Issue: "ar-a1b2", Seq: 1, Action: "supersede", Comment: "c9", Supersedes: "c1", Record: rec}}},
			[]string{"ar-a1b2 seq 1 landed: posted c9, superseding c1\n"}},
		{"unchanged", outcomeDoc{Records: []outcomeRow{{Issue: "ar-a1b2", Seq: 1, Action: "unchanged", Record: rec}}},
			[]string{"ar-a1b2 seq 1 landed: unchanged, nothing posted\n"}},
		{"dry run", outcomeDoc{DryRun: true, Records: []outcomeRow{{Issue: "ar-a1b2", Seq: 1, Action: "post", Record: rec}}},
			[]string{"ar-a1b2 seq 1 landed: would post\noutcome: v1 landed seq 1\n{\"v\":1,\"issue\":\"ar-a1b2\""}},
	} {
		var out bytes.Buffer
		r := &runner{env: Env{Stdout: &out}}
		r.printOutcomes(tc.doc)
		for _, w := range tc.want {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: printed %q, want %q in it", tc.name, out.String(), w)
			}
		}
	}
}
