package main

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// expAppend is experiment 1: many processes append, with and without a
// sync of each append.
func expAppend(o opts) error {
	fmt.Printf("Each process appends %d entries of %d to %d bytes. Times are of one append, in ms: all of it, then its parts (waiting for the lock, opening and writing, the sync).\n\n",
		o.cfg.M, o.cfg.SizeMin, o.cfg.SizeMax)
	t := newTable("lock", "sync", "processes", "entries", "MiB", "took, s", "appends a second",
		"p50", "p95", "p99", "max", "wait p50", "wait p99", "write p50", "write p99", "sync p50", "sync p99", "file")
	for _, kind := range o.locks {
		for _, n := range o.procs {
			for _, sync := range []bool{true, false} {
				dir, err := newStore(o.base, "append")
				if err != nil {
					return err
				}
				cfg := o.cfg
				cfg.Dir, cfg.Lock, cfg.Fsync = dir, kind, sync
				kids, err := spawnN(cfg, "append", n, 0)
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
				res, err := readResults(dir)
				if err != nil {
					return err
				}
				verdict, err := checkAppended(dir, n, cfg.M)
				if err != nil {
					return err
				}
				if failed+stalled > 0 {
					verdict += fmt.Sprintf("; %d children failed, %d stalled", failed, stalled)
				}
				if res.N["steals"]+res.N["lock_lost"] > 0 {
					verdict += fmt.Sprintf("; %d take-overs, %d unlocks found the lock taken", res.N["steals"], res.N["lock_lost"])
				}
				all, w, wr, sy := res.Lat["total"], res.Lat["wait"], res.Lat["write"], res.Lat["sync"]
				t.row(kind, yesNo(sync), n, n*cfg.M, fmt.Sprintf("%.1f", float64(res.N["bytes"])/(1<<20)),
					fmt.Sprintf("%.2f", took.Seconds()), fmt.Sprintf("%.0f", float64(n*cfg.M)/took.Seconds()),
					pct(all, 0.5), pct(all, 0.95), pct(all, 0.99), pct(all, 1),
					pct(w, 0.5), pct(w, 0.99), pct(wr, 0.5), pct(wr, 0.99), pct(sy, 0.5), pct(sy, 0.99), verdict)
			}
		}
	}
	return nil
}

// checkAppended checks an outbox that n processes each appended m entries
// to, none of them killed: exactly n times m entries, every line whole and
// valid, and each process's entries in the order it wrote them.
func checkAppended(dir string, n, m int) (string, error) {
	s, err := readOutbox(filepath.Join(dir, outboxFile))
	if err != nil {
		return "", err
	}
	next := make([]int, n)
	disorder := 0
	for _, e := range s.entries {
		if e.Proc < 0 || e.Proc >= n || e.Seq != next[e.Proc] {
			disorder++
			continue
		}
		next[e.Proc]++
	}
	short := 0
	for _, got := range next {
		if got != m {
			short++
		}
	}
	if len(s.entries) == n*m && s.bad == 0 && s.tail == 0 && disorder == 0 && short == 0 {
		return "ok", nil
	}
	return fmt.Sprintf("WRONG: %d entries of %d, %d bad lines, %d bytes of unfinished tail, %d out of order, %d processes short",
		len(s.entries), n*m, s.bad, s.tail, disorder, short), nil
}

// acks reads what one appender noted: the appends that returned, and
// whether its last note is of holding the lock.
func acks(dir string, id int) (returned []int, holding bool, err error) {
	f, err := os.Open(filepath.Join(dir, "ack", strconv.Itoa(id)))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		kind, num, _ := strings.Cut(sc.Text(), " ")
		seq, err := strconv.Atoi(num)
		if err != nil {
			continue // a note cut short by the kill
		}
		holding = kind == "L"
		if kind == "U" {
			returned = append(returned, seq)
		}
	}
	return returned, holding, sc.Err()
}

// expCrash is experiment 2: appenders are killed as they append.
func expCrash(o opts) error {
	n := o.n()
	kills := o.kills
	if kills < 0 {
		kills = n / 2
	}
	kills = min(kills, n-1)
	fmt.Printf("%d processes each append %d entries, with a sync; %d are killed as they go, every other one aimed at the process that holds the lock. An entry is written in one write, or in two with 1 ms between them.\n\n",
		n, o.cfg.M, kills)
	t := newTable("lock", "an entry is", "killed", "of them, surely holding the lock", "survivors that finished", "stalled",
		"appends that returned", "of them missing", "entries twice", "in the file, never returned", "out of order",
		"torn lines, ended by the next writer", "unfinished tail, bytes", "longest wait for the lock, ms", "take-overs", "unlocks that found the lock taken")
	var samples []string
	for _, kind := range o.locks {
		for _, chunked := range []bool{false, true} {
			dir, err := newStore(o.base, "crash")
			if err != nil {
				return err
			}
			cfg := o.cfg
			cfg.Dir, cfg.Lock, cfg.Ack, cfg.Holder = dir, kind, true, true
			cfg.Chunked, cfg.ChunkPause = chunked, time.Millisecond
			kids, err := spawnN(cfg, "append", n, 0)
			if err != nil {
				return err
			}
			if _, err := release(dir, n); err != nil {
				killAll(kids)
				return err
			}
			killed := 0
			for killed < kills {
				time.Sleep(time.Duration(5+rand.IntN(95)) * time.Millisecond)
				var alive []*child
				for _, c := range kids {
					if !c.exited() && !c.killed() {
						alive = append(alive, c)
					}
				}
				if len(alive) == 0 {
					break
				}
				victim := alive[rand.IntN(len(alive))]
				if killed%2 == 0 {
					if b, err := os.ReadFile(filepath.Join(dir, "holder")); err == nil {
						if id, err := strconv.Atoi(string(b)); err == nil && id < n && !kids[id].exited() && !kids[id].killed() {
							victim = kids[id]
						}
					}
				}
				victim.kill()
				killed++
			}
			failed, stalled := waitAll(kids, o.timeout)

			s, err := readOutbox(filepath.Join(dir, outboxFile))
			if err != nil {
				return err
			}
			inFile := map[string]int{}
			last := map[int]int{}
			disorder := 0
			for _, e := range s.entries {
				inFile[e.Key]++
				if prev, ok := last[e.Proc]; ok && e.Seq <= prev {
					disorder++
				}
				last[e.Proc] = e.Seq
			}
			twice := 0
			for _, c := range inFile {
				if c > 1 {
					twice++
				}
			}
			returned, missing, holding, finished := 0, 0, 0, 0
			isReturned := map[string]bool{}
			for _, c := range kids {
				seqs, held, err := acks(dir, c.id)
				if err != nil {
					return err
				}
				// A kill sent after the process had exited killed nothing.
				if c.killed() && held {
					holding++
				}
				if !c.killed() && len(seqs) == cfg.M && c.err == nil {
					finished++
				}
				for _, seq := range seqs {
					returned++
					isReturned[key(c.id, seq)] = true
					if inFile[key(c.id, seq)] == 0 {
						missing++
					}
				}
			}
			res, err := readResults(dir)
			if err != nil {
				return err
			}
			how := "one write"
			if chunked {
				how = "two writes"
			}
			survivors := fmt.Sprintf("%d of %d", finished, n-killed)
			if failed > 0 {
				survivors += fmt.Sprintf(" (%d failed)", failed)
			}
			t.row(kind, how, killed, holding, survivors, stalled, returned, missing, twice,
				len(inFile)-len(isReturned)+missing, disorder, s.bad, s.tail,
				pct(res.Lat["wait"], 1), res.N["steals"], res.N["lock_lost"])
			if s.bad > 0 {
				samples = append(samples, fmt.Sprintf("%s, %s: a torn line, as the next writer left it: %s", kind, how, s.sample))
			}
		}
	}
	fmt.Println()
	for _, s := range samples {
		fmt.Println(s)
	}
	fmt.Println()
	return tornTail(o)
}

// tornTail kills a writer halfway through the last line of a file and
// shows what a reader finds, with the first lock primitive asked for.
func tornTail(o opts) error {
	dir, err := newStore(o.base, "tail")
	if err != nil {
		return err
	}
	cfg := o.cfg
	cfg.Dir, cfg.Lock = dir, o.locks[0]
	ob, err := openOutbox(cfg, 0)
	if err != nil {
		return err
	}
	rng := newRand(0)
	for seq := range 3 {
		if _, err := ob.append(makeEntry(0, seq, rng, 400), seq); err != nil {
			return err
		}
	}
	slow := cfg
	slow.Ack, slow.Chunked, slow.ChunkPause, slow.M = true, true, time.Minute, 1
	c, err := spawn(slow, "append", 1)
	if err != nil {
		return err
	}
	if err := touch(filepath.Join(dir, "go")); err != nil {
		return err
	}
	// It notes "L 0" when it has the lock and then writes the first half.
	for deadline := time.Now().Add(time.Minute); ; {
		if b, _ := os.ReadFile(filepath.Join(dir, "ack", "1")); strings.HasPrefix(string(b), "L 0\n") {
			break
		}
		if time.Now().After(deadline) {
			killAll([]*child{c})
			return fmt.Errorf("the slow writer never took the lock")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	c.kill()
	<-c.done

	path := filepath.Join(dir, outboxFile)
	s, err := readOutbox(path)
	if err != nil {
		return err
	}
	fmt.Printf("A writer killed between the two writes of the file's last entry (%s). A reader finds: %d whole entries, %d bad lines, and %d bytes after the last newline: %s\n",
		cfg.Lock, len(s.entries), s.bad, s.tail, s.tailIs)
	free, err := ob.lk.TryLock()
	if err != nil {
		return err
	}
	fmt.Printf("The append lock is free at once after the kill: %s\n", yesNo(free))
	if !free {
		return nil
	}
	cut, err := truncateTail(path)
	if err != nil {
		return err
	}
	if err := ob.lk.Unlock(); err != nil {
		return err
	}
	if _, err := ob.append(makeEntry(0, 3, rng, 400), 3); err != nil {
		return err
	}
	s, err = readOutbox(path)
	if err != nil {
		return err
	}
	fmt.Printf("Truncated under the lock (%d bytes cut), then one more append: %d whole entries, %d bad lines, %d bytes of tail\n", cut, len(s.entries), s.bad, s.tail)
	return nil
}
