package telemetry

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"strings"
)

// How a person appears in what leaves the machine through telemetry: a
// span's principal and session (design §16.4, decision 32). It does not
// change the daemon's own log.
const (
	// PrincipalsPseudonym, the default, puts a stable stand-in in place
	// of each name: the same person is the same stand-in in every span,
	// and the backend never learns who.
	PrincipalsPseudonym = "pseudonym"
	// PrincipalsName puts the principal's name and the session's id.
	PrincipalsName = "name"
	// PrincipalsOmit puts nothing about who.
	PrincipalsOmit = "omit"
)

// ValidPrincipals reports whether mode is a value of telemetry:
// principals:, or empty, which is the default.
func ValidPrincipals(mode string) bool {
	switch mode {
	case "", PrincipalsPseudonym, PrincipalsName, PrincipalsOmit:
		return true
	}
	return false
}

// A stand-in is its prefix and standInHex hex digits: the first 8 bytes
// of an HMAC-SHA-256 under a key of keySize random bytes. 64 bits is
// ample to tell the people of one server apart, and too few to be what
// keeps a name hidden: the key does that.
const (
	principalPrefix = "p_"
	sessionPrefix   = "s_"
	standInHex      = 16
	keySize         = 32
)

// people is how principals and sessions appear on spans.
type people struct {
	mode string
	// key makes the stand-ins; nil when there is none, and then a
	// pseudonym is nothing at all.
	key []byte
}

// openPeople returns the people of cfg. With create set, which is when
// spans will carry stand-ins, the key is read from cfg.KeyFile or made
// there; an empty KeyFile keeps a key in memory, whose stand-ins do not
// outlast the process. Without create an existing file is still read, so
// that stand-ins made earlier can be resolved ([Telemetry.Whois]), and
// one that cannot be read is logged and left: nothing is written.
func openPeople(cfg Config, create bool, log *slog.Logger) (people, error) {
	p := people{mode: cfg.Principals}
	if p.mode == "" {
		p.mode = PrincipalsPseudonym
	}
	if !ValidPrincipals(p.mode) {
		return p, fmt.Errorf("telemetry: principals %q is not %s, %s or %s; fix: set telemetry: principals: to one of them in starfixd's config file, or leave it out for %s",
			cfg.Principals, PrincipalsPseudonym, PrincipalsName, PrincipalsOmit, PrincipalsPseudonym)
	}
	create = create && p.mode == PrincipalsPseudonym
	if cfg.KeyFile == "" {
		if create {
			p.key = make([]byte, keySize)
			_, _ = rand.Read(p.key) // crypto/rand.Read never fails
		}
		return p, nil
	}
	key, err := readKey(cfg.KeyFile)
	switch {
	case err == nil:
		p.key = key
	case errors.Is(err, fs.ErrNotExist) && create:
		if p.key, err = makeKey(cfg.KeyFile); err != nil {
			return p, err
		}
		log.Info("telemetry: made the key that principals' stand-ins are derived from", "path", cfg.KeyFile)
	case errors.Is(err, fs.ErrNotExist):
	case create:
		return p, err
	default:
		log.Warn("telemetry: the stand-in key cannot be used, so `sfx admin principal whois` resolves nothing", "err", err)
	}
	return p, nil
}

// readKey reads the key file at path: one line of 64 hex digits, in a
// regular file that only its owner can read. The error for a file that
// does not exist is fs.ErrNotExist's; any other names a fix, and never
// repeats what the file holds.
func readKey(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the key file
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("telemetry: read the stand-in key %s: %w; fix: make the file readable by the user starfixd runs as, or delete it for a new key", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("telemetry: read the stand-in key %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("telemetry: the stand-in key %s is not a regular file; fix: move it away, and starfixd makes a new key", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("telemetry: the stand-in key %s is mode %04o, and whoever reads it can test a name against a stand-in; fix: chmod 600 %s", path, perm, path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4*keySize))
	if err != nil {
		return nil, fmt.Errorf("telemetry: read the stand-in key %s: %w", path, err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != keySize {
		return nil, fmt.Errorf("telemetry: the stand-in key %s does not hold a key; fix: delete it, and starfixd makes a new one; stand-ins made with the old key can then no longer be resolved", path)
	}
	return key, nil
}

// makeKey writes a new key to path, mode 0600, and returns it. The file
// is created, never replaced: one that appeared meanwhile is another
// daemon's, and is left as it is.
func makeKey(path string) ([]byte, error) {
	key := make([]byte, keySize)
	_, _ = rand.Read(key) // crypto/rand.Read never fails
	fix := fmt.Sprintf("fix: create its directory, owned by the user starfixd runs as and mode 0700, and let starfixd write there (with the guide's unit, ReadWritePaths= names it; docs/server.md, Traces); or set telemetry: principals: to %s or %s, which need no key; or set %s=%s to export no traces",
		PrincipalsName, PrincipalsOmit, envTracesExporter, exporterNone)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator names the key file
	if err != nil {
		return nil, fmt.Errorf("telemetry: make the stand-in key %s: %w; %s", path, err, fix)
	}
	_, werr := f.WriteString(hex.EncodeToString(key) + "\n")
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		// Half a key is worse than none: the next start would refuse it.
		_ = os.Remove(path)
		return nil, fmt.Errorf("telemetry: make the stand-in key %s: %w; %s", path, err, fix)
	}
	return key, nil
}

// standIn is prefix and the stand-in for value under key. domain keeps
// a session's stand-in apart from that of a principal of the same text.
func standIn(key []byte, prefix, domain, value string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(domain))
	mac.Write([]byte(value))
	return prefix + hex.EncodeToString(mac.Sum(nil))[:standInHex]
}

// plainID reports whether s may go on a span as it is: 1 to 255 bytes
// of printable ASCII with no space, as a principal's name and the
// session ids starfix makes are.
func plainID(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// principal is what a span says of the principal name: "" for nothing.
func (p people) principal(name string) string {
	switch {
	case name == "":
	case p.mode == PrincipalsName && plainID(name):
		return name
	case p.mode == PrincipalsPseudonym && p.key != nil:
		return standIn(p.key, principalPrefix, "", name)
	}
	return ""
}

// session is what a span says of the session id: "" for nothing. A
// session id can name a person's machine, so it is treated as the
// principal is.
func (p people) session(id string) string {
	switch {
	case id == "":
	case p.mode == PrincipalsName && plainID(id):
		return id
	case p.mode == PrincipalsPseudonym && p.key != nil:
		return standIn(p.key, sessionPrefix, "session\x00", id)
	}
	return ""
}

// ValidStandIn reports whether s has the form of a principal's
// stand-in: p_ and 16 lowercase hex digits.
func ValidStandIn(s string) bool {
	hexDigits, ok := strings.CutPrefix(s, principalPrefix)
	if !ok || len(hexDigits) != standInHex {
		return false
	}
	for i := range len(hexDigits) {
		if c := hexDigits[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// StandIn returns the stand-in for the principal name under this
// server's key, whatever telemetry: principals: is set to. It reports
// false when the server has no key: it has made no stand-in since the
// key file was last deleted.
func (t *Telemetry) StandIn(principal string) (string, bool) {
	if t == nil || t.who.key == nil {
		return "", false
	}
	return standIn(t.who.key, principalPrefix, "", principal), true
}

// Whois returns which of names the stand-in is for. It reports false
// when it is for none of them, or the server has no key.
func (t *Telemetry) Whois(alias string, names iter.Seq[string]) (string, bool) {
	for name := range names {
		s, ok := t.StandIn(name)
		if !ok {
			return "", false
		}
		if s == alias {
			return name, true
		}
	}
	return "", false
}
