package server

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/telemetry"
)

// noLeak fails the test if, once its cleanups have run, more goroutines
// are running than when it was called. It waits for them to settle: the
// goroutines of a closed connection take a moment to return.
func noLeak(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(15 * time.Second)
		for {
			n := runtime.NumGoroutine()
			if n <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Errorf("%d goroutines running, %d before the test:\n%s", n, before, buf[:runtime.Stack(buf, true)])
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// mix is the work one run of TestDeadCollector does: a tracker's usual
// requests, reads and writes, some refused.
func mix(t *testing.T, c *conn, rounds int) {
	t.Helper()
	for i := range rounds {
		is := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("issue %d", i)})
		mustAsk[proto.ShowResult](c, proto.OpShow, proto.ShowArgs{ID: is.ID})
		mustAsk[proto.ListResult](c, proto.OpList, proto.ListArgs{})
		mustAsk[proto.ListResult](c, proto.OpReady, proto.LimitArgs{})
		if _, perr := ask[proto.ShowResult](c, proto.OpShow, proto.ShowArgs{ID: "sf-none"}); perr == nil {
			t.Fatal("show of an unknown issue succeeded")
		}
		if _, perr := ask[proto.WriteResult](c, "no.such.op", struct{}{}); perr == nil {
			t.Fatal("an unknown op succeeded")
		}
		if i%5 == 0 {
			mustAsk[proto.StartResult](c, proto.OpStart, proto.StartArgs{ID: is.ID})
			mustAsk[proto.WriteResult](c, proto.OpComment, proto.CommentArgs{ID: is.ID, Body: "progress"})
			mustAsk[proto.FinishResult](c, proto.OpFinish, proto.FinishArgs{ID: is.ID, Reason: "done"})
		}
	}
}

// A collector that is down, and one that accepts connections and never
// answers, change nothing a client can see (design §16.6): requests are
// no slower than with telemetry off, memory does not grow with the
// failures, no goroutine is left, the failures are logged once for each
// signal, and the stop is not held. Metrics and traces both go to the
// dead collector: every request ends spans that can never be exported.
//
// The thresholds are loose, since the machine may be busy: they catch a
// request that waits for an export, which would cost a timeout each, not
// a few percent.
func TestDeadCollector(t *testing.T) {
	const rounds = 60
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	// Connections queue in the listener's backlog and are never read.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })

	type result struct {
		took time.Duration
		heap uint64
		log  string
	}
	run := func(t *testing.T, endpoint string) result {
		t.Helper()
		noLeak(t)
		log := &syncBuffer{}
		env := map[string]string{}
		if endpoint != "" {
			// Exports every 20 ms, each given 150 ms: many fail, or time
			// out, while the requests run. The queue of spans is small,
			// so that it fills.
			env = map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://" + endpoint, "OTEL_METRIC_EXPORT_INTERVAL": "20",
				"OTEL_METRIC_EXPORT_TIMEOUT": "150", "OTEL_EXPORTER_OTLP_TIMEOUT": "150",
				"OTEL_BSP_SCHEDULE_DELAY": "20", "OTEL_BSP_EXPORT_TIMEOUT": "150", "OTEL_BSP_MAX_QUEUE_SIZE": "64", "OTEL_BSP_MAX_EXPORT_BATCH_SIZE": "16"}
		}
		tel, err := telemetry.New(t.Context(), telemetry.Config{Version: "v0.2.0", PerProject: true, Ops: Ops(), Logger: slogTo(log),
			Getenv: func(k string) string { return env[k] }})
		if err != nil {
			t.Fatal(err)
		}
		if on := endpoint != ""; tel.Enabled() != on || tel.Tracing() != on {
			t.Fatalf("metrics on = %t and traces on = %t with endpoint %q", tel.Enabled(), tel.Tracing(), endpoint)
		}
		s := newServerTel(t, Limits{WriteBurst: 100000, WriteRate: 100000}, tel)
		c := dialPipe(t, s, alice)
		mix(t, c, 3) // warm the connections and caches before timing
		start := time.Now()
		mix(t, c, rounds)
		took := time.Since(start)
		c.hangUp()

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		stop := time.Now()
		_ = tel.Shutdown(ctx)
		if d := time.Since(stop); d > 5*time.Second {
			t.Errorf("stopping telemetry took %s with a 1s deadline", d)
		}
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return result{took: took, heap: ms.HeapAlloc, log: log.String()}
	}

	var off result
	t.Run("telemetry off", func(t *testing.T) { off = run(t, "") })
	for _, tc := range []struct{ name, endpoint string }{
		{"a closed port", closedAddr},
		{"a listener that never answers", silent.Addr().String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, tc.endpoint)
			if limit := 4*off.took + 5*time.Second; got.took > limit {
				t.Errorf("%d rounds of requests took %s, and %s with telemetry off: want no more than %s", rounds, got.took, off.took, limit)
			}
			if limit := off.heap + 64<<20; got.heap > limit {
				t.Errorf("the heap holds %d MiB after the run, and %d MiB with telemetry off: want no more than 64 MiB over", got.heap>>20, off.heap>>20)
			}
			if n := strings.Count(got.log, "a metrics export failed"); n != 1 {
				t.Errorf("%d export failures logged, want exactly 1 for a collector that stays dead:\n%s", n, got.log)
			}
			if n := strings.Count(got.log, "a trace export failed"); n != 1 {
				t.Errorf("%d trace export failures logged, want exactly 1 for a collector that stays dead:\n%s", n, got.log)
			}
			if strings.Contains(got.log, "level=ERROR") {
				t.Errorf("an error was logged:\n%s", got.log)
			}
			t.Logf("%s: %d rounds in %s (%s with telemetry off)", tc.name, rounds, got.took, off.took)
		})
	}
}
