package outbox

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// The files of an outbox's directory (doc.go, "Files").
const (
	outboxFile     = "outbox.ndjson"
	answeredFile   = "answered.ndjson"
	statsFile      = "stats.json"
	appendLockFile = "append.lock"
	replayLockFile = "replay.lock"
)

// Defaults of [Options].
const (
	// DefaultMaxEntries is how many entries may wait, pending and refused
	// together.
	DefaultMaxEntries = 1000
	// DefaultMaxBytes is how large the outbox file may grow.
	DefaultMaxBytes = 4 << 20
	// DefaultMaxEntryBytes is how large one entry's line may be.
	DefaultMaxEntryBytes = 256 << 10
	// DefaultMaxAge is how long an entry may wait and still be sent. It
	// is 24 hours because today's server refuses an idempotency key older
	// than that; protocol 6 raises the window to 7 days (design §18.5).
	DefaultMaxAge = 24 * time.Hour
	// DefaultSendTimeout bounds the delivery of one entry.
	DefaultSendTimeout = 5 * time.Second
	// DefaultReplayBudget bounds one call of [Outbox.Replay].
	DefaultReplayBudget = 5 * time.Second
)

// Errors. Every refusal of this package wraps one of them, with a
// message that names the cause, so a caller can tell them apart with
// [errors.Is] and turn each into a proto.Error with a fix (rule 11). Any
// other error is a failure of the local files.
var (
	// ErrNotQueueable: the op is never queued, or not yet.
	ErrNotQueueable = errors.New("not queueable")
	// ErrNoKey: the arguments carry no idempotency key. Replay is
	// at-least-once, and the key is what makes that safe.
	ErrNoKey = errors.New("no idempotency key")
	// ErrInvalid: the entry is malformed.
	ErrInvalid = errors.New("invalid entry")
	// ErrSecret: the arguments look like they hold a credential.
	ErrSecret = errors.New("looks like a secret")
	// ErrTooLarge: one entry is larger than MaxEntryBytes.
	ErrTooLarge = errors.New("entry too large")
	// ErrFull: the outbox is at its cap on entries or on bytes.
	ErrFull = errors.New("outbox full")
	// ErrNotFound: no entry with that sequence number is in the state
	// the call needs.
	ErrNotFound = errors.New("no such entry")
	// ErrBusy: a process is replaying, so an entry's state cannot be
	// changed by hand now. It is over within that replay's budget.
	ErrBusy = errors.New("outbox busy")
)

// Options are an outbox's bounds and its clock. A field that is zero or
// less takes its default.
type Options struct {
	// MaxEntries caps the entries waiting, pending and refused together.
	MaxEntries int
	// MaxBytes caps the size of the outbox file.
	MaxBytes int64
	// MaxEntryBytes caps one entry's line.
	MaxEntryBytes int
	// MaxAge is how long an entry may wait and still be sent.
	MaxAge time.Duration
	// SendTimeout bounds the delivery of one entry, so a replayer that
	// is alive and stuck does not hold the replay lock without bound.
	SendTimeout time.Duration
	// ReplayBudget bounds one call of Replay, deliveries included.
	ReplayBudget time.Duration
	// Now is the client's clock; time.Now when nil. It dates entries and
	// answers and decides what is stale. The two bounds above are
	// deadlines of a context, and run on the process's own timers.
	Now func() time.Time
}

// Outbox is the queue in one directory. It holds no file open between
// calls, and is safe for use by several goroutines, as the directory is
// by several processes.
type Outbox struct {
	dir  string
	opts Options
}

// Open returns the outbox in dir, which it makes if it is not there and
// keeps at mode 0700. The caller chooses dir: one for each server,
// project and principal, on a local disk (doc.go).
func Open(dir string, opts Options) (*Outbox, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the directory its caller chose
		return nil, fmt.Errorf("open outbox: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
		return nil, fmt.Errorf("open outbox: %w", err)
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxEntryBytes <= 0 {
		opts.MaxEntryBytes = DefaultMaxEntryBytes
	}
	if opts.MaxAge <= 0 {
		opts.MaxAge = DefaultMaxAge
	}
	if opts.SendTimeout <= 0 {
		opts.SendTimeout = DefaultSendTimeout
	}
	if opts.ReplayBudget <= 0 {
		opts.ReplayBudget = DefaultReplayBudget
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Outbox{dir: dir, opts: opts}, nil
}

func (o *Outbox) path(name string) string { return filepath.Join(o.dir, name) }

func (o *Outbox) now() time.Time { return o.opts.Now() }

// locked runs f holding the append lock, which is held for one append
// or one read of the files and never across a delivery.
func (o *Outbox) locked(f func() error) error {
	lk, err := acquire(o.path(appendLockFile), true)
	if err != nil {
		return err
	}
	return errors.Join(f(), lk.release())
}

// Append queues e and returns it as stored, or refuses it with one of
// the package's errors. The caller sets Op, Args, Session and Machine;
// the rest is Append's. When Append returns without error the entry is
// on disk and synced: nothing is queued in memory alone.
func (o *Outbox) Append(e Entry) (Entry, error) {
	e, err := o.prepare(e)
	if err != nil {
		return Entry{}, err
	}
	err = o.locked(func() error {
		// The outbox is opened only now, under the lock, every time:
		// compaction replaces the file, and a descriptor from before
		// would be of the old one, whose writes succeed and are lost
		// (spike/outbox/RESULTS.md, section 5).
		f, err := os.OpenFile(o.path(outboxFile), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // the outbox's own file
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // what was written is synced below
		data, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		v, err := o.view(data)
		if err != nil {
			return err
		}
		if n := v.live(); n >= o.opts.MaxEntries {
			return fmt.Errorf("%w: %d entries wait, which is the cap", ErrFull, n)
		}
		if v.next >= maxSeq {
			return fmt.Errorf("%w: its file says every sequence number is used, which only damage does", ErrFull)
		}
		e.Seq = v.next
		line, err := encode(e)
		if err != nil {
			return err
		}
		if len(line) > o.opts.MaxEntryBytes {
			return fmt.Errorf("%w: the %s entry is %d bytes, and the cap is %d", ErrTooLarge, e.Op, len(line), o.opts.MaxEntryBytes)
		}
		if v.torn {
			// A writer died partway through the last line. End it, in
			// the same write, so this entry is a line of its own and not
			// thrown away with the fragment.
			line = append([]byte{'\n'}, line...)
		}
		if int64(v.size)+int64(len(line)) > o.opts.MaxBytes {
			return fmt.Errorf("%w: the outbox holds %d bytes, and the cap is %d", ErrFull, v.size, o.opts.MaxBytes)
		}
		// One write for the whole line, and a sync before the append
		// returns. If either fails the caller is told the operation was
		// not queued, so what reached the file is taken out again.
		if _, err := f.Write(line); err != nil {
			_ = os.Truncate(o.path(outboxFile), int64(v.size))
			return err
		}
		if err := f.Sync(); err != nil {
			_ = os.Truncate(o.path(outboxFile), int64(v.size))
			return err
		}
		if v.size == 0 {
			// The file may be new, and then its name is only as durable
			// as the directory. A compacted outbox is never empty, so
			// this is once for a directory.
			if err := syncDir(o.dir); err != nil {
				_ = os.Truncate(o.path(outboxFile), 0)
				return err
			}
		}
		o.count(func(s *Stats) { s.queued(e.Op, e.At) })
		return nil
	})
	if err != nil {
		return Entry{}, fmt.Errorf("queue %s: %w", e.Op, err)
	}
	return e, nil
}

// view is the outbox file and the answered file read together.
type view struct {
	scan
	state   map[uint64]answer // the last answer of each entry that is in the file
	answers int               // the answers in the answered file, of any entry
}

// pending reports whether the entry seq waits to be sent.
func (v *view) pending(seq uint64) bool {
	a, ok := v.state[seq]
	return !ok || a.State == statePending
}

// refused reports whether the entry seq waits for a person.
func (v *view) refused(seq uint64) bool { return v.state[seq].State == stateRefused }

// live counts the entries that wait, for the server or for a person.
func (v *view) live() int {
	n := 0
	for _, e := range v.entries {
		if v.pending(e.Seq) || v.refused(e.Seq) {
			n++
		}
	}
	return n
}

// view reads the answered file beside the outbox file's data. The caller
// holds the append lock, under which the answered file may gain lines
// and is never replaced.
func (o *Outbox) view(data []byte) (*view, error) {
	v := &view{scan: scanOutbox(data), state: make(map[uint64]answer)}
	b, err := os.ReadFile(o.path(answeredFile)) //nolint:gosec // the outbox's own file
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	last, n := scanAnswers(b)
	v.answers = n
	for _, e := range v.entries {
		if a, ok := last[e.Seq]; ok {
			v.state[e.Seq] = a
		}
	}
	// A new entry must not take a number an answer already names, or it
	// would be taken for answered and never sent. The outbox file alone
	// says which numbers are used, unless it was lost or damaged and the
	// answers were not.
	for seq := range last {
		v.next = max(v.next, seq+1)
	}
	return v, nil
}

// load reads both files. The caller holds the append lock: no file of
// the outbox is opened without it, so nothing has one open when a
// compaction, which holds it too, renames over it (doc.go, "Windows").
func (o *Outbox) load() (*view, error) {
	data, err := os.ReadFile(o.path(outboxFile)) //nolint:gosec // the outbox's own file
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return o.view(data)
}

// Refusal is an entry the server refused, or one that went stale, with
// why. It is never sent again by itself.
type Refusal struct {
	Entry Entry
	// Code, Message and Fix are the server's, or this package's for a
	// stale entry ([CodeStale]).
	Code    string
	Message string
	Fix     string
	// At is when it was refused.
	At time.Time
}

// Listing is what waits in an outbox.
type Listing struct {
	// Pending are the entries not yet answered, in the order of replay.
	Pending []Entry
	// Refused are the entries that wait for a person to retry or drop.
	Refused []Refusal
}

// List returns what waits.
func (o *Outbox) List() (Listing, error) {
	var l Listing
	err := o.locked(func() error {
		v, err := o.load()
		if err != nil {
			return err
		}
		for _, e := range v.entries {
			switch a := v.state[e.Seq]; {
			case v.pending(e.Seq):
				l.Pending = append(l.Pending, e)
			case v.refused(e.Seq):
				l.Refused = append(l.Refused, Refusal{Entry: e, Code: a.Code, Message: a.Message, Fix: a.Fix, At: a.At})
			}
		}
		return nil
	})
	if err != nil {
		return Listing{}, fmt.Errorf("list outbox: %w", err)
	}
	return l, nil
}
