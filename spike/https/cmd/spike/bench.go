package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/spike/https/dpop"
)

// cmdBench measures each pipe, directly and through the reverse proxy,
// and the SSH path of today: the time to open a connection and be
// welcomed, a request's round trip, and how long a pushed frame takes to
// arrive.
func cmdBench(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	direct := fs.String("aries", "https://aries.internal:8443", "the server, directly")
	proxy := fs.String("proxy", "https://proxy.internal", "the server, through the reverse proxy")
	sshAddr := fs.String("ssh", "", "an sshd whose forced command echoes, host:port")
	sshKeyFile := fs.String("ssh-key", "", "the key for -ssh")
	ca := fs.String("ca", "", "the internal CA")
	state := fs.String("state", "", "the spike's state directory")
	n := fs.Int("n", 300, "round trips per measure")
	idle := fs.Duration("idle", 0, "hold a WebSocket through the proxy idle this long, then use it")
	_ = fs.Parse(args)
	tc, err := clientTLS(*ca, "")
	if err != nil {
		return err
	}
	st, key, err := loadStored(filepath.Join(*state, "signin"))
	if err != nil {
		return err
	}
	fmt.Printf("bench: %d round trips each; push: 50 frames, one every 20 ms; connect: 20 new connections (TCP, TLS, upgrade, hello)\n", *n)
	fmt.Printf("%-34s %12s %12s %12s %12s %12s\n", "pipe", "connect p50", "rtt p50", "rtt p99", "push p50", "push p99")
	for _, target := range []struct{ name, base string }{{"direct", *direct}, {"via nginx", *proxy}} {
		benchWS(ctx, target.name, target.base, tc, st, key, *n)
		benchStream(ctx, target.name, target.base, tc, st, key, *n)
		benchSSE(ctx, target.name, target.base, tc, st, key, *n, false)
		benchSSE(ctx, target.name, target.base, tc, st, key, *n, true)
	}
	if *idle > 0 {
		benchIdle(ctx, *proxy, tc, st, key, *idle)
	}
	if *sshAddr != "" {
		return benchSSH(ctx, *sshAddr, *sshKeyFile, *n)
	}
	return nil
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

func row(name string, connect, rtt, push []time.Duration, note string) {
	f := func(d time.Duration) string {
		if d == 0 {
			return "-"
		}
		return d.Round(10 * time.Microsecond).String()
	}
	fmt.Printf("%-34s %12s %12s %12s %12s %12s  %s\n", name, f(pct(connect, .5)), f(pct(rtt, .5)), f(pct(rtt, .99)), f(pct(push, .5)), f(pct(push, .99)), note)
}

func freshTLS(tc *tls.Config) *tls.Config {
	c := tc.Clone()
	c.ClientSessionCache = nil // no resumption: each connect is a full handshake
	return c
}

func benchWS(ctx context.Context, where, base string, tc *tls.Config, st *stored, key *ecdsa.PrivateKey, n int) {
	var connect []time.Duration
	for range 20 {
		t := time.Now()
		cn, _, err := dialWS(ctx, base, freshTLS(tc), st, key)
		if err != nil {
			row("websocket "+where, nil, nil, nil, "FAILED: "+err.Error())
			return
		}
		connect = append(connect, time.Since(t))
		_ = cn.c.Close()
	}
	cn, _, err := dialWS(ctx, base, tc, st, key)
	if err != nil {
		row("websocket "+where, connect, nil, nil, "FAILED: "+err.Error())
		return
	}
	defer cn.c.Close()
	var rtt []time.Duration
	for range n {
		t := time.Now()
		if _, err := cn.call("echo", map[string]string{"x": "y"}); err != nil {
			row("websocket "+where, connect, rtt, nil, "FAILED: "+err.Error())
			return
		}
		rtt = append(rtt, time.Since(t))
	}
	if _, err := cn.call("push", pushArgs{N: 50, Every: 20}); err != nil {
		row("websocket "+where, connect, rtt, nil, "FAILED: "+err.Error())
		return
	}
	push := collect(cn.evts, 50)
	row("websocket "+where, connect, rtt, push, "")
}

// collect reads pushed frames until n arrive or 3 s pass with none.
func collect(evts <-chan *proto.Frame, n int) []time.Duration {
	var push []time.Duration
	for len(push) < n {
		select {
		case f, ok := <-evts:
			if !ok {
				return push
			}
			var p pushed
			if json.Unmarshal(f.E, &p) == nil && p.Sent > 0 {
				push = append(push, time.Since(time.Unix(0, p.Sent)))
			}
		case <-time.After(3 * time.Second):
			return push
		}
	}
	return push
}

func authHeader(h http.Header, method, u string, st *stored, key *ecdsa.PrivateKey) {
	p, _ := dpop.Proof(key, method, u, st.AccessToken, time.Now())
	h.Set("Authorization", "DPoP "+st.AccessToken)
	h.Set("DPoP", p)
}

// benchStream is the full-duplex request: frames in the body as they
// are written, frames in the response as they are flushed.
func benchStream(ctx context.Context, where, base string, tc *tls.Config, st *stored, key *ecdsa.PrivateKey, n int) {
	name := "h2 full duplex " + where
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	defer pw.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/stream", pr)
	authHeader(req.Header, http.MethodPost, base+"/v1/stream", st, key)
	req.Header.Set("Content-Type", "application/x-ndjson")
	enc := proto.NewEncoder(pw)
	type answer struct {
		resp *http.Response
		err  error
	}
	got := make(chan answer, 1)
	t := time.Now()
	go func() {
		resp, err := hc.Do(req)
		got <- answer{resp, err}
	}()
	go func() { _ = enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: 8}) }()
	var a answer
	select {
	case a = <-got:
	case <-time.After(5 * time.Second):
		row(name, nil, nil, nil, "FAILED: no response headers in 5 s (the request body was held back)")
		return
	}
	if a.err != nil {
		row(name, nil, nil, nil, "FAILED: "+a.err.Error())
		return
	}
	defer a.resp.Body.Close()
	if a.resp.StatusCode != 200 {
		row(name, nil, nil, nil, "FAILED: "+a.resp.Status)
		return
	}
	frames := make(chan *proto.Frame, 1024)
	go func() {
		dec := proto.NewDecoder(a.resp.Body)
		for {
			f, err := dec.Decode()
			if err != nil {
				close(frames)
				return
			}
			frames <- f
		}
	}()
	next := func() *proto.Frame {
		select {
		case f := <-frames:
			return f
		case <-time.After(5 * time.Second):
			return nil
		}
	}
	if w := next(); w == nil || w.T != proto.FrameWelcome {
		row(name, nil, nil, nil, fmt.Sprintf("FAILED: no welcome in 5 s (proto %s)", a.resp.Proto))
		return
	}
	connect := []time.Duration{time.Since(t)}
	var rtt []time.Duration
	for i := range n {
		t := time.Now()
		_ = enc.Encode(&proto.Frame{T: proto.FrameReq, ID: uint64(i + 1), Op: "echo", Args: json.RawMessage(`{"x":"y"}`)})
		if f := next(); f == nil {
			row(name, connect, rtt, nil, "FAILED: a response did not arrive in 5 s")
			return
		}
		rtt = append(rtt, time.Since(t))
	}
	_ = enc.Encode(&proto.Frame{T: proto.FrameReq, ID: 99999, Op: "push", Args: mustJSON(pushArgs{N: 50, Every: 20})})
	evts := make(chan *proto.Frame, 64)
	go func() {
		for f := range frames {
			if f.T == proto.FrameEvent {
				evts <- f
			}
		}
		close(evts)
	}()
	row(name, connect, rtt, collect(evts, 50), "(connect is one sample, "+a.resp.Proto+")")
}

// benchSSE is a POST per request and server-sent events for pushes.
func benchSSE(ctx context.Context, where, base string, tc *tls.Config, st *stored, key *ecdsa.PrivateKey, n int, nobuf bool) {
	name := "rpc + sse " + where
	q := ""
	if nobuf {
		name, q = "rpc + sse, X-Accel-Buffering: no "+where, "?nobuf=1"
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}, Timeout: 30 * time.Second}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/events"+q, nil)
	authHeader(req.Header, http.MethodGet, base+"/v1/events", st, key)
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true}}).Do(req)
	if err != nil {
		row(name, nil, nil, nil, "FAILED: "+err.Error())
		return
	}
	defer resp.Body.Close()
	evts := make(chan *proto.Frame, 1024)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var f proto.Frame
				if json.Unmarshal([]byte(d), &f) == nil {
					evts <- &f
				}
			}
		}
		close(evts)
	}()
	time.Sleep(200 * time.Millisecond) // let the stream register
	call := func(op string, args any) error {
		b, _ := json.Marshal(&proto.Frame{T: proto.FrameReq, ID: 1, Op: op, Args: mustJSON(args)})
		r, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/rpc", strings.NewReader(string(b)))
		authHeader(r.Header, http.MethodPost, base+"/v1/rpc", st, key)
		resp, err := hc.Do(r)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		var f proto.Frame
		if err := json.NewDecoder(resp.Body).Decode(&f); err != nil {
			return err
		}
		if f.Err != nil {
			return f.Err
		}
		return nil
	}
	var rtt []time.Duration
	for range n {
		t := time.Now()
		if err := call("echo", map[string]string{"x": "y"}); err != nil {
			row(name, nil, rtt, nil, "FAILED: "+err.Error())
			return
		}
		rtt = append(rtt, time.Since(t))
	}
	if err := call("push", pushArgs{N: 50, Every: 20}); err != nil {
		row(name, nil, rtt, nil, "FAILED: "+err.Error())
		return
	}
	push := collect(evts, 50)
	note := ""
	if len(push) < 50 {
		note = fmt.Sprintf("only %d of 50 pushed frames arrived within 3 s of the last", len(push))
	}
	row(name, nil, rtt, push, note+" (each request carries a new DPoP proof)")
}

// benchSSH is today's path: SSH to sshd, a forced command that echoes
// (cat), Aries's frames as lines. It has no push to measure.
func benchSSH(ctx context.Context, addr, keyFile string, n int) error {
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return err
	}
	cfg := &ssh.ClientConfig{User: "aries", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// The spike measures; it pins nothing. aries pins the host key.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second}
	var connect []time.Duration
	var keep *ssh.Client
	var in io.WriteCloser
	var out *bufio.Reader
	for i := range 21 {
		t := time.Now()
		cl, err := ssh.Dial("tcp", addr, cfg)
		if err != nil {
			row("ssh (today's path)", nil, nil, nil, "FAILED: "+err.Error())
			return nil
		}
		s, err := cl.NewSession()
		if err != nil {
			return err
		}
		w, _ := s.StdinPipe()
		r, _ := s.StdoutPipe()
		if err := s.Start("x"); err != nil {
			return err
		}
		br := bufio.NewReader(r)
		hello, _ := json.Marshal(&proto.Frame{T: proto.FrameHello, Proto: 8})
		_, _ = w.Write(append(hello, '\n'))
		if _, err := br.ReadString('\n'); err != nil {
			return err
		}
		if i < 20 {
			connect = append(connect, time.Since(t))
			_ = cl.Close()
			continue
		}
		keep, in, out = cl, w, br
	}
	defer keep.Close()
	var rtt []time.Duration
	line, _ := json.Marshal(&proto.Frame{T: proto.FrameReq, ID: 1, Op: "echo", Args: json.RawMessage(`{"x":"y"}`)})
	line = append(line, '\n')
	for range n {
		t := time.Now()
		if _, err := in.Write(line); err != nil {
			return err
		}
		if _, err := out.ReadString('\n'); err != nil {
			return err
		}
		rtt = append(rtt, time.Since(t))
	}
	row("ssh (today's path, echo by cat)", connect, rtt, nil, "")
	return nil
}

// sshKey writes a key for the SSH comparison, and the keys file line
// that forces cat.
func sshKey(dir string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh.key"), pemEncode(pemBlock.Type, pemBlock.Bytes), 0o600); err != nil {
		return err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	line := `restrict,command="/bin/cat" ` + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))) + "\n"
	return os.WriteFile(filepath.Join(dir, "aries_keys"), []byte(line), 0o644)
}

func pemEncode(typ string, b []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b})
}

// benchIdle holds a WebSocket through the proxy with nothing to say for
// longer than nginx's proxy_read_timeout (60 s), then uses it: the
// daemon's pings are what keep it open.
func benchIdle(ctx context.Context, proxy string, tc *tls.Config, st *stored, key *ecdsa.PrivateKey, d time.Duration) {
	cn, _, err := dialWS(ctx, proxy, tc, st, key)
	if err != nil {
		fmt.Printf("idle via nginx: FAILED: %v\n", err)
		return
	}
	defer cn.c.Close()
	time.Sleep(d)
	t := time.Now()
	_, err = cn.call("echo", map[string]string{"x": "y"})
	fmt.Printf("idle via nginx: a WebSocket idle for %s, then a request: err %v, %s\n", d, err, time.Since(t).Round(10*time.Microsecond))
}
