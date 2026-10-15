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
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/ariesworx/aries/internal/logs"
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
	// path is the key's file, or empty when none is named, and pathErr
	// why none could be.
	path    string
	pathErr error
	// wanted says spans are to carry stand-ins, so the key is made when
	// its file is not there. Without it an existing file is still read,
	// so that stand-ins made earlier can be resolved.
	wanted bool
	log    *slog.Logger
	// state is the key, or why there is none; RetryKey replaces it.
	state atomic.Pointer[keyState]
}

// keyState is the key that makes the stand-ins, or why there is none.
type keyState struct {
	// key is nil when there is none, and then a pseudonym is nothing at
	// all: a span says nothing about who, as with PrincipalsOmit. No key
	// is ever made up in its place: stand-ins that changed at each start
	// and that nobody could resolve would be worse than none.
	key []byte
	// problem is why a key that is wanted is not there; nil when there
	// is a key, or none is wanted.
	problem *keyProblem
}

// keyProblem is why the stand-in key cannot be used.
type keyProblem struct {
	// cause is one short clause, and never holds the file's content.
	cause string
	// damaged says the file is there and cannot be trusted: others can
	// read it, or it does not hold a key. That is not a key that could
	// not be made: someone may have read or changed it.
	damaged bool
	// fixes are what the operator can do, a short line each.
	fixes []string
}

// openPeople returns the people of cfg. wanted says spans will be
// exported, so that with PrincipalsPseudonym the key is read from
// cfg.KeyFile or made there. A key that cannot be read or made is never
// an error: spans then say nothing about who, the log says why and what
// to do, and RetryKey looks again. memory, for tests, keeps a key in
// memory when cfg names no file.
func openPeople(cfg Config, wanted, memory bool, log *slog.Logger) (*people, error) {
	p := &people{mode: cfg.Principals, path: cfg.KeyFile, pathErr: cfg.KeyFileErr, log: log}
	p.state.Store(&keyState{})
	if p.mode == "" {
		p.mode = PrincipalsPseudonym
	}
	if !ValidPrincipals(p.mode) {
		return p, fmt.Errorf("telemetry: principals %q is not %s, %s or %s; fix: set telemetry: principals: to one of them in starfixd's config file, or leave it out for %s",
			cfg.Principals, PrincipalsPseudonym, PrincipalsName, PrincipalsOmit, PrincipalsPseudonym)
	}
	p.wanted = wanted && p.mode == PrincipalsPseudonym
	if p.path == "" && memory && p.wanted {
		key := make([]byte, keySize)
		_, _ = rand.Read(key) // crypto/rand.Read never fails
		p.state.Store(&keyState{key: key})
		return p, nil
	}
	st := p.load()
	p.state.Store(st)
	if st.problem != nil {
		p.report(st.problem)
	}
	return p, nil
}

// load reads the key, or makes it when it is wanted and not there.
func (p *people) load() *keyState {
	if p.path == "" {
		if !p.wanted {
			return &keyState{}
		}
		cause := "no file is named for it"
		if p.pathErr != nil {
			cause = p.pathErr.Error()
		}
		return &keyState{problem: &keyProblem{cause: cause, fixes: []string{
			"set telemetry: principals_key: to an absolute path in starfixd's config file, in a directory starfixd may write to",
			p.orDoWithout(),
		}}}
	}
	key, prob := readKey(p.path)
	switch {
	case prob == nil && key != nil:
		return &keyState{key: key}
	case prob != nil && !p.wanted:
		// Nothing needs the key now. It is only what whois would use,
		// which says why it cannot.
		return &keyState{}
	case prob != nil:
		prob.fixes = append(prob.fixes, p.orDoWithout())
		return &keyState{problem: prob}
	case !p.wanted:
		return &keyState{}
	}
	key, err := makeKey(p.path)
	if err != nil {
		dir := filepath.Dir(p.path)
		return &keyState{problem: &keyProblem{cause: "it cannot be made: " + err.Error(), fixes: []string{
			fmt.Sprintf("make its directory, owned by the user starfixd runs as: install -d -m 0700 -o starfix -g starfix %s", dir),
			fmt.Sprintf("under systemd, let starfixd write there with a drop-in: ReadWritePaths=%s (docs/server.md, Traces)", dir),
			"with the AppArmor profiles, starfixd-serve must allow the file: deploy/apparmor, and its tunables if the path is not the default",
			p.orDoWithout(),
		}}}
	}
	p.log.Info("telemetry: made the key that principals' stand-ins are derived from", "event", logs.TelemetryKeyMade, "path", p.path)
	return &keyState{key: key}
}

// orDoWithout is the fix that needs no key.
func (p *people) orDoWithout() string {
	return fmt.Sprintf("or do without: telemetry: principals: %s says nothing about who on purpose, and %s=%s exports no traces", PrincipalsOmit, envTracesExporter, exporterNone)
}

// report logs, at error level, that spans and exported log records say
// nothing about who and why, then each fix: a short line each, so that
// `systemctl status`, which cuts a long line, shows them. Every line is
// the one event, and what varies is in a field: a message is fixed text
// (internal/logs).
func (p *people) report(prob *keyProblem) {
	p.log.Error("telemetry: spans say nothing about who: the key for principals' stand-ins is unavailable", "event", logs.TelemetryKeyUnavailable)
	if p.path != "" {
		p.log.Error("the key's file", "event", logs.TelemetryKeyUnavailable, "path", p.path)
	}
	p.log.Error("cause", "event", logs.TelemetryKeyUnavailable, logs.KeyCause, prob.cause)
	for _, fix := range prob.fixes {
		p.log.Error("fix", "event", logs.TelemetryKeyUnavailable, logs.KeyFix, fix)
	}
	p.log.Error("starfixd serves as it is, and looks for the key again at each tick of its reaper: no restart is needed", "event", logs.TelemetryKeyUnavailable)
}

// RetryKey looks for the stand-in key again when spans are to carry
// stand-ins and it was unavailable: it reads the file, or makes it. The
// daemon calls it at each tick of its reaper, so that making the
// directory, or repairing the file, is all an operator has to do. When
// the key is there it says so, once, and spans name people by stand-ins
// from then on; when the cause has changed it says the new one.
func (t *Telemetry) RetryKey() {
	if t == nil {
		return
	}
	p := t.who
	old := p.state.Load()
	if !p.wanted || old.key != nil {
		return
	}
	st := p.load()
	p.state.Store(st)
	switch {
	case st.key != nil:
		p.log.Info("telemetry: the key for principals' stand-ins is there; spans name people by stand-ins from now on", "event", logs.TelemetryKeyFound, "path", p.path)
	case st.problem != nil && (old.problem == nil || old.problem.cause != st.problem.cause):
		p.report(st.problem)
	}
}

// People is how people appear on the spans a Telemetry exports now.
type People struct {
	// Mode is the setting: PrincipalsPseudonym, PrincipalsName or
	// PrincipalsOmit.
	Mode string
	// StandIns says spans carry stand-ins: the mode is pseudonym and the
	// key is there.
	StandIns bool
	// Cause is why, with pseudonym, spans say nothing about who: the key
	// is unavailable. Empty otherwise. Damaged says the key's file is
	// there and cannot be trusted, and KeyFile names it.
	Cause   string
	Damaged bool
	KeyFile string
}

// People reports how people appear on spans now.
func (t *Telemetry) People() People {
	if t == nil {
		return People{Mode: PrincipalsOmit}
	}
	st := t.who.state.Load()
	out := People{Mode: t.who.mode, StandIns: t.who.mode == PrincipalsPseudonym && st.key != nil, KeyFile: t.who.path}
	if st.problem != nil {
		out.Cause, out.Damaged = st.problem.cause, st.problem.damaged
	}
	return out
}

// readKey reads the key file at path: one line of 64 hex digits, in a
// regular file that only its owner can read. It returns neither a key
// nor a problem for a file that does not exist. A problem never repeats
// what the file holds.
func readKey(path string) ([]byte, *keyProblem) {
	replace := "delete it, and starfixd makes a new key: stand-ins made with the old one can then no longer be resolved"
	f, err := os.Open(path) //nolint:gosec // the operator names the key file
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, &keyProblem{cause: "it cannot be read: " + err.Error(), fixes: []string{
			"make the file readable by the user starfixd runs as, and by nobody else (mode 0600)",
			"with the AppArmor profiles, starfixd-serve must allow the file: deploy/apparmor, and its tunables if the path is not the default",
		}}
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, &keyProblem{cause: "it cannot be read: " + err.Error()}
	}
	if !fi.Mode().IsRegular() {
		return nil, &keyProblem{cause: "its path holds something that is not a regular file", damaged: true,
			fixes: []string{"move it away, and starfixd makes a new key there"}}
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, &keyProblem{cause: fmt.Sprintf("its file is mode %04o: others can read it, and whoever has can test a name against a stand-in", perm), damaged: true,
			fixes: []string{
				"if someone may have read it, " + replace,
				fmt.Sprintf("if nobody can have: chmod 600 %s", path),
			}}
	}
	b, err := io.ReadAll(io.LimitReader(f, 4*keySize))
	if err != nil {
		return nil, &keyProblem{cause: "it cannot be read: " + err.Error()}
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != keySize {
		return nil, &keyProblem{cause: "its file does not hold a key: it was cut short, or changed", damaged: true, fixes: []string{replace}}
	}
	return key, nil
}

// makeKey writes a new key to path, mode 0600, and returns it. The file
// is created, never replaced: one that appeared meanwhile is another
// daemon's, and is left as it is.
func makeKey(path string) ([]byte, error) {
	key := make([]byte, keySize)
	_, _ = rand.Read(key)                                                 // crypto/rand.Read never fails
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator names the key file
	if err != nil {
		// The path is in the log line already.
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			err = pe.Err
		}
		return nil, err
	}
	_, werr := f.WriteString(hex.EncodeToString(key) + "\n")
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		// Half a key is worse than none: the next look would find it
		// damaged.
		_ = os.Remove(path)
		return nil, err
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
func (p *people) principal(name string) string {
	switch {
	case name == "":
	case p.mode == PrincipalsName && plainID(name):
		return name
	case p.mode == PrincipalsPseudonym:
		if key := p.state.Load().key; key != nil {
			return standIn(key, principalPrefix, "", name)
		}
	}
	return ""
}

// session is what a span says of the session id: "" for nothing. A
// session id can name a person's machine, so it is treated as the
// principal is.
func (p *people) session(id string) string {
	switch {
	case id == "":
	case p.mode == PrincipalsName && plainID(id):
		return id
	case p.mode == PrincipalsPseudonym:
		if key := p.state.Load().key; key != nil {
			return standIn(key, sessionPrefix, "session\x00", id)
		}
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
	if t == nil {
		return "", false
	}
	key := t.who.state.Load().key
	if key == nil {
		return "", false
	}
	return standIn(key, principalPrefix, "", principal), true
}

// NoKey says why this server can resolve no stand-in, as a clause for a
// refusal, or returns "" when it has the key.
func (t *Telemetry) NoKey() string {
	if t == nil {
		return "it exports no telemetry"
	}
	st := t.who.state.Load()
	switch {
	case st.key != nil:
		return ""
	case st.problem != nil:
		return "the key is unavailable (" + st.problem.cause + "), so its spans say nothing about who"
	case t.who.mode != PrincipalsPseudonym:
		return "telemetry: principals: is " + t.who.mode + ", which makes no stand-in, and there is no key file from before"
	case t.tracer == nil:
		return "it exports no traces, and there is no key file from before"
	}
	return "there is no key"
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
