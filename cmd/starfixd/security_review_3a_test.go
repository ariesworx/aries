package main

// Review artifact for docs/design/security-review-stage-3a.md, finding
// F-7. It skips with a message that states the finding, or fails when
// STARFIX_REVIEW_FAIL=1, and passes once it no longer reproduces.

import (
	"os"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// F-7: `starfixd control init` is documented as safe to run again, and
// the Docker image runs it at every start. It gives every principal with
// a line in authorized_keys a membership of the adopted project when it
// has none, so a membership an admin removed comes back at the next run,
// while the line is still in the file: and the line has to stay there,
// or be replaced by a key in the store, while its principal belongs to
// any other project.
func TestReviewF7ControlInitRestoresARemovedMembership(t *testing.T) {
	s := newSite(t, "")
	keys := writeKeys(t, forced(t, "dana", "dana-1"), forced(t, "bob", "bob-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
	c := s.control(t)
	// bob is taken off the adopted project.
	if _, err := c.RemoveMember(t.Context(), dana, "bob", adopted); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if role, ok, err := s.control(t).Role(t.Context(), "bob", adopted); err != nil || ok {
		t.Fatalf("bob's role after his removal = %q, %v, %v; want none", role, ok, err)
	}
	// The operator runs it again, to import a new line, or the container
	// restarts.
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init again: %v\n%s", got.err, got.stderr)
	}
	sock := serveSite(t, s)
	w := hello(t, sock, "bob", adopted)
	if w.Err != nil {
		if w.Err.Code != proto.CodeForbidden {
			t.Fatalf("bob's handshake = %v", w.Err)
		}
		return
	}
	msg := "SECURITY REVIEW F-7 reproduced: bob was removed from the adopted project with `member remove`; `starfixd control init` run again, " +
		"with his line still in authorized_keys, made him a member again, and his handshake is welcomed"
	if os.Getenv("STARFIX_REVIEW_FAIL") != "" {
		t.Error(msg)
		return
	}
	t.Skip(msg)
}
