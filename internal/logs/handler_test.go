package logs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// line is one JSON log line, decoded, and order its keys as written.
type line struct {
	fields map[string]any
	order  []string
}

// lines decodes the JSON lines in b.
func lines(t *testing.T, b *bytes.Buffer) []line {
	t.Helper()
	var out []line
	for raw := range strings.SplitSeq(strings.TrimSpace(b.String()), "\n") {
		if raw == "" {
			continue
		}
		l := line{fields: map[string]any{}}
		if err := json.Unmarshal([]byte(raw), &l.fields); err != nil {
			t.Fatalf("log line %q is not JSON: %v", raw, err)
		}
		dec := json.NewDecoder(strings.NewReader(raw))
		_, _ = dec.Token()
		for dec.More() {
			k, _ := dec.Token()
			l.order = append(l.order, k.(string))
			var skip json.RawMessage
			_ = dec.Decode(&skip)
		}
		out = append(out, l)
	}
	return out
}

// fixedIDs is an IDs that finds a span on a context that holds spanKey.
type spanKey struct{}

func fixedIDs(ctx context.Context) (string, string, bool) {
	if ctx.Value(spanKey{}) == nil {
		return "", "", false
	}
	return "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7", true
}

func TestHandlerAddsContextIDs(t *testing.T) {
	span := context.WithValue(t.Context(), spanKey{}, true)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		ids  IDs
		want map[string]string // "" means the field is absent
	}{
		{"no request and no span", t.Context(), fixedIDs, map[string]string{KeyReq: "", KeyTraceID: "", KeySpanID: ""}},
		{"a request", WithReq(t.Context(), "00000000000000aa"), fixedIDs, map[string]string{KeyReq: "00000000000000aa", KeyTraceID: ""}},
		{"a span", span, fixedIDs,
			map[string]string{KeyReq: "", KeyTraceID: "4bf92f3577b34da6a3ce929d0e0e4736", KeySpanID: "00f067aa0ba902b7"}},
		{"a request under a span", WithReq(span, "00000000000000bb"), fixedIDs,
			map[string]string{KeyReq: "00000000000000bb", KeyTraceID: "4bf92f3577b34da6a3ce929d0e0e4736", KeySpanID: "00f067aa0ba902b7"}},
		{"no reader of spans", WithReq(span, "00000000000000cc"), nil, map[string]string{KeyReq: "00000000000000cc", KeyTraceID: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			log := slog.New(NewHandler(slog.NewJSONHandler(&b, nil), tc.ids)).With(KeyProject, "p1")
			log.InfoContext(tc.ctx, "served", KeyEvent, RequestDone, KeyOp, "show")
			ls := lines(t, &b)
			if len(ls) != 1 {
				t.Fatalf("%d lines logged, want 1:\n%s", len(ls), b.String())
			}
			got := ls[0].fields
			for k, want := range tc.want {
				if v, _ := got[k].(string); v != want {
					t.Errorf("field %s = %q, want %q; line: %v", k, v, want, got)
				}
			}
			for k, want := range map[string]string{KeyEvent: string(RequestDone), KeyOp: "show", KeyProject: "p1", "msg": "served"} {
				if got[k] != want {
					t.Errorf("field %s = %v, want %q", k, got[k], want)
				}
			}
		})
	}
}

// A line's own fields come before the ids the context adds, so that
// event is the first field after slog's own wherever it is logged.
func TestHandlerKeepsEventFirst(t *testing.T) {
	var b bytes.Buffer
	log := slog.New(NewHandler(slog.NewJSONHandler(&b, nil), fixedIDs))
	log.InfoContext(WithReq(context.WithValue(t.Context(), spanKey{}, true), "00000000000000aa"), "served", KeyEvent, RequestDone, KeyOp, "show")
	want := []string{"time", "level", "msg", KeyEvent, KeyOp, KeyReq, KeyTraceID, KeySpanID}
	if got := lines(t, &b)[0].order; !slices.Equal(got, want) {
		t.Errorf("fields in the order %v, want %v", got, want)
	}
}

// The schema is flat: a group qualifies keys and nests nothing.
func TestHandlerFlattensGroups(t *testing.T) {
	var b bytes.Buffer
	log := slog.New(NewHandler(slog.NewJSONHandler(&b, nil), nil)).With("a", 1).WithGroup("g").With("b", 2).WithGroup("h")
	log.InfoContext(WithReq(t.Context(), "00000000000000aa"), "m", "c", 3)
	got := lines(t, &b)[0].fields
	for k, want := range map[string]any{"a": 1.0, "g.b": 2.0, "g.h.c": 3.0, KeyReq: "00000000000000aa"} {
		if got[k] != want {
			t.Errorf("field %s = %v, want %v; line: %v", k, got[k], want, got)
		}
	}
}

func TestHandlerEnabledIsTheJournals(t *testing.T) {
	var b bytes.Buffer
	log := slog.New(NewHandler(slog.NewJSONHandler(&b, &slog.HandlerOptions{Level: slog.LevelWarn}), nil))
	log.Info("quiet", KeyEvent, ConnOpened)
	log.Warn("loud", KeyEvent, ConnAccept)
	if ls := lines(t, &b); len(ls) != 1 || ls[0].fields["msg"] != "loud" {
		t.Errorf("logged %v, want the warning alone", ls)
	}
}

// tapped records what a tap is given.
type tapped struct {
	slog.Handler
	got []map[string]string
}

func (h *tapped) Handle(_ context.Context, r slog.Record) error {
	m := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	h.got = append(h.got, m)
	return nil
}

// The tap is given each record whole, through every logger derived from
// the handler, whenever it was derived; and no record once it is
// removed.
func TestHandlerTap(t *testing.T) {
	var b bytes.Buffer
	h := NewHandler(slog.NewJSONHandler(&b, nil), fixedIDs)
	early := slog.New(h).With(KeyProject, "p1")
	early.Info("before the tap", KeyEvent, ConnOpened)

	tap := &tapped{Handler: slog.DiscardHandler}
	tap.Handler = slog.NewJSONHandler(&bytes.Buffer{}, nil) // an enabled one
	h.Tap(tap)
	ctx := WithReq(context.WithValue(t.Context(), spanKey{}, true), "00000000000000aa")
	early.With(KeyPrincipal, "alice").WarnContext(ctx, "refused", KeyEvent, RequestRefused, KeyCode, "invalid")
	h.Tap(nil)
	early.Info("after the tap", KeyEvent, ConnOpened)

	if len(tap.got) != 1 {
		t.Fatalf("the tap was given %d records, want the 1 logged while it was set: %v", len(tap.got), tap.got)
	}
	want := map[string]string{"msg": "refused", KeyProject: "p1", KeyPrincipal: "alice", KeyEvent: string(RequestRefused), KeyCode: "invalid",
		KeyReq: "00000000000000aa", KeyTraceID: "4bf92f3577b34da6a3ce929d0e0e4736", KeySpanID: "00f067aa0ba902b7"}
	for k, v := range want {
		if tap.got[0][k] != v {
			t.Errorf("the tap's record has %s = %q, want %q", k, tap.got[0][k], v)
		}
	}
	if len(tap.got[0]) != len(want) {
		t.Errorf("the tap's record is %v, want exactly %v", tap.got[0], want)
	}
	if n := len(lines(t, &b)); n != 3 {
		t.Errorf("the journal has %d lines, want all 3: the tap never replaces it", n)
	}
}

func TestNewReq(t *testing.T) {
	shape := regexp.MustCompile(`^[0-9a-f]{16}$`)
	seen := map[string]bool{}
	for range 1000 {
		r := NewReq()
		if !shape.MatchString(r) {
			t.Fatalf("NewReq() = %q, want 16 hex digits", r)
		}
		if seen[r] {
			t.Fatalf("NewReq() gave %q twice in 1000", r)
		}
		seen[r] = true
	}
	if got := Req(t.Context()); got != "" {
		t.Errorf("Req of a context with no request = %q, want none", got)
	}
}
