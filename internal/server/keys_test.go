package server

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// sam is a second server admin, in the tests that name one.
var sam = store.Actor{Principal: "sam", Session: "s-s", Machine: "laptop-s"}

// testKey is the public key keyLine gives owner: its type, its base64,
// its fingerprint and the key itself.
func testKey(t *testing.T, owner string) (typ, blob, fingerprint string, pub ssh.PublicKey) {
	t.Helper()
	sum := sha256.Sum256([]byte(owner))
	pub, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(sum[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	return pub.Type(), base64.StdEncoding.EncodeToString(pub.Marshal()), ssh.FingerprintSHA256(pub), pub
}

// fingerprintOf is testKey's fingerprint alone.
func fingerprintOf(t *testing.T, owner string) string {
	t.Helper()
	_, _, fp, _ := testKey(t, owner)
	return fp
}

// waitUntil polls cond for up to 10 seconds.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// fakeDaemon answers every connection to a unix socket with the frame
// answer returns, or closes it unanswered when answer returns nil.
func fakeDaemon(t *testing.T, answer func(asked *proto.Frame) *proto.Frame) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sff")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = c.Close() }()
				f, err := proto.NewDecoder(c).Decode()
				if err != nil {
					return
				}
				if out := answer(f); out != nil {
					_ = proto.NewEncoder(c).Encode(out)
				}
			})
		}
	})
	return sock
}

// dialKey connects a to project proj as a bridge whose forced command
// named the key fingerprint, and returns the welcome and the connection;
// closed is true when the daemon closed it without a welcome.
func dialKey(t *testing.T, s *Server, a store.Actor, proj, fingerprint string) (w *proto.Frame, c *conn, closed bool) {
	t.Helper()
	srv, cli := net.Pipe()
	c = &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	t.Cleanup(c.hangUp)
	c.wg.Go(func() {
		defer func() { _ = srv.Close() }()
		s.handle(t.Context(), srv)
	})
	c.wg.Go(func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: a.Principal, Key: fingerprint})
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: proj, Session: a.Session, Machine: a.Machine})
	})
	_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := c.dec.Decode()
	if err != nil {
		return nil, c, true
	}
	return f, c, false
}

// TestBridgeFrameKey pins what the daemon makes of the fingerprint a
// bridge frame carries, and that a frame without one, as a bridge from
// before this release sends and a line of authorized_keys still does,
// is served as before.
func TestBridgeFrameKey(t *testing.T) {
	m := adminWorld(t, Limits{}) // carol has a key
	carols := fingerprintOf(t, "carol")

	// An old-style frame, decoded from the bytes an old bridge writes.
	var old proto.Frame
	if err := json.Unmarshal([]byte(`{"t":"bridge","pr":"carol"}`), &old); err != nil || old.Key != "" {
		t.Fatalf("an old bridge frame decodes to %+v, %v", old, err)
	}
	w, c, closed := dialKey(t, m.Server, carol, projA, old.Key)
	if closed || w.Err != nil || w.Principal != "carol" {
		t.Fatalf("a bridge frame with no key = %+v, closed %v; want a welcome", w, closed)
	}
	// A request answered means the connection has been recorded.
	if !c.alive() || !m.has(owner{principal: "carol", project: projA}) || m.has(owner{principal: "carol", project: projA, key: carols}) {
		t.Error("the connection with no key is not recorded as carol's, with no key")
	}

	w, c, closed = dialKey(t, m.Server, carol, projA, carols)
	if closed || w.Err != nil {
		t.Fatalf("a bridge frame with carol's key = %+v, closed %v; want a welcome", w, closed)
	}
	if !c.alive() || !m.has(owner{principal: "carol", project: projA, key: carols}) {
		t.Error("the connection is not recorded with the fingerprint of the key that authenticated it")
	}

	refused := []struct {
		name string
		who  store.Actor
		key  string
		code proto.Code // "" means closed without a welcome
	}{
		{"a key that is no fingerprint", carol, "ssh-ed25519 AAAA", ""},
		{"a fingerprint with text after it", carol, carols + " --socket /tmp/x", ""},
		{"a key the store does not hold", carol, fingerprintOf(t, "nobody"), proto.CodeAuth},
		{"another principal's key", bob, carols, proto.CodeAuth},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			w, _, closed := dialKey(t, m.Server, tc.who, projA, tc.key)
			switch {
			case tc.code == "" && !closed:
				t.Errorf("got %+v (%v), want the connection closed", w, w.Err)
			case tc.code != "" && (closed || w.Err == nil || w.Err.Code != tc.code || w.Err.Fix == ""):
				t.Errorf("got %+v, closed %v; want a refusal %s with a fix", w, closed, tc.code)
			}
		})
	}

	// A server of one project serves no keys, and ignores one named.
	if w, _, closed := dialKey(t, newServer(t), alice, project, carols); closed || w.Err != nil {
		t.Errorf("a bridge frame with a key, on a server of one project = %+v, closed %v; want a welcome", w, closed)
	}
}

// has reports whether the server has recorded a connection as o's.
func (m *multi) has(o owner) bool {
	m.Server.mu.Lock()
	defer m.Server.mu.Unlock()
	for _, got := range m.conns {
		if got == o {
			return true
		}
	}
	return false
}

// TestKeyRemoveDrops removes a key and checks that the connections it
// had authenticated close, that those of the same principal that came in
// by authorized_keys stay, and that the key lets nobody in again.
func TestKeyRemoveDrops(t *testing.T) {
	m := adminWorld(t, Limits{})
	carols := fingerprintOf(t, "carol")
	d := mustDialTo(t, m.Server, dana, projA)
	_, byKeyA, _ := dialKey(t, m.Server, carol, projA, carols)
	_, byKeyB, _ := dialKey(t, m.Server, carol, projB, carols)
	byFile := mustDialTo(t, m.Server, carol, projA)
	other := mustDialTo(t, m.Server, bob, projA)
	for _, c := range []*conn{byKeyA, byKeyB} {
		if !c.alive() {
			t.Fatal("a connection by carol's key is not open")
		}
	}

	if got := m.sshd(t, carols); got != "carol" {
		t.Fatalf("the keys file gives carol's key to %q before its removal, want carol", got)
	}
	res := mustAsk[proto.KeyResult](d, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: carols})
	// carol's connection by the hand-maintained file stays. Nothing warns:
	// that file does not hold this key.
	if want := (proto.KeyResult{ID: carols, Principal: "carol", Type: "ssh-ed25519", Comment: "carol@example.com", Dropped: 2}); res != want {
		t.Errorf("key.remove = %+v\nwant %+v: carol's key, and 2 connections dropped", res, want)
	}
	if !byKeyA.dropped() || !byKeyB.dropped() {
		t.Error("a connection the removed key authenticated is still open")
	}
	if !byFile.alive() || !other.alive() || !d.alive() {
		t.Error("removing the key closed a connection it had not authenticated")
	}
	if got := m.sshd(t, carols); got != "" {
		t.Errorf("the keys file still gives the removed key to %q once the admin is answered", got)
	}
	if w, _, closed := dialKey(t, m.Server, carol, projA, carols); closed || w.Err == nil || w.Err.Code != proto.CodeAuth {
		t.Errorf("a handshake naming the removed key = %+v, closed %v; want an auth refusal", w, closed)
	}
	if !strings.Contains(m.log.String(), `msg="connections dropped" reason="key removed" by=dana connections=2`) {
		t.Errorf("the log lacks the drop:\n%s", m.log.String())
	}

	// A key removed while a handshake ran is caught as the connection is
	// recorded.
	if _, err := m.ctl.AddKey(t.Context(), dana, "carol", keyLine(t, "carol")); err != nil {
		t.Fatal(err)
	}
	sess := &session{actor: carol, proj: &served{id: projA}, fingerprint: carols}
	if !m.track(byFile.nc, sess) {
		t.Fatal("track refused a connection by a key the store holds")
	}
	if _, err := m.ctl.RemoveKey(t.Context(), dana, carols); err != nil {
		t.Fatal(err)
	}
	if m.track(byFile.nc, sess) {
		t.Error("track accepted a connection whose key was removed during the handshake")
	}
	m.untrack(byFile.nc)
}

// TestKeyOps runs key.add, keys and key.remove as each kind of caller.
func TestKeyOps(t *testing.T) {
	m := adminWorld(t, Limits{Control: store.ControlLimits{Keys: 2}})
	if _, err := m.ctl.AddPrincipal(t.Context(), dana, "dana", ""); err != nil { // a server admin with a row
		t.Fatal(err)
	}
	conns := map[string]*conn{
		"dana":  mustDialTo(t, m.Server, dana, projA),
		"alice": mustDialTo(t, m.Server, alice, projA), // admin of a: reaches bob, and herself
		"erin":  mustDialTo(t, m.Server, erin, projB),  // admin of b
		"bob":   mustDialTo(t, m.Server, bob, projA),   // a member
	}
	carols := fingerprintOf(t, "carol")
	const (
		ownFix   = "ask another admin to run it: a server admin, or a project admin who administers every project you belong to; the server's operator can also change authorized_keys on the server"
		reachFix = "check the name, or ask a server admin to run it; a project admin may only while every project the principal belongs to is one they administer"
		listFix  = "ask a server admin, or a project admin who administers every project the principal belongs to; `sfx admin key list %s` lists your own"
	)
	seq := func() int {
		t.Helper()
		evs, err := m.ctl.Events(t.Context(), 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	before := seq()
	refused := []struct {
		name string
		who  string
		op   string
		args any
		code proto.Code
		msg  string
		fix  string
	}{
		{"a member adds their own key", "bob", proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob")}, proto.CodeForbidden,
			"bob may not add a key to bob: nobody changes the keys of their own account", ownFix},
		{"a project admin adds their own key", "alice", proto.OpKeyAdd, proto.KeyAddArgs{Name: "alice", Key: keyLine(t, "alice")}, proto.CodeForbidden,
			"alice may not add a key to alice: nobody changes the keys of their own account", ownFix},
		{"a server admin adds their own key", "dana", proto.OpKeyAdd, proto.KeyAddArgs{Name: "dana", Key: keyLine(t, "dana")}, proto.CodeForbidden,
			"dana may not add a key to dana: nobody changes the keys of their own account", ownFix},
		{"a member adds a key to another", "bob", proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "x")}, proto.CodeForbidden,
			"bob may not add a key to carol: it is for project admins and server admins",
			"ask a server admin to run it, or a project admin who administers every project the principal belongs to"},
		{"a project admin adds a key out of reach", "alice", proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "x")}, proto.CodeForbidden,
			"alice may not add a key to carol: carol does not exist, or belongs to a project alice does not administer, or to none", reachFix},
		{"a project admin adds a key to a server admin", "alice", proto.OpKeyAdd, proto.KeyAddArgs{Name: "dana", Key: keyLine(t, "x")}, proto.CodeForbidden,
			"alice may not add a key to dana: dana is a server admin", "ask a server admin to run it"},
		// The same refusal for a key out of reach as for one no one has.
		{"a project admin removes a key out of reach", "alice", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: carols}, proto.CodeForbidden,
			"alice may not remove that key: it is no key of a principal alice may manage",
			"check the fingerprint with `sfx admin key list NAME`, or ask a server admin to run it"},
		{"a project admin removes a key no one has", "alice", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fingerprintOf(t, "nobody")}, proto.CodeForbidden,
			"alice may not remove that key: it is no key of a principal alice may manage",
			"check the fingerprint with `sfx admin key list NAME`, or ask a server admin to run it"},
		{"a member removes a key", "bob", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: carols}, proto.CodeForbidden,
			"bob may not remove that key: it is for project admins and server admins",
			"ask a server admin to run it, or a project admin who administers every project the principal belongs to"},
		{"a member lists another's keys", "bob", proto.OpKeys, proto.KeysArgs{Name: "carol"}, proto.CodeForbidden,
			"bob may not list the keys of carol: it is for the principal, for an admin who manages it, and for server admins", strings.ReplaceAll(listFix, "%s", "bob")},
		{"a project admin lists keys out of reach", "alice", proto.OpKeys, proto.KeysArgs{Name: "carol"}, proto.CodeForbidden,
			"alice may not list the keys of carol: it is for the principal, for an admin who manages it, and for server admins", strings.ReplaceAll(listFix, "%s", "alice")},
		// A name no one has is refused as one out of reach is.
		{"a project admin lists the keys of no one", "alice", proto.OpKeys, proto.KeysArgs{Name: "zed"}, proto.CodeForbidden,
			"alice may not list the keys of zed: it is for the principal, for an admin who manages it, and for server admins", strings.ReplaceAll(listFix, "%s", "alice")},
		{"a server admin lists the keys of no one", "dana", proto.OpKeys, proto.KeysArgs{Name: "zed"}, proto.CodeNotFound,
			"principal zed not found", "check the name; a server admin lists them with `sfx admin principal list`"},
		{"a server admin removes a key no one has", "dana", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fingerprintOf(t, "nobody")}, proto.CodeNotFound,
			"key " + fingerprintOf(t, "nobody") + " not found", "list a principal's keys, with their fingerprints, with `sfx admin key list NAME`"},
		{"a key another principal has", "dana", proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "carol")}, proto.CodeExists,
			"key " + carols + " already belongs to another principal",
			"use another key: a key belongs to one principal, and an agent or a machine that needs less access gets a key and a principal of its own"},
		{"a private key", "dana", proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA\n-----END OPENSSH PRIVATE KEY-----\n"},
			proto.CodeInvalid, "the key must be one public key in authorized_keys form: TYPE BASE64 [COMMENT], with no options",
			"give the public key file, such as ~/.ssh/id_ed25519.pub: `sfx admin key add NAME FILE`"},
		{"a fingerprint that is none", "dana", proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: "carol"}, proto.CodeInvalid,
			"a key is named by its SHA256: fingerprint", "correct it and retry; `sfx admin key remove -h` shows the usage"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := ask[json.RawMessage](conns[tc.who], tc.op, tc.args)
			if perr == nil || perr.Code != tc.code || perr.Message != tc.msg || perr.Fix != tc.fix {
				t.Errorf("%s by %s = %+v\nwant %s: %q\nfix %q", tc.op, tc.who, perr, tc.code, tc.msg, tc.fix)
			}
			if perr != nil && strings.Contains(perr.Message+perr.Fix, "PRIVATE") {
				t.Errorf("the refusal repeats what was sent: %+v", perr)
			}
		})
	}
	if after := seq(); after != before {
		t.Errorf("the refusals recorded %d events, want none", after-before)
	}

	// Another admin with reach may add and remove; the same key again
	// changes nothing.
	bobs := fingerprintOf(t, "bob")
	add := mustAsk[proto.KeyResult](conns["alice"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob")})
	if add != (proto.KeyResult{ID: bobs, Principal: "bob", Type: "ssh-ed25519", Comment: "bob@example.com", Change: "added"}) {
		t.Errorf("key.add bob by alice = %+v", add)
	}
	if again := mustAsk[proto.KeyResult](conns["dana"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob")}); again.Change != "unchanged" || again.ID != bobs {
		t.Errorf("key.add of a key bob has = %+v, want unchanged", again)
	}
	mustAsk[proto.KeyResult](conns["dana"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob 2")})
	// The cap on a principal's keys.
	_, perr := ask[proto.KeyResult](conns["dana"], proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob 3")})
	if perr == nil || perr.Code != proto.CodeInvalid || perr.Message != "a principal keeps at most 2 keys" ||
		perr.Fix != "remove a key the principal no longer uses with `sfx admin key remove FINGERPRINT`, or ask the server's operator to raise keys_per_principal under limits: in starfixd's config" {
		t.Errorf("a third key with keys_per_principal 2 = %+v", perr)
	}

	// Who may list: a server admin, an admin with reach, and the
	// principal itself.
	for _, who := range []string{"dana", "alice", "bob"} {
		ks := mustAsk[proto.KeysResult](conns[who], proto.OpKeys, proto.KeysArgs{Name: "bob"})
		var fps []string
		for _, k := range ks.Keys {
			fps = append(fps, k.Fingerprint)
			if k.Type != "ssh-ed25519" || k.AddedAt.IsZero() || k.AddedBy == "" {
				t.Errorf("keys of bob for %s: %+v, want its type, who added it and when", who, k)
			}
		}
		if want := []string{bobs, fingerprintOf(t, "bob 2")}; ks.Principal != "bob" || !slices.Equal(fps, want) {
			t.Errorf("keys of bob for %s = %v, want %v, oldest first", who, fps, want)
		}
	}
	if own := mustAsk[proto.KeysResult](conns["alice"], proto.OpKeys, proto.KeysArgs{Name: "alice"}); own.Keys == nil || len(own.Keys) != 0 {
		t.Errorf("alice's own keys = %+v, want an empty list", own.Keys)
	}
	rm := mustAsk[proto.KeyResult](conns["alice"], proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: bobs})
	if rm.ID != bobs || rm.Principal != "bob" || rm.Dropped != 0 {
		t.Errorf("key.remove of bob's key by alice = %+v", rm)
	}

	// principal.add with keys: the principal and its keys together.
	hal := mustAsk[proto.PrincipalAddResult](conns["alice"], proto.OpPrincipalAdd,
		proto.PrincipalAddArgs{Name: "hal", Keys: []string{keyLine(t, "hal"), keyLine(t, "hal 2")}})
	if hal.ID != "hal" || hal.Project != projA || !slices.Equal(hal.Keys, []string{fingerprintOf(t, "hal"), fingerprintOf(t, "hal 2")}) {
		t.Errorf("principal.add hal with two keys = %+v", hal)
	}
	if got := m.sshd(t, fingerprintOf(t, "hal")); got != "hal" {
		t.Errorf("the keys file gives hal's new key to %q once the admin is answered, want hal", got)
	}
	for _, tc := range []struct {
		name string
		who  string
		args proto.PrincipalAddArgs
		code proto.Code
		msg  string
	}{
		// A project admin is not told the key is anyone's (review F-6); a
		// server admin is.
		{"with a key another has, by a project admin", "alice", proto.PrincipalAddArgs{Name: "gus", Keys: []string{keyLine(t, "carol")}}, proto.CodeForbidden,
			"alice may not add that key to gus: it is no key alice may give a principal"},
		{"with a key another has", "dana", proto.PrincipalAddArgs{Name: "gus", Project: projA, Keys: []string{keyLine(t, "carol")}}, proto.CodeExists, "already belongs to another principal"},
		{"with a line that is no key", "alice", proto.PrincipalAddArgs{Name: "gus", Keys: []string{"nonsense"}}, proto.CodeInvalid, "the key must be one public key"},
		{"with more keys than a principal keeps", "alice", proto.PrincipalAddArgs{Name: "gus", Keys: []string{keyLine(t, "g1"), keyLine(t, "g2"), keyLine(t, "g3")}},
			proto.CodeInvalid, "a principal keeps at most 2 keys"},
		{"with more keys than one request carries", "dana", proto.PrincipalAddArgs{Name: "gus", Keys: make([]string, proto.MaxKeysPerAdd+1)},
			proto.CodeInvalid, "more than one principal add carries"},
		{"a server admin with no row, of themselves", "sam", proto.PrincipalAddArgs{Name: "sam", Unassigned: true, Keys: []string{keyLine(t, "sam")}},
			proto.CodeForbidden, "sam may not add a key to sam: nobody changes the keys of their own account"},
	} {
		t.Run("principal.add "+tc.name, func(t *testing.T) {
			c := conns[tc.who]
			if tc.who == "sam" {
				ms := newMultiAdmins(t, Limits{}, nil, dana.Principal, sam.Principal)
				ms.project(t, projA)
				c = mustDialTo(t, ms.Server, sam, projA)
			}
			_, perr := ask[proto.PrincipalAddResult](c, proto.OpPrincipalAdd, tc.args)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || perr.Fix == "" {
				t.Errorf("principal.add %s = %+v; want %s, a message containing %q and a fix", tc.args.Name, perr, tc.code, tc.msg)
			}
			if _, err := m.ctl.Principal(t.Context(), "gus"); err == nil {
				t.Error("the refused add left the principal behind")
			}
		})
	}
}

// TestDisabledServerAdmin pins decision 20 in the daemon: a server admin
// whose row is disabled is refused at the handshake and is no admin in
// any project's store, until enabled; one with no row is not disabled;
// and the last enabled server admin cannot be disabled.
func TestDisabledServerAdmin(t *testing.T) {
	m := newMultiAdmins(t, Limits{}, nil, dana.Principal, sam.Principal)
	p := m.project(t, projA, "dana", "bob")
	ctx := t.Context()
	st, err := m.cfg.Open(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	admins := func(want map[string]bool) {
		t.Helper()
		for name, w := range want {
			if got := st.IsAdmin(name); got != w {
				t.Errorf("the project store's IsAdmin(%s) = %v, want %v", name, got, w)
			}
			if got := m.ctl.IsServerAdmin(name); got != w {
				t.Errorf("the control store's IsServerAdmin(%s) = %v, want %v", name, got, w)
			}
		}
	}
	admins(map[string]bool{"dana": true, "sam": true, "bob": false})

	// sam, who has no row, is let in everywhere and disables dana.
	s := mustDialTo(t, m.Server, sam, projA)
	d := mustDialTo(t, m.Server, dana, projA)
	is := mustAsk[proto.CreateResult](d, proto.OpCreate, proto.CreateArgs{Title: "held by bob"})
	b := mustDialTo(t, m.Server, bob, projA)
	mustAsk[proto.StartResult](b, proto.OpStart, proto.StartArgs{ID: is.ID})
	mustAsk[proto.PrincipalResult](s, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "dana"})
	if !d.dropped() {
		t.Error("the disabled server admin's connection is still open")
	}
	admins(map[string]bool{"dana": false, "sam": true})
	w, _ := dialTo(t, m.Server, dana, projA)
	if w.Err == nil || w.Err.Code != proto.CodeForbidden || w.Err.Message != "dana is disabled on this server" ||
		!strings.Contains(w.Err.Fix, "`sfx admin principal enable dana`") {
		t.Errorf("a disabled server admin's handshake = %+v; want forbidden, told it is disabled and what enables it", w.Err)
	}
	// A project's store asks on every write: she may not override bob's
	// claim, as an admin may.
	title := "changed"
	cur, err := st.GetIssue(ctx, store.IssueID(is.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateIssue(ctx, dana, cur.ID, cur.Rev, store.IssuePatch{Title: &title}); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("a disabled server admin changing an issue bob holds = %v, want ErrForbidden", err)
	}

	mustAsk[proto.PrincipalResult](s, proto.OpPrincipalEnable, proto.PrincipalArgs{Name: "dana"})
	admins(map[string]bool{"dana": true})
	d = mustDialTo(t, m.Server, dana, projA)
	if _, err := st.UpdateIssue(ctx, dana, cur.ID, cur.Rev, store.IssuePatch{Title: &title}); err != nil {
		t.Errorf("an enabled server admin changes an issue bob holds: %v", err)
	}

	// With sam disabled, dana is the last: she cannot be disabled, by
	// herself or at all, and her connection stays.
	if _, err := m.ctl.AddPrincipal(ctx, dana, "sam", ""); err != nil {
		t.Fatal(err)
	}
	mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "sam"})
	if !s.dropped() {
		t.Error("sam's connection is still open")
	}
	_, perr := ask[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "dana"})
	if perr == nil || perr.Code != proto.CodeInvalid ||
		perr.Message != "dana is the last server admin who is not disabled; without one, nobody could enable anyone from here" ||
		perr.Fix != "enable another server admin first with `sfx admin principal enable NAME`, or ask the server's operator to name another under admins: in starfixd's config" {
		t.Errorf("disabling the last enabled server admin = %+v", perr)
	}
	if !d.alive() {
		t.Error("the refused disable closed the admin's connection")
	}
}

// A protocol 5 client, as the build before the key commands speaks,
// still works against this server: it sends no key op, and its
// principal.add, which has no keys field, adds a principal without
// keys as it did.
func TestProtocol5ClientStillServed(t *testing.T) {
	m := adminWorld(t, Limits{})
	srv, cli := net.Pipe()
	c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	t.Cleanup(c.hangUp)
	c.wg.Go(func() { m.handle(t.Context(), srv) })
	c.wg.Go(func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: dana.Principal})
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: 5, Project: projA, Session: dana.Session, Machine: dana.Machine})
	})
	if w := c.read(); w.T != proto.FrameWelcome || w.Err != nil || w.Min != 2 || w.Max != proto.ProtoMax {
		t.Fatalf("a protocol 5 hello = %+v (%v), want a welcome", w, w.Err)
	}
	// The arguments as a protocol 5 client encodes them.
	hal := mustAsk[proto.PrincipalResult](c, proto.OpPrincipalAdd, json.RawMessage(`{"name":"hal"}`))
	if hal != (proto.PrincipalResult{ID: "hal", Rev: 1, State: "active", Project: projA, ProjectName: "proj-a"}) {
		t.Errorf("a protocol 5 principal.add = %+v", hal)
	}
	if ks, err := m.ctl.Keys(t.Context(), "hal"); err != nil || len(ks) != 0 {
		t.Errorf("hal's keys = %v, %v; want none", ks, err)
	}
	for _, r := range []struct {
		op   string
		args string
	}{
		{proto.OpMemberSet, `{"name":"hal","role":"admin"}`},
		{proto.OpMembers, `{}`},
		{proto.OpPrincipalDisable, `{"name":"hal"}`},
		{proto.OpPrincipalEnable, `{"name":"hal"}`},
		{proto.OpMemberRemove, `{"name":"hal"}`},
		{proto.OpProjects, `{}`},
		{proto.OpList, `{}`},
	} {
		if _, perr := ask[json.RawMessage](c, r.op, json.RawMessage(r.args)); perr != nil {
			t.Errorf("protocol 5 %s: %v", r.op, perr)
		}
	}
}

// A protocol 5 server, as the build before the key commands runs,
// refuses this client at the handshake, telling it to have the server
// upgraded, before a key op could fail as unknown.
func TestHandshakeProtocol5ServerRefusesNewClient(t *testing.T) {
	s := newServer(t)
	s.cfg.ProtoMin, s.cfg.ProtoMax = 2, 5
	f, err := handshake(t, s, &proto.Frame{T: proto.FrameBridge, Principal: "alice"},
		&proto.Frame{T: proto.FrameHello, Version: "v0.5.0", Proto: proto.Proto, Project: project, Session: "s", Machine: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Err == nil || f.Err.Code != proto.CodeVersion || !strings.Contains(f.Err.Message, "accepts 2 to 5") ||
		!strings.Contains(f.Err.Fix, "starfixd upgrade") {
		t.Fatalf("a protocol %d client to a protocol 5 server = %+v, want a version refusal naming starfixd upgrade", proto.Proto, f.Err)
	}
}

// TestBridgeKey checks what the bridge sends: the fingerprint its forced
// command named, in a frame an older daemon reads as it always did, and
// nothing at all for a key that is no fingerprint.
func TestBridgeKey(t *testing.T) {
	fp := fingerprintOf(t, "carol")
	frames := make(chan *proto.Frame, 2)
	sock := fakeDaemon(t, func(f *proto.Frame) *proto.Frame {
		frames <- f
		return &proto.Frame{T: proto.FrameWelcome, Principal: f.Principal}
	})
	for _, key := range []string{fp, ""} {
		var out strings.Builder
		if err := Bridge(t.Context(), sock, "carol", key, strings.NewReader(""), &out); err != nil {
			t.Fatalf("Bridge with key %q: %v", key, err)
		}
		if f := <-frames; f.T != proto.FrameBridge || f.Principal != "carol" || f.Key != key {
			t.Errorf("Bridge with key %q sent %+v", key, f)
		}
	}
	// Without a key the frame is byte for byte what it was.
	b, err := json.Marshal(&proto.Frame{T: proto.FrameBridge, Principal: "carol"})
	if err != nil || string(b) != `{"t":"bridge","pr":"carol"}` {
		t.Errorf("a bridge frame with no key encodes to %s, %v; want the frame of a bridge before keys", b, err)
	}
	// A daemon from before keys reads a frame with one, and ignores it.
	var old struct {
		T         string `json:"t"`
		Principal string `json:"pr"`
	}
	if err := json.Unmarshal([]byte(`{"t":"bridge","pr":"carol","k":"`+fp+`"}`), &old); err != nil || old.T != "bridge" || old.Principal != "carol" {
		t.Errorf("an older frame type reads a bridge frame with a key as %+v, %v", old, err)
	}

	for _, key := range []string{"ssh-ed25519 AAAA", fp + " --socket /tmp/x", "SHA256:short"} {
		var out strings.Builder
		err := Bridge(t.Context(), sock, "carol", key, strings.NewReader(""), &out)
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.CodeAuth || !strings.Contains(out.String(), `"c":"auth"`) {
			t.Errorf("Bridge with key %q = %v, wrote %q; want an auth refusal written for the client", key, err, out.String())
		}
		select {
		case f := <-frames:
			t.Errorf("Bridge with key %q reached the daemon: %+v", key, f)
		default:
		}
	}
}
