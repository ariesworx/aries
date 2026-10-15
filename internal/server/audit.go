package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/store"
)

// The audit stream (design §16.4, decision 30): each event of the
// control store's log that changes who has access to what is also
// written as a log line with audit=true, for a SIEM that reads the
// journal. The event log is the record, and the stream its copy: the
// lines are read from the log, after the write that made them has
// committed, so a line says what an event says and nothing else, and
// there is a line for a change whoever made it, `starfixd control init`
// run beside the daemon included.
//
// The daemon keeps only how far it has read, in memory. It starts at the
// newest event there is when it starts, so a restart repeats nothing;
// what was written while no daemon ran is in the event log alone.

// auditPage is how many events one read of the log takes, and
// auditPages how many reads one look makes: what is left waits for the
// next look, a tick later at most (rule 18).
const (
	auditPage  = 1000
	auditPages = 10
)

// auditor is how far the audit stream has read the control store's log.
type auditor struct {
	mu sync.Mutex
	// seq is the newest event read; started says it is set.
	seq     int64
	started bool
}

// auditStart starts the audit stream after the newest event of the
// control store's log. A read that fails is logged, and the next look
// starts the stream instead.
func (s *Server) auditStart(ctx context.Context) {
	if s.cfg.Control == nil || s.cfg.NoAudit {
		return
	}
	a := &s.audited
	a.mu.Lock()
	defer a.mu.Unlock()
	a.started = false
	s.auditStartLocked(ctx)
}

// auditStartLocked is auditStart with s.audited.mu held.
func (s *Server) auditStartLocked(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	seq, err := s.cfg.Control.LastEventSeq(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.cfg.Logger.Error("read the control store's log for the audit stream; the lines follow when a read succeeds",
				"event", logs.AuditReadFailed, "err", err)
		}
		return
	}
	s.audited.seq, s.audited.started = seq, true
}

// Audit writes a line of the audit stream for each event of the control
// store's log since the last look, as the server does once a control
// write has been answered for and at each tick of the reaper, which is
// when a change made beside the daemon reaches the stream. A server of
// one project, and one with Config.NoAudit, writes none. The lines are
// no request's: they carry the event's own seq, not the id of whichever
// request happened to read the log, so ctx gives them nothing but its
// deadline.
func (s *Server) Audit(ctx context.Context) {
	if s.cfg.Control == nil || s.cfg.NoAudit {
		return
	}
	a := &s.audited
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started {
		s.auditStartLocked(ctx)
		return
	}
	// The write has committed, so its line is owed even when the
	// request that made it has gone away.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RequestTimeout)
	defer cancel()
	for range auditPages {
		evs, err := s.cfg.Control.Events(ctx, a.seq, auditPage)
		if err != nil {
			s.cfg.Logger.Error("read the control store's log for the audit stream; the lines follow when a read succeeds",
				"event", logs.AuditReadFailed, "err", err)
			return
		}
		for _, e := range evs {
			s.auditLine(e)
			a.seq = e.Seq
		}
		if len(evs) < auditPage {
			return
		}
	}
}

// auditLine writes e as a line of the audit stream, if it changes
// anyone's access: who did it, the op, and the target as the event log
// names it, with the target's parts in fields of their own. An event
// with no actor is not an audit record (design §16.4). A key's event
// holds the key's text, and none of that is written: fingerprints,
// never keys.
func (s *Server) auditLine(e store.Event) {
	ev, ok := logs.Audit(string(e.Op))
	if !ok || e.Actor.Principal == "" {
		return
	}
	attrs := []any{logs.KeyAudit, true, logs.KeySeq, e.Seq, logs.KeyAt, e.At, logs.KeyPrincipal, e.Actor.Principal,
		logs.KeySession, e.Actor.Session, logs.KeyOp, string(e.Op), logs.KeyTarget, e.Target}
	// The state the event left, or the one it removed.
	var state struct {
		Principal string `json:"principal"`
		Role      string `json:"role"`
	}
	raw := e.After
	if len(raw) == 0 || string(raw) == "null" {
		raw = e.Before
	}
	_ = json.Unmarshal(raw, &state) // a target that does not decode is still named by the event's own
	switch e.Op {
	case store.OpProjectAdd, store.OpProjectActivate, store.OpProjectArchive:
		attrs = append(attrs, logs.KeyProject, e.Target)
	case store.OpPrincipalAdd, store.OpPrincipalDisable, store.OpPrincipalEnable:
		attrs = append(attrs, logs.KeySubject, e.Target)
	case store.OpKeyAdd, store.OpKeyRemove:
		attrs = append(attrs, logs.KeyFingerprint, e.Target)
		if state.Principal != "" {
			attrs = append(attrs, logs.KeySubject, state.Principal)
		}
	case store.OpMemberSet, store.OpMemberRemove:
		if project, principal, ok := strings.Cut(e.Target, "/"); ok {
			attrs = append(attrs, logs.KeyProject, project, logs.KeySubject, principal)
		}
		if e.Op == store.OpMemberSet && state.Role != "" {
			attrs = append(attrs, logs.KeyRole, state.Role)
		}
	}
	s.cfg.Logger.With("event", ev).Info("audit", attrs...)
}
