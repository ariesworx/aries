package server

// Review artifacts for docs/design/security-review-stage-3a.md. Each test
// demonstrates one finding of that review against the code as it is. A
// test that reproduces its finding skips with a message that states it,
// or fails when STARFIX_REVIEW_FAIL=1; one that no longer reproduces it
// passes, and can then be turned into a regression test or deleted.

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// reviewFinding reports that a finding of the security review was
// reproduced.
func reviewFinding(t *testing.T, id, text string) {
	t.Helper()
	if os.Getenv("STARFIX_REVIEW_FAIL") != "" {
		t.Errorf("SECURITY REVIEW %s reproduced: %s", id, text)
		return
	}
	t.Skipf("SECURITY REVIEW %s reproduced: %s", id, text)
}

// F-5: a project admin adds the public key of a server admin, whose key
// is a line of the hand-maintained authorized_keys file and not in the
// store, to a principal within reach. The store accepts it, says nothing
// of the file, and the keys file then holds a line that signs that key
// in as the other principal. sshd takes the first line that matches: with
// authorized_keys read first, as on main, the admin's own line still
// wins; with the generated file read first, the order of the change in
// flight, the server admin signs in as the project admin's principal and
// is a server admin no more.
func TestReviewF5StoreShadowsAHandMaintainedKey(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice=admin", "bob")
	danasKey, _, _ := strings.Cut(keyLine(t, "dana-break-glass"), " dana")
	hand := `restrict,command="/usr/local/bin/starfixd stdio --principal dana" ` + danasKey + "\n"
	if err := os.WriteFile(m.byHand, []byte(hand), 0o600); err != nil {
		t.Fatal(err)
	}
	a := mustDialTo(t, m.Server, alice, projA)
	res, perr := ask[proto.KeyResult](a, proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: danasKey})
	if perr != nil {
		t.Logf("the store refused a key that is another principal's line of authorized_keys: %v", perr)
		return
	}
	b, err := os.ReadFile(m.keysFile)
	if err != nil {
		t.Fatal(err)
	}
	want := "--principal bob --key " + res.ID + `" ` + danasKey
	if !strings.Contains(string(b), want) {
		t.Fatalf("the keys file has no line for the key as bob:\n%s", b)
	}
	reviewFinding(t, "F-5", "project admin alice gave bob the public key of server admin dana, which is a line of authorized_keys only; "+
		"the answer carried no warning (in_authorized_keys: "+map[bool]string{true: "true", false: "false"}[res.InAuthorizedKeys]+", warning: \""+res.Warning+"\") "+
		"and the generated file now maps dana's key to bob. Whichever file sshd reads first decides who dana is")
}

// F-9: `project archive` refuses the server's last active project, since
// every admin op rides a connection to an active project. The check
// reads the index before the write and is not part of it, so two
// archives sent together, of the last two projects, both pass it. No
// project is then active: nobody can connect, the daemon refuses to
// start again, and `control init` leaves an archived project archived.
func TestReviewF9TwoArchivesLeaveNoActiveProject(t *testing.T) {
	for attempt := range 24 {
		m := newMulti(t, Limits{}, nil)
		m.project(t, projA)
		m.project(t, projB)
		c1, c2 := mustDialTo(t, m.Server, dana, projA), mustDialTo(t, m.Server, dana, projB)
		var wg sync.WaitGroup
		errs := make([]*proto.Error, 2)
		for i, c := range []*conn{c1, c2} {
			wg.Go(func() {
				if i == 0 {
					time.Sleep(time.Duration(attempt%12) * 250 * time.Microsecond)
				}
				_, errs[i] = ask[proto.ProjectResult](c, proto.OpProjectArchive, proto.ProjectArgs{Project: []string{projA, projB}[i]})
			})
		}
		wg.Wait()
		if m.ctl.ActiveProjects() == 0 {
			w, _ := dialTo(t, m.Server, dana, projA)
			reviewFinding(t, "F-9", "two `project archive` requests sent together archived the server's last two active projects; the server admin's next handshake is refused (\""+w.Err.Message+"\") and no project is left to connect to")
			return
		}
	}
	t.Log("24 attempts did not archive both projects: the gap between the check and the write (admin.go, projectArchive) was not hit")
}
