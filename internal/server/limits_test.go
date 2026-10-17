package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

func TestResolveSettingsLimits(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		body string
		want func(Limits) bool
		err  string
	}{
		{name: "defaults", body: "", want: func(l Limits) bool { return l == DefaultLimits }},
		{name: "set", body: "limits:\n  labels_per_issue: 9\n  conns_per_principal: 4\n  idle_timeout: 90s\n  writes_per_second: 2.5\n  agent_keep: 14d\n",
			want: func(l Limits) bool {
				return l.Labels == 9 && l.ConnsPerPrincipal == 4 && l.IdleTimeout == Duration(90*time.Second) &&
					l.WriteRate == 2.5 && l.AgentKeep == Duration(14*24*time.Hour) && l.Conns == DefaultLimits.Conns
			}},
		{name: "zero takes the default", body: "limits:\n  labels_per_issue: 0\n  write_burst: 0\n  writes_per_second: 0\n",
			want: func(l Limits) bool { return l == DefaultLimits }},
		{name: "handshakes default", body: "limits:\n  handshakes_per_principal: 0\n", want: func(l Limits) bool { return l.Handshakes == 16 }},
		{name: "conns per principal default", body: "limits:\n  conns_per_principal: 0\n", want: func(l Limits) bool { return l.ConnsPerPrincipal == 64 }},
		{name: "handshakes set", body: "limits:\n  handshakes_per_principal: 4\n",
			want: func(l Limits) bool {
				return l.Handshakes == 4 && l.ConnsPerPrincipal == DefaultLimits.ConnsPerPrincipal
			}},
		{name: "negative handshakes", body: "limits:\n  handshakes_per_principal: -1\n", err: "limit handshakes_per_principal must be zero or a positive number"},
		{name: "database connections", body: "limits:\n  db_conns_per_server: 12\n  db_conns_per_project: 3\n",
			want: func(l Limits) bool { return l.DBConns == 12 && l.DBConnsPerProject == 3 }},
		{name: "database connections default", body: "limits:\n  db_conns_per_server: 0\n",
			want: func(l Limits) bool { return l.DBConns == 32 && l.DBConnsPerProject == 8 }},
		{name: "negative db_conns_per_server", body: "limits:\n  db_conns_per_server: -1\n", err: "limit db_conns_per_server must be zero or a positive number"},
		{name: "negative db_conns_per_project", body: "limits:\n  db_conns_per_project: -1\n", err: "limit db_conns_per_project must be zero or a positive number"},
		{name: "a project's connections past the server's", body: "limits:\n  db_conns_per_server: 4\n  db_conns_per_project: 5\n",
			err: "limit db_conns_per_project, 5, is more than db_conns_per_server, 4; fix:"},
		{name: "usage limits", body: "limits:\n  records_per_usage_call: 50\n  usage_per_day: 900\n",
			want: func(l Limits) bool {
				return l.UsageRecords == 50 && l.UsagePerDay == 900 && l.Labels == DefaultLimits.Labels
			}},
		{name: "negative store limit", body: "limits:\n  acceptance_items_per_issue: -1\n", err: "acceptance_items_per_issue"},
		{name: "negative usage limit", body: "limits:\n  usage_per_day: -1\n", err: "usage_per_day"},
		{name: "paths limit", body: "limits:\n  paths_per_issue: 50\n",
			want: func(l Limits) bool { return l.Paths == 50 && l.Labels == DefaultLimits.Labels }},
		{name: "negative paths limit", body: "limits:\n  paths_per_issue: -1\n", err: "paths_per_issue"},
		{name: "memory limits", body: "limits:\n  memory_body_bytes: 2048\n  tags_per_memory: 5\n  memory_tag_bytes: 32\n  memories_per_scope: 50\n  memory_key_bytes: 64\n",
			want: func(l Limits) bool {
				return l.MemoryBody == 2048 && l.MemoryTags == 5 && l.MemoryTagLength == 32 && l.Memories == 50 &&
					l.MemoryKeyLength == 64 && l.Labels == DefaultLimits.Labels
			}},
		{name: "memory limits default", body: "limits:\n  memory_body_bytes: 0\n",
			want: func(l Limits) bool {
				return l.MemoryBody == 4096 && l.MemoryTags == 20 && l.MemoryTagLength == 64 && l.Memories == 1000 && l.MemoryKeyLength == 128
			}},
		{name: "negative memory limit", body: "limits:\n  memories_per_scope: -1\n", err: "memories_per_scope"},
		{name: "prices limit", body: "limits:\n  prices_per_project: 20\n",
			want: func(l Limits) bool { return l.Prices == 20 && l.Labels == DefaultLimits.Labels }},
		{name: "prices limit default", body: "limits:\n  prices_per_project: 0\n", want: func(l Limits) bool { return l.Prices == 1000 }},
		{name: "negative prices limit", body: "limits:\n  prices_per_project: -1\n", err: "prices_per_project"},
		{name: "prices past the most", body: "limits:\n  prices_per_project: 10001\n", err: "prices_per_project is 10001; it can be at most 10000"},
		{name: "hours limits", body: "limits:\n  hours_per_day: 5\n  hours_note_bytes: 80\n",
			want: func(l Limits) bool {
				return l.HoursPerDay == 5 && l.HoursNote == 80 && l.Prices == DefaultLimits.Prices
			}},
		{name: "hours limits default", body: "limits:\n  hours_per_day: 0\n",
			want: func(l Limits) bool { return l.HoursPerDay == 50 && l.HoursNote == 500 }},
		{name: "negative hours_per_day", body: "limits:\n  hours_per_day: -1\n", err: "hours_per_day"},
		{name: "hours_note_bytes past its column", body: "limits:\n  hours_note_bytes: 65536\n", err: "hours_note_bytes is 65536; it can be at most 65535"},
		{name: "plan limits", body: "limits:\n  plans_per_project: 20\n  principals_per_plan: 7\n",
			want: func(l Limits) bool { return l.Plans == 20 && l.PlanPrincipals == 7 }},
		{name: "plan limits default", body: "limits:\n  plans_per_project: 0\n",
			want: func(l Limits) bool { return l.Plans == 1000 && l.PlanPrincipals == 100 }},
		{name: "plans past the most", body: "limits:\n  plans_per_project: 10001\n", err: "plans_per_project is 10001; it can be at most 10000"},
		{name: "principals_per_plan past the most", body: "limits:\n  principals_per_plan: 1001\n", err: "principals_per_plan is 1001; it can be at most 1000"},
		{name: "conflict limits", body: "limits:\n  conflicts_per_issue: 3\n  conflicts_per_principal: 9\n",
			want: func(l Limits) bool { return l.ConflictsPerIssue == 3 && l.ConflictsPerPrincipal == 9 }},
		{name: "conflict limits default", body: "limits:\n  conflicts_per_issue: 0\n",
			want: func(l Limits) bool { return l.ConflictsPerIssue == 20 && l.ConflictsPerPrincipal == 100 }},
		{name: "negative conflicts_per_issue", body: "limits:\n  conflicts_per_issue: -1\n", err: "conflicts_per_issue"},
		{name: "negative conflicts_per_principal", body: "limits:\n  conflicts_per_principal: -1\n", err: "conflicts_per_principal"},
		{name: "gate limits", body: "limits:\n  approvers_per_gate: 3\n  open_gates_per_project: 40\n",
			want: func(l Limits) bool { return l.GateApprovers == 3 && l.OpenGates == 40 }},
		{name: "gate limits default", body: "limits:\n  approvers_per_gate: 0\n",
			want: func(l Limits) bool { return l.GateApprovers == 10 && l.OpenGates == 1000 }},
		{name: "negative approvers_per_gate", body: "limits:\n  approvers_per_gate: -1\n", err: "approvers_per_gate"},
		{name: "approvers_per_gate past the most", body: "limits:\n  approvers_per_gate: 101\n", err: "approvers_per_gate is 101; it can be at most 100"},
		{name: "negative open_gates_per_project", body: "limits:\n  open_gates_per_project: -1\n", err: "open_gates_per_project"},
		{name: "lock limits", body: "limits:\n  locks_per_project: 30\n  holders_per_lock: 5\n  waiters_per_lock: 7\n  lock_waits_per_principal: 2\n",
			want: func(l Limits) bool {
				return l.Locks == 30 && l.LockCapacity == 5 && l.LockWaiters == 7 && l.LockWaits == 2
			}},
		{name: "lock limits default", body: "limits:\n  locks_per_project: 0\n",
			want: func(l Limits) bool {
				return l.Locks == 200 && l.LockCapacity == 64 && l.LockWaiters == 100 && l.LockWaits == 20
			}},
		{name: "negative waiters_per_lock", body: "limits:\n  waiters_per_lock: -1\n", err: "waiters_per_lock"},
		{name: "holders_per_lock past the most", body: "limits:\n  holders_per_lock: 1001\n", err: "holders_per_lock is 1001; it can be at most 1000"},
		{name: "memory body past the column", body: "limits:\n  memory_body_bytes: 70000\n", err: "memory_body_bytes"},
		{name: "negative server limit", body: "limits:\n  write_burst: -1\n", err: "limit write_burst must be zero or a positive number"},
		{name: "write rate not a number", body: "limits:\n  writes_per_second: .nan\n", err: "limit writes_per_second must be zero or a positive number"},
		{name: "infinite write rate", body: "limits:\n  writes_per_second: .inf\n", err: "limit writes_per_second must be zero or a positive number"},
		{name: "idem window default", body: "limits:\n  conns_per_server: 5\n",
			want: func(l Limits) bool { return l.IdemWindow == Duration(7*24*time.Hour) }},
		{name: "idem window set", body: "limits:\n  idem_window: 30d\n",
			want: func(l Limits) bool {
				return l.IdemWindow == Duration(30*24*time.Hour) && l.Conns == DefaultLimits.Conns
			}},
		{name: "idem window of an hour", body: "limits:\n  idem_window: 1h\n",
			want: func(l Limits) bool { return l.IdemWindow == Duration(time.Hour) }},
		{name: "idem window below an hour", body: "limits:\n  idem_window: 59m\n", err: "limit idem_window is 59m; it must be from 1h to 365d"},
		{name: "idem window past a year", body: "limits:\n  idem_window: 366d\n", err: "limit idem_window is 366d; it must be from 1h to 365d"},
		{name: "negative idem window", body: "limits:\n  idem_window: -1h\n", err: "not a duration"},
		{name: "away max default", body: "limits:\n  conns_per_server: 5\n",
			want: func(l Limits) bool { return l.AwayMax == Duration(7*24*time.Hour) }},
		{name: "away max set", body: "limits:\n  away_max: 30d\n",
			want: func(l Limits) bool { return l.AwayMax == Duration(30*24*time.Hour) && l.Conns == DefaultLimits.Conns }},
		{name: "away max of an hour", body: "limits:\n  away_max: 1h\n",
			want: func(l Limits) bool { return l.AwayMax == Duration(time.Hour) }},
		{name: "away max below an hour", body: "limits:\n  away_max: 59m\n", err: "limit away_max is 59m; it must be from 1h to 30d"},
		{name: "away max past 30 days", body: "limits:\n  away_max: 31d\n", err: "limit away_max is 31d; it must be from 1h to 30d"},
		{name: "HTTPS limits", body: "limits:\n  https_conns_per_server: 100\n  https_conns_per_addr: 4\n  https_header_bytes: 8192\n  https_header_timeout: 5s\n" +
			"  https_body_bytes: 1024\n  https_body_timeout: 2s\n  upgrade_timeout: 3s\n  ws_message_bytes: 65536\n  auth_failures_per_minute: 6\n  auth_failure_burst: 12\n",
			want: func(l Limits) bool {
				return l.HTTPSConns == 100 && l.HTTPSConnsPerAddr == 4 && l.HTTPSHeaderBytes == 8192 && l.HTTPSHeaderTimeout == Duration(5*time.Second) &&
					l.HTTPSBodyBytes == 1024 && l.HTTPSBodyTimeout == Duration(2*time.Second) && l.UpgradeTimeout == Duration(3*time.Second) && l.WSMessageBytes == 65536 &&
					l.AuthFailureRate == 6 && l.AuthFailureBurst == 12 && l.Conns == DefaultLimits.Conns
			}},
		{name: "HTTPS limits default", body: "limits:\n  https_conns_per_server: 0\n",
			want: func(l Limits) bool {
				return l.HTTPSConns == 4096 && l.HTTPSConnsPerAddr == 64 && l.HTTPSHeaderBytes == 16<<10 && l.HTTPSHeaderTimeout == Duration(10*time.Second) &&
					l.HTTPSBodyBytes == 64<<10 && l.HTTPSBodyTimeout == Duration(10*time.Second) && l.UpgradeTimeout == Duration(10*time.Second) && l.WSMessageBytes == 4<<20 &&
					l.AuthFailureRate == 30 && l.AuthFailureBurst == 60
			}},
		{name: "sign-in limits", body: "limits:\n  signins_pending_per_server: 50\n  signins_pending_per_addr: 4\n  dpop_proofs_per_server: 500\n",
			want: func(l Limits) bool {
				return l.SignInsPending == 50 && l.SignInsPendingPerAddr == 4 && l.DPoPProofs == 500 && l.Conns == DefaultLimits.Conns
			}},
		{name: "sign-in limits default", body: "limits:\n  signins_pending_per_server: 0\n",
			want: func(l Limits) bool {
				return l.SignInsPending == 10000 && l.SignInsPendingPerAddr == 16 && l.DPoPProofs == 100000
			}},
		{name: "negative signins_pending_per_server", body: "limits:\n  signins_pending_per_server: -1\n",
			err: "limit signins_pending_per_server must be zero or a positive number"},
		{name: "negative signins_pending_per_addr", body: "limits:\n  signins_pending_per_addr: -1\n",
			err: "limit signins_pending_per_addr must be zero or a positive number"},
		{name: "negative dpop_proofs_per_server", body: "limits:\n  dpop_proofs_per_server: -5\n",
			err: "limit dpop_proofs_per_server must be zero or a positive number"},
		{name: "negative https_conns_per_addr", body: "limits:\n  https_conns_per_addr: -1\n", err: "limit https_conns_per_addr must be zero or a positive number"},
		{name: "ws_message_bytes past a frame", body: "limits:\n  ws_message_bytes: 4194305\n", err: "limit ws_message_bytes must be zero or a positive number"},
		{name: "auth failures not a number", body: "limits:\n  auth_failures_per_minute: .nan\n", err: "limit auth_failures_per_minute"},
		{name: "bad duration", body: "limits:\n  idle_timeout: soon\n", err: "soon"},
		{name: "unknown limit", body: "limits:\n  lables: 3\n", err: "lables"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ResolveSettings(Settings{}, write(tc.name+".yaml", tc.body), func(string) string { return "" })
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("ResolveSettings(%q) = %v, want an error naming %q", tc.body, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want(s.Limits) {
				t.Errorf("ResolveSettings(%q).Limits = %+v", tc.body, s.Limits)
			}
		})
	}
}

func TestBuckets(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	b := buckets{rate: 2, burst: 3}
	steps := []struct {
		at   time.Duration
		key  string
		ok   bool
		wait time.Duration
	}{
		{0, "alice", true, 0}, {0, "alice", true, 0}, {0, "alice", true, 0},
		{0, "alice", false, time.Second},
		{0, "bob", true, 0},
		{500 * time.Millisecond, "alice", true, 0},
		{500 * time.Millisecond, "alice", false, time.Second},
		{time.Hour, "alice", true, 0}, {time.Hour, "alice", true, 0}, {time.Hour, "alice", true, 0},
		{time.Hour, "alice", false, time.Second},
	}
	for i, st := range steps {
		ok, wait := b.take(st.key, t0.Add(st.at))
		if ok != st.ok || wait != st.wait {
			t.Errorf("step %d: take(%s, +%s) = %v, %s; want %v, %s", i, st.key, st.at, ok, wait, st.ok, st.wait)
		}
	}
}

// has says whether a bucket holds a token, and how long until it will,
// and takes none.
func TestBucketsHas(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	b := buckets{rate: 0.5, burst: 1}
	if ok, wait := b.has("10.0.0.1", t0); !ok || wait != 0 {
		t.Fatalf("has(an address never seen) = %v, %s; want true", ok, wait)
	}
	if ok, _ := b.take("10.0.0.1", t0); !ok {
		t.Fatal("take(the first) = false")
	}
	for range 3 {
		if ok, wait := b.has("10.0.0.1", t0); ok || wait != 2*time.Second {
			t.Fatalf("has(an empty bucket) = %v, %s; want false, 2s", ok, wait)
		}
	}
	if ok, _ := b.has("10.0.0.1", t0.Add(2*time.Second)); !ok {
		t.Error("has(a bucket refilled) = false")
	}
}

// A bucket that has filled again is let go when buckets are pruned: it is
// the same as one not there, so a key that has ever written does not hold
// memory for good (rule 18).
func TestBucketsPrune(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	b := buckets{rate: 2, burst: 3}
	for _, key := range []string{"alice", "alice", "alice", "bob"} {
		b.take(key, t0)
	}
	steps := []struct {
		at   time.Duration
		kept []string
	}{
		{0, []string{"alice", "bob"}},
		{500 * time.Millisecond, []string{"alice"}}, // bob's one token is back
		{time.Second, []string{"alice"}},            // alice has 2 of 3
		{1500 * time.Millisecond, nil},
		{-time.Hour, nil}, // a clock set back finds nothing to keep, and adds no bucket
	}
	for _, st := range steps {
		b.prune(t0.Add(st.at))
		got := slices.Sorted(maps.Keys(b.m))
		if !slices.Equal(got, st.kept) {
			t.Errorf("prune(+%s) kept %v, want %v", st.at, got, st.kept)
		}
	}
	// A key let go starts again with a full bucket, as it would have had.
	for i := range 4 {
		ok, _ := b.take("alice", t0.Add(2*time.Second))
		if want := i < 3; ok != want {
			t.Errorf("take %d of alice's after the prune = %v, want %v", i, ok, want)
		}
	}
}

func TestSampler(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := sampler{n: 2}
	ctx := t.Context()
	for i := range 5 {
		s.line(ctx, log, projA+"/mallory", t0.Add(time.Duration(i)*time.Second), slog.LevelInfo, logs.RequestRefused, "request", "i", i)
	}
	s.line(ctx, log, "alice", t0, slog.LevelInfo, logs.RequestRefused, "request", "i", 9)
	s.line(ctx, log, projA+"/mallory", t0.Add(time.Minute), slog.LevelInfo, logs.RequestRefused, "request", "i", 5)
	got := buf.String()
	for _, w := range []string{"msg=request event=request.refused i=0", "i=1", "i=9", "i=5",
		`msg="refusal lines suppressed" event=log.suppressed principal=mallory lines=3 per_minute=2`} {
		if !strings.Contains(got, w) {
			t.Errorf("log lacks %q:\n%s", w, got)
		}
	}
	for _, w := range []string{"i=2", "i=3", "i=4"} {
		if strings.Contains(got, w) {
			t.Errorf("log has suppressed line %q:\n%s", w, got)
		}
	}
}

// The reproducers of the review (S-4, S-5, S-6, S-7), as regressions.

func TestSecWriterHold(t *testing.T) {
	// Serial: bounds elapsed time, which a test running beside it would stretch.
	s := newServer(t)
	labels := make([]string, 20000)
	for i := range labels {
		labels[i] = fmt.Sprintf("l%05d", i)
	}
	start := time.Now()
	_, perr := call[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "x", Labels: labels})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "at most 50 labels") {
		t.Errorf("create with 20000 labels = %v, want invalid naming the cap of 50", perr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the refusal took %s; it should not touch the writer", d)
	}
	var b strings.Builder
	for i := range 201 {
		fmt.Fprintf(&b, "- i%d\n", i)
	}
	if _, perr := call[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "y", Acceptance: b.String()}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("create with 201 acceptance items = %v, want invalid", perr)
	}
	c := mustCall[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "z", Acceptance: "- one"})
	ticks := make([]int, 6000)
	for i := range ticks {
		ticks[i] = i + 1
	}
	if _, perr := call[proto.AcceptResult](t, s, bob, proto.OpAccept, proto.AcceptArgs{ID: c.ID, Tick: ticks}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("tick 6000 items = %v, want invalid", perr)
	}
}

// encodes checks that res fits one frame.
func encodes(t *testing.T, s *Server, a store.Actor, op string, args any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, perr := s.Dispatch(t.Context(), a, op, raw)
	if perr != nil {
		t.Fatalf("%s: %v", op, perr)
	}
	f, err := proto.Response(1, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.NewEncoder(&bytes.Buffer{}).Encode(f); err != nil {
		t.Fatalf("%s reply does not fit a frame: %v", op, err)
	}
	return f.OK
}

func TestSecSizes(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{Limits: store.Limits{Notices: 1000}})
	c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "x"})
	big := strings.Repeat("A", 65535)
	u := mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: c.ID, Rev: c.Rev, Body: &big, Design: &big, Notes: &big})
	huge := strings.Repeat("C", 200000)
	if _, perr := call[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: c.ID, Rev: u.Rev, Body: &huge}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("200 KB body = %v, want invalid", perr)
	}
	body := strings.Repeat("D", 65535)
	for i := range 70 {
		if _, perr := call[proto.CommentResult](t, s, bob, proto.OpComment, proto.CommentArgs{ID: c.ID, Body: body}); perr != nil {
			t.Fatalf("comment %d: %v", i, perr)
		}
	}
	n, before := 0, ""
	for range 10 {
		var r proto.CommentsResult
		if err := json.Unmarshal(encodes(t, s, alice, proto.OpComments, proto.PageArgs{ID: c.ID, Before: before}), &r); err != nil {
			t.Fatal(err)
		}
		n += len(r.Comments)
		if before = r.Earlier; before == "" {
			break
		}
	}
	if n != 70 {
		t.Errorf("paging comments read %d, want 70", n)
	}
	var h proto.HistoryResult
	if err := json.Unmarshal(encodes(t, s, alice, proto.OpHistory, proto.PageArgs{ID: c.ID}), &h); err != nil {
		t.Fatal(err)
	}
	if h.Total != 72 || len(h.Events) != 72 {
		t.Errorf("history = %d of %d events, want all 72 in one page now that events are compact", len(h.Events), h.Total)
	}
}

func TestSecRegistry(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	for i := range 300 {
		a := store.Actor{Principal: "mallory", Session: fmt.Sprintf("%04d", i) + strings.Repeat("s", 251), Machine: strings.Repeat("m", 255)}
		if err := s.cfg.Store.TouchAgent(t.Context(), a, "claude-code"); err != nil {
			t.Fatal(err)
		}
	}
	var w proto.WhoResult
	if err := json.Unmarshal(encodes(t, s, alice, proto.OpWho, proto.WhoArgs{Since: "7d"}), &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Agents) != 100 || w.More != 156 {
		t.Errorf("who = %d agents and %d more, want 100 and 156 (256 sessions kept)", len(w.Agents), w.More)
	}
}

func TestSecMentions(t *testing.T) {
	t.Parallel()
	// The allowance is for a minute of the store's time, so the store's
	// clock stands still: on the real one, a machine that takes more than
	// a minute over these requests delivers a second allowance.
	s, _ := newServerClock(t, Limits{}, fixedNow)
	for _, p := range []string{"v0", "v1", "v2", "v3", "v4", "v5", "v6", "v7", "v8", "v9"} {
		if err := s.cfg.Store.TouchAgent(t.Context(), store.Actor{Principal: p, Session: "x", Machine: "m"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	c := mustCall[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "x"})
	for range 50 {
		mustCall[proto.CommentResult](t, s, bob, proto.OpComment, proto.CommentArgs{ID: c.ID, Body: "@v0 @v1 @v2 @v3 @v4 @v5 @v6 @v7 @v8 @v9"})
	}
	for range 50 {
		mustCall[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "spam", Assignee: "v0"})
	}
	pg, err := s.cfg.Store.Inbox(t.Context(), store.Actor{Principal: "v0", Session: "x", Machine: "m"}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := store.DefaultLimits.Notices; pg.Unread != want {
		t.Errorf("v0 unread after 100 requests from bob = %d, want %d", pg.Unread, want)
	}
}

// S-5: show and blocked cap the edges they list and count the rest.
func TestShowAndBlockedCapEdges(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{Limits: store.Limits{Deps: 1000}})
	hub := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "hub"})
	for range proto.MaxShowDeps + 3 {
		b := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "blocker"})
		mustCall[proto.Empty](t, s, alice, proto.OpDepAdd, proto.DepArgs{From: hub.ID, To: b.ID})
	}
	sh := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: hub.ID})
	if len(sh.Deps) != proto.MaxShowDeps || sh.DepsMore != 3 {
		t.Errorf("show = %d deps and %d more, want %d and 3", len(sh.Deps), sh.DepsMore, proto.MaxShowDeps)
	}
	bl := mustCall[proto.BlockedResult](t, s, alice, proto.OpBlocked, proto.LimitArgs{})
	if len(bl.Issues) != 1 || len(bl.Issues[0].BlockedBy) != proto.MaxBlockers || bl.Issues[0].More != proto.MaxShowDeps+3-proto.MaxBlockers {
		t.Errorf("blocked = %+v, want hub with %d blockers listed", bl.Issues, proto.MaxBlockers)
	}
}

// handshakeAs connects a over a pipe and returns the welcome, which may be
// a refusal.
func handshakeAs(t *testing.T, s *Server, a store.Actor) (*proto.Frame, *conn) {
	t.Helper()
	return handshakeProto(t, s, a, proto.Proto)
}

// handshakeProto is handshakeAs with a client speaking protocol p.
func handshakeProto(t *testing.T, s *Server, a store.Actor, p int) (*proto.Frame, *conn) {
	t.Helper()
	srv, cli := net.Pipe()
	c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	t.Cleanup(func() { _ = cli.Close(); c.wg.Wait() })
	c.wg.Go(func() { s.handle(t.Context(), srv) })
	c.wg.Go(func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: a.Principal})
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: p, Project: project, Session: a.Session, Machine: a.Machine})
	})
	return c.read(), c
}

// S-11: connections are capped per principal and in all, refused with
// busy in the welcome, and a slot frees when its connection ends.
func TestConnectionCaps(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{ConnsPerPrincipal: 2, Conns: 3})
	w1, c1 := handshakeAs(t, s, alice)
	w2, _ := handshakeAs(t, s, alice)
	w3, _ := handshakeAs(t, s, alice)
	if w1.Err != nil || w2.Err != nil {
		t.Fatalf("first two of alice's connections refused: %v, %v", w1.Err, w2.Err)
	}
	// Clients tell this refusal from every other by proto.AtConnLimit.
	if w3.Err == nil || w3.Err.Message != "alice is at its connection limit (2)" || w3.Err.Fix != proto.FixConnLimit || !proto.AtConnLimit(w3.Err) {
		t.Errorf("alice's third connection: %+v, want busy naming her limit, with FixConnLimit, that AtConnLimit reads", w3.Err)
	}
	if w, _ := handshakeAs(t, s, bob); w.Err != nil {
		t.Errorf("bob's connection refused: %v", w.Err)
	}
	if w, _ := handshakeAs(t, s, dana); w.Err == nil || w.Err.Code != proto.CodeBusy ||
		!strings.Contains(w.Err.Message, "server is at its connection limit (3)") || proto.AtConnLimit(w.Err) {
		t.Errorf("a fourth connection in all: %+v, want busy naming the server's limit, which AtConnLimit does not read", w.Err)
	}
	// Closing one of alice's frees its slot.
	_ = c1.nc.Close()
	deadline := time.Now().Add(patience)
	for {
		w, c := handshakeAs(t, s, alice)
		if w.Err == nil {
			break
		}
		_ = c.nc.Close()
		if time.Now().After(deadline) {
			t.Fatalf("alice's slot was never freed: %v", w.Err)
		}
		runtime.Gosched()
	}
}

// proto.AtConnLimit checks the principal in the message by a copy of
// PrincipalPattern, which proto cannot import: the two agree at the
// pattern's edges, and the subject of the server's own limit is no
// principal.
func TestAtConnLimitKnowsEveryPrincipal(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"a", "a" + strings.Repeat("z", 63), "a.b_c-9", "z0", "a" + strings.Repeat("z", 64), "A", "9a", "-a", "a b", "the server", "alicé"} {
		if got, want := proto.AtConnLimit(proto.ConnLimit(name, 1)), PrincipalPattern.MatchString(name); got != want {
			t.Errorf("AtConnLimit(ConnLimit(%q, 1)) = %v, want %v, as PrincipalPattern says", name, got, want)
		}
	}
}

// A client that leaves before reading its welcome gives back the slot the
// handshake took for it; otherwise enough of them would lock its
// principal out until a restart.
func TestWelcomeWriteFailureFreesSlot(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	srv, cli := net.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { s.handle(t.Context(), srv); _ = srv.Close() })
	_ = cli.SetDeadline(time.Now().Add(patience))
	// net.Pipe has no buffer: once the hello is written the server has
	// read it, and all it does with the connection next is write the
	// welcome, which the close makes fail.
	enc := proto.NewEncoder(cli)
	if err := enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: alice.Principal}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: alice.Session,
		Machine: alice.Machine}); err != nil {
		t.Fatal(err)
	}
	_ = cli.Close()
	wg.Wait()
	s.slots.mu.Lock()
	n, total := s.slots.by[alice.Principal], s.slots.total
	s.slots.mu.Unlock()
	if n != 0 || total != 0 {
		t.Errorf("after a client closed before its welcome, alice holds %d connection slots, %d in all; want 0", n, total)
	}
}

// S-11: a connection that sends nothing for IdleTimeout is closed with a
// note, unless it watches its inbox.
func TestIdleTimeout(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	var past atomic.Bool
	s.wall = func() time.Time {
		if past.Load() {
			return time.Now().Add(-time.Hour) // every deadline is already gone
		}
		return time.Now()
	}
	past.Store(true)
	w, c := handshakeAs(t, s, alice)
	if w.Err != nil {
		t.Fatal(w.Err)
	}
	f := c.read()
	if f.Err == nil || f.Err.Code != proto.CodeUnavailable || !strings.Contains(f.Err.Message, "idle") {
		t.Fatalf("idle connection got %+v, want an unavailable note about idling", f)
	}

	past.Store(false)
	w, c = handshakeAs(t, s, bob)
	if w.Err != nil {
		t.Fatal(w.Err)
	}
	c.until(c.send(proto.OpWatch, proto.WatchArgs{}))
	past.Store(true)
	c.until(c.send(proto.OpReady, proto.LimitArgs{}))
	c.until(c.send(proto.OpReady, proto.LimitArgs{})) // read with the old clock's deadline, had it one
}

// A server whose clock is behind the real one, as a test's fixed clock
// falls behind once real time passes it, does not idle out its
// connections: the idle deadline is the socket's, on the real clock.
func TestIdleDeadlineIsOnTheRealClock(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.now = func() time.Time { return time.Now().Add(-time.Hour) }
	w, c := handshakeAs(t, s, alice)
	if w.Err != nil {
		t.Fatal(w.Err)
	}
	for range 3 {
		c.until(c.send(proto.OpReady, proto.LimitArgs{}))
	}
}

// The write rate limit: a principal's writes past its bucket are refused
// with busy and a wait; its reads, and others' writes, go on.
func TestWriteRateLimit(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{WriteBurst: 3, WriteRate: 0.001})
	_, a := handshakeAs(t, s, alice)
	for i := range 3 {
		a.until(a.send(proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("w%d", i)}))
	}
	id := a.send(proto.OpCreate, proto.CreateArgs{Title: "one too many"})
	f := a.read()
	if f.ID != id || f.Err == nil || f.Err.Code != proto.CodeBusy || !strings.HasPrefix(f.Err.Fix, "wait ") {
		t.Fatalf("fourth write = %+v, want busy with a wait", f)
	}
	a.until(a.send(proto.OpList, proto.ListArgs{}))
	_, b := handshakeAs(t, s, bob)
	b.until(b.send(proto.OpCreate, proto.CreateArgs{Title: "bob's"}))
}

// A reply that would pass the frame limit is refused with invalid; the
// connection stays up.
func TestSendTooLarge(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close(); _ = cli.Close() }()
	sess := &session{enc: proto.NewEncoder(srv)}
	go func() { _ = s.send(sess, 7, map[string]string{"x": strings.Repeat("y", proto.MaxFrame)}, nil) }()
	_ = cli.SetReadDeadline(time.Now().Add(patience))
	f, err := proto.NewDecoder(cli).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != 7 || f.Err == nil || f.Err.Code != proto.CodeInvalid || !strings.Contains(f.Err.Message, "larger than 4 MiB") {
		t.Errorf("oversized reply = %+v, want invalid for request 7", f)
	}
}

// Prune runs the store's pruning with the configured windows.
func TestServerPrune(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	s.Prune(t.Context()) // nothing to prune, and no error logged
}

// Prune lets go of the write buckets that have filled again, and keeps
// the others.
func TestServerPruneLetsFullBucketsGo(t *testing.T) {
	t.Parallel()
	s := newServerWith(t, Limits{WriteBurst: 3, WriteRate: 0.001})
	var ahead atomic.Int64
	s.now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }
	_, a := handshakeAs(t, s, alice)
	a.until(a.send(proto.OpCreate, proto.CreateArgs{Title: "one"}))
	keys := func() int {
		s.writes.mu.Lock()
		defer s.writes.mu.Unlock()
		return len(s.writes.m)
	}
	s.Prune(t.Context())
	if n := keys(); n != 1 {
		t.Fatalf("after a write and a prune, %d write buckets; want alice's, which has not filled again", n)
	}
	ahead.Store(int64(2000 * time.Second)) // a token comes back in 1,000 s
	s.Prune(t.Context())
	if n := keys(); n != 0 {
		t.Errorf("after alice's bucket filled again and a prune, %d write buckets; want none", n)
	}
}
