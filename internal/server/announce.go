package server

import (
	"context"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/store"
)

// announceSignIn tells g's principal of the sign-in in its inbox in every
// active project it is a member of (identity.md §13): a sign-in the
// person did not make, a phished one, is then in front of them at their
// next `aries inbox` or prime, with how to end it. An inbox is a
// project's, so the notice goes to each; one in the control store would
// need a read and an ack beside the inbox's, and the push, for one kind.
//
// The grant has committed, so the notices go on if the request ends. A
// project's store that is closed is opened for it through the registry,
// as the archive does, so that the project never has two writers. A
// store that cannot be opened or written is logged and passed over: the
// sign-in stands, and the audit stream has it.
func (s *Server) announceSignIn(ctx context.Context, g store.Grant) {
	ctx = context.WithoutCancel(ctx)
	ms, err := s.cfg.Control.ProjectsOf(ctx, g.Principal)
	if err != nil {
		s.cfg.Logger.ErrorContext(ctx, "announce a sign-in", "event", logs.SignInAnnounceFailed, "principal", g.Principal, "grant", g.ID, "err", err)
		return
	}
	n := store.SignInNotice{Principal: g.Principal, Grant: g.ID, Provider: g.Provider, Flow: g.Flow, Client: g.ClientID,
		Machine: g.Machine, EndsAt: g.EndsAt}
	for _, m := range ms {
		if err := s.announceIn(ctx, m.Project, n); err != nil {
			s.cfg.Logger.ErrorContext(ctx, "announce a sign-in", "event", logs.SignInAnnounceFailed, "principal", g.Principal, "grant", g.ID,
				"project", m.Project, "err", err)
		}
	}
}

// announceIn puts n in the inbox of project id, if it is active.
func (s *Server) announceIn(ctx context.Context, id string, n store.SignInNotice) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	p, err := s.cfg.Control.Project(ctx, id)
	if err != nil || p.State != store.ProjectActive {
		return err // only an active project is served, and has an inbox to read
	}
	held, err := s.stores.acquire(ctx, p)
	if err != nil {
		return err
	}
	defer s.stores.release(held)
	return held.pr.store.AnnounceSignIn(ctx, n)
}
