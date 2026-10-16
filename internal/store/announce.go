package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Announcing a sign-in (identity.md §13). A sign-in is the control
// store's, which records it with a signin.grant event; the person learns
// of it here, in the inbox of each project their principal is a member
// of, where `aries inbox`, prime and the push already reach them. The
// daemon calls AnnounceSignIn on each such project's store once the
// grant has committed.

// InboxSignIn tells a principal it signed in: whoever holds the token
// works as the principal until the sign-in ends, so a sign-in the person
// did not make is theirs to end. It is addressed to every session.
const InboxSignIn InboxKind = "signin.granted"

// announcer is who a sign-in's announcement is from.
var announcer = Actor{Principal: ReaperActor.Principal, Session: "signin", Machine: "server"}

// SignInNotice is a sign-in as the inbox tells its principal of it:
// nothing of the person's account at the provider, and no token.
type SignInNotice struct {
	Principal string
	// Grant is the sign-in's id, which `aries admin signin end` takes.
	Grant    string
	Provider string
	Flow     string
	// Client is the client the sign-in is for (aries-cli), and Machine
	// what that client said it is, trusted for nothing.
	Client  string
	Machine string
	EndsAt  time.Time
}

// validate refuses, with ErrInvalid, a notice the inbox would not show.
func (n SignInNotice) validate() error {
	if err := validPrincipal(n.Principal); err != nil {
		return err
	}
	if !providerPattern.MatchString(n.Provider) {
		return fmt.Errorf("%w: sign-in notice: provider %q is not a provider's name", ErrInvalid, n.Provider)
	}
	if !slices.Contains(signInFlows, n.Flow) {
		return fmt.Errorf("%w: sign-in notice: flow %q is not one", ErrInvalid, n.Flow)
	}
	if n.EndsAt.IsZero() {
		return fmt.Errorf("%w: sign-in notice: no end", ErrInvalid)
	}
	for _, f := range []struct{ name, v string }{{"grant", n.Grant}, {"client", n.Client}, {"machine", n.Machine}} {
		if err := checkLine("sign-in notice: "+f.name, f.v, maxName, true); err != nil {
			return err
		}
	}
	return nil
}

// body is what the item says. The machine, which the client chose and
// may be long, comes last, so that the cut to InboxBodyMax takes it
// before the way to end the sign-in.
func (n SignInNotice) body() string {
	return fmt.Sprintf("signed in at %s (%s, %s) until %s; not you? `aries admin signin end %s`; `aries logout` on that machine ends it too; machine %s",
		n.Provider, n.Flow, n.Client, n.EndsAt.UTC().Format(time.RFC3339), n.Grant, n.Machine)
}

// AnnounceSignIn puts n in its principal's inbox as a signin.granted
// item from ariesd, pushed to the principal's watching sessions. It is
// never held back by the Notices limit; Limits.InboxUnread and the
// pruning of read items bound it as they bound every item. It records no
// event: the sign-in's history is the control store's signin.grant
// event, and this is the person's copy of it (rule 7). A notice the
// inbox would not show is refused with ErrInvalid.
func (s *Store) AnnounceSignIn(ctx context.Context, n SignInNotice) error {
	if err := n.validate(); err != nil {
		return err
	}
	return s.write(ctx, announcer, func(w *wtx) error {
		w.quiet = true
		return w.notify(ctx, InboxItem{To: n.Principal, Kind: InboxSignIn, Body: n.body()})
	})
}
