package doltmetrics

import (
	"compress/gzip"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// secret stands for a credential someone put in the URL.
const secret = "hunter2"

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct {
		name, url, err string
	}{
		{name: "IPv4 loopback", url: "http://127.0.0.1:11228/metrics"},
		{name: "IPv6 loopback", url: "http://[::1]:11228/metrics"},
		{name: "another loopback address", url: "http://127.0.0.2:9000/dolt/metrics"},
		{name: "a name, resolved when the daemon starts", url: "http://localhost:11228/metrics"},
		{name: "https", url: "https://127.0.0.1:11228/metrics", err: "not an http:// URL"},
		{name: "no scheme", url: "127.0.0.1:11228/metrics", err: "not an http:// URL"},
		{name: "a file", url: "file:///etc/passwd", err: "not an http:// URL"},
		{name: "not a URL", url: "http://127.0.0.1:11228/%zz", err: "not an http:// URL"},
		{name: "no host", url: "http://:11228/metrics", err: "names no host and port"},
		{name: "no port", url: "http://127.0.0.1/metrics", err: "names no host and port"},
		{name: "port zero", url: "http://127.0.0.1:0/metrics", err: "names no host and port"},
		{name: "port out of range", url: "http://127.0.0.1:70000/metrics", err: "names no host and port"},
		{name: "no path", url: "http://127.0.0.1:11228", err: "has no path"},
		{name: "only the root", url: "http://127.0.0.1:11228/", err: "has no path"},
		{name: "a routable address", url: "http://192.0.2.10:11228/metrics", err: "not a loopback address"},
		{name: "every interface", url: "http://0.0.0.0:11228/metrics", err: "not a loopback address"},
		{name: "every IPv6 interface", url: "http://[::]:11228/metrics", err: "not a loopback address"},
		{name: "a user", url: "http://alice@127.0.0.1:11228/metrics", err: "holds a user, a query or a fragment"},
		{name: "a password", url: "http://alice:" + secret + "@127.0.0.1:11228/metrics", err: "holds a user, a query or a fragment"},
		{name: "a query", url: "http://127.0.0.1:11228/metrics?token=" + secret, err: "holds a user, a query or a fragment"},
		{name: "a fragment", url: "http://127.0.0.1:11228/metrics#x", err: "holds a user, a query or a fragment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckURL(tc.url)
			switch {
			case tc.err == "" && err != nil:
				t.Errorf("CheckURL(%q) = %v, want nil", tc.url, err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "; fix: ")):
				t.Errorf("CheckURL(%q) = %v, want an error containing %q and a fix", tc.url, err, tc.err)
			}
			// A refusal never repeats what may be a secret.
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("CheckURL(%q) = %v, which repeats the password", tc.url, err)
			}
		})
	}
}

// A host that is a name is resolved when the reader is made, and
// refused unless every address it has is loopback.
func TestNewResolvesNames(t *testing.T) {
	ips := func(addrs ...string) func(context.Context, string) ([]net.IPAddr, error) {
		return func(context.Context, string) ([]net.IPAddr, error) {
			var out []net.IPAddr
			for _, a := range addrs {
				out = append(out, net.IPAddr{IP: net.ParseIP(a)})
			}
			return out, nil
		}
	}
	for _, tc := range []struct {
		name   string
		lookup func(context.Context, string) ([]net.IPAddr, error)
		err    string
	}{
		{name: "loopback", lookup: ips("127.0.0.1", "::1")},
		{name: "one address is routable", lookup: ips("127.0.0.1", "192.0.2.10"), err: "192.0.2.10, which is not a loopback address"},
		{name: "no address", lookup: ips(), err: "could not be resolved"},
		{name: "the lookup fails", lookup: func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("no such host") },
			err: "could not be resolved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := open(t.Context(), "http://dolt.example.com:11228/metrics", tc.lookup)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("open = %v, want a reader", err)
				}
				r.Close()
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "; fix: ") {
				t.Errorf("open = %v, want an error containing %q and a fix", err, tc.err)
			}
		})
	}
	// An address is not looked up.
	r, err := open(t.Context(), "http://127.0.0.1:11228/metrics", func(context.Context, string) ([]net.IPAddr, error) {
		t.Error("an IP address was looked up")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

// Whatever a name resolves to later, the reader connects to loopback
// addresses only: the check is made on each connection's own address.
func TestConnectsToLoopbackOnly(t *testing.T) {
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:11228", true}, {"[::1]:11228", true}, {"127.9.9.9:1", true},
		{"192.0.2.10:11228", false}, {"[2001:db8::1]:11228", false}, {"0.0.0.0:11228", false}, {"[::]:11228", false},
		{"169.254.169.254:80", false}, {"not an address", false},
	} {
		err := loopbackOnly("tcp", tc.addr, nil)
		if (err == nil) != tc.ok {
			t.Errorf("loopbackOnly(%q) = %v, want ok %t", tc.addr, err, tc.ok)
		}
	}
}

const good = "dss_dolt_version 3.3816578e+07\ndss_concurrent_connections 4\nprocess_resident_memory_bytes 5.9670528e+07\n" +
	"process_start_time_seconds 1.791835308e+09\ngo_gc_gomemlimit_bytes 9.223372036854776e+18\ngo_gc_duration_seconds_count 3\n"

// noLeak fails the test if, once its cleanups have run, more goroutines
// are running than when it was called.
func noLeak(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Errorf("%d goroutines running, %d before the test:\n%s", runtime.NumGoroutine(), before, buf[:runtime.Stack(buf, true)])
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// serve starts an HTTP server on loopback that answers with h, and
// returns the URL of its /metrics.
func serve(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL + "/metrics"
}

// The endpoint is another process's: slow, silent, endless, wrong or
// pointing elsewhere, a read of it ends within its deadline with an
// error and no figure, and leaves nothing running.
func TestRead(t *testing.T) {
	// timeout is the deadline of a read that must run out of it, which
	// the test waits out. A read that is answered has patient instead:
	// under timeout, a machine that took 300 ms over one request on
	// loopback would end it with the deadline, not the answer the case is
	// about.
	const timeout, patient = 300 * time.Millisecond, time.Minute
	// wait holds a handler until its client has gone.
	wait := func(r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	var elsewhere atomic.Int64
	target := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(good))
	})
	// A listener whose connections are accepted by the kernel and never
	// read.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = silent.Close() })
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + closed.Addr().String() + "/metrics"
	_ = closed.Close()

	for _, tc := range []struct {
		name string
		url  string
		h    http.HandlerFunc
		// want is the error's text, or empty for a sample; is, when set,
		// is the error it must wrap.
		want string
		is   error
		conn int
	}{
		{name: "Dolt's own answer", h: func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/metrics" {
				http.Error(w, "no", http.StatusTeapot)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8; escaping=underscores")
			_, _ = w.Write([]byte(good))
		}, conn: 4},
		{name: "a missing series", h: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("dss_concurrent_connections 7\n"))
		}, conn: 7},
		{name: "nothing listens", url: closedURL, want: "connection refused"},
		{name: "never answers", url: "http://" + silent.Addr().String() + "/metrics", want: "deadline exceeded", is: context.DeadlineExceeded},
		{name: "slow to answer", h: func(_ http.ResponseWriter, r *http.Request) { wait(r) }, want: "deadline exceeded", is: context.DeadlineExceeded},
		{name: "slow to finish", h: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(good))
			w.(http.Flusher).Flush()
			wait(r)
		}, want: "deadline exceeded", is: context.DeadlineExceeded},
		{name: "a body without end", h: func(w http.ResponseWriter, r *http.Request) {
			line := []byte(strings.Repeat("# padding\n", 1000))
			for r.Context().Err() == nil {
				if _, err := w.Write(line); err != nil {
					return
				}
			}
		}, want: "larger than 1 MiB", is: ErrTooLarge},
		{name: "garbage", h: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>\x00\xff not metrics</html>"))
		}, want: "none of Dolt's series", is: ErrNoSeries},
		{name: "compressed without being asked", h: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			zw := gzip.NewWriter(w)
			_, _ = zw.Write([]byte(good))
			_ = zw.Close()
		}, want: "none of Dolt's series", is: ErrNoSeries},
		{name: "a redirect is not followed", h: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusFound)
		}, want: "answered 302"},
		{name: "a permanent redirect is not followed", h: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target, http.StatusPermanentRedirect)
		}, want: "answered 308"},
		{name: "an error", h: func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, good, http.StatusInternalServerError)
		}, want: "answered 500"},
		{name: "not found", h: func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, want: "answered 404"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noLeak(t)
			u := tc.url
			if u == "" {
				u = serve(t, tc.h)
			}
			r, err := New(t.Context(), u)
			if err != nil {
				t.Fatalf("New(%q) = %v", u, err)
			}
			r.timeout = patient
			if errors.Is(tc.is, context.DeadlineExceeded) {
				r.timeout = timeout
			}
			defer r.Close()
			start := time.Now()
			got, err := r.Read(t.Context())
			// The two seconds are for the machine, not the reader: how late
			// a goroutine whose deadline has passed may be told so, and get
			// to run, where the tests share a processor.
			if took := time.Since(start); took > r.timeout+2*time.Second {
				t.Errorf("Read took %s with a deadline of %s", took, r.timeout)
			}
			if tc.want == "" {
				if err != nil || got.Conns == nil || *got.Conns != tc.conn {
					t.Errorf("Read = %s, %v; want %d connections", show(got), err, tc.conn)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || (tc.is != nil && !errors.Is(err, tc.is)) {
				t.Errorf("Read = %v, want an error containing %q (and wrapping %v)", err, tc.want, tc.is)
			}
			if show(got) != show(Sample{}) {
				t.Errorf("Read failed and still returned %s", show(got))
			}
		})
	}
	if n := elsewhere.Load(); n != 0 {
		t.Errorf("the redirects' target was asked %d times, want 0", n)
	}
}

// A read ends when its context does.
func TestReadCanceled(t *testing.T) {
	noLeak(t)
	u := serve(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	r, err := New(t.Context(), u)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if _, err := r.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Read = %v, want context.Canceled", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Read took %s after its context was canceled", took)
	}
}
