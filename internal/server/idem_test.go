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

// wire opens a connection to s as alice, says hello, and returns what
// sends one request on it and reads its answer, as the wire carries it.
func wire(t *testing.T, s *Server) func(op string, args any) *proto.Frame {
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
		{T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: alice.Session, Machine: alice.Machine}} {
		if err := enc.Encode(f); err != nil {
			t.Fatal(err)
		}
	}
	if w := read(); w.T != proto.FrameWelcome || w.Err != nil {
		t.Fatalf("hello = %+v, want a welcome", w)
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

// TestKeyedOpsReplayOnce: the six ops the outbox queues besides those
// that make something, sent twice with their key, are applied once, and
// the second answer is the first.
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
}

// TestDepRmReplaysOnce: dep.rm honors its idempotency key as dep.add
// does (decision 88). A remove sent again with its key, after someone
// put the edge back, answers as the first did and leaves the edge where
// it is, rather than removing it a second time; the key sent for
// another edge is a clash.
func TestDepRmReplaysOnce(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	blocker := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "blocker"})
	other := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "another blocker"})
	edge := proto.DepArgs{From: is.ID, To: blocker.ID}
	add := func(key string) {
		t.Helper()
		in := edge
		in.Idem = key
		mustCall[proto.Empty](t, s, alice, proto.OpDepAdd, in)
	}
	add("k-add-1")
	rm := edge
	rm.Idem = "k-rm"
	first, perr := call[json.RawMessage](t, s, alice, proto.OpDepRm, rm)
	if perr != nil {
		t.Fatalf("dep.rm: %v", perr)
	}
	add("k-add-2")
	evs := eventCount(t, s)
	again, perr := call[json.RawMessage](t, s, store.Actor{Principal: "alice", Session: "s-other", Machine: "laptop-b"}, proto.OpDepRm, rm)
	if perr != nil {
		t.Fatalf("dep.rm again: %v", perr)
	}
	if string(again) != string(first) {
		t.Errorf("dep.rm again = %s, want the first answer %s", again, first)
	}
	if got := eventCount(t, s); got != evs {
		t.Errorf("dep.rm again wrote events: %d, want %d", got, evs)
	}
	show := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: is.ID})
	if len(show.Deps) != 1 || show.Deps[0].To != blocker.ID {
		t.Errorf("after dep.rm was sent again with its key, %s depends on %+v; want the edge to %s put back after it, kept", is.ID, show.Deps, blocker.ID)
	}
	clash := rm
	clash.To = other.ID
	if _, perr := call[json.RawMessage](t, s, alice, proto.OpDepRm, clash); perr == nil || perr.Code != proto.CodeConflict {
		t.Errorf("dep.rm with the key of another edge = %v, want conflict", perr)
	}
}

// eventCount counts the store's events.
func eventCount(t *testing.T, s *Server) int {
	t.Helper()
	evs, err := realStore(s.single.store).Events(t.Context(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

// TestStaleKey: a key used longer ago than the server's window (limits:
// idem_window) is refused as stale, with a fix that says the request was
// applied then and how to make the change again. Within the window the
// same key replays; with other arguments it is conflict.
func TestStaleKey(t *testing.T) {
	t.Parallel()
	clk := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clk }
	s, _ := newServerClock(t, Limits{IdemWindow: Duration(48 * time.Hour)}, now)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	comment := proto.CommentArgs{ID: is.ID, Body: "written once", Idem: "k-1"}
	mustCall[proto.CommentResult](t, s, alice, proto.OpComment, comment)
	send := wire(t, s)

	mu.Lock()
	clk = clk.Add(47 * time.Hour)
	mu.Unlock()
	if f := send(proto.OpComment, comment); f.Err != nil {
		t.Errorf("the key within the window = %v, want its answer again", f.Err)
	}
	other := comment
	other.Body = "written otherwise"
	if f := send(proto.OpComment, other); f.Err == nil || f.Err.Code != proto.CodeConflict {
		t.Errorf("the key for another request = %+v, want conflict", f.Err)
	}

	mu.Lock()
	clk = clk.Add(2 * time.Hour)
	mu.Unlock()
	f := send(proto.OpComment, comment)
	if f.Err == nil || f.Err.Code != proto.CodeStale ||
		!strings.Contains(f.Err.Message, `idempotency key "k-1" was used at 2026-10-07T12:00:00Z, longer ago than the 2d a key's result is kept: its request was applied then`) ||
		!strings.HasPrefix(f.Err.Fix, "nothing to do if that was this request") {
		t.Errorf("the key past the window = %+v; want stale, saying it was applied, with a fix", f.Err)
	}
}
