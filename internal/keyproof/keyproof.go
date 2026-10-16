// Package keyproof is how a connection over HTTPS proves, before T2's
// sign-in exists, that it holds one of the SSH keys the server knows:
// the same keys that sign in over SSH (docs/design/transport.md, As
// built for T1).
//
// After the WebSocket upgrade, and before the protocol's hello, three
// messages pass on the connection, each one JSON line in one message:
//
//	client: {"t":"key.offer","url":URL,"keys":[KEY,…]}    the keys aries would offer sshd, at most MaxKeys
//	server: {"t":"key.challenge","key":I,"nonce":NONCE}   the first of them it knows, and 32 random bytes
//	client: {"t":"key.proof","sig":SIG}                   that key's signature of the transcript
//
// The transcript ([Sign]) names the server's URL as the client has it,
// which the server checks against its own configured URLs, never the
// request's Host (spike/https found nginx rewrites it): a proof made
// for one server cannot be relayed to another. The nonce is the
// server's, fresh for the connection, so a proof cannot be replayed on
// another. A refusal at either step is the protocol's own: a welcome
// frame with the error, which aries reads as it reads any refused
// welcome. None of this is a frame of the protocol, whose frames from
// the hello on are unchanged.
package keyproof

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// The messages' types, their "t".
const (
	TypeOffer     = "key.offer"
	TypeChallenge = "key.challenge"
	TypeProof     = "key.proof"
)

// MaxKeys is the most keys an offer may name. aries offers five at most,
// as it does to sshd.
const MaxKeys = 16

// NonceSize is the length of a challenge's nonce.
const NonceSize = 32

// ConnectPath is the path of the WebSocket under a server's URL.
const ConnectPath = "/v1/connect"

// domain separates the transcript from anything else a key signs.
const domain = "aries-keyproof-v1"

// NewNonce returns a challenge's nonce: NonceSize random bytes.
func NewNonce() []byte {
	b := make([]byte, NonceSize)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return b
}

// transcript is what a key signs: the domain, then the server's URL, the
// nonce and the public key, each as an SSH string (a 32-bit length and
// the bytes), so that no two of them can be read as another.
func transcript(serverURL string, nonce []byte, pub ssh.PublicKey) []byte {
	var b bytes.Buffer
	for _, part := range [][]byte{[]byte(domain), []byte(serverURL), nonce, pub.Marshal()} {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(part))) //nolint:gosec // short
		b.Write(part)
	}
	return b.Bytes()
}

// Sign signs the transcript of a proof for serverURL and nonce with s.
// An RSA key signs with SHA-256, never SHA-1, which Verify refuses; a
// signer of an RSA key that cannot choose its algorithm is refused.
func Sign(s ssh.Signer, serverURL string, nonce []byte) (*ssh.Signature, error) {
	data := transcript(serverURL, nonce, s.PublicKey())
	if s.PublicKey().Type() != ssh.KeyAlgoRSA {
		return s.Sign(rand.Reader, data)
	}
	as, ok := s.(ssh.AlgorithmSigner)
	if !ok {
		return nil, errors.New("this RSA key cannot sign with SHA-256")
	}
	return as.SignWithAlgorithm(rand.Reader, data, ssh.KeyAlgoRSASHA256)
}

// Verify checks that sig is pub's signature of the transcript for
// serverURL and nonce, in an algorithm of pub's type: SHA-256 or SHA-512
// for RSA.
func Verify(pub ssh.PublicKey, serverURL string, nonce []byte, sig *ssh.Signature) error {
	switch {
	case sig == nil:
		return errors.New("no signature")
	case len(nonce) != NonceSize:
		return errors.New("the nonce is not the server's")
	}
	switch t := pub.Type(); {
	case t == ssh.KeyAlgoRSA:
		if sig.Format != ssh.KeyAlgoRSASHA256 && sig.Format != ssh.KeyAlgoRSASHA512 {
			return fmt.Errorf("an RSA signature in %s, not SHA-256 or SHA-512", sig.Format)
		}
	case sig.Format != t:
		return fmt.Errorf("a signature in %s by a key of type %s", sig.Format, t)
	}
	if err := pub.Verify(transcript(serverURL, nonce, pub), sig); err != nil {
		return fmt.Errorf("the signature does not verify: %w", err)
	}
	return nil
}

// Offer is a client's first message: the server's URL as the client has
// it, normalized, and the keys it would sign in with, in its order.
type Offer struct {
	URL  string
	Keys []ssh.PublicKey
}

type wireOffer struct {
	T    string   `json:"t"`
	URL  string   `json:"url"`
	Keys []string `json:"keys"`
}

// NewOffer encodes an offer of keys to the server at serverURL, as one
// line.
func NewOffer(serverURL string, keys []ssh.PublicKey) []byte {
	w := wireOffer{T: TypeOffer, URL: serverURL, Keys: make([]string, len(keys))}
	for i, k := range keys {
		w.Keys[i] = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	}
	return line(w)
}

// line is v's JSON and a newline.
func line(v any) []byte {
	b, _ := json.Marshal(v) // the types above always encode
	return append(b, '\n')
}

// ParseOffer decodes and checks an offer: its type, a URL NormalizeURL
// takes, and from one to MaxKeys keys, each a public key in
// authorized_keys form with no options (a comment is ignored) and no
// certificate. An error names which key, never its text.
func ParseOffer(b []byte) (Offer, error) {
	var w wireOffer
	if err := json.Unmarshal(b, &w); err != nil {
		return Offer{}, errors.New("the key offer is not JSON")
	}
	if w.T != TypeOffer {
		return Offer{}, fmt.Errorf("expected a %s, got %q", TypeOffer, w.T)
	}
	u, err := NormalizeURL(w.URL)
	if err != nil {
		return Offer{}, fmt.Errorf("the key offer's url: %w", err)
	}
	switch {
	case len(w.Keys) == 0:
		return Offer{}, errors.New("the key offer names no key")
	case len(w.Keys) > MaxKeys:
		return Offer{}, fmt.Errorf("the key offer names %d keys; it may name at most %d", len(w.Keys), MaxKeys)
	}
	o := Offer{URL: u, Keys: make([]ssh.PublicKey, len(w.Keys))}
	for i, k := range w.Keys {
		pub, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(k))
		if err != nil || len(options) > 0 || len(rest) > 0 || strings.ContainsAny(k, "\r\n") {
			return Offer{}, fmt.Errorf("key %d of the offer is not one public key", i+1)
		}
		if _, cert := pub.(*ssh.Certificate); cert {
			return Offer{}, fmt.Errorf("key %d of the offer is a certificate, which the server does not take", i+1)
		}
		o.Keys[i] = pub
	}
	return o, nil
}

// Challenge is the server's answer to an offer: which key, by its index
// in the offer, and the nonce to sign.
type Challenge struct {
	Key   int    `json:"key"`
	Nonce []byte `json:"nonce"`
}

type wireChallenge struct {
	T string `json:"t"`
	Challenge
}

// NewChallenge encodes a challenge to sign nonce with the offer's key
// at index key, as one line.
func NewChallenge(key int, nonce []byte) []byte {
	return line(wireChallenge{T: TypeChallenge, Challenge: Challenge{Key: key, Nonce: nonce}})
}

// ParseChallenge decodes a challenge and checks its type, index and
// nonce's length; the caller checks the index against its offer.
func ParseChallenge(b []byte) (Challenge, error) {
	var w wireChallenge
	switch err := json.Unmarshal(b, &w); {
	case err != nil:
		return Challenge{}, errors.New("the server's challenge is not JSON")
	case w.T != TypeChallenge:
		return Challenge{}, fmt.Errorf("expected a %s, got %q", TypeChallenge, w.T)
	case w.Key < 0 || len(w.Nonce) != NonceSize:
		return Challenge{}, errors.New("the server's challenge names no key or no nonce")
	}
	return w.Challenge, nil
}

type wireProof struct {
	T   string `json:"t"`
	Sig []byte `json:"sig"`
}

// NewProof encodes a proof, the signature in SSH's wire form, as one
// line.
func NewProof(sig *ssh.Signature) []byte {
	return line(wireProof{T: TypeProof, Sig: ssh.Marshal(sig)})
}

// ParseProof decodes a proof's signature.
func ParseProof(b []byte) (*ssh.Signature, error) {
	var w wireProof
	switch err := json.Unmarshal(b, &w); {
	case err != nil:
		return nil, errors.New("the key proof is not JSON")
	case w.T != TypeProof:
		return nil, fmt.Errorf("expected a %s, got %q", TypeProof, w.T)
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(w.Sig, &sig); err != nil || sig.Format == "" || len(sig.Blob) == 0 {
		return nil, errors.New("the key proof holds no signature")
	}
	return &sig, nil
}

// pathPattern is what the path of a server's URL may be: segments of
// unreserved characters.
var pathPattern = lazyregexp.New(`^(/[A-Za-z0-9._~-]+)*$`)

// NormalizeURL returns a server's URL in the one form both sides
// compare: https, the host in lower case, no default port, no trailing
// slash, and no user, query or fragment, which a server's URL never
// has.
func NormalizeURL(s string) (string, error) {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return "", errors.New("not a URL")
	case !strings.EqualFold(u.Scheme, "https"):
		return "", fmt.Errorf("%q is not an https:// URL", s)
	case u.User != nil:
		return "", errors.New("a server's URL names no user")
	case u.RawQuery != "" || u.ForceQuery:
		return "", errors.New("a server's URL has no query")
	case u.Fragment != "" || strings.Contains(s, "#"):
		return "", errors.New("a server's URL has no fragment")
	case u.Hostname() == "":
		return "", errors.New("the URL names no host")
	}
	path := strings.TrimSuffix(u.EscapedPath(), "/")
	if !pathPattern.MatchString(path) {
		return "", errors.New("the URL's path may hold only letters, digits and ._~-")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" && p != "443" {
		host += ":" + p
	}
	return "https://" + host + path, nil
}

// ConnectURL is the WebSocket's URL under serverURL, a URL NormalizeURL
// returned.
func ConnectURL(serverURL string) string {
	return "wss" + strings.TrimPrefix(serverURL, "https") + ConnectPath
}
