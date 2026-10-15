package server

import (
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// speaking opens a connection to s as alice, says hello speaking
// protocol v, and returns what sends one request on it and reads its
// answer.
func speaking(t *testing.T, s *Server, v int) func(op string, args any) *proto.Frame {
	t.Helper()
	srv, cli := net.Pipe()
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = cli.Close(); wg.Wait() })
	wg.Go(func() {
		defer func() { _ = srv.Close() }()
		s.handle(t.Context(), srv)
	})
	enc, dec := proto.NewEncoder(cli), proto.NewDecoder(cli)
	read := func() *proto.Frame {
		t.Helper()
		_ = cli.SetReadDeadline(time.Now().Add(patience))
		for {
			f, err := dec.Decode()
			if err != nil {
				t.Fatalf("read from the server: %v", err)
			}
			if f.T != proto.FrameEvent {
				return f
			}
		}
	}
	for _, f := range []*proto.Frame{{T: proto.FrameBridge, Principal: alice.Principal},
		{T: proto.FrameHello, Proto: v, Project: project, Session: alice.Session, Machine: alice.Machine}} {
		if err := enc.Encode(f); err != nil {
			t.Fatal(err)
		}
	}
	if w := read(); w.T != proto.FrameWelcome || w.Err != nil {
		t.Fatalf("hello speaking protocol %d = %+v, want a welcome", v, w)
	}
	var id uint64
	return func(op string, args any) *proto.Frame {
		t.Helper()
		id++
		f, err := proto.Request(id, op, args)
		if err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(f); err != nil {
			t.Fatal(err)
		}
		return read()
	}
}

// TestKeyedOpsReplayOnce: the six ops protocol 6 gives a key, sent twice
// with it, are applied once, and the second answer is the first. A
// client of protocol 5, which sends no key, is served as before.
func TestKeyedOpsReplayOnce(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate,
		proto.CreateArgs{Title: "work", Labels: []string{"old"}, Acceptance: "- one\n- two"})
	blocker := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "blocker"})
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	in, out := int64(10), int64(1)
	title := "renamed"
	for _, tc := range []struct {
		op   string
		args any
	}{
		{proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Title: &title, Idem: "k-update"}},
		{proto.OpLabelAdd, proto.LabelArgs{ID: is.ID, Label: "new", Idem: "k-label-add"}},
		{proto.OpLabelRm, proto.LabelArgs{ID: is.ID, Label: "old", Idem: "k-label-rm"}},
		{proto.OpDepAdd, proto.DepArgs{From: is.ID, To: blocker.ID, Idem: "k-dep-add"}},
		{proto.OpAccept, proto.AcceptArgs{ID: is.ID, Tick: []int{1}, Idem: "k-accept"}},
		{proto.OpUsage, proto.UsageArgs{Records: []proto.UsageRecord{{Harness: "claude-code", RequestID: "r1", Model: "claude-opus-4-1",
			At: at, Granularity: "request", Tokens: proto.Tokens{Input: &in, Output: &out}}}, Idem: "k-usage"}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			first, perr := call[json.RawMessage](t, s, alice, tc.op, tc.args)
			if perr != nil {
				t.Fatalf("%s: %v", tc.op, perr)
			}
			evs := eventCount(t, s)
			// From another session of alice's, as an outbox delivers it.
			again, perr := call[json.RawMessage](t, s, store.Actor{Principal: "alice", Session: "s-other", Machine: "laptop-b"}, tc.op, tc.args)
			if perr != nil {
				t.Fatalf("%s again: %v", tc.op, perr)
			}
			if string(again) != string(first) {
				t.Errorf("%s again = %s, want the first answer %s", tc.op, again, first)
			}
			if got := eventCount(t, s); got != evs {
				t.Errorf("%s again wrote events: %d, want %d", tc.op, got, evs)
			}
		})
	}
	// A client of protocol 5 sends no key, and is served by a server that
	// still accepts 5, as one built before protocol 7 does.
	s.cfg.ProtoMin = 5
	send := speaking(t, s, 5)
	cur := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: is.ID})
	if f := send(proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: cur.Issue.Rev, Title: &title}); f.Err != nil {
		t.Errorf("update from a client of protocol 5 = %v, want it applied", f.Err)
	}
}

// eventCount counts the store's events.
func eventCount(t *testing.T, s *Server) int {
	t.Helper()
	evs, err := s.single.store.Events(t.Context(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

// TestStaleKey: a key used longer ago than the server's window (limits:
// idem_window) is refused as stale to a client of protocol 6, with a fix
// that says the request was applied then and how to make the change
// again; a client of protocol 5 is sent conflict, which it knows. Within
// the window the same key replays; with other arguments it is conflict.
func TestStaleKey(t *testing.T) {
	t.Parallel()
	clk := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clk }
	s, _ := newServerClock(t, Limits{IdemWindow: Duration(48 * time.Hour)}, now)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	comment := proto.CommentArgs{ID: is.ID, Body: "written once", Idem: "k-1"}
	mustCall[proto.CommentResult](t, s, alice, proto.OpComment, comment)
	s.cfg.ProtoMin = 5 // as a server built before protocol 7 accepts
	v6, v5 := speaking(t, s, 6), speaking(t, s, 5)

	mu.Lock()
	clk = clk.Add(47 * time.Hour)
	mu.Unlock()
	if f := v6(proto.OpComment, comment); f.Err != nil {
		t.Errorf("the key within the window = %v, want its answer again", f.Err)
	}
	other := comment
	other.Body = "written otherwise"
	if f := v6(proto.OpComment, other); f.Err == nil || f.Err.Code != proto.CodeConflict {
		t.Errorf("the key for another request = %+v, want conflict", f.Err)
	}

	mu.Lock()
	clk = clk.Add(2 * time.Hour)
	mu.Unlock()
	f := v6(proto.OpComment, comment)
	if f.Err == nil || f.Err.Code != proto.CodeStale ||
		!strings.Contains(f.Err.Message, `idempotency key "k-1" was used at 2026-10-07T12:00:00Z, longer ago than the 2d a key's result is kept: its request was applied then`) ||
		!strings.HasPrefix(f.Err.Fix, "nothing to do if that was this request") {
		t.Errorf("the key past the window, to a client of protocol 6 = %+v; want stale, saying it was applied, with a fix", f.Err)
	}
	if f := v5(proto.OpComment, comment); f.Err == nil || f.Err.Code != proto.CodeConflict {
		t.Errorf("the key past the window, to a client of protocol 5 = %+v; want conflict, which protocol 5 knows", f.Err)
	}
}
