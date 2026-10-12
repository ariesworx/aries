package server

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// `sfx admin status` (design §15.4): what an admin watches a server of
// many projects by. Everything in it is what the daemon holds in memory,
// apart from one count on an index for each open store listed (the
// sessions seen lately). It opens no store, reads no control table and
// writes nothing.
//
// Of Dolt, which is another process under another user, it says what
// the daemon last read from Dolt's own metrics endpoint, when it is
// told where that is (doltwatch.go): the report itself reads nothing.
// It does not say a database's size on disk. Dolt gives its
// least-privileged account no table or function for that
// (information_schema.tables estimates the current rows and leaves out
// the history, which is most of a Dolt database), and the files are
// Dolt's user's to read.

// status reports the server and every project to a server admin, and to
// a project admin the active projects they administer. Anyone else is
// refused.
func status(ctx context.Context, s *Server, sess *session, in proto.StatusArgs) (any, *proto.Error) {
	ctl, actor := s.cfg.Control, sess.actor.Principal
	admin := ctl.IsServerAdmin(actor)
	var rows []store.Project
	for _, p := range ctl.KnownProjects() {
		// A project admin sees what `project list` shows them, the
		// projects that are served, and of those the ones they run.
		if admin || (p.State == store.ProjectActive && ctl.Access(actor, p.ID).Role == store.RoleAdmin) {
			rows = append(rows, p)
		}
	}
	if !admin && len(rows) == 0 {
		return nil, proto.Errf(proto.CodeForbidden,
			"ask a server admin, or an admin of the project you want to know about, to run `sfx admin status`",
			fmt.Sprintf("%s may not see the server's status: it is for server admins, and for project admins about their own projects", actor))
	}
	return s.report(ctx, rows, admin, in)
}

// report is the status of the projects rows, sorted by name, as in pages
// them, and of the server itself when server is set.
func (s *Server) report(ctx context.Context, rows []store.Project, server bool, in proto.StatusArgs) (any, *proto.Error) {
	rows, more, perr := page(rows, func(p store.Project) string { return p.Name }, in.AdminListArgs)
	if perr != nil {
		return nil, perr
	}
	out := proto.StatusResult{Projects: make([]proto.ProjectStatus, 0, len(rows)), More: more}
	if server {
		out.Server = s.serverStatus()
	}

	type count struct{ conns, file int }
	byProject := map[string]count{}
	s.mu.Lock()
	for _, o := range s.conns {
		if o.principal == "" {
			continue // still in its handshake
		}
		c := byProject[o.project]
		c.conns++
		if o.key == "" {
			c.file++
		}
		byProject[o.project] = c
	}
	s.mu.Unlock()
	at := map[string]int{}
	for _, p := range rows {
		c := byProject[p.ID]
		at[p.ID] = len(out.Projects)
		out.Projects = append(out.Projects, proto.ProjectStatus{ID: p.ID, Name: p.Name, State: string(p.State), Conns: c.conns, FileConns: c.file})
	}
	s.each(func(pr *served) {
		i, ok := at[pr.id]
		if !ok {
			return
		}
		st := &out.Projects[i]
		st.Open = true
		used := time.Unix(0, pr.used.Load()).UTC()
		st.LastUsed = &used
		w := pr.store.WriteStats()
		st.Writes = &proto.WriteStatus{LastMinute: w.LastMinute, Samples: w.Samples, P50Micros: w.P50.Microseconds(), P99Micros: w.P99.Microseconds()}
		// The one read: a count on the registry's index by last_seen. A
		// failure leaves the figure out and is logged; the rest stands.
		n, err := pr.store.SessionsSeen(ctx, 0)
		if err != nil {
			if ctx.Err() == nil {
				pr.log.Error("count the sessions seen, for status", "err", err)
			}
			return
		}
		st.Sessions = &n
	})
	return out, nil
}

// serverStatus is the daemon as a whole, from its own counters, the Go
// runtime and the control store's memory. A server of one project has no
// control store, so it counts no projects or principals, and its one
// store is the most it can have open.
func (s *Server) serverStatus() *proto.ServerStatus {
	lim := s.cfg.Limits
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s.slots.mu.Lock()
	conns := s.slots.total
	s.slots.mu.Unlock()
	out := &proto.ServerStatus{
		Version: s.cfg.Version, ProtoMin: s.cfg.ProtoMin, ProtoMax: s.cfg.ProtoMax,
		UptimeSeconds: int64(s.now().Sub(s.started) / time.Second),
		OpenStores:    s.OpenStores(), OpenStoresMax: lim.OpenStores,
		Conns: conns, ConnsMax: lim.Conns,
		HeapBytes: ms.HeapAlloc, SysBytes: ms.Sys, Goroutines: runtime.NumGoroutine(),
		Projects: map[string]int{}, Principals: map[string]int{},
		KeysFile: s.keysStatus(),
		Clock:    s.clockStatus(),
		Dolt:     s.doltStatus(),
	}
	if s.cfg.Control == nil {
		out.OpenStoresMax = 1
		return out
	}
	counts := s.cfg.Control.Counts()
	for state, n := range counts.Projects {
		out.Projects[string(state)] = n
	}
	for state, n := range counts.Principals {
		out.Principals[string(state)] = n
	}
	return out
}
