package main

import (
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// expCache is experiment 5: many processes write and read the same few
// cache files.
func expCache(o opts) error {
	n := o.n()
	dir, err := newStore(o.base, "probe")
	if err != nil {
		return err
	}
	if err := probeRename(dir); err != nil {
		return err
	}

	fmt.Printf("\n%d processes each make %d reads and writes, three writes in ten, of %d files of 0.2 to 16 KiB, each written to a temporary file in the same directory and renamed over its name. Every read is checked to be one whole file.\n\n",
		n, o.cfg.Ops, o.cfg.Files)
	t := newTable("sync before rename", "killed", "reads", "partial", "missing", "failed", "writes", "failed", "renames tried again",
		"read p50, ms", "p99", "max", "write p50, ms", "p99", "max", "took, s", "files whole at the end", "temporary files left", "modes", "children failed", "stalled")
	var notes []string
	for _, v := range []struct {
		sync  bool
		kills int
	}{{false, 0}, {true, 0}, {false, max(n/10, 1)}} {
		dir, err := newStore(o.base, "cache")
		if err != nil {
			return err
		}
		cfg := o.cfg
		cfg.Dir, cfg.Fsync = dir, v.sync
		rng := newRand(0)
		for k := range cfg.Files {
			if _, err := writeCache(dir, k, makeDoc(k, -1, 0, rng), true); err != nil {
				return err
			}
		}
		kids, err := spawnN(cfg, "cache", n, 0)
		if err != nil {
			return err
		}
		start, err := release(dir, n)
		if err != nil {
			killAll(kids)
			return err
		}
		killed := 0
		for killed < min(v.kills, n-1) {
			time.Sleep(time.Duration(5+rand.IntN(45)) * time.Millisecond)
			if c := kids[rand.IntN(n)]; !c.exited() && !c.killed() {
				c.kill()
				killed++
			} else if !slices.ContainsFunc(kids, func(c *child) bool { return !c.exited() }) {
				break
			}
		}
		failed, stalled := waitAll(kids, o.timeout)
		took := time.Since(start)
		res, err := readResults(dir)
		if err != nil {
			return err
		}

		whole := 0
		for k := range cfg.Files {
			if b, err := os.ReadFile(cachePath(dir, k)); err == nil && checkDoc(b, k) {
				whole++
			}
		}
		des, err := os.ReadDir(filepath.Join(dir, "cache"))
		if err != nil {
			return err
		}
		temps := 0
		modes := map[string]bool{}
		for _, de := range des {
			if strings.HasPrefix(de.Name(), "tmp-") {
				temps++
				continue
			}
			if st, err := de.Info(); err == nil {
				modes[fmt.Sprintf("%04o", st.Mode().Perm())] = true
			}
		}
		st, err := os.Stat(filepath.Join(dir, "cache"))
		if err != nil {
			return err
		}
		readFailed, writeFailed := int64(0), int64(0)
		for _, k := range slices.Sorted(maps.Keys(res.N)) {
			switch {
			case strings.HasPrefix(k, "read failed: "):
				readFailed += res.N[k]
			case strings.HasPrefix(k, "write failed: "):
				writeFailed += res.N[k]
			default:
				continue
			}
			notes = append(notes, fmt.Sprintf("%s: %d times", k, res.N[k]))
		}
		r, w := res.Lat["read"], res.Lat["write"]
		t.row(yesNo(v.sync), killed, res.N["reads"], res.N["read_partial"], res.N["read_missing"], readFailed,
			res.N["writes"], writeFailed, res.N["rename_retries"],
			pct(r, 0.5), pct(r, 0.99), pct(r, 1), pct(w, 0.5), pct(w, 0.99), pct(w, 1),
			fmt.Sprintf("%.1f", took.Seconds()), fmt.Sprintf("%d of %d", whole, cfg.Files), temps,
			fmt.Sprintf("files %s in a %04o directory", strings.Join(slices.Sorted(maps.Keys(modes)), " "), st.Mode().Perm()),
			failed, stalled)
	}
	fmt.Println()
	for _, s := range notes {
		fmt.Println(s)
	}
	return nil
}

// probeRename prints what this system does when a file is replaced or
// removed while a handle on it is open, which is what a reader of the
// cache meets.
func probeRename(dir string) error {
	p := filepath.Join(dir, "probe")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(p+".new", []byte("new"), 0o600); err != nil {
		return err
	}
	h, err := os.Open(p)
	if err != nil {
		return err
	}
	defer h.Close()
	said := func(err error) string {
		if err != nil {
			return "refused: " + cause(err)
		}
		return "allowed"
	}
	fmt.Println("Replacing a file that is open, in one process:")
	fmt.Printf("- rename over a file that a handle has open for reading: %s\n", said(os.Rename(p+".new", p)))
	b, err := io.ReadAll(h)
	fmt.Printf("- that handle then reads: %q (%v)\n", b, err)
	b, err = os.ReadFile(p)
	fmt.Printf("- a new open of the name reads: %q (%v)\n", b, err)
	fmt.Printf("- remove of the name while that handle is open: %s\n", said(os.Remove(p)))
	fmt.Printf("- the name exists afterward: %s\n", yesNo(exists(p)))
	return nil
}
