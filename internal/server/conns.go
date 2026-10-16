package server

import (
	"fmt"
	"net"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
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
	// transport is how the connection came (TransportHTTPS or
	// TransportSSH), once its handshake is done.
	transport string
	// grant and identity are the sign-in the connection presented, and
	// the identity it was made by, for a connection a sign-in let in;
	// sess is its session, which a sign-in's end closes with a last
	// frame.
	grant    string
	identity int64
	sess     *session
}

// connsBy counts the connections past their handshake that came by each
// transport.
func (s *Server) connsBy() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, o := range s.conns {
		if o.principal != "" {
			out[o.transport]++
		}
	}
	return out
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
	o := owner{principal: sess.actor.Principal, project: sess.proj.id, key: sess.fingerprint, transport: sess.transport, sess: sess}
	if in := sess.signedIn; in != nil {
		o.grant, o.identity = in.grant, in.identity
	}
	s.conns[c] = o
	s.mu.Unlock()
	s.cfg.Telemetry.Record(telemetry.Connections, 1, sess.proj.id)
	if s.stores == nil {
		return true
	}
	if _, refused := s.admit(sess.actor.Principal, sess.proj.id); refused != nil {
		return false
	}
	if in := sess.signedIn; in != nil {
		// A sign-in ended while the handshake ran is not seen by
		// endGrant, which looks only at connections already recorded.
		g, ok := s.cfg.Control.TokenGrant(in.hash)
		return ok && g.ID == in.grant && g.Live(s.cfg.Control.Now())
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
	// A key the operator's authorized_keys file gives to another
	// principal is not the store's to let in, whatever an older keys
	// file says: the next one written has no line for it (keyLines).
	owner, byHand := s.handOwner(fingerprint)
	if p, _, ok := s.cfg.Control.AuthorizedKey(fingerprint); ok && p == principal && (!byHand || owner == principal) {
		return nil
	}
	return proto.Errf(proto.CodeAuth,
		fmt.Sprintf("use another of your keys, or ask an admin to add this one again: `aries admin key add %s FILE.pub`", principal),
		fmt.Sprintf("the key that signed in is no longer one the server holds for %s", principal))
}

// untrack forgets c when its handler returns.
func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	o := s.conns[c]
	delete(s.conns, c)
	s.mu.Unlock()
	if o.principal != "" { // track counted it
		s.cfg.Telemetry.Record(telemetry.Connections, -1, o.project)
	}
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
		dropped := n
		if from.ended {
			dropped++ // the admin's own, closed once it is answered
		}
		s.cfg.Telemetry.Record(telemetry.ConnectionsDropped, float64(dropped), dropReason(why))
		s.cfg.Logger.Info("connections dropped", "event", logs.ConnDropped, "reason", why, "by", from.actor.Principal, "connections", n, "own", from.ended)
	}
	return len(closing)
}

// closeSessions ends the connections past their handshake that match
// selects, each with e as its last frame, and returns how many.
func (s *Server) closeSessions(match func(owner) bool, e *proto.Error) int {
	s.mu.Lock()
	var sessions []*session
	for _, o := range s.conns {
		if o.principal != "" && o.sess != nil && match(o) {
			sessions = append(sessions, o.sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		s.endSession(sess, e)
	}
	if n := len(sessions); n > 0 {
		s.cfg.Telemetry.Record(telemetry.ConnectionsDropped, float64(n), telemetry.DropSignIn)
	}
	return len(sessions)
}

// endSession closes sess's connection with e as its last frame: an
// answer to no request, which aries takes for why the connection ended.
// The frame waits at most a second for a client that reads nothing.
func (s *Server) endSession(sess *session, e *proto.Error) {
	_ = sess.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = s.send(sess, 0, nil, e)
	_ = sess.conn.Close()
}

// fileConns returns how many of principal's connections past the
// handshake named no key of the store: sshd let them in by a line of the
// hand-maintained authorized_keys file.
func (s *Server) fileConns(principal string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, o := range s.conns {
		if o.principal == principal && o.key == "" && o.grant == "" {
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
