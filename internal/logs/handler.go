package logs

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync/atomic"
)

// reqKey is the context key of a request's id.
type reqKey struct{}

// NewReq returns an id for a request: 16 hex digits, chosen at random.
// It only has to tell one request's lines from another's in a log, so
// it is not made with crypto/rand.
func NewReq() string {
	return fmt.Sprintf("%016x", rand.Uint64()) //nolint:gosec // a label for log lines, no secret
}

// WithReq returns ctx carrying req, a request's id: every line logged
// with that context through a [Handler] has it.
func WithReq(ctx context.Context, req string) context.Context {
	return context.WithValue(ctx, reqKey{}, req)
}

// Req returns the id of the request ctx belongs to, or "" outside one.
func Req(ctx context.Context) string {
	req, _ := ctx.Value(reqKey{}).(string)
	return req
}

// IDs reads the ids of the span a context carries, and reports false
// when it carries none.
type IDs func(ctx context.Context) (traceID, spanID string, ok bool)

// Handler is ariesd's log handler. It writes each record to the
// handler it wraps, which is the journal's, with what the record's
// context says of it: the request's id ([WithReq]) and the ids of its
// span. So a call site passes its context and names neither.
//
// The schema is flat: a group opened with WithGroup qualifies the keys
// that follow as GROUP.KEY and nests nothing.
//
// A record is also given to the handler set with [Handler.Tap], whole:
// with the attributes of With, of the call and of the context, in that
// order. Stopping the tap never stops the journal.
type Handler struct {
	next slog.Handler
	sh   *shared
	// attrs are those given to WithAttrs, qualified, for the tap; next
	// already has them.
	attrs []slog.Attr
	// prefix qualifies the keys of the attributes that follow: the open
	// groups, each followed by a dot.
	prefix string
}

// shared is what a Handler and those derived from it have in common.
type shared struct {
	ids IDs
	tap atomic.Pointer[slog.Handler]
}

// NewHandler returns a Handler over next. ids may be nil.
func NewHandler(next slog.Handler, ids IDs) *Handler {
	return &Handler{next: next, sh: &shared{ids: ids}}
}

// Tap has every record that is logged from now on given to h as well,
// through this Handler and every one derived from it; nil stops that. h
// must not block: it is called on the goroutine that logged.
func (h *Handler) Tap(tap slog.Handler) {
	if tap == nil {
		h.sh.tap.Store(nil)
		return
	}
	h.sh.tap.Store(&tap)
}

// Enabled reports whether the journal takes records at level.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

// Handle writes r, with the ids ctx carries.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if h.prefix != "" {
		q := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
		r.Attrs(func(a slog.Attr) bool {
			q.AddAttrs(h.qualified(a))
			return true
		})
		r = q
	}
	var buf [3]slog.Attr
	ids := buf[:0]
	if req := Req(ctx); req != "" {
		ids = append(ids, slog.String(KeyReq, req))
	}
	if h.sh.ids != nil {
		if traceID, spanID, ok := h.sh.ids(ctx); ok {
			ids = append(ids, slog.String(KeyTraceID, traceID), slog.String(KeySpanID, spanID))
		}
	}
	if len(ids) > 0 {
		// A copy of its own: the caller's record may share its
		// attributes with another handler's.
		r = r.Clone()
		r.AddAttrs(ids...)
	}
	err := h.next.Handle(ctx, r)
	if tap := h.sh.tap.Load(); tap != nil && (*tap).Enabled(ctx, r.Level) {
		whole := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
		whole.AddAttrs(h.attrs...)
		r.Attrs(func(a slog.Attr) bool {
			whole.AddAttrs(a)
			return true
		})
		_ = (*tap).Handle(ctx, whole) // the tap's failures are its own to count
	}
	return err
}

// WithAttrs returns a Handler whose records also have attrs.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	q := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		q[i] = h.qualified(a)
	}
	return &Handler{next: h.next.WithAttrs(q), sh: h.sh, attrs: append(slices.Clip(h.attrs), q...), prefix: h.prefix}
}

// WithGroup returns a Handler that qualifies the keys that follow with
// name.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &Handler{next: h.next, sh: h.sh, attrs: h.attrs, prefix: h.prefix + name + "."}
}

// qualified is a under the open groups.
func (h *Handler) qualified(a slog.Attr) slog.Attr {
	if h.prefix != "" {
		a.Key = h.prefix + a.Key
	}
	return a
}
