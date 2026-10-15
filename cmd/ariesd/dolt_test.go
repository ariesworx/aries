package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
)

// With telemetry.dolt_metrics_url set, the daemon as serve runs it reads
// Dolt's metrics by itself, `starfixd status` shows them as text and as
// JSON, and the stop leaves nothing running.
func TestRunDaemonReadsDoltsMetrics(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	var hits atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		_, _ = w.Write([]byte("dss_dolt_version 3.3816578e+07\ndss_concurrent_connections 4\nprocess_resident_memory_bytes 5.9670528e+07\n" +
			"go_gc_gomemlimit_bytes 9.223372036854776e+18\ngo_gc_duration_seconds_count 3\n"))
	}))
	t.Cleanup(endpoint.Close)

	s := newSite(t, "")
	rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\ntelemetry:\n  dolt_metrics_url: "+endpoint.URL+"/metrics\n")
	d := runSite(t, s, nil)
	waitFor(t, "the daemon to answer its socket", func() bool {
		_, err := server.Probe(t.Context(), d.sock, adopted, "test")
		return err == nil
	})
	waitFor(t, "a read of Dolt's metrics", func() bool { return hits.Load() > 0 })
	const line = "Dolt 2.4.2, uptime unknown, 4 connections, 57 MiB resident, no memory limit\n"
	waitFor(t, "starfixd status to show Dolt", func() bool { return strings.Contains(status(t, s, d.sock).stdout, line) })
	res := status(t, s, d.sock, "--json")
	var doc proto.StatusResult
	if err := json.Unmarshal([]byte(res.stdout), &doc); res.err != nil || err != nil || strings.Count(res.stdout, "\n") != 1 {
		t.Fatalf("starfixd status --json = %v, %v; want exactly one JSON document, got %q", res.err, err, res.stdout)
	}
	if dolt := doc.Server.Dolt; dolt == nil || dolt.Unknown || dolt.Version != "2.4.2" || dolt.Conns == nil || *dolt.Conns != 4 || dolt.ReadAt == nil {
		t.Errorf("starfixd status --json lacks Dolt's figures: %s", res.stdout)
	}
	if err := d.stop(); err != nil {
		t.Fatalf("runDaemon = %v, want nil", err)
	}
	if out := d.log.String(); !strings.Contains(out, "reading Dolt's metrics for the status report") || !strings.Contains(out, endpoint.URL+"/metrics") {
		t.Errorf("the log does not say that Dolt's metrics are read, and from where:\n%s", out)
	}
}

// With no URL, which is the default, the daemon asks nothing of Dolt's
// endpoint and its status says nothing of Dolt.
func TestRunDaemonReadsNothingOfDoltByDefault(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	s := newSite(t, "")
	rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\n")
	d := runSite(t, s, nil)
	waitFor(t, "the daemon to answer its socket", func() bool {
		_, err := server.Probe(t.Context(), d.sock, adopted, "test")
		return err == nil
	})
	res := status(t, s, d.sock)
	if res.err != nil || !strings.HasPrefix(res.stdout, "starfixd ") || strings.Contains(res.stdout, "Dolt") {
		t.Errorf("starfixd status = %v; want the report, with nothing of Dolt:\n%s", res.err, res.stdout)
	}
	if res := status(t, s, d.sock, "--json"); res.err != nil || strings.Contains(res.stdout, "dolt") {
		t.Errorf("starfixd status --json = %v; want nothing of Dolt:\n%s", res.err, res.stdout)
	}
	if err := d.stop(); err != nil {
		t.Fatalf("runDaemon = %v, want nil", err)
	}
	if out := d.log.String(); strings.Contains(out, "Dolt's metrics") {
		t.Errorf("a daemon with no URL logged about Dolt's metrics:\n%s", out)
	}
}

// The daemon checks the URL itself when it starts, whatever resolved its
// settings: one that is not on loopback fails the start, with a fix,
// before the database is touched.
func TestRunDaemonRefusesDoltsMetricsElsewhere(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	set := server.Settings{DSN: "nobody@tcp(127.0.0.1:1)/nowhere", Project: adopted, Socket: filepath.Join(t.TempDir(), "d.sock")}
	set.Limits = server.DefaultLimits
	set.Telemetry.DoltMetricsURL = "http://192.0.2.10:11228/metrics"
	err := runDaemon(t.Context(), set, slog.New(slog.DiscardHandler), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "telemetry dolt_metrics_url") || !strings.Contains(err.Error(), "not a loopback address") ||
		!strings.Contains(err.Error(), "; fix: ") {
		t.Errorf("runDaemon with Dolt's metrics on another machine = %v, want a refusal naming the setting and a fix", err)
	}
}
