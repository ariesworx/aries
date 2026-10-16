package server

import (
	"crypto/tls"
	"io"
	"net/http"
	"testing"
)

// Every route the HTTPS listener serves, sent with no credentials, and
// the status each answers. Only the two descriptions and the revocation
// answer 200 without authentication: revocation answers 200 whether or
// not the token is known (RFC 7009, §2.2), and ends nothing for a
// request with no token. A route the listener registers is added to this
// table with its status without credentials.
func TestHTTPSRoutes(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: w.pool, MinVersion: tls.VersionTLS12}, ForceAttemptHTTP2: true}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	routes := []struct {
		method, path string
		status       int
	}{
		// Public: the server's description and the authorization server's metadata.
		{http.MethodGet, "/.well-known/aries", http.StatusOK},
		{http.MethodGet, "/.well-known/oauth-authorization-server", http.StatusOK},
		// The connect path with no upgrade, and with no proof or token.
		{http.MethodGet, "/v1/connect", http.StatusBadRequest},
		// The sign-in paths refuse a request with no client or no code.
		{http.MethodGet, "/oauth/authorize", http.StatusBadRequest},
		{http.MethodGet, "/auth/callback", http.StatusBadRequest},
		{http.MethodPost, "/oauth/token", http.StatusBadRequest},
		{http.MethodPost, "/oauth/revoke", http.StatusOK},
		// A method the path does not take falls through to the 404 of "/".
		{http.MethodPost, "/v1/connect", http.StatusNotFound},
		{http.MethodPost, "/.well-known/aries", http.StatusNotFound},
		{http.MethodGet, "/oauth/token", http.StatusNotFound},
		{http.MethodGet, "/oauth/revoke", http.StatusNotFound},
		{http.MethodPut, "/oauth/token", http.StatusNotFound},
		// Paths the listener does not serve, including the loopback
		// listener's and the sign-in paths' prefixes.
		{http.MethodGet, "/", http.StatusNotFound},
		{http.MethodGet, "/metrics", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusNotFound},
		{http.MethodGet, "/readyz", http.StatusNotFound},
		{http.MethodGet, "/v1/connect/x", http.StatusNotFound},
		{http.MethodGet, "/.well-known/aries/x", http.StatusNotFound},
		{http.MethodGet, "/oauth/token/x", http.StatusNotFound},
		{http.MethodGet, "/.well-known/oauth-authorization-server/x", http.StatusNotFound},
	}
	for _, r := range routes {
		req, err := http.NewRequestWithContext(t.Context(), r.method, w.url+r.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", r.method, r.path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != r.status {
			t.Errorf("%s %s with no credentials = %d, want %d", r.method, r.path, resp.StatusCode, r.status)
		}
	}
}
