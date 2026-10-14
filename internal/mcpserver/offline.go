package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/offline"
	"github.com/ariesworx/starfix/internal/outbox"
	"github.com/ariesworx/starfix/internal/proto"
)

// sent remembers the writes a tool call sent, or would have sent, in
// order. Those that did not reach the server are what is queued (design
// §18.3): one, or each label of a label call.
type sent struct {
	writes []attempt
	// unqueued is why the call's write is not queued, when the tool knew
	// before it sent one (notQueued).
	unqueued string
}

// notQueued is the error of a tool whose write, which did not reach the
// server, cannot be queued, and why; c is the connection the tool was
// given, which notes it when it is a recorder, so that away says why
// though the tool ran without a connection.
func notQueued(c Conn, err error, why string) error {
	if r, ok := c.(recorder); ok {
		r.sent.unqueued = why
	}
	return &unqueuedError{err: err, why: why}
}

// attempt is one write of a tool call: its op, its arguments with their
// idempotency key, whose connection it went on, and what came of it.
type attempt struct {
	op   string
	args any
	who  string
	err  error
}

// lost returns the writes that did not reach the server.
func (s *sent) lost() []attempt {
	var out []attempt
	for _, w := range s.writes {
		if client.Unreachable(w.err) {
			out = append(out, w)
		}
	}
	return out
}

// recorder is a Conn that notes each write in sent as it passes it on.
// It keeps the one method beyond Conn that a tool asks a connection for.
type recorder struct {
	Conn
	sent *sent
}

func (r recorder) Call(ctx context.Context, op string, args, result any) error {
	err := r.Conn.Call(ctx, op, args, result)
	if _, unqueued := outbox.Unqueued(op); unqueued || outbox.Queueable(op) {
		r.sent.writes = append(r.sent.writes, attempt{op: op, args: args, who: r.Principal(), err: err})
	}
	return err
}

// Key is the key that signed the connection in, when it says.
func (r recorder) Key() client.KeyInfo {
	if k, ok := r.Conn.(interface{ Key() client.KeyInfo }); ok {
		return k.Key()
	}
	return client.KeyInfo{}
}

// noConn stands in for the connection a tool call never got: every call
// fails with the dial's error. Running a tool against it, behind a
// recorder, yields the write the tool would have sent.
type noConn struct{ err error }

func (c noConn) Call(context.Context, string, any, any) error { return c.err }
func (c noConn) Err() error                                   { return c.err }
func (noConn) Close() error                                   { return nil }
func (noConn) Principal() string                              { return "" }
func (noConn) Session() string                                { return "" }
func (noConn) Notices(string) []string                        { return nil }
func (noConn) Project() string                                { return "" }

// unqueuedError is a write that did not reach the server and that the
// outbox never queues, with why.
type unqueuedError struct {
	err error
	why string
}

func (e *unqueuedError) Error() string { return e.err.Error() }
func (e *unqueuedError) Unwrap() error { return e.err }

// queuedResult is what an agent is told of the writes of one call that
// were queued, the last of which is k: one terse line, which is not an
// error, and never an id and a rev as a write that was made returns
// (design §18.2, §18.8).
func queuedResult(k offline.Kept, n int) *mcp.CallToolResult {
	line := "queued: starfix cannot reach its server, so this " + k.Entry.Op +
		" waits on this machine and is sent when the server answers. Carry on, and do not repeat it."
	if n > 1 {
		line = fmt.Sprintf("queued: starfix cannot reach its server, so these %d writes (%s) wait on this machine and are sent when the server answers. Carry on, and do not repeat them.",
			n, k.Entry.Op)
	}
	if k.Entry.Op == proto.OpCreate {
		line += " The issue has no id until then, so nothing can refer to it."
	}
	doc, _ := json.Marshal(struct {
		Queued bool   `json:"queued"`
		Op     string `json:"op"`
		Entry  uint64 `json:"entry"`
	}{true, k.Entry.Op, k.Entry.Seq})
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: line}}, StructuredContent: json.RawMessage(doc)}
}

// away handles a tool call that failed with err. When the server was
// not reached and the call's writes that did not reach it are ones the
// outbox queues, it queues them, in order, and returns the result that
// says so. Otherwise it returns nil and the error to report: err itself,
// the outbox's refusal, or err marked as a write that is never queued.
// rerun runs the tool against a connection, for the call that failed
// before it had one.
func (s *Server) away(err error, w *sent, rerun func(Conn)) (*mcp.CallToolResult, error) {
	off := s.opts.Offline
	if off == nil {
		return nil, err
	}
	if !client.Unreachable(err) {
		// The agent is told of the refusal itself, and to ask a person;
		// what its principal queued waits in the refused list for one.
		off.Ended(err)
		return nil, err
	}
	off.Missed()
	if _, unq := errors.AsType[*unqueuedError](err); unq {
		return nil, err
	}
	if len(w.writes) == 0 {
		rerun(recorder{Conn: noConn{err: err}, sent: w})
	}
	if w.unqueued != "" {
		return nil, &unqueuedError{err: err, why: w.unqueued}
	}
	lost := w.lost()
	if len(lost) == 0 {
		return nil, err
	}
	for _, l := range lost {
		if why, ok := outbox.Unqueued(l.op); ok {
			return nil, &unqueuedError{err: err, why: why}
		}
		if !outbox.Queueable(l.op) {
			return nil, err
		}
	}
	var k offline.Kept
	for _, l := range lost {
		var kerr error
		if k, kerr = off.Keep(l.who, l.op, l.args, err); kerr != nil {
			return nil, kerr
		}
		s.waiting.Store(true)
	}
	return queuedResult(k, len(lost)), nil
}

// connected runs on each new connection, before the request that opened
// it, holding the link: it notes who the server took the key for, and
// sends what waits in that principal's outbox, within the outbox's
// replay budget. What is left is sent by drain.
func (s *Server) connected(ctx context.Context, c Conn) {
	off := s.opts.Offline
	if off == nil {
		return
	}
	if moved := off.Connected(c); moved != "" {
		s.moved.Store(&moved)
	}
	if s.replay(ctx, c) {
		select {
		case s.drain <- struct{}{}:
		default: // drainLoop already knows
		}
	}
}

// replay sends what waits on c, and reports whether another round
// should follow: this one ran out of time with entries left, and got
// somewhere. A round that sent nothing is not followed by another: the
// server is too slow to answer one write within the budget, and the
// next call or renewal finds out whether it still is. The caller holds
// the link.
func (s *Server) replay(ctx context.Context, c Conn) (more bool) {
	rep, _ := s.opts.Offline.Replay(ctx, c) // a failure of the local files leaves the entries where they are
	s.waiting.Store(rep.Pending > 0 || rep.Busy)
	return rep.Pending > 0 && rep.Delivered+rep.Refused+rep.Stale > 0 &&
		errors.Is(rep.Stopped, context.DeadlineExceeded) && ctx.Err() == nil
}

// drainLoop sends, in the background, what a connection's first replay
// left in the outbox, a budget's worth at a time, taking the link for
// each round so that tool calls go between. A round that runs out of
// time in the middle of a request closes its connection, as a canceled
// call does, so the next round dials another. It ends with ctx.
func (s *Server) drainLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.drain:
		}
		for more := true; more && ctx.Err() == nil; {
			more = s.link.drain(ctx, func(c Conn) bool { return s.replay(ctx, c) })
		}
	}
}

// notices are the lines a session's start adds to prime's for the
// outbox: a key the server gave to someone else, and what waits or was
// refused.
func (s *Server) notices(c Conn) []string {
	off := s.opts.Offline
	if off == nil {
		return nil
	}
	var out []string
	if m := s.moved.Load(); m != nil {
		out = append(out, *m)
	}
	if n := off.Notice(c); n != "" {
		out = append(out, n)
	}
	if n := off.Rekeyed(); n != "" {
		out = append(out, n)
	}
	return out
}
