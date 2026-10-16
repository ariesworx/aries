package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// authorizeFrom answers a's request to /oauth/authorize as the server
// answers one from addr, and returns where it sends the browser.
func (w *signInWorld) authorizeFrom(t *testing.T, a *attempt, addr string) *url.URL {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, w.authorizeURL(a), nil)
	req.RemoteAddr = addr + ":40000"
	rec := httptest.NewRecorder()
	w.auth.authorize(rec, req)
	to, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusFound || err != nil {
		t.Fatalf("authorize from %s = %d, Location %q; want a redirect", addr, rec.Code, rec.Header().Get("Location"))
	}
	return to
}

// refusedAt returns the error, and its description, that a redirect to
// the loopback address carries; both are empty for a redirect to the
// provider.
func refusedAt(to *url.URL) (code, why string) {
	if !strings.HasPrefix(to.String(), loopbackRedirect) {
		return "", ""
	}
	return to.Query().Get("error"), to.Query().Get("error_description")
}

// Anonymous requests to /oauth/authorize, each of which holds a sign-in
// in progress, cannot block a person's: one address holds
// signins_pending_per_addr of them at most, and past
// signins_pending_per_server the oldest is let go of.
func TestPendingSignInsFlood(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		from func(i int) string
		// refused is how many of the 40 requests are refused.
		refused int
		// midway is whether a sign-in begun before the flood, from
		// another address, still finishes.
		midway bool
		// evicted is how many sign-ins were let go of to make room.
		evicted int
	}{
		{"one address", func(int) string { return "192.0.2.1" }, 38, true, 0},
		// 41 begun before the person's, and the person's, past 8.
		{"many addresses", func(i int) string { return fmt.Sprintf("192.0.2.%d", i+1) }, 0, false, 34},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tel, m := measured(t, false)
			w := newLimitedSignInWorld(t, Limits{SignInsPending: 8, SignInsPendingPerAddr: 2}, nil, tel)
			before := newAttempt(t)
			atProvider := w.authorizeFrom(t, before, "198.51.100.7")
			refused := 0
			for i := range 40 {
				code, why := refusedAt(w.authorizeFrom(t, newAttempt(t), tc.from(i)))
				switch {
				case code == "":
				case code == "temporarily_unavailable" && strings.Contains(why, "this address has 2 sign-ins in progress"):
					refused++
				default:
					t.Errorf("flood request %d refused with %s: %s", i, code, why)
				}
			}
			if refused != tc.refused {
				t.Errorf("%d of 40 requests were refused, want %d", refused, tc.refused)
			}
			if got := w.signIn(t, newAttempt(t)); got.AccessToken == "" {
				t.Errorf("a sign-in during the flood = %+v, want a token", got)
			}
			checkSoon(t, m, []want{
				{"aries.limit.refusals", []string{"limit", "signins_pending_per_addr"}, float64(tc.refused)},
				{"aries.limit.refusals", []string{"limit", "signins_pending_per_server"}, float64(tc.evicted)},
			})
			if tc.midway {
				back := w.browse(t, atProvider.String())
				if got := w.exchange(t, before, back.Get("code"), ""); got.AccessToken == "" {
					t.Errorf("a sign-in begun before the flood = %+v, want a token", got)
				}
				return
			}
			resp, err := w.browser.Get(atProvider.String())
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("a sign-in begun before a flood past signins_pending_per_server came back %s, Location %q; want 400, the oldest let go of",
					resp.Status, resp.Header.Get("Location"))
			}
		})
	}
}

// A sign-in refused past signins_pending_per_addr is charged to its
// address's auth failures, so that an address that keeps asking is
// refused, once its bucket is empty, even after its sign-ins lapsed.
func TestPendingSignInsChargeAuthFailures(t *testing.T) {
	t.Parallel()
	w := newLimitedSignInWorld(t, Limits{SignInsPendingPerAddr: 1, AuthFailureBurst: 2, AuthFailureRate: 0.01}, nil, nil)
	const from = "192.0.2.1"
	for i, want := range []string{"", "temporarily_unavailable", "temporarily_unavailable"} {
		if got, why := refusedAt(w.authorizeFrom(t, newAttempt(t), from)); got != want {
			t.Fatalf("request %d from %s refused with %q (%s), want %q", i, from, got, why, want)
		}
	}
	w.clk.advance(11 * time.Minute) // past signInWait: the one sign-in held has lapsed
	if got, why := refusedAt(w.authorizeFrom(t, newAttempt(t), from)); got != "temporarily_unavailable" || !strings.Contains(why, "wait") {
		t.Errorf("after its sign-ins lapsed, an address with no auth failures left was answered %q (%s); want temporarily_unavailable, saying how long to wait", got, why)
	}
	if got, why := refusedAt(w.authorizeFrom(t, newAttempt(t), "198.51.100.7")); got != "" {
		t.Errorf("another address was refused with %s (%s); want it sent to the provider", got, why)
	}
}
