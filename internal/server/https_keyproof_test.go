package server

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/wsconn"
)

// readRefusal reads the next message on c and returns it as a refusing
// welcome, failing t when it is not one.
func readRefusal(t *testing.T, c *wsconn.Conn, after string) *proto.Frame {
	t.Helper()
	m, err := c.Read()
	f := refusalIn(m)
	if f == nil {
		t.Fatalf("after %s: read %q, %v; want a refusal", after, m, err)
	}
	return f
}

// challenge sends an offer of keys for the server at offered on c and
// returns the server's challenge, failing t on a refusal.
func challenge(t *testing.T, c *wsconn.Conn, offered string, keys ...ssh.PublicKey) keyproof.Challenge {
	t.Helper()
	if err := c.Write(keyproof.NewOffer(offered, keys)); err != nil {
		t.Fatalf("send the key offer: %v", err)
	}
	m, err := c.Read()
	if err != nil {
		t.Fatalf("read the challenge: %v", err)
	}
	if f := refusalIn(m); f != nil {
		t.Fatalf("the key offer was refused: %v", f.Err)
	}
	ch, err := keyproof.ParseChallenge(m)
	if err != nil {
		t.Fatalf("the challenge: %v (%q)", err, m)
	}
	return ch
}

// A proof is good for the connection whose nonce it signed, and no
// other: one captured from a connection alice proved is refused on the
// next, which the server challenges with a nonce of its own.
func TestHTTPSProofReplay(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	alice := signerOf(t, "alice")
	c1, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch1 := challenge(t, c1, u, alice.PublicKey())
	captured, err := keyproof.Sign(alice, u, ch1.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := c1.Write(keyproof.NewProof(captured)); err != nil {
		t.Fatal(err)
	}
	if w := hello(t, c1); w.Err != nil || w.Principal != "alice" {
		t.Fatalf("alice's own proof = %+v", w)
	}

	c2, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch2 := challenge(t, c2, u, alice.PublicKey())
	if string(ch2.Nonce) == string(ch1.Nonce) {
		t.Fatal("two connections were challenged with the same nonce")
	}
	if err := c2.Write(keyproof.NewProof(captured)); err != nil {
		t.Fatal(err)
	}
	if f := readRefusal(t, c2, "a replayed proof"); f.Err.Code != proto.CodeAuth || !strings.Contains(f.Err.Message, "does not verify") || f.Principal != "" {
		t.Errorf("a proof replayed on another connection = %+v; want refused auth, naming no one", f.Err)
	}
}

// The server challenges the first key of the offer it knows, by its
// index; a client that proves another key of its offer is refused.
func TestHTTPSChallengesKnownKey(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	mallory, alice := signerOf(t, "mallory"), signerOf(t, "alice")
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch := challenge(t, c, u, mallory.PublicKey(), alice.PublicKey())
	if ch.Key != 1 {
		t.Fatalf("the challenge names key %d of [mallory, alice], want 1", ch.Key)
	}
	sig, err := keyproof.Sign(alice, u, ch.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(keyproof.NewProof(sig)); err != nil {
		t.Fatal(err)
	}
	if w := hello(t, c); w.Err != nil || w.Principal != "alice" {
		t.Errorf("alice's proof of the second key offered = %+v", w)
	}

	// mallory's key signs for the challenge to alice's: refused.
	c2, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch = challenge(t, c2, u, mallory.PublicKey(), alice.PublicKey())
	if sig, err = keyproof.Sign(mallory, u, ch.Nonce); err != nil {
		t.Fatal(err)
	}
	if err := c2.Write(keyproof.NewProof(sig)); err != nil {
		t.Fatal(err)
	}
	if f := readRefusal(t, c2, "a proof by the key not challenged"); f.Err.Code != proto.CodeAuth {
		t.Errorf("a proof by the key not challenged = %+v; want refused auth", f.Err)
	}
}

// What the request says of where it came from names no URL: a proof
// for the host X-Forwarded-Host or Forwarded claims is refused, and a
// proof for the configured URL is taken whatever they say.
func TestHTTPSForwardedHeadersIgnored(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	h := http.Header{
		"X-Forwarded-Host":  {"aries.example.com"},
		"X-Forwarded-Proto": {"https"},
		"Forwarded":         {"host=aries.example.com;proto=https"},
	}
	c, _, err := wsDial(t, u, pool, h)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c, "https://aries.example.com", signerOf(t, "alice"), nil); f == nil || f.Err.Code != proto.CodeAuth ||
		!strings.Contains(f.Err.Message, "aries.example.com") {
		t.Errorf("a proof for the forwarded host = %+v; want refused auth", f)
	}
	c2, _, err := wsDial(t, u, pool, h)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c2, u, signerOf(t, "alice"), nil); f != nil {
		t.Fatalf("a proof for the configured URL with forwarded headers: %v", f.Err)
	}
	if w := hello(t, c2); w.Err != nil || w.Principal != "alice" {
		t.Errorf("alice with forwarded headers = %+v", w)
	}
}

// After the proof, the next message must be the hello: a second offer,
// made to prove another key on a connection already proved, is refused
// and closes the connection.
func TestHTTPSNothingButHelloAfterProof(t *testing.T) {
	t.Parallel()
	_, u, pool := httpsServer(t, nil)
	c, _, err := wsDial(t, u, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f := prove(t, c, u, signerOf(t, "alice"), nil); f != nil {
		t.Fatalf("alice's proof: %v", f.Err)
	}
	if err := c.Write(keyproof.NewOffer(u, []ssh.PublicKey{signerOf(t, "mallory").PublicKey()})); err != nil {
		t.Fatal(err)
	}
	if f := readRefusal(t, c, "a second offer"); f.Err.Code != proto.CodeInvalid || f.Principal != "" {
		t.Errorf("a second offer in place of the hello = %+v; want it refused invalid, naming no one", f.Err)
	}
	if _, err := c.Read(); err == nil {
		t.Error("the connection stayed open after a second offer")
	}
}
