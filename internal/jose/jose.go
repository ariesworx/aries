// Package jose reads and writes the compact JSON Web Signatures (RFC
// 7515) that sign-in needs, with the standard library alone: the ID
// tokens identity providers sign (internal/oidc), and the DPoP proofs
// aries signs for each token it presents (internal/dpop).
//
// It knows two algorithms, RS256 and ES256, which between them cover
// every provider the design names (docs/design/identity.md §6.4); any
// other is refused, "none" and the HMAC family above all, and so is any
// critical header extension. A key is used only under the algorithm its
// type is for, and an RSA key under 2,048 bits not at all.
package jose

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// The algorithms accepted.
const (
	AlgRS256 = "RS256"
	AlgES256 = "ES256"
)

// MaxToken bounds the size of a token this package looks at. Real ID
// tokens are 1 to 3 KiB; Entra's with many groups can pass 8.
const MaxToken = 16 << 10

// minRSABits is the smallest RSA modulus accepted.
const minRSABits = 2048

// ErrMalformed is wrapped by every refusal of Parse: what was given is
// not a compact JWS this package accepts.
var ErrMalformed = errors.New("malformed JWS")

// Header is the protected header of a JWS, as far as a verifier needs
// it. JWK is the public key a DPoP proof carries (RFC 9449 §4.2).
type Header struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid,omitempty"`
	Typ  string   `json:"typ,omitempty"`
	JWK  *JWK     `json:"jwk,omitempty"`
	Crit []string `json:"crit,omitempty"`
}

// JWS is a parsed compact serialization. Payload is not to be trusted
// until Verify has returned nil.
type JWS struct {
	Header  Header
	Payload []byte
	signed  string // header.payload, as sent
	sig     []byte
}

func malformed(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrMalformed}, a...)...)
}

// Parse splits a compact JWS and decodes its header. It checks only
// form: three parts of unpadded base64url, a header that is one JSON
// object naming an accepted algorithm, no critical extensions, and a
// signature. Every refusal wraps ErrMalformed.
func Parse(token string) (*JWS, error) {
	if len(token) > MaxToken {
		return nil, malformed("a token of %d bytes is over %d", len(token), MaxToken)
	}
	h, rest, ok1 := strings.Cut(token, ".")
	p, s, ok2 := strings.Cut(rest, ".")
	if !ok1 || !ok2 || strings.Contains(s, ".") {
		return nil, malformed("not the three parts of a compact JWS")
	}
	hb, err := b64(h)
	if err != nil {
		return nil, malformed("header: %v", err)
	}
	var j JWS
	dec := json.NewDecoder(bytes.NewReader(hb))
	if err := dec.Decode(&j.Header); err != nil {
		return nil, malformed("header: %v", err)
	}
	if dec.More() {
		return nil, malformed("header: data after the object")
	}
	switch j.Header.Alg {
	case AlgRS256, AlgES256:
	default:
		return nil, malformed("algorithm %q is not accepted (RS256 or ES256)", j.Header.Alg)
	}
	if j.Header.Crit != nil {
		return nil, malformed("critical header extensions are not accepted")
	}
	if j.Payload, err = b64(p); err != nil {
		return nil, malformed("payload: %v", err)
	}
	if j.sig, err = b64(s); err != nil || len(j.sig) == 0 {
		return nil, malformed("signature: not base64url, or empty")
	}
	j.signed = token[:len(h)+1+len(p)]
	return &j, nil
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Verify checks the signature under key, which must be of the kind the
// header's algorithm names: *rsa.PublicKey for RS256, a P-256
// *ecdsa.PublicKey for ES256. The algorithm comes from the header and the
// key's type must agree with it, so a key cannot be used under an
// algorithm it was not published for.
func (j *JWS) Verify(key crypto.PublicKey) error {
	digest := sha256.Sum256([]byte(j.signed))
	switch j.Header.Alg {
	case AlgRS256:
		k, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("RS256 needs an RSA key, not %T", key)
		}
		if k.N.BitLen() < minRSABits {
			return fmt.Errorf("an RSA key of %d bits is under %d", k.N.BitLen(), minRSABits)
		}
		if rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], j.sig) != nil {
			return errors.New("bad signature")
		}
		return nil
	case AlgES256:
		k, ok := key.(*ecdsa.PublicKey)
		if !ok || k.Curve != elliptic.P256() {
			return fmt.Errorf("ES256 needs a P-256 key, not %T", key)
		}
		// A JWS carries R||S, 32 bytes each (RFC 7518 §3.4), not ASN.1.
		if len(j.sig) != 64 {
			return fmt.Errorf("an ES256 signature of %d bytes, not 64", len(j.sig))
		}
		r := new(big.Int).SetBytes(j.sig[:32])
		s := new(big.Int).SetBytes(j.sig[32:])
		if !ecdsa.Verify(k, digest[:], r, s) {
			return errors.New("bad signature")
		}
		return nil
	}
	return fmt.Errorf("algorithm %q is not accepted", j.Header.Alg)
}

// Sign returns claims, marshaled as JSON, signed by key in the compact
// form: RS256 for an *rsa.PrivateKey, ES256 for a P-256
// *ecdsa.PrivateKey. h.Alg is set from the key; the rest of h is sent as
// given.
func Sign(key crypto.Signer, h Header, claims any) (string, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		h.Alg = AlgRS256
	case *ecdsa.PrivateKey:
		if k.Curve != elliptic.P256() {
			return "", errors.New("ES256 signs with a P-256 key")
		}
		h.Alg = AlgES256
	default:
		return "", fmt.Errorf("cannot sign with a %T", key)
	}
	hb, err := json.Marshal(h)
	if err != nil {
		return "", fmt.Errorf("sign: header: %w", err)
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("sign: claims: %w", err)
	}
	return sign(enc(hb)+"."+enc(cb), key)
}

// sign appends the signature of signed, header.payload, under key.
func sign(signed string, key crypto.Signer) (string, error) {
	digest := sha256.Sum256([]byte(signed))
	var sig []byte
	switch k := key.(type) {
	case *rsa.PrivateKey:
		s, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, digest[:])
		if err != nil {
			return "", fmt.Errorf("sign: %w", err)
		}
		sig = s
	case *ecdsa.PrivateKey:
		r, s, err := ecdsa.Sign(rand.Reader, k, digest[:])
		if err != nil {
			return "", fmt.Errorf("sign: %w", err)
		}
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	default:
		return "", fmt.Errorf("cannot sign with a %T", key)
	}
	return signed + "." + enc(sig), nil
}

// JWK is one key of a JSON Web Key Set (RFC 7517), as far as RS256 and
// ES256 need.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n,omitempty"`
	E   string `json:"e,omitempty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
}

// Set is a JSON Web Key Set.
type Set struct {
	Keys []JWK `json:"keys"`
}

// PublicJWK describes key, an *rsa.PublicKey or a P-256
// *ecdsa.PublicKey, as a JWK with id kid (empty for none).
func PublicJWK(key crypto.PublicKey, kid string) (JWK, error) {
	switch k := key.(type) {
	case *rsa.PublicKey:
		return JWK{Kty: "RSA", Kid: kid, N: enc(k.N.Bytes()), E: enc(big.NewInt(int64(k.E)).Bytes())}, nil
	case *ecdsa.PublicKey:
		b, err := k.Bytes() // 0x04 ‖ X ‖ Y for P-256
		if err != nil || k.Curve != elliptic.P256() {
			return JWK{}, errors.New("a JWK here is of a P-256 key")
		}
		return JWK{Kty: "EC", Kid: kid, Crv: "P-256", X: enc(b[1:33]), Y: enc(b[33:])}, nil
	}
	return JWK{}, fmt.Errorf("no JWK for a %T", key)
}

// PublicKey returns the key a JWK describes, for signing keys only. A
// key marked for encryption ("use":"enc") is refused, as Keycloak
// publishes one beside its signing keys, and so is a key whose "alg"
// names another algorithm than its type is for.
func (k JWK) PublicKey() (crypto.PublicKey, error) {
	if k.Use != "" && k.Use != "sig" {
		return nil, fmt.Errorf("key %q is for %q, not signing", k.Kid, k.Use)
	}
	switch k.Kty {
	case "RSA":
		if k.Alg != "" && k.Alg != AlgRS256 {
			return nil, fmt.Errorf("key %q: an RSA key for %q, not RS256", k.Kid, k.Alg)
		}
		n, err := b64(k.N)
		if err != nil {
			return nil, fmt.Errorf("key %q: n: %w", k.Kid, err)
		}
		e, err := b64(k.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, fmt.Errorf("key %q: bad exponent", k.Kid)
		}
		exp := 0
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		if exp < 3 || exp%2 == 0 {
			return nil, fmt.Errorf("key %q: bad exponent", k.Kid)
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		if pub.N.BitLen() < minRSABits {
			return nil, fmt.Errorf("key %q: an RSA key of %d bits is under %d", k.Kid, pub.N.BitLen(), minRSABits)
		}
		return pub, nil
	case "EC":
		if k.Alg != "" && k.Alg != AlgES256 {
			return nil, fmt.Errorf("key %q: an EC key for %q, not ES256", k.Kid, k.Alg)
		}
		if k.Crv != "P-256" {
			return nil, fmt.Errorf("key %q: curve %q is not accepted", k.Kid, k.Crv)
		}
		x, err1 := b64(k.X)
		y, err2 := b64(k.Y)
		if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
			return nil, fmt.Errorf("key %q: bad point", k.Kid)
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", k.Kid, err)
		}
		return pub, nil
	}
	return nil, fmt.Errorf("key %q: type %q is not accepted", k.Kid, k.Kty)
}

// Thumbprint is the key's JWK SHA-256 thumbprint (RFC 7638), base64url:
// the required members only, in lexicographic order, with no
// whitespace. A DPoP-bound token records its key's (RFC 9449 §6).
func (k JWK) Thumbprint() string {
	var s string
	switch k.Kty {
	case "EC":
		s = fmt.Sprintf(`{"crv":%q,"kty":"EC","x":%q,"y":%q}`, k.Crv, k.X, k.Y)
	default:
		s = fmt.Sprintf(`{"e":%q,"kty":%q,"n":%q}`, k.E, k.Kty, k.N)
	}
	h := sha256.Sum256([]byte(s))
	return enc(h[:])
}
