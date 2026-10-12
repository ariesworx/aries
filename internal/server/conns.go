package server

import (
	"net"

	"github.com/ariesworx/starfix/internal/store"
)

// owner says whose a connection is, once its handshake is done: the
// principal, and the project it connected to. The zero value is a
// connection still in its handshake.
type owner struct {
	principal string
	project   string
}

// track records whose c is, so that it can be found when that access
// ends, and reports whether the access still holds. A server of many
// projects asks the control store again here: a membership that ended
// while the handshake ran was not seen by drop, which looks only at
// connections already recorded, so one of the two always sees it.
func (s *Server) track(c net.Conn, sess *session) bool {
	s.mu.Lock()
	s.conns[c] = owner{principal: sess.actor.Principal, project: sess.proj.id}
	s.mu.Unlock()
	if s.stores == nil {
		return true
	}
	_, refused := s.admit(sess.actor.Principal, sess.proj.id)
	return refused == nil
}

// untrack forgets c when its handler returns.
func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// drop closes the connections whose owner match selects, because the
// access that let them in has ended (design §15.2), and returns how
// many. The connection of from, the session whose request ended that
// access, is not closed here: it is marked to close once the reply to
// that request is sent, so the admin still learns what happened.
//
// The caller has committed the change, so the control store already
// refuses the next handshake.
func (s *Server) drop(from *session, why string, match func(owner) bool) int {
	s.mu.Lock()
	var closing []net.Conn
	for c, o := range s.conns {
		if o.principal == "" || !match(o) {
			continue
		}
		if c == from.conn {
			from.ended = true
			continue
		}
		closing = append(closing, c)
	}
	s.mu.Unlock()
	// Closing a connection ends its handler, which frees what it held.
	for _, c := range closing {
		_ = c.Close()
	}
	if n := len(closing); n > 0 || from.ended {
		s.cfg.Logger.Info("connections dropped", "reason", why, "by", from.actor.Principal, "connections", n, "own", from.ended)
	}
	return len(closing)
}

// unopenedProjects returns the projects OpenProjects could not open and
// nothing has opened since, in no order.
func (s *Server) unopenedProjects() []store.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]store.Project, 0, len(s.unopened))
	for _, p := range s.unopened {
		out = append(out, p)
	}
	return out
}
