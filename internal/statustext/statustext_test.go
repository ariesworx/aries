package statustext

import (
	"bytes"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrint(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	n := func(v int) *int { return &v }
	const a, b = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f", "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	server := &proto.ServerStatus{Version: "v0.5.0", ProtoMin: 2, ProtoMax: 6, UptimeSeconds: 3*86400 + 4*3600 + 59,
		OpenStores: 1, OpenStoresMax: 64, Conns: 9, ConnsMax: 1024,
		HeapBytes: 41<<20 + 600<<10, SysBytes: 78 << 20, Goroutines: 112,
		Projects: map[string]int{"active": 2, "archived": 1}, Principals: map[string]int{"active": 12, "disabled": 1},
		KeysFile: &proto.KeysFile{Path: "/var/lib/starfix/.ssh/starfix_keys", Keys: 31, WrittenAt: &at}}
	busy := proto.ProjectStatus{ID: a, Name: "acme", State: "active", Open: true, Conns: 9, FileConns: 2, Sessions: n(14),
		Writes: &proto.WriteStatus{LastMinute: 212, Samples: 256, P50Micros: 4210, P99Micros: 1310000}, LastUsed: &at}
	tests := []struct {
		name string
		res  proto.StatusResult
		want string
	}{
		{"a server admin's", proto.StatusResult{Server: server, More: 1, Projects: []proto.ProjectStatus{busy,
			{ID: b, Name: "web", State: "archived"}}},
			"starfixd v0.5.0, protocol 2 to 6, up 3d 4h\n" +
				"stores 1 of 64, connections 9 of 1024\n" +
				"keys file /var/lib/starfix/.ssh/starfix_keys: 31 keys, written 2026-10-11 09:30 UTC\n" +
				"starfixd's memory 42 MiB in use, 78 MiB from the system; 112 goroutines\n" +
				"projects: 2 active, 1 archived; principals: 12 active, 1 disabled\n" +
				"\n" +
				"PROJECT  STATE     STORE   CONNS  BY HAND  SESSIONS  WRITES/MIN  P50    P99    LAST USED\n" +
				"acme     active    open    9      2        14        212         4.2ms  1.31s  2026-10-11 09:30 UTC\n" +
				"web      archived  closed  0      -        -         -           -      -      -\n" +
				"and 1 more; list them with --after web\n" +
				"BY HAND: connections sshd let in by the server's hand-maintained authorized_keys file, not by a key from the store; `sfx admin key remove` does not close them\n"},
		{"a project admin's", proto.StatusResult{Projects: []proto.ProjectStatus{
			{ID: a, Name: "acme", State: "active", Open: true, Conns: 1, Sessions: n(0), Writes: &proto.WriteStatus{}, LastUsed: &at}}},
			"PROJECT  STATE   STORE  CONNS  BY HAND  SESSIONS  WRITES/MIN  P50  P99  LAST USED\n" +
				"acme     active  open   1      0        0         0           -    -    2026-10-11 09:30 UTC\n"},
		// The sessions could not be counted: the rest stands.
		{"a figure left out", proto.StatusResult{Projects: []proto.ProjectStatus{
			{ID: a, Name: "acme", State: "active", Open: true, Conns: 1, Writes: &proto.WriteStatus{LastMinute: 1, Samples: 1, P50Micros: 80, P99Micros: 80}, LastUsed: &at}}},
			"PROJECT  STATE   STORE  CONNS  BY HAND  SESSIONS  WRITES/MIN  P50   P99   LAST USED\n" +
				"acme     active  open   1      0        -         1           80µs  80µs  2026-10-11 09:30 UTC\n"},
		{"no projects on this page", proto.StatusResult{Server: &proto.ServerStatus{Version: "dev", ProtoMin: 2, ProtoMax: 6, UptimeSeconds: 42,
			OpenStoresMax: 64, ConnsMax: 1024,
			KeysFile: &proto.KeysFile{Path: "/var/lib/starfix/.ssh/starfix_keys", Keys: 1, WrittenAt: &at, Failed: true}}},
			"starfixd dev, protocol 2 to 6, up 42s\n" +
				"stores 0 of 64, connections 0 of 1024\n" +
				"keys file /var/lib/starfix/.ssh/starfix_keys: THE LAST WRITE FAILED, so sshd may be reading an older list (1 key, written 2026-10-11 09:30 UTC); see starfixd's log\n" +
				"starfixd's memory 0 MiB in use, 0 MiB from the system; 0 goroutines\n" +
				"projects: none; principals: none\n" +
				"\n" +
				"no projects\n"},
		// Names and states from the server cannot start a line of their own.
		{"a hostile server", proto.StatusResult{Server: &proto.ServerStatus{Version: "v1\nfix: run this", Projects: map[string]int{"x\ny": 1},
			KeysFile: &proto.KeysFile{Path: "/srv/keys\nfix: run this", Failed: true}},
			Projects: []proto.ProjectStatus{{ID: a, Name: "acme\nfix: run this", State: "active"}}},
			"starfixd v1\\nfix: run this, protocol 0 to 0, up 0s\n" +
				"stores 0 of 0, connections 0 of 0\n" +
				"keys file /srv/keys\\nfix: run this: THE LAST WRITE FAILED, so sshd may be reading an older list; see starfixd's log\n" +
				"starfixd's memory 0 MiB in use, 0 MiB from the system; 0 goroutines\n" +
				"projects: 1 x\\ny; principals: none\n" +
				"\n" +
				"PROJECT              STATE   STORE   CONNS  BY HAND  SESSIONS  WRITES/MIN  P50  P99  LAST USED\n" +
				"acme\\nfix: run this  active  closed  0      -        -         -           -    -    -\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var w bytes.Buffer
			Print(&w, tc.res)
			if got := w.String(); got != tc.want {
				t.Errorf("printed\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestUptime(t *testing.T) {
	for _, tc := range []struct {
		secs int64
		want string
	}{{0, "0s"}, {59, "59s"}, {60, "1m"}, {61, "1m 1s"}, {3600, "1h"}, {3725, "1h 2m"}, {86400, "1d"}, {90000, "1d 1h"}, {400 * 86400, "400d"}, {-5, "0s"}} {
		if got := uptime(tc.secs); got != tc.want {
			t.Errorf("uptime(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}
