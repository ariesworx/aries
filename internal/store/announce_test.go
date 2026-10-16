package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// signInNotice is a sign-in of alice's as the daemon announces it.
func signInNotice() SignInNotice {
	return SignInNotice{Principal: "alice", Grant: "g-0123456789abcdef", Provider: "corp", Flow: FlowLoopback,
		Client: "aries-cli", Machine: "laptop-a", EndsAt: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
}

// TestAnnounceSignIn: a sign-in is an item in the principal's inbox,
// for every session, from ariesd, saying how to end it; it is pushed to
// a watch like any item, is never dropped by notices_per_minute, and
// records no event, the control store's signin.grant being its history.
func TestAnnounceSignIn(t *testing.T) {
	t.Parallel()
	s := openStore(t, newDSN(t), Options{Limits: Limits{Notices: 1}})
	w := s.Watch("alice", "sess-a")
	defer w.Close()
	seq := lastSeq(t, s)
	const announced = 3 // past notices_per_minute
	for range announced {
		if err := s.AnnounceSignIn(t.Context(), signInNotice()); err != nil {
			t.Fatalf("AnnounceSignIn: %v", err)
		}
	}
	page, err := s.Inbox(t.Context(), alice, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != announced || page.Unread != announced {
		t.Fatalf("alice's inbox after %d sign-ins = %+v, want %d unread items", announced, page, announced)
	}
	const want = "signed in at corp (loopback, aries-cli) until 2026-10-17T12:00:00Z; not you? `aries admin signin end g-0123456789abcdef`;" +
		" `aries logout` on that machine ends it too; machine laptop-a"
	for _, it := range page.Items {
		if it.Kind != InboxSignIn || it.From != ReaperActor.Principal || it.Session != "" || it.Issue != "" || it.Body != want {
			t.Errorf("the item = %+v, want %s from %s for every session, saying %q", it, InboxSignIn, ReaperActor.Principal, want)
		}
	}
	if pushed, _, _ := w.Take(); len(pushed) != announced {
		t.Errorf("the watch was pushed %d items, want %d", len(pushed), announced)
	}
	if got := lastSeq(t, s); got != seq {
		t.Errorf("the log's last event after the announcements = %d, want %d: an announcement records no event", got, seq)
	}
}

// TestAnnounceSignInLongMachine: a machine's name, which the client
// chose, is cut before the way to end the sign-in is.
func TestAnnounceSignInLongMachine(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	n := signInNotice()
	n.Machine = strings.Repeat("m", maxName)
	if err := s.AnnounceSignIn(t.Context(), n); err != nil {
		t.Fatalf("AnnounceSignIn: %v", err)
	}
	page, err := s.Inbox(t.Context(), alice, false, 0)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("alice's inbox = %+v, %v; want one item", page, err)
	}
	if body := page.Items[0].Body; !strings.Contains(body, "`aries admin signin end g-0123456789abcdef`") {
		t.Errorf("the item says %q, which lost the way to end the sign-in", body)
	}
}

// TestAnnounceSignInRefusals: a notice the store would not show is
// refused with ErrInvalid, and nothing is written.
func TestAnnounceSignInRefusals(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	tests := []struct {
		name string
		edit func(*SignInNotice)
	}{
		{"no principal", func(n *SignInNotice) { n.Principal = "" }},
		{"a reserved principal", func(n *SignInNotice) { n.Principal = ReaperActor.Principal }},
		{"no grant", func(n *SignInNotice) { n.Grant = "" }},
		{"a grant on two lines", func(n *SignInNotice) { n.Grant = "g-1\ng-2" }},
		{"a provider that is not a name", func(n *SignInNotice) { n.Provider = "Corp Login" }},
		{"a flow that is not one", func(n *SignInNotice) { n.Flow = "fax" }},
		{"no client", func(n *SignInNotice) { n.Client = "" }},
		{"a machine with a control character", func(n *SignInNotice) { n.Machine = "laptop\x1b[2J" }},
		{"no end", func(n *SignInNotice) { n.EndsAt = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := signInNotice()
			tc.edit(&n)
			if err := s.AnnounceSignIn(t.Context(), n); !errors.Is(err, ErrInvalid) {
				t.Errorf("AnnounceSignIn(%+v) = %v, want ErrInvalid", n, err)
			}
		})
	}
	if page, err := s.Inbox(t.Context(), alice, true, 0); err != nil || len(page.Items) != 0 {
		t.Errorf("alice's inbox after the refusals = %+v, %v; want nothing", page.Items, err)
	}
}
