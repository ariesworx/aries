package e2e

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"
)

// soakProfileEnv names the directory TestSoakProfile writes to; unset,
// the test is skipped.
const soakProfileEnv = "ARIES_SOAK_PROFILE"

// TestSoakProfile is the soak, measured for what it holds and what its
// garbage collector does (docs/design/profile-ariesd.md): the same
// run as TestSoak, with a heap profile and a goroutine profile taken
// after a forced collection early in the work, late in it and at rest,
// and the runtime's own counts read every five seconds between. It
// writes to the directory ARIES_SOAK_PROFILE names, and is skipped
// without it:
//
//	ARIES_SOAK_PROFILE=/tmp/soak ARIES_SOAK=2m go test -run 'TestSoakProfile$' -memprofilerate 4096 -timeout 30m -v ./internal/e2e/
//
// Run it without -race, which distorts both memory and the collector.
// -memprofilerate 4096 samples an allocation in each 4 KiB, where the
// default samples one in each 512 KiB: the heap of a soak is small.
// The test binary holds the daemon, the SSH server before it and every
// session's client, so the heap is all of theirs: `go tool pprof
// -focus` picks the daemon's packages out of a profile.
//
// heap-early.pb.gz, heap-late.pb.gz and heap-rest.pb.gz are the heap
// profiles (compare two with `go tool pprof -sample_index=inuse_space
// -base`), goroutines-*.txt the goroutines at the same moments, and
// runtime.csv the readings.
func TestSoakProfile(t *testing.T) {
	// Serial: measures this process, its heap and its goroutines, which a test beside it would add to.
	dir := os.Getenv(soakProfileEnv)
	if dir == "" {
		t.Skipf("set %s to a directory to profile the soak", soakProfileEnv)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // the directory the run was given
		t.Fatal(err)
	}
	cfg, err := soakConfigFrom(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("soak: seed %d, %s of work; profiles in %s", cfg.seed, cfg.duration, dir)
	s := newSoak(t, cfg)

	p := &soakProfiler{t: t, dir: dir, start: time.Now()}
	if p.csv, err = os.Create(filepath.Join(dir, "runtime.csv")); err != nil { //nolint:gosec // the directory the run was given
		t.Fatal(err)
	}
	defer func() { _ = p.csv.Close() }()
	_, _ = fmt.Fprintln(p.csv, strings.Join(soakReadingNames, ","))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		early, late := time.After(min(20*time.Second, cfg.duration/4)), time.After(cfg.duration*9/10)
		for {
			select {
			case <-stop:
				return
			case <-early:
				p.snapshot("early")
			case <-late:
				p.snapshot("late")
			case <-tick.C:
				p.read("")
			}
		}
	})
	s.run()
	close(stop)
	wg.Wait()
	p.snapshot("rest")
	s.verify()
	s.checkTelemetry()
	s.checkDeadCollector()
	s.report()
	s.explain()
	t.Log(p.summary())
}

// soakProfiler writes a soak's profiles and readings.
type soakProfiler struct {
	t     *testing.T
	dir   string
	start time.Time
	csv   *os.File
	first soakReading
	last  soakReading
	// pauses is the collector's pause histogram at the last reading, and
	// pausesFirst its counts at the first.
	pauses      *metrics.Float64Histogram
	pausesFirst []uint64
}

// soakReading is one reading of the runtime.
type soakReading struct {
	at                                    time.Duration
	goroutines                            uint64
	heapLive, heapObjects, heapGoal       uint64
	heapReleased, total, stacks           uint64
	cycles                                uint64
	allocBytes, allocObjects              uint64
	gcCPU, userCPU, totalCPU, gcPauseSecs float64
}

var soakReadingNames = []string{"seconds", "mark", "goroutines", "heap_live_bytes", "heap_objects_bytes", "heap_goal_bytes",
	"heap_released_bytes", "total_bytes", "stack_bytes", "gc_cycles", "alloc_bytes", "alloc_objects",
	"gc_cpu_seconds", "user_cpu_seconds", "total_cpu_seconds", "gc_pause_seconds"}

var soakSamples = []metrics.Sample{
	{Name: "/sched/goroutines:goroutines"},
	{Name: "/gc/heap/live:bytes"},
	{Name: "/memory/classes/heap/objects:bytes"},
	{Name: "/gc/heap/goal:bytes"},
	{Name: "/memory/classes/heap/released:bytes"},
	{Name: "/memory/classes/total:bytes"},
	{Name: "/memory/classes/heap/stacks:bytes"},
	{Name: "/gc/cycles/total:gc-cycles"},
	{Name: "/gc/heap/allocs:bytes"},
	{Name: "/gc/heap/allocs:objects"},
	{Name: "/cpu/classes/gc/total:cpu-seconds"},
	{Name: "/cpu/classes/user:cpu-seconds"},
	{Name: "/cpu/classes/total:cpu-seconds"},
	{Name: "/sched/pauses/total/gc:seconds"},
}

// read takes one reading and writes it to the CSV, marked mark.
func (p *soakProfiler) read(mark string) soakReading {
	samples := make([]metrics.Sample, len(soakSamples))
	copy(samples, soakSamples)
	metrics.Read(samples)
	u := func(i int) uint64 { return samples[i].Value.Uint64() }
	f := func(i int) float64 { return samples[i].Value.Float64() }
	r := soakReading{at: time.Since(p.start), goroutines: u(0), heapLive: u(1), heapObjects: u(2), heapGoal: u(3), heapReleased: u(4),
		total: u(5), stacks: u(6), cycles: u(7), allocBytes: u(8), allocObjects: u(9), gcCPU: f(10), userCPU: f(11), totalCPU: f(12)}
	p.pauses = samples[13].Value.Float64Histogram()
	r.gcPauseSecs = histogramSum(p.pauses)
	_, _ = fmt.Fprintf(p.csv, "%.1f,%s,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%.4f,%.4f,%.4f,%.6f\n", r.at.Seconds(), mark, r.goroutines, r.heapLive,
		r.heapObjects, r.heapGoal, r.heapReleased, r.total, r.stacks, r.cycles, r.allocBytes, r.allocObjects, r.gcCPU, r.userCPU, r.totalCPU, r.gcPauseSecs)
	if p.first.at == 0 {
		p.first, p.pausesFirst = r, append([]uint64(nil), p.pauses.Counts...)
	}
	p.last = r
	return r
}

// histogramSum estimates the total of what h counted, taking each
// bucket at its middle.
func histogramSum(h *metrics.Float64Histogram) float64 {
	var sum float64
	for i, n := range h.Counts {
		lo, hi := h.Buckets[i], h.Buckets[i+1]
		if math.IsInf(lo, -1) {
			lo = hi
		}
		if math.IsInf(hi, 1) {
			hi = lo
		}
		sum += float64(n) * (lo + hi) / 2
	}
	return sum
}

// snapshot forces a collection, then writes the heap and the goroutines
// as they are, and a reading marked name.
func (p *soakProfiler) snapshot(name string) {
	runtime.GC()
	runtime.GC() // the profile is as of the collection before the last
	for kind, file := range map[string]string{"heap": "heap-" + name + ".pb.gz", "goroutine": "goroutines-" + name + ".txt"} {
		f, err := os.Create(filepath.Join(p.dir, file)) //nolint:gosec // the directory the run was given
		if err != nil {
			p.t.Errorf("profile %s: %v", file, err)
			continue
		}
		debug := 0
		if kind == "goroutine" {
			debug = 1 // text, grouped by stack
		}
		if err := pprof.Lookup(kind).WriteTo(f, debug); err != nil {
			p.t.Errorf("profile %s: %v", file, err)
		}
		_ = f.Close()
	}
	r := p.read(name)
	p.t.Logf("profile %s at %s: %d goroutines, %.1f MiB of live heap objects, %.1f MiB mapped, %d collections",
		name, r.at.Round(time.Second), r.goroutines, float64(r.heapObjects)/(1<<20), float64(r.total)/(1<<20), r.cycles)
}

// summary says what the collector did between the first and the last
// reading.
func (p *soakProfiler) summary() string {
	a, b := p.first, p.last
	secs := (b.at - a.at).Seconds()
	cycles := b.cycles - a.cycles
	gc, total := b.gcCPU-a.gcCPU, b.totalCPU-a.totalCPU
	user := b.userCPU - a.userCPU
	// The longest pause since the first reading: the upper bound of the
	// highest bucket that gained a count.
	var longest float64
	var pauses uint64
	if p.pauses != nil {
		for i, n := range p.pauses.Counts {
			if i < len(p.pausesFirst) {
				n -= p.pausesFirst[i]
			}
			if n > 0 {
				pauses += n
				if hi := p.pauses.Buckets[i+1]; !math.IsInf(hi, 1) {
					longest = hi
				}
			}
		}
	}
	return fmt.Sprintf("collector over %.0fs: %d cycles (%.2f a second), %.1f MiB allocated a second in %.0f objects a second, "+
		"GC CPU %.2fs of %.2fs busy (%.1f%% of user and GC CPU, %.3f%% of the %d processors' time), %d stop-the-world pauses of %.1f ms in all, none over %.0f µs",
		secs, cycles, float64(cycles)/secs, float64(b.allocBytes-a.allocBytes)/secs/(1<<20), float64(b.allocObjects-a.allocObjects)/secs,
		gc, user+gc, 100*gc/(user+gc), 100*gc/total, runtime.GOMAXPROCS(0), pauses, 1000*(b.gcPauseSecs-a.gcPauseSecs), longest*1e6)
}
