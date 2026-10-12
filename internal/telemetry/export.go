package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// guarded is the OTLP exporter with its failures handled here, once: a
// failed export is counted on starfix.telemetry.dropped and logged,
// sampled, and never returned to the SDK, whose handler would log it
// again at every interval.
type guarded struct {
	sdkmetric.Exporter
	t *Telemetry
}

// Export sends rm. Nothing is lost for good when it fails: the totals
// are cumulative, and the next export carries them.
func (g *guarded) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	if err := g.Exporter.Export(ctx, rm); err != nil {
		if errors.Is(err, context.Canceled) {
			// The daemon is stopping, and cut short an export that was
			// under way: no failure of the collector's, and the last
			// export, which follows, carries the totals.
			return nil
		}
		g.t.Count(Dropped, SignalMetrics)
		g.t.errs.failed("telemetry: a metrics export failed; the next one carries the same totals", err)
		return nil
	}
	g.t.errs.ok()
	return nil
}

// errEvery is how often a failure that keeps happening is logged again.
const errEvery = 10 * time.Minute

// errlog writes the exporters' failures to starfixd's log (design
// §16.6): the first at once, and while they go on one line every
// errEvery saying how many were left out, so a collector that is down
// for a day costs about 150 lines, not one for each export. Every line
// is redacted first.
type errlog struct {
	log     *slog.Logger
	secrets []string
	// what names the exports in the line that says they succeed again:
	// "metrics" or "trace".
	what string
	// now is the clock; tests replace it.
	now func() time.Time

	mu sync.Mutex
	// last is when a failure was last logged; suppressed counts those
	// left out since, and failing those since the last success.
	last       time.Time
	suppressed int
	failing    int
}

func newErrlog(log *slog.Logger, secrets []string, what string) *errlog {
	return &errlog{log: log, secrets: secrets, what: what, now: time.Now}
}

// redact removes every secret from s: a header's value may be a token,
// and a collector's refusal, or a proxy's error page, may repeat the
// request it was sent.
func (e *errlog) redact(s string) string {
	for _, sec := range e.secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	return s
}

// failed logs msg and err, unless one was logged within errEvery.
func (e *errlog) failed(msg string, err error) {
	e.mu.Lock()
	now := e.now()
	e.failing++
	if !e.last.IsZero() && now.Sub(e.last) < errEvery && !now.Before(e.last) {
		e.suppressed++
		e.mu.Unlock()
		return
	}
	left := e.suppressed
	e.last, e.suppressed = now, 0
	e.mu.Unlock()
	attrs := []any{"err", e.redact(err.Error())}
	if left > 0 {
		attrs = append(attrs, "failures_not_logged", left)
	}
	e.log.Warn(msg, attrs...)
}

// ok notes an export that succeeded, and says so once if exports had
// been failing.
func (e *errlog) ok() {
	e.mu.Lock()
	n := e.failing
	e.failing, e.suppressed, e.last = 0, 0, time.Time{}
	e.mu.Unlock()
	if n > 0 {
		e.log.Info("telemetry: "+e.what+" exports succeed again", "failed", n)
	}
}

// setSDKLogging sends what the SDK itself reports to e: errors it
// handles on its own goroutines (otel.Handle), and its internal logger's
// errors, which would otherwise be printed on stderr unformatted. The
// SDK's informational lines are dropped. Both settings are the
// process's, so the latest Telemetry made has them.
func setSDKLogging(e *errlog) {
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		if err != nil {
			e.failed("telemetry: the SDK reported an error", err)
		}
	}))
	otel.SetLogger(logr.New(sdkSink{e}))
}

// sdkSink is the SDK's internal logger over an errlog.
type sdkSink struct{ e *errlog }

func (sdkSink) Init(logr.RuntimeInfo)            {}
func (sdkSink) Enabled(int) bool                 { return false }
func (sdkSink) Info(int, string, ...any)         {}
func (s sdkSink) WithValues(...any) logr.LogSink { return s }
func (s sdkSink) WithName(string) logr.LogSink   { return s }

// Error logs msg and err; the SDK's key-value pairs are left out, since
// nothing vets what they hold.
func (s sdkSink) Error(err error, msg string, _ ...any) {
	if err == nil {
		err = fmt.Errorf("%s", msg)
	}
	s.e.failed("telemetry: the SDK reported an error: "+s.e.redact(msg), err)
}

// promLog is the Prometheus handler's error log over an errlog.
type promLog struct{ e *errlog }

func (p promLog) Println(v ...any) {
	p.e.failed("telemetry: a scrape failed", fmt.Errorf("%s", strings.TrimSpace(fmt.Sprintln(v...))))
}
