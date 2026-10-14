package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/logs"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// jsonLine is one line of the daemon's log in JSON.
type jsonLine map[string]any

// str is the line's field key as text, or "" when it is absent or not
// text.
func (l jsonLine) str(key string) string {
	s, _ := l[key].(string)
	return s
}

// jsonLogged has m's server log in JSON, at debug, through the logger
// starfixd makes, and returns where the lines go. It is called before
// the server opens a store, since a store's logger is made when it
// opens.
func jsonLogged(t *testing.T, m *multi) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	log, err := NewLogger(buf, "debug", "json")
	if err != nil {
		t.Fatal(err)
	}
	m.cfg.Logger = log
	return buf
}

// logLines decodes the JSON lines in text.
func jsonLines(t *testing.T, text string) []jsonLine {
	t.Helper()
	var out []jsonLine
	for raw := range strings.SplitSeq(text, "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var l jsonLine
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			t.Fatalf("log line %q is not JSON: %v", raw, err)
		}
		out = append(out, l)
	}
	return out
}

// auditLines returns the lines of the audit stream among ls.
func auditLines(ls []jsonLine) []jsonLine {
	var out []jsonLine
	for _, l := range ls {
		if l[logs.KeyAudit] == true {
			out = append(out, l)
		}
	}
	return out
}

// controlEvents returns every event of m's control store with seq
// greater than after.
func controlEvents(t *testing.T, m *multi, after int64) []store.Event {
	t.Helper()
	var all []store.Event
	for {
		evs, err := m.ctl.Events(t.Context(), after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
		if len(evs) < 1000 {
			return all
		}
		after = evs[len(evs)-1].Seq
	}
}

// Each event of the control store's log that changes who has access to
// what is also written as one log line with audit=true, after the write
// commits: the actor, the op and the target (design §16.4, decision 30).
// The stream is read from the event log, so it is that log's copy and
// can say nothing the log does not.
func TestAuditStream(t *testing.T) {
	t.Parallel()
	m := newMulti(t, Limits{ControlWriteRate: 100000, ControlWriteBurst: 100000}, nil)
	out := jsonLogged(t, m)
	m.project(t, projA, "alice=admin")
	m.refresh(t.Context())
	// What was written before the daemon started is history it does not
	// repeat: the stream starts at the newest event there was then, as
	// New has it start.
	m.auditStart(t.Context())
	m.tick(t.Context(), false)
	if got := auditLines(jsonLines(t, out.String())); len(got) != 0 {
		t.Fatalf("%d audit lines for events from before the server started, want none: %v", len(got), got)
	}
	start, err := m.ctl.LastEventSeq(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	d := mustDialTo(t, m.Server, dana, projA)
	key := keyLine(t, "hal")
	fp := fingerprintOf(t, "hal")
	// Every op that changes access, through the server.
	mustAsk[proto.PrincipalAddResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal", Project: projA})
	mustAsk[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "hal", Key: key + " hal@example.com"})
	mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "hal", Project: projA, Role: "admin"})
	mustAsk[proto.KeyResult](d, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fp})
	mustAsk[proto.MemberRemoveResult](d, proto.OpMemberRemove, proto.MemberArgs{Name: "hal", Project: projA})
	mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "hal"})
	mustAsk[proto.PrincipalResult](d, proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "hal"})
	// A request that is refused writes no event, and so no line.
	if _, perr := ask[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "nobody"}); perr == nil {
		t.Fatal("disabling a principal that does not exist succeeded")
	}
	// A change made beside the daemon, as `starfixd control init` makes
	// one, is in the stream after the reaper's next tick.
	m.project(t, projB, "erin=admin")
	before := len(auditLines(jsonLines(t, out.String())))
	m.tick(t.Context(), false)
	if after := len(auditLines(jsonLines(t, out.String()))); after <= before {
		t.Errorf("a tick wrote %d audit lines for the events written beside the daemon, want some", after-before)
	}
	// An archive and an activation, by the server again. dana's own
	// connection is to project A, so B is the one archived.
	mustAsk[proto.ProjectResult](d, proto.OpProjectArchive, proto.ProjectArgs{Project: projB})
	mustAsk[proto.ProjectResult](d, proto.OpProjectActivate, proto.ProjectArgs{Project: projB})
	m.tick(t.Context(), false) // nothing new: no line twice

	lines := auditLines(jsonLines(t, out.String()))
	var want []store.Event
	for _, e := range controlEvents(t, m, start) {
		if _, ok := logs.Audit(string(e.Op)); ok {
			want = append(want, e)
		}
	}
	if len(want) < 12 {
		t.Fatalf("the control store has %d events that change access: the test covers too little", len(want))
	}
	if len(lines) != len(want) {
		t.Fatalf("%d audit lines for %d events of the control store's log, want one each:\n%s", len(lines), len(want), out.String())
	}
	seen := map[string]bool{}
	for i, e := range want {
		l := lines[i]
		ev, _ := logs.Audit(string(e.Op))
		seen[string(e.Op)] = true
		at, _ := time.Parse(time.RFC3339Nano, l.str(logs.KeyAt))
		if l.str(logs.KeyEvent) != string(ev) || l[logs.KeySeq] != float64(e.Seq) || l.str(logs.KeyOp) != string(e.Op) ||
			l.str(logs.KeyTarget) != e.Target || l.str(logs.KeyPrincipal) != e.Actor.Principal || l.str(logs.KeySession) != e.Actor.Session ||
			!at.Equal(e.At) || l.str("level") != "INFO" {
			t.Errorf("audit line %d = %v\nwant the event %+v", i, l, e)
		}
		// A line is no request's: it carries the event's seq, not an id
		// of whichever request happened to read the log.
		if l.str(logs.KeyReq) != "" {
			t.Errorf("audit line %d carries the request id %q", i, l.str(logs.KeyReq))
		}
	}
	for _, op := range []store.Op{store.OpProjectAdd, store.OpProjectActivate, store.OpProjectArchive, store.OpPrincipalAdd,
		store.OpPrincipalDisable, store.OpPrincipalEnable, store.OpKeyAdd, store.OpKeyRemove, store.OpMemberSet, store.OpMemberRemove} {
		if !seen[string(op)] {
			t.Errorf("no audit line for %s: the test did not cover it", op)
		}
	}

	// What each kind of event's line says of its target.
	find := func(ev logs.Event, target string) jsonLine {
		t.Helper()
		// The newest: adding hal to the project made him a member first.
		for _, l := range slices.Backward(lines) {
			if l.str(logs.KeyEvent) == string(ev) && l.str(logs.KeyTarget) == target {
				return l
			}
		}
		t.Fatalf("no %s line for %s", ev, target)
		return nil
	}
	for _, tc := range []struct {
		name   string
		ev     logs.Event
		target string
		want   map[string]string
	}{
		{"a principal added", logs.AuditPrincipalAdd, "hal", map[string]string{"subject": "hal", "project": "", "fingerprint": "", "role": ""}},
		{"a principal disabled", logs.AuditPrincipalDisable, "hal", map[string]string{"subject": "hal"}},
		{"a key added", logs.AuditKeyAdd, fp, map[string]string{"subject": "hal", "fingerprint": fp, "project": ""}},
		{"a key removed", logs.AuditKeyRemove, fp, map[string]string{"subject": "hal", "fingerprint": fp}},
		{"a membership set", logs.AuditMemberSet, projA + "/hal", map[string]string{"subject": "hal", "project": projA, "role": "admin"}},
		{"a membership removed", logs.AuditMemberRemove, projA + "/hal", map[string]string{"subject": "hal", "project": projA, "role": ""}},
		{"a project archived", logs.AuditProjectArchive, projB, map[string]string{"subject": "", "project": projB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := find(tc.ev, tc.target)
			for k, v := range tc.want {
				if l.str(k) != v {
					t.Errorf("%s = %q, want %q; line: %v", k, l.str(k), v, l)
				}
			}
		})
	}
	// Fingerprints, never keys: a key event's before and after hold the
	// key's text, and none of it reaches a line.
	b64 := strings.Fields(key)[1]
	if all := out.String(); strings.Contains(all, b64) || strings.Contains(all, "hal@example.com") {
		t.Errorf("the log holds a key's text or its comment:\n%s", all)
	}
}

// audit_log: false writes no audit line, and changes nothing else.
func TestAuditStreamOff(t *testing.T) {
	t.Parallel()
	m := newMulti(t, Limits{}, nil)
	m.cfg.NoAudit = true
	out := jsonLogged(t, m)
	m.project(t, projA, "alice=admin")
	m.refresh(t.Context())
	d := mustDialTo(t, m.Server, dana, projA)
	mustAsk[proto.PrincipalAddResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "hal", Project: projA})
	m.tick(t.Context(), false)
	if got := auditLines(jsonLines(t, out.String())); len(got) != 0 {
		t.Errorf("%d audit lines with the audit log off: %v", len(got), got)
	}
	if evs := controlEvents(t, m, 0); len(evs) == 0 {
		t.Error("the control store's log is empty: it is the record whatever the setting")
	}
}
