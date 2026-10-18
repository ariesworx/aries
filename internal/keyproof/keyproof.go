// Package keyproof is how a connection over HTTPS proves, before T2's
// sign-in exists, that it holds one of the SSH keys the server knows:
// the same keys that sign in over SSH (docs/design/transport.md, As
// built for T1).
//
// After the WebSocket upgrade, and before the protocol's hello, three
// messages pass on the connection, each one JSON line in one message:
//
//	client: {"t":"key.offer","url":URL,"keys":[KEY,…]}               the keys aries would offer sshd, at most MaxKeys
//	server: {"t":"key.challenge","key":I,"nonce":NONCE,"bind":B}     the first of them it knows, 32 random bytes, and the binding
//	client: {"t":"key.proof","v":2,"sig":SIG}                        that key's signature of the transcript
//
// The transcript ([SignV2]) names the server's URL as the client has
// it, which the server checks against its own configured URLs, never
// the request's Host (spike/https found nginx rewrites it): a proof
// made for one server cannot be relayed to another. The nonce is the
// server's, fresh for the connection, so a proof cannot be replayed on
// another. A refusal at either step is the protocol's own: a welcome
// frame with the error, which aries reads as it reads any refused
// welcome. None of this is a frame of the protocol, whose frames from
// the hello on are unchanged.
//
// The binding (finding F-2 of the v0.5.0 security review) is what ties
// the proof to the TLS session it is made on. Where ariesd terminates
// TLS, B is [BindTLSExporter], and the transcript adds the session's
// exporter (RFC 9266, [Exporter]), which the server reads from its own
// end: whoever relays the proof between two TLS sessions, with a
// certificate the client trusts, relays a proof of the wrong session.
// Behind a proxy, which terminates TLS itself, B is [BindNone], and the
// transcript adds no exporter. Protocol 9's aries signs the v2
// transcript alone, and refuses a challenge that names no binding,
// which only a server older than it (or someone between) sends: so no
// signature it makes is a v1 proof a relay could take to a server that
// still accepts v0.5.0's aries, which signs v1 ([Sign]) and sends no
// "v".
package keyproof

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
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

// domain separates the transcript from anything else a key signs:
// domain the v1 transcript, domainV2 the v2 one.
const (
	domain   = "aries-keyproof-v1"
	domainV2 = "aries-keyproof-v2"
)

// The bindings a challenge may ask for: the TLS session's exporter, or
// none, behind a proxy. A challenge that names none of the two is a
// server's older than protocol 9.
const (
	BindTLSExporter = "tls-exporter"
	BindNone        = "none"
)

// ExporterLabel and ExporterSize are RFC 9266's tls-exporter: the
// label, with no context, and the length of the keying material.
const (
	ExporterLabel = "EXPORTER-Channel-Binding"
	ExporterSize  = 32
)

// Exporter is the tls-exporter channel binding of the TLS session cs
// describes (RFC 9266): the same at both of its ends, and another for
// any other session. Go exports it under TLS 1.3, and under TLS 1.2
// when the extended master secret was negotiated, which Go's clients
// and servers always offer; otherwise it is an error.
func Exporter(cs *tls.ConnectionState) ([]byte, error) {
	if cs == nil {
		return nil, errors.New("the connection is not TLS")
	}
	b, err := cs.ExportKeyingMaterial(ExporterLabel, nil, ExporterSize)
	if err != nil {
		return nil, fmt.Errorf("the TLS session exports no channel binding: %w", err)
	}
	return b, nil
}

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
	return framed([]byte(domain), []byte(serverURL), nonce, pub.Marshal())
}

// transcriptV2 is what a key signs in a v2 proof: v1's parts under
// domainV2, then the exporter, empty when the challenge asked for no
// binding.
func transcriptV2(serverURL string, nonce []byte, pub ssh.PublicKey, exporter []byte) []byte {
	return framed([]byte(domainV2), []byte(serverURL), nonce, pub.Marshal(), exporter)
}

// framed is parts, each as an SSH string.
func framed(parts ...[]byte) []byte {
	var b bytes.Buffer
	for _, part := range parts {
		_ = binary.Write(&b, binary.BigEndian, uint32(len(part))) //nolint:gosec // short
		b.Write(part)
	}
	return b.Bytes()
}

// Sign signs the v1 transcript of a proof for serverURL and nonce with
// s, as v0.5.0's aries does: this aries signs [SignV2]'s. An RSA key
// signs with SHA-256, never SHA-1, which Verify refuses; a signer of an
// RSA key that cannot choose its algorithm is refused.
func Sign(s ssh.Signer, serverURL string, nonce []byte) (*ssh.Signature, error) {
	return sign(s, transcript(serverURL, nonce, s.PublicKey()))
}

// SignV2 signs the v2 transcript of a proof for serverURL and nonce
// with s, bound to exporter: ExporterSize bytes of the client's own TLS
// session, or nil where the challenge asked for no binding.
func SignV2(s ssh.Signer, serverURL string, nonce, exporter []byte) (*ssh.Signature, error) {
	if err := checkExporter(exporter); err != nil {
		return nil, err
	}
	return sign(s, transcriptV2(serverURL, nonce, s.PublicKey(), exporter))
}

// checkExporter refuses an exporter that is neither none nor
// ExporterSize bytes.
func checkExporter(exporter []byte) error {
	if len(exporter) != 0 && len(exporter) != ExporterSize {
		return fmt.Errorf("a channel binding of %d bytes, not %d", len(exporter), ExporterSize)
	}
	return nil
}

// sign signs data with s, an RSA key with SHA-256.
func sign(s ssh.Signer, data []byte) (*ssh.Signature, error) {
	if s.PublicKey().Type() != ssh.KeyAlgoRSA {
		return s.Sign(rand.Reader, data)
	}
	as, ok := s.(ssh.AlgorithmSigner)
	if !ok {
		return nil, errors.New("this RSA key cannot sign with SHA-256")
	}
	return as.SignWithAlgorithm(rand.Reader, data, ssh.KeyAlgoRSASHA256)
}

// Verify checks that sig is pub's signature of the v1 transcript for
// serverURL and nonce, in an algorithm of pub's type: SHA-256 or SHA-512
// for RSA.
func Verify(pub ssh.PublicKey, serverURL string, nonce []byte, sig *ssh.Signature) error {
	return verify(pub, nonce, sig, transcript(serverURL, nonce, pub))
}

// VerifyV2 checks that sig is pub's signature of the v2 transcript for
// serverURL and nonce bound to exporter, the server's end of the TLS
// session, or nil where it asked for no binding; as Verify checks its
// algorithm.
func VerifyV2(pub ssh.PublicKey, serverURL string, nonce, exporter []byte, sig *ssh.Signature) error {
	if err := checkExporter(exporter); err != nil {
		return err
	}
	return verify(pub, nonce, sig, transcriptV2(serverURL, nonce, pub, exporter))
}

// verify checks that sig is pub's signature of data, the transcript of
// a proof for nonce.
func verify(pub ssh.PublicKey, nonce []byte, sig *ssh.Signature, data []byte) error {
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
	if err := pub.Verify(data, sig); err != nil {
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
// in the offer, the nonce to sign, and the binding the proof must
// carry: BindTLSExporter or BindNone, or empty from a server older than
// protocol 9.
type Challenge struct {
	Key   int    `json:"key"`
	Nonce []byte `json:"nonce"`
	Bind  string `json:"bind,omitempty"`
}

type wireChallenge struct {
	T string `json:"t"`
	Challenge
}

// NewChallenge encodes a challenge to sign nonce with the offer's key
// at index key, bound as bind says, as one line.
func NewChallenge(key int, nonce []byte, bind string) []byte {
	return line(wireChallenge{T: TypeChallenge, Challenge: Challenge{Key: key, Nonce: nonce, Bind: bind}})
}

// ParseChallenge decodes a challenge and checks its type, index and
// nonce's length; the caller checks the index against its offer, and
// what to do with its binding.
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
	V   *int   `json:"v,omitempty"`
	Sig []byte `json:"sig"`
}

// Proof is a client's proof: the signature, and the version of the
// transcript it signs, 1 or 2.
type Proof struct {
	Sig *ssh.Signature
	V   int
}

// NewProof encodes a v1 proof, the signature in SSH's wire form, as one
// line, as v0.5.0's aries sends it: with no version.
func NewProof(sig *ssh.Signature) []byte {
	return line(wireProof{T: TypeProof, Sig: ssh.Marshal(sig)})
}

// NewProofV2 encodes a proof of the v2 transcript, as one line.
func NewProofV2(sig *ssh.Signature) []byte {
	v := 2
	return line(wireProof{T: TypeProof, V: &v, Sig: ssh.Marshal(sig)})
}

// ParseProof decodes a proof: its signature, and its version, 1 when it
// names none.
func ParseProof(b []byte) (Proof, error) {
	var w wireProof
	switch err := json.Unmarshal(b, &w); {
	case err != nil:
		return Proof{}, errors.New("the key proof is not JSON")
	case w.T != TypeProof:
		return Proof{}, fmt.Errorf("expected a %s, got %q", TypeProof, w.T)
	case w.V != nil && *w.V != 1 && *w.V != 2:
		return Proof{}, fmt.Errorf("a key proof of version %d, which this server does not read", *w.V)
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(w.Sig, &sig); err != nil || sig.Format == "" || len(sig.Blob) == 0 {
		return Proof{}, errors.New("the key proof holds no signature")
	}
	v := 1
	if w.V != nil {
		v = *w.V
	}
	return Proof{Sig: &sig, V: v}, nil
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
