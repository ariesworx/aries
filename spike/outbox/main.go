// Command outbox is the stage 5 spike, slice F0 (docs/design/aries.md
// §18.4, §18.10, §18.13): it measures the proposed local store, an
// append-only NDJSON outbox and a cache of whole files, under many real
// operating system processes on one directory.
//
// It re-executes itself: the parent starts -n children, lets them all go at
// once, kills some where the experiment says so, and then checks what is on
// disk. Standard library only.
//
// Usage:
//
//	go run ./spike/outbox -exp all
//	go run ./spike/outbox -exp locks   [-n 100] [-lock all] [-iter 200] [-rounds 20]
//	go run ./spike/outbox -exp append  [-n 1,10,100] [-m 50] [-size 300,4096] [-lock all]
//	go run ./spike/outbox -exp crash   [-n 100] [-m 50] [-kills 50]
//	go run ./spike/outbox -exp replay  [-n 100] [-entries 3000] [-kills 20] [-rpc 2ms]
//	go run ./spike/outbox -exp compact [-n 100] [-m 50] [-every 200] [-kills 10]
//	go run ./spike/outbox -exp cache   [-n 100] [-files 8] [-ops 500]
//
// -lock names the lock primitives to run: "default" (flock on Linux and
// macOS, LockFileEx on Windows), "all", or a comma-separated list of flock,
// fcntl, ofd, lockfileex and excl. excl is a lock file made with O_EXCL and
// taken over when its modification time is older than -lease.
//
// What each experiment does:
//
//   - locks: for each primitive, whether it excludes at all (-n processes
//     each add one to a number in a file -iter times; a lost update is a
//     failure), what an uncontended lock and unlock costs, how long a
//     waiter waits after the holder is killed, whether a second handle in
//     the same process is kept out, and whether closing another descriptor
//     of the lock file drops the lock. With excl among the primitives, it
//     also leaves a stale lock file, has -n processes try it at the same
//     moment, and counts how many come away holding it.
//   - append: -n processes each append -m entries under the append lock,
//     with and without a sync of each one, and the file is then checked:
//     the count, every line whole and valid, each process's entries in
//     order.
//   - crash: the same, with -kills processes killed as they go, half of
//     them aimed at whoever holds the lock; once with each entry written
//     in one write and once in two with a pause, so that a kill can tear a
//     line. The survivors must finish, and every append that returned must
//     be in the file once.
//   - replay: -n processes contend to replay -entries entries to a stand-in
//     for the server (a log file each, with times); the holder is killed
//     -kills times.
//   - compact: appenders append while a replayer delivers and, every
//     -every entries, rewrites the outbox and the answered file under both
//     locks; then again with the replayer killed, and again with appenders
//     that keep the outbox open across the rewrite.
//   - cache: -n processes write (temp file, then rename) and read -files
//     files; every read is checked to be a whole file. It also prints what
//     this system does when a file is renamed over, or removed, while open.
//
// -dir sets where the store directories are made (default: the system's
// temporary directory), and -keep leaves them there.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// config is what a child is told. It crosses to the child as JSON in argv;
// nothing in it is secret.
type config struct {
	Dir          string        `json:"dir"`
	Lock         string        `json:"lock"`
	M            int           `json:"m"`
	SizeMin      int           `json:"size_min"`
	SizeMax      int           `json:"size_max"`
	Fsync        bool          `json:"fsync"`
	Chunked      bool          `json:"chunked"`
	ChunkPause   time.Duration `json:"chunk_pause"`
	KeepOpen     bool          `json:"keep_open"`
	Ack          bool          `json:"ack"`
	Holder       bool          `json:"holder"`
	Lease        time.Duration `json:"lease"`
	Poll         time.Duration `json:"poll"`
	RPC          time.Duration `json:"rpc"`
	SyncAnswered bool          `json:"sync_answered"`
	Drain        bool          `json:"drain"`
	CompactEvery int           `json:"compact_every"`
	Files        int           `json:"files"`
	Ops          int           `json:"ops"`
	Iter         int           `json:"iter"`
}

// opts is the parent's flags.
type opts struct {
	base    string
	keep    bool
	procs   []int
	locks   []string
	kills   int
	entries int
	rounds  int
	timeout time.Duration
	cfg     config
}

// n is the largest process count asked for: every experiment but append
// runs at one count.
func (o opts) n() int { return slices.Max(o.procs) }

func main() {
	var (
		exp     = flag.String("exp", "all", "experiment: locks, append, crash, replay, compact, cache or all")
		procs   = flag.String("n", "100", "processes; append takes a comma-separated list")
		locks   = flag.String("lock", "default", `lock primitives: "default", "all" or a comma-separated list`)
		m       = flag.Int("m", 50, "entries each appender appends")
		size    = flag.String("size", "300,4096", "least and most bytes of an entry")
		kills   = flag.Int("kills", -1, "processes to kill (default: by experiment)")
		entries = flag.Int("entries", 3000, "entries in the outbox for replay")
		rounds  = flag.Int("rounds", 20, "rounds of the holder-death measurement in locks")
		iter    = flag.Int("iter", 200, "additions each process makes in locks")
		every   = flag.Int("every", 200, "compact after this many answered entries")
		files   = flag.Int("files", 8, "cache files")
		ops     = flag.Int("ops", 500, "cache operations each process makes")
		lease   = flag.Duration("lease", 2*time.Second, "lease of the excl lock")
		poll    = flag.Duration("poll", time.Millisecond, "how often the excl lock is retried")
		rpc     = flag.Duration("rpc", 2*time.Millisecond, "how long the stand-in server takes to answer")
		timeout = flag.Duration("timeout", 5*time.Minute, "how long one run may take before its children are killed")
		dir     = flag.String("dir", "", "where to make the store directories (default: the system's temporary directory)")
		keep    = flag.Bool("keep", false, "keep the store directories")

		childRole = flag.String("child", "", "internal: run as a child in this role")
		childID   = flag.Int("id", 0, "internal: the child's number")
		childCfg  = flag.String("cfg", "", "internal: the child's config, as JSON")
	)
	flag.Parse()

	if *childRole != "" {
		var cfg config
		if err := json.Unmarshal([]byte(*childCfg), &cfg); err != nil {
			fatal("child config: %v", err)
		}
		if err := runChild(*childRole, *childID, cfg); err != nil {
			fatal("%s %d: %v", *childRole, *childID, err)
		}
		return
	}

	o := opts{keep: *keep, kills: *kills, entries: *entries, rounds: *rounds, timeout: *timeout}
	for s := range strings.SplitSeq(*procs, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			fatal("-n %q: want positive numbers", *procs)
		}
		o.procs = append(o.procs, n)
	}
	switch *locks {
	case "default":
		o.locks = []string{defaultLock}
	case "all":
		o.locks = lockKinds()
	default:
		for s := range strings.SplitSeq(*locks, ",") {
			if !slices.Contains(lockKinds(), s) {
				fatal("-lock %q: this system has %s", s, strings.Join(lockKinds(), ", "))
			}
			o.locks = append(o.locks, s)
		}
	}
	lo, hi, ok := strings.Cut(*size, ",")
	sizeMin, err1 := strconv.Atoi(lo)
	sizeMax, err2 := strconv.Atoi(hi)
	if !ok || err1 != nil || err2 != nil || sizeMin < 200 || sizeMax < sizeMin {
		fatal("-size %q: want least,most with least at least 200", *size)
	}
	o.cfg = config{
		M: *m, SizeMin: sizeMin, SizeMax: sizeMax, Fsync: true,
		Lease: *lease, Poll: *poll, RPC: *rpc,
		CompactEvery: *every, Files: *files, Ops: *ops, Iter: *iter,
	}

	base, err := os.MkdirTemp(*dir, "aries-outbox-")
	if err != nil {
		fatal("%v", err)
	}
	o.base = base
	if !o.keep {
		defer os.RemoveAll(base)
	}

	fmt.Printf("# %s/%s, %d CPUs, %s, store under %s\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), filepath.Dir(base))
	fmt.Printf("# %s\n", time.Now().UTC().Format(time.RFC3339))
	o.locks = slices.DeleteFunc(o.locks, func(kind string) bool {
		err := usable(kind, filepath.Join(base, "usable-"+kind), o.cfg)
		if err != nil {
			fmt.Printf("# lock %s cannot be used on this system: %v\n", kind, err)
		}
		return err != nil
	})
	if len(o.locks) == 0 {
		os.RemoveAll(base)
		fatal("no lock primitive left to run")
	}
	fmt.Println()

	all := map[string]func(opts) error{
		"locks": expLocks, "append": expAppend, "crash": expCrash,
		"replay": expReplay, "compact": expCompact, "cache": expCache,
	}
	order := []string{"locks", "append", "crash", "replay", "compact", "cache"}
	if *exp != "all" {
		if all[*exp] == nil {
			fatal("-exp %q: want one of %s, or all", *exp, strings.Join(order, ", "))
		}
		order = []string{*exp}
	}
	for _, name := range order {
		fmt.Printf("## %s\n\n", name)
		if err := all[name](o); err != nil {
			os.RemoveAll(base)
			fatal("%s: %v", name, err)
		}
		fmt.Println()
	}
}

// usable takes and releases a lock of the given kind once.
func usable(kind, path string, cfg config) error {
	lk, err := openLock(kind, path, cfg.Lease, cfg.Poll)
	if err != nil {
		return err
	}
	defer lk.Close()
	if err := lk.Lock(); err != nil {
		return err
	}
	return lk.Unlock()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "outbox: "+format+"\n", args...)
	os.Exit(1)
}
