// Package jose verifies the compact JSON Web Signatures that OpenID
// Connect providers and workload platforms sign their tokens with, using
// the standard library alone. It knows two algorithms, RS256 and ES256,
// which between them cover every issuer the design names (Google, Entra
// ID, Okta, Keycloak, GitHub Actions, Google Cloud and Azure); anything
// else is refused, "none" and the HMAC family above all.
//
// It is the spike's answer to the dependency question (RESULTS.md): the
// whole of the signature check is this file.
package jose

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// MaxToken bounds the size of a token this package will look at. Real ID
// tokens are 1 to 3 KiB; Entra's with many groups can pass 8.
const MaxToken = 16 << 10

// minRSABits is the smallest RSA modulus accepted.
const minRSABits = 2048

// Header is the protected header of a JWS, as far as a verifier needs it.
type Header struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Typ  string   `json:"typ"`
	Crit []string `json:"crit"`
}

// JWS is a parsed compact serialization. Payload is not to be trusted
// until Verify has returned nil.
type JWS struct {
	Header  Header
	Payload []byte
	signed  []byte // header.payload, as sent
	sig     []byte
}

// Parse splits a compact JWS and decodes its header. It checks only
// form: three parts of unpadded base64url, a header that is a JSON
// object naming a known algorithm, and no critical extensions.
func Parse(token string) (*JWS, error) {
	if len(token) > MaxToken {
		return nil, fmt.Errorf("token of %d bytes is over %d", len(token), MaxToken)
	}
	h, rest, ok := strings.Cut(token, ".")
	if !ok {
		return nil, errors.New("not a compact JWS: no '.'")
	}
	p, s, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(s, ".") {
		return nil, errors.New("not a compact JWS: not three parts")
	}
	hb, err := b64(h)
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	var j JWS
	dec := json.NewDecoder(bytes.NewReader(hb))
	if err := dec.Decode(&j.Header); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if dec.More() {
		return nil, errors.New("header: trailing data")
	}
	switch j.Header.Alg {
	case "RS256", "ES256":
	default:
		return nil, fmt.Errorf("algorithm %q is not accepted", j.Header.Alg)
	}
	if len(j.Header.Crit) > 0 {
		return nil, errors.New("critical header extensions are not supported")
	}
	if j.Payload, err = b64(p); err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	if j.sig, err = b64(s); err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	j.signed = []byte(token[:len(h)+1+len(p)])
	return &j, nil
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.Strict().DecodeString(s) }

// Verify checks the signature under key, which must be of the kind the
// header's algorithm names: *rsa.PublicKey for RS256, a P-256
// *ecdsa.PublicKey for ES256. The algorithm comes from the header and the
// key's type must agree with it, so a key cannot be used under an
// algorithm it was not published for.
func (j *JWS) Verify(key crypto.PublicKey) error {
	digest := sha256.Sum256(j.signed)
	switch j.Header.Alg {
	case "RS256":
		k, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("RS256 needs an RSA key, not %T", key)
		}
		if k.N.BitLen() < minRSABits {
			return fmt.Errorf("RSA key of %d bits is under %d", k.N.BitLen(), minRSABits)
		}
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], j.sig); err != nil {
			return errors.New("bad signature")
		}
		return nil
	case "ES256":
		k, ok := key.(*ecdsa.PublicKey)
		if !ok || k.Curve != elliptic.P256() {
			return fmt.Errorf("ES256 needs a P-256 key, not %T", key)
		}
		// JWS carries R||S, 32 bytes each (RFC 7518 §3.4), not ASN.1.
		if len(j.sig) != 64 {
			return fmt.Errorf("ES256 signature of %d bytes, want 64", len(j.sig))
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

// JWK is one key of a JSON Web Key Set, as far as RS256 and ES256 need.
type JWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// Set is a JSON Web Key Set.
type Set struct {
	Keys []JWK `json:"keys"`
}

// PublicKey returns the key a JWK describes, for signing keys only. A
// key marked for encryption ("use":"enc") is refused, as Keycloak
// publishes one beside its signing keys.
func (k JWK) PublicKey() (crypto.PublicKey, error) {
	if k.Use != "" && k.Use != "sig" {
		return nil, fmt.Errorf("key %q is for %q, not signing", k.Kid, k.Use)
	}
	switch k.Kty {
	case "RSA":
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
			return nil, fmt.Errorf("key %q: RSA key of %d bits is under %d", k.Kid, pub.N.BitLen(), minRSABits)
		}
		return pub, nil
	case "EC":
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
