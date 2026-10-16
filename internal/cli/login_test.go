package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
)

// TestLoopbackIgnoresOtherState: a request to the callback that does not
// carry the sign-in's state, from anything on this machine, is refused
// and leaves the login waiting for the browser's real return.
func TestLoopbackIgnoresOtherState(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		addresses := make(chan string, 2)
		results := make(chan error, 1)
		h := loopbackHandler("good-state", addresses, results)

		for _, target := range []string{
			"http://127.0.0.1:4000/callback?state=wrong&code=c1",
			"http://127.0.0.1:4000/callback?code=c1",
		} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
			if w.Code != http.StatusBadRequest {
				t.Errorf("GET %s = %d %q, want %d", target, w.Code, w.Body, http.StatusBadRequest)
			}
		}
		select {
		case a := <-addresses:
			t.Fatalf("a request with another state handed the login %q, want nothing", a)
		default:
		}

		back := "http://127.0.0.1:4000/callback?state=good-state&code=c2"
		results <- nil
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, back, nil))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "signed in") {
			t.Errorf("GET %s = %d %q, want %d and signed in", back, w.Code, w.Body, http.StatusOK)
		}
		select {
		case a := <-addresses:
			if a != back {
				t.Errorf("the login was handed %q, want %q", a, back)
			}
		default:
			t.Errorf("the browser's return with the state handed the login nothing, want %q", back)
		}

		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, back, nil))
		if w.Code != http.StatusConflict {
			t.Errorf("GET %s again = %d %q, want %d", back, w.Code, w.Body, http.StatusConflict)
		}
	})
}
