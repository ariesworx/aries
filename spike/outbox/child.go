package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// runChild is what a re-executed process does.
func runChild(role string, id int, cfg config) error {
	switch role {
	case "append":
		return childAppend(cfg, id)
	case "counter":
		return childCounter(cfg, id)
	case "hold":
		return childHold(cfg)
	case "waiter":
		return childWaiter(cfg, id)
	case "try":
		return childTry(cfg, id)
	case "grab":
		return childGrab(cfg, id)
	case "replay":
		return childReplay(cfg, id)
	case "cache":
		return childCache(cfg, id)
	}
	return fmt.Errorf("no role %q", role)
}

func newRand(id int) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(id), uint64(time.Now().UnixNano())))
}

// childAppend appends cfg.M entries.
func childAppend(cfg config, id int) error {
	ob, err := openOutbox(cfg, id)
	if err != nil {
		return err
	}
	rng, res := newRand(id), newResult()
	if err := ready(cfg.Dir, "append", id); err != nil {
		return err
	}
	for seq := range cfg.M {
		line := makeEntry(id, seq, rng, cfg.SizeMin+rng.IntN(cfg.SizeMax-cfg.SizeMin+1))
		at, err := ob.append(line, seq)
		if err != nil {
			return fmt.Errorf("append %d: %w", seq, err)
		}
		res.add("total", at.total)
		res.add("wait", at.wait)
		res.add("write", at.write)
		if cfg.Fsync {
			res.add("sync", at.sync)
		}
		res.N["bytes"] += int64(len(line))
		if at.repaired {
			res.N["repaired"]++
		}
		if at.lost {
			res.N["lock_lost"]++
		}
	}
	if x, ok := ob.lk.(*exclLock); ok {
		res.N["steals"] = int64(x.steals)
	}
	return res.write(cfg.Dir, "append", id)
}

// childCounter adds one to the number in a file, cfg.Iter times, under the
// lock. The read and the write are separate calls, so two holders at once
// lose an addition.
func childCounter(cfg config, id int) error {
	lk, err := openLock(cfg.Lock, filepath.Join(cfg.Dir, appendLock), cfg.Lease, cfg.Poll)
	if err != nil {
		return err
	}
	path, res := filepath.Join(cfg.Dir, "counter"), newResult()
	if err := ready(cfg.Dir, "counter", id); err != nil {
		return err
	}
	for range cfg.Iter {
		t0 := time.Now()
		if err := lk.Lock(); err != nil {
			return err
		}
		res.add("wait", time.Since(t0))
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(string(b))
		if err != nil {
			// A read between another holder's truncate and write.
			res.N["garbled"]++
		} else if err := os.WriteFile(path, []byte(strconv.Itoa(n+1)), 0o600); err != nil {
			return err
		}
		if err := lk.Unlock(); errors.Is(err, errLockLost) {
			res.N["lock_lost"]++
		} else if err != nil {
			return err
		}
	}
	if x, ok := lk.(*exclLock); ok {
		res.N["steals"] = int64(x.steals)
	}
	return res.write(cfg.Dir, "counter", id)
}

// childHold takes the lock and keeps it until it is killed.
func childHold(cfg config) error {
	lk, err := openLock(cfg.Lock, filepath.Join(cfg.Dir, appendLock), cfg.Lease, cfg.Poll)
	if err != nil {
		return err
	}
	if err := lk.Lock(); err != nil {
		return err
	}
	if err := touch(filepath.Join(cfg.Dir, "held")); err != nil {
		return err
	}
	for {
		time.Sleep(time.Hour)
		runtime.KeepAlive(lk) // or its file is closed when collected, and the lock with it
	}
}

// childWaiter waits for the lock and notes when it got it.
func childWaiter(cfg config, id int) error {
	lk, err := openLock(cfg.Lock, filepath.Join(cfg.Dir, appendLock), cfg.Lease, cfg.Poll)
	if err != nil {
		return err
	}
	if err := touch(filepath.Join(cfg.Dir, "ready", fmt.Sprintf("waiter-%d", id))); err != nil {
		return err
	}
	if err := lk.Lock(); err != nil {
		return err
	}
	got := time.Now().UnixNano()
	return os.WriteFile(filepath.Join(cfg.Dir, "got"), []byte(strconv.FormatInt(got, 10)), 0o600)
}

// childTry says whether the lock is free, by trying it once.
func childTry(cfg config, id int) error {
	lk, err := openLock(cfg.Lock, filepath.Join(cfg.Dir, appendLock), cfg.Lease, cfg.Poll)
	if err != nil {
		return err
	}
	ok, err := lk.TryLock()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.Dir, fmt.Sprintf("try-%d", id)), []byte(yesNo(ok)), 0o600)
}

// childGrab tries the lock once, at the same moment as every other child,
// and says whether it got it. It never unlocks.
func childGrab(cfg config, id int) error {
	lk, err := openLock(cfg.Lock, filepath.Join(cfg.Dir, appendLock), cfg.Lease, cfg.Poll)
	if err != nil {
		return err
	}
	res := newResult()
	if err := ready(cfg.Dir, "grab", id); err != nil {
		return err
	}
	ok, err := lk.TryLock()
	if err != nil {
		return err
	}
	if ok {
		res.N["got"] = 1
	}
	return res.write(cfg.Dir, "grab", id)
}

// replayer delivers the outbox to the stand-in server, one entry at a
// time, while it holds the replay lock.
type replayer struct {
	cfg config
	id  int
	lk  locker   // the replay lock
	app locker   // the append lock, taken to compact
	srv *os.File // what this process sent the server, and when
	res *result

	answered     map[string]bool
	ans          *os.File
	offset       int64 // how far into the outbox it has read
	sinceCompact int
	lastRenew    time.Time
}

func childReplay(cfg config, id int) error {
	r := &replayer{cfg: cfg, id: id, res: newResult()}
	var err error
	if r.lk, err = openLock(cfg.Lock, filepath.Join(cfg.Dir, replayLock), cfg.Lease, cfg.Poll); err != nil {
		return err
	}
	if r.app, err = openLock(cfg.Lock, filepath.Join(cfg.Dir, appendLock), cfg.Lease, cfg.Poll); err != nil {
		return err
	}
	r.srv, err = os.OpenFile(filepath.Join(cfg.Dir, "server", strconv.Itoa(id)), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := ready(cfg.Dir, "replay", id); err != nil {
		return err
	}
	holder := filepath.Join(cfg.Dir, "holders", strconv.Itoa(id))
	for {
		if err := r.lk.Lock(); err != nil {
			return err
		}
		r.log('A', "-")
		if err := touch(holder); err != nil {
			return err
		}
		done, err := r.hold()
		if err != nil {
			return err
		}
		_ = os.Remove(holder)
		if err := r.lk.Unlock(); errors.Is(err, errLockLost) {
			r.res.N["lock_lost"]++
		} else if err != nil {
			return err
		}
		if done {
			return r.res.write(cfg.Dir, "replay", id)
		}
	}
}

// log notes an event with the wall clock: A, the lock acquired; B and E,
// the beginning and end of one entry's delivery; X, the lease found lost.
func (r *replayer) log(kind byte, key string) {
	fmt.Fprintf(r.srv, "%c %s %d\n", kind, key, time.Now().UnixNano())
}

// hold replays until nothing is left. It reports false if it stopped
// because its lease was taken.
func (r *replayer) hold() (bool, error) {
	ansPath := filepath.Join(r.cfg.Dir, answeredFile)
	r.answered = map[string]bool{}
	b, err := os.ReadFile(ansPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if end := bytes.LastIndexByte(b, '\n'); end >= 0 {
		for k := range strings.SplitSeq(string(b[:end]), "\n") {
			r.answered[k] = true
		}
	}
	if err := r.openAnswered(); err != nil {
		return false, err
	}
	defer func() { r.ans.Close() }()
	r.offset, r.lastRenew = 0, time.Now()

	for {
		// Read whether to stop before reading the outbox: the stop file
		// is made after the last append.
		stop := r.cfg.Drain || exists(filepath.Join(r.cfg.Dir, "stop"))
		lines, err := r.next()
		if err != nil {
			return false, err
		}
		for _, line := range lines {
			e, ok := parseEntry(line)
			if !ok {
				r.res.N["bad_lines_skipped"]++
				continue
			}
			if r.answered[e.Key] {
				continue
			}
			if held, err := r.renew(); err != nil || !held {
				return false, err
			}
			r.log('B', e.Key)
			time.Sleep(r.cfg.RPC)
			r.log('E', e.Key)
			if _, err := r.ans.WriteString(e.Key + "\n"); err != nil {
				return false, err
			}
			if r.cfg.SyncAnswered {
				if err := r.ans.Sync(); err != nil {
					return false, err
				}
			}
			r.answered[e.Key] = true
			r.res.N["delivered"]++
			r.sinceCompact++
			if r.cfg.CompactEvery > 0 && r.sinceCompact >= r.cfg.CompactEvery {
				if err := r.compact(); err != nil {
					return false, fmt.Errorf("compact: %w", err)
				}
				break // the rest of these lines are of the old file
			}
		}
		if len(lines) > 0 {
			continue
		}
		if stop {
			return true, nil
		}
		if held, err := r.renew(); err != nil || !held {
			return false, err
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *replayer) openAnswered() error {
	f, err := os.OpenFile(filepath.Join(r.cfg.Dir, answeredFile), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	r.ans = f
	_, err = repairTail(f)
	return err
}

// renew extends a lease, if the lock is one, a quarter of the way through
// it, and reports whether the lock is still held.
func (r *replayer) renew() (bool, error) {
	rn, ok := r.lk.(renewer)
	if !ok || time.Since(r.lastRenew) < r.cfg.Lease/4 {
		return true, nil
	}
	r.lastRenew = time.Now()
	held, err := rn.Renew()
	if err == nil && !held {
		r.log('X', "-")
		r.res.N["lease_lost"]++
	}
	return held, err
}

// next returns the whole lines appended since the last call. It takes no
// lock: appenders only add, and a line still being written has no newline
// yet.
func (r *replayer) next() ([][]byte, error) {
	f, err := os.Open(filepath.Join(r.cfg.Dir, outboxFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(r.offset, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	end := bytes.LastIndexByte(b, '\n') + 1
	if end < len(b) {
		r.res.N["unfinished_tail_seen"]++
	}
	if end == 0 {
		return nil, nil
	}
	r.offset += int64(end)
	return bytes.Split(b[:end-1], []byte{'\n'}), nil
}

// compact rewrites the outbox without the entries that were answered and
// empties the answered file. The caller holds the replay lock; this takes
// the append lock as well, in that order, so appenders wait while it runs.
// The outbox is replaced first: a death between the two renames leaves
// answered keys with no entry, which costs nothing, where the other order
// would leave entries with no answer, to be delivered again.
func (r *replayer) compact() (err error) {
	t0 := time.Now()
	if err := r.app.Lock(); err != nil {
		return err
	}
	t1 := time.Now()
	defer func() {
		if uerr := r.app.Unlock(); errors.Is(uerr, errLockLost) {
			r.res.N["lock_lost"]++
		} else if err == nil {
			err = uerr
		}
		r.res.add("compact_wait", t1.Sub(t0))
		r.res.add("compact_work", time.Since(t1))
	}()
	path := filepath.Join(r.cfg.Dir, outboxFile)
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var keep []byte
	for len(b) > 0 {
		line, rest, whole := bytes.Cut(b, []byte{'\n'})
		if !whole {
			break // under the append lock, a line without an end is a dead writer's
		}
		b = rest
		if e, ok := parseEntry(line); ok && !r.answered[e.Key] {
			keep = append(append(keep, line...), '\n')
		}
	}
	if err := replaceFile(path, keep, true); err != nil {
		return err
	}
	if err := r.ans.Close(); err != nil {
		return err
	}
	if err := replaceFile(filepath.Join(r.cfg.Dir, answeredFile), nil, true); err != nil {
		return err
	}
	if err := syncDir(r.cfg.Dir); err != nil {
		return err
	}
	r.answered, r.offset, r.sinceCompact = map[string]bool{}, 0, 0
	return r.openAnswered()
}

// cacheDoc is one cached answer.
type cacheDoc struct {
	K    int    `json:"k"`
	Proc int    `json:"proc"`
	N    int    `json:"n"`
	Body string `json:"body"`
	Sum  uint32 `json:"sum"`
}

// cachePath names a cache file by a hash of what it holds, as §18.4 has
// it.
func cachePath(dir string, k int) string {
	sum := sha256.Sum256([]byte("show:" + strconv.Itoa(k)))
	return filepath.Join(dir, "cache", hex.EncodeToString(sum[:16]))
}

func makeDoc(k, proc, n int, rng *rand.Rand) []byte {
	body := fill(rng, 200+rng.IntN(16<<10))
	b, err := json.Marshal(cacheDoc{K: k, Proc: proc, N: n, Body: body, Sum: crc32.ChecksumIEEE([]byte(body))})
	if err != nil {
		panic(err) // cannot happen: the struct is strings and numbers
	}
	return b
}

// writeCache writes a cache file whole: a temporary file in the same
// directory, then a rename over the name. It returns how many times the
// rename had to be tried again.
func writeCache(dir string, k int, doc []byte, sync bool) (int, error) {
	f, err := os.CreateTemp(filepath.Join(dir, "cache"), "tmp-*") // mode 0600
	if err != nil {
		return 0, err
	}
	_, err = f.Write(doc)
	if err == nil && sync {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		_ = os.Remove(f.Name())
		return 0, err
	}
	for try := 0; ; try++ {
		if err = os.Rename(f.Name(), cachePath(dir, k)); err == nil {
			return try, nil
		}
		if try == 100 {
			_ = os.Remove(f.Name())
			return try, err
		}
		time.Sleep(time.Millisecond)
	}
}

// checkDoc says whether b is a whole cache file for k.
func checkDoc(b []byte, k int) bool {
	var d cacheDoc
	return json.Unmarshal(b, &d) == nil && d.K == k && d.Sum == crc32.ChecksumIEEE([]byte(d.Body))
}

// cause is an error without the path it happened at.
func cause(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err.Error()
	}
	return err.Error()
}

// childCache makes cfg.Ops reads and writes of cfg.Files cache files,
// three writes in ten.
func childCache(cfg config, id int) error {
	rng, res := newRand(id), newResult()
	if err := ready(cfg.Dir, "cache", id); err != nil {
		return err
	}
	for n := range cfg.Ops {
		k := rng.IntN(cfg.Files)
		if rng.IntN(10) < 3 {
			doc := makeDoc(k, id, n, rng)
			t0 := time.Now()
			retries, err := writeCache(cfg.Dir, k, doc, cfg.Fsync)
			res.add("write", time.Since(t0))
			res.N["writes"]++
			res.N["rename_retries"] += int64(retries)
			if err != nil {
				res.N["write failed: "+cause(err)]++
			}
			continue
		}
		t0 := time.Now()
		b, err := os.ReadFile(cachePath(cfg.Dir, k))
		res.add("read", time.Since(t0))
		res.N["reads"]++
		switch {
		case errors.Is(err, fs.ErrNotExist):
			res.N["read_missing"]++
		case err != nil:
			res.N["read failed: "+cause(err)]++
		case !checkDoc(b, k):
			res.N["read_partial"]++
		}
	}
	return res.write(cfg.Dir, "cache", id)
}
