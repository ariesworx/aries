package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/proto"
)

// BenchmarkProcess is what a running starfixd costs as the operating
// system sees it (docs/design/profile-starfixd.md): it builds the
// binary as a release is built, runs `starfixd serve --dev` on a new
// database, and reads the process's resident memory from ps, and its
// own account of its heap from the /metrics it serves, idle and then
// with 200 connections past their handshake. It also runs 20 `starfixd
// stdio`, the bridge sshd starts for each connection, and reads theirs.
//
//	go test -run '^$' -bench 'Process' -benchtime 1x ./cmd/starfixd/
//
// One iteration is one daemon. It needs dolt, the go command and ps,
// and is skipped on Windows.
func BenchmarkProcess(b *testing.B) {
	if runtime.GOOS == "windows" {
		b.Skip("reads resident memory from ps")
	}
	if errors.Is(doltErr, dolttest.ErrNoDolt) {
		b.Skip("dolt is not on PATH")
	}
	if doltErr != nil {
		b.Fatalf("dolt sql-server: %v", doltErr)
	}
	exe := filepath.Join(b.TempDir(), "starfixd")
	build := exec.CommandContext(b.Context(), "go", "build", "-trimpath", "-ldflags", "-s -w", "-o", exe, ".") //nolint:gosec // the benchmark's own directory
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		b.Skipf("go build: %v\n%s", err, out)
	}
	for _, mode := range []string{"off", "metrics"} {
		b.Run(mode, func(b *testing.B) {
			var sum processCost
			for b.Loop() {
				sum = sum.add(measureProcess(b, exe, mode == "metrics"))
			}
			n := float64(b.N)
			b.ReportMetric(sum.idleRSS/n/(1<<20), "idle-rss-MiB")
			b.ReportMetric(sum.connRSS/n, "rss-B/conn")
			b.ReportMetric(sum.bridgeRSS/n/(1<<20), "bridge-rss-MiB")
			if sum.bridgeOwn > 0 {
				b.ReportMetric(sum.bridgeOwn/n/(1<<20), "bridge-own-MiB")
				b.ReportMetric(sum.daemonOwn/n/(1<<20), "daemon-200-conns-own-MiB")
			}
			if mode == "metrics" {
				b.ReportMetric(sum.idleHeap/n/(1<<20), "idle-go-memory-MiB")
				b.ReportMetric(sum.connHeap/n, "go-memory-B/conn")
				b.ReportMetric(sum.idleGoroutines/n, "idle-goroutines")
			}
			b.ReportMetric(0, "ns/op") // the time is the setup's, and says nothing
		})
	}
}

// processCost is one daemon's readings, in bytes.
type processCost struct {
	idleRSS, connRSS, bridgeRSS float64
	idleHeap, connHeap          float64
	idleGoroutines              float64
	// bridgeOwn and daemonOwn are the memory that is a bridge's alone,
	// and the daemon's with its 200 connections: resident memory less
	// the pages shared with other processes, the binary's own text among
	// them, which every bridge maps and the machine holds once.
	bridgeOwn, daemonOwn float64
}

func (a processCost) add(c processCost) processCost {
	return processCost{a.idleRSS + c.idleRSS, a.connRSS + c.connRSS, a.bridgeRSS + c.bridgeRSS,
		a.idleHeap + c.idleHeap, a.connHeap + c.connHeap, a.idleGoroutines + c.idleGoroutines,
		a.bridgeOwn + c.bridgeOwn, a.daemonOwn + c.daemonOwn}
}

// benchProject is the project the measured daemon serves.
const benchProject = "00000000-0000-4000-8000-0000000000b1"

// measureProcess runs one daemon and reads what it costs.
func measureProcess(b *testing.B, exe string, metrics bool) processCost {
	b.Helper()
	ctx := b.Context()
	dsn, err := dolt.NewDatabase(ctx)
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := dolt.DropDatabase(dsn); err != nil {
			b.Errorf("drop the benchmark's database: %v", err)
		}
	}()
	// A unix socket's path is short; the benchmark's own directory may
	// not be.
	dir, err := os.MkdirTemp("", "sfxp")
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	sock := filepath.Join(dir, "run", "s")
	cfg := filepath.Join(dir, "starfixd.yaml")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		b.Fatal(err)
	}
	env := []string{"STARFIXD_DSN=" + dsn, "HOME=" + dir, "PATH=" + os.Getenv("PATH")}
	var addr string
	if metrics {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatal(err)
		}
		addr = l.Addr().String()
		_ = l.Close()
		env = append(env, "STARFIXD_TELEMETRY_LISTEN="+addr)
	}
	var log bytes.Buffer
	daemon := exec.CommandContext(ctx, exe, "serve", "--dev", "--config", cfg, "--socket", sock, "--project", benchProject) //nolint:gosec // the binary just built
	daemon.Env, daemon.Stderr = env, &log
	if err := daemon.Start(); err != nil {
		b.Fatal(err)
	}
	defer func() {
		_ = daemon.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = daemon.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = daemon.Process.Kill()
			<-done
		}
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		if c, err := net.Dial("unix", sock); err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			b.Fatalf("starfixd did not listen on %s within a minute:\n%s", sock, log.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	read := func() (rss, heap, goroutines float64) {
		time.Sleep(2 * time.Second) // lets the runtime give back what the start used
		rss = residentBytes(b, daemon.Process.Pid)
		if metrics {
			// The runtime's memory figures are read at most every 15
			// seconds, whatever asks.
			time.Sleep(16 * time.Second)
			m := scrape(b, "http://"+addr+"/metrics")
			heap, goroutines = m["go_memory_used_bytes"], m["go_goroutine_count"]
		}
		return rss, heap, goroutines
	}
	var cost processCost
	cost.idleRSS, cost.idleHeap, cost.idleGoroutines = read()

	// 200 connections past their handshake, of ten principals, each
	// having asked once. They speak to the socket as the bridge does.
	const conns = 200
	var open []net.Conn
	defer func() {
		for _, c := range open {
			_ = c.Close()
		}
	}()
	for i := range conns {
		c, err := net.Dial("unix", sock)
		if err != nil {
			b.Fatal(err)
		}
		open = append(open, c)
		enc, in := proto.NewEncoder(c), bufio.NewReader(c)
		ask, err := proto.Request(1, proto.OpInbox, proto.InboxArgs{Limit: 1})
		if err != nil {
			b.Fatal(err)
		}
		for _, f := range []*proto.Frame{
			{T: proto.FrameBridge, Principal: "principal" + strconv.Itoa(i%10)},
			{T: proto.FrameHello, Proto: proto.Proto, Project: benchProject, Session: "s-" + strconv.Itoa(i), Machine: "bench"},
			ask,
		} {
			if err := enc.Encode(f); err != nil {
				b.Fatal(err)
			}
		}
		for range 2 { // the welcome, and the answer
			line, err := in.ReadBytes('\n')
			if err != nil || bytes.Contains(line, []byte(`"err":`)) {
				b.Fatalf("connection %d: %v %s\n%s", i, err, line, log.String())
			}
		}
	}
	rss, heap, _ := read()
	cost.connRSS, cost.connHeap = (rss-cost.idleRSS)/conns, (heap-cost.idleHeap)/conns

	// The bridge: `starfixd stdio`, one process for each connection sshd
	// lets in, which copies its stdin and stdout to the socket.
	const bridges = 20
	var pids []int
	var stops []func()
	defer func() {
		for _, stop := range stops {
			stop()
		}
	}()
	for i := range bridges {
		bridge := exec.CommandContext(ctx, exe, "stdio", "--config", cfg, "--socket", sock, "--principal", "principal"+strconv.Itoa(i%10)) //nolint:gosec // the binary just built
		bridge.Env = env
		stdin, err := bridge.StdinPipe()
		if err != nil {
			b.Fatal(err)
		}
		stdout, err := bridge.StdoutPipe()
		if err != nil {
			b.Fatal(err)
		}
		if err := bridge.Start(); err != nil {
			b.Fatal(err)
		}
		stops = append(stops, func() {
			_ = stdin.Close()
			_ = bridge.Process.Kill()
			_ = bridge.Wait()
		})
		if err := proto.NewEncoder(stdin).Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: benchProject,
			Session: "s-bridge-" + strconv.Itoa(i), Machine: "bench"}); err != nil {
			b.Fatal(err)
		}
		if line, err := bufio.NewReader(stdout).ReadBytes('\n'); err != nil || bytes.Contains(line, []byte(`"err":`)) {
			b.Fatalf("bridge %d: %v %s", i, err, line)
		}
		pids = append(pids, bridge.Process.Pid)
	}
	time.Sleep(time.Second)
	for _, pid := range pids {
		cost.bridgeRSS += residentBytes(b, pid) / bridges
		cost.bridgeOwn += ownBytes(b, pid) / bridges
	}
	cost.daemonOwn = ownBytes(b, daemon.Process.Pid)
	return cost
}

// residentBytes is pid's resident set size, as ps gives it.
func residentBytes(b *testing.B, pid int) float64 {
	b.Helper()
	out, err := exec.CommandContext(b.Context(), "ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // a process the benchmark started
	if err != nil {
		b.Fatalf("ps: %v", err)
	}
	kib, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		b.Fatalf("ps printed %q for the resident size of %d", out, pid)
	}
	return kib * 1024
}

// ownBytes is the memory that is pid's alone, or 0 where this cannot
// tell: on Linux the private pages of /proc's smaps_rollup, and on macOS
// the physical footprint vmmap prints.
func ownBytes(b *testing.B, pid int) float64 {
	b.Helper()
	switch runtime.GOOS {
	case "linux":
		page, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/smaps_rollup")
		if err != nil {
			return 0
		}
		var kib float64
		for line := range strings.Lines(string(page)) {
			if f := strings.Fields(line); len(f) >= 2 && strings.HasPrefix(f[0], "Private_") {
				n, _ := strconv.ParseFloat(f[1], 64)
				kib += n
			}
		}
		return kib * 1024
	case "darwin":
		out, err := exec.CommandContext(b.Context(), "vmmap", "--summary", strconv.Itoa(pid)).Output() //nolint:gosec // a process the benchmark started
		if err != nil {
			return 0
		}
		for line := range strings.Lines(string(out)) {
			rest, ok := strings.CutPrefix(line, "Physical footprint:")
			if !ok {
				continue
			}
			rest = strings.TrimSpace(rest)
			unit := map[byte]float64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30}[rest[len(rest)-1]]
			n, err := strconv.ParseFloat(rest[:len(rest)-1], 64)
			if err != nil || unit == 0 {
				return 0
			}
			return n * unit
		}
	}
	return 0
}

// scrape reads a Prometheus text page, and returns each metric's value
// summed over its series.
func scrape(b *testing.B, url string) map[string]float64 {
	b.Helper()
	ctx, cancel := context.WithTimeout(b.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		b.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		b.Fatalf("scrape %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	page, err := io.ReadAll(res.Body)
	if err != nil {
		b.Fatal(err)
	}
	out := map[string]float64{}
	for line := range strings.Lines(string(page)) {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if i := strings.IndexByte(name, '{'); i >= 0 {
			// Labels may hold spaces: the value is what follows the brace.
			j := strings.LastIndexByte(line, '}')
			name, value = name[:i], strings.TrimSpace(line[j+1:])
		}
		v, err := strconv.ParseFloat(strings.Fields(value + " 0")[0], 64)
		if err != nil {
			continue
		}
		out[name] += v
	}
	if len(out) == 0 {
		b.Fatalf("scrape %s: no metric in %d bytes", url, len(page))
	}
	return out
}
