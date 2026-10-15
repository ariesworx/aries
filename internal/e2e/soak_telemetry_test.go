package e2e

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// The soak with the daemon's telemetry on (design §16.6 and §16.7),
// which ARIES_SOAK_TELEMETRY asks for. It is off by default: the
// gate's short soak runs as it always has.
//
//	check    The instruments and spans are read in process when the run
//	         is at rest, and must say what the event logs say
//	         (checkTelemetry). A metric that is there and wrong fails.
//	refused  Metrics and traces are exported to a port nothing listens
//	         on. Every other check of the soak must hold all the same,
//	         and the daemon must count what it dropped and warn once
//	         for each signal (checkDeadCollector).
//	hang     The same, to a collector that accepts and never answers.
const (
	telOff     = ""
	telCheck   = "check"
	telRefused = "refused"
	telHang    = "hang"
)

// soakTel is the telemetry of one run.
type soakTel struct {
	mode string
	// tel is what the daemon records with; nil when mode is telOff.
	tel *telemetry.Telemetry
	// manual reads tel in process, in the mode telCheck.
	manual *telemetry.Manual
	// metricsURL is tel's own /metrics, in the modes with a dead
	// collector, where no reader is in process.
	metricsURL string
	// spans counts the spans that have ended, in the mode telCheck, and
	// people are the names no span may carry.
	spans  spanTally
	people func(string) bool
}

// opResult names the requests of one op that ended with one result.
type opResult struct{ op, result string }

// spanTally counts a run's spans by what the checks ask of them, so
// that a long run need not keep each one: a run of two minutes ends
// about 100,000.
type spanTally struct {
	mu sync.Mutex
	n  int
	// attempts and retried count the attempts of a write that ended ok,
	// and those lost to another transaction, by project.
	attempts, retried map[string]int
	// requests counts the server spans of requests, and handshakes
	// those of handshakes by result.
	requests   map[opResult]int
	handshakes map[string]int
	// named holds the first spans found saying who by anything but a
	// stand-in, or carrying a person's name.
	named []string
}

var (
	standInPrincipal = regexp.MustCompile(`^p_[0-9a-f]{16}$`)
	standInSession   = regexp.MustCompile(`^s_[0-9a-f]{16}$`)
)

// add counts spans. person reports whether a string is the name of a
// principal or a session of the run.
func (t *spanTally) add(spans []telemetry.SpanData, person func(string) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.attempts == nil {
		t.attempts, t.retried, t.requests, t.handshakes = map[string]int{}, map[string]int{}, map[opResult]int{}, map[string]int{}
	}
	named := func(format string, args ...any) {
		if len(t.named) < 10 {
			t.named = append(t.named, fmt.Sprintf(format, args...))
		}
	}
	t.n += len(spans)
	for _, sp := range spans {
		switch {
		case sp.Name == "store.write.attempt" && sp.Attrs["outcome"] == "ok":
			t.attempts[sp.Attrs["project"]]++
		case sp.Name == "store.write.attempt" && sp.Attrs["outcome"] == "retry":
			t.retried[sp.Attrs["project"]]++
		case sp.Kind != "server":
		case sp.Name == "handshake":
			t.handshakes[sp.Attrs["result"]]++
		default:
			t.requests[opResult{sp.Name, sp.Attrs["result"]}]++
		}
		if v, ok := sp.Attrs["principal"]; ok && !standInPrincipal.MatchString(v) {
			named("the span %s has principal=%q, want a stand-in, p_ and 16 hex digits", sp.Name, v)
		}
		if v, ok := sp.Attrs["session"]; ok && !standInSession.MatchString(v) {
			named("the span %s has session=%q, want a stand-in, s_ and 16 hex digits", sp.Name, v)
		}
		for k, v := range sp.Attrs {
			if person != nil && person(v) {
				named("the span %s carries %s=%q, the name of a principal or a session", sp.Name, k, v)
			}
		}
	}
}

// drain counts the spans that ended since it was last called. The run
// calls it as it goes, and once more at rest.
func (st *soakTel) drain() {
	if st.manual != nil {
		st.spans.add(st.manual.TakeSpans(), st.people)
	}
}

// newSoakTel makes the run's telemetry, and stops it when t ends. Call
// it before the world is made, so that it stops after the daemon has.
func newSoakTel(t *testing.T, mode string, logger *slog.Logger) *soakTel {
	t.Helper()
	st := &soakTel{mode: mode}
	cfg := telemetry.Config{Version: "v0.2.0", Ops: server.Ops(), PerProject: true, Logger: logger}
	switch mode {
	case telOff:
		return st
	case telCheck:
		tel, manual, err := telemetry.NewManual(cfg)
		if err != nil {
			t.Fatalf("soak: telemetry: %v", err)
		}
		t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
		st.tel, st.manual = tel, manual
		return st
	}

	// A collector that is not there: a port that refuses, or one that
	// accepts and holds every connection without a word.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	collector := dead.Addr().String()
	if mode == telRefused {
		_ = dead.Close()
	} else {
		wg.Go(func() {
			var held []net.Conn
			defer func() {
				for _, c := range held {
					_ = c.Close()
				}
			}()
			for {
				c, err := dead.Accept()
				if err != nil {
					return
				}
				held = append(held, c)
			}
		})
	}
	// The exports are frequent and give up soon, so that even the short
	// run meets many failures.
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + collector,
		"OTEL_EXPORTER_OTLP_TIMEOUT":  "1000",
		"OTEL_METRIC_EXPORT_INTERVAL": "1000",
		"OTEL_BSP_SCHEDULE_DELAY":     "500",
		"OTEL_BSP_EXPORT_TIMEOUT":     "2000",
	}
	cfg.Getenv, cfg.Pull, cfg.KeyFile = func(k string) string { return env[k] }, true, filepath.Join(t.TempDir(), "principals.key")
	tel, err := telemetry.New(ctx, cfg)
	if err != nil {
		cancel()
		t.Fatalf("soak: telemetry: %v", err)
	}
	// The daemon's own /metrics, on a port the test is given:
	// telemetry.Listen takes only a port that is named.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatalf("soak: telemetry listener: %v", err)
	}
	wg.Go(func() {
		if err := tel.Serve(ctx, l, telemetry.ServeOptions{Logger: logger}); err != nil {
			t.Errorf("soak: telemetry listener: %v", err)
		}
	})
	st.tel, st.metricsURL = tel, "http://"+l.Addr().String()+"/metrics"
	t.Cleanup(func() {
		// As `ariesd serve` stops it: the last exports get 5 seconds,
		// and a collector that is not there must not hold the stop for
		// longer (design §16.6).
		sctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		t0 := time.Now()
		_ = tel.Shutdown(sctx)
		if took := time.Since(t0); took > 7*time.Second {
			t.Errorf("soak: stopping telemetry with a dead collector (%s) took %s, want at most the 5s its last exports are given", mode, took.Round(time.Millisecond))
		}
		cancel()
		_ = dead.Close()
		wg.Wait()
	})
	return st
}

// telWant is what the event logs and the run's journal say the
// instruments and the spans must show.
type telWant struct {
	// claims counts the claim events of every project's log, by the
	// event and reason aries.claims has for them.
	claims map[[2]string]int
	// control counts the changes in the control log that a request
	// made, by the request's op. One change is one transaction, named
	// for its first event, since adding a principal also records its
	// keys and its membership; and adding a project activates it in a
	// second transaction, which is not counted as a request to
	// activate. The operator's changes, made before the daemon ran, and
	// the daemon's own are left out.
	control map[string]int
	// txs counts each project's transactions that recorded an event.
	txs map[string]int
	// acked and refused count the journal's requests that were
	// answered, by op, and by op and code. Sessions the journal does
	// not keep send requests too, so the server has counted at least
	// these.
	acked   map[string]int
	refused map[[2]string]int
	// closed is how many connections the run saw the server close as
	// access ended, and ended names the reasons, as
	// aries.connections.dropped has them, that the control log has a
	// change for.
	closed int
	ended  map[string]bool
}

// count is a histogram's number of observations as an int. A run makes
// thousands of them, nowhere near where the conversion could overflow.
func count(n uint64) int {
	return int(n) //nolint:gosec // see the comment above
}

// claimReasons maps the reason a claim.release event gives (decision 21)
// to the reason aries.claims counts it under.
var claimReasons = map[string]string{
	lostMembership: telemetry.ReasonMembership, lostDisabled: telemetry.ReasonDisabled, lostArchived: telemetry.ReasonArchived,
}

// compareTelemetry returns every way the instruments (snap) and the
// spans disagree with want, and with each other.
func compareTelemetry(snap telemetry.Snapshot, spans *spanTally, want telWant) []string {
	spans.mu.Lock()
	defer spans.mu.Unlock()
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	// Claims: counted from the store after each write commits, so they
	// are the log's, exactly. A renewal is no event, and is not held to
	// anything.
	const claims = "aries.claims"
	seen := map[[2]string]bool{}
	for _, p := range snap.Series(claims) {
		k := [2]string{p.Attrs[telemetry.KeyEvent], p.Attrs[telemetry.KeyReason]}
		seen[k] = true
		if k[0] == telemetry.ClaimRenew {
			continue
		}
		if int(p.Value) != want.claims[k] {
			bad("%s{event=%s,reason=%s} is %g, and the projects' event logs have %d", claims, k[0], k[1], p.Value, want.claims[k])
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(want.claims), func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) }) {
		if !seen[k] && want.claims[k] > 0 {
			bad("%s has no series for event=%s, reason=%s, and the projects' event logs have %d", claims, k[0], k[1], want.claims[k])
		}
	}

	// Writes: every transaction that recorded an event was a write that
	// committed, and every such write was an attempt that ended ok; a
	// write lost to another transaction is one retry and one span.
	const writes, retries = "aries.store.write.duration", "aries.store.write.retries"
	attempts, lost := spans.attempts, spans.retried
	for _, id := range slices.Sorted(maps.Keys(want.txs)) {
		wrote := count(snap.Observations(writes, telemetry.KeyProject, id))
		if wrote < want.txs[id] {
			bad("%s{project=%s} has %d observations, and the project's event log has %d transactions", writes, id, wrote, want.txs[id])
		}
		if attempts[id] < wrote {
			bad("project %s: %d spans of a write attempt that ended ok, and %s has %d observations", id, attempts[id], writes, wrote)
		}
		if got := int(snap.Sum(retries, telemetry.KeyProject, id)); got != lost[id] {
			bad("%s{project=%s} is %d, and %d spans are of an attempt that ended in a retry", retries, id, got, lost[id])
		}
	}

	// Requests: one observation and one span each, with one result; and
	// the server answered at least what the journal saw answered.
	const requests, byProject, handshakes = "aries.request.duration", "aries.project.requests", "aries.handshakes"
	measured, traced, shaken := map[opResult]int{}, spans.requests, spans.handshakes
	total := 0
	for _, p := range snap.Series(requests) {
		measured[opResult{p.Attrs[telemetry.KeyOp], p.Attrs[telemetry.KeyResult]}] += count(p.Count)
		total += count(p.Count)
	}
	keys := slices.Collect(maps.Keys(measured))
	for k := range traced {
		if _, ok := measured[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b opResult) int { return strings.Compare(a.op+" "+a.result, b.op+" "+b.result) })
	for _, k := range keys {
		if measured[k] != traced[k] {
			bad("%s{op=%s,result=%s} has %d observations, and %d spans are of such a request", requests, k.op, k.result, measured[k], traced[k])
		}
	}
	if got := int(snap.Sum(byProject)); got != total {
		bad("%s counts %d requests, and %s has %d observations", byProject, got, requests, total)
	}
	for _, op := range slices.Sorted(maps.Keys(want.acked)) {
		if got := measured[opResult{op, telemetry.ResultOK}]; got < want.acked[op] {
			bad("%s{op=%s,result=ok} has %d observations, and the sessions saw %d answered ok", requests, op, got, want.acked[op])
		}
	}
	for _, k := range slices.SortedFunc(maps.Keys(want.refused), func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) }) {
		if got := measured[opResult{k[0], k[1]}]; got < want.refused[k] {
			bad("%s{op=%s,result=%s} has %d observations, and the sessions saw %d refused so", requests, k[0], k[1], got, want.refused[k])
		}
	}
	results := map[string]bool{}
	for _, p := range snap.Series(handshakes) {
		results[p.Attrs[telemetry.KeyResult]] = true
	}
	for r := range shaken {
		results[r] = true
	}
	for _, r := range slices.Sorted(maps.Keys(results)) {
		if got := int(snap.Value(handshakes, telemetry.KeyResult, r)); got != shaken[r] {
			bad("%s{result=%s} is %d, and %d spans are of such a handshake", handshakes, r, got, shaken[r])
		}
	}

	// Control writes. Every change in the log was a request that
	// succeeded, so the counter is at least the log's. It may be ahead:
	// a change asked for again, after its answer was lost or because it
	// already held, succeeds and records nothing. What keeps it from
	// being ahead wrongly is the other count of the same requests, made
	// where each is answered: the two agree exactly.
	const control = "aries.control.writes"
	changed := map[string]int{}
	for _, p := range snap.Series(control) {
		if p.Attrs[telemetry.KeyResult] == telemetry.ResultOK {
			changed[p.Attrs[telemetry.KeyOp]] += int(p.Value)
		}
	}
	for op := range want.control {
		changed[op] += 0
	}
	for _, op := range slices.Sorted(maps.Keys(changed)) {
		if changed[op] < want.control[op] {
			bad("%s{op=%s,result=ok} is %d, and the control store's log has %d changes of it", control, op, changed[op], want.control[op])
		}
		if asked := measured[opResult{op, telemetry.ResultOK}]; changed[op] != asked {
			bad("%s{op=%s,result=ok} is %d, and %s has %d observations of such a request", control, op, changed[op], requests, asked)
		}
	}

	// Connections the server closed: at least those the run saw closed
	// as access ended, and none for a reason the control log has no
	// change for.
	const dropped = "aries.connections.dropped"
	ended := 0
	for _, p := range snap.Series(dropped) {
		reason := p.Attrs[telemetry.KeyReason]
		switch reason {
		case telemetry.DropMembership, telemetry.DropDisabled, telemetry.DropArchived, telemetry.DropKey, telemetry.DropAccess:
			ended += int(p.Value)
		}
		if _, access := want.ended[reason]; access && !want.ended[reason] && p.Value > 0 {
			bad("%s{reason=%s} is %g, and the control store's log has no change that ends access so", dropped, reason, p.Value)
		}
	}
	if ended < want.closed {
		bad("%s counts %d connections closed as access ended, and the run saw %d closed", dropped, ended, want.closed)
	}

	// People: a span says who by a stand-in, and by nothing else.
	return append(out, spans.named...)
}

// telemetryWant gathers what the replayed logs and the journal say.
func (s *soak) telemetryWant() telWant {
	w := telWant{claims: map[[2]string]int{}, control: map[string]int{}, txs: map[string]int{}, acked: map[string]int{},
		refused: map[[2]string]int{}, closed: int(s.acc.closed.Load()),
		ended: map[string]bool{telemetry.DropMembership: false, telemetry.DropDisabled: false, telemetry.DropArchived: false, telemetry.DropKey: false}}
	none := telemetry.ReasonNone
	for _, p := range s.projs {
		r := p.rep
		r.mu.Lock()
		w.txs[p.id] = r.txs
		w.claims[[2]string{telemetry.ClaimTake, none}] += r.ops["claim.take"]
		w.claims[[2]string{telemetry.ClaimExpire, none}] += r.ops["claim.expire"]
		w.claims[[2]string{telemetry.ClaimRelease, none}] += r.ops["claim.release"] - len(r.lost)
		for _, l := range r.lost {
			reason, ok := claimReasons[l.reason]
			if !ok {
				reason = telemetry.Other
			}
			w.claims[[2]string{telemetry.ClaimRelease, reason}]++
		}
		r.mu.Unlock()
	}
	s.ctl.mu.Lock()
	for i, e := range s.ctl.evs {
		if e.by == store.OperatorPrincipal || e.by == reaper.principal {
			continue
		}
		// The events of one transaction have its time and its actor.
		if i == 0 || !s.ctl.evs[i-1].at.Equal(e.at) || s.ctl.evs[i-1].by != e.by {
			w.control[e.op]++
		}
		switch store.Op(e.op) {
		case store.OpMemberRemove:
			w.ended[telemetry.DropMembership] = true
		case store.OpPrincipalDisable:
			w.ended[telemetry.DropDisabled] = true
		case store.OpProjectArchive:
			w.ended[telemetry.DropArchived] = true
		case store.OpKeyRemove:
			w.ended[telemetry.DropKey] = true
		}
	}
	s.ctl.mu.Unlock()
	w.control[string(store.OpProjectActivate)] -= w.control[string(store.OpProjectAdd)]
	for _, c := range s.mon.calls() {
		switch c.out {
		case acked:
			w.acked[c.op]++
		case refused:
			if c.err != nil {
				w.refused[[2]string{c.op, string(c.err.Code)}]++
			}
		}
	}
	return w
}

// checkTelemetry holds the daemon's instruments and spans to the event
// logs, once the run is at rest and every session has closed.
func (s *soak) checkTelemetry() {
	if s.tel.manual == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// The logs to their ends first: the instruments counted every event
	// as its write committed.
	if err := s.snapshot(); err != nil {
		s.t.Errorf("soak: telemetry: the last snapshot: %v", err)
		return
	}
	snap, err := s.tel.manual.Collect(ctx)
	if err != nil {
		s.t.Errorf("soak: telemetry: %v", err)
		return
	}
	s.tel.drain()
	want := s.telemetryWant()
	for _, v := range compareTelemetry(snap, &s.tel.spans, want) {
		s.mon.fail("", "telemetry: %s", v)
	}
	// The audit stream is read from the same control log, a tick of
	// the reaper after the last change at the latest.
	pause(ctx, time.Second)
	s.logs.mu.Lock()
	lines := slices.Clone(s.logs.audits)
	s.logs.mu.Unlock()
	s.ctl.mu.Lock()
	evs := slices.Clone(s.ctl.evs)
	s.ctl.mu.Unlock()
	for _, v := range compareAudit(lines, evs, int(s.stats.restarts.Load())) {
		s.mon.fail("", "telemetry: %s", v)
	}
	changes := 0
	for _, e := range evs {
		if _, audited := logs.Audit(e.op); audited && e.by != "" && e.by != store.OperatorPrincipal {
			changes++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "soak: telemetry, against the event logs: %d series of %d instruments, %d spans\n", len(snap), len(snap.Names()), s.tel.spans.n)
	const claims = "aries.claims"
	fmt.Fprintf(&b, "  claims: taken %g, expired %g, released %g (%g for a membership, %g for a disabled principal, %g for an archive), renewed %g\n",
		snap.Sum(claims, telemetry.KeyEvent, telemetry.ClaimTake), snap.Sum(claims, telemetry.KeyEvent, telemetry.ClaimExpire),
		snap.Sum(claims, telemetry.KeyEvent, telemetry.ClaimRelease),
		snap.Value(claims, telemetry.KeyEvent, telemetry.ClaimRelease, telemetry.KeyReason, telemetry.ReasonMembership),
		snap.Value(claims, telemetry.KeyEvent, telemetry.ClaimRelease, telemetry.KeyReason, telemetry.ReasonDisabled),
		snap.Value(claims, telemetry.KeyEvent, telemetry.ClaimRelease, telemetry.KeyReason, telemetry.ReasonArchived),
		snap.Sum(claims, telemetry.KeyEvent, telemetry.ClaimRenew))
	fmt.Fprintf(&b, "  audit stream: %d lines for the %d access changes in the control store's log that a request made\n", len(lines), changes)
	b.WriteString("  control writes that succeeded, and the changes in the log:")
	for _, op := range slices.Sorted(maps.Keys(want.control)) {
		fmt.Fprintf(&b, " %s %g/%d;", op, snap.Sum("aries.control.writes", telemetry.KeyOp, op, telemetry.KeyResult, telemetry.ResultOK), want.control[op])
	}
	b.WriteString("\n")
	for _, p := range s.projs {
		fmt.Fprintf(&b, "  project %s: %d writes committed for %d transactions with events, %g retries\n", p.name,
			snap.Observations("aries.store.write.duration", telemetry.KeyProject, p.id), want.txs[p.id],
			snap.Sum("aries.store.write.retries", telemetry.KeyProject, p.id))
	}
	journaled := 0
	for _, n := range want.acked {
		journaled += n
	}
	fmt.Fprintf(&b, "  requests: %g measured, %g of them ok (the sessions saw %d answered ok); handshakes %g; connections closed by the server %g (the run saw %d close as access ended)\n",
		snap.Sum("aries.project.requests"), snap.Sum("aries.project.requests", telemetry.KeyResultClass, telemetry.ClassOK), journaled,
		snap.Sum("aries.handshakes"), snap.Sum("aries.connections.dropped"), want.closed)
	s.t.Log(b.String())
}

// checkDeadCollector checks what a collector that is not there may
// change: the count of what was dropped, and one warning for each
// signal (design §16.6). That it changed nothing else is every other
// check of the run, which the caller has made.
func (s *soak) checkDeadCollector() {
	if s.tel.metricsURL == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dropped, err := scrapeDropped(ctx, s.tel.metricsURL)
	if err != nil {
		s.t.Errorf("soak: telemetry: read the daemon's own /metrics: %v", err)
		return
	}
	for _, signal := range []string{telemetry.SignalMetrics, telemetry.SignalTraces} {
		if dropped[signal] <= 0 {
			s.mon.fail("", "telemetry: aries_telemetry_dropped_total{signal=%q} is %g after a run with a collector that is not there (%s), want more than 0",
				signal, dropped[signal], s.tel.mode)
		}
	}
	// One line when the failures start, and one more every ten minutes
	// they last.
	most := 1 + int(s.ended/(10*time.Minute))
	for _, msg := range []string{"a metrics export failed", "a trace export failed"} {
		if n := s.logs.matching(msg); n < 1 || n > most {
			s.mon.fail("", "telemetry: the daemon logged %q %d times in %s with a collector that is not there (%s), want 1 to %d",
				msg, n, s.ended.Round(time.Second), s.tel.mode, most)
		}
	}
	s.t.Logf("soak: telemetry, with a collector that is not there (%s): %g exports of metrics failed and %g spans were dropped; every other check ran as it does with telemetry off",
		s.tel.mode, dropped[telemetry.SignalMetrics], dropped[telemetry.SignalTraces])
}

// scrapeDropped reads aries_telemetry_dropped_total, by signal, from
// a /metrics endpoint.
func scrapeDropped(ctx context.Context, url string) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	out := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		series, value, ok := strings.Cut(sc.Text(), " ")
		if !ok || !strings.HasPrefix(series, "aries_telemetry_dropped_total{") {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sc.Text(), err)
		}
		for _, signal := range []string{telemetry.SignalMetrics, telemetry.SignalTraces} {
			if strings.Contains(series, `signal="`+signal+`"`) {
				out[signal] = v
			}
		}
	}
	return out, sc.Err()
}

// auditLine is one line of the daemon's audit stream (design §16.4): the
// number of the control store's event it copies, and what it says of
// it.
type auditLine struct {
	seq                   int64
	op, principal, target string
}

// keepAudit keeps r if it is a line of the audit stream.
func (l *logCounter) keepAudit(r slog.Record) {
	var a auditLine
	audit := false
	r.Attrs(func(at slog.Attr) bool {
		switch at.Key {
		case logs.KeyAudit:
			audit = at.Value.Kind() == slog.KindBool && at.Value.Bool()
		case logs.KeySeq:
			a.seq = at.Value.Int64()
		case logs.KeyOp:
			a.op = at.Value.String()
		case logs.KeyPrincipal:
			a.principal = at.Value.String()
		case logs.KeyTarget:
			a.target = at.Value.String()
		}
		return true
	})
	if !audit {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.audits = append(l.audits, a)
}

// compareAudit returns every way the audit stream's lines disagree with
// the control store's event log, evs, whose event of number n is
// evs[n-1]. A line copies one event: its op, its actor and its target.
// The lines come in the log's order and none comes twice. And every
// event that changes access has its line, but the operator's, made
// before the daemon ran, and those a daemon had not yet read when it
// was stopped: the stream starts again at the newest event there is,
// so at most the events of one look are lost at each of restarts.
func compareAudit(lines []auditLine, evs []cevent, restarts int) []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }
	seen := map[int64]bool{}
	last := int64(0)
	for _, l := range lines {
		switch {
		case l.seq < 1 || l.seq > int64(len(evs)):
			bad("an audit line has seq %d (%s by %s on %s), and the control store's log has events 1 to %d", l.seq, l.op, l.principal, l.target, len(evs))
			continue
		case seen[l.seq]:
			bad("the audit stream has event %d (%s) twice", l.seq, l.op)
		case l.seq < last:
			bad("the audit stream has event %d after event %d: out of the log's order", l.seq, last)
		}
		seen[l.seq], last = true, max(last, l.seq)
		if e := evs[l.seq-1]; e.op != l.op || e.by != l.principal || e.target != l.target {
			bad("the audit line of event %d says %s by %s on %s, and the control store's log says %s by %s on %s",
				l.seq, l.op, l.principal, l.target, e.op, e.by, e.target)
		}
	}
	var missing []string
	for i, e := range evs {
		if _, audited := logs.Audit(e.op); audited && e.by != "" && e.by != store.OperatorPrincipal && !seen[int64(i+1)] {
			missing = append(missing, fmt.Sprintf("%d (%s by %s on %s)", i+1, e.op, e.by, e.target))
		}
	}
	// One look reads the events of one request, or of one tick.
	if most := restarts * auditPerLook; len(missing) > most {
		bad("the audit stream has no line for %d events that change access, want at most %d for %d restarts: %s",
			len(missing), most, restarts, strings.Join(missing[:min(len(missing), 8)], "; "))
	}
	return out
}

// auditPerLook is how many events the audit stream may lose when the
// daemon stops between a change and the look that would have read it:
// the events of the changes in flight, each at most a principal, its
// keys and its membership.
const auditPerLook = 8

// The check of the audit stream catches what it claims to.
func TestAuditCheckCatches(t *testing.T) {
	t.Parallel()
	evs := []cevent{
		{by: store.OperatorPrincipal, op: "project.add", target: "p1"},
		{by: "root", op: "principal.add", target: "alice"},
		{by: "root", op: "member.set", target: "p1/alice"},
		{by: "ariesd", op: "clock.step", target: "clock"},
		{by: "root", op: "key.remove", target: "SHA256:abc"},
	}
	right := []auditLine{{2, "principal.add", "root", "alice"}, {3, "member.set", "root", "p1/alice"}, {5, "key.remove", "root", "SHA256:abc"}}
	if got := compareAudit(right, evs, 0); len(got) > 0 {
		t.Fatalf("compareAudit of a stream that is the log's = %q, want nothing", got)
	}
	tests := []struct {
		name     string
		lines    []auditLine
		restarts int
		says     string
	}{
		{"another actor", []auditLine{{2, "principal.add", "bob", "alice"}, right[1], right[2]}, 0, "says principal.add by bob on alice"},
		{"another target", []auditLine{right[0], {3, "member.set", "root", "p1/bob"}, right[2]}, 0, "says member.set by root on p1/bob"},
		{"another op", []auditLine{right[0], right[1], {5, "key.add", "root", "SHA256:abc"}}, 0, "says key.add by root"},
		{"a line twice", []auditLine{right[0], right[0], right[1], right[2]}, 0, "has event 2 (principal.add) twice"},
		{"out of order", []auditLine{right[1], right[0], right[2]}, 0, "has event 2 after event 3"},
		{"an event that is not there", append(slices.Clone(right), auditLine{9, "key.add", "root", "x"}), 0, "has seq 9"},
		{"a line missing", right[:2], 0, "no line for 1 events that change access"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := compareAudit(tc.lines, evs, tc.restarts)
			if !slices.ContainsFunc(got, func(v string) bool { return strings.Contains(v, tc.says) }) {
				t.Errorf("compareAudit = %q, want a violation containing %q", got, tc.says)
			}
		})
	}
	// A restart may lose the lines of the changes in flight.
	if got := compareAudit(right[:2], evs, 1); len(got) > 0 {
		t.Errorf("compareAudit with one line missing and one restart = %q, want nothing", got)
	}
}

// matching counts the lines logged whose message contains msg.
func (l *logCounter) matching(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for line, count := range l.lines {
		if strings.Contains(line, msg) {
			n += count
		}
	}
	return n
}

// The check of the instruments against the logs catches what it claims
// to: each case is a run whose telemetry is right but for one thing.
func TestTelemetryCheckCatches(t *testing.T) {
	t.Parallel()
	const pa = "11111111-1111-4111-8111-111111111111"
	point := func(name string, value float64, attrs ...string) telemetry.Point {
		p := telemetry.Point{Name: name, Value: value, Attrs: map[string]string{}}
		for i := 0; i+1 < len(attrs); i += 2 {
			p.Attrs[attrs[i]] = attrs[i+1]
		}
		return p
	}
	hist := func(name string, count uint64, attrs ...string) telemetry.Point {
		p := point(name, 0, attrs...)
		p.Count = count
		return p
	}
	span := func(name, kind string, attrs ...string) telemetry.SpanData {
		sp := telemetry.SpanData{Name: name, Kind: kind, Attrs: map[string]string{}}
		for i := 0; i+1 < len(attrs); i += 2 {
			sp.Attrs[attrs[i]] = attrs[i+1]
		}
		return sp
	}
	// A run of one project: three takes, one expiry, one plain release
	// and one for a removed membership; four transactions with events
	// among five writes, one of them retried; two creates, a refused
	// show and two handshakes; one connection closed with a membership.
	snap := func() telemetry.Snapshot {
		return telemetry.Snapshot{
			point("aries.claims", 3, "event", "take", "reason", "none"),
			point("aries.claims", 1, "event", "expire", "reason", "none"),
			point("aries.claims", 1, "event", "release", "reason", "none"),
			point("aries.claims", 1, "event", "release", "reason", "membership"),
			point("aries.claims", 7, "event", "renew", "reason", "none"),
			point("aries.control.writes", 1, "op", "member.remove", "result", "ok"),
			hist("aries.store.write.duration", 5, "project", pa),
			point("aries.store.write.retries", 1, "project", pa),
			hist("aries.request.duration", 2, "op", "create", "result", "ok"),
			hist("aries.request.duration", 1, "op", "show", "result", "not_found"),
			hist("aries.request.duration", 1, "op", "member.remove", "result", "ok"),
			point("aries.project.requests", 3, "project", pa, "result_class", "ok"),
			point("aries.project.requests", 1, "project", pa, "result_class", "refused"),
			point("aries.handshakes", 2, "result", "ok"),
			point("aries.connections.dropped", 1, "reason", "membership"),
		}
	}
	spans := func() []telemetry.SpanData {
		var out []telemetry.SpanData
		for range 5 {
			out = append(out, span("store.write.attempt", "internal", "project", pa, "outcome", "ok"))
		}
		return append(out,
			span("store.write.attempt", "internal", "project", pa, "outcome", "retry"),
			span("create", "server", "op", "create", "result", "ok", "principal", "p_0123456789abcdef", "session", "s_0123456789abcdef"),
			span("create", "server", "op", "create", "result", "ok", "principal", "p_0123456789abcdef", "session", "s_0123456789abcdef"),
			span("show", "server", "op", "show", "result", "not_found", "principal", "p_0123456789abcdef"),
			span("member.remove", "server", "op", "member.remove", "result", "ok", "principal", "p_fedcba9876543210"),
			span("handshake", "server", "result", "ok", "principal", "p_0123456789abcdef"),
			span("handshake", "server", "result", "ok", "principal", "p_fedcba9876543210"),
			span("reaper", "internal"))
	}
	want := func() telWant {
		return telWant{
			claims:  map[[2]string]int{{"take", "none"}: 3, {"expire", "none"}: 1, {"release", "none"}: 1, {"release", "membership"}: 1},
			control: map[string]int{"member.remove": 1}, txs: map[string]int{pa: 4},
			acked: map[string]int{"create": 2}, refused: map[[2]string]int{{"show", "not_found"}: 1},
			closed: 1, ended: map[string]bool{"membership": true, "disabled": false, "archived": false, "key": false},
		}
	}
	tally := func(spans []telemetry.SpanData) *spanTally {
		var st spanTally
		st.add(spans, func(v string) bool { return v == "alice" || v == "lib-alice-1g0" })
		return &st
	}
	set := func(s telemetry.Snapshot, name string, value float64, count uint64, attrs ...string) telemetry.Snapshot {
		for i, p := range s {
			if q := point(name, 0, attrs...); p.Name == name && maps.Equal(p.Attrs, q.Attrs) {
				s[i].Value, s[i].Count = value, count
				return s
			}
		}
		return append(s, telemetry.Point{Name: name, Value: value, Count: count, Attrs: point(name, 0, attrs...).Attrs})
	}

	if got := compareTelemetry(snap(), tally(spans()), want()); len(got) > 0 {
		t.Fatalf("compareTelemetry of a run whose telemetry is right = %q, want nothing", got)
	}
	// A change asked for again, after its answer was lost, succeeds
	// twice and is in the log once.
	again := set(snap(), "aries.control.writes", 2, 0, "op", "member.remove", "result", "ok")
	again = set(again, "aries.request.duration", 0, 2, "op", "member.remove", "result", "ok")
	again = set(again, "aries.project.requests", 4, 0, "project", pa, "result_class", "ok")
	resent := append(spans(), span("member.remove", "server", "op", "member.remove", "result", "ok", "principal", "p_0123456789abcdef"))
	if got := compareTelemetry(again, tally(resent), want()); len(got) > 0 {
		t.Errorf("compareTelemetry with a control write sent again after a lost answer = %q, want nothing", got)
	}
	tests := []struct {
		name  string
		snap  func(telemetry.Snapshot) telemetry.Snapshot
		spans func([]telemetry.SpanData) []telemetry.SpanData
		want  func(*telWant)
		says  string
	}{
		{name: "a take not counted", says: `aries.claims{event=take,reason=none} is 2, and the projects' event logs have 3`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.claims", 2, 0, "event", "take", "reason", "none")
			}},
		{name: "an expiry counted twice", says: `aries.claims{event=expire,reason=none} is 2`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.claims", 2, 0, "event", "expire", "reason", "none")
			}},
		{name: "a release for an archive counted as plain", says: `no series for event=release, reason=archived`,
			want: func(w *telWant) { w.claims[[2]string{"release", "archived"}] = 1 }},
		{name: "a release under a reason the log does not give", says: `aries.claims{event=release,reason=disabled} is 1, and the projects' event logs have 0`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.claims", 1, 0, "event", "release", "reason", "disabled")
			}},
		{name: "a control write counted twice", says: `aries.control.writes{op=member.remove,result=ok} is 2, and aries.request.duration has 1 observations`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.control.writes", 2, 0, "op", "member.remove", "result", "ok")
			}},
		{name: "a control write not counted", says: `aries.control.writes{op=member.remove,result=ok} is 0, and the control store's log has 1 changes`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.control.writes", 0, 0, "op", "member.remove", "result", "ok")
			}},
		{name: "a control write the log lacks", says: `aries.control.writes{op=key.add,result=ok} is 1, and aries.request.duration has 0 observations`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.control.writes", 1, 0, "op", "key.add", "result", "ok")
			}},
		{name: "fewer writes than transactions", says: `has 3 observations, and the project's event log has 4 transactions`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.store.write.duration", 0, 3, "project", pa)
			}},
		{name: "a write with no span", says: `4 spans of a write attempt that ended ok`,
			spans: func(s []telemetry.SpanData) []telemetry.SpanData { return s[1:] }},
		{name: "a retry not counted", says: `aries.store.write.retries{project=` + pa + `} is 0, and 1 spans`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.store.write.retries", 0, 0, "project", pa)
			}},
		{name: "a request with no span", says: `aries.request.duration{op=show,result=not_found} has 1 observations, and 0 spans`,
			spans: func(s []telemetry.SpanData) []telemetry.SpanData {
				return slices.DeleteFunc(s, func(sp telemetry.SpanData) bool { return sp.Name == "show" })
			}},
		{name: "a request measured under another result", says: `aries.request.duration{op=create,result=ok} has 1 observations, and 2 spans`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				s = set(s, "aries.request.duration", 0, 1, "op", "create", "result", "ok")
				return set(s, "aries.request.duration", 0, 1, "op", "create", "result", "invalid")
			}},
		{name: "a request not counted for its project", says: `aries.project.requests counts 3 requests, and aries.request.duration has 4`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.project.requests", 0, 0, "project", pa, "result_class", "refused")
			}},
		{name: "fewer answered than the sessions saw", says: `the sessions saw 3 answered ok`,
			want: func(w *telWant) { w.acked["create"] = 3 }},
		{name: "fewer refused than the sessions saw", says: `the sessions saw 2 refused so`,
			want: func(w *telWant) { w.refused[[2]string{"show", "not_found"}] = 2 }},
		{name: "a handshake not counted", says: `aries.handshakes{result=ok} is 1, and 2 spans`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.handshakes", 1, 0, "result", "ok")
			}},
		{name: "a closed connection not counted", says: `counts 0 connections closed as access ended, and the run saw 1`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.connections.dropped", 0, 0, "reason", "membership")
			}},
		{name: "a connection closed for a reason the log lacks", says: `aries.connections.dropped{reason=archived} is 1`,
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.connections.dropped", 1, 0, "reason", "archived")
			}},
		{name: "a principal by name", says: `has principal="alice", want a stand-in`,
			spans: func(s []telemetry.SpanData) []telemetry.SpanData {
				return append(s, span("show", "server", "op", "show", "result", "not_found", "principal", "alice"))
			},
			snap: func(s telemetry.Snapshot) telemetry.Snapshot {
				return set(s, "aries.request.duration", 0, 2, "op", "show", "result", "not_found")
			}},
		{name: "a session id in another attribute", says: `carries project="lib-alice-1g0"`,
			spans: func(s []telemetry.SpanData) []telemetry.SpanData {
				return append(s, span("reaper", "internal", "project", "lib-alice-1g0"))
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sn, sp, w := snap(), spans(), want()
			if tc.snap != nil {
				sn = tc.snap(sn)
			}
			if tc.spans != nil {
				sp = tc.spans(sp)
			}
			if tc.want != nil {
				tc.want(&w)
			}
			got := compareTelemetry(sn, tally(sp), w)
			if !slices.ContainsFunc(got, func(v string) bool { return strings.Contains(v, tc.says) }) {
				t.Errorf("compareTelemetry = %q, want a violation containing %q", got, tc.says)
			}
		})
	}
}

// ARIES_SOAK_TELEMETRY takes one of three words, and nothing else.
func TestSoakTelemetryConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value, want string
		refused     bool
	}{
		{"", telOff, false}, {"check", telCheck, false}, {"refused", telRefused, false}, {"hang", telHang, false},
		{"1", "", true}, {"on", "", true},
	} {
		cfg, err := soakConfigFrom(func(k string) string {
			if k == "ARIES_SOAK_TELEMETRY" {
				return tc.value
			}
			return ""
		})
		if (err != nil) != tc.refused || cfg.telemetry != tc.want {
			t.Errorf("soakConfigFrom with ARIES_SOAK_TELEMETRY=%q = telemetry %q, %v; want %q, refused %v", tc.value, cfg.telemetry, err, tc.want, tc.refused)
		}
	}
}
