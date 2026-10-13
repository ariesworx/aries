package main

import (
	"bufio"
	"cmp"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// event is one line of what a replayer noted: see replayer.log.
type event struct {
	id   int
	kind string
	key  string
	t    int64
}

// delivery is one entry sent to the stand-in server by one process, from
// begin to end on the wall clock. A process killed in the middle of one
// leaves it with no end, and it is then taken to end where it began.
type delivery struct {
	id         int
	key        string
	begin, end int64
}

// readServer reads every replayer's notes: all events by time, and the
// deliveries by when they began.
func readServer(dir string) ([]event, []delivery, error) {
	des, err := os.ReadDir(filepath.Join(dir, "server"))
	if err != nil {
		return nil, nil, err
	}
	var events []event
	var dels []delivery
	for _, de := range des {
		id, err := strconv.Atoi(de.Name())
		if err != nil {
			continue
		}
		f, err := os.Open(filepath.Join(dir, "server", de.Name()))
		if err != nil {
			return nil, nil, err
		}
		open := false // whether this process's last delivery has no end yet
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			p := strings.Fields(sc.Text())
			if len(p) != 3 {
				continue // a note cut short by the kill
			}
			t, err := strconv.ParseInt(p[2], 10, 64)
			if err != nil {
				continue
			}
			events = append(events, event{id: id, kind: p[0], key: p[1], t: t})
			switch p[0] {
			case "B":
				dels = append(dels, delivery{id: id, key: p[1], begin: t, end: t})
				open = true
			case "E":
				if open && dels[len(dels)-1].key == p[1] {
					dels[len(dels)-1].end = t
				}
				open = false
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	slices.SortStableFunc(events, func(a, b event) int { return cmp.Compare(a.t, b.t) })
	slices.SortStableFunc(dels, func(a, b delivery) int { return cmp.Compare(a.begin, b.begin) })
	return events, dels, nil
}

// overlaps counts deliveries that began while another process's delivery
// was still going: two replayers at once.
func overlaps(dels []delivery) int {
	n := 0
	var until int64
	who := -1
	for _, d := range dels {
		if d.begin < until && d.id != who {
			n++
		}
		if d.end >= until {
			until, who = d.end, d.id
		}
	}
	return n
}

// kill is one replayer the parent killed while it held the replay lock.
type kill struct {
	id int
	at time.Time
}

// killHolders kills whichever process holds the replay lock, n times, each
// after it has held it for between lo and hi. It stops early when every
// child has exited.
func killHolders(dir string, kids []*child, n int, lo, hi time.Duration) []kill {
	byID := map[int]*child{}
	for _, c := range kids {
		byID[c.id] = c
	}
	var kills []kill
	for len(kills) < n {
		var holder *child
		des, _ := os.ReadDir(filepath.Join(dir, "holders"))
		for _, de := range des {
			id, _ := strconv.Atoi(de.Name())
			if c := byID[id]; c != nil && !c.killed() && !c.exited() {
				holder = c
				break
			}
		}
		if holder == nil {
			if !slices.ContainsFunc(kids, func(c *child) bool { return !c.exited() }) {
				break
			}
			time.Sleep(time.Millisecond)
			continue
		}
		time.Sleep(lo + rand.N(hi-lo))
		if holder.exited() {
			continue
		}
		kills = append(kills, kill{id: holder.id, at: holder.kill()})
	}
	return kills
}

// after is how long after each kill the first event of the given kind by
// another process came, sorted.
func after(kills []kill, events []event, kind string) []int64 {
	var out []int64
	for _, k := range kills {
		at := k.at.UnixNano()
		for _, e := range events {
			if e.kind == kind && e.t > at && e.id != k.id {
				out = append(out, e.t-at)
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// expReplay is experiment 3: many processes contend to replay, and the one
// replaying is killed.
func expReplay(o opts) error {
	n := o.n()
	kills := o.kills
	if kills < 0 {
		kills = 20
	}
	kills = min(kills, n-1)
	fmt.Printf("An outbox of %d entries; %d processes wait for the replay lock, and whoever holds it delivers entries in order to a stand-in server that takes %v, noting each answer in the answered file. The holder is killed %d times, each after 30 to 150 ms of holding the lock. The excl lock has a lease of %v, renewed every quarter of it, and is retried every %v.\n\n",
		o.entries, n, o.cfg.RPC, kills, o.cfg.Lease, o.cfg.Poll)
	t := newTable("lock", "answers synced", "killed", "kill to another holding the lock: p50, ms", "max", "kill to the next delivery: p50, ms", "max",
		"delivered once", "twice", "more", "never", "deliveries while another's was going", "first deliveries out of order", "leases found lost", "took, s", "children failed", "stalled")
	type variant struct {
		kind string
		sync bool
	}
	var vs []variant
	for _, kind := range o.locks {
		vs = append(vs, variant{kind, true})
	}
	vs = append(vs, variant{o.locks[0], false})
	for _, v := range vs {
		dir, err := newStore(o.base, "replay")
		if err != nil {
			return err
		}
		cfg := o.cfg
		cfg.Dir, cfg.Lock, cfg.Drain, cfg.SyncAnswered, cfg.CompactEvery = dir, v.kind, true, v.sync, 0
		rng := newRand(0)
		var all []byte
		for seq := range o.entries {
			all = append(all, makeEntry(0, seq, rng, cfg.SizeMin+rng.IntN(cfg.SizeMax-cfg.SizeMin+1))...)
		}
		if err := os.WriteFile(filepath.Join(dir, outboxFile), all, 0o600); err != nil {
			return err
		}
		kids, err := spawnN(cfg, "replay", n, 0)
		if err != nil {
			return err
		}
		start, err := release(dir, n)
		if err != nil {
			killAll(kids)
			return err
		}
		killed := killHolders(dir, kids, kills, 30*time.Millisecond, 150*time.Millisecond)
		failed, stalled := waitAll(kids, o.timeout)
		took := time.Since(start)

		events, dels, err := readServer(dir)
		if err != nil {
			return err
		}
		count := map[string]int{}
		disorder, high := 0, -1
		for _, d := range dels {
			count[d.key]++
			if count[d.key] > 1 {
				continue
			}
			_, num, _ := strings.Cut(d.key, "-")
			seq, _ := strconv.Atoi(num)
			if seq < high {
				disorder++
			}
			high = max(high, seq)
		}
		once, twice, more := 0, 0, 0
		for _, c := range count {
			switch c {
			case 1:
				once++
			case 2:
				twice++
			default:
				more++
			}
		}
		lost := 0
		for _, e := range events {
			if e.kind == "X" {
				lost++
			}
		}
		acq, next := after(killed, events, "A"), after(killed, events, "B")
		t.row(v.kind, yesNo(v.sync), len(killed), pct(acq, 0.5), pct(acq, 1), pct(next, 0.5), pct(next, 1),
			once, twice, more, o.entries-len(count), overlaps(dels), disorder, lost,
			fmt.Sprintf("%.1f", took.Seconds()), failed, stalled)
	}
	return nil
}

// expCompact is experiment 4: compaction while appenders append.
func expCompact(o opts) error {
	n := o.n()
	kills := o.kills
	if kills < 0 {
		kills = 10
	}
	kind := o.locks[0]
	fmt.Printf("%d processes each append %d entries, with a sync, while a replayer delivers them (the server takes %v, answers synced) and compacts after every %d answers: under the replay lock it takes the append lock, rewrites the outbox without what was answered, then empties the answered file. Lock: %s.\n\n",
		n, o.cfg.M, o.cfg.RPC, o.cfg.CompactEvery, kind)
	t := newTable("run", "appended", "delivered once", "twice", "more", "never: lost", "left in the outbox", "deliveries while another's was going",
		"replayers killed", "compactions timed", "a compaction waits for the append lock: p50, ms", "max", "then holds it: p50, ms", "max", "an append: p50, ms", "p99", "max", "took, s", "children failed", "stalled")
	for _, v := range []struct {
		name     string
		kills    int
		keepOpen bool
	}{
		{"as designed", 0, false},
		{"the replayer killed", kills, false},
		{"appenders keep the outbox open", 0, true},
	} {
		dir, err := newStore(o.base, "compact")
		if err != nil {
			return err
		}
		cfg := o.cfg
		cfg.Dir, cfg.Lock, cfg.SyncAnswered = dir, kind, true
		app := cfg
		app.KeepOpen = v.keepOpen
		appenders, err := spawnN(app, "append", n, 0)
		if err != nil {
			return err
		}
		replayers, err := spawnN(cfg, "replay", v.kills+2, 1000)
		if err != nil {
			killAll(appenders)
			return err
		}
		start, err := release(dir, n+len(replayers))
		if err != nil {
			killAll(append(appenders, replayers...))
			return err
		}
		killed := killHolders(dir, replayers, v.kills, 100*time.Millisecond, 400*time.Millisecond)
		failed, stalled := waitAll(appenders, o.timeout)
		if err := touch(filepath.Join(dir, "stop")); err != nil {
			return err
		}
		f2, s2 := waitAll(replayers, o.timeout)
		failed, stalled = failed+f2, stalled+s2
		took := time.Since(start)

		_, dels, err := readServer(dir)
		if err != nil {
			return err
		}
		count := map[string]int{}
		for _, d := range dels {
			count[d.key]++
		}
		s, err := readOutbox(filepath.Join(dir, outboxFile))
		if err != nil {
			return err
		}
		left := map[string]bool{}
		for _, e := range s.entries {
			left[e.Key] = true
		}
		once, twice, more, never, undelivered := 0, 0, 0, 0, 0
		for id := range n {
			for seq := range cfg.M {
				k := key(id, seq)
				switch c := count[k]; {
				case c == 1:
					once++
				case c == 2:
					twice++
				case c > 2:
					more++
				case left[k]:
					undelivered++
				default:
					never++
				}
			}
		}
		res, err := readResults(dir)
		if err != nil {
			return err
		}
		cw, cp, ap := res.Lat["compact_wait"], res.Lat["compact_work"], res.Lat["total"]
		t.row(v.name, n*cfg.M, once, twice, more, never, undelivered, overlaps(dels),
			len(killed), len(cp), pct(cw, 0.5), pct(cw, 1), pct(cp, 0.5), pct(cp, 1), pct(ap, 0.5), pct(ap, 0.99), pct(ap, 1),
			fmt.Sprintf("%.1f", took.Seconds()), failed, stalled)
	}
	return nil
}
