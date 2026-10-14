package statustext

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrint(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	n := func(v int) *int { return &v }
	const a, b = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f", "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	server := &proto.ServerStatus{Version: "v0.5.0", ProtoMin: 5, ProtoMax: 5, UptimeSeconds: 3*86400 + 4*3600 + 59,
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
			"starfixd v0.5.0, protocol 5 to 5, up 3d 4h\n" +
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
		// The database did not answer: unknown, never zero or a dash, and
		// one line for each cause.
		{"sessions unknown", proto.StatusResult{Projects: []proto.ProjectStatus{
			{ID: a, Name: "acme", State: "active", Open: true, Conns: 1, SessionsUnknown: "database did not answer in 2s", Writes: &proto.WriteStatus{}, LastUsed: &at},
			{ID: b, Name: "web", State: "active", Open: true, SessionsUnknown: "database did not answer in 2s", Writes: &proto.WriteStatus{}, LastUsed: &at},
			{ID: b, Name: "zed", State: "active", Open: true, SessionsUnknown: "the read failed;\nsee starfixd's log", Writes: &proto.WriteStatus{}, LastUsed: &at},
			{ID: b, Name: "zoo", State: "active", Open: true, Sessions: n(3), SessionsUnknown: "stale", Writes: &proto.WriteStatus{}, LastUsed: &at}}},
			"PROJECT  STATE   STORE  CONNS  BY HAND  SESSIONS  WRITES/MIN  P50  P99  LAST USED\n" +
				"acme     active  open   1      0        unknown   0           -    -    2026-10-11 09:30 UTC\n" +
				"web      active  open   0      0        unknown   0           -    -    2026-10-11 09:30 UTC\n" +
				"zed      active  open   0      0        unknown   0           -    -    2026-10-11 09:30 UTC\n" +
				"zoo      active  open   0      0        3         0           -    -    2026-10-11 09:30 UTC\n" +
				"sessions: unknown for 2 projects (database did not answer in 2s); the rest is what starfixd holds in memory. Check Dolt on the server (docs/server.md, Troubleshooting)\n" +
				"sessions: unknown for 1 project (the read failed;\\nsee starfixd's log); the rest is what starfixd holds in memory. Check Dolt on the server (docs/server.md, Troubleshooting)\n"},
		{"no projects on this page", proto.StatusResult{Server: &proto.ServerStatus{Version: "dev", ProtoMin: 5, ProtoMax: 5, UptimeSeconds: 42,
			OpenStoresMax: 64, ConnsMax: 1024,
			KeysFile: &proto.KeysFile{Path: "/var/lib/starfix/.ssh/starfix_keys", Keys: 1, WrittenAt: &at, Failed: true, HeldBack: 2}}},
			"starfixd dev, protocol 5 to 5, up 42s\n" +
				"stores 0 of 64, connections 0 of 1024\n" +
				"keys file /var/lib/starfix/.ssh/starfix_keys: THE LAST WRITE FAILED, so sshd may be reading an older list (1 key, written 2026-10-11 09:30 UTC); see starfixd's log\n" +
				"keys held back from it: 2, which the server's authorized_keys file gives to another principal; sshd takes that file's line for them, and starfixd's log names each\n" +
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
	t.Parallel()
	for _, tc := range []struct {
		secs int64
		want string
	}{{0, "0s"}, {59, "59s"}, {60, "1m"}, {61, "1m 1s"}, {3600, "1h"}, {3725, "1h 2m"}, {86400, "1d"}, {90000, "1d 1h"}, {400 * 86400, "400d"}, {-5, "0s"}} {
		if got := uptime(tc.secs); got != tc.want {
			t.Errorf("uptime(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

// The status says what the server has seen of its clock, and nothing
// while it has seen nothing.
func TestClockLines(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	behind := &proto.ClockBehind{Seconds: 3*3600 + 120, Since: at, Log: "project acme (database starfix_p001)", History: at.Add(3 * time.Hour)}
	stop := *behind
	stop.RefusesStart = true
	tests := []struct {
		name string
		c    proto.ClockStatus
		want string
	}{
		{"a quiet clock", proto.ClockStatus{Now: at}, ""},
		{"a jump forward", proto.ClockStatus{Now: at, StepsForward: 1, LastStepAt: &at, LastStepSeconds: 32400},
			"clock: set 1 time since starfixd started (1 forward, 0 back), the last forward by 9h0m0s at 2026-10-11 09:30 UTC; see starfixd's log for `clock stepped`\n"},
		{"a step back", proto.ClockStatus{Now: at, StepsForward: 1, StepsBackward: 2, LastStepAt: &at, LastStepSeconds: -90},
			"clock: set 3 times since starfixd started (1 forward, 2 back), the last back by 1m30s at 2026-10-11 09:30 UTC; see starfixd's log for `clock stepped`\n"},
		{"behind history, warn", proto.ClockStatus{Now: at, Behind: behind},
			"CLOCK BEHIND HISTORY: the server's clock is 3h2m0s behind the latest event of project acme (database starfix_p001) (2026-10-11 12:30 UTC), since 2026-10-11 09:30 UTC. starfixd's time runs on from there at half speed until the clock has caught up. On the server, check `timedatectl`\n"},
		{"behind history, stop", proto.ClockStatus{Now: at, Behind: &stop},
			"CLOCK BEHIND HISTORY: the server's clock is 3h2m0s behind the latest event of project acme (database starfix_p001) (2026-10-11 12:30 UTC), since 2026-10-11 09:30 UTC. starfixd's time runs on from there at half speed until the clock has caught up. STARFIXD WILL NOT START AGAIN until the clock is corrected (clock_behind: stop). On the server, check `timedatectl`\n"},
		{"a hostile server", proto.ClockStatus{Behind: &proto.ClockBehind{Log: "x\nfix: run this"}},
			"CLOCK BEHIND HISTORY: the server's clock is 0s behind the latest event of x\\nfix: run this (0001-01-01 00:00 UTC), since 0001-01-01 00:00 UTC. starfixd's time runs on from there at half speed until the clock has caught up. On the server, check `timedatectl`\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clockLines(tc.c); got != tc.want {
				t.Errorf("clockLines(%+v) =\n%s\nwant\n%s", tc.c, got, tc.want)
			}
			// Print puts the lines among the server's.
			var w bytes.Buffer
			Print(&w, proto.StatusResult{Server: &proto.ServerStatus{Clock: &tc.c}})
			if !strings.Contains(w.String(), tc.want+"starfixd's memory") {
				t.Errorf("Print lacks the clock lines before the memory line:\n%s", w.String())
			}
		})
	}
}

// The status says how people appear in the server's traces, and loudly
// when they are left out because the stand-in key is unavailable.
func TestTracesLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tr   proto.TracesStatus
		want string
	}{
		{"stand-ins", proto.TracesStatus{People: "pseudonym"}, "traces: people are named by stand-ins (telemetry: principals: pseudonym)\n"},
		{"names", proto.TracesStatus{People: "name"}, "traces: people are named by name (telemetry: principals: name)\n"},
		{"nobody, by the setting", proto.TracesStatus{People: "omit"}, "traces: nothing says who (telemetry: principals: omit)\n"},
		{"the key cannot be made", proto.TracesStatus{People: "unavailable", Cause: "it cannot be made: no such file or directory", KeyFile: "/var/lib/starfix/telemetry/principals.key"},
			"TRACES SAY NOTHING ABOUT WHO: the key for stand-ins, /var/lib/starfix/telemetry/principals.key, is unavailable: it cannot be made: no such file or directory. starfixd's log has the fix (`telemetry: spans say nothing about who`), and starfixd looks again every half minute\n"},
		{"the key's file cannot be trusted", proto.TracesStatus{People: "unavailable", Damaged: true, Cause: "its file is mode 0644: others can read it", KeyFile: "/var/lib/starfix/telemetry/principals.key"},
			"TRACES SAY NOTHING ABOUT WHO: the key for stand-ins, /var/lib/starfix/telemetry/principals.key, CANNOT BE TRUSTED: its file is mode 0644: others can read it. starfixd's log has the fix (`telemetry: spans say nothing about who`), and starfixd looks again every half minute\n"},
		{"a hostile server", proto.TracesStatus{People: "x\nfix: run this", Cause: "y\x1b[2K"}, "traces: x\\nfix: run this\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tracesLine(tc.tr); got != tc.want {
				t.Errorf("tracesLine(%+v) =\n%s\nwant\n%s", tc.tr, got, tc.want)
			}
			var b strings.Builder
			Print(&b, proto.StatusResult{Server: &proto.ServerStatus{Version: "v0.5.0", Traces: &tc.tr}})
			if !strings.Contains(b.String(), tc.want) {
				t.Errorf("Print does not show the line %q:\n%s", tc.want, b.String())
			}
		})
	}
	var b strings.Builder
	Print(&b, proto.StatusResult{Server: &proto.ServerStatus{Version: "v0.5.0"}})
	if strings.Contains(strings.ToLower(b.String()), "traces") {
		t.Errorf("a server that exports no traces has a line about them:\n%s", b.String())
	}
}

// The status says what starfixd last read of Dolt's own metrics: nothing
// when it reads none, "unknown" with the last success when the latest
// read failed, and otherwise each figure, with "unknown" for one Dolt
// did not give. The collector's cycles get a verdict only with a memory
// limit set and enough writes to judge by.
func TestDoltLines(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	u := func(v uint64) *uint64 { return &v }
	gc := func(cycles, writes int64) *proto.DoltGC {
		g := &proto.DoltGC{WindowSeconds: 600, Cycles: cycles, Writes: writes}
		if writes > 0 {
			g.Per100Writes = new(100 * float64(cycles) / float64(writes))
		}
		return g
	}
	full := proto.DoltStatus{ReadAt: &at, Version: "2.4.2", StartedAt: &at, UptimeSeconds: new(int64(3*86400 + 4*3600)),
		ResidentBytes: u(1234 << 20), Conns: new(14), MemLimitBytes: u(1500 << 20)}
	with := func(g *proto.DoltGC, limit *uint64) proto.DoltStatus {
		d := full
		d.GC, d.MemLimitBytes = g, limit
		return d
	}
	const head = "Dolt 2.4.2, up 3d 4h, 14 connections, 1234 MiB resident, "
	tests := []struct {
		name string
		d    proto.DoltStatus
		want string
	}{
		{"before a second reading", full, head + "memory limit 1500 MiB\n"},
		{"a limit with room", with(gc(37, 2051), u(1500<<20)),
			head + "memory limit 1500 MiB\n" +
				"Dolt's collector: 37 cycles for 2051 writes in 10m, 1.8 for each 100 writes: under 3, as with a memory limit that had room in the measurements (over 14 was one too low)\n"},
		{"a limit too low", with(gc(1762, 6000), u(850<<20)),
			head + "memory limit 850 MiB\n" +
				"Dolt's collector: 1762 cycles for 6000 writes in 10m, 29 for each 100 writes: OVER 14, AS WITH A MEMORY LIMIT TOO LOW in the measurements: Dolt is spending its CPU collecting. Raise GOMEMLIMIT or restart Dolt (docs/server.md, Dolt's memory)\n"},
		{"between the two", with(gc(220, 3000), u(900<<20)),
			head + "memory limit 900 MiB\n" +
				"Dolt's collector: 220 cycles for 3000 writes in 10m, 7.3 for each 100 writes: between the 3 of a memory limit with room and the 14 of one too low, in the measurements\n"},
		{"exactly at a threshold is between", with(gc(15, 500), u(900<<20)),
			head + "memory limit 900 MiB\n" +
				"Dolt's collector: 15 cycles for 500 writes in 10m, 3.0 for each 100 writes: between the 3 of a memory limit with room and the 14 of one too low, in the measurements\n"},
		// An idle Dolt collects about every two minutes, whatever is
		// written: a few writes say nothing of the limit.
		{"too few writes to judge by", with(gc(5, 499), u(900<<20)),
			head + "memory limit 900 MiB\n" +
				"Dolt's collector: 5 cycles for 499 writes in 10m: too few writes to judge the memory limit by (it takes 500)\n"},
		{"no writes", with(gc(5, 0), u(900<<20)),
			head + "memory limit 900 MiB\n" +
				"Dolt's collector: 5 cycles for 0 writes in 10m: too few writes to judge the memory limit by (it takes 500)\n"},
		// A limit far too low has Dolt collecting without pause whether or
		// not anything is written: more cycles than 500 writes would
		// excuse is a verdict by itself.
		{"few writes, and cycles past what 500 would excuse", with(gc(502, 0), u(120<<20)),
			head + "memory limit 120 MiB\n" +
				"Dolt's collector: 502 cycles for 0 writes in 10m: over 14 for each 100 even counted against 500 writes, AS WITH A MEMORY LIMIT TOO LOW in the measurements: Dolt is spending its CPU collecting. Raise GOMEMLIMIT or restart Dolt (docs/server.md, Dolt's memory)\n"},
		{"few writes, and exactly what 500 would excuse", with(gc(70, 499), u(900<<20)),
			head + "memory limit 900 MiB\n" +
				"Dolt's collector: 70 cycles for 499 writes in 10m: too few writes to judge the memory limit by (it takes 500)\n"},
		{"few writes, cycles past that, and no limit", with(gc(502, 0), u(0)),
			head + "no memory limit\n" +
				"Dolt's collector: 502 cycles for 0 writes in 10m\n"},
		{"no limit, so no verdict", with(gc(1762, 6000), u(0)),
			head + "no memory limit\n" +
				"Dolt's collector: 1762 cycles for 6000 writes in 10m, 29 for each 100 writes\n"},
		{"no limit and few writes", with(gc(5, 1), u(0)),
			head + "no memory limit\n" +
				"Dolt's collector: 5 cycles for 1 write in 10m\n"},
		{"a limit Dolt did not report", with(gc(1762, 6000), nil),
			head + "memory limit unknown\n" +
				"Dolt's collector: 1762 cycles for 6000 writes in 10m, 29 for each 100 writes\n"},
		{"a short window", proto.DoltStatus{ReadAt: &at, Version: "2.4.2", MemLimitBytes: u(0), GC: &proto.DoltGC{WindowSeconds: 45, Cycles: 1, Writes: 2, Per100Writes: new(50.0)}},
			"Dolt 2.4.2, uptime unknown, connections unknown, memory unknown, no memory limit\n" +
				"Dolt's collector: 1 cycle for 2 writes in 45s\n"},
		{"one figure only", proto.DoltStatus{ReadAt: &at, Conns: new(1)},
			"Dolt, version unknown, uptime unknown, 1 connection, memory unknown, memory limit unknown\n"},
		{"unknown, read before", proto.DoltStatus{Unknown: true, ReadAt: &at},
			"Dolt: unknown: starfixd could not read Dolt's metrics, and last could at 2026-10-11 09:30 UTC; see starfixd's log\n"},
		{"unknown, never read", proto.DoltStatus{Unknown: true},
			"Dolt: unknown: starfixd has not read Dolt's metrics since it started; see starfixd's log\n"},
		// Figures that came with a failed read are not shown.
		{"unknown wins over figures", proto.DoltStatus{Unknown: true, ReadAt: &at, Version: "2.4.2", Conns: new(3)},
			"Dolt: unknown: starfixd could not read Dolt's metrics, and last could at 2026-10-11 09:30 UTC; see starfixd's log\n"},
		{"a hostile server", proto.DoltStatus{Version: "2\nfix: run this"},
			"Dolt 2\\nfix: run this, uptime unknown, connections unknown, memory unknown, memory limit unknown\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := doltLines(tc.d); got != tc.want {
				b, _ := json.Marshal(tc.d)
				t.Errorf("doltLines(%s) =\n%s\nwant\n%s", b, got, tc.want)
			}
			// Print puts the lines after starfixd's own memory.
			var w bytes.Buffer
			Print(&w, proto.StatusResult{Server: &proto.ServerStatus{Dolt: &tc.d}})
			if !strings.Contains(w.String(), "goroutines\n"+tc.want+"projects: ") {
				t.Errorf("Print lacks Dolt's lines between starfixd's memory and the projects:\n%s", w.String())
			}
		})
	}
	// A server that reads nothing of Dolt says nothing of it.
	var w bytes.Buffer
	Print(&w, proto.StatusResult{Server: &proto.ServerStatus{}})
	if strings.Contains(w.String(), "Dolt") {
		t.Errorf("Print names Dolt with no Dolt status:\n%s", w.String())
	}
}
