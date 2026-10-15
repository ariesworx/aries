package server

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// What the daemon measures of itself (design §16.4), and whether it is
// ready (/readyz). Every measurement goes through Config.Telemetry,
// which is nil-safe: with none, each call here returns at once.
//
// No attribute's value is one a client chose. An op is recorded only
// through the Telemetry, which knows the ops of [Ops] and records any
// other as unknown; a project is one a connection was admitted to; a
// result is a protocol code; and the principal, session, machine and
// issue of a request are never attributes of a metric.
//
// Spans (design §16.4) are started here and in the files that do the
// work, always through the Telemetry, which decides what a span may
// carry (telemetry/trace.go has the rule): ids, codes and small
// numbers. A span is never given a request's arguments, a result, or an
// error. It may say who (Span.SetWho), as telemetry: principals: allows.

// Ops returns every operation the server answers, sorted: the values
// the op attribute of a metric may take (telemetry.Config.Ops).
func Ops() []string {
	ops := slices.Collect(maps.Keys(handlers))
	ops = slices.AppendSeq(ops, maps.Keys(adminHandlers))
	slices.Sort(ops)
	return ops
}

// resultOf is the result attribute of a request or a control write: ok,
// or the refusal's code.
func resultOf(perr *proto.Error) string {
	if perr == nil {
		return telemetry.ResultOK
	}
	return string(perr.Code)
}

// classOf is a request's result class: ok; error when the server could
// not serve it, which is its code unavailable; and refused for every
// other refusal, which is the request's doing and not the server's.
func classOf(perr *proto.Error) string {
	switch {
	case perr == nil:
		return telemetry.ClassOK
	case perr.Code == proto.CodeUnavailable:
		return telemetry.ClassError
	}
	return telemetry.ClassRefused
}

// observeRequest records one request on sess, answered with perr, that
// began at start.
func (s *Server) observeRequest(sess *session, op string, perr *proto.Error, start time.Time) {
	tel := s.cfg.Telemetry
	if !tel.Enabled() {
		return
	}
	res := resultOf(perr)
	tel.Since(telemetry.RequestDuration, start, op, res)
	tel.Count(telemetry.ProjectRequests, sess.proj.id, classOf(perr))
	if controlWrites[op] {
		tel.Count(telemetry.ControlWrites, op, res)
	}
}

// endRequest ends the server span of a request on sess that was
// answered with perr. Only a request the server could not serve is
// marked failed: a refusal is the request's doing, and its code is the
// span's result.
func (s *Server) endRequest(span telemetry.Span, sess *session, perr *proto.Error) {
	if span.Recording() {
		// Ids and a code, and never the request's arguments, its result
		// or the refusal's message (telemetry/trace.go, THE RULE).
		res := resultOf(perr)
		span.Set(telemetry.KeyResult, res)
		span.Set(telemetry.KeyProject, sess.proj.id)
		span.SetInt(telemetry.KeyProtocol, sess.proto)
		span.SetWho(sess.actor.Principal, sess.actor.Session)
		if classOf(perr) == telemetry.ClassError {
			span.Fail(res)
		}
	}
	span.End()
}

// observeHandshake counts a handshake that returned sess and err, by
// how it ended: ok, the refusal's code, or failed when it ended with no
// protocol error to name, and says the same on its span, which the
// caller ends. A stale key lookup, closed unanswered, is no handshake:
// it is not counted, and its span says nothing of how it ended.
func (s *Server) observeHandshake(span telemetry.Span, sess *session, err error) {
	if sess == nil && err == nil {
		return
	}
	res := telemetry.ResultOK
	if err != nil {
		res = telemetry.ResultFailed
		if perr, ok := errors.AsType[*proto.Error](err); ok {
			res = string(perr.Code)
		}
	}
	s.cfg.Telemetry.Count(telemetry.Handshakes, res)
	if !span.Recording() {
		return
	}
	span.Set(telemetry.KeyResult, res)
	if res == telemetry.ResultFailed || res == string(proto.CodeUnavailable) {
		span.Fail(res)
	}
	if sess == nil {
		return
	}
	// The principal is the one sshd authenticated; the session and the
	// protocol are set only once the hello was read and checked.
	if sess.proj != nil {
		span.Set(telemetry.KeyProject, sess.proj.id)
	}
	if sess.proto > 0 {
		span.SetInt(telemetry.KeyProtocol, sess.proto)
	}
	span.SetWho(sess.actor.Principal, sess.actor.Session)
}

// dropReason is the reason attribute for connections drop closed
// because of why, one of the lost* reasons or a removed key.
func dropReason(why string) string {
	switch why {
	case lostMembership:
		return telemetry.DropMembership
	case lostDisabled:
		return telemetry.DropDisabled
	case lostArchived:
		return telemetry.DropArchived
	case keyRemoved:
		return telemetry.DropKey
	}
	return telemetry.Other
}

// keyRemoved is why drop closes the connections a removed key let in.
const keyRemoved = "key removed"

// observeLimit counts the refusals mapErr words for a store's cap that
// has an error type of its own. A request past any other of the store's
// limits is refused as plain invalid, which the store does not tell
// apart from any other invalid request, and is not counted here.
func (pr *served) observeLimit(op string, err error) {
	if !pr.tel.Enabled() {
		return
	}
	limit := ""
	if e, ok := errors.AsType[*store.ControlLimitError](err); ok {
		limit = e.Limit
	} else if _, ok := errors.AsType[*store.PriceLimitError](err); ok {
		limit = telemetry.LimitPrices
	} else if _, ok := errors.AsType[*store.PlanLimitError](err); ok {
		limit = telemetry.LimitPlans
	} else if _, ok := errors.AsType[*store.HoursLimitError](err); ok {
		limit = telemetry.LimitHoursPerDay
	} else if e, ok := errors.AsType[*store.LockLimitError](err); ok {
		limit = e.Limit
	} else if errors.Is(err, store.ErrBusy) && op == proto.OpUsage {
		limit = telemetry.LimitUsagePerDay
	}
	if limit != "" {
		pr.tel.Count(telemetry.LimitRefusals, limit)
	}
}

// storeObserver is a project store's [store.Observer]: it records the
// store's writes, retries, Dolt commits and changes of a claim.
type storeObserver struct {
	tel     *telemetry.Telemetry
	project string
}

// StoreObserver returns the observer to open project's store with
// (store.Options.Observer), so that its writes, retries, Dolt commits
// and claims are measured by tel. A nil tel returns nil: the store then
// calls nothing.
func StoreObserver(tel *telemetry.Telemetry, project string) store.Observer {
	if !tel.Enabled() {
		return nil
	}
	return storeObserver{tel: tel, project: project}
}

func (o storeObserver) Wrote(took time.Duration) {
	o.tel.Record(telemetry.StoreWriteDuration, took.Seconds(), o.project)
}

func (o storeObserver) Retried() { o.tel.Count(telemetry.StoreWriteRetries, o.project) }

func (o storeObserver) Committed(took time.Duration, _ error) {
	o.tel.Record(telemetry.StoreCommitDuration, took.Seconds())
}

func (o storeObserver) ClaimChanged(change store.ClaimChange, reason string) {
	o.tel.Count(telemetry.Claims, string(change), claimReason(reason))
}

func (o storeObserver) LockChanged(change store.LockChange, waited time.Duration) {
	o.tel.Count(telemetry.Locks, string(change))
	if change == store.LockGranted {
		o.tel.Record(telemetry.LockWaitDuration, waited.Seconds())
	}
}

// storeTracer is a store's [store.Tracer]: a span for each attempt of a
// write, for its wait for the writer, for each Dolt commit and for the
// migrations of an open, under the span of whatever asked for the work.
type storeTracer struct {
	tel     *telemetry.Telemetry
	project string
}

// StoreTracer returns the tracer to open project's store with
// (store.Options.Tracer), so that where its writes spend their time
// shows in tel's traces. project is empty for the control store. A tel
// that traces nothing returns nil: the store then calls nothing.
func StoreTracer(tel *telemetry.Telemetry, project string) store.Tracer {
	if !tel.Tracing() {
		return nil
	}
	return storeTracer{tel: tel, project: project}
}

// stageSpans names the span of each stage.
var stageSpans = map[store.Stage]telemetry.SpanName{
	store.StageAttempt: telemetry.SpanWriteAttempt,
	store.StageWait:    telemetry.SpanWriteWait,
	store.StageCommit:  telemetry.SpanStoreCommit,
	store.StageMigrate: telemetry.SpanStoreMigrate,
}

func (o storeTracer) Begin(ctx context.Context, stage store.Stage, attempt int) (context.Context, func(store.Outcome)) {
	ctx, span := o.tel.Start(ctx, stageSpans[stage])
	if !span.Recording() {
		// The context still carries the decision not to sample, for the
		// stages begun inside this one.
		return ctx, unsampled
	}
	if o.project != "" {
		span.Set(telemetry.KeyProject, o.project)
	}
	if stage == store.StageAttempt {
		span.SetInt(telemetry.KeyAttempt, attempt)
	}
	return ctx, func(out store.Outcome) {
		span.Set(telemetry.KeyOutcome, string(out))
		// An attempt that ends in an error is most often the request's
		// own refusal, which its span's result names; any other stage
		// that does is the server's failure.
		if out == store.OutcomeError && stage != store.StageAttempt {
			span.Fail(string(out))
		}
		span.End()
	}
}

// unsampled ends a stage whose span records nothing.
func unsampled(store.Outcome) {}

// claimReason is the reason attribute for a claim released for reason,
// which is empty or one of the lost* reasons the server gives EndClaims.
func claimReason(reason string) string {
	switch reason {
	case "":
		return telemetry.ReasonNone
	case lostMembership:
		return telemetry.ReasonMembership
	case lostDisabled:
		return telemetry.ReasonDisabled
	case lostArchived:
		return telemetry.ReasonArchived
	}
	return telemetry.Other
}

// How /readyz's check of Dolt is paced: one cheap query, bounded by
// doltCheckTimeout, whose answer stands for doltCheckEvery. However
// often /readyz is asked, Dolt is asked at most that often.
const (
	doltCheckEvery   = 5 * time.Second
	doltCheckTimeout = 2 * time.Second
)

// doltCheck is the last check of Dolt and when it was made.
type doltCheck struct {
	// mu is held while a check runs, so that requests arriving together
	// share one.
	mu  sync.Mutex
	at  time.Time
	err error
	// now is the clock the checks are paced by; nil is time.Now. It is
	// the machine's, not Server.now, which tests move by hours.
	now func() time.Time
}

// Ready says why the daemon is not ready to serve, in one line, or
// returns "" when it is: it is listening on its socket, its last check
// of Dolt passed, and, when it writes a keys file, the last write did
// not fail. It is what /readyz answers from.
//
// The check of Dolt is a ping on a pooled connection to the control
// database, or to the one project's, made at most every doltCheckEvery
// and bounded by doltCheckTimeout; between checks the last answer
// stands, so a caller may ask as often as it likes.
func (s *Server) Ready(ctx context.Context) string {
	if !s.listening.Load() {
		return "ariesd is not listening on its socket"
	}
	if err := s.checkDolt(ctx); err != nil {
		return "the last check of Dolt failed"
	}
	if k := s.keysStatus(); k != nil && k.Failed {
		return "the last write of the keys file failed"
	}
	return ""
}

// checkDolt returns the last check's error, checking again if it is
// older than doltCheckEvery.
func (s *Server) checkDolt(ctx context.Context) error {
	d := &s.dolt
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now
	if d.now != nil {
		now = d.now
	}
	if !d.at.IsZero() && now().Sub(d.at) < doltCheckEvery {
		return d.err
	}
	// The answer stands for every caller for a while, so the check does
	// not end with the request that happened to start it.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), doltCheckTimeout)
	defer cancel()
	if s.cfg.Control != nil {
		d.err = s.cfg.Control.Ping(cctx)
	} else {
		d.err = s.cfg.Store.Ping(cctx)
	}
	d.at = now()
	if d.err != nil {
		s.refusals.line(ctx, s.cfg.Logger, "", s.now(), slog.LevelError, logs.DoltCheckFailed, "the check of Dolt failed; /readyz answers not ready", "err", d.err)
	}
	return d.err
}

// tracesStatus is how people appear in the server's traces, for the
// status report; nil for a server that exports none.
func (s *Server) tracesStatus() *proto.TracesStatus {
	tel := s.cfg.Telemetry
	if !tel.Tracing() {
		return nil
	}
	p := tel.People()
	if p.Mode == telemetry.PrincipalsPseudonym && !p.StandIns {
		return &proto.TracesStatus{People: "unavailable", Cause: p.Cause, Damaged: p.Damaged, KeyFile: p.KeyFile}
	}
	return &proto.TracesStatus{People: p.Mode}
}
