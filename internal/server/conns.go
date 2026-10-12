package server

import (
	"fmt"
	"net"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// owner says whose a connection is, once its handshake is done: the
// principal, the project it connected to, and the fingerprint of the key
// that authenticated it when sshd took the key's line from the keys file
// (empty for a line of the hand-maintained authorized_keys file, which
// names none). A connection still in its handshake has no principal
// here: for it, since is when it was accepted, and hello whether its
// hello has been read.
type owner struct {
	principal string
	project   string
	key       string
	since     time.Time
	hello     bool
}

// oldestWaiting returns the connection accepted longest ago whose hello
// has not been read, or nil when there is none. The caller holds s.mu.
func (s *Server) oldestWaiting() net.Conn {
	var oldest net.Conn
	var since time.Time
	for c, o := range s.conns {
		if o.principal == "" && !o.hello && (oldest == nil || o.since.Before(since)) {
			oldest, since = c, o.since
		}
	}
	return oldest
}

// greeted records that c's hello has been read, so that it is no longer
// one to close when the server runs out of sockets.
func (s *Server) greeted(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.conns[c]; ok { // a test may hand a connection straight to the handler
		o.hello = true
		s.conns[c] = o
	}
}

// track records whose c is, so that it can be found when that access
// ends, and reports whether the access still holds. A server of many
// projects asks the control store again here: a membership that ended,
// or a key that was removed, while the handshake ran was not seen by
// drop, which looks only at connections already recorded, so one of the
// two always sees it.
func (s *Server) track(c net.Conn, sess *session) bool {
	s.mu.Lock()
	s.conns[c] = owner{principal: sess.actor.Principal, project: sess.proj.id, key: sess.fingerprint}
	s.mu.Unlock()
	if s.stores == nil {
		return true
	}
	if _, refused := s.admit(sess.actor.Principal, sess.proj.id); refused != nil {
		return false
	}
	return s.keyHolds(sess.actor.Principal, sess.fingerprint) == nil
}

// keyHolds refuses a connection that says the control store's key
// fingerprint let principal in when the store, now, holds no such key
// for an active principal of that name: the key was removed, or its
// principal disabled, and sshd read a keys file from before that; or
// the forced command was written by hand. It is what makes the keys
// file a cache: whatever a stale file lets past sshd, the store decides
// here. A connection that names no key came in by the hand-maintained
// authorized_keys file, and is not checked.
func (s *Server) keyHolds(principal, fingerprint string) *proto.Error {
	if fingerprint == "" {
		return nil
	}
	if p, _, ok := s.cfg.Control.AuthorizedKey(fingerprint); ok && p == principal {
		return nil
	}
	return proto.Errf(proto.CodeAuth,
		fmt.Sprintf("use another of your keys, or ask an admin to add this one again: `sfx admin key add %s FILE.pub`", principal),
		fmt.Sprintf("the key that signed in is no longer one the server holds for %s", principal))
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

// fileConns returns how many of principal's connections past the
// handshake named no key of the store: sshd let them in by a line of the
// hand-maintained authorized_keys file.
func (s *Server) fileConns(principal string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, o := range s.conns {
		if o.principal == principal && o.key == "" {
			n++
		}
	}
	return n
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
