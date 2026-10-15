package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/proto"
)

// The soak's partition mode (design §18.10, F5's gate): "the soak with
// the client cut from the server for minutes at a time, and its event
// log still equal to what the sessions were told". With
// STARFIX_SOAK_PARTITION set, every agent's `sfx mcp` has an outbox and a
// cache, as `sfx mcp` does by default, and now and then an agent is cut
// off from the server for 2 to 30 minutes of the store's clock while it
// goes on working: its writes are queued, its reads of its own work
// answered from the cache, its starts refused, and its leases lapse on
// the server, where others may take its issues. When it is let back,
// its next connection replays what waits, and a finish or handoff that
// arrives after its lease lapsed is applied or kept as a comment
// (§18.6).
//
// A session is cut by its dials: while it is cut, its `sfx mcp` dials
// fail as an unreachable server's do (code unavailable), and the
// connection it had fails its next request, sending nothing, as one whose
// network dropped, which `sfx mcp` closes itself. (Closing it from the
// cutting goroutine raced the SSH channel's writes, under -race.) A dial
// that was under way when the cut came is closed as it completes.
// Whatever the timing, then, a request made while the session is cut
// reaches nothing, and one that reaches the server went on a connection
// made while it was not cut; what
// it is told (queued, or answered) is what the check holds the log to,
// and the check goes by keys and markers in the log's own order, never
// by a clock.
//
// At rest the check (partitionFaults) holds every write a session was
// told it queued to its outbox, what the server answered its replays,
// and the event log: applied once, parked, kept as a comment, or refused
// with a reason the person can read in `sfx outbox`; none lost, none
// silently dropped, none applied twice. The replay of each project's log
// already holds that no claim went to two sessions, that a late write
// applied only under a claim nobody took since (claim.lapsed), and that
// one was kept only where the issue had moved on (issue.kept).

// partition is what the partition mode keeps of the run.
type partition struct {
	mu sync.Mutex
	// queued are the writes sessions were told were queued, in order.
	queued []queuedWrite
	// cuts counts the times a session was cut off, cutFor how long in
	// all on the store's clock, and cached the answers a cut session read
	// from the cache.
	cuts    int
	cutFor  time.Duration
	cached  int
	refused int // starts and other writes refused while cut
}

// queuedWrite is a write a session was told it queued: entry is its
// number in the principal's outbox, idem its key, read from the outbox
// as it was queued, and markers the text unique to it that the log shows
// wherever it took effect.
type queuedWrite struct {
	who     sessKey
	p       *sproj
	entry   uint64
	op      string
	idem    string
	markers []string
}

// refusal is an entry an outbox keeps as refused, with the server's
// code and message.
type refusal struct{ code, message string }

// answer is the server's answer to one replay of an entry: kept is
// whether an applied one said it was kept as a comment.
type answer struct {
	out  outcome
	kept bool
}

// stampRec is an event stamped with an idempotency key: the last of the
// write the key stands for.
type stampRec struct {
	seq int64
	op  string
}

// partitionEnd is what a run left, as the check reads it: the outboxes
// at rest (pending and refused, by key), the server's answers to the
// replays, by key, the keys of which some attempt, live or replayed,
// went unanswered as its connection was lost, the events of the log
// stamped with each key, and how often each marker is in the log.
type partitionEnd struct {
	pending map[string]bool
	refused map[string]refusal
	answers map[string][]answer
	lost    map[string]bool
	stamps  map[string][]stampRec
	markers map[string]int
}

// noopOps are the ops that write nothing, and so stamp no event, when
// they change nothing: a label already there, an edge or a tick already
// made, records all stored before.
var noopOps = map[string]bool{proto.OpLabelAdd: true, proto.OpLabelRm: true, proto.OpDepAdd: true, proto.OpAccept: true,
	proto.OpUsage: true, proto.OpUpdate: true}

// partitionFaults holds each write a session was told it queued to what
// became of it. An entry still pending at rest was never delivered. One
// refused must say why, and must not be in the log. One gone from its
// outbox must have been answered, be stamped on exactly one event, unless
// its op can change nothing, have each marker in the log exactly once,
// and be kept as a comment exactly when the server said it was. A replay
// whose key no session queued is a write from nowhere.
func partitionFaults(queued []queuedWrite, end partitionEnd) []violation {
	var out []violation
	fail := func(w queuedWrite, format string, args ...any) {
		out = append(out, violation{msg: fmt.Sprintf("%s's %s (entry %d, key %s): ", w.who, w.op, w.entry, w.idem) + fmt.Sprintf(format, args...)})
	}
	known := map[string]bool{}
	for _, w := range queued {
		if w.idem == "" {
			fail(w, "told it was queued, but there was no entry %d in its outbox", w.entry)
			continue
		}
		known[w.idem] = true
		stamps := end.stamps[w.idem]
		if end.pending[w.idem] {
			fail(w, "still waits in its outbox at rest: never delivered")
			continue
		}
		if r, ok := end.refused[w.idem]; ok {
			if r.code == "" || r.message == "" {
				fail(w, "refused with no reason in its outbox (code %q, message %q)", r.code, r.message)
			}
			if len(stamps) == 0 {
				for _, m := range w.markers {
					if n := end.markers[m]; n != 0 {
						fail(w, "refused, but %q is in the event log %d times", m, n)
					}
				}
				continue
			}
			// An attempt whose answer was lost may have been applied,
			// and the outbox then refused unsent, as when the
			// principal's access ended (design §18.3): the person retries
			// it, and is given the first answer. The server refusing a
			// key it applied is another matter.
			switch {
			case slices.ContainsFunc(end.answers[w.idem], func(a answer) bool { return a.out == refused }):
				fail(w, "refused (%s) when replayed, but the log applied it (event %d, %s)", r.code, stamps[0].seq, stamps[0].op)
			case !end.lost[w.idem]:
				fail(w, "refused (%s), but the log applied it, and no attempt of it went unanswered (event %d, %s)", r.code, stamps[0].seq, stamps[0].op)
			}
			continue
		}
		var last *answer
		refusedOnce := false
		for i, a := range end.answers[w.idem] {
			switch a.out {
			case acked:
				last = &end.answers[w.idem][i]
			case refused:
				refusedOnce = true
			}
		}
		if last == nil {
			switch {
			case refusedOnce:
				fail(w, "refused when replayed, but its outbox does not keep the refusal")
			case len(stamps) > 0:
				fail(w, "gone from its outbox with no answer, though the log applied it (event %d)", stamps[0].seq)
			default:
				fail(w, "silently dropped: gone from its outbox, never answered, and not in the event log")
			}
			continue
		}
		switch {
		case len(stamps) > 1:
			fail(w, "applied %d times: events %v carry its key", len(stamps), stamps)
		case len(stamps) == 0 && !noopOps[w.op]:
			fail(w, "lost: %s answered, and no event carries its key", w.op)
			continue
		}
		for _, m := range w.markers {
			if n := end.markers[m]; n != 1 {
				fail(w, "%q is in the event log %d times, want once", m, n)
			}
		}
		if len(stamps) == 0 || w.op != proto.OpFinish && w.op != proto.OpHandoff {
			continue
		}
		switch keptLog := stamps[0].op == "issue.kept"; {
		case last.kept && !keptLog:
			fail(w, "told it was kept as a comment, but the log applied it (event %d, %s)", stamps[0].seq, stamps[0].op)
		case !last.kept && keptLog:
			fail(w, "told it applied, but the log kept it as a comment (event %d)", stamps[0].seq)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(end.answers)) {
		if !known[k] {
			out = append(out, violation{msg: fmt.Sprintf("replayed a write with key %s, which no session was told it queued", k)})
		}
	}
	return out
}

// partitionFrom reads the partition mode from STARFIX_SOAK_PARTITION:
// empty is off; 1 or true, on.
func partitionFrom(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "":
		return false, nil
	case "1", "true", "on":
		return true, nil
	}
	return false, fmt.Errorf("STARFIX_SOAK_PARTITION=%q: want 1 for the partition mode, or nothing", v)
}

// The bounds of one cut, and of the time an agent is connected between
// two, on the store's clock. A cut longer than the agent's 15-minute
// lease lets the lease lapse, and others take its issues.
const (
	cutMin = 2 * time.Minute
	cutMax = 30 * time.Minute
)

// cutSpan is a span from cutMin to cutMax, as rng chooses: on the
// store's clock, and in real time.
func (ms *mcpSession) cutSpan() (onStore, inReal time.Duration) {
	d := cutMin + time.Duration(ms.rng.Int64N(int64(cutMax-cutMin)))
	return d, ms.s.clock.real(d)
}

// errCut is what a cut session's dials fail with: what an unreachable
// server's do (client.Unreachable).
var errCut = proto.Errf(proto.CodeUnavailable, "retry once the network is back", "soak: the network between this session and the server is cut")

// errCutConn is how a connection fails once its session is cut off: as
// one whose network dropped, which sfx mcp closes and dials again.
var errCutConn = errors.New("soak: the connection's network is cut")

// partitionStep cuts the agent off once it has been connected for a
// while, and lets it back once its cut is over: each span from cutMin
// to cutMax, as the session's own choices, which a seed replays.
func (ms *mcpSession) partitionStep() {
	if ms.s.part == nil {
		return
	}
	ms.mu.Lock()
	defer ms.mu.Unlock()
	now := time.Now()
	switch {
	case ms.cutUntil.IsZero():
		_, gap := ms.cutSpan()
		ms.cutUntil = now.Add(gap) // the first cut comes then
	case ms.cut.Load() && now.After(ms.cutUntil):
		_, gap := ms.cutSpan()
		ms.cut.Store(false)
		ms.cutUntil = now.Add(gap)
	case !ms.cut.Load() && now.After(ms.cutUntil):
		d, span := ms.cutSpan()
		ms.cut.Store(true)
		ms.cutUntil = now.Add(span)
		ms.s.part.mu.Lock()
		ms.s.part.cuts++
		ms.s.part.cutFor += d
		ms.s.part.mu.Unlock()
	}
}

// rejoin ends the agent's cut, as the end of the run does.
func (ms *mcpSession) rejoin() { ms.cut.Store(false) }

// isCut reports whether the agent is cut off now.
func (ms *mcpSession) isCut() bool { return ms.cut.Load() }

// write calls a tool that writes, and notes a write it was told it
// queued, with its entry's key read from the outbox, and the markers the
// log shows wherever it takes effect.
func (ms *mcpSession) write(name string, args map[string]any, markers ...string) (string, bool) {
	res := ms.toolResult(name, args)
	text, isErr := firstText(res)
	if ms.s.part == nil || res == nil {
		return text, isErr
	}
	cut := ms.isCut()
	switch {
	case !isErr && strings.HasPrefix(text, "queued: "):
		var q struct {
			Op    string `json:"op"`
			Entry uint64 `json:"entry"`
		}
		b, _ := json.Marshal(res.StructuredContent)
		if json.Unmarshal(b, &q) != nil {
			ms.s.mon.fail("", "%s: %s answered queued with no entry: %s", ms.key, name, b)
			return text, isErr
		}
		w := queuedWrite{who: ms.key, p: ms.p, entry: q.Entry, op: q.Op, idem: ms.entryKey(q.Entry), markers: markers}
		ms.s.part.mu.Lock()
		ms.s.part.queued = append(ms.s.part.queued, w)
		ms.s.part.mu.Unlock()
	case cut && !isErr:
		ms.s.mon.fail("", "%s: %s was answered while cut off from the server: %q", ms.key, name, text)
	case cut:
		ms.s.part.mu.Lock()
		ms.s.part.refused++
		ms.s.part.mu.Unlock()
	}
	return text, isErr
}

// entryKey returns the idempotency key of entry seq of this session's
// principal's outbox, or "" when it holds no such entry.
func (ms *mcpSession) entryKey(seq uint64) string {
	l, _, err := ms.off.Local()
	if err != nil {
		return ""
	}
	ob, ok, err := l.Existing(ms.key.principal)
	if err != nil || !ok {
		return ""
	}
	list, err := ob.List()
	if err != nil {
		return ""
	}
	for _, e := range list.Pending {
		if e.Seq == seq {
			return e.Idem
		}
	}
	for _, r := range list.Refused {
		if r.Entry.Seq == seq {
			return r.Entry.Idem
		}
	}
	return ""
}

// firstText is a tool result's first text block, and whether it is an
// error; a nil result is an error with no text.
func firstText(res *mcp.CallToolResult) (string, bool) {
	if res == nil {
		return "", true
	}
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok {
			return tc.Text, res.IsError
		}
	}
	return "", res.IsError
}

// fromCache reports whether a tool result was answered from the cache,
// which says so in a line of its own, or prime's notice.
func fromCache(res *mcp.CallToolResult) bool {
	if res == nil || res.IsError {
		return false
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok && strings.Contains(tc.Text, "this machine's copy") {
			return true
		}
	}
	return false
}

// typedArgs decodes the arguments of a replayed entry, which an outbox
// sends as the JSON it stored, into the op's *Args, as the checks read a
// live request's; nil for an op the agents do not queue.
func typedArgs(op string, raw json.RawMessage) any {
	switch op {
	case proto.OpCreate:
		return decodeAs[proto.CreateArgs](raw)
	case proto.OpComment:
		return decodeAs[proto.CommentArgs](raw)
	case proto.OpFinish:
		return decodeAs[proto.FinishArgs](raw)
	case proto.OpHandoff:
		return decodeAs[proto.HandoffArgs](raw)
	case proto.OpUpdate:
		return decodeAs[proto.UpdateArgs](raw)
	case proto.OpLabelAdd, proto.OpLabelRm:
		return decodeAs[proto.LabelArgs](raw)
	case proto.OpDepAdd:
		return decodeAs[proto.DepArgs](raw)
	case proto.OpAccept:
		return decodeAs[proto.AcceptArgs](raw)
	}
	return nil
}

// decodeAs decodes raw as a T, or returns nil.
func decodeAs[T any](raw json.RawMessage) any {
	var v T
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// idemOf is a replayed request's idempotency key.
func idemOf(raw json.RawMessage) string {
	var a struct {
		Idem string `json:"idem"`
	}
	_ = json.Unmarshal(raw, &a)
	return a.Idem
}

// keyOf is the idempotency key of a request's *Args, or "" when it has
// none.
func keyOf(args any) string {
	v := reflect.ValueOf(args)
	if v.Kind() != reflect.Struct {
		return ""
	}
	if f := v.FieldByName("Idem"); f.IsValid() && f.Kind() == reflect.String {
		return f.String()
	}
	return ""
}

// argsOf is the request a call made, decoded when it was a replay.
func argsOf(c call) any {
	if c.replayed != nil {
		return c.replayed
	}
	return c.args
}

// drain lets every cut agent back and sends what waits in every outbox
// of the run: the agents' own connections send theirs, and an outbox
// whose agent vanished is sent by a new session of its principal, as on
// a machine where the next `sfx mcp` sends what the last one left. It
// waits until nothing is pending, or a minute has passed.
func (s *soak) drain(ctx context.Context) {
	if s.part == nil {
		return
	}
	var mcps []*mcpSession
	for _, a := range s.all() {
		if ms, ok := a.(*mcpSession); ok {
			ms.rejoin()
			mcps = append(mcps, ms)
		}
	}
	type userKey struct {
		principal string
		p         *sproj
	}
	seen := map[userKey]bool{}
	var wg sync.WaitGroup
	for _, ms := range mcps {
		k := userKey{ms.key.principal, ms.p}
		if seen[k] {
			continue
		}
		seen[k] = true
		sweep := &mcpSession{session: &session{s: s, p: ms.p, tag: ms.key.principal + "-sweep", u: ms.u,
			rng:   rand.New(rand.NewPCG(s.cfg.seed, uint64(len(seen)))), //nolint:gosec // a reproducible workload, not security
			lease: 15 * time.Minute, renewEvery: time.Hour}, held:       map[string]int{}}
		sweep.key = sessKey{ms.key.principal, "mcp-" + sweep.tag}
		if err := sweep.open(); err != nil {
			s.mon.fail("", "%s: start sfx mcp to send the outbox: %v", sweep.key, err)
			continue
		}
		s.mu.Lock()
		s.every = append(s.every, sweep)
		s.mu.Unlock()
		wg.Go(func() {
			defer sweep.stop()
			sweep.sweep(ctx)
		})
	}
	wg.Wait()
}

// sweep sends what waits in the session's principal's outbox, until
// nothing does. An agent cut off late in a long run may have queued
// hundreds of writes, which take minutes to send; a minute in which
// fewer did not wait fails the run.
func (ms *mcpSession) sweep(ctx context.Context) {
	last, since := -1, time.Now()
	for ctx.Err() == nil {
		// A tool call, whose first connection sends what waits, and a
		// renewal, as `sfx mcp`'s tick makes, which sends more on the
		// open connection.
		text, isErr := ms.tool("inbox", nil)
		if isErr {
			ms.s.t.Logf("soak: %s: inbox while sending the outbox: %.300s", ms.key, text)
		}
		ms.srv.Renew(ctx)
		n, err := ms.pending()
		if err != nil {
			ms.s.mon.fail("", "%s: read the outbox: %v", ms.key, err)
			return
		}
		if n == 0 {
			return
		}
		if last < 0 || n < last {
			last, since = n, time.Now()
		}
		if time.Since(since) > time.Minute {
			ms.s.mon.fail("", "%s: %d writes have waited in the outbox for a minute with none sent%s", ms.key, n, ms.stuck())
			return
		}
		pause(ctx, 200*time.Millisecond)
	}
}

// stuck says, for a failure, which entry waits first in the outbox and
// what the server answered each time it was sent.
func (ms *mcpSession) stuck() string {
	l, _, err := ms.off.Local()
	if err != nil {
		return ""
	}
	ob, ok, err := l.Existing(ms.key.principal)
	if err != nil || !ok {
		return ""
	}
	list, err := ob.List()
	if err != nil || len(list.Pending) == 0 {
		return ""
	}
	e := list.Pending[0]
	out := fmt.Sprintf(": the first is entry %d, %s %.200s;", e.Seq, e.Op, e.Args)
	for _, c := range ms.s.mon.calls() {
		if raw, ok := c.args.(json.RawMessage); ok && idemOf(raw) == e.Idem {
			out += fmt.Sprintf(" sent by %s: %s", c.who, c.out)
			if c.err != nil {
				out += fmt.Sprintf(" (%s: %.200s)", c.err.Code, c.err.Message)
			}
			out += ";"
		}
	}
	return out
}

// pending counts the entries that wait in this session's principal's
// outbox.
func (ms *mcpSession) pending() (int, error) {
	l, _, err := ms.off.Local()
	if err != nil {
		return 0, err
	}
	ob, ok, err := l.Existing(ms.key.principal)
	if err != nil || !ok {
		return 0, err
	}
	list, err := ob.List()
	return len(list.Pending), err
}

// checkPartition runs partitionFaults over p: the writes queued for it,
// its principals' outboxes at rest, the answers to its replays, and its
// event log as the replay read it.
func (s *soak) checkPartition(p *sproj, calls []call) {
	if s.part == nil {
		return
	}
	s.part.mu.Lock()
	var queued []queuedWrite
	for _, w := range s.part.queued {
		if w.p == p {
			queued = append(queued, w)
		}
	}
	s.part.mu.Unlock()
	end := partitionEnd{pending: map[string]bool{}, refused: map[string]refusal{}, answers: map[string][]answer{},
		lost: map[string]bool{}, stamps: p.rep.stamped, markers: p.rep.markers}
	for _, c := range calls {
		if c.out == lost {
			if k := keyOf(argsOf(c)); k != "" {
				end.lost[k] = true
			}
		}
		raw, ok := c.args.(json.RawMessage)
		if !ok {
			continue
		}
		a := answer{out: c.out}
		switch r := c.res.(type) {
		case proto.HandoffResult:
			a.kept = r.Comment != ""
		case proto.UpdateResult:
			a.kept = r.Comment != ""
		}
		k := idemOf(raw)
		end.answers[k] = append(end.answers[k], a)
	}
	for _, u := range s.outboxUsers(p) {
		l, _, err := u.off.Local()
		if err != nil {
			s.t.Errorf("outbox of %s in %s: %v", u.key.principal, p.name, err)
			continue
		}
		ob, ok, err := l.Existing(u.key.principal)
		if err != nil {
			s.t.Errorf("outbox of %s in %s: %v", u.key.principal, p.name, err)
			continue
		}
		if !ok {
			continue
		}
		list, err := ob.List()
		if err != nil {
			s.t.Errorf("outbox of %s in %s: %v", u.key.principal, p.name, err)
			continue
		}
		for _, e := range list.Pending {
			end.pending[e.Idem] = true
		}
		for _, r := range list.Refused {
			end.refused[r.Entry.Idem] = refusal{code: r.Code, message: r.Message}
		}
	}
	for _, v := range partitionFaults(queued, end) {
		s.mon.fail(v.issue, "%s", v.msg)
	}
}

// reportPartition says what the partition mode did: the cuts, what the
// cut sessions queued and read from the cache, and what became of the
// replays.
func (s *soak) reportPartition(b *bytes.Buffer, calls []call) {
	if s.part == nil {
		return
	}
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	byOp := map[string]int{}
	for _, w := range s.part.queued {
		byOp[w.op]++
	}
	var replays, kept, parked, refusedN, lostN int
	for _, c := range calls {
		if c.replayed == nil {
			continue
		}
		replays++
		switch c.out {
		case refused:
			refusedN++
		case lost:
			lostN++
		}
		switch r := c.res.(type) {
		case proto.HandoffResult:
			if r.Comment != "" {
				kept++
			}
		case proto.UpdateResult:
			if len(r.Parked) > 0 {
				parked++
			}
		}
	}
	var lapsed, keptEvents int
	for _, p := range s.projs {
		p.rep.mu.Lock()
		lapsed += p.rep.ops["claim.lapsed"]
		keptEvents += p.rep.ops["issue.kept"]
		p.rep.mu.Unlock()
	}
	fmt.Fprintf(b, "  partition: %d cuts, %s of the store's clock in all; %d writes queued %v, %d writes refused while cut, %d answers from the cache; %d replays (%d refused, %d lost and sent again, %d kept as a comment, %d parked); claim.lapsed %d, issue.kept %d in the logs\n",
		s.part.cuts, s.part.cutFor, len(s.part.queued), byOp, s.part.refused, s.part.cached, replays, refusedN, lostN, kept, parked, lapsed, keptEvents)
}

// outboxUsers returns one agent of each principal with an agent in p:
// an outbox is a principal's, shared by its sessions on a machine.
func (s *soak) outboxUsers(p *sproj) []*mcpSession {
	var out []*mcpSession
	seen := map[string]bool{}
	for _, a := range s.all() {
		if ms, ok := a.(*mcpSession); ok && ms.p == p && !seen[ms.key.principal] {
			seen[ms.key.principal] = true
			out = append(out, ms)
		}
	}
	return out
}
