package server

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/telemetry"
)

// What the daemon measures of itself (design §16.4), and whether it is
// ready (/readyz). Every measurement goes through Config.Telemetry,
// which is nil-safe: with none, each call here returns at once.
//
// No attribute's value is one a client chose. An op is recorded only
// through the Telemetry, which knows the ops of [Ops] and records any
// other as unknown; a project is one a connection was admitted to; a
// result is a protocol code; and the principal, session, machine and
// issue of a request are never attributes.

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

// observeHandshake counts a handshake that returned sess and err, by
// how it ended: ok, the refusal's code, or failed when it ended with no
// protocol error to name. A stale key lookup, closed unanswered, is no
// handshake and is not counted.
func (s *Server) observeHandshake(sess *session, err error) {
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
		return "starfixd is not listening on its socket"
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
		s.refusals.log(s.cfg.Logger, "", s.now(), slog.LevelError, "the check of Dolt failed; /readyz answers not ready", "err", d.err)
	}
	return d.err
}
