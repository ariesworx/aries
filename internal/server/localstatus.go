package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// `ariesd status` (design §16.4): the status report of `aries admin
// status`, for someone on the server with no admin key. It reaches the
// daemon as the upgrade's health probe does (probe.go): over the
// daemon's own socket, as the daemon's own user, under a reserved
// principal. What keeps it from every other local user is what guards
// the socket: its directory is mode 0700, and on Linux the daemon checks
// that the peer runs as its own user (CheckPeer). No SSH key reaches it,
// since Bridge refuses a reserved principal to every key.

// StatusPrincipal is the principal `ariesd status` connects as. It is
// reserved (store.ReservedPrincipals), as ProbePrincipal is. A
// connection under it is in no project, holds no connection slot, is
// not listed in who, and is answered one thing: the status report.
const StatusPrincipal = "ariesd-status"

// statusSession is the session a status connection is welcomed as.
const statusSession = "status"

// serveStatus answers the status requests of a connection welcomed as
// StatusPrincipal, until it closes, sends anything else, or says nothing
// for helloTimeout. Any other connection in no project, which is the
// upgrade's probe, is answered nothing more.
func (s *Server) serveStatus(ctx context.Context, sess *session) {
	if sess.actor.Principal != StatusPrincipal {
		return
	}
	for {
		_ = sess.conn.SetReadDeadline(time.Now().Add(helloTimeout))
		f, err := sess.dec.Decode()
		if err != nil {
			return
		}
		if f.T != proto.FrameReq || f.Op != proto.OpStatus {
			_ = s.send(sess, f.ID, nil, proto.Errf(proto.CodeInvalid, "run `ariesd status`, which asks for nothing else",
				fmt.Sprintf("this connection answers only %q requests", proto.OpStatus)))
			return
		}
		rctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		var res any
		args, perr := decodeArgs[proto.StatusArgs](f.Args)
		if perr == nil {
			res, perr = s.localStatus(rctx, args)
		}
		cancel()
		if err := s.send(sess, f.ID, res, perr); err != nil {
			return
		}
	}
}

// localStatus is the status report for the daemon's own user: the
// server and every project, as a server admin sees them. A server of one
// project reports itself and that project, which has no name there and
// is listed by its id.
func (s *Server) localStatus(ctx context.Context, in proto.StatusArgs) (any, *proto.Error) {
	if s.cfg.Control != nil {
		return s.report(ctx, s.cfg.Control.KnownProjects(), true, in, time.Time{})
	}
	one := store.Project{ID: s.single.id, Name: s.single.id, State: store.ProjectActive}
	return s.report(ctx, []store.Project{one}, true, in, time.Time{})
}

// Status connects to the daemon's socket as the daemon's own user, as
// Probe does, and returns the status report a server admin gets from
// `aries admin status`, with every project: it asks page after page. It
// gives up after 30 seconds.
func Status(ctx context.Context, socket, clientVersion string) (proto.StatusResult, error) {
	var out proto.StatusResult
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return out, fmt.Errorf("connect to %s: %w; fix: run it on the server as the user ariesd runs as, and start the daemon if it is stopped (`systemctl start ariesd`)", socket, err)
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	enc, dec := proto.NewEncoder(c), proto.NewDecoder(c)
	// A daemon that refuses the peer closes the connection unanswered,
	// which may fail either write here or the read of the welcome. The
	// first failure is the one reported: joined, two failed writes made
	// an error of two lines, with the fix on the second.
	err = enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: StatusPrincipal})
	if err == nil {
		err = enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Version: clientVersion, Session: statusSession, Machine: "localhost"})
	}
	var w *proto.Frame
	if err == nil {
		w, err = dec.Decode()
	}
	if err != nil {
		return out, fmt.Errorf("handshake: %w; fix: run it as the user ariesd runs as, which is the only one the daemon answers on its socket", err)
	}
	if w.T != proto.FrameWelcome {
		return out, fmt.Errorf("handshake: got a %q frame, not a welcome", w.T)
	}
	if w.Err != nil {
		return out, errors.Join(errors.New("handshake refused"), w.Err)
	}
	args := proto.StatusArgs{AdminListArgs: proto.AdminListArgs{Limit: proto.AdminListMax}}
	for id := uint64(1); ; id++ {
		req, err := proto.Request(id, proto.OpStatus, args)
		if err != nil {
			return out, err
		}
		// The daemon bounds the report's reads of the database
		// (statusReadTimeout), so one that welcomes and then does not
		// answer is itself stuck, or went away.
		const noAnswer = "status: the daemon did not answer: %w; fix: see ariesd's log (`journalctl -u ariesd`) for what it waits on, and restart it if that does not end (`systemctl restart ariesd`)"
		if err := enc.Encode(req); err != nil {
			return out, fmt.Errorf(noAnswer, err)
		}
		f, err := dec.Decode()
		if err != nil {
			return out, fmt.Errorf(noAnswer, err)
		}
		if f.Err != nil {
			return out, f.Err
		}
		var page proto.StatusResult
		if err := json.Unmarshal(f.OK, &page); err != nil {
			return out, fmt.Errorf("status: %w", err)
		}
		if out.Server == nil {
			out.Server = page.Server
		}
		out.Projects = append(out.Projects, page.Projects...)
		if page.More == 0 || len(page.Projects) == 0 {
			return out, nil
		}
		args.After = page.Projects[len(page.Projects)-1].Name
	}
}
