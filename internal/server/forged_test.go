package server

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/wsconn"
)

// What the listener trusts of where a request came from (transport.md
// §12.4, item 7): nothing. A request's Host and its X-Forwarded-* (and
// Forwarded and X-Real-IP, which proxies also send) are the client's to
// say, directly or through a proxy that passes them on, so none of them
// may change a key proof, a redirect, a limit per address or a field of
// the log. These tests send each forged, in both modes: with TLS at
// ariesd, and behind a proxy that terminates it.

// forgedHost is the host a forger names in Host and X-Forwarded-Host.
const forgedHost = "aries.attacker.example"

// forgedAddr is the address a forger claims in X-Forwarded-For.
const forgedAddr = "203.0.113.7"

// forgery is a set of headers a client forges.
type forgery struct {
	name   string
	host   string // the Host header; "" leaves the real one
	header http.Header
}

// forgeries are the headers a client can forge, one at a time and all
// together.
var forgeries = []forgery{
	{name: "Host", host: forgedHost},
	{name: "X-Forwarded-For", header: http.Header{"X-Forwarded-For": {forgedAddr}}},
	{name: "X-Forwarded-Proto", header: http.Header{"X-Forwarded-Proto": {"http"}}},
	{name: "X-Forwarded-Host", header: http.Header{"X-Forwarded-Host": {forgedHost}}},
	{name: "Forwarded", header: http.Header{"Forwarded": {"for=" + forgedAddr + ";host=" + forgedHost + ";proto=http"}}},
	{name: "X-Real-IP", header: http.Header{"X-Real-Ip": {forgedAddr}}},
	{name: "all of them", host: forgedHost, header: http.Header{
		"X-Forwarded-For": {forgedAddr}, "X-Forwarded-Proto": {"http"}, "X-Forwarded-Host": {forgedHost},
		"Forwarded": {"for=" + forgedAddr + ";host=" + forgedHost + ";proto=http"}, "X-Real-Ip": {forgedAddr},
	}},
}

// apply puts f's headers on r.
func (f forgery) apply(r *http.Request) {
	if f.host != "" {
		r.Host = f.host
	}
	for k, vs := range f.header {
		r.Header[k] = vs
	}
}

// forger is a relay on loopback that forges f's headers on the first
// request of each connection, as a client that writes its own request
// can, and then relays bytes both ways: to target over TLS with tc, or
// in plain when tc is nil. wsconn.Dial cannot set Host itself (net/http
// writes the URL's), so a WebSocket with a forged Host goes through
// this. Over TLS, it sends the exporter of each session it opens on
// exporters, which a proof made through it is bound to: the forger
// stands for a client that writes its own request, whose TLS session
// with ariesd it is. It returns the relay's address.
func forger(t *testing.T, target string, tc *tls.Config, f forgery, exporters chan<- []byte) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { relayForged(c, target, tc, f, exporters) })
		}
	})
	return l.Addr().String()
}

func relayForged(c net.Conn, target string, tc *tls.Config, f forgery, exporters chan<- []byte) {
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)
	r, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	var up net.Conn
	if up, err = net.Dial("tcp", target); err != nil {
		return
	}
	defer func() { _ = up.Close() }()
	if tc != nil {
		t := tls.Client(up, tc)
		if t.Handshake() != nil {
			return
		}
		cs := t.ConnectionState()
		exporter, err := keyproof.Exporter(&cs)
		if err != nil {
			return
		}
		exporters <- exporter
		up = t
	}
	f.apply(r)
	if r.Write(up) != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	<-done
}

// forgedListener is the server of one project with alice's key, served
// in mode ("direct", with TLS at ariesd, or "proxy", plain HTTP to
// 127.0.0.1 alone), logging to log; and how to reach it.
type forgedListener struct {
	s      *Server
	public string                                          // the URL clients have, and a proof must name
	addr   string                                          // the listener's address
	tc     *tls.Config                                     // TLS to the listener; nil behind a proxy
	pool   *x509.CertPool                                  // trusts the listener's certificate, in direct mode
	log    *syncBuffer                                     // the server's log
	via    func(f forgery, exporters chan<- []byte) string // a forger's ws:// URL to the listener
	// bound is the exporter of the TLS session the forger of each
	// connection opened to the listener, in direct mode.
	mu    sync.Mutex
	bound map[*wsconn.Conn][]byte
}

func newForgedListener(t *testing.T, mode string, lim Limits) *forgedListener {
	t.Helper()
	fl := &forgedListener{log: &syncBuffer{}, bound: map[*wsconn.Conn][]byte{}}
	cfg := Config{Limits: lim, AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice")), Logger: slogTo(fl.log)}
	u, pool := listenHTTPS(t, &cfg, func(h *HTTPS) {
		if mode == "proxy" {
			h.TLS, h.Trusted, h.URLs = nil, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}, []string{"https://aries.example.com"}
		}
	})
	fl.public, fl.addr, fl.pool = cfg.HTTPS.URLs[0], cfg.HTTPS.Listener.Addr().String(), pool
	if mode == "direct" {
		fl.public = u
		fl.tc = &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
	}
	fl.s, _ = newServerConfig(t, cfg, nil)
	serving(t, fl.s)
	fl.via = func(f forgery, exporters chan<- []byte) string {
		return "ws://" + forger(t, fl.addr, fl.tc, f, exporters) + keyproof.ConnectPath
	}
	return fl
}

// prove is prove through a forger: bound, in direct mode, to the TLS
// session the forger opened for c.
func (fl *forgedListener) prove(t *testing.T, c *wsconn.Conn, offered string, signer ssh.Signer) *proto.Frame {
	t.Helper()
	fl.mu.Lock()
	exporter := fl.bound[c]
	fl.mu.Unlock()
	return proveAs(t, c, offered, signer, nil, 2, exporter)
}

// dial opens /v1/connect through a forger of f.
func (fl *forgedListener) dial(t *testing.T, f forgery) (*wsconn.Conn, *http.Response, error) {
	t.Helper()
	exporters := make(chan []byte, 1)
	c, resp, err := wsconn.Dial(t.Context(), fl.via(f, exporters), nil, nil, 0)
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
		if fl.tc != nil {
			// The forger opened its TLS session before it sent the
			// upgrade, which Dial has the answer to.
			fl.mu.Lock()
			fl.bound[c] = <-exporters
			fl.mu.Unlock()
		}
	}
	return c, resp, err
}

// A forged Host or X-Forwarded-* changes no key proof: a proof for the
// server's public URL is welcomed whatever the request says, and one for
// the host the forger names is refused, with a fix that names the
// public URL, never the forged one.
func TestForgedHeadersKeyProof(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"direct", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fl := newForgedListener(t, mode, Limits{})
			// In turn: the test holds a Postgres server (CONTRIBUTING.md).
			for _, f := range forgeries {
				t.Run(f.name, func(t *testing.T) {
					c, _, err := fl.dial(t, f)
					if err != nil {
						t.Fatalf("dial with a forged %s: %v", f.name, err)
					}
					if r := fl.prove(t, c, fl.public, signerOf(t, "alice")); r != nil {
						t.Fatalf("alice's proof for %s, with a forged %s: refused %v", fl.public, f.name, r.Err)
					}
					if w := hello(t, c); w.Err != nil || w.Principal != "alice" {
						t.Fatalf("alice with a forged %s: %+v, %v", f.name, w, w.Err)
					}
					for _, named := range []string{"https://" + forgedHost, "http://" + forgedHost, "http://" + strings.TrimPrefix(fl.public, "https://")} {
						c, _, err := fl.dial(t, f)
						if err != nil {
							t.Fatal(err)
						}
						// An http:// URL is refused before it is compared, as
						// no key offer at all.
						want := proto.CodeInvalid
						if strings.HasPrefix(named, "https://") {
							want = proto.CodeAuth
						}
						r := fl.prove(t, c, named, signerOf(t, "alice"))
						if r == nil || r.Err.Code != want || strings.Contains(r.Err.Fix, forgedHost) ||
							want == proto.CodeAuth && !strings.Contains(r.Err.Fix, fl.public) {
							t.Errorf("a proof for %s with a forged %s: %+v; want refused %s, with a fix that names %s and not the forged host", named, f.name, r, want, fl.public)
						}
					}
				})
			}
		})
	}
}

// A forged Host or X-Forwarded-* changes no field of the log: a refusal
// is logged with the peer's address (the proxy's, behind one), and
// nothing the forger wrote reaches a line.
func TestForgedHeadersLog(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"direct", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fl := newForgedListener(t, mode, Limits{})
			all := forgeries[len(forgeries)-1]
			// A key the server does not know, a proof for another server
			// and a browser's Origin, each refused and logged.
			c, _, err := fl.dial(t, all)
			if err != nil {
				t.Fatal(err)
			}
			if r := fl.prove(t, c, fl.public, signerOf(t, "mallory")); r == nil {
				t.Fatal("mallory's key was not refused")
			}
			if c, _, err = fl.dial(t, all); err != nil {
				t.Fatal(err)
			}
			if r := fl.prove(t, c, "https://"+forgedHost, signerOf(t, "alice")); r == nil {
				t.Fatal("a proof for the forged host was not refused")
			}
			origin := forgery{name: "Origin", host: forgedHost, header: all.header.Clone()}
			origin.header.Set("Origin", "https://"+forgedHost)
			if _, resp, err := fl.dial(t, origin); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("an upgrade with an Origin = %v, %v; want 403", resp, err)
			}
			log := fl.log.String()
			if !strings.Contains(log, "reason=") {
				t.Fatalf("no refusal was logged:\n%s", log)
			}
			for _, forged := range []string{forgedHost, forgedAddr, "proto=http"} {
				if strings.Contains(log, forged) {
					t.Errorf("the log holds %q, which the request forged:\n%s", forged, log)
				}
			}
			for line := range strings.Lines(log) {
				if i := strings.Index(line, " addr="); i >= 0 && strings.Contains(line, "refused") &&
					!strings.HasPrefix(line[i:], " addr=127.0.0.1") {
					t.Errorf("a refusal logged an address other than the peer's 127.0.0.1: %s", line)
				}
			}
		})
	}
}

// Behind a proxy every peer is the proxy, so refusal lines are sampled by
// address and reason: a flood of one kind of refusal (an unknown key)
// uses up its own allowance and never hides another kind (a proof for
// another URL).
func TestRefusalLogsByReason(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"direct", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fl := newForgedListener(t, mode, Limits{RefusalLogs: 1})
			all := forgeries[len(forgeries)-1]
			for range 5 {
				c, _, err := fl.dial(t, all)
				if err != nil {
					t.Fatal(err)
				}
				if r := fl.prove(t, c, fl.public, signerOf(t, "mallory")); r == nil {
					t.Fatal("mallory's key was not refused")
				}
			}
			c, _, err := fl.dial(t, all)
			if err != nil {
				t.Fatal(err)
			}
			if r := fl.prove(t, c, "https://"+forgedHost, signerOf(t, "alice")); r == nil {
				t.Fatal("a proof for another URL was not refused")
			}
			log := fl.log.String()
			if n := strings.Count(log, "reason=unknown_key"); n != 1 {
				t.Errorf("%d unknown_key lines logged, want 1:\n%s", n, log)
			}
			if !strings.Contains(log, "reason=url") {
				t.Errorf("the url refusal was hidden by the flood of unknown_key:\n%s", log)
			}
		})
	}
}

// A forged X-Forwarded-For (or Forwarded, or X-Real-IP) moves no limit
// per address. With TLS at ariesd, the address charged is the peer's,
// so a client that claims a new address on each try is held to its own
// share all the same; behind a proxy, the limits per address do not
// apply (every connection is the proxy's), so a client cannot lock out
// an address by claiming it.
func TestForgedHeadersLimits(t *testing.T) {
	t.Parallel()
	claims := func(i int) forgery {
		a := fmt.Sprintf("198.51.100.%d", i+1)
		return forgery{name: "address " + a, header: http.Header{"X-Forwarded-For": {a}, "Forwarded": {"for=" + a}, "X-Real-Ip": {a}}}
	}
	t.Run("direct refusals", func(t *testing.T) {
		t.Parallel()
		fl := newForgedListener(t, "direct", Limits{AuthFailureRate: 1, AuthFailureBurst: 2})
		for i := range 2 {
			c, _, err := fl.dial(t, claims(i))
			if err != nil {
				t.Fatal(err)
			}
			if r := fl.prove(t, c, fl.public, signerOf(t, "mallory")); r == nil {
				t.Fatal("mallory's key was not refused")
			}
		}
		_, resp, err := fl.dial(t, claims(2))
		if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("an upgrade claiming a third address after 2 refused proofs, at a burst of 2 = %v, %v; want 429", resp, err)
		}
	})
	t.Run("direct connections", func(t *testing.T) {
		t.Parallel()
		fl := newForgedListener(t, "direct", Limits{HTTPSConnsPerAddr: 2})
		for i := range 2 {
			if _, _, err := fl.dial(t, claims(i)); err != nil {
				t.Fatalf("connection %d of 2: %v", i+1, err)
			}
		}
		if c, _, err := fl.dial(t, claims(2)); err == nil {
			_ = c.Close()
			t.Error("a third connection from one address, claiming another, was served past https_conns_per_addr of 2")
		}
	})
	t.Run("proxy", func(t *testing.T) {
		t.Parallel()
		fl := newForgedListener(t, "proxy", Limits{AuthFailureRate: 1, AuthFailureBurst: 1, HTTPSConnsPerAddr: 1})
		victim := claims(0)
		var held []*wsconn.Conn
		for range 3 {
			c, _, err := fl.dial(t, victim)
			if err != nil {
				t.Fatalf("behind a proxy, a connection was refused by a limit per address: %v", err)
			}
			if r := fl.prove(t, c, fl.public, signerOf(t, "mallory")); r == nil {
				t.Fatal("mallory's key was not refused")
			}
			held = append(held, c)
		}
		// Refused three times as the victim's address, past a burst of
		// one, the victim's next connection is still served.
		c, _, err := fl.dial(t, victim)
		if err != nil {
			t.Fatalf("the claimed address after 3 refusals: %v; want served", err)
		}
		if r := fl.prove(t, c, fl.public, signerOf(t, "alice")); r != nil {
			t.Fatalf("alice's proof, claiming the address mallory claimed: refused %v", r.Err)
		}
		if w := hello(t, c); w.Err != nil || w.Principal != "alice" {
			t.Errorf("alice after mallory's refusals: %+v, %v", w, w.Err)
		}
		for _, c := range held {
			_ = c.Close()
		}
	})
}

// A forged Host or X-Forwarded-* changes no redirect and no URL the
// server gives out: the authorization server's metadata, the redirect
// to the provider with its redirect_uri, the redirect back to aries
// with its iss, and a redirect of net/http's own to a cleaned path are
// all built from public_urls, in both modes.
func TestForgedHeadersRedirects(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"direct", "proxy"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			var addr string
			w := newSignInWorldWith(t, Limits{}, nil, nil, func(h *HTTPS) {
				addr = h.Listener.Addr().String()
				if mode == "proxy" {
					h.TLS, h.Trusted = nil, []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
				}
			})
			base := w.url
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: w.pool, MinVersion: tls.VersionTLS12}}
			if mode == "proxy" {
				base = "http://" + addr
			}
			t.Cleanup(tr.CloseIdleConnections)
			client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			get := func(t *testing.T, f forgery, u string) (*http.Response, string) {
				t.Helper()
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
				if err != nil {
					t.Fatal(err)
				}
				f.apply(req)
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("GET %s: %v", u, err)
				}
				defer func() { _ = resp.Body.Close() }()
				b, _ := io.ReadAll(resp.Body)
				return resp, string(b)
			}
			at := func(u string) string { return base + strings.TrimPrefix(u, w.url) }
			// In turn: the test holds a Postgres server (CONTRIBUTING.md).
			for _, f := range forgeries {
				t.Run(f.name, func(t *testing.T) {
					resp, body := get(t, f, base+pathASMeta)
					if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"issuer":"`+w.url+`"`) || strings.Contains(body, forgedHost) ||
						strings.Contains(body, `"http://`) {
						t.Errorf("the metadata with a forged %s = %d %s; want every URL %s's", f.name, resp.StatusCode, body, w.url)
					}
					// To the provider, with the server's own callback.
					resp, _ = get(t, f, at(w.authorizeURL(newAttempt(t))))
					to, err := url.Parse(resp.Header.Get("Location"))
					if resp.StatusCode != http.StatusFound || err != nil || !strings.HasPrefix(to.String(), w.iss.URL) ||
						to.Query().Get("redirect_uri") != w.url+pathCallback {
						t.Errorf("authorize with a forged %s = %d to %q; want the provider, with redirect_uri %s", f.name, resp.StatusCode, resp.Header.Get("Location"), w.url+pathCallback)
					}
					// Back to aries, refused, with the server's own iss,
					// even when the request names the forged host as its
					// resource.
					bad := strings.Replace(at(w.authorizeURL(newAttempt(t))), "response_type=code", "response_type=token", 1) +
						"&resource=" + url.QueryEscape("https://"+forgedHost)
					resp, _ = get(t, f, bad)
					back, err := url.Parse(resp.Header.Get("Location"))
					if resp.StatusCode != http.StatusFound || err != nil || !strings.HasPrefix(back.String(), loopbackRedirect) ||
						back.Query().Get("iss") != w.url {
						t.Errorf("a refused authorize with a forged %s = %d to %q; want the loopback redirect with iss %s", f.name, resp.StatusCode, resp.Header.Get("Location"), w.url)
					}
					// net/http's own redirect to a cleaned path names only
					// the path.
					resp, _ = get(t, f, base+"/v1/../.well-known/aries")
					if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && (strings.Contains(loc, forgedHost) || strings.Contains(loc, "//")) {
						t.Errorf("a path cleaned with a forged %s is redirected to %q; want a path alone", f.name, loc)
					}
					for k, vs := range resp.Header {
						for _, v := range vs {
							if strings.Contains(v, forgedHost) {
								t.Errorf("a response to a forged %s says %s: %s", f.name, k, v)
							}
						}
					}
				})
			}
		})
	}
}

// Behind a proxy, ariesd serves only the addresses trusted names:
// another is closed unanswered, on every path, whatever it says it is.
func TestProxyServesOnlyTrusted(t *testing.T) {
	t.Parallel()
	all := forgeries[len(forgeries)-1]
	claimsTrusted := forgery{name: "claims a trusted address", header: http.Header{
		"X-Forwarded-For": {"127.0.0.2"}, "Forwarded": {"for=127.0.0.2"}, "X-Real-Ip": {"127.0.0.2"}}}
	for _, tc := range []struct {
		name    string
		trusted []string
		f       forgery
		ok      bool
	}{
		{name: "the proxy", trusted: []string{"127.0.0.1/32"}, ok: true},
		{name: "the proxy, among others", trusted: []string{"10.0.0.0/8", "127.0.0.1"}, ok: true},
		{name: "the proxy, forging all", trusted: []string{"127.0.0.1/32"}, f: all, ok: true},
		{name: "another network", trusted: []string{"10.0.0.0/8"}},
		{name: "a neighbor on loopback", trusted: []string{"127.0.0.2/32"}},
		{name: "a neighbor that claims the trusted address", trusted: []string{"127.0.0.2/32"}, f: claimsTrusted},
		{name: "IPv6 loopback, when the peer is IPv4's", trusted: []string{"::1/128"}},
		{name: "another network, forging all", trusted: []string{"10.0.0.0/8"}, f: all},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var prefixes []netip.Prefix
			for _, p := range tc.trusted {
				h := HTTPSSettings{TLS: TLSSettings{Proxy: ProxySettings{Trusted: []string{p}}}}
				ps, err := h.Trusted()
				if err != nil {
					t.Fatal(err)
				}
				prefixes = append(prefixes, ps...)
			}
			cfg := Config{AuthorizedKeys: handFile(t, bridgeLine(t, "alice", "alice"))}
			listenHTTPS(t, &cfg, func(h *HTTPS) { h.TLS, h.Trusted, h.URLs = nil, prefixes, []string{"https://aries.example.com"} })
			addr := cfg.HTTPS.Listener.Addr().String()
			s, _ := newServerConfig(t, cfg, nil)
			serving(t, s)
			// A plain request and an upgrade, through a forger of tc.f.
			client := &http.Client{Transport: &http.Transport{}}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/.well-known/aries", nil)
			tc.f.apply(req)
			resp, err := client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			if served := err == nil; served != tc.ok {
				t.Errorf("GET /.well-known/aries from 127.0.0.1, trusting %v: served %v (%v), want %v", tc.trusted, served, err, tc.ok)
			}
			c, _, err := wsconn.Dial(t.Context(), "ws://"+forger(t, addr, nil, tc.f, nil)+keyproof.ConnectPath, nil, nil, 0)
			if err == nil {
				defer func() { _ = c.Close() }()
			}
			if served := err == nil; served != tc.ok {
				t.Fatalf("an upgrade from 127.0.0.1, trusting %v: served %v (%v), want %v", tc.trusted, served, err, tc.ok)
			}
			if !tc.ok {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					t.Errorf("an untrusted connection was held open, not closed: %v", err)
				}
				return
			}
			if r := prove(t, c, "https://aries.example.com", signerOf(t, "alice"), nil); r != nil {
				t.Fatalf("alice through the proxy: %v", r.Err)
			}
			if w := hello(t, c); w.Err != nil || w.Principal != "alice" {
				t.Errorf("alice through the proxy: %+v", w)
			}
		})
	}
}
