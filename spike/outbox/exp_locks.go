package main

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// table prints rows as a Markdown table, so a run can be pasted into
// RESULTS.md as it is.
type table struct{ cols int }

func newTable(cols ...string) *table {
	fmt.Println("| " + strings.Join(cols, " | ") + " |")
	fmt.Println(strings.Repeat("|---", len(cols)) + "|")
	return &table{cols: len(cols)}
}

func (t *table) row(cells ...any) {
	if len(cells) != t.cols {
		panic(fmt.Sprintf("table: %d cells for %d columns", len(cells), t.cols)) // a mistake in this file
	}
	s := make([]string, len(cells))
	for i, c := range cells {
		s[i] = fmt.Sprint(c)
	}
	fmt.Println("| " + strings.Join(s, " | ") + " |")
}

// expLocks measures each lock primitive by itself.
func expLocks(o opts) error {
	n := o.n()

	fmt.Printf("Exclusion: %d processes each add one to a number in a file %d times, under the lock.\n\n", n, o.cfg.Iter)
	t := newTable("lock", "additions", "lost", "took, s", "locks a second", "wait p50, ms", "p99", "max", "take-overs", "unlocks that found the lock taken")
	for _, kind := range o.locks {
		dir, err := newStore(o.base, "locks")
		if err != nil {
			return err
		}
		cfg := o.cfg
		cfg.Dir, cfg.Lock = dir, kind
		counter := filepath.Join(dir, "counter")
		if err := os.WriteFile(counter, []byte("0"), 0o600); err != nil {
			return err
		}
		kids, err := spawnN(cfg, "counter", n, 0)
		if err != nil {
			return err
		}
		start, err := release(dir, n)
		if err != nil {
			killAll(kids)
			return err
		}
		failed, stalled := waitAll(kids, o.timeout)
		took := time.Since(start)
		if failed+stalled > 0 {
			return fmt.Errorf("%s: %d children failed and %d stalled", kind, failed, stalled)
		}
		res, err := readResults(dir)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(counter)
		if err != nil {
			return err
		}
		got, _ := strconv.Atoi(string(b))
		want := n * cfg.Iter
		w := res.Lat["wait"]
		t.row(kind, want, want-got, fmt.Sprintf("%.2f", took.Seconds()),
			fmt.Sprintf("%.0f", float64(want)/took.Seconds()),
			pct(w, 0.5), pct(w, 0.99), pct(w, 1), res.N["steals"], res.N["lock_lost"])
	}

	fmt.Printf("\nOne process alone, and what the lock belongs to.\n\n")
	t = newTable("lock", "lock and unlock, alone, µs", "a second handle in the same process is kept out", "another process is kept out after the holder opens and closes the lock file again")
	for _, kind := range o.locks {
		dir, err := newStore(o.base, "locks")
		if err != nil {
			return err
		}
		cfg := o.cfg
		cfg.Dir, cfg.Lock = dir, kind
		path := filepath.Join(dir, appendLock)
		l1, err := openLock(kind, path, cfg.Lease, cfg.Poll)
		if err != nil {
			return err
		}
		const loops = 5000
		t0 := time.Now()
		for range loops {
			if err := l1.Lock(); err != nil {
				return fmt.Errorf("%s: %w", kind, err)
			}
			if err := l1.Unlock(); err != nil {
				return fmt.Errorf("%s: %w", kind, err)
			}
		}
		alone := time.Since(t0) / loops

		if err := l1.Lock(); err != nil {
			return err
		}
		l2, err := openLock(kind, path, cfg.Lease, cfg.Poll)
		if err != nil {
			return err
		}
		second, err := l2.TryLock()
		if err != nil {
			return fmt.Errorf("%s: second handle: %w", kind, err)
		}
		// Start again with only l1, since l2 may have taken or dropped
		// the process's lock.
		if second {
			_ = l2.Unlock()
		}
		_ = l2.Close()
		_ = l1.Unlock()
		if err := l1.Lock(); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_ = f.Close()
		c, err := spawn(cfg, "try", 0)
		if err != nil {
			return err
		}
		<-c.done
		if c.err != nil {
			return fmt.Errorf("%s: try: %w", kind, c.err)
		}
		free, err := os.ReadFile(filepath.Join(dir, "try-0"))
		if err != nil {
			return err
		}
		_ = l1.Unlock()
		_ = l1.Close()
		t.row(kind, fmt.Sprintf("%.1f", float64(alone)/float64(time.Microsecond)), yesNo(!second), yesNo(string(free) == "no"))
	}

	fmt.Printf("\nThe holder is killed while another process waits for the lock: from the kill to the waiter holding it, %d rounds.\n\n", o.rounds)
	t = newTable("lock", "rounds", "min, ms", "p50", "max", "waiters that never got it")
	for _, kind := range o.locks {
		var waits []int64
		never := 0
		for range o.rounds {
			dir, err := newStore(o.base, "death")
			if err != nil {
				return err
			}
			cfg := o.cfg
			cfg.Dir, cfg.Lock = dir, kind
			holder, err := spawn(cfg, "hold", 0)
			if err != nil {
				return err
			}
			if err := waitFor(filepath.Join(dir, "held"), time.Minute); err != nil {
				killAll([]*child{holder})
				return err
			}
			waiter, err := spawn(cfg, "waiter", 1)
			if err != nil {
				killAll([]*child{holder})
				return err
			}
			if err := waitFor(filepath.Join(dir, "ready", "waiter-1"), time.Minute); err != nil {
				killAll([]*child{holder, waiter})
				return err
			}
			// Long enough for the waiter to be blocked in the lock call.
			time.Sleep(time.Duration(50+rand.IntN(50)) * time.Millisecond)
			if exists(filepath.Join(dir, "got")) {
				killAll([]*child{holder, waiter})
				return fmt.Errorf("%s: the waiter got the lock while the holder lived", kind)
			}
			killed := holder.kill()
			if _, stalled := waitAll([]*child{waiter}, cfg.Lease+30*time.Second); stalled > 0 {
				never++
				<-holder.done
				continue
			}
			<-holder.done
			b, err := os.ReadFile(filepath.Join(dir, "got"))
			if err != nil {
				return err
			}
			got, _ := strconv.ParseInt(string(b), 10, 64)
			waits = append(waits, got-killed.UnixNano())
		}
		slices.Sort(waits)
		t.row(kind, o.rounds, pct(waits, 0), pct(waits, 0.5), pct(waits, 1), never)
	}
	if slices.Contains(o.locks, "excl") {
		return expSteal(o)
	}
	return nil
}

// expSteal measures the take-over of a stale excl lock file: every process
// tries it once, at the same moment, and exactly one should get it.
func expSteal(o opts) error {
	const rounds = 50
	n := o.n()
	fmt.Printf("\nTaking over a stale excl lock file: it is left with a modification time two leases old, and %d processes each try the lock once, at the same moment. %d rounds.\n\n", n, rounds)
	holders := map[int64]int{}
	for range rounds {
		dir, err := newStore(o.base, "steal")
		if err != nil {
			return err
		}
		cfg := o.cfg
		cfg.Dir, cfg.Lock = dir, "excl"
		path := filepath.Join(dir, appendLock)
		if err := os.WriteFile(path, []byte("a dead holder\n"), 0o600); err != nil {
			return err
		}
		old := time.Now().Add(-2 * cfg.Lease)
		if err := os.Chtimes(path, old, old); err != nil {
			return err
		}
		kids, err := spawnN(cfg, "grab", n, 0)
		if err != nil {
			return err
		}
		if _, err := release(dir, n); err != nil {
			killAll(kids)
			return err
		}
		if failed, stalled := waitAll(kids, o.timeout); failed+stalled > 0 {
			return fmt.Errorf("steal: %d children failed and %d stalled", failed, stalled)
		}
		res, err := readResults(dir)
		if err != nil {
			return err
		}
		holders[res.N["got"]]++
		if !o.keep {
			os.RemoveAll(dir)
		}
	}
	t := newTable("processes that got the lock in a round", "rounds")
	for _, k := range slices.Sorted(maps.Keys(holders)) {
		t.row(k, holders[k])
	}
	return nil
}
