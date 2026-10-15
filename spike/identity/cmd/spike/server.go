package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/spike/identity/oidc"
)

// enroller stands in for ariesd's side of `aries login` (docs/design/identity.md §5.3):
// it issues a challenge, checks the token it is handed with the new
// public key, finds or binds the person's identity, and answers with the
// keys-file line it would write. It is in the same process as the client
// here; in the design it is reached over the pre-authentication path.
type enroller struct {
	v        *oidc.Verifier
	now      func() time.Time
	lifetime time.Duration
	authAge  time.Duration
	// fresh bounds a device-grant token's age, since it is not bound
	// to the key (decision 55: accepted once, and only fresh).
	fresh time.Duration
	// bridge is the forced command's program.
	bridge string

	mu         sync.Mutex
	invites    map[string]string    // invited address -> principal
	bound      map[[2]string]string // (issuer, subject) -> principal
	challenges map[string]time.Time // issued, each spent once
	seen       map[[32]byte]bool    // unbound tokens already accepted
}

func newEnroller(v *oidc.Verifier, now func() time.Time) *enroller {
	return &enroller{
		v: v, now: now, lifetime: 24 * time.Hour, authAge: 10 * time.Minute, fresh: 2 * time.Minute,
		bridge:     "/usr/local/bin/ariesd",
		invites:    map[string]string{"alice@example.com": "alice"},
		bound:      map[[2]string]string{},
		challenges: map[string]time.Time{},
		seen:       map[[32]byte]bool{},
	}
}

// challenge issues a single-use challenge, valid for ten minutes.
func (e *enroller) challenge() string {
	c := rand.Text()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.challenges[c] = e.now().Add(10 * time.Minute)
	return c
}

// nonceFor is what the client sends as the nonce, and what the server
// expects back in the token: a commitment to the server's challenge and
// the new key, so the token is good for that key at this server, once.
func nonceFor(challenge string, pub ssh.PublicKey) string {
	h := sha256.New()
	h.Write([]byte("aries-login-v1\x00"))
	h.Write([]byte(challenge))
	h.Write([]byte{0})
	h.Write(pub.Marshal())
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// result is what an enrollment answers.
type result struct {
	Principal string
	Line      string
	Expires   time.Time
	Bound     bool // the identity was bound by this sign-in
	Claims    *oidc.Claims
}

// enroll checks token for pub under challenge. bound says the flow
// carried the nonce (loopback, paste); the device grant cannot.
func (e *enroller) enroll(ctx context.Context, token string, pub ssh.PublicKey, challenge string, bound bool) (*result, error) {
	e.mu.Lock()
	until, ok := e.challenges[challenge]
	delete(e.challenges, challenge)
	e.mu.Unlock()
	if !ok || e.now().After(until) {
		return nil, errors.New("unknown or spent challenge: run aries login again")
	}
	want := oidc.Expect{AuthAge: e.authAge}
	if bound {
		want.Nonce = nonceFor(challenge, pub)
	}
	c, err := e.v.Verify(ctx, token, want)
	if err != nil {
		return nil, err
	}
	if !bound {
		if e.now().Sub(time.Unix(c.Iat, 0)) > e.fresh {
			return nil, fmt.Errorf("an unbound token must be used within %s of its issue", e.fresh)
		}
		sum := sha256.Sum256([]byte(token))
		e.mu.Lock()
		dup := e.seen[sum]
		e.seen[sum] = true
		e.mu.Unlock()
		if dup {
			return nil, errors.New("this token was already used")
		}
	}
	principal, newly, err := e.identity(c)
	if err != nil {
		return nil, err
	}
	exp := e.now().Add(e.lifetime).Truncate(time.Second)
	return &result{
		Principal: principal, Line: keyLine(e.bridge, principal, pub, exp), Expires: exp, Bound: newly, Claims: c,
	}, nil
}

// identity finds the principal for the token's (issuer, subject), or binds
// an invitation by its verified address on the first sign-in (decision
// 57). After binding, only the subject is matched.
func (e *enroller) identity(c *oidc.Claims) (string, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id := [2]string{c.Iss, c.Sub}
	if p, ok := e.bound[id]; ok {
		return p, false, nil
	}
	email, _ := c.All["email"].(string)
	verified, _ := c.All["email_verified"].(bool)
	p, invited := e.invites[email]
	if !invited || !verified {
		return "", false, errors.New("no invitation for this identity: ask an admin to invite you")
	}
	for k, q := range e.bound {
		if q == p && k[0] == c.Iss {
			return "", false, errors.New("the invitation is bound to another subject at this issuer")
		}
	}
	e.bound[id] = p
	delete(e.invites, email)
	return p, true, nil
}

// keyLine is the line ariesd would write to its keys file: today's line
// with expiry-time in UTC, marked Z so that sshd does not read it in the
// server's own zone (RESULTS.md, section 4).
func keyLine(bridge, principal string, pub ssh.PublicKey, exp time.Time) string {
	return fmt.Sprintf("restrict,expiry-time=%q,command=\"%s stdio --principal %s --key %s\" %s",
		exp.UTC().Format("20060102150405")+"Z", bridge, principal, ssh.FingerprintSHA256(pub),
		trimNL(ssh.MarshalAuthorizedKey(pub)))
}

func trimNL(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		return string(b[:n-1])
	}
	return string(b)
}
