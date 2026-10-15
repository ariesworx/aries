// Package dpop is OAuth 2.0 Demonstrating Proof of Possession (RFC 9449)
// for the spike, with ES256 only and the standard library alone: the
// client signs a short proof for each HTTP request with a key it keeps,
// and the server checks the proof and that the access token is bound to
// that key's thumbprint. A token copied off the machine, out of a log or
// a proxy, is then useless without the key.
package dpop

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strings"
	"sync"
	"time"
)

var b64 = base64.RawURLEncoding

// Skew is how far a proof's iat may be from the server's clock.
const Skew = 60 * time.Second

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func pubJWK(k *ecdsa.PublicKey) jwk {
	b, _ := k.Bytes() // uncompressed: 0x04 ‖ X ‖ Y
	return jwk{Kty: "EC", Crv: "P-256", X: b64.EncodeToString(b[1:33]), Y: b64.EncodeToString(b[33:])}
}

// Thumbprint is the key's JWK SHA-256 thumbprint (RFC 7638), which a
// bound token records as cnf.jkt.
func Thumbprint(k *ecdsa.PublicKey) string { return thumb(pubJWK(k)) }

func thumb(j jwk) string {
	// Members in lexicographic order, no whitespace (RFC 7638 §3).
	s := fmt.Sprintf(`{"crv":"%s","kty":"%s","x":"%s","y":"%s"}`, j.Crv, j.Kty, j.X, j.Y)
	h := sha256.Sum256([]byte(s))
	return b64.EncodeToString(h[:])
}

type header struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	JWK jwk    `json:"jwk"`
}

type claims struct {
	JTI string `json:"jti"`
	HTM string `json:"htm"`
	HTU string `json:"htu"`
	IAT int64  `json:"iat"`
	ATH string `json:"ath,omitempty"`
}

func ath(token string) string {
	if token == "" {
		return ""
	}
	h := sha256.Sum256([]byte(token))
	return b64.EncodeToString(h[:])
}

// Proof makes a proof for one request: method htm to htu, presenting
// token (empty at the token endpoint, where there is none yet).
func Proof(key *ecdsa.PrivateKey, htm, htu, token string, at time.Time) (string, error) {
	h, err := json.Marshal(header{Typ: "dpop+jwt", Alg: "ES256", JWK: pubJWK(&key.PublicKey)})
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims{JTI: rand.Text(), HTM: htm, HTU: htu, IAT: at.Unix(), ATH: ath(token)})
	if err != nil {
		return "", err
	}
	signed := b64.EncodeToString(h) + "." + b64.EncodeToString(c)
	sum := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signed + "." + b64.EncodeToString(sig), nil
}

// Verifier checks proofs, and remembers each proof's jti for the window
// in which its iat is accepted, so that a proof is used once. It holds
// at most max of them (rule 18); past that a proof is refused, busy,
// until the oldest age out.
type Verifier struct {
	max    int
	window time.Duration

	mu   sync.Mutex
	seen map[string]time.Time // jti → when it may be forgotten
}

// NewVerifier returns a Verifier holding at most max proofs, each for
// window (twice Skew covers a proof from the past and one from the
// future).
func NewVerifier(max int, window time.Duration) *Verifier {
	return &Verifier{max: max, window: window, seen: make(map[string]time.Time)}
}

// Verify checks proof for a request of method htm to htu that presents
// token, at the server's time now, and returns the thumbprint of the key
// that signed it. The caller compares it with the token's binding.
func (v *Verifier) Verify(proof, htm, htu, token string, now time.Time) (string, error) {
	if len(proof) > 4<<10 {
		return "", errors.New("dpop proof over 4 KiB")
	}
	hs, rest, ok1 := strings.Cut(proof, ".")
	cs, ss, ok2 := strings.Cut(rest, ".")
	if !ok1 || !ok2 {
		return "", errors.New("dpop proof is not a compact JWT")
	}
	var h header
	var c claims
	hb, err1 := b64.Strict().DecodeString(hs)
	cb, err2 := b64.Strict().DecodeString(cs)
	sig, err3 := b64.Strict().DecodeString(ss)
	if err := errors.Join(err1, err2, err3); err != nil {
		return "", fmt.Errorf("dpop proof: %w", err)
	}
	if err := json.Unmarshal(hb, &h); err != nil {
		return "", fmt.Errorf("dpop proof header: %w", err)
	}
	if err := json.Unmarshal(cb, &c); err != nil {
		return "", fmt.Errorf("dpop proof claims: %w", err)
	}
	if h.Typ != "dpop+jwt" || h.Alg != "ES256" || h.JWK.Kty != "EC" || h.JWK.Crv != "P-256" {
		return "", errors.New("dpop proof: typ, alg or key not accepted (dpop+jwt, ES256, P-256)")
	}
	x, err1 := b64.Strict().DecodeString(h.JWK.X)
	y, err2 := b64.Strict().DecodeString(h.JWK.Y)
	if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 || len(sig) != 64 {
		return "", errors.New("dpop proof: bad key or signature length")
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return "", fmt.Errorf("dpop proof key: %w", err)
	}
	sum := sha256.Sum256([]byte(hs + "." + cs))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return "", errors.New("dpop proof: bad signature")
	}
	if c.HTM != htm {
		return "", fmt.Errorf("dpop proof htm %q, want %q", c.HTM, htm)
	}
	if !sameURL(c.HTU, htu) {
		return "", fmt.Errorf("dpop proof htu %q, want %q", c.HTU, htu)
	}
	if c.ATH != ath(token) {
		return "", errors.New("dpop proof ath does not match the access token")
	}
	iat := time.Unix(c.IAT, 0)
	if iat.Before(now.Add(-Skew)) || iat.After(now.Add(Skew)) {
		return "", fmt.Errorf("dpop proof iat %s is more than %s from the server's time", iat.UTC().Format(time.RFC3339), Skew)
	}
	if c.JTI == "" || len(c.JTI) > 64 {
		return "", errors.New("dpop proof: jti missing or over 64 bytes")
	}
	if err := v.spend(c.JTI, now); err != nil {
		return "", err
	}
	return thumb(h.JWK), nil
}

func (v *Verifier) spend(jti string, now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if until, ok := v.seen[jti]; ok && now.Before(until) {
		return errors.New("dpop proof replayed: jti already seen")
	}
	if len(v.seen) >= v.max {
		for k, until := range v.seen {
			if !now.Before(until) {
				delete(v.seen, k)
			}
		}
		if len(v.seen) >= v.max {
			return errors.New("busy: too many dpop proofs in the window; retry in a minute")
		}
	}
	v.seen[jti] = now.Add(v.window)
	return nil
}

// sameURL compares htu as RFC 9449 §4.3 says: without query and fragment.
func sameURL(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) && ua.Path == ub.Path
}
