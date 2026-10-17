package server

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// Expiring keys (transport.md §11.3, identity.md §4; slice S1).

// endsAt is when the expiring-key tests' clocks start: 09:00:00.5 on a
// server set eleven hours west of UTC, where a value without its Z
// would have revived an ended key for eleven hours (identity.md §4.1).
var endsAt = time.Date(2026, 10, 16, 9, 0, 0, 5e8, time.FixedZone("UTC-11", -11*3600))

// expiryLine is the keys file's line for owner's test key, ending at
// the expiry-time value end.
func expiryLine(t *testing.T, owner, principal, end string) string {
	t.Helper()
	typ, blob, fp, _ := testKey(t, owner)
	return "restrict,expiry-time=\"" + end + "\",command=\"/usr/local/bin/ariesd stdio --principal " + principal + " --key " + fp + "\" " + typ + " " + blob + "\n"
}

// key.add gives a key an end by a duration, counted on the server's
// clock, or by a time; the result and the list say when, and the keys
// file's line carries it as expiry-time in UTC, marked Z.
func TestKeyAddExpires(t *testing.T) {
	t.Parallel()
	m := adminWorldAt(t, Limits{}, &clock{t: endsAt}) // carol has a key with no end
	d := mustDialTo(t, m.Server, dana, projA)

	in := mustAsk[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob"), ExpiresIn: "2h"})
	want := time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC) // 20:00:00.5 UTC, two hours on, to the second
	if in.ExpiresAt == nil || !in.ExpiresAt.Equal(want) || in.Change != "added" {
		t.Errorf("key.add with expires_in 2h = %+v; want it added, ending %v", in, want)
	}
	// A time in any zone is the same instant, kept in UTC.
	at := time.Date(2026, 10, 18, 3, 4, 5, 0, time.FixedZone("UTC+14", 14*3600))
	on := mustAsk[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "frank", Key: keyLine(t, "frank"), ExpiresAt: &at})
	if on.ExpiresAt == nil || !on.ExpiresAt.Equal(at) || on.ExpiresAt.Location() != time.UTC {
		t.Errorf("key.add with expires_at %v = %+v; want that end, in UTC", at, on)
	}

	// The digits are UTC's and end in Z, whatever the server's zone: sshd
	// reads digits without the Z in its own (identity.md §4.1).
	got := m.keysFileOf(t)
	wantFile := expiryLine(t, "bob", "bob", "20261016220000Z") + keysLine(t, "carol") + expiryLine(t, "frank", "frank", "20261017130405Z")
	if got != wantFile {
		t.Errorf("the keys file holds\n%swant\n%s", got, wantFile)
	}
	for _, owner := range []string{"bob", "frank"} {
		if p := m.sshd(t, fingerprintOf(t, owner)); p != owner {
			t.Errorf("sshd reads %s's line, with its end, as letting in %q", owner, p)
		}
	}

	ks := mustAsk[proto.KeysResult](d, proto.OpKeys, proto.KeysArgs{Name: "bob"})
	if len(ks.Keys) != 1 || ks.Keys[0].ExpiresAt == nil || !ks.Keys[0].ExpiresAt.Equal(want) || ks.Keys[0].Origin != store.OriginAdmin {
		t.Errorf("keys bob = %+v; want the key, ending %v, of origin admin", ks.Keys, want)
	}

	refused := []struct {
		name string
		args proto.KeyAddArgs
		code proto.Code
		fix  string // the fix contains it
	}{
		{"a duration that is none", proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob 2"), ExpiresIn: "soon"},
			proto.CodeInvalid, "--expires"},
		{"both", proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob 2"), ExpiresIn: "2h", ExpiresAt: &at},
			proto.CodeInvalid, "--expires"},
		{"an end that has passed", proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob 2"), ExpiresAt: new(endsAt.Add(-time.Minute))},
			proto.CodeInvalid, "--expires"},
		{"past the ceiling", proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob 2"), ExpiresIn: "3651d"},
			proto.CodeInvalid, "--expires"},
		{"an end for a key with another", proto.KeyAddArgs{Name: "bob", Key: keyLine(t, "bob"), ExpiresIn: "3h"},
			proto.CodeExists, "aries admin key remove"},
		{"an end for a key with none", proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "carol"), ExpiresIn: "1h"},
			proto.CodeExists, "aries admin key remove"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := ask[proto.KeyResult](d, proto.OpKeyAdd, tc.args)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Fix, tc.fix) {
				t.Errorf("key.add %+v = %+v; want %s with a fix naming %s", tc.args, perr, tc.code, tc.fix)
			}
		})
	}
}

// At a key's end the daemon closes the connections it signed in, takes
// its line out of the keys file and refuses it at the handshake, on the
// store's clock; the key stays listed until it is pruned.
func TestKeyEndClosesConnections(t *testing.T) {
	t.Parallel()
	ck := &clock{t: endsAt}
	m := adminWorldAt(t, Limits{}, ck) // carol has a key with no end
	ctx := t.Context()
	carols, temp := fingerprintOf(t, "carol"), fingerprintOf(t, "carol temp")
	d := mustDialTo(t, m.Server, dana, projA)
	added := mustAsk[proto.KeyResult](d, proto.OpKeyAdd, proto.KeyAddArgs{Name: "carol", Key: keyLine(t, "carol temp"), ExpiresIn: "1h"})
	_, byTempA, _ := dialKey(t, m.Server, carol, projA, temp)
	_, byTempB, _ := dialKey(t, m.Server, carol, projB, temp)
	_, byPlain, _ := dialKey(t, m.Server, carol, projA, carols)
	other := mustDialTo(t, m.Server, bob, projA)
	for _, c := range []*conn{byTempA, byTempB, byPlain} {
		if !c.alive() {
			t.Fatal("a connection by one of carol's keys is not open")
		}
	}

	ck.advance(time.Hour - time.Second)
	if next := m.ExpireKeys(ctx); !next.Equal(*added.ExpiresAt) {
		t.Errorf("ExpireKeys a second before the end = next %v, want %v", next, *added.ExpiresAt)
	}
	if !byTempA.alive() || m.sshd(t, temp) != "carol" {
		t.Fatal("the key ended a second early")
	}

	ck.advance(time.Second)
	if next := m.ExpireKeys(ctx); !next.IsZero() {
		t.Errorf("ExpireKeys at the end = next %v, want none", next)
	}
	if !byTempA.dropped() || !byTempB.dropped() {
		t.Error("a connection the ended key signed in is still open")
	}
	if !byPlain.alive() || !other.alive() || !d.alive() {
		t.Error("the key's end closed a connection it had not signed in")
	}
	if p := m.sshd(t, temp); p != "" {
		t.Errorf("the keys file still gives the ended key to %q", p)
	}
	w, _, closed := dialKey(t, m.Server, carol, projA, temp)
	if closed || w.Err == nil || w.Err.Code != proto.CodeAuth || !strings.Contains(w.Err.Message, "ended at 2026-10-16T21:00:00Z") ||
		!strings.Contains(w.Err.Fix, "aries admin key add") {
		t.Errorf("a handshake by the ended key = %+v, closed %v; want auth, saying when it ended, with a fix", w, closed)
	}
	log := m.log.String()
	expired := `event=` + string(logs.KeyExpired) + ` fingerprint=` + temp + ` subject=carol at=2026-10-16T21:00:00.000Z connections=2`
	if !strings.Contains(log, expired) || !strings.Contains(log, `event=conn.dropped reason="key expired" by=ariesd connections=2`) {
		t.Errorf("the log lacks the key's end or the drop:\n%s", log)
	}
	// Said once, not on every check.
	m.ExpireKeys(ctx)
	if n := strings.Count(m.log.String(), "event="+string(logs.KeyExpired)); n != 1 {
		t.Errorf("the key's end was logged %d times, want once", n)
	}

	// Listed until it is pruned, an hour after its end.
	if ks := mustAsk[proto.KeysResult](d, proto.OpKeys, proto.KeysArgs{Name: "carol"}); len(ks.Keys) != 2 {
		t.Errorf("keys carol after the end = %+v; want both, the ended one still listed", ks.Keys)
	}
	ck.advance(store.KeyPruneAfter + time.Second)
	m.Prune(ctx)
	if ks := mustAsk[proto.KeysResult](d, proto.OpKeys, proto.KeysArgs{Name: "carol"}); len(ks.Keys) != 1 || ks.Keys[0].Fingerprint != carols {
		t.Errorf("keys carol after pruning = %+v; want the key with no end alone", ks.Keys)
	}
	if log := m.log.String(); !strings.Contains(log, "event="+string(logs.ReaperPruned)) || !strings.Contains(log, "keys=1") {
		t.Errorf("the log lacks the pruned key:\n%s", log)
	}
}

// A key past its end proves nothing over HTTPS either (decision 89),
// and the connection it proved closes at the end.
func TestHTTPSKeyEnded(t *testing.T) {
	t.Parallel()
	ck := &clock{t: endsAt}
	var u string
	var pool *x509.CertPool
	m := newMultiWith(t, Limits{}, ck, nil, func(c *Config) { u, pool = listenHTTPS(t, c, nil) }, dana.Principal)
	m.project(t, projA, "carol")
	if _, err := m.ctl.AddKeyEnding(t.Context(), dana, "carol", keyLine(t, "carol"), store.KeyEnd{In: time.Hour}); err != nil {
		t.Fatal(err)
	}
	m.refresh(t.Context())
	serving(t, m.Server)

	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c, u, signerOf(t, "carol"), nil); f != nil {
		t.Fatalf("carol's proof by her key before its end: %v", f.Err)
	}
	if w := helloTo(t, c, projA); w.Err != nil || w.Principal != "carol" {
		t.Fatalf("carol over HTTPS before her key's end: %+v", w)
	}

	ck.advance(time.Hour)
	m.ExpireKeys(t.Context())
	_ = c.SetReadDeadline(time.Now().Add(patience))
	for {
		if _, err := c.Read(); err != nil {
			if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
				t.Error("carol's connection over HTTPS stayed open after her key's end")
			}
			break
		}
	}
	c2, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The refusal follows the proof: an ended key is told so once proved.
	f := prove(t, c2, u, signerOf(t, "carol"), nil)
	if f == nil {
		f = refusalIn(mustRead(t, c2))
	}
	if f == nil || f.Err.Code != proto.CodeAuth ||
		!strings.Contains(f.Err.Message, "ended at 2026-10-16T21:00:00Z") || f.Err.Fix == "" {
		t.Errorf("carol's proof after her key's end = %+v; want refused auth, saying when the key ended, with a fix", f)
	}
}

// endingControl is a control store whose keys end at the times in ends,
// on the bubble's clock, for the expiry timer's test.
type endingControl struct {
	fakeControl
	mu   sync.Mutex
	ends map[string]time.Time
}

func (c *endingControl) Now() time.Time { return time.Now().UTC() }

func (c *endingControl) KeyEnds() (ended []store.PrincipalKey, next time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for fp, end := range c.ends {
		switch {
		case !now.Before(end):
			ended = append(ended, store.PrincipalKey{Fingerprint: fp, Principal: "alice", ExpiresAt: &end})
		case next.IsZero() || end.Before(next):
			next = end
		}
	}
	return ended, next
}

func (c *endingControl) set(fp string, end time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ends[fp] = end
}

// The daemon closes a key's connections at its end by a timer, not at
// the next reaper tick; and a key added with an earlier end than the one
// the timer waits for is seen at once.
func TestKeyEndTimer(t *testing.T) {
	// Serial: a synctest bubble's virtual time takes no real time worth sharing.
	synctest.Test(t, func(t *testing.T) {
		ctl := &endingControl{fakeControl: fakeControl{members: map[string]bool{"alice": true}}, ends: map[string]time.Time{}}
		var log syncBuffer
		s, err := New(Config{Control: ctl, Open: func(context.Context, store.Project) (Store, error) { return &fakeStore{}, nil }, NoAudit: true,
			Version: "v0.2.0", ReapInterval: time.Hour, Logger: slogTo(&log), PeerCheck: anyPeer})
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		far, soon := fingerprintOf(t, "far"), fingerprintOf(t, "soon")
		ctl.set(far, start.Add(10*time.Minute))
		byFar, byFarEnded := keyConn(t, s, far)
		bySoon, bySoonEnded := keyConn(t, s, soon)
		defer func() { _ = byFar.Close(); _ = bySoon.Close() }()

		ctx, cancel := context.WithCancel(t.Context())
		s.wg.Go(func() { s.expireLoop(ctx) })
		defer func() { cancel(); s.wg.Wait() }()

		// The loop waits for far's end; soon is given an earlier one.
		time.Sleep(time.Minute)
		ctl.set(soon, start.Add(2*time.Minute))
		s.keysChanged()
		time.Sleep(time.Minute - time.Millisecond)
		synctest.Wait()
		if bySoonEnded.Load() || byFarEnded.Load() {
			t.Fatal("a connection closed before its key's end")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if !bySoonEnded.Load() {
			t.Error("the connection of a key added with an earlier end is open at that end")
		}
		time.Sleep(8*time.Minute - time.Millisecond)
		synctest.Wait()
		if byFarEnded.Load() {
			t.Fatal("far's connection closed before its end")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if !byFarEnded.Load() {
			t.Error("far's connection is open at its key's end, though the reaper's tick is an hour away")
		}
	})
}

// keyConn records a connection of alice's to projA signed in by the key
// fp, as a handshake would, and returns the client's end with a flag
// that is set once the server closes it.
func keyConn(t *testing.T, s *Server, fp string) (net.Conn, *atomic.Bool) {
	t.Helper()
	srv, cli := net.Pipe()
	s.mu.Lock()
	s.conns[srv] = owner{principal: "alice", project: projA, key: fp, transport: TransportSSH}
	s.mu.Unlock()
	var ended atomic.Bool
	go func() {
		_, _ = io.Copy(io.Discard, cli)
		ended.Store(true)
	}()
	return cli, &ended
}

// An ended key is told when it ended only once proved, as a disabled
// principal's is: its public key, which anyone may hold, gets a
// challenge, and a proof by another key is refused as any bad proof,
// naming no principal and no end (sshd says nothing of a key it
// refuses).
func TestHTTPSKeyEndedUnproved(t *testing.T) {
	t.Parallel()
	ck := &clock{t: endsAt}
	var u string
	var pool *x509.CertPool
	m := newMultiWith(t, Limits{}, ck, nil, func(c *Config) { u, pool = listenHTTPS(t, c, nil) }, dana.Principal)
	m.project(t, projA, "carol")
	if _, err := m.ctl.AddKeyEnding(t.Context(), dana, "carol", keyLine(t, "carol"), store.KeyEnd{In: time.Hour}); err != nil {
		t.Fatal(err)
	}
	m.refresh(t.Context())
	serving(t, m.Server)
	ck.advance(time.Hour)
	m.ExpireKeys(t.Context())

	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(keyproof.NewOffer(u, []ssh.PublicKey{signerOf(t, "carol").PublicKey()})); err != nil {
		t.Fatal(err)
	}
	msg, err := c.Read()
	if err != nil {
		t.Fatal(err)
	}
	if f := refusalIn(msg); f != nil {
		t.Fatalf("an offer of carol's ended key, unproved, was refused at once: %+v; want a challenge", f.Err)
	}
	ch, err := keyproof.ParseChallenge(msg)
	if err != nil {
		t.Fatalf("the answer to an offer of an ended key: %v (%q)", err, msg)
	}
	sig, err := keyproof.Sign(signerOf(t, "mallory"), u, ch.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(keyproof.NewProof(sig)); err != nil {
		t.Fatal(err)
	}
	f := refusalIn(mustRead(t, c))
	if f == nil || f.Err.Code != proto.CodeAuth || !strings.Contains(f.Err.Message, "does not verify") {
		t.Fatalf("a proof of carol's ended key by another key = %+v; want the bad proof's refusal", f)
	}
	if text := f.Err.Message + " " + f.Err.Fix; strings.Contains(text, "carol") || strings.Contains(text, "ended") || strings.Contains(text, "2026") {
		t.Errorf("an unproved ended key's refusal says %q; it names the principal or the end", text)
	}
}

// mustRead reads one message on c.
func mustRead(t *testing.T, c interface{ Read() ([]byte, error) }) []byte {
	t.Helper()
	m, err := c.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return m
}
