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

// TestLoginWorkloadUsage: `aries login --workload` refuses what it
// cannot do before it reads anything: a person's flag, a kind it does
// not know, and a GitHub runner with no data directory of the job's.
func TestLoginWorkloadUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		code int
		want string
	}{
		{"with --url", []string{"login", "--workload", "github-actions", "--url"}, nil, ExitUsage, "--url is a person's sign-in"},
		{"an unknown kind", []string{"login", "--workload", "kubernetes"}, nil, ExitUsage, "azure, github-actions, google"},
		{"an unknown kind from the environment", []string{"login"}, map[string]string{"ARIES_WORKLOAD": "jenkins"}, ExitUsage, `not "jenkins"`},
		{"a runner with no data directory", []string{"login", "--workload", "github-actions"}, map[string]string{"RUNNER_TEMP": "/tmp/runner"},
			ExitFailure, "ARIES_DATA_DIR: ${{ runner.temp }}/aries"},
		{"a data directory set: on to the config", []string{"login", "--workload", "github-actions"},
			map[string]string{"RUNNER_TEMP": "/tmp/runner", "ARIES_DATA_DIR": "/tmp/runner/aries"}, ExitFailure, ".aries.yaml not found"},
	} {
		var out, errb strings.Builder
		code := Run(t.Context(), append([]string{"-C", empty}, tc.args...), Env{Stdout: &out, Stderr: &errb,
			Getenv: func(k string) string { return tc.env[k] }, Version: "v0.3.0"})
		if code != tc.code || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%s: aries %v = %d, %q; want %d naming %q", tc.name, tc.args, code, errb.String(), tc.code, tc.want)
		}
	}
}
