package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/telemetry"
)

// The soak with a flood (transport.md §12.4, item 6), which
// ARIES_SOAK_FLOOD=1 asks for. Beside the sessions, goroutines send the
// HTTPS listener what no one signed in would: TLS handshakes begun and
// abandoned, upgrades that never prove a key, junk to /oauth/token and
// /oauth/authorize, and connections that are slow to send their headers
// or their body. The run reports what was sent, how each kind ended at
// the client, and, from the daemon's own instruments, which limit
// refused what. It checks nothing of the flood: every check of the soak
// holds as without it, and the report is for a person to judge, by the
// latency of the sessions' ops set beside a run without the flood.
//
//	ARIES_SOAK_FLOOD        1 to flood; nothing for none.
//	ARIES_SOAK_FLOOD_RATE   attacks begun each second, 20 by default.
//	ARIES_SOAK_FLOOD_CONNS  most attacks open at once, 100 by default;
//	                        one that would pass it is counted, not sent.
//	ARIES_SOAK_FLOOD_FROM   a loopback address, such as 127.0.0.2, to
//	                        send from (Linux has all of 127.0.0.0/8; on
//	                        macOS: sudo ifconfig lo0 alias 127.0.0.2).
//
// The flood goes straight to the daemon's listener, not through the
// world's front, so that the daemon sees the flood's addresses. The
// sessions' connections come from loopback too, so from 127.0.0.1 the
// flood and the sessions are one address to the listener, and the
// per-address cap (https_conns_per_addr) would shut the sessions out:
// without ARIES_SOAK_FLOOD_FROM the run raises that cap, and only the
// per-server cap and the timeouts are the listener's defenses in the
// run. With it, the cap keeps its default and the flood is one address
// beside the sessions' other.
//
// The flood sends no key proof that names a key, so it never takes from
// the auth-failure bucket (the soak sets it far past its need all the
// same, as a removed key's sessions would empty it). The run turns on
// the daemon's sign-in provider, so that /oauth/* routes exist.
const (
	envFlood      = "ARIES_SOAK_FLOOD"
	envFloodRate  = "ARIES_SOAK_FLOOD_RATE"
	envFloodConns = "ARIES_SOAK_FLOOD_CONNS"
	envFloodFrom  = "ARIES_SOAK_FLOOD_FROM"
)

// floodConfig shapes one run's flood.
type floodConfig struct {
	on bool
	// rate is how many attacks begin each second, and conns the most
	// that are open at once.
	rate, conns int
	// from is the loopback address the attacks come from; empty is the
	// system's choice, 127.0.0.1.
	from string
}

// floodConfigFrom reads the flood's variables.
func floodConfigFrom(getenv func(string) string) (floodConfig, error) {
	var c floodConfig
	switch v := getenv(envFlood); v {
	case "":
		return c, nil
	case "1":
		c.on = true
	default:
		return c, fmt.Errorf("%s=%q: want 1, or nothing for no flood", envFlood, v)
	}
	c.rate, c.conns = 20, 100
	for _, f := range []struct {
		env  string
		dst  *int
		most int
	}{{envFloodRate, &c.rate, 1000}, {envFloodConns, &c.conns, 10000}} {
		if v := getenv(f.env); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > f.most {
				return c, fmt.Errorf("%s=%q: want a number from 1 to %d", f.env, v, f.most)
			}
			*f.dst = n
		}
	}
	if v := getenv(envFloodFrom); v != "" {
		ip, err := netip.ParseAddr(v)
		if err != nil || !ip.Is4() || !ip.IsLoopback() {
			return c, fmt.Errorf("%s=%q: want an IPv4 loopback address such as 127.0.0.2", envFloodFrom, v)
		}
		c.from = v
	}
	return c, nil
}

// floodHold is the longest an attack that holds a connection keeps it.
const floodHold = 30 * time.Second

// flood is one run's flood.
type flood struct {
	cfg floodConfig
	w   *world
	// manual reads the daemon's instruments, which the flood turns on
	// when the run has no telemetry of its own.
	manual *telemetry.Manual

	open, peak atomic.Int64
	seq        atomic.Int64

	mu      sync.Mutex
	sent    map[string]int
	ended   map[string]map[string]int // kind → how it ended at the client
	skipped int
}

// floodKinds are the attacks, begun in turn.
var floodKinds = []string{"tcp_abandon", "tls_half_hello", "tls_abandon", "connect_plain", "upgrade_silent", "upgrade_junk",
	"oauth_token", "oauth_authorize", "slow_headers", "slow_body"}

// newFloodTel makes the telemetry the daemon runs with in a run that
// floods and has none of its own, so that the report can say which limit
// refused what. Nothing checks it. Call it before the world is made.
func newFloodTel(t *testing.T, logger *slog.Logger) (*telemetry.Telemetry, *telemetry.Manual) {
	t.Helper()
	tel, manual, err := telemetry.NewManual(telemetry.Config{Version: "v0.2.0", Ops: server.Ops(), PerProject: true, Logger: logger})
	if err != nil {
		t.Fatalf("soak: flood: telemetry: %v", err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	return tel, manual
}

// run floods until ctx is done, then closes every connection it holds
// and waits for each attack to end.
func (f *flood) run(ctx context.Context) {
	f.sent, f.ended = map[string]int{}, map[string]map[string]int{}
	var wg sync.WaitGroup
	if f.manual != nil {
		// The instruments need the spans taken now and then, or the
		// Manual keeps every one.
		wg.Go(func() {
			for ctx.Err() == nil {
				f.manual.TakeSpans()
				pause(ctx, 500*time.Millisecond)
			}
		})
	}
	tick := time.NewTicker(time.Second / time.Duration(f.cfg.rate))
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-tick.C:
		}
		kind := floodKinds[i%len(floodKinds)]
		if n := f.open.Add(1); n > int64(f.cfg.conns) {
			f.open.Add(-1)
			f.mu.Lock()
			f.skipped++
			f.mu.Unlock()
			continue
		} else if n > f.peak.Load() {
			f.peak.Store(n)
		}
		f.mu.Lock()
		f.sent[kind]++
		f.mu.Unlock()
		wg.Go(func() {
			defer f.open.Add(-1)
			out := f.attack(ctx, kind)
			f.mu.Lock()
			if f.ended[kind] == nil {
				f.ended[kind] = map[string]int{}
			}
			f.ended[kind][out]++
			f.mu.Unlock()
		})
	}
}

// addr is the daemon's listener now, which changes at a restart.
func (f *flood) addr() string {
	f.w.web.mu.Lock()
	defer f.w.web.mu.Unlock()
	return f.w.web.upstream
}

// dial opens a TCP connection to addr, from the flood's address.
func (f *flood) dial(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	if f.cfg.from != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(f.cfg.from)}
	}
	return d.DialContext(ctx, "tcp", addr)
}

// dialTLS opens a connection and completes its TLS handshake.
func (f *flood) dialTLS(ctx context.Context, addr string) (net.Conn, error) {
	c, err := f.dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(c, &tls.Config{RootCAs: f.w.web.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return tc, nil
}

// dialFailed says why a connection was not made: the daemon not
// listening (it is restarting), or the listener not answering.
func dialFailed(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused: no daemon listening"
	case errors.Is(err, context.Canceled):
		return "ended with the run"
	case errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	}
	return "failed: " + err.Error()
}

// guard closes c when ctx is done. Its result closes c and ends that.
func guard(ctx context.Context, c net.Conn) func() {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	return func() { stop(); _ = c.Close() }
}

// after names how long an attack was held, coarsely.
func after(d time.Duration) string {
	for _, b := range []time.Duration{time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 20 * time.Second} {
		if d < b {
			return "<" + b.String()
		}
	}
	return ">=20s"
}

// attack runs one attack of kind and says how it ended at the client.
func (f *flood) attack(ctx context.Context, kind string) string {
	addr := f.addr()
	if addr == "" {
		return "no daemon"
	}
	switch kind {
	case "tcp_abandon":
		c, err := f.dial(ctx, addr)
		if err != nil {
			return dialFailed(err)
		}
		_ = c.Close()
		return "closed by the client"
	case "tls_half_hello":
		c, err := f.dial(ctx, addr)
		if err != nil {
			return dialFailed(err)
		}
		defer guard(ctx, c)()
		// The start of a record that says a ClientHello follows, and
		// no more of it.
		_, _ = c.Write([]byte{0x16, 0x03, 0x01, 0x00, 0xf0})
		return "closed by the client"
	case "tls_abandon":
		c, err := f.dialTLS(ctx, addr)
		if err != nil {
			return dialFailed(err)
		}
		_ = c.Close()
		return "closed by the client"
	case "connect_plain":
		return f.request(ctx, addr, http.MethodGet, "/v1/connect", "", nil)
	case "upgrade_silent", "upgrade_junk":
		return f.upgrade(ctx, addr, kind == "upgrade_junk")
	case "oauth_token":
		switch f.seq.Add(1) % 3 {
		case 0:
			return f.request(ctx, addr, http.MethodPost, "/oauth/token", "application/x-www-form-urlencoded",
				strings.NewReader("grant_type=authorization_code&code=junk&code_verifier=junk&client_id=junk"))
		case 1:
			return f.request(ctx, addr, http.MethodPost, "/oauth/token", "application/json", strings.NewReader(`{"grant_type":"junk"}`))
		}
		return f.request(ctx, addr, http.MethodPost, "/oauth/token", "application/x-www-form-urlencoded", bytes.NewReader(bytes.Repeat([]byte("a=b&"), 32<<10)))
	case "oauth_authorize":
		return f.request(ctx, addr, http.MethodGet, "/oauth/authorize?response_type=code&client_id=junk&state=junk&redirect_uri=http%3A%2F%2F127.0.0.1%3A1%2Fcb&code_challenge=junk&code_challenge_method=S256", "", nil)
	case "slow_headers":
		return f.drip(ctx, addr, "GET /.well-known/aries HTTP/1.1\r\nHost: x\r\nX-Slow: ", "x")
	case "slow_body":
		return f.drip(ctx, addr, "POST /oauth/token HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100000\r\n\r\n", "a")
	}
	panic("soak: flood: no attack " + kind) // a programming error: floodKinds names it
}

// request sends one request on a connection of its own and says the
// status it was answered with.
func (f *flood) request(ctx context.Context, addr, method, path, ctype string, body io.Reader) string {
	c, err := f.dialTLS(ctx, addr)
	if err != nil {
		return dialFailed(err)
	}
	defer guard(ctx, c)()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+path, body)
	if err != nil {
		return "no request"
	}
	req.Close = true
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	_ = c.SetDeadline(time.Now().Add(20 * time.Second))
	// A request whose body is past the cap is answered while it is being
	// sent, or the connection is closed under it: the write fails, and
	// the answer, if any, is read all the same.
	werr := make(chan error, 1)
	go func() { werr <- req.Write(c) }()
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	_ = c.Close()
	<-werr
	if err != nil {
		return "closed without an answer"
	}
	defer func() { _ = resp.Body.Close() }()
	return "HTTP " + strconv.Itoa(resp.StatusCode)
}

// upgrade asks for a WebSocket on /v1/connect and then proves nothing:
// it sends junk for the key offer, or nothing, and waits to be closed.
func (f *flood) upgrade(ctx context.Context, addr string, junk bool) string {
	c, err := f.dialTLS(ctx, addr)
	if err != nil {
		return dialFailed(err)
	}
	defer guard(ctx, c)()
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	_ = c.SetDeadline(time.Now().Add(floodHold))
	if _, err := fmt.Fprintf(c, "GET /v1/connect HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		addr, base64.StdEncoding.EncodeToString(key)); err != nil {
		return "closed without an answer"
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return "closed without an answer"
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		return "HTTP " + strconv.Itoa(resp.StatusCode)
	}
	start := time.Now()
	if junk {
		// A masked text frame: the client's frames are masked.
		const text = "this is not a key offer\n" // under 126 bytes: the length is one byte, 0x80 for the mask
		payload := []byte(text)
		mask := []byte{1, 2, 3, 4}
		frame := []byte{0x81, byte(0x80 + len(text))}
		frame = append(frame, mask...)
		for i, b := range payload {
			frame = append(frame, b^mask[i%4])
		}
		if _, err := c.Write(frame); err != nil {
			return "upgraded; closed by the server"
		}
	}
	_, err = io.Copy(io.Discard, br)
	took := time.Since(start)
	if ctx.Err() != nil {
		return "ended with the run"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "upgraded; held until the client left at " + floodHold.String()
	}
	if junk {
		return "upgraded; junk sent; closed by the server after " + after(took)
	}
	return "upgraded; silent; closed by the server after " + after(took)
}

// drip sends the start of a request and then a byte a second, until the
// server closes the connection or floodHold has passed.
func (f *flood) drip(ctx context.Context, addr, start, b string) string {
	c, err := f.dialTLS(ctx, addr)
	if err != nil {
		return dialFailed(err)
	}
	defer guard(ctx, c)()
	// The server may answer and close before the drip is done: a closed
	// connection is found by the write that fails, or by the read.
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c)
		close(closed)
	}()
	began := time.Now()
	if _, err := io.WriteString(c, start); err != nil {
		return "closed by the server after " + after(time.Since(began))
	}
	for time.Since(began) < floodHold {
		select {
		case <-ctx.Done():
			return "ended with the run"
		case <-closed:
			return "closed by the server after " + after(time.Since(began))
		case <-time.After(time.Second):
		}
		if _, err := io.WriteString(c, b); err != nil {
			return "closed by the server after " + after(time.Since(began))
		}
	}
	return "held until the client left at " + floodHold.String()
}

// report adds what the flood sent, how each kind ended, and what the
// daemon counted, to b.
func (f *flood) report(b *bytes.Buffer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	total := 0
	for _, n := range f.sent {
		total += n
	}
	from := "127.0.0.1"
	if f.cfg.from != "" {
		from = f.cfg.from
	}
	fmt.Fprintf(b, "  flood: %d attacks at %d a second from %s, at most %d open at once (%d reached), %d not sent for that limit\n",
		total, f.cfg.rate, from, f.cfg.conns, f.peak.Load(), f.skipped)
	for _, k := range floodKinds {
		if f.sent[k] == 0 {
			continue
		}
		fmt.Fprintf(b, "    %-16s %5d:", k, f.sent[k])
		for _, o := range slices.Sorted(maps.Keys(f.ended[k])) {
			fmt.Fprintf(b, " %s %d;", o, f.ended[k][o])
		}
		b.WriteString("\n")
	}
	if f.manual == nil {
		b.WriteString("    the daemon's counts are not read: the run has telemetry of its own\n")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snap, err := f.manual.Collect(ctx)
	if err != nil {
		fmt.Fprintf(b, "    the daemon's counts: %v\n", err)
		return
	}
	b.WriteString("    the daemon counted (the sessions' own upgrades and key proofs among them):\n")
	for _, p := range snap {
		if !strings.HasPrefix(p.Name, "aries.https.") && p.Name != "aries.limit.refusals" || p.Value == 0 {
			continue
		}
		var attrs []string
		for _, k := range slices.Sorted(maps.Keys(p.Attrs)) {
			attrs = append(attrs, k+"="+p.Attrs[k])
		}
		fmt.Fprintf(b, "      %-38s %-48s %g\n", p.Name, strings.Join(attrs, " "), p.Value)
	}
}

// reportFlood adds the flood's report to b, when the run had one.
func (s *soak) reportFlood(b *bytes.Buffer) {
	if s.flood != nil {
		s.flood.report(b)
	}
}
