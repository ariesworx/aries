package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// TestAdminIdentityCommands drives the identity commands of slice N1
// through `aries` to a real daemon and store: a server admin invites an
// address for a person, lists and removes it, and sets a principal's
// display name and kind; the person sees their own identities; a project
// admin is refused, with a fix.
func TestAdminIdentityCommands(t *testing.T) {
	t.Parallel()
	w := keysWorld(t, server.Limits{})
	w.member("alice", project, store.RoleAdmin)
	w.member("bob", project, store.RoleMember)
	if _, err := w.ctl.AddPrincipal(t.Context(), sam, "dana", ""); err != nil { // a server admin with a row
		t.Fatal(err)
	}
	dana, alice, bob := w.newUser("dana", ""), w.newUser("alice", ""), w.newUser("bob", "")

	inv := decode[proto.IdentityResult](t, dana.ok("admin", "invite", "add", "bob", "bob@example.com", "--provider", "google", "--json"))
	if inv.Change != "added" || inv.Principal != "bob" || inv.State != proto.IdentityPending || inv.ExpiresAt == nil {
		t.Fatalf("invite add --json = %+v", inv)
	}
	id := fmt.Sprint(inv.ID)
	if got := dana.ok("admin", "invite", "add", "bob", "BOB@example.com", "--provider", "google"); !strings.HasPrefix(got,
		"invitation "+id+" for bob renewed: BOB@example.com at google, open until ") {
		t.Errorf("invite add again = %q, want the invitation renewed", got)
	}
	if got := dana.ok("admin", "invite", "list", "--pending"); !strings.Contains(got, id+"   bob        google    BOB@example.com  pending") {
		t.Errorf("invite list --pending:\n%s", got)
	}
	for who, u := range map[string]*user{"a server admin": dana, "bob himself": bob} {
		if got := u.ok("admin", "identity", "list", "bob"); !strings.HasPrefix(got, "identities of bob\n") || !strings.Contains(got, "pending") {
			t.Errorf("identity list bob, by %s:\n%s", who, got)
		}
	}
	if e := alice.refused(proto.CodeForbidden, "admin", "invite", "add", "bob", "b2@example.com", "--provider", "entra"); !strings.Contains(e.Fix, "server admin") {
		t.Errorf("invite add by a project admin: fix %q, want it to name a server admin", e.Fix)
	}
	dana.refused(proto.CodeInvalid, "admin", "invite", "add", "bob", "not-an-address", "--provider", "google")

	if got := dana.ok("admin", "principal", "set", "bob", "--display-name", "Bob Example"); got != "bob is now a person, shown as \"Bob Example\"\n" {
		t.Errorf("principal set --display-name = %q", got)
	}
	if got := dana.ok("admin", "principal", "add", "ci-bot", "--kind", "service", "--owner", "alice"); !strings.HasPrefix(got, "principal ci-bot added, a member of project ") {
		t.Errorf("principal add --kind service = %q", got)
	}
	shown := decode[proto.PrincipalShowResult](t, bob.ok("admin", "principal", "show", "bob", "--json"))
	if shown.DisplayName != "Bob Example" || shown.Kind != proto.KindPerson || len(shown.Identities) != 1 {
		t.Errorf("principal show bob, by bob = %+v", shown)
	}
	if e := dana.refused(proto.CodeInvalid, "admin", "invite", "add", "ci-bot", "ci@example.com", "--provider", "google"); !strings.Contains(e.Fix, "--kind person") {
		t.Errorf("invite add for a service: fix %q", e.Fix)
	}

	if got := dana.ok("admin", "invite", "remove", id); got != "invitation "+id+" removed: BOB@example.com at google, for bob\n" {
		t.Errorf("invite remove = %q", got)
	}
	if got := dana.ok("admin", "identity", "list", "bob"); got != "no identities of bob\n" {
		t.Errorf("identity list after the removal = %q", got)
	}
	evs := w.controlEvents()
	for _, want := range []string{"identity.invite bob by dana", "principal.set bob by dana", "principal.add ci-bot by dana", "identity.remove bob by dana"} {
		if !slices.Contains(evs, want) {
			t.Errorf("the control store's events lack %q: %v", want, evs)
		}
	}
}
