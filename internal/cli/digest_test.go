package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

func TestPrintDigest(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return until.Add(-d) }
	tests := []struct {
		name string
		d    proto.DigestResult
		want string
	}{
		{"quiet", proto.DigestResult{Since: ago(24 * time.Hour), Until: until},
			"since 2026-10-06 12:00 UTC (1d): 0 events\n"},
		{"every section", proto.DigestResult{
			Since: ago(7 * 24 * time.Hour), Until: until, Truncated: true,
			Totals: proto.DigestTotals{Events: 40, Closed: 12, Started: 1, InProgress: 1, Stalled: 1, Blocked: 1,
				HandedOff: 1, Created: 1, Discovered: 1, Conflicts: 1},
			Closed:     []proto.DigestItem{{ID: "ar-a1", Title: "Fix the login redirect", Priority: 1, By: "bob", At: ago(3 * time.Hour)}},
			Started:    []proto.DigestItem{{ID: "ar-b2", Title: "Add digest", Priority: 2, By: "alice", At: ago(90 * time.Minute)}},
			InProgress: []proto.DigestItem{{ID: "ar-b2", Title: "Add digest", Priority: 2, By: "alice", At: ago(28 * time.Hour)}},
			Stalled:    []proto.DigestItem{{ID: "ar-c3", Title: "Old work", Priority: 3, By: "carol", At: ago(9 * 24 * time.Hour)}},
			Blocked:    []proto.DigestItem{{ID: "ar-d4", Title: "Ship it", Priority: 0, BlockedBy: []string{"ar-a1", "ar-b2"}}},
			HandedOff: []proto.DigestItem{{ID: "ar-a1", Title: "Fix the login redirect", Priority: 1, By: "bob", At: ago(3 * time.Hour),
				Note: "cookie path\nis gone; " + strings.Repeat("x", 80)}},
			Created:    []proto.DigestItem{{ID: "ar-e5", Title: "Flaky test", Priority: 2, By: "bob", At: ago(3 * time.Hour)}},
			Discovered: []proto.DigestItem{{ID: "ar-e5", Title: "Flaky test", Priority: 2, By: "bob", At: ago(3 * time.Hour), From: "ar-a1"}},
			Conflicts:  []proto.DigestItem{{ID: "ar-a1", Title: "Fix the login redirect", Priority: 1, By: "alice", At: ago(50 * time.Hour), Note: "title (conflict 3)"}},
		}, `since 2026-09-30 12:00 UTC (7d): 40 events, some totals partial
closed 12, 1 shown
  ar-a1  P1  Fix the login redirect  bob  3h ago
started 1
  ar-b2  P2  Add digest  alice  1h ago
in progress 1
  ar-b2  P2  Add digest  alice  for 1d4h
stalled 1
  ar-c3  P3  Old work  carol  idle 9d
blocked 1
  ar-d4  P0  Ship it  by ar-a1, ar-b2
handed off 1
  ar-a1  P1  Fix the login redirect  bob  3h ago  cookie path is gone; ` + strings.Repeat("x", 39) + `…
created 1
  ar-e5  P2  Flaky test  bob  3h ago
discovered 1
  ar-e5  P2  Flaky test  bob  3h ago  from ar-a1
old conflicts 1
  ar-a1  P1  Fix the login redirect  alice  parked 2d2h ago  title (conflict 3)
`},
		{"dates", proto.DigestResult{
			Since: ago(24 * time.Hour), Until: until,
			Totals:  proto.DigestTotals{Events: 2, Ready: 1, DueSoon: 1, Overdue: 2},
			Ready:   []proto.DigestItem{{ID: "ar-m1", Title: "Maintenance window", Priority: 1, By: "bob", At: ago(2 * time.Hour)}},
			DueSoon: []proto.DigestItem{{ID: "ar-r1", Title: "Quarterly report", Priority: 2, At: until.Add(20 * time.Hour)}},
			Overdue: []proto.DigestItem{{ID: "ar-m1", Title: "Maintenance window", Priority: 1, By: "bob", At: ago(time.Hour)}},
		}, `since 2026-10-06 12:00 UTC (1d): 2 events
ready since its date 1
  ar-m1  P1  Maintenance window  bob  from 2026-10-07 10:00 UTC
due soon 1
  ar-r1  P2  Quarterly report    due 2026-10-08 08:00 UTC, in 20h
overdue 2, 1 shown
  ar-m1  P1  Maintenance window  bob  due 2026-10-07 11:00 UTC, 1h ago
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printDigest(&b, tc.d)
			if b.String() != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", b.String(), tc.want)
			}
		})
	}
}
