// Package dpop is OAuth 2.0 Demonstrating Proof of Possession (RFC
// 9449), with ES256 alone and the standard library: aries signs a short
// proof for each request that presents its token, with a P-256 key it
// keeps, and ariesd checks the proof and that the token is bound to that
// key's thumbprint (docs/design/transport.md §6.4). A token copied off
// the machine, out of a log, a proxy or a backup, is useless without
// the key.
//
// The server remembers each proof's jti for the window in which its iat
// is accepted, so that a proof is used once; the memory is bounded
// (rule 18) and is the daemon's own: several daemons in front of one
// database would each remember their own (transport.md §13).
package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/jose"
)

// Skew is how far a proof's iat may be from the server's time, either
// way.
const Skew = time.Minute

// Window is how long a proof is remembered: every iat Skew would
// accept.
const Window = 2 * Skew

// MaxProof bounds the size of a proof.
const MaxProof = 4 << 10

// maxJTI bounds a proof's jti.
const maxJTI = 64

// typ is the type a proof's header names (RFC 9449 §4.2).
const typ = "dpop+jwt"

// Reason says why a proof was refused, for the log and the counts; the
// message says it to a person.
type Reason string

// The reasons a proof is refused.
const (
	ReasonMissing   Reason = "missing"   // no proof
	ReasonMalformed Reason = "malformed" // not a proof of this form
	ReasonSignature Reason = "signature" // not signed by the key it carries
	ReasonRequest   Reason = "request"   // for another method or URL
	ReasonToken     Reason = "token"     // for another access token (ath)
	ReasonTime      Reason = "time"      // iat too far from the server's time
	ReasonReplayed  Reason = "replayed"  // its jti was seen in the window
	ReasonBusy      Reason = "busy"      // the proofs remembered are at their bound
	ReasonKey       Reason = "key"       // signed by another key than the token is bound to
)

// Error refuses a proof.
type Error struct {
	Reason Reason
	msg    string
}

func (e *Error) Error() string { return "dpop proof: " + e.msg }

func refuse(r Reason, format string, a ...any) *Error {
	return &Error{Reason: r, msg: fmt.Sprintf(format, a...)}
}

// NewKey makes a P-256 key for proofs.
func NewKey() (*ecdsa.PrivateKey, error) { return ecdsa.GenerateKey(elliptic.P256(), rand.Reader) }

// MarshalKey is k in PKCS #8 PEM, as the key's file holds it.
func MarshalKey(k *ecdsa.PrivateKey) ([]byte, error) {
	b, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), nil
}

// ParseKey reads a P-256 key MarshalKey wrote.
func ParseKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "PRIVATE KEY" {
		return nil, errors.New("the DPoP key is not a PKCS #8 private key in PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the DPoP key: %w", err)
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok || ek.Curve != elliptic.P256() {
		return nil, errors.New("the DPoP key is not a P-256 key")
	}
	return ek, nil
}

// Thumbprint is the JWK thumbprint (RFC 7638) of pub, which a bound
// token records as its key (cnf.jkt).
func Thumbprint(pub *ecdsa.PublicKey) string {
	j, err := jose.PublicJWK(pub, "")
	if err != nil {
		return ""
	}
	return j.Thumbprint()
}

type claims struct {
	JTI string `json:"jti"`
	HTM string `json:"htm"`
	HTU string `json:"htu"`
	IAT int64  `json:"iat"`
	ATH string `json:"ath,omitempty"`
}

// ath is the access token's hash a proof carries (RFC 9449 §4.2).
func ath(token string) string {
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// Proof makes a proof for one request: method to htu, the request's URL
// without query, presenting token (empty at the token endpoint, where
// there is none yet), at the time at.
func Proof(key *ecdsa.PrivateKey, method, htu, token string, at time.Time) (string, error) {
	j, err := jose.PublicJWK(&key.PublicKey, "")
	if err != nil {
		return "", err
	}
	return jose.Sign(key, jose.Header{Typ: typ, JWK: &j}, claims{JTI: rand.Text(), HTM: method, HTU: htu, IAT: at.Unix(), ATH: ath(token)})
}

// Verifier checks proofs, and remembers each one's jti for Window so
// that a proof is used once. It holds at most a bounded number; past
// that a proof is refused, busy, until the oldest age out. It is safe
// for concurrent use.
type Verifier struct {
	max int

	mu   sync.Mutex
	seen map[string]time.Time // jti → when it may be forgotten
}

// NewVerifier returns a Verifier that remembers at most most proofs.
func NewVerifier(most int) *Verifier {
	return &Verifier{max: most, seen: map[string]time.Time{}}
}

// Len is how many proofs are remembered.
func (v *Verifier) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.seen)
}

// Verify checks proof for a request of method to one of htus (each a URL
// without query: the server's own URLs and the path, never the request's
// Host), presenting token, at the server's time now, and returns the
// thumbprint of the key that signed it, which the caller compares with
// the token's. A refusal is an *Error. Only a proof that passes every
// other check is remembered.
func (v *Verifier) Verify(proof, method string, htus []string, token string, now time.Time) (string, error) {
	switch {
	case proof == "":
		return "", refuse(ReasonMissing, "none was sent")
	case len(proof) > MaxProof:
		return "", refuse(ReasonMalformed, "over %d bytes", MaxProof)
	}
	j, err := jose.Parse(proof)
	if err != nil {
		return "", refuse(ReasonMalformed, "%v", err)
	}
	h := j.Header
	if h.Typ != typ || h.Alg != jose.AlgES256 || h.JWK == nil || h.JWK.Kty != "EC" {
		return "", refuse(ReasonMalformed, "typ, alg or key not accepted: dpop+jwt, ES256 and a P-256 key")
	}
	pub, err := h.JWK.PublicKey()
	if err != nil {
		return "", refuse(ReasonMalformed, "its key: %v", err)
	}
	if err := j.Verify(pub); err != nil {
		return "", refuse(ReasonSignature, "%v", err)
	}
	var c claims
	if err := json.Unmarshal(j.Payload, &c); err != nil {
		return "", refuse(ReasonMalformed, "claims: %v", err)
	}
	if c.JTI == "" || len(c.JTI) > maxJTI {
		return "", refuse(ReasonMalformed, "jti missing or over %d bytes", maxJTI)
	}
	if c.HTM != method {
		return "", refuse(ReasonRequest, "for %q, not %s", c.HTM, method)
	}
	if u, ok := normalize(c.HTU); !ok || !slices.Contains(htus, u) {
		return "", refuse(ReasonRequest, "for another URL than this server's %s", strings.Join(htus, " or "))
	}
	if c.ATH != ath(token) {
		return "", refuse(ReasonToken, "not made for the token it comes with")
	}
	iat := time.Unix(c.IAT, 0)
	if iat.Before(now.Add(-Skew)) || iat.After(now.Add(Skew)) {
		return "", refuse(ReasonTime, "made at %s, more than %s from the server's time %s; check this machine's clock",
			iat.UTC().Format(time.RFC3339), Skew, now.UTC().Format(time.RFC3339))
	}
	if err := v.spend(c.JTI, now); err != nil {
		return "", err
	}
	return h.JWK.Thumbprint(), nil
}

func (v *Verifier) spend(jti string, now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if until, ok := v.seen[jti]; ok && now.Before(until) {
		return refuse(ReasonReplayed, "used already")
	}
	if len(v.seen) >= v.max {
		for k, until := range v.seen {
			if !now.Before(until) {
				delete(v.seen, k)
			}
		}
		if len(v.seen) >= v.max {
			return refuse(ReasonBusy, "the server holds %d proofs from the last %s, the most it may", v.max, Window)
		}
	}
	v.seen[jti] = now.Add(Window)
	return nil
}

// Prune forgets the proofs whose window has passed at now.
func (v *Verifier) Prune(now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, until := range v.seen {
		if !now.Before(until) {
			delete(v.seen, k)
		}
	}
}

// normalize is htu as the server's URLs are written: https, the host in
// lowercase without the default port, and the path, with no query or
// fragment (RFC 9449 §4.3).
func normalize(htu string) (string, bool) {
	u, err := url.Parse(htu)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && p != "443" {
		host += ":" + p
	}
	return "https://" + host + u.EscapedPath(), true
}
