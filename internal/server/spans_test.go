package server

import (
	"cmp"
	"context"
	"maps"
	"net"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// A caller's trace context, as a client sends one.
const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
	sampledTP   = "00-" + callerTrace + "-" + callerSpan + "-01"
	unsampledTP = "00-" + callerTrace + "-" + callerSpan + "-00"
)

var (
	principalAlias = regexp.MustCompile(`^p_[0-9a-f]{16}$`)
	sessionAlias   = regexp.MustCompile(`^s_[0-9a-f]{16}$`)
	hexID          = regexp.MustCompile(`^[0-9a-f]+$`)
)

// spanned makes a Telemetry whose spans and instruments a test reads in
// process.
func spanned(t *testing.T, cfg telemetry.Config) (*telemetry.Telemetry, *telemetry.Manual) {
	t.Helper()
	cfg.Version, cfg.PerProject, cfg.Ops = "v0.2.0", true, Ops()
	tel, m, err := telemetry.NewManual(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel, m
}

// newServerLogged is newServerTel with the server's log kept, at debug.
func newServerLogged(t *testing.T, tel *telemetry.Telemetry) (*Server, *syncBuffer) {
	t.Helper()
	dsn := newDatabase(t)
	st, err := store.Open(t.Context(), dsn, store.Options{Prefix: "sf", Admins: []string{dana.Principal},
		Observer: StoreObserver(tel, project), Tracer: StoreTracer(tel, project)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := &syncBuffer{}
	s, err := New(Config{Store: st, Project: project, Version: "v0.2.0", PeerCheck: func(net.Conn) error { return nil },
		Telemetry: tel, ReapInterval: -1, Logger: slogTo(log)})
	if err != nil {
		t.Fatal(err)
	}
	return s, log
}

// tree draws the span and those beneath it by name, children in the
// order they started: "create(store.write.attempt(store.write.wait))".
func tree(spans []telemetry.SpanData, root telemetry.SpanData) string {
	var kids []telemetry.SpanData
	for _, s := range spans {
		if s.ParentID == root.SpanID && s.TraceID == root.TraceID {
			kids = append(kids, s)
		}
	}
	if len(kids) == 0 {
		return root.Name
	}
	slices.SortStableFunc(kids, func(a, b telemetry.SpanData) int { return a.Start.Compare(b.Start) })
	parts := make([]string, len(kids))
	for i, k := range kids {
		parts[i] = tree(spans, k)
	}
	return root.Name + "(" + strings.Join(parts, " ") + ")"
}

// roots returns the spans with no parent among spans, in the order
// they started: the root of each trace, and a request's span, whose
// parent is the caller's.
func roots(spans []telemetry.SpanData) []telemetry.SpanData {
	ids := map[string]bool{}
	for _, s := range spans {
		ids[s.SpanID] = true
	}
	var out []telemetry.SpanData
	for _, s := range spans {
		if !ids[s.ParentID] {
			out = append(out, s)
		}
	}
	slices.SortStableFunc(out, func(a, b telemetry.SpanData) int { return a.Start.Compare(b.Start) })
	return out
}

// forest draws every trace among spans, as tree does.
func forest(spans []telemetry.SpanData) []string {
	var out []string
	for _, r := range roots(spans) {
		out = append(out, tree(spans, r))
	}
	return out
}

// logLine returns the log's last line for a request of op, or "".
func logLine(log *syncBuffer, op string) string {
	last := ""
	for line := range strings.SplitSeq(log.String(), "\n") {
		if strings.Contains(line, "msg=request ") && strings.Contains(line, " op="+op+" ") {
			last = line
		}
	}
	return last
}

// logged returns the value of key on a text log line, or "".
func logged(line, key string) string {
	_, rest, ok := strings.Cut(line, " "+key+"=")
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, " ")
	return v
}

// A known sequence of requests through a server and a store gives
// exactly these spans (design §16.4): one server span for each request,
// named for its op and parented on the caller's span when the request
// brought a valid trace context, with the store's work beneath it; and
// the request's log line names the trace and the server's span.
func TestRequestSpans(t *testing.T) {
	t.Parallel()
	tel, m := spanned(t, telemetry.Config{})
	s, log := newServerLogged(t, tel)
	c := dialPipe(t, s, alice)
	alicesAlias, ok := tel.StandIn("alice")
	if !ok {
		t.Fatal("the telemetry has no stand-in key")
	}
	// The first answer comes after the handshake's span has ended.
	traced(c, proto.OpClaims, proto.LimitArgs{}, "", "")

	t.Run("handshake", func(t *testing.T) {
		spans := m.Spans()
		var hs telemetry.SpanData
		for _, sp := range spans {
			if sp.Name == "handshake" {
				hs = sp
			}
		}
		if hs.Kind != "server" || hs.ParentID != "" {
			t.Fatalf("handshake span = %+v, want a server span that is the root of its trace\n%s", hs, telemetry.SpanList(spans))
		}
		if got, want := tree(spans, hs), "handshake(store.write.attempt(store.write.wait))"; got != want {
			t.Errorf("handshake trace = %s, want %s: the registry's note of the session beneath the handshake", got, want)
		}
		want := map[string]string{"result": "ok", "project": project, "protocol": strconv.Itoa(proto.Proto), "principal": alicesAlias}
		got := maps.Clone(hs.Attrs)
		session := got["session"]
		delete(got, "session")
		if !maps.Equal(got, want) || !sessionAlias.MatchString(session) {
			t.Errorf("handshake attributes = %v, want %v and a session stand-in", hs.Attrs, want)
		}
	})
	m.ResetSpans()
	created := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "an issue to start"})
	m.ResetSpans()

	steps := []struct {
		name          string
		op            string
		args          any
		parent, state string
		refused       proto.Code
		// span is the server span's name, and tree the trace beneath it.
		span, tree string
		// remote says the span's parent is the caller's, in its trace.
		remote bool
		result string
		status string
		// none says no span is recorded: the caller did not sample.
		none bool
	}{
		{name: "a write with the caller's trace context", op: proto.OpCreate, args: proto.CreateArgs{Title: "first", Body: "text"},
			parent: sampledTP, state: "vendor=opaque-STATE",
			span: "create", tree: "create(store.write.attempt(store.write.wait))", remote: true, result: "ok"},
		{name: "a read with no trace context", op: proto.OpList, args: proto.ListArgs{}, span: "list", tree: "list", result: "ok"},
		{name: "a refusal is the request's, and no failure of the span", op: proto.OpShow, args: proto.ShowArgs{ID: "sf-none"},
			parent: sampledTP, refused: proto.CodeNotFound, span: "show", tree: "show", remote: true, result: "not_found"},
		{name: "an op the server does not know", op: "no.such.op", args: struct{}{}, refused: proto.CodeInvalid,
			span: "unknown", tree: "unknown", result: "invalid"},
		{name: "a trace context that is not one", op: proto.OpReady, args: proto.LimitArgs{},
			parent: "00-EVIL\x1b[2K" + callerTrace[9:] + "-" + callerSpan + "-01", state: "EVIL=state", span: "ready", tree: "ready", result: "ok"},
		{name: "a later version's trace context", op: proto.OpBlocked, args: proto.LimitArgs{},
			parent: "01-" + callerTrace + "-" + callerSpan + "-01", span: "blocked", tree: "blocked", result: "ok"},
		{name: "a write of several parts", op: proto.OpStart, args: proto.StartArgs{ID: created.ID}, parent: sampledTP,
			span: "start", tree: "start(store.write.attempt(store.write.wait))", remote: true, result: "ok"},
		{name: "an admin op on a server of one project", op: proto.OpProjects, args: proto.AdminListArgs{}, refused: proto.CodeInvalid,
			span: "projects", tree: "projects", result: "invalid"},
		{name: "a caller that did not sample", op: proto.OpWho, args: proto.WhoArgs{}, parent: unsampledTP, none: true},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			m.ResetSpans()
			r := traced(c, st.op, st.args, st.parent, st.state)
			switch {
			case st.refused == "" && r.Err != nil:
				t.Fatalf("%s = %v; tracing must never fail a request", st.op, r.Err)
			case st.refused != "" && (r.Err == nil || r.Err.Code != st.refused):
				t.Fatalf("%s = %+v, want the refusal %s", st.op, r.Err, st.refused)
			}
			spans := m.Spans()
			line := logLine(log, st.op)
			if line == "" {
				t.Fatalf("no log line for the %s request:\n%s", st.op, log)
			}
			lineTrace, lineSpan := logged(line, "trace_id"), logged(line, "span_id")
			if st.none {
				if len(spans) != 0 {
					t.Errorf("%d spans recorded for a trace the caller did not sample, want none:\n%s", len(spans), telemetry.SpanList(spans))
				}
				// The line still joins the caller's trace, under a span
				// of the server's own.
				if lineTrace != callerTrace || len(lineSpan) != 16 || !hexID.MatchString(lineSpan) || lineSpan == callerSpan {
					t.Errorf("the log line has trace_id=%s span_id=%s; want the caller's trace %s and a span id of the server's\n%s",
						lineTrace, lineSpan, callerTrace, line)
				}
				return
			}
			top := roots(spans)
			if len(top) != 1 {
				t.Fatalf("%d traces for one request, want 1:\n%s", len(top), telemetry.SpanList(spans))
			}
			sp := top[0]
			if sp.Name != st.span || sp.Kind != "server" {
				t.Errorf("the request's span is %s (%s), want %s (server)", sp.Name, sp.Kind, st.span)
			}
			if got := tree(spans, sp); got != st.tree {
				t.Errorf("trace = %s, want %s", got, st.tree)
			}
			if st.remote {
				if sp.TraceID != callerTrace || sp.ParentID != callerSpan || !sp.RemoteParent {
					t.Errorf("span is in trace %s under %s (remote %t); want the caller's trace %s and span %s", sp.TraceID, sp.ParentID, sp.RemoteParent, callerTrace, callerSpan)
				}
			} else if sp.ParentID != "" || sp.TraceID == callerTrace {
				t.Errorf("span is in trace %s under %q; want the root of a trace of its own", sp.TraceID, sp.ParentID)
			}
			want := map[string]string{"op": st.span, "result": st.result, "project": project, "protocol": strconv.Itoa(proto.Proto), "principal": alicesAlias}
			got := maps.Clone(sp.Attrs)
			session := got["session"]
			delete(got, "session")
			if !maps.Equal(got, want) || !sessionAlias.MatchString(session) {
				t.Errorf("attributes = %v, want %v and a session stand-in", sp.Attrs, want)
			}
			if sp.Status != st.status {
				t.Errorf("status = %q (%s), want %q", sp.Status, sp.StatusNote, st.status)
			}
			for _, child := range spans {
				if child.SpanID == sp.SpanID {
					continue
				}
				if child.TraceID != sp.TraceID || child.Kind != "internal" || child.Attrs["project"] != project || child.Attrs["outcome"] != "ok" {
					t.Errorf("child span %s: trace %s, kind %s, attributes %v; want the request's trace, internal, the project and outcome ok",
						child.Name, child.TraceID, child.Kind, child.Attrs)
				}
				if child.Name == "store.write.attempt" && child.Attrs["attempt"] != "1" {
					t.Errorf("the attempt's number is %q, want 1", child.Attrs["attempt"])
				}
				if _, who := child.Attrs["principal"]; who {
					t.Errorf("child span %s names a principal: only the request's span says who", child.Name)
				}
			}
			// The line and the trace can be joined: the trace's id, and
			// the id of the server's span, not the caller's.
			if lineTrace != sp.TraceID || lineSpan != sp.SpanID {
				t.Errorf("the log line has trace_id=%s span_id=%s; want the span's, %s and %s\n%s", lineTrace, lineSpan, sp.TraceID, sp.SpanID, line)
			}
		})
	}
	// A request the server could not serve is a failure of its span,
	// with the code and never the message.
	t.Run("a request the server could not serve", func(t *testing.T) {
		if err := s.cfg.Store.Close(); err != nil {
			t.Fatal(err)
		}
		m.ResetSpans()
		r := traced(c, proto.OpList, proto.ListArgs{}, sampledTP, "")
		if r.Err == nil || r.Err.Code != proto.CodeUnavailable {
			t.Fatalf("list on a closed store = %+v, want unavailable", r.Err)
		}
		spans := m.Spans()
		if len(spans) != 1 || spans[0].Status != "error" || spans[0].StatusNote != "unavailable" || spans[0].Attrs["result"] != "unavailable" {
			t.Errorf("spans = \n%swant one, failed, with the status description and the result unavailable", telemetry.SpanList(spans))
		}
	})

	// Nothing of a trace context that was ignored, and no tracestate at
	// all, reaches the log or a span.
	for _, leaked := range []string{"EVIL", "opaque-STATE"} {
		if strings.Contains(log.String(), leaked) {
			t.Errorf("the log holds %q, from a trace context:\n%s", leaked, log)
		}
	}
}

// The store's tracer gives each attempt of a write a span of its own,
// numbered, so a write that is retried shows as several; a retry is not
// a failure of the span, and a migration that fails is.
func TestStoreTracer(t *testing.T) {
	t.Parallel()
	if tr := StoreTracer(nil, projA); tr != nil {
		t.Errorf("StoreTracer(nil) = %v, want nil: the store then calls nothing", tr)
	}
	off, err := telemetry.New(t.Context(), telemetry.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if tr := StoreTracer(off, projA); tr != nil {
		t.Errorf("StoreTracer of a Telemetry that traces nothing = %v, want nil", tr)
	}

	tel, m := spanned(t, telemetry.Config{})
	tr := StoreTracer(tel, projA)
	ctx, req := tel.StartRequest(t.Context(), proto.OpCreate, proto.TraceContext{})
	for attempt, out := range []store.Outcome{store.OutcomeRetry, store.OutcomeRetry, store.OutcomeOK} {
		actx, end := tr.Begin(ctx, store.StageAttempt, attempt+1)
		_, waited := tr.Begin(actx, store.StageWait, 0)
		waited(store.OutcomeOK)
		end(out)
	}
	_, end := tr.Begin(ctx, store.StageAttempt, 4)
	end(store.OutcomeError)
	for _, stage := range []store.Stage{store.StageMigrate, store.StageWait} {
		_, end := tr.Begin(ctx, stage, 0)
		end(store.OutcomeError)
	}
	_, end = StoreTracer(tel, "").Begin(ctx, store.StageMigrate, 0)
	end(store.OutcomeOK)
	req.End()
	spans := m.Spans()
	want := "create(" + strings.Repeat("store.write.attempt(store.write.wait) ", 3) + "store.write.attempt store.migrate store.write.wait store.migrate)"
	if got := tree(spans, spans[len(spans)-1]); got != want {
		t.Errorf("trace = %s\nwant    %s", got, want)
	}
	var attempts, failed []string
	for _, sp := range spans {
		if sp.Name == "store.write.attempt" {
			attempts = append(attempts, sp.Attrs["attempt"]+":"+sp.Attrs["outcome"]+":"+sp.Status)
		}
		if sp.Status == "error" {
			failed = append(failed, sp.Name+":"+sp.StatusNote)
		}
	}
	if want := []string{"1:retry:", "2:retry:", "3:ok:", "4:error:"}; !slices.Equal(attempts, want) {
		t.Errorf("attempts, as number:outcome:status = %v, want %v", attempts, want)
	}
	if want := []string{"store.migrate:error", "store.write.wait:error"}; !slices.Equal(failed, want) {
		t.Errorf("spans marked failed = %v, want %v", failed, want)
	}
	last := spans[len(spans)-2]
	if _, ok := last.Attrs["project"]; ok || last.Name != "store.migrate" {
		t.Errorf("the control store's span %s has attributes %v, want a migration that names no project", last.Name, last.Attrs)
	}

	// In a trace that is not sampled nothing is recorded, and the
	// stages begun beneath keep the decision.
	m.ResetSpans()
	ctx, req = tel.StartRequest(t.Context(), proto.OpCreate, proto.TraceContext{TraceID: callerTrace, SpanID: callerSpan, Flags: "00"})
	actx, end := tr.Begin(ctx, store.StageAttempt, 1)
	_, waited := tr.Begin(actx, store.StageWait, 0)
	waited(store.OutcomeOK)
	end(store.OutcomeOK)
	req.End()
	if got := m.Spans(); len(got) != 0 {
		t.Errorf("%d spans recorded in an unsampled trace:\n%s", len(got), telemetry.SpanList(got))
	}
	if id, _, ok := telemetry.IDs(actx); !ok || id != callerTrace {
		t.Errorf("the attempt's context is in trace %q (%t), want the caller's", id, ok)
	}
}

// Background work is visible: a tick of the reaper is one trace, with
// the clock check, the reread of the control store, the write of the
// keys file and the access sweep beneath it; each of those is the root
// of a trace when it runs on its own; and a handshake that opens a
// project's store shows the opening and its migrations.
func TestBackgroundSpans(t *testing.T) {
	t.Parallel()
	tel, m := spanned(t, telemetry.Config{})
	mu := newMultiTel(t, Limits{}, nil, tel, dana.Principal)
	mu.project(t, projA, "alice")
	m.ResetSpans()

	c := mustDialTo(t, mu.Server, alice, projA)
	mustAsk[proto.ListResult](c, proto.OpReady, proto.LimitArgs{})
	t.Run("a handshake that opens the store", func(t *testing.T) {
		spans := m.Spans()
		got := forest(spans)
		// The store is opened and migrated; then, before it serves, its
		// lapsed claims are ended, its holders' access swept and its old
		// rows pruned; then the session is noted in the registry.
		want := []string{
			"handshake(store.open(store.migrate) " +
				"store.write.attempt(store.write.wait) " +
				"access.sweep(store.write.attempt(store.write.wait)) " +
				"store.write.attempt(store.write.wait) " +
				"store.write.attempt(store.write.wait))",
			"ready",
		}
		if !slices.Equal(got, want) {
			t.Errorf("traces =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		for _, sp := range spans {
			switch sp.Name {
			case "store.open":
				if sp.Attrs["project"] != projA || sp.Attrs["migrated"] != "true" || sp.Status != "" {
					t.Errorf("store.open: attributes %v, status %q; want the project, migrated true and no failure", sp.Attrs, sp.Status)
				}
			case "access.sweep":
				if sp.Attrs["project"] != projA {
					t.Errorf("access.sweep of one store: attributes %v, want its project", sp.Attrs)
				}
			}
		}
	})

	t.Run("a tick of the reaper", func(t *testing.T) {
		// The keys file is gone, so this tick writes it.
		if err := os.Remove(mu.keysFile); err != nil {
			t.Fatal(err)
		}
		m.ResetSpans()
		mu.tick(t.Context(), true)
		spans := m.Spans()
		got := forest(spans)
		want := []string{"reaper(clock.check control.reload keys_file.write " +
			"store.write.attempt(store.write.wait) " + // the lapsed claims of the one open store
			"access.sweep(store.write.attempt(store.write.wait)) " +
			"store.write.attempt(store.write.wait))"} // its prune
		if !slices.Equal(got, want) {
			t.Errorf("traces =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
		var reaper, keys telemetry.SpanData
		for _, sp := range spans {
			switch sp.Name {
			case "reaper":
				reaper = sp
			case "keys_file.write":
				keys = sp
			}
			if _, who := sp.Attrs["principal"]; who {
				t.Errorf("background span %s names a principal", sp.Name)
			}
		}
		if reaper.Kind != "internal" || reaper.ParentID != "" {
			t.Errorf("reaper span = %+v, want an internal span at the root of its trace", reaper)
		}
		if want := map[string]string{"result": "ok", "changed": "true"}; !maps.Equal(keys.Attrs, want) {
			t.Errorf("keys_file.write attributes = %v, want %v", keys.Attrs, want)
		}
		// The line of what the tick did carries the tick's trace, and
		// the span it was logged under.
		var line string
		for l := range strings.SplitSeq(mu.log.String(), "\n") {
			if strings.Contains(l, `msg="keys file written"`) {
				line = l // the last one
			}
		}
		if logged(line, "trace_id") != reaper.TraceID || logged(line, "span_id") != keys.SpanID {
			t.Errorf("the line of the keys file's write has trace_id=%s span_id=%s; want the tick's trace %s and the write's span %s\n%s",
				logged(line, "trace_id"), logged(line, "span_id"), reaper.TraceID, keys.SpanID, line)
		}
	})

	t.Run("each part on its own is a root", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			do   func()
			want []string
		}{
			{"the clock check", func() { mu.CheckClock(t.Context()) }, []string{"clock.check"}},
			{"the access sweep", func() { mu.Sweep(t.Context()) }, []string{"access.sweep(store.write.attempt(store.write.wait))"}},
			{"the keys file, already right", func() {
				if err := mu.WriteKeys(); err != nil {
					t.Fatal(err)
				}
			}, []string{"keys_file.write"}},
			{"the reread of the control store", func() { mu.refresh(t.Context()) }, []string{"control.reload", "keys_file.write"}},
		} {
			m.ResetSpans()
			tc.do()
			spans := m.Spans()
			if got := forest(spans); !slices.Equal(got, tc.want) {
				t.Errorf("%s: traces = %v, want %v", tc.name, got, tc.want)
			}
			for _, sp := range spans {
				if sp.Name == "keys_file.write" && sp.Attrs["changed"] != "false" {
					t.Errorf("%s: keys_file.write says changed=%s for a file that was already right", tc.name, sp.Attrs["changed"])
				}
			}
		}
	})
}

// telemetry: principals: decides what a span says of who (decision 32):
// a stand-in by default, the name, or nothing. The daemon's own log is
// not changed by it.
func TestSpanPrincipals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode               string
		principal, session *regexp.Regexp
	}{
		{"", principalAlias, sessionAlias},
		{telemetry.PrincipalsPseudonym, principalAlias, sessionAlias},
		{telemetry.PrincipalsName, regexp.MustCompile(`^alice$`), regexp.MustCompile(`^s-a$`)},
		{telemetry.PrincipalsOmit, nil, nil},
	} {
		t.Run(cmp.Or(tc.mode, "default"), func(t *testing.T) {
			tel, m := spanned(t, telemetry.Config{Principals: tc.mode})
			s, log := newServerLogged(t, tel)
			c := dialPipe(t, s, alice)
			mustAsk[proto.ListResult](c, proto.OpList, proto.ListArgs{})
			checked := 0
			for _, sp := range m.Spans() {
				p, hasP := sp.Attrs["principal"]
				sess, hasS := sp.Attrs["session"]
				if sp.Kind != "server" {
					if hasP || hasS {
						t.Errorf("span %s says who (%q, %q): only a request's and a handshake's span may", sp.Name, p, sess)
					}
					continue
				}
				checked++
				switch {
				case tc.principal == nil && (hasP || hasS):
					t.Errorf("span %s carries principal %q and session %q, want neither", sp.Name, p, sess)
				case tc.principal != nil && (!tc.principal.MatchString(p) || !tc.session.MatchString(sess)):
					t.Errorf("span %s carries principal %q and session %q, want them to match %s and %s", sp.Name, p, sess, tc.principal, tc.session)
				}
			}
			if checked != 2 {
				t.Errorf("%d server spans checked, want the handshake's and the request's", checked)
			}
			// The journal is the server's own record, and names names.
			if line := logLine(log, proto.OpList); !strings.Contains(line, " principal=alice ") || !strings.Contains(line, " session=s-a ") {
				t.Errorf("the request's log line does not name the principal and the session:\n%s", line)
			}
		})
	}
}
