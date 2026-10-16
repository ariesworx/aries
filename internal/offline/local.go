package offline

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/outbox"
	"github.com/ariesworx/aries/internal/proto"
)

// EnvDataDir names the directory aries keeps its outboxes in, in place of
// the user's data directory. It must be an absolute path. Tests set it,
// so that none touches a person's own.
const EnvDataDir = "ARIES_DATA_DIR"

// DataDir returns the directory aries keeps its own data in on goos:
// EnvDataDir's when set, else "aries" under the user's data directory,
// which on Windows is the local application data directory and never the
// roaming one (doc.go, "Where"). It makes nothing. A refusal names
// EnvDataDir in its fix.
func DataDir(goos string, getenv func(string) string, home func() (string, error)) (string, error) {
	refuse := func(msg string) (string, error) {
		return "", proto.Errf(proto.CodeUnavailable,
			"set "+EnvDataDir+" to the absolute path of a directory of your own, on a local disk, for aries to keep its outbox in", msg)
	}
	if d := getenv(EnvDataDir); d != "" {
		if !filepath.IsAbs(d) {
			return refuse(EnvDataDir + " is not an absolute path")
		}
		return filepath.Clean(d), nil
	}
	if goos == "windows" {
		if d := getenv("LocalAppData"); filepath.IsAbs(d) {
			return filepath.Join(d, "aries"), nil
		}
		return refuse("LocalAppData is not set, so there is no local application data directory to keep an outbox in")
	}
	if d := getenv("XDG_DATA_HOME"); goos != "darwin" && filepath.IsAbs(d) {
		return filepath.Join(d, "aries"), nil
	}
	h, err := home()
	if err != nil || h == "" {
		return refuse("there is no home directory to keep an outbox under")
	}
	if goos == "darwin" {
		return filepath.Join(h, "Library", "Application Support", "aries"), nil
	}
	return filepath.Join(h, ".local", "share", "aries"), nil
}

// The files of a server and project's directory, beside its outboxes.
const (
	// seenFile says who the server took each key for (doc.go, "Whose").
	seenFile = "seen.json"
	// missedFile holds when the server was last not reached, as text.
	missedFile = "missed"
)

// seenVersion is the version of seen.json's format. A file of another
// version is read as empty, and what it said is learned again at the
// next handshake.
const seenVersion = 1

// maxKeys bounds the keys seen.json remembers; past it the one confirmed
// longest ago goes.
const maxKeys = 32

// missEvery is how often, at most, the time the server was last not
// reached is written while it stays away.
const missEvery = time.Minute

// seen is seen.json: the server, the project, and for each key the
// server accepted from this machine its public fingerprint, the
// principal and when that was last confirmed. It holds nothing else: no
// key, no path to one, no token. A principal may have several keys, and
// a key that replaces another finds the same outbox, which is named for
// the principal.
type seen struct {
	V int `json:"v"`
	// Server is the host key the directory was made under, and Project
	// the project's id: what the directory, whose name is a hash of
	// them, is for.
	Server  string `json:"server"`
	Project string `json:"project"`
	// HostKeys are the host keys the config pinned at the last
	// handshake, when they were other than Server alone: a config that
	// pins any one of them finds this directory ([Find]), which is how
	// the server stays one server across a rotation of its host key.
	HostKeys []string `json:"host_keys,omitempty"`
	// Keys maps the fingerprint of a key that signed in to who the server
	// took it for.
	Keys map[string]keyNote `json:"keys,omitempty"`
	// Unconfigured is the fingerprint the server accepted the last time
	// aries connected with no key configured, having offered several.
	Unconfigured string `json:"unconfigured,omitempty"`
	// Addr is where the config said the server was at the last handshake
	// ([Address]): what tells a server rebuilt with a new host key for
	// the same one (Local.Rekeyed). Empty in a record from before it.
	Addr string `json:"addr,omitempty"`
	// Proto is the protocol of the last handshake, kept as a record: since
	// v1.0.0 settled the protocol at 8, nothing reads it.
	Proto int `json:"proto,omitempty"`
}

type keyNote struct {
	Principal string    `json:"principal"`
	At        time.Time `json:"at"`
}

// Options are a Local's clock, its outboxes' bounds and the server's
// address.
type Options struct {
	// Now is the client's clock; time.Now when nil.
	Now func() time.Time
	// Address is where the config says the server is ([Address]), noted
	// at each handshake.
	Address string
	// Outbox bounds each outbox; its zero value takes the defaults, and
	// its clock is Now.
	Outbox outbox.Options
	// Cache bounds each principal's cache; its zero value takes the
	// defaults.
	Cache CacheOptions
}

// Local is what this machine keeps for one server and project: who the
// server took its keys for, and an outbox for each of those principals.
// It holds no file open, and is safe for use by several goroutines and
// processes.
type Local struct {
	root, dir       string
	server, project string
	// pinned are the host keys the config pins, which a handshake notes.
	pinned []string
	opts   Options
}

// Open returns the Local kept under exactly the host key hostKey for the
// project, under root ([DataDir]), as when the config pins that key
// alone and nothing was kept for the server under another. A config's
// Local is [Find]'s. It makes nothing until something is kept.
func Open(root, hostKey, project string, o Options) *Local {
	return open(root, hostKey, []string{hostKey}, project, o)
}

// open is the Local kept under the host key server, for a config that
// pins pinned.
func open(root, server string, pinned []string, project string, o Options) *Local {
	if o.Now == nil {
		o.Now = time.Now
	}
	o.Outbox.Now = o.Now
	return &Local{root: root, dir: filepath.Join(root, hashed("server", server, project)), server: server, project: project,
		pinned: pinned, opts: o}
}

// Find returns the Local for the server whose host keys the config pins,
// pinned, in the config's order, and the project, under root
// ([DataDir]). The server is one server whichever of them it presents,
// and stays one across a rotation (doc.go, "Where"): Find takes the
// directory kept under the first pinned key that has a record, else the
// one whose record says a handshake was made while the config pinned
// one of these keys, else the first key's. It makes nothing.
func Find(root string, pinned []string, project string, o Options) *Local {
	for _, k := range pinned {
		if _, ok := record(filepath.Join(root, hashed("server", k, project)), k, project); ok {
			return open(root, k, pinned, project, o)
		}
	}
	if anchor, ok := linked(root, pinned, project); ok {
		return open(root, anchor, pinned, project, o)
	}
	return open(root, pinned[0], pinned, project, o)
}

// linked finds, among the directories of root, one kept for project
// under another host key whose record lists one of pinned among the keys
// pinned at its last handshake, and returns the key it is kept under.
// Of several, the first by name is taken, so every process takes the
// same one.
func linked(root string, pinned []string, project string) (string, bool) {
	if private(root) != nil {
		return "", false
	}
	ents, err := os.ReadDir(root) // sorted by name
	if err != nil {
		return "", false
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		s, ok := record(filepath.Join(root, e.Name()), "", project)
		if ok && slices.ContainsFunc(s.HostKeys, func(k string) bool { return slices.Contains(pinned, k) }) {
			return s.Server, true
		}
	}
	return "", false
}

// record reads the seen.json of dir, when dir is the user's own, and
// the record is of this version, names project and, unless server is
// "", that host key, and is the record of the directory it is in.
func record(dir, server, project string) (seen, bool) {
	if private(dir) != nil {
		return seen{}, false
	}
	b, err := os.ReadFile(filepath.Join(dir, seenFile)) //nolint:gosec // aries's own file, in a directory checked to be the user's
	if err != nil {
		return seen{}, false
	}
	var s seen
	if json.Unmarshal(b, &s) != nil || s.V != seenVersion || s.Project != project || s.Server == "" ||
		(server != "" && s.Server != server) || filepath.Base(dir) != hashed("server", s.Server, project) {
		return seen{}, false
	}
	return s, true
}

// hashed names a directory for parts, which are other people's text (a
// principal's name is the server's) and so name no file themselves.
func hashed(kind string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte("aries " + kind))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Dir is the directory of this server and project.
func (l *Local) Dir() string { return l.dir }

// outboxDir is principal's outbox: named for the principal, never for a
// key, so a principal's next key delivers what the last one queued.
func (l *Local) outboxDir(principal string) string {
	return filepath.Join(l.dir, "outbox-"+hashed("principal", principal))
}

// private refuses dirs that are there and are not the current user's
// alone: owned by someone else, or readable or writable by the group or
// by others, as a key file is checked (client.OwnedByUser). What is kept
// here is who a person is to a server and the text of what they wrote.
// It changes no mode: a directory that is open was not made by aries, or
// was opened since, and either is for its owner to see to. A directory
// that is not there yet passes: aries makes it 0700.
//
// On Windows nothing is checked: ownership and access are in ACLs, which
// this does not read. The local application data directory is the
// user's own there by default.
func private(dirs ...string) error {
	for _, dir := range dirs {
		st, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		why := ""
		switch {
		case err != nil:
			why = fmt.Sprintf("%s cannot be read: %v", dir, errors.Unwrap(err))
		case !st.IsDir():
			why = dir + " is not a directory"
		case !client.OwnedByUser(st):
			why = dir + " belongs to another user"
		case runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0:
			why = fmt.Sprintf("%s can be read or written by other users (mode %04o)", dir, st.Mode().Perm())
		}
		if why != "" {
			return proto.Errf(proto.CodeUnavailable,
				fmt.Sprintf("if it is yours, `chmod 700 %s`; otherwise set %s to a directory of your own", dir, EnvDataDir),
				"aries keeps its outbox only in a directory that is yours alone: "+why)
		}
	}
	return nil
}

// ready checks the directories of this server and project, and with
// outboxOf that principal's outbox too, and makes the ones that are not
// there when create is set.
func (l *Local) ready(create bool, outboxOf string) error {
	dirs := []string{l.root, l.dir}
	if outboxOf != "" {
		dirs = append(dirs, l.outboxDir(outboxOf))
	}
	if err := private(dirs...); err != nil {
		return err
	}
	if !create {
		return nil
	}
	// The data directory's parents are the system's; the two below are
	// aries's own, and the outbox makes its own the same way.
	if err := os.MkdirAll(l.root, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(l.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return private(dirs...) // what MkdirAll found there, made by someone else meanwhile
}

// read returns seen.json, empty when it is missing, does not parse, or
// is of another version or place: what it says can be learned again at
// the next handshake. It reads nothing from a directory that is not the
// user's own.
func (l *Local) read() seen {
	if l.ready(false, "") != nil {
		return seen{V: seenVersion, Server: l.server, Project: l.project}
	}
	if s, ok := record(l.dir, l.server, l.project); ok {
		return s
	}
	return seen{V: seenVersion, Server: l.server, Project: l.project}
}

// replace writes data as the file name of the directory, through a
// temporary file of this call's own: many processes write here, and
// each write is whole. Two that write at once lose one's change, which
// the next handshake or miss makes again.
func (l *Local) replace(name string, data []byte) error {
	if err := l.ready(true, ""); err != nil {
		return err
	}
	f, err := os.CreateTemp(l.dir, name+"-*.tmp") // mode 0600
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Close()); err == nil {
		err = os.Rename(f.Name(), filepath.Join(l.dir, name))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

// Reached notes that the server took the key with this fingerprint for
// principal, at a handshake; configured says the key
// was the one ARIES_KEY_FILE or `key:` names, and not one of several
// offered. It returns who the key was noted for before, when that was
// someone else:
// the server has moved the key, and what the other principal queued with
// it is no longer this key's to send.
//
// It writes only when something is news, or was last confirmed a day
// ago, so a machine's many commands do not each write. A server that
// names no principal, or a connection whose key is not known, is not
// noted.
func (l *Local) Reached(principal, fingerprint string, configured bool) (was string, err error) {
	if principal == "" || fingerprint == "" {
		return "", nil
	}
	if err := l.ready(false, ""); err != nil {
		return "", err
	}
	s, now := l.read(), l.opts.Now()
	k, known := s.Keys[fingerprint]
	fresh := known && k.Principal == principal && now.Sub(k.At) < 24*time.Hour && !now.Before(k.At)
	// The keys pinned now are noted only when they are not the
	// directory's own alone, so a config of one key writes the record
	// it always has.
	var pinned []string
	if !slices.Equal(l.pinned, []string{l.server}) {
		pinned = l.pinned
	}
	if fresh && (configured || s.Unconfigured == fingerprint) && s.Addr == l.opts.Address && s.Proto == proto.Proto && slices.Equal(s.HostKeys, pinned) {
		return "", nil
	}
	s.Addr, s.Proto, s.HostKeys = l.opts.Address, proto.Proto, pinned
	if known && k.Principal != principal {
		was = k.Principal
	}
	if s.Keys == nil {
		s.Keys = make(map[string]keyNote)
	}
	s.Keys[fingerprint] = keyNote{Principal: principal, At: now}
	if !configured {
		s.Unconfigured = fingerprint
	}
	for len(s.Keys) > maxKeys {
		oldest := ""
		for fp, k := range s.Keys {
			if oldest == "" || k.At.Before(s.Keys[oldest].At) {
				oldest = fp
			}
		}
		delete(s.Keys, oldest)
	}
	b, err := json.Marshal(s)
	if err == nil {
		err = l.replace(seenFile, append(b, '\n'))
	}
	if err != nil {
		return was, fmt.Errorf("note who the server took this key for: %w", err)
	}
	return was, nil
}

// Missed notes that the server was not reached just now. A failure to
// note it is dropped: it is a time shown to a person, and must not fail
// what the caller does next.
func (l *Local) Missed() {
	last, now := l.LastMissed(), l.opts.Now()
	if !last.IsZero() && now.Sub(last) < missEvery && !now.Before(last) {
		return
	}
	_ = l.replace(missedFile, []byte(now.UTC().Format(time.RFC3339)+"\n"))
}

// LastMissed is when the server was last not reached, to within a
// minute, or the zero time when it never was.
func (l *Local) LastMissed() time.Time {
	if l.ready(false, "") != nil {
		return time.Time{}
	}
	b, err := os.ReadFile(filepath.Join(l.dir, missedFile))
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(string(b))) // not a time: never
	return t
}

// Principal returns who a write belongs to while the server cannot say,
// from the fingerprints of the keys aries would offer, in order.
//
// With a key configured, which is then the only one offered, it is the
// principal the server last took that key for. With none configured,
// the server takes the first offered key it knows, which this machine
// cannot work out; so it is the principal of the key the server
// accepted the last time none was configured, and only while that key
// is still among those offered. It reports false otherwise: this
// machine has not reached the server with these keys, and nothing can
// be queued.
func (l *Local) Principal(offered []string, configured bool) (string, bool) {
	s := l.read()
	fp := s.Unconfigured
	if configured && len(offered) == 1 {
		fp = offered[0]
	}
	if fp == "" || !slices.Contains(offered, fp) {
		return "", false
	}
	k := s.Keys[fp]
	return k.Principal, k.Principal != ""
}

// Principals lists, in order, everyone the server took a key of this
// machine for, and everyone an outbox was made for here: a key the
// server has since given to someone else leaves its first principal
// with an outbox and no key.
func (l *Local) Principals() []string {
	set := map[string]bool{}
	for _, k := range l.read().Keys {
		set[k.Principal] = true
	}
	if l.ready(false, "") == nil {
		owners, _ := filepath.Glob(filepath.Join(l.dir, "outbox-*"+ownerSuffix)) // the pattern is well formed
		for _, f := range owners {
			b, err := os.ReadFile(f) //nolint:gosec // aries's own file, in a directory checked to be the user's
			if p := strings.TrimSpace(string(b)); err == nil && l.outboxDir(p)+ownerSuffix == f {
				set[p] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// ownerSuffix ends the name of the file beside an outbox that holds its
// principal's name, which the outbox's own name, a hash, does not say.
const ownerSuffix = ".principal"

// Outbox returns principal's outbox, which it makes if it is not there.
func (l *Local) Outbox(principal string) (*outbox.Outbox, error) {
	if err := l.ready(true, principal); err != nil {
		return nil, err
	}
	if _, err := os.Stat(l.outboxDir(principal) + ownerSuffix); errors.Is(err, fs.ErrNotExist) {
		if err := l.replace(filepath.Base(l.outboxDir(principal))+ownerSuffix, []byte(principal+"\n")); err != nil {
			return nil, err
		}
	}
	return outbox.Open(l.outboxDir(principal), l.opts.Outbox)
}

// Existing returns principal's outbox when its directory is there, for
// a replay or for a person to read and change; and false, making
// nothing, when nothing was ever queued for them.
func (l *Local) Existing(principal string) (*outbox.Outbox, bool, error) {
	if principal == "" {
		return nil, false, nil
	}
	if err := l.ready(false, principal); err != nil {
		return nil, false, err
	}
	if _, err := os.Stat(l.outboxDir(principal)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	ob, err := outbox.Open(l.outboxDir(principal), l.opts.Outbox)
	return ob, err == nil, err
}

// Count is how many entries wait in an outbox: for the server, and for
// a person.
type Count struct{ Pending, Refused int }

// Waiting counts what waits in principal's outbox.
func (l *Local) Waiting(principal string) (Count, error) {
	ob, ok, err := l.Existing(principal)
	if err != nil || !ok {
		return Count{}, err
	}
	list, err := ob.List()
	return Count{Pending: len(list.Pending), Refused: len(list.Refused)}, err
}

// ErrNotQueued matches, with errors.Is, every refusal of [Local.Keep]:
// the write did not reach the server and is not in an outbox either.
var ErrNotQueued = errors.New("not queued")

// notQueued is a refusal of Keep: what a person is told, as a
// *proto.Error, and the outbox's own error when it has one.
type notQueued struct {
	pe    *proto.Error
	cause error
}

func (e *notQueued) Error() string { return e.pe.Error() }

func (e *notQueued) Unwrap() []error {
	if e.cause == nil {
		return []error{e.pe, ErrNotQueued}
	}
	return []error{e.pe, ErrNotQueued, e.cause}
}

// Kept is a write that is in an outbox.
type Kept struct {
	// Entry is the write as stored; its Seq is the number `aries outbox`
	// shows.
	Entry outbox.Entry
	// Principal is whose outbox it is in.
	Principal string
	// Since is when the outage this write fell in began, as the outbox
	// saw it: the first write queued since the server last answered one.
	Since time.Time
}

// rerun is the fix of a write that could be neither sent nor queued, and
// that nothing but the server coming back mends.
const rerun = "run it again when the server answers"

// Keep puts e, a write that did not reach the server, in principal's
// outbox. reach is the error that says the server was not reached; its
// message opens the message of a refusal. principal is "" when this
// machine does not know whose write it is ([Local.Principal]), which is
// a refusal too.
//
// Every refusal is a *proto.Error with a fix (rule 11) that matches
// [ErrNotQueued] and, when the outbox refused, the outbox's error:
//
//	nobody's          unavailable  run it again when the server answers
//	outbox.ErrFull    busy         send what waits, or drop some of it
//	outbox.ErrTooLarge invalid     shorten its text
//	outbox.ErrSecret  invalid      remove the secret
//	anything else     unavailable  the directory, or run it again
func (l *Local) Keep(principal string, e outbox.Entry, reach error) (Kept, error) {
	l.Missed()
	was := reach.Error()
	if pe, ok := errors.AsType[*proto.Error](reach); ok {
		was = pe.Message
	}
	refuse := func(code proto.Code, cause error, fix, why string) (Kept, error) {
		return Kept{}, &notQueued{cause: cause, pe: proto.Errf(code, fix, fmt.Sprintf("%s, and this %s was not queued: %s", was, e.Op, why))}
	}
	if principal == "" {
		return refuse(proto.CodeUnavailable, nil,
			rerun+"; a write is queued for later only once a command of yours has reached the server from this machine with this key",
			"this machine has never reached the server with this key, so aries cannot tell whose write it is")
	}
	ob, err := l.Outbox(principal)
	var stored outbox.Entry
	if err == nil {
		stored, err = ob.Append(e)
	}
	// The outbox's errors read "queue OP: SENTINEL: why"; a person is
	// told the why.
	why := func(sentinel error) string {
		_, after, _ := strings.Cut(err.Error(), sentinel.Error()+": ")
		return after
	}
	switch {
	case err == nil:
	case errors.Is(err, outbox.ErrFull):
		return refuse(proto.CodeBusy, err,
			rerun+", which also sends what waits; `aries outbox` lists it, and `aries outbox drop N` removes an entry that is no longer wanted",
			why(outbox.ErrFull))
	case errors.Is(err, outbox.ErrTooLarge):
		return refuse(proto.CodeInvalid, err, "shorten its text and run it again, or "+rerun, why(outbox.ErrTooLarge))
	case errors.Is(err, outbox.ErrSecret):
		return refuse(proto.CodeInvalid, err,
			proto.FixSecret+" and run it again, naming where the secret is kept instead (a vault path, an environment variable)",
			why(outbox.ErrSecret))
	case errors.Is(err, outbox.ErrNotQueueable):
		return refuse(proto.CodeUnavailable, err, rerun, why(outbox.ErrNotQueueable))
	case errors.Is(err, outbox.ErrNoKey), errors.Is(err, outbox.ErrInvalid):
		return refuse(proto.CodeUnavailable, err, rerun, err.Error())
	default:
		if pe, ok := errors.AsType[*proto.Error](err); ok { // the directory is not the user's own
			return refuse(pe.Code, nil, pe.Fix, pe.Message)
		}
		return refuse(proto.CodeUnavailable, nil,
			fmt.Sprintf("check that %s can be written and its disk has room (%s names another directory), or %s", l.dir, EnvDataDir, rerun),
			"the outbox could not be written: "+err.Error())
	}
	k := Kept{Entry: stored, Principal: principal, Since: stored.At}
	if st, err := ob.Stats(); err == nil && !st.Outages.Since.IsZero() {
		k.Since = st.Outages.Since
	}
	return k, nil
}

// Conn is the part of a connection a replay uses: *client.Conn, and
// what stands in for it in tests.
type Conn interface {
	// Call runs one operation; a server refusal is a *proto.Error.
	Call(ctx context.Context, op string, args, result any) error
	// Err is non-nil once the connection is lost.
	Err() error
	// Principal is who the server took the connection's key for.
	Principal() string
}

// Replay sends what waits in the outbox of c's principal, in order, on
// c, and no other principal's (doc.go, "Whose"). It makes nothing when
// nothing was ever queued for them. The bounds are the outbox's
// ([outbox.Outbox.Replay]): one call ends within its budget, and what
// is left waits for the next.
//
// The server's answer decides what becomes of an entry. Applied, it is
// gone. Refused, it goes to the refused list with the server's code,
// message and fix, and is never sent again by itself. When the server
// was not reached with it (the connection was lost, or it answered
// unavailable), or told the caller to wait (busy), the entry stays
// pending with everything after it, and the replay stops.
//
// A replay that runs out of time closes c, as a canceled call does:
// the caller checks c.Err and connects again for its own request.
func (l *Local) Replay(ctx context.Context, c Conn) (outbox.Report, error) {
	ob, ok, err := l.Existing(c.Principal())
	if err != nil || !ok {
		return outbox.Report{}, err
	}
	return ob.Replay(ctx, func(ctx context.Context, e outbox.Entry) (outbox.Outcome, error) {
		var merged proto.UpdateResult
		var late proto.HandoffResult // a finish's comment decodes into it too
		var out any
		switch e.Op {
		case proto.OpUpdate:
			// The server compares it with the issue at its rev, and parks
			// what someone else changed since (design §18.7).
			out = &merged
		case proto.OpFinish, proto.OpHandoff:
			// The server applies it if nobody took the issue since its
			// claim, lapsed or not, and keeps it as a comment if somebody
			// did (design §18.6).
			out = &late
		}
		park := func(args json.RawMessage) json.RawMessage {
			if out == nil {
				return args
			}
			return withPark(args)
		}
		err := c.Call(ctx, e.Op, park(e.Args), out)
		if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeInvalid && c.Err() == nil {
			if bare, had := withoutPaths(e); had {
				err = c.Call(ctx, e.Op, park(bare), out)
			}
		}
		if err == nil {
			return outbox.Outcome{Parked: len(merged.Parked) > 0, Kept: merged.Comment != "" || late.Comment != ""}, nil
		}
		pe, ok := errors.AsType[*proto.Error](err)
		if !ok || c.Err() != nil || pe.Code == proto.CodeUnavailable || pe.Code == proto.CodeBusy {
			return outbox.Outcome{}, err
		}
		return outbox.Outcome{Refused: true, Code: string(pe.Code), Message: pe.Message, Fix: pe.Fix}, nil
	})
}

// Address is where cfg says the server is, as noted with what is kept
// for it: host and port, and the IAP project and zone when the server is
// reached through IAP. Two configs that give the same address name the
// same server, as far as the config can tell.
func Address(cfg *client.Config) string {
	addr := net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port))
	if iap := cfg.Server.IAP; iap != nil {
		addr += fmt.Sprintf(" through IAP (project %s, zone %s, instance %s)", iap.Project, iap.Zone, cmp.Or(iap.Instance, cfg.Server.Host))
	}
	return addr
}

// Former is what this machine keeps for the same project under a host
// key the server no longer has: a server rebuilt with a new key, or a
// checkout pointed at another server for the project. Nothing in it is
// sent by itself: a changed host key is for a person to look at.
type Former struct {
	// HostKey is the host key it was kept under.
	HostKey string
	// Address is where the server was then, or "" when the record is
	// from an aries that did not note it.
	Address string
	// Local is what was kept, to read, and to drop from.
	Local *Local
}

// Rekeyed returns, by host key, what this machine keeps for this project
// under other host keys of the server: each directory of the data
// directory but l's whose record names this project, which [Find] did
// not take for the keys pinned now. A directory with no record names no
// project, and is left out, as is one that is not the user's own.
func (l *Local) Rekeyed() ([]Former, error) {
	if err := private(l.root); err != nil {
		return nil, err
	}
	dirs, err := os.ReadDir(l.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Former
	for _, d := range dirs {
		dir := filepath.Join(l.root, d.Name())
		if !d.IsDir() || dir == l.dir {
			continue
		}
		s, ok := record(dir, "", l.project)
		if !ok {
			continue
		}
		f := Open(l.root, s.Server, s.Project, Options{Now: l.opts.Now, Outbox: l.opts.Outbox, Address: s.Addr})
		out = append(out, Former{HostKey: s.Server, Address: s.Addr, Local: f})
	}
	slices.SortFunc(out, func(a, b Former) int { return strings.Compare(a.HostKey, b.HostKey) })
	return out, nil
}

// Errors of [Local.Adopt].
var (
	// ErrOtherAddress: the entry was kept for a server at another
	// address than this one's, or at one that was not noted.
	ErrOtherAddress = errors.New("kept for a server at another address")
	// ErrNoEntry: no entry that waits has that number.
	ErrNoEntry = errors.New("no such entry")
)

// Adopt moves entry seq of principal's outbox in from, which is kept
// under a host key the server no longer has ([Local.Rekeyed]), to that
// principal's outbox here, where it waits to be sent like any other, with
// its op, arguments and idempotency key, and returns it as stored here.
// A person decides that, never aries: it is [ErrOtherAddress] unless from
// was kept for a server at this one's address, which is what says it is
// the same server. The entry is added here before it leaves from, so a
// failure between the two leaves it in both, and its key makes the
// second sending of it harmless.
func (l *Local) Adopt(from *Local, principal string, seq uint64) (outbox.Entry, error) {
	if from.opts.Address == "" || from.opts.Address != l.opts.Address {
		return outbox.Entry{}, ErrOtherAddress
	}
	ob, ok, err := from.Existing(principal)
	if err != nil {
		return outbox.Entry{}, err
	}
	if !ok {
		return outbox.Entry{}, ErrNoEntry
	}
	list, err := ob.List()
	if err != nil {
		return outbox.Entry{}, err
	}
	var e *outbox.Entry
	for i := range list.Pending {
		if list.Pending[i].Seq == seq {
			e = &list.Pending[i]
		}
	}
	for i := range list.Refused {
		if list.Refused[i].Entry.Seq == seq {
			e = &list.Refused[i].Entry
		}
	}
	if e == nil {
		return outbox.Entry{}, ErrNoEntry
	}
	to, err := l.Outbox(principal)
	if err != nil {
		return outbox.Entry{}, err
	}
	stored, err := to.Append(outbox.Entry{Op: e.Op, Args: e.Args, Session: e.Session, Machine: e.Machine})
	if err != nil {
		return outbox.Entry{}, err
	}
	if err := ob.Drop(seq); err != nil {
		return stored, fmt.Errorf("entry %d is now %d here, and is still there too: %w", seq, stored.Seq, err)
	}
	return stored, nil
}

// Ended refuses what waits in principal's outbox with the server's
// refusal pe, which said at a handshake that principal's access to the
// project has ended ([client.AccessEnded]), and returns how many entries
// it refused. They go to the refused list, as a refusal of each would
// have, where a person retries them once access is back or drops them;
// until then each would only go stale (design §18.3, "Who it runs as").
// It makes nothing when nothing was ever queued for principal, and does
// nothing while another process replays that outbox, which got in.
func (l *Local) Ended(principal string, pe *proto.Error) (int, error) {
	ob, ok, err := l.Existing(principal)
	if err != nil || !ok {
		return 0, err
	}
	n, err := ob.RefuseAll(outbox.Outcome{Refused: true, Code: string(pe.Code), Message: pe.Message, Fix: pe.Fix})
	if errors.Is(err, outbox.ErrBusy) {
		return 0, nil
	}
	return n, err
}

// withoutPaths returns the arguments of a finish or a handoff without
// the paths its work touched, and whether it had any. They are a hint
// read from git: a server that refuses them is sent the rest, as
// proto.RetryWithoutPaths does for a write that is not replayed. They
// are no part of what an idempotency key stands for.
func withoutPaths(e outbox.Entry) (json.RawMessage, bool) {
	if e.Op != proto.OpFinish && e.Op != proto.OpHandoff {
		return nil, false
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(e.Args, &args) != nil {
		return nil, false
	}
	if _, had := args["paths"]; !had {
		return nil, false
	}
	delete(args, "paths")
	b, err := json.Marshal(args)
	return b, err == nil
}

// withPark returns an update's arguments with park set, which asks the
// server to park what conflicts rather than refuse a stale rev. Arguments
// that do not parse are returned as they are, for the server to refuse.
func withPark(args json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(args, &fields) != nil {
		return args
	}
	fields["park"] = json.RawMessage("true")
	b, err := json.Marshal(fields)
	if err != nil {
		return args
	}
	return b
}

// Target is what an entry is about, for a list a person reads: the
// issue's id, or the memory's key, or "" when its op names neither, as
// a create does.
func Target(e outbox.Entry) string {
	var a struct {
		ID    string `json:"id"`
		Issue string `json:"issue"`
		Key   string `json:"key"`
		From  string `json:"from"`
	}
	_ = json.Unmarshal(e.Args, &a) // arguments that do not parse name nothing
	switch e.Op {
	case proto.OpRemember:
		return a.Key
	case proto.OpDepAdd:
		return a.From
	case proto.OpHoursLog:
		return a.Issue
	}
	return a.ID
}
