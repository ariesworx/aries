package telemetry

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/logs"
)

// The loopback listener (design §16.3, decision 29): one HTTP listener,
// on a loopback address only, with no authentication, for a collector or
// a monitor on the same machine. It answers GET /metrics, /healthz and
// /readyz, and nothing else.

// DefaultRequests is how many requests the listener serves at once when
// ServeOptions.Requests is zero.
const DefaultRequests = 8

// Timeouts of the listener. A scrape is small and local, so they are
// short: they are what keeps a client that connects and says nothing, or
// reads nothing, from holding a connection.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 30 * time.Second
	maxHeaderBytes    = 16 << 10
	stopTimeout       = 2 * time.Second
)

// CheckAddr refuses a listen address that is not HOST:PORT with a port
// from 1 to 65535, or whose host is an IP address that is not loopback.
// It resolves nothing: a host that is a name is checked by [Listen],
// when the daemon starts.
func CheckAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("telemetry listen address %q is not HOST:PORT; fix: set listen: under telemetry: to a loopback address and a port, such as 127.0.0.1:9464", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("telemetry listen address %q has no port from 1 to 65535; fix: set listen: under telemetry: to a loopback address and a port, such as 127.0.0.1:9464", addr)
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		return notLoopback(addr, ip.String())
	}
	return nil
}

// notLoopback refuses addr, whose host is or resolves to ip.
func notLoopback(addr, ip string) error {
	return fmt.Errorf("telemetry listen address %q is %s, which is not a loopback address, and the listener has no authentication; "+
		"fix: set listen: under telemetry: to 127.0.0.1:PORT or [::1]:PORT, and have a collector on this machine read it (docs/server.md, Observability)", addr, ip)
}

// Listen opens the loopback listener at addr, HOST:PORT. A host that is
// a name is resolved, and refused unless every address it has is
// loopback; the listener is then bound to the first of them, not to the
// name, so what was checked is what is bound. A failure to bind names a
// fix.
func Listen(ctx context.Context, addr string) (net.Listener, error) {
	return listen(ctx, addr, net.DefaultResolver.LookupIPAddr)
}

// listen is Listen with the lookup of names given, for tests.
func listen(ctx context.Context, addr string, lookup func(context.Context, string) ([]net.IPAddr, error)) (net.Listener, error) {
	if err := CheckAddr(addr); err != nil {
		return nil, err
	}
	host, port, _ := net.SplitHostPort(addr) // CheckAddr split it
	bind := addr
	if net.ParseIP(host) == nil {
		ips, err := lookup(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("telemetry listen address %q: the host could not be resolved; fix: set listen: under telemetry: to 127.0.0.1:%s", addr, port)
		}
		for _, ip := range ips {
			if !ip.IP.IsLoopback() {
				return nil, notLoopback(addr, ip.IP.String())
			}
		}
		bind = net.JoinHostPort(ips[0].IP.String(), port)
	}
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", bind)
	if err != nil {
		return nil, fmt.Errorf("telemetry listen address %q: %w; fix: free the port or set listen: under telemetry: to another, or remove it to run without the listener", addr, err)
	}
	return l, nil
}

// ServeOptions configure [Telemetry.Serve].
type ServeOptions struct {
	// Ready answers /readyz: empty when the daemon is ready, and
	// otherwise one line saying what is not. Nil is always ready. It is
	// called for every request to /readyz, so it must not run a query
	// each time.
	Ready func(context.Context) string
	// Requests caps the requests served at once; one more is answered
	// 503. Twice it caps the connections open, and one more is closed
	// unanswered. Zero takes DefaultRequests.
	Requests int
	// Logger receives the HTTP server's own errors. Nil discards.
	Logger *slog.Logger
}

// Serve answers on l until ctx is done, then closes l, gives requests in
// flight a moment to end, and returns nil once every goroutine it
// started has returned. It serves
//
//	GET /metrics  the instruments in Prometheus's text format, or 404
//	              when t does not offer them (Config.Pull)
//	GET /healthz  200 while the process serves
//	GET /readyz   200 when o.Ready returns "", else 503 and its line
//
// and answers any other path 404. A nil t serves the two health paths.
func (t *Telemetry) Serve(ctx context.Context, l net.Listener, o ServeOptions) error {
	if o.Requests <= 0 {
		o.Requests = DefaultRequests
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	if t != nil && t.pull != nil {
		mux.Handle("GET /metrics", t.pull)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { plain(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if o.Ready != nil {
			if why := o.Ready(r.Context()); why != "" {
				plain(w, http.StatusServiceUnavailable, "not ready: "+why)
				return
			}
		}
		plain(w, http.StatusOK, "ready")
	})
	slots := make(chan struct{}, o.Requests)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				mux.ServeHTTP(w, r)
			default:
				t.Count(LimitRefusals, LimitTelemetryRequests)
				w.Header().Set("Retry-After", "1")
				plain(w, http.StatusServiceUnavailable, "busy: too many requests in flight")
			}
		}),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          log.New(listenerLog{o.Logger}, "", 0),
	}
	// However Serve ends, the goroutine that stops the server on ctx is
	// waited for.
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		// ctx is done; the requests in flight get a moment of their own.
		sctx, stop := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		defer stop()
		if err := srv.Shutdown(sctx); err != nil {
			_ = srv.Close()
		}
	}()
	err := srv.Serve(&capped{Listener: l, max: 2 * o.Requests})
	asked := ctx.Err() != nil
	cancel()
	<-done
	if !asked {
		return fmt.Errorf("telemetry listener: %w", err)
	}
	return nil
}

// plain writes a one-line text answer.
func plain(w http.ResponseWriter, code int, line string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, line+"\n")
}

// capped is a listener that holds at most max connections open: one
// accepted past that is closed at once, so a local client cannot take
// file descriptors without bound by opening connections it never uses.
type capped struct {
	net.Listener
	max int

	mu   sync.Mutex
	open int
}

func (c *capped) Accept() (net.Conn, error) {
	for {
		conn, err := c.Listener.Accept()
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		full := c.open >= c.max
		if !full {
			c.open++
		}
		c.mu.Unlock()
		if full {
			_ = conn.Close()
			continue
		}
		return &cappedConn{Conn: conn, c: c}, nil
	}
}

// cappedConn gives its place back when it is closed, and before the
// connection itself closes: a client that has seen the listener close a
// connection finds its place free.
type cappedConn struct {
	net.Conn
	c    *capped
	once sync.Once
}

func (cc *cappedConn) Close() error {
	cc.once.Do(func() {
		cc.c.mu.Lock()
		cc.c.open--
		cc.c.mu.Unlock()
	})
	return cc.Conn.Close()
}

// listenerLog is the HTTP server's error log over starfixd's logger:
// each thing it says is one line of the schema, with what it said, which
// is the standard library's text, in one field.
type listenerLog struct{ log *slog.Logger }

func (l listenerLog) Write(p []byte) (int, error) {
	l.log.Warn("the telemetry listener reported an error", "event", logs.TelemetryListenerError, logs.KeyDetail, strings.TrimSpace(string(p)))
	return len(p), nil
}
