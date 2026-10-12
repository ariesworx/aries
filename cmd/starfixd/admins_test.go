package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/store"
)

// TestServeWarnsOfAnAdminSomeoneElseMade pins a note of the security
// review of stage 3a. A project admin may create a principal of any free
// name, with a key of their own. If the operator later names that
// principal under admins:, the project admin's key is a server admin's.
// The daemon cannot refuse to start for it, but it says so at start:
// which configured admin has a row or a key that no server admin, and
// not the operator, put there.
func TestServeWarnsOfAnAdminSomeoneElseMade(t *testing.T) {
	s := newSite(t, "")
	keys := writeKeys(t, forced(t, "dana", "dana-1"), forced(t, "alice", "alice-1"))
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
	alice := store.Actor{Principal: "alice", Session: "s", Machine: "m"}
	c := s.control(t)
	ctx := t.Context()
	if _, err := c.SetMember(ctx, dana, "alice", adopted, store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	// alice, a project admin, makes zoe, with a key alice holds; and adds
	// a key to yan, whom dana made.
	zoesKey, zoesFP := pubKey(t, "alices-own")
	if _, err := c.AddPrincipal(ctx, alice, "zoe", adopted, zoesKey); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddPrincipal(ctx, dana, "yan", adopted); err != nil {
		t.Fatal(err)
	}
	yansKey, yansFP := pubKey(t, "alices-other")
	if _, err := c.AddKey(ctx, alice, "yan", yansKey); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	start := func(admins string) string {
		t.Helper()
		// The site's config, as newSite writes it, with other admins.
		rewrite(t, s, fmt.Sprintf("project: %s\nprefix: acme\naccount: acme-2026\ncontrol_db: %s\nadmins: [%s]\n", adopted, s.controlDB, admins))
		var log bytes.Buffer
		d, err := openDaemon(ctx, s.settings(t), slog.New(slog.NewTextHandler(&log, nil)))
		if err != nil {
			t.Fatalf("open daemon with admins %s: %v", admins, err)
		}
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		return log.String()
	}
	const what = "a configured server admin was set up by someone who is no server admin"
	if log := start("dana"); strings.Contains(log, what) {
		t.Errorf("with admins dana, whose row and key the operator made, the daemon warned:\n%s", log)
	}
	log := start("dana, zoe, yan")
	for _, want := range []string{
		what, "event=admin.unvouched", "admin=zoe", "created_by=alice", "fingerprint=" + zoesFP, "added_by=alice",
		"admin=yan", "fingerprint=" + yansFP,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("with zoe and yan named admins, the daemon's log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "admin=dana") {
		t.Errorf("the daemon warned of dana, whom the operator set up:\n%s", log)
	}
	if strings.Contains(log, "AAAA") {
		t.Errorf("the warning holds a key, not its fingerprint:\n%s", log)
	}
}
