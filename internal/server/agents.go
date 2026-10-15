package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// The note of a session in its project's registry, which `aries who`
// reads, is a write, and so waits on the database. A connection makes it beside
// the wait for its first request, not before it, and each request waits
// for it (presence.ensure): so that none reads or writes a store for a
// session the registry has not been told of, and none waits for the
// note longer than it may wait at all. That is the request timeout for
// every op but `status`, which is what an admin asks when the database
// is in trouble and waits on the database for statusReadTimeout, the note
// included.

// presence is the note of one connection's session. The connection's
// handler starts it (Server.present) and ends it before returning
// (end), so the goroutine that makes the first note never outlives the
// connection, or the handler's hold on the project's store.
type presence struct {
	pr      *served
	log     *slog.Logger
	actor   store.Actor
	harness string
	// done is closed when the first note has ended; stop gives it up.
	done chan struct{}
	stop context.CancelFunc
	// noted is whether the session is in the registry. The first note
	// sets it before done is closed; after that it is the request
	// loop's.
	noted bool
}

// present starts noting sess's session in its project's registry and
// returns at once. ctx is the handshake's, so the first note's write is
// in the handshake's trace. Nothing is noted for the upgrade's probe: it
// is a health check, not an agent to list in who.
func (s *Server) present(ctx context.Context, log *slog.Logger, sess *session) *presence {
	p := &presence{pr: sess.proj, log: log, actor: sess.actor, harness: sess.harness, done: make(chan struct{}), stop: func() {}}
	if sess.actor.Principal == ProbePrincipal {
		p.noted = true
		close(p.done)
		return p
	}
	if !store.ValidHarness(p.harness) {
		log.InfoContext(ctx, "ignored harness", "event", logs.AgentHarness, "harness", fmt.Sprintf("%.40q", p.harness))
		p.harness = ""
	}
	ctx, p.stop = context.WithCancel(ctx)
	go func() {
		defer close(p.done)
		p.noted = p.pr.touch(ctx, p.log, p.actor, p.harness)
	}()
	return p
}

// ensure is called by the request loop before a request that reads or
// writes a store. It waits for the first note for as long as the
// request has, and if that note failed makes it again, within the
// request's time: a session is noted by the first of its requests that
// the database answers. The request goes on whether or not the note
// was made.
func (p *presence) ensure(ctx context.Context) {
	select {
	case <-p.done:
	case <-ctx.Done():
		return
	}
	if !p.noted {
		p.noted = p.pr.touch(ctx, p.log, p.actor, p.harness)
	}
}

// noteGrace is how long a connection that has ended waits for the note
// of its session to finish before giving it up. A connection that
// asked for anything has waited for the note already, so this is for
// one that said hello and left: it is listed by who when the database answers,
// as it was when the note came before the first request, and holds its
// place and its project's store no longer than the status report waits
// on the database when the database does not.
const noteGrace = statusReadTimeout

// end waits for the first note to finish, giving it up after noteGrace.
// The first note has ended when it returns.
func (p *presence) end() {
	t := time.NewTimer(noteGrace)
	defer t.Stop()
	select {
	case <-p.done:
	case <-t.C:
	}
	p.stop()
	<-p.done
}

// touch records that a's session is present, within pr.timeout, and
// reports whether it did. harness is one the store accepts
// (store.ValidHarness). Presence is best effort: a failure is logged and
// never fails the connection or the request that caused it.
func (pr *served) touch(ctx context.Context, log *slog.Logger, a store.Actor, harness string) bool {
	tctx, cancel := context.WithTimeout(ctx, pr.timeout)
	defer cancel()
	err := pr.store.TouchAgent(tctx, a, harness)
	if err != nil && ctx.Err() == nil {
		log.ErrorContext(ctx, "register agent", "event", logs.AgentRegister, "err", err)
	}
	return err == nil
}

func who(ctx context.Context, pr *served, _ store.Actor, in proto.WhoArgs) (any, *proto.Error) {
	var since time.Duration
	if in.Since != "" {
		d, err := proto.ParseDuration(in.Since)
		if err != nil || d > store.MaxAgentWindow {
			return nil, proto.Errf(proto.CodeInvalid, "give a window up to 7d, such as 15m, 2h or 1d",
				fmt.Sprintf("since %q is not a duration up to 7d", in.Since))
		}
		since = d
	}
	now := pr.store.Now()
	agents, more, err := pr.store.WhoPage(ctx, since, in.Limit)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpWho, "", 0, err)
	}
	out := proto.WhoResult{Now: now, Agents: []proto.Agent{}, More: more}
	for _, a := range agents {
		w := proto.Agent{Principal: a.Principal, Session: a.Session, Machine: a.Machine, Harness: a.Harness,
			Started: a.Started, LastSeen: a.LastSeen}
		for _, id := range a.Claims {
			w.Claims = append(w.Claims, string(id))
		}
		out.Agents = append(out.Agents, w)
	}
	return out, nil
}
