package main

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
)

// procMem is a process's resident memory as its operating system counts
// it, in bytes. anon and file split it into pages with nothing behind them
// (the heap) and pages of files (Dolt's storage, the binary), which the
// kernel can drop and read again; both are -1 where only the total is
// known. swap is what the process has swapped out, which rss leaves out.
type procMem struct {
	rss, anon, file, swap int64
}

// cgroup is the memory of the control group this process and Dolt run in,
// which in a container is the container: bytes charged, the limit (0 for
// none), the anonymous, file and kernel parts, and the processes the
// kernel has killed in it for want of memory.
type cgroup struct {
	current, max, anon, file, kernel, oomKills int64
}

// reading is Dolt's memory read every way there is at one moment.
type reading struct {
	proc    procMem
	procErr error
	cg      cgroup
	hasCg   bool
	prom    map[string]float64 // Dolt's own metrics; nil if the endpoint did not answer
	disk    int64              // bytes of Dolt's data directory
}

// measure reads srv's memory. A way that fails shows as unknown in the
// reading rather than failing the run.
func measure(ctx context.Context, srv *dolttest.Server, dir string) reading {
	var r reading
	r.proc, r.procErr = readProc(srv.PID())
	r.cg, r.hasCg = readCgroup()
	r.prom, _ = scrape(ctx, srv.MetricsURL())
	_ = filepath.WalkDir(filepath.Join(dir, "data"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // a file Dolt removed meanwhile
		}
		if info, err := d.Info(); err == nil {
			r.disk += info.Size()
		}
		return nil
	})
	return r
}

func mib(b int64) string {
	if b < 0 {
		return "?"
	}
	return strconv.FormatInt(b>>20, 10)
}

// String is the reading on one line, in MiB: the operating system's
// figures, then Dolt's own, then the cgroup's.
func (r reading) String() string {
	var b strings.Builder
	if r.procErr != nil {
		fmt.Fprintf(&b, "rss unknown (%v)", r.procErr)
	} else {
		fmt.Fprintf(&b, "rss %s MiB (anon %s, file %s, swapped %s)", mib(r.proc.rss), mib(r.proc.anon), mib(r.proc.file), mib(r.proc.swap))
	}
	if r.prom == nil {
		b.WriteString(" | dolt's metrics unknown")
	} else {
		p := func(name string) string { return mib(int64(r.prom[name])) }
		fmt.Fprintf(&b, " | dolt says rss %s, go sys %s, heap in use %s, alloc %s, idle %s, released %s, next gc %s",
			p("process_resident_memory_bytes"), p("go_memstats_sys_bytes"), p("go_memstats_heap_inuse_bytes"), p("go_memstats_heap_alloc_bytes"),
			p("go_memstats_heap_idle_bytes"), p("go_memstats_heap_released_bytes"), p("go_memstats_next_gc_bytes"))
	}
	if r.hasCg {
		fmt.Fprintf(&b, " | cgroup %s MiB (anon %s, file %s, kernel %s), oom kills %d", mib(r.cg.current), mib(r.cg.anon), mib(r.cg.file), mib(r.cg.kernel), r.cg.oomKills)
	}
	fmt.Fprintf(&b, " | data on disk %s MiB", mib(r.disk))
	return b.String()
}

// work is what Dolt spent between two readings on writes writes that took
// wall: its CPU, and its collector's cycles and pauses. The collector's
// own CPU is printed where Dolt's metrics give it.
func work(before, after reading, writes int, wall time.Duration) string {
	if before.prom == nil || after.prom == nil {
		return "dolt's cpu unknown"
	}
	d := func(name string) float64 { return after.prom[name] - before.prom[name] }
	cpu := d("process_cpu_seconds_total")
	s := fmt.Sprintf("dolt cpu %.1fs (%.2f cores, %.2f ms a write), gc cycles %.0f, gc pauses %.3fs",
		cpu, cpu/wall.Seconds(), cpu*1000/float64(writes), d("go_gc_duration_seconds_count"), d("go_gc_duration_seconds_sum"))
	if _, ok := after.prom["go_cpu_classes_gc_total_cpu_seconds_total"]; ok {
		s += fmt.Sprintf(", gc cpu %.1fs", d("go_cpu_classes_gc_total_cpu_seconds_total"))
	}
	return s
}

// scrape reads the samples without labels from a Prometheus text endpoint.
func scrape(ctx context.Context, url string) (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	m := map[string]float64{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		name, value, ok := strings.Cut(sc.Text(), " ")
		if !ok || strings.HasPrefix(name, "#") || strings.Contains(name, "{") {
			continue
		}
		if v, err := strconv.ParseFloat(value, 64); err == nil {
			m[name] = v
		}
	}
	return m, sc.Err()
}
