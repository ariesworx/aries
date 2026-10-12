package server

// Review artifacts for docs/design/security-review-stage-3a.md. Each test
// demonstrates one finding of that review against the code as it is. A
// test that reproduces its finding skips with a message that states it,
// or fails when STARFIX_REVIEW_FAIL=1; one that no longer reproduces it
// passes, and can then be turned into a regression test or deleted.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
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

// F-2: decision 19 keeps an admin from adding a key to their own account,
// so that an agent holding the admin's key cannot outlive it. A server
// admin may still add a key to another server admin's account, and
// create a principal with a key and make it an admin of every project:
// either gives whoever holds the first key a second way in that survives
// the first.
func TestReviewF2ServerAdminKeyOutlivesTheAdmin(t *testing.T) {
	m := newMultiAdmins(t, Limits{}, nil, "dana", "dave")
	m.project(t, projA, "alice=admin")
	m.project(t, projB, "erin=admin")
	for _, name := range []string{"dana", "dave"} {
		if _, err := m.ctl.AddPrincipal(t.Context(), dana, name, ""); err != nil {
			t.Fatal(err)
		}
	}
	// An agent with dana's key, steered by what it read, runs `sfx admin`.
	d := mustDialTo(t, m.Server, dana, projA)
	if _, perr := ask[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "dana", Key: keyLine(t, "attacker-0")}); perr == nil {
		t.Fatal("dana added a key to her own account: decision 19 is not enforced at all")
	}
	_, perr := ask[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "dave", Key: keyLine(t, "attacker-1")})
	if perr != nil {
		t.Logf("a key for another server admin was refused: %v", perr)
		return
	}
	_, perr = ask[proto.PrincipalAddResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "ghost", Unassigned: true, Keys: []string{keyLine(t, "attacker-2")}})
	if perr != nil {
		t.Fatalf("principal.add ghost: %v", perr)
	}
	for _, p := range []string{projA, projB} {
		mustAsk[proto.MemberSetResult](d, proto.OpMemberSet, proto.MemberSetArgs{Name: "ghost", Project: p, Role: "admin"})
	}
	// dana's own access ends: the other server admin disables her.
	dv := mustDialTo(t, m.Server, store.Actor{Principal: "dave", Session: "s-dave", Machine: "m"}, projA)
	mustAsk[proto.PrincipalResult](dv, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "dana"})
	if w, _ := dialTo(t, m.Server, dana, projA); w.Err == nil {
		t.Fatal("dana was let in after being disabled")
	}
	// The keys the agent planted still work: one as a server admin.
	asDave := store.Actor{Principal: "dave", Session: "s-attacker", Machine: "elsewhere"}
	w, c, closed := dialKey(t, m.Server, asDave, projB, fingerprintOf(t, "attacker-1"))
	if closed || w.Err != nil {
		t.Fatalf("the planted key for dave was refused: %+v", w)
	}
	st, perr := ask[proto.StatusResult](c, proto.OpStatus, proto.StatusArgs{})
	if perr != nil || st.Server == nil {
		t.Fatalf("status with the planted key = %+v, %v; want a server admin's view", st, perr)
	}
	ghost := store.Actor{Principal: "ghost", Session: "s-ghost", Machine: "elsewhere"}
	if w, _, closed := dialKey(t, m.Server, ghost, projB, fingerprintOf(t, "attacker-2")); closed || w.Err != nil {
		t.Fatalf("the planted principal was refused: %+v", w)
	}
	reviewFinding(t, "F-2", "with dana's key alone, a key was added to server admin dave's account and a new principal was made admin of every project; "+
		"both still sign in after dana is disabled, the first as a server admin. Decision 19 stops only the admin's own account, "+
		"so docs/security-model.md's \"its access ends with the key it was given\" does not hold")
}

// F-3: a project admin plants a key on a principal within reach; when the
// admin of another project later adds that principal, the key reaches
// that project too. The notice that lists the keys a new member brings
// (keysCarried) is shown only to an admin with reach over the principal,
// which the admin of the second project never has, and `key list` refuses
// them. So nobody on the receiving side is shown the key.
func TestReviewF3PlantedKeyRidesIntoAnotherProject(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice=admin", "victor")
	m.project(t, projB, "erin=admin")
	a := mustDialTo(t, m.Server, alice, projA)
	add, perr := ask[proto.KeyResult](a, proto.OpKeyAdd, proto.KeyAddArgs{Name: "victor", Key: keyLine(t, "alice-second")})
	if perr != nil {
		t.Fatalf("alice adds a key to victor, a member of her project only: %v", perr)
	}
	// erin, who administers b and nothing else, adds victor to b.
	e := mustDialTo(t, m.Server, erin, projB)
	set := mustAsk[proto.MemberSetResult](e, proto.OpMemberSet, proto.MemberSetArgs{Name: "victor"})
	_, listErr := ask[proto.KeysResult](e, proto.OpKeys, proto.KeysArgs{Name: "victor"})
	if len(set.Keys) > 0 || listErr == nil {
		t.Logf("erin was shown victor's keys: member.set %+v, key list error %v", set, listErr)
		return
	}
	// alice, who is in no way a member of b, reads b as victor.
	asVictor := store.Actor{Principal: "victor", Session: "s-alice", Machine: "alices-laptop"}
	w, c, closed := dialKey(t, m.Server, asVictor, projB, add.ID)
	if closed || w.Err != nil {
		t.Fatalf("the planted key was refused in b: %+v", w)
	}
	if _, perr := ask[proto.ListResult](c, proto.OpList, proto.ListArgs{}); perr != nil {
		t.Fatalf("list in b with the planted key: %v", perr)
	}
	if w, _ := dialTo(t, m.Server, alice, projB); w.Err == nil {
		t.Fatal("alice herself was let into b")
	}
	reviewFinding(t, "F-3", "alice, admin of project a only, added a key of her own to victor while victor was in a alone; erin then added victor to project b, "+
		"was shown no key (member.set: "+set.Change+", keys: none) and is refused `key list victor`; alice's key now reads and writes project b as victor")
}

// F-4: a connection takes a per-principal slot only after its hello. One
// principal, even one the server would refuse (disabled, or in no
// project), can hold twice Limits.Conns connections between the bridge
// frame and the hello for 30 seconds each, and while it does every other
// connection is closed unread, an admin's included.
func TestReviewF4HalfOpenConnectionsShutEveryoneOut(t *testing.T) {
	m := newMulti(t, Limits{Conns: 3, ConnsPerPrincipal: 1}, nil)
	m.project(t, projA, "alice=admin", "mallory")
	if _, err := m.ctl.DisablePrincipal(t.Context(), dana, "mallory"); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "sfr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Serve(ctx, l) }()
	t.Cleanup(func() { cancel(); <-done })

	// mallory is disabled and limited to one connection; she opens six
	// and sends each one's bridge frame, as six SSH sessions would.
	for range 2 * 3 {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if err := proto.NewEncoder(c).Encode(&proto.Frame{T: proto.FrameBridge, Principal: "mallory"}); err != nil {
			t.Fatal(err)
		}
	}
	waitUntil(t, "the server to hold mallory's connections", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.conns) >= 6
	})
	// The server admin now tries to connect, to see what is going on.
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	enc := proto.NewEncoder(c)
	_ = enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: "dana"})
	_ = enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: projA, Session: "s-d", Machine: "m"})
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	w, err := proto.NewDecoder(c).Decode()
	if err == nil && w.Err == nil {
		t.Logf("the admin was welcomed past %d half-open connections", 6)
		return
	}
	if err == nil {
		t.Fatalf("the admin was refused with %v, not closed unread", w.Err)
	}
	reviewFinding(t, "F-4", "six connections of one disabled principal, each past its bridge frame and none past its hello, "+
		"filled the server (conns: 3, conns_per_principal: 1); the server admin's connection was then closed without an answer ("+err.Error()+"). "+
		"Each lasts 30 seconds and can be reopened, so one key holder keeps every person and admin out")
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
