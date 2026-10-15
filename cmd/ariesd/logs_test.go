package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ariesworx/starfix/internal/store"
)

// With OTEL_LOGS_EXPORTER=otlp the daemon's log lines also go out over
// OTLP, to /v1/logs, and stderr has them all the same (design §16.4).
// Without it nothing of the log is exported, whatever else is: the
// journal is the logs' home.
func TestRunDaemonExportsLogs(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	for _, tc := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"asked for", map[string]string{"OTEL_LOGS_EXPORTER": "otlp", "OTEL_BLRP_SCHEDULE_DELAY": "50"}, true},
		{"not asked for", map[string]string{"OTEL_METRIC_EXPORT_INTERVAL": "50"}, false},
		{"set to none", map[string]string{"OTEL_LOGS_EXPORTER": "none", "OTEL_METRIC_EXPORT_INTERVAL": "50"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			var mu sync.Mutex
			posted := map[string][]byte{}
			col := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				posted[r.URL.Path] = append(posted[r.URL.Path], b...)
				mu.Unlock()
			}))
			t.Cleanup(col.Close)
			got := func(path string) []byte {
				mu.Lock()
				defer mu.Unlock()
				return bytes.Clone(posted[path])
			}

			s := newSite(t, "")
			rewrite(t, s, "project: "+adopted+"\nadmins: [dana]\n")
			env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": col.URL, "OTEL_TRACES_EXPORTER": "none"}
			for k, v := range tc.env {
				env[k] = v
			}
			d := runSite(t, s, env)
			waitFor(t, "the daemon to serve", func() bool { return strings.Contains(d.log.String(), "starfixd serving") })
			if w := hello(t, d.sock, "alice", second); w.Err == nil {
				t.Fatalf("hello for a project the server does not serve was welcomed")
			}
			waitFor(t, "the refused handshake to be logged", func() bool { return strings.Contains(d.log.String(), "event=handshake.failed") })
			if tc.want {
				waitFor(t, "the lines to reach the collector", func() bool {
					b := got("/v1/logs")
					return bytes.Contains(b, []byte("daemon.serving")) && bytes.Contains(b, []byte("handshake.failed"))
				})
			} else {
				waitFor(t, "an export of metrics to reach the collector", func() bool { return len(got("/v1/metrics")) > 0 })
			}
			if err := d.stop(); err != nil {
				t.Fatalf("runDaemon = %v, want nil", err)
			}
			logs := got("/v1/logs")
			if (len(logs) > 0) != tc.want {
				t.Errorf("%d bytes of logs were exported, want some: %t", len(logs), tc.want)
			}
			out := d.log.String()
			for _, want := range []string{`msg="starfixd serving" event=daemon.serving`, `msg="handshake failed" event=handshake.failed`} {
				if !strings.Contains(out, want) {
					t.Errorf("stderr lacks %q, which it has whatever is exported:\n%s", want, out)
				}
			}
			if tc.want {
				// The message and the event, and nothing of the error's
				// text, which names the project asked for.
				for _, want := range []string{"starfixd serving", "handshake failed", "starfixd"} {
					if !bytes.Contains(logs, []byte(want)) {
						t.Errorf("the exported logs lack %q", want)
					}
				}
				if bytes.Contains(logs, []byte(second)) || bytes.Contains(logs, []byte("alice")) {
					t.Errorf("the exported logs hold an error's text or a name")
				}
				if !strings.Contains(out, " logs="+col.URL+"/v1/logs ") {
					t.Errorf("the start-up line does not say where logs go:\n%s", out)
				}
			}
		})
	}
}

// The daemon serve builds writes the audit stream unless the config
// file says audit_log: false (design §16.3): an access change in the
// control database is a log line with audit=true, or is not.
func TestServeAuditLogSetting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, extra string
		want        bool
	}{
		{"on by default", "", true},
		{"set on", "telemetry:\n  audit_log: true\n", true},
		{"set off", "telemetry:\n  audit_log: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSite(t, tc.extra)
			keys := writeKeys(t, forced(t, "dana", "dana-1"))
			if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
				t.Fatalf("control init: %v\n%s", got.err, got.stderr)
			}
			var log bytes.Buffer
			d, err := openDaemon(t.Context(), s.settings(t), slog.New(slog.NewTextHandler(&log, nil)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			// A change made after the start, as an admin's is.
			dana := store.Actor{Principal: "dana", Session: "s-d", Machine: "m"}
			if _, err := d.ctl.AddPrincipal(t.Context(), dana, "hal", adopted); err != nil {
				t.Fatal(err)
			}
			d.srv.Audit(t.Context())
			line := "msg=audit event=audit.principal.add audit=true"
			if got := strings.Contains(log.String(), line) && strings.Contains(log.String(), " principal=dana session=s-d op=principal.add target=hal subject=hal"); got != tc.want {
				t.Errorf("the log has the audit line for the new principal: %t, want %t:\n%s", got, tc.want, log.String())
			}
			if strings.Contains(log.String(), "audit=true") != tc.want {
				t.Errorf("the log has audit lines: %t, want %t:\n%s", !tc.want, tc.want, log.String())
			}
		})
	}
}
