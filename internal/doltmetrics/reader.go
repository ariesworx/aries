// Package doltmetrics reads the few figures starfixd shows of Dolt from
// Dolt's own Prometheus endpoint (design §16.5): its version, memory,
// start time, connections, memory limit and count of garbage collection
// cycles.
//
// The endpoint is another process's, so nothing it sends is trusted. A
// [Reader] asks one URL, which must be http:// on a loopback address; it
// connects to loopback addresses only, follows no redirect, uses no
// proxy, and gives up after [Timeout]. [Parse] reads at most [MaxBody]
// bytes, with a parser of its own that knows six series by name and
// passes over the rest. The package imports the standard library only:
// the Prometheus client that internal/telemetry links is for writing
// starfixd's metrics, and stays there.
package doltmetrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"syscall"
	"time"
)

// Timeout bounds one read: the connection, the request and the body.
const Timeout = 5 * time.Second

// Example is a URL the refusals suggest: Dolt's endpoint as
// docs/server.md sets it up.
const Example = "http://127.0.0.1:11228/metrics"

// fix ends every refusal of a URL.
const fix = "; fix: set dolt_metrics_url: under telemetry: to the http:// URL of Dolt's metrics endpoint on a loopback address, such as " +
	Example + ", or remove it to show nothing of Dolt (docs/server.md, Observability)"

// CheckURL refuses a URL a [Reader] would not read: one that is not
// http:// with a host, a port from 1 to 65535 and a path, that holds a
// user, a query or a fragment, or whose host is an IP address that is
// not loopback. It resolves nothing: a host that is a name is checked
// by [New], when the daemon starts. The endpoint has no authentication,
// so one on another machine would be read in the clear from whoever
// answered; and the URL is logged, so it has no place for a secret. A
// refusal names the URL's host and never the rest of it.
func CheckURL(raw string) error {
	_, err := parseURL(raw)
	return err
}

// parseURL is CheckURL, returning the URL.
func parseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Opaque != "" {
		return nil, errors.New("telemetry dolt_metrics_url is not an http:// URL" + fix)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("telemetry dolt_metrics_url holds a user, a query or a fragment, and Dolt's endpoint takes none" + fix)
	}
	host := u.Hostname()
	if n, err := strconv.Atoi(u.Port()); err != nil || n < 1 || n > 65535 || host == "" {
		return nil, fmt.Errorf("telemetry dolt_metrics_url, to %q, names no host and port from 1 to 65535%s", u.Host, fix)
	}
	if u.Path == "" || u.Path == "/" {
		return nil, fmt.Errorf("telemetry dolt_metrics_url, to %q, has no path; Dolt serves its metrics at /metrics%s", u.Host, fix)
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsLoopback() {
		return nil, notLoopback(u.Host, ip.String())
	}
	return u, nil
}

// notLoopback refuses a URL to host, which is or resolves to ip.
func notLoopback(host, ip string) error {
	return fmt.Errorf("telemetry dolt_metrics_url, to %q, is %s, which is not a loopback address, and Dolt's endpoint has no authentication%s", host, ip, fix)
}

// Reader reads Dolt's metrics from one URL. Make it with [New], and
// Close it when done.
type Reader struct {
	url     string
	client  *http.Client
	timeout time.Duration
}

// New returns a Reader of the endpoint at rawURL, which must pass
// [CheckURL]. A host that is a name is resolved, and refused unless
// every address it has is loopback.
func New(ctx context.Context, rawURL string) (*Reader, error) {
	return open(ctx, rawURL, net.DefaultResolver.LookupIPAddr)
}

// open is New with the lookup of names given, for tests.
func open(ctx context.Context, rawURL string, lookup func(context.Context, string) ([]net.IPAddr, error)) (*Reader, error) {
	u, err := parseURL(rawURL)
	if err != nil {
		return nil, err
	}
	if host := u.Hostname(); net.ParseIP(host) == nil {
		ips, err := lookup(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("telemetry dolt_metrics_url, to %q: the host could not be resolved%s", u.Host, fix)
		}
		for _, ip := range ips {
			if !ip.IP.IsLoopback() {
				return nil, notLoopback(u.Host, ip.IP.String())
			}
		}
	}
	// A transport of the reader's own: the default one would use a proxy
	// from the environment, keep connections open between reads and ask
	// for a compressed body, which could expand past any cap on what is
	// read.
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: Timeout, Control: loopbackOnly}).DialContext,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 16 << 10,
		ResponseHeaderTimeout:  Timeout,
	}
	client := &http.Client{Transport: tr,
		// A redirect is an answer that is not 200, not somewhere to go.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Reader{url: u.String(), client: client, timeout: Timeout}, nil
}

// loopbackOnly is a net.Dialer's Control: it refuses a connection to
// any address that is not loopback. It sees the address being
// connected to, after any name was resolved, so a name that pointed at
// loopback when the daemon started cannot lead anywhere else later.
func loopbackOnly(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !ap.Addr().Unmap().IsLoopback() {
		return fmt.Errorf("%s is not a loopback address", address)
	}
	return nil
}

// URL is the URL the reader asks.
func (r *Reader) URL() string { return r.url }

// Read asks the endpoint once, and returns what [Parse] makes of a 200
// answer's body. It follows no redirect and takes at most [Timeout],
// or as long as ctx allows if that is less; any other status, a body
// past [MaxBody] and one with none of Dolt's series are errors.
func (r *Reader) Read(ctx context.Context) (Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, http.NoBody)
	if err != nil {
		return Sample{}, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return Sample{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Sample{}, fmt.Errorf("GET %s answered %d, not 200", r.url, resp.StatusCode)
	}
	s, err := Parse(resp.Body)
	if err != nil {
		return Sample{}, fmt.Errorf("GET %s: %w", r.url, err)
	}
	return s, nil
}

// Close releases what the reader holds. A read under way ends with its
// context.
func (r *Reader) Close() { r.client.CloseIdleConnections() }
