package telemetry

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// lookup is a fake resolver.
func lookup(hosts map[string][]string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(_ context.Context, host string) ([]net.IPAddr, error) {
		ips, ok := hosts[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []net.IPAddr
		for _, ip := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return out, nil
	}
}

// The listener binds a loopback address or nothing.
func TestListenLoopbackOnly(t *testing.T) {
	t.Parallel()
	hosts := map[string][]string{
		"localhost":        {"127.0.0.1", "::1"},
		"metrics.internal": {"192.0.2.10"},
		"mixed.internal":   {"127.0.0.1", "192.0.2.10"},
		"none.internal":    {},
	}
	for _, tc := range []struct {
		addr    string
		wantErr string // empty: it binds
		static  bool   // CheckAddr alone refuses it, before any lookup
	}{
		{addr: "127.0.0.1:0"},
		{addr: "127.0.0.2:0"},
		{addr: "localhost:0"},
		{addr: "0.0.0.0:9464", wantErr: "not a loopback address", static: true},
		{addr: "[::]:9464", wantErr: "not a loopback address", static: true},
		{addr: "192.0.2.10:9464", wantErr: "not a loopback address", static: true},
		{addr: "[2001:db8::1]:9464", wantErr: "not a loopback address", static: true},
		{addr: ":9464", wantErr: "not HOST:PORT", static: true},
		{addr: "127.0.0.1", wantErr: "not HOST:PORT", static: true},
		{addr: "127.0.0.1:http", wantErr: "no port from 1 to 65535", static: true},
		{addr: "127.0.0.1:70000", wantErr: "no port from 1 to 65535", static: true},
		{addr: "", wantErr: "not HOST:PORT", static: true},
		{addr: "metrics.internal:9464", wantErr: "192.0.2.10, which is not a loopback address"},
		{addr: "mixed.internal:9464", wantErr: "192.0.2.10, which is not a loopback address"},
		{addr: "none.internal:9464", wantErr: "could not be resolved"},
		{addr: "unknown.internal:9464", wantErr: "could not be resolved"},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			addr := tc.addr
			if tc.wantErr == "" {
				// Port 0 is for the test only: CheckAddr wants a real port.
				if err := CheckAddr(strings.Replace(addr, ":0", ":9464", 1)); err != nil {
					t.Errorf("CheckAddr = %v, want nil", err)
				}
				l, err := listenAny(t, addr, lookup(hosts))
				if err != nil {
					if strings.HasPrefix(addr, "127.0.0.2") {
						t.Skipf("this system has no 127.0.0.2: %v", err)
					}
					t.Fatalf("listen(%q) = %v", addr, err)
				}
				defer func() { _ = l.Close() }()
				if ip := l.Addr().(*net.TCPAddr).IP; !ip.IsLoopback() {
					t.Errorf("listen(%q) bound %s, which is not loopback", addr, ip)
				}
				return
			}
			if err := CheckAddr(addr); (err != nil) != tc.static {
				t.Errorf("CheckAddr(%q) = %v, want an error: %t", addr, err, tc.static)
			}
			l, err := listen(t.Context(), addr, lookup(hosts))
			if err == nil {
				_ = l.Close()
				t.Fatalf("listen(%q) bound %s, want a refusal", addr, l.Addr())
			}
			if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "fix: ") {
				t.Errorf("listen(%q) = %v, want an error containing %q and a fix", addr, err, tc.wantErr)
			}
		})
	}
}

// listenAny is listen for an address with port 0, which CheckAddr
// refuses in a setting and a test needs.
func listenAny(t *testing.T, addr string, look func(context.Context, string) ([]net.IPAddr, error)) (net.Listener, error) {
	t.Helper()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	// Find a free port on the host's first address, then bind it by name.
	ip := host
	if net.ParseIP(host) == nil {
		ips, err := look(t.Context(), host)
		if err != nil {
			return nil, err
		}
		ip = ips[0].IP.String()
	}
	probe, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(probe.Addr().String())
	_ = probe.Close()
	return listen(t.Context(), net.JoinHostPort(host, port), look)
}

// A port that is taken fails the start, with a fix.
func TestListenBindFailure(t *testing.T) {
	t.Parallel()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	l, err := Listen(t.Context(), held.Addr().String())
	if err == nil {
		_ = l.Close()
		t.Fatalf("Listen on a taken port succeeded")
	}
	if !strings.Contains(err.Error(), "fix: free the port") {
		t.Errorf("Listen on a taken port = %v, want a fix", err)
	}
}

// served starts tel's listener with o and returns its URL. The listener
// has stopped, and its goroutines returned, when the test's cleanups
// have run.
func served(t *testing.T, tel *Telemetry, o ServeOptions) string {
	t.Helper()
	l, err := Listen(t.Context(), freeAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tel.Serve(ctx, l, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve = %v, want nil after its context ended", err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("Serve did not return 10s after its context ended")
		}
	})
	return "http://" + l.Addr().String()
}

// freeAddr returns a loopback address with a port that was free a moment
// ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func TestServe(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	tel, _, err := NewManual(Config{Pull: true, Ops: testOps})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	tel.Count(Handshakes, ResultOK)
	var mu sync.Mutex
	why := ""
	base := served(t, tel, ServeOptions{Ready: func(context.Context) string {
		mu.Lock()
		defer mu.Unlock()
		return why
	}})
	for _, tc := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/healthz", 200, "ok\n"},
		{"GET", "/readyz", 200, "ready\n"},
		{"GET", "/metrics", 200, `aries_handshakes_total{result="ok"} 1`},
		{"HEAD", "/healthz", 200, ""},
		{"GET", "/", 404, ""},
		{"GET", "/metrics/", 404, ""},
		// What the SDK's dependencies register on http.DefaultServeMux
		// (golang.org/x/net/trace does, at init) is not served here.
		{"GET", "/debug/pprof/", 404, ""},
		{"GET", "/debug/requests", 404, ""},
		{"GET", "/debug/events", 404, ""},
		{"GET", "/debug/vars", 404, ""},
		{"GET", "/healthz/x", 404, ""},
		{"POST", "/healthz", 405, ""},
		{"DELETE", "/metrics", 405, ""},
	} {
		status, body := do(t, tc.method, base+tc.path)
		if status != tc.status || !strings.Contains(body, tc.body) {
			t.Errorf("%s %s = %d %q, want %d and a body containing %q", tc.method, tc.path, status, body, tc.status, tc.body)
		}
	}
	mu.Lock()
	why = "the last check of Dolt failed"
	mu.Unlock()
	if status, body := do(t, "GET", base+"/readyz"); status != 503 || body != "not ready: the last check of Dolt failed\n" {
		t.Errorf("GET /readyz when not ready = %d %q, want 503 and one line saying why", status, body)
	}
	if status, _ := do(t, "GET", base+"/healthz"); status != 200 {
		t.Errorf("GET /healthz when not ready = %d, want 200: the process still serves", status)
	}
}

// A nil Telemetry, and one that offers no pull, still answer health.
func TestServeWithoutMetrics(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	var tel *Telemetry
	base := served(t, tel, ServeOptions{})
	if status, _ := do(t, "GET", base+"/metrics"); status != 404 {
		t.Errorf("GET /metrics with no metrics offered = %d, want 404", status)
	}
	if status, body := do(t, "GET", base+"/readyz"); status != 200 || body != "ready\n" {
		t.Errorf("GET /readyz = %d %q, want 200 ready", status, body)
	}
}

func do(t *testing.T, method, url string) (int, string) {
	t.Helper()
	status, body, err := try(t, method, url)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return status, body
}

// try is do for a request that the listener may not answer: err is the
// client's, as when the connection was one past the listener's cap and
// was closed.
//
// The request has a connection of its own, and try returns only once the
// listener has closed it. An answer comes before the listener has let go
// of the connection that carried it, so a client that went on at the
// answer could find that connection still counted against the cap on
// connections, and its next one closed unanswered. The listener gives a
// connection's place back before it closes it (cappedConn.Close), so
// after try nothing of this request counts.
func try(t *testing.T, method, url string) (int, string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Close = true // the listener closes the connection after its answer
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", req.URL.Host)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = conn.Close() }()
	// Only so that a listener which neither answers nor closes fails the
	// test instead of hanging it.
	_ = conn.SetDeadline(time.Now().Add(time.Minute))
	if err := req.Write(conn); err != nil {
		return 0, "", err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return 0, "", err
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	if _, err := io.Copy(io.Discard, br); err != nil {
		return 0, "", fmt.Errorf("waiting for the listener to close the connection after its answer: %w", err)
	}
	return resp.StatusCode, string(body), nil
}

// Requests past the cap are answered 503 at once and counted, and
// connections past twice the cap are closed.
func TestServeCaps(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	for _, tc := range []struct {
		name     string
		requests int // ServeOptions.Requests; 0 takes the default
		cap      int
	}{
		{"default", 0, DefaultRequests},
		{"one", 1, 1},
		{"three", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			tel, m, err := NewManual(Config{Ops: testOps})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
			entered, release := make(chan struct{}, 64), make(chan struct{})
			var once sync.Once
			free := func() { once.Do(func() { close(release) }) }
			base := served(t, tel, ServeOptions{Requests: tc.requests, Ready: func(context.Context) string {
				entered <- struct{}{}
				<-release
				return ""
			}})
			t.Cleanup(free) // before the listener stops, which waits for them

			type answer struct {
				status int
				err    error
			}
			var wg sync.WaitGroup
			held := make(chan answer, tc.cap)
			for range tc.cap {
				wg.Go(func() {
					status, _, err := try(t, "GET", base+"/readyz")
					held <- answer{status, err}
				})
			}
			for range tc.cap {
				select {
				case <-entered:
				case <-time.After(time.Minute):
					t.Fatal("the requests under the cap did not all reach the handler")
				}
			}
			// The cap is full: the next request is refused at once, whatever
			// it asks for. The listener holds twice the cap in connections,
			// which with a cap of one is the request held and one more: each
			// of these is answered only because the one before it has gone
			// from the listener's count, which try waits for.
			for _, path := range []string{"/readyz", "/healthz", "/metrics"} {
				status, body := do(t, "GET", base+path)
				if status != 503 || !strings.Contains(body, "busy") {
					t.Errorf("GET %s with %d requests in flight = %d %q, want 503 busy", path, tc.cap, status, body)
				}
			}
			free()
			wg.Wait()
			close(held)
			for a := range held {
				if a.err != nil || a.status != 200 {
					t.Errorf("a request under the cap = %d, %v; want 200", a.status, a.err)
				}
			}
			if status, _ := do(t, "GET", base+"/healthz"); status != 200 {
				t.Errorf("GET /healthz after the cap freed = %d, want 200", status)
			}
			snap := collect(t, m)
			if v := snap.Value("aries.limit.refusals", "limit", LimitTelemetryRequests); v != 3 {
				t.Errorf("aries.limit.refusals{limit=%s} = %v, want 3", LimitTelemetryRequests, v)
			}
		})
	}
}

// Connections past twice the cap are closed unanswered, so a client that
// opens connections and sends nothing takes a bounded number.
func TestServeCapsConnections(t *testing.T) {
	// Serial: counts the process's goroutines (noLeak).
	noLeak(t)
	base := served(t, nil, ServeOptions{Requests: 2})
	addr := strings.TrimPrefix(base, "http://")
	var idle []net.Conn
	t.Cleanup(func() {
		for _, c := range idle {
			_ = c.Close()
		}
	})
	for range 4 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		idle = append(idle, c)
	}
	// The four are accepted in order, so the fifth is past the cap once
	// they are: it is closed without an answer.
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = c.Write([]byte("GET /healthz HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
		buf := make([]byte, 16)
		n, _ := c.Read(buf)
		_ = c.Close()
		if n == 0 {
			break // closed unanswered
		}
		if time.Now().After(deadline) {
			t.Fatalf("a connection past twice the cap was answered %q", buf[:n])
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A place frees when a connection closes.
	_ = idle[0].Close()
	deadline = time.Now().Add(10 * time.Second)
	for {
		tr := &http.Transport{DisableKeepAlives: true}
		resp, err := (&http.Client{Transport: tr, Timeout: 2 * time.Second}).Get(base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no connection was accepted after one closed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
