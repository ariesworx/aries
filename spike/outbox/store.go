package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// The files of one store directory.
const (
	outboxFile   = "outbox.ndjson"
	answeredFile = "answered.ndjson"
	appendLock   = "append.lock"
	replayLock   = "replay.lock"
)

// entry is one queued operation, the size and shape of a handoff. Sum is a
// checksum of Text, so a line made of two entries' bytes cannot pass for
// one.
type entry struct {
	Proc int    `json:"proc"`
	Seq  int    `json:"seq"`
	Key  string `json:"key"`
	Op   string `json:"op"`
	Text string `json:"text"`
	Sum  uint32 `json:"sum"`
}

const alphabet = "abcdefghijklmnopqrstuvwxyz      ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.,\"\\"

func fill(rng *rand.Rand, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}

func key(proc, seq int) string { return fmt.Sprintf("%d-%d", proc, seq) }

// makeEntry is one line of the outbox, newline included, of about size
// bytes.
func makeEntry(proc, seq int, rng *rand.Rand, size int) []byte {
	text := fill(rng, max(size-90, 16))
	b, err := json.Marshal(entry{
		Proc: proc, Seq: seq, Key: key(proc, seq), Op: "handoff",
		Text: text, Sum: crc32.ChecksumIEEE([]byte(text)),
	})
	if err != nil {
		panic(err) // cannot happen: the struct is strings and numbers
	}
	return append(b, '\n')
}

// parseEntry reads one line, without its newline. A torn or mixed line is
// refused.
func parseEntry(line []byte) (entry, bool) {
	var e entry
	if err := json.Unmarshal(line, &e); err != nil {
		return e, false
	}
	return e, e.Key == key(e.Proc, e.Seq) && e.Sum == crc32.ChecksumIEEE([]byte(e.Text))
}

// repairTail ends a file's last line if a writer died partway through it,
// so that the next line starts a line of its own; without it, the next
// entry would be joined to the fragment and thrown away with it. The
// caller holds the lock that serializes writers. A fragment is never
// valid JSON unless it is a whole entry that lacks only its newline, which
// this then completes.
func repairTail(f *os.File) (bool, error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return false, err
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], st.Size()-1); err != nil {
		return false, err
	}
	if last[0] == '\n' {
		return false, nil
	}
	_, err = f.Write([]byte{'\n'})
	return true, err
}

// outbox appends entries as §18.4 proposes: take the append lock, write the
// line, sync, release.
type outbox struct {
	cfg  config
	id   int
	lk   locker
	path string
	kept *os.File // the outbox, held open across appends, with cfg.KeepOpen
	ack  *os.File
}

func openOutbox(cfg config, id int) (*outbox, error) {
	lk, err := openLock(cfg.Lock, filepath.Join(cfg.Dir, appendLock), cfg.Lease, cfg.Poll)
	if err != nil {
		return nil, err
	}
	o := &outbox{cfg: cfg, id: id, lk: lk, path: filepath.Join(cfg.Dir, outboxFile)}
	if cfg.KeepOpen {
		if o.kept, err = o.open(); err != nil {
			return nil, err
		}
	}
	if cfg.Ack {
		o.ack, err = os.OpenFile(filepath.Join(cfg.Dir, "ack", strconv.Itoa(id)), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (o *outbox) open() (*os.File, error) {
	return os.OpenFile(o.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
}

// appendTimes is where one append's time went.
type appendTimes struct {
	wait, write, sync, total time.Duration
	repaired                 bool // it ended a line a dead writer had left
	lost                     bool // the lock was taken from it while it held it
}

// append adds one line. With cfg.Ack it notes "L seq" in the process's ack
// file once it holds the lock and "U seq" just before it lets go, so the
// parent can tell an append that returned from one that was cut short, and
// a process killed while it held the lock from one that was not.
func (o *outbox) append(line []byte, seq int) (at appendTimes, err error) {
	t0 := time.Now()
	if err := o.lk.Lock(); err != nil {
		return at, fmt.Errorf("lock: %w", err)
	}
	t1 := time.Now()
	at.wait = t1.Sub(t0)
	defer func() {
		if err == nil && o.ack != nil {
			_, err = fmt.Fprintf(o.ack, "U %d\n", seq)
		}
		if uerr := o.lk.Unlock(); errors.Is(uerr, errLockLost) {
			at.lost = true
		} else if err == nil {
			err = uerr
		}
		at.total = time.Since(t0)
	}()
	if o.ack != nil {
		if _, err := fmt.Fprintf(o.ack, "L %d\n", seq); err != nil {
			return at, err
		}
	}
	if o.cfg.Holder {
		if err := os.WriteFile(filepath.Join(o.cfg.Dir, "holder"), []byte(strconv.Itoa(o.id)), 0o600); err != nil {
			return at, err
		}
	}

	// The outbox is opened only now, under the lock: compaction replaces
	// it by rename, and a descriptor from before would be of the old file.
	f := o.kept
	if f == nil {
		if f, err = o.open(); err != nil {
			return at, err
		}
		defer func() {
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}()
	}
	if at.repaired, err = repairTail(f); err != nil {
		return at, err
	}
	if o.cfg.Chunked {
		// Two writes with a pause, as a short write or a full disk would
		// make them: a kill between them tears the line.
		half := len(line) / 2
		if _, err := f.Write(line[:half]); err != nil {
			return at, err
		}
		time.Sleep(o.cfg.ChunkPause)
		line = line[half:]
	}
	if _, err := f.Write(line); err != nil {
		return at, err
	}
	t2 := time.Now()
	at.write = t2.Sub(t1)
	if o.cfg.Fsync {
		if err := f.Sync(); err != nil {
			return at, err
		}
		at.sync = time.Since(t2)
	}
	return at, nil
}

// scan is what a reader finds in an outbox file.
type scan struct {
	entries []entry
	bad     int    // whole lines that are not an entry: skipped
	sample  string // the first of them
	tail    int    // bytes after the last newline: a line not finished
	tailIs  string
	bytes   int
}

// readOutbox reads the file as a reader that trusts nothing would: a line
// that does not parse is skipped and counted, and bytes after the last
// newline are not a line yet.
func readOutbox(path string) (scan, error) {
	var s scan
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	s.bytes = len(b)
	for len(b) > 0 {
		line, rest, whole := bytes.Cut(b, []byte{'\n'})
		if !whole {
			s.tail, s.tailIs = len(line), clip(line)
			break
		}
		b = rest
		if e, ok := parseEntry(line); ok {
			s.entries = append(s.entries, e)
		} else {
			if s.bad == 0 {
				s.sample = clip(line)
			}
			s.bad++
		}
	}
	return s, nil
}

// clip shows the two ends of a torn line.
func clip(line []byte) string {
	if len(line) <= 72 {
		return fmt.Sprintf("%q (%d bytes)", line, len(line))
	}
	return fmt.Sprintf("%q ... %q (%d bytes)", line[:40], line[len(line)-24:], len(line))
}

// truncateTail cuts an unfinished last line off, which is the other thing
// a reader may do with it. The caller holds the append lock, or knows no
// writer is alive.
func truncateTail(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	end := bytes.LastIndexByte(b, '\n') + 1
	if end == len(b) {
		return 0, nil
	}
	return len(b) - end, os.Truncate(path, int64(end))
}

// syncDir makes a rename in dir durable. Windows has no such call.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// replaceFile writes data to a temporary file beside path and renames it
// into place.
func replaceFile(path string, data []byte, sync bool) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && sync {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
