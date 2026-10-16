package client

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/proto"
)

// fakeAS is a stand-in for the server's authorization endpoints: it
// signs whoever comes in at once, and checks what the token endpoint is
// sent as the server does.
type fakeAS struct {
	srv *httptest.Server
	url string

	mu      sync.Mutex
	codes   map[string]string // code → challenge
	proofs  int
	revoked []string
	refuse  *proto.Error // set: the token endpoint refuses with it
}

func newFakeAS(t *testing.T) (*fakeAS, *Config) {
	t.Helper()
	f := &fakeAS{codes: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/aries", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"version": "test", "transports": []string{"https"}, "auth": []string{"key", "signin"},
			"providers": []map[string]string{{"name": "corp", "kind": "oidc"}, {"name": "other", "kind": "oidc"}}})
	})
	mux.HandleFunc("GET /oauth/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "aries-cli" || q.Get("code_challenge_method") != "S256" || q.Get("provider") != "corp" ||
			q.Get("scope") != "cli mcp" || q.Get("resource") != f.url {
			http.Error(w, "bad request "+r.URL.RawQuery, http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.codes["code-1"] = q.Get("code_challenge")
		f.mu.Unlock()
		v := url.Values{"code": {"code-1"}, "state": {q.Get("state")}, "iss": {f.url}}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+v.Encode(), http.StatusFound) //nolint:gosec // a test's stand-in, redirecting where the test asks
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.refuse != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "access_denied", "error_description": f.refuse.Message,
				"code": string(f.refuse.Code), "fix": f.refuse.Fix})
			return
		}
		jkt, err := dpop.NewVerifier(10).Verify(r.Header.Get("DPoP"), "POST", []string{f.url + "/oauth/token"}, "", time.Now())
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if err != nil || f.codes[r.PostForm.Get("code")] != base64.RawURLEncoding.EncodeToString(sum[:]) || r.PostForm.Get("machine") != "laptop" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "no"})
			return
		}
		f.proofs++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + jkt[:8], "token_type": "DPoP", "expires_in": 86400,
			"expires_at": "2026-10-17T12:00:00Z", "scope": "cli mcp", "principal": "alice", "provider": "corp", "grant": "g-1"})
	})
	mux.HandleFunc("POST /oauth/revoke", func(_ http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PostForm.Get("token"))
		f.mu.Unlock()
	})
	f.srv = httptest.NewUnstartedServer(mux)
	f.srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	f.url = f.srv.URL
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
	cfg := &Config{Project: "00000000-0000-4000-8000-000000000001"}
	cfg.Server.URL, cfg.Server.CA = f.url, ca
	return f, cfg
}

// follow opens u as a browser would, following redirects until one
// leads to the loopback redirect, which it returns unopened.
func follow(t *testing.T, f *fakeAS, u, redirect string) string {
	t.Helper()
	c := f.srv.Client()
	c.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(req.URL.String(), redirect) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.Header.Get("Location")
}

func TestLogin(t *testing.T) {
	t.Parallel()
	f, cfg := newFakeAS(t)
	info, err := FetchServerInfo(t.Context(), cfg)
	if err != nil {
		t.Fatalf("FetchServerInfo = %v", err)
	}
	if len(info.Providers) != 2 || info.Providers[0].Name != "corp" {
		t.Fatalf("FetchServerInfo = %+v, want two providers, corp first", info)
	}
	const redirect = "http://127.0.0.1:53682/callback"
	l, err := StartLogin(cfg, "corp", redirect)
	if err != nil {
		t.Fatalf("StartLogin = %v", err)
	}
	if strings.Contains(l.URL, l.verifier) || !strings.HasPrefix(l.URL, f.url+"/oauth/authorize?") {
		t.Fatalf("StartLogin's URL = %s, want the server's authorization endpoint, never the verifier", l.URL)
	}
	back := follow(t, f, l.URL, redirect)
	s, err := l.Finish(t.Context(), back, "laptop", "loopback")
	if err != nil {
		t.Fatalf("Finish = %v", err)
	}
	if !strings.HasPrefix(s.Token, "tok-") || s.Principal != "alice" || s.Provider != "corp" || s.Grant != "g-1" ||
		s.Server != cfg.Server.URL || s.Key == nil || s.ExpiresAt.Format(time.RFC3339) != "2026-10-17T12:00:00Z" {
		t.Errorf("Finish = %+v, want alice's sign-in for the day, with its key", s)
	}
	if want := "tok-" + dpop.Thumbprint(&s.Key.PublicKey)[:8]; s.Token != want {
		t.Errorf("the token is bound to %s, want the sign-in's own key", s.Token)
	}
	if err := Logout(t.Context(), cfg, s); err != nil {
		t.Errorf("Logout = %v", err)
	}
	if len(f.revoked) != 1 || f.revoked[0] != s.Token {
		t.Errorf("Logout revoked %d tokens, want the sign-in's", len(f.revoked))
	}
}

func TestLoginFinishRefuses(t *testing.T) {
	t.Parallel()
	f, cfg := newFakeAS(t)
	const redirect = "http://127.0.0.1:53682/callback"
	l, err := StartLogin(cfg, "corp", redirect)
	if err != nil {
		t.Fatal(err)
	}
	back := follow(t, f, l.URL, redirect)
	u, _ := url.Parse(back)
	with := func(k, v string) string {
		q := u.Query()
		if v == "" {
			q.Del(k)
		} else {
			q.Set(k, v)
		}
		c := *u
		c.RawQuery = q.Encode()
		return c.String()
	}
	for _, tc := range []struct{ name, address, want string }{
		{"another sign-in's address", with("state", "not-this-one"), "another sign-in"},
		{"no state", with("state", ""), "another sign-in"},
		{"another server's", with("iss", "https://evil.example.com"), "evil.example.com"},
		{"no iss", with("iss", ""), "names no server"},
		{"no code", with("code", ""), "no code"},
		{"another address", strings.Replace(back, "53682", "1", 1), "53682"},
		{"not an address", "%zz", "address"},
		{"the provider refused", with("error", "access_denied") + "&error_description=" + url.QueryEscape("no invitation for you"), "no invitation for you"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := l.Finish(t.Context(), tc.address, "laptop", "url")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Finish(%s) = %v, want a refusal naming %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestLoginRefusalCarriesFix: the server's refusal at the token
// endpoint, an uninvited person's say, reaches aries as the protocol's
// refusal, with its code and fix.
func TestLoginRefusalCarriesFix(t *testing.T) {
	t.Parallel()
	f, cfg := newFakeAS(t)
	f.refuse = proto.Errf(proto.CodeForbidden, "ask an admin of your project to invite this address", "no invitation for this address at corp")
	const redirect = "http://127.0.0.1:53682/callback"
	l, err := StartLogin(cfg, "corp", redirect)
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Finish(t.Context(), follow(t, f, l.URL, redirect), "laptop", "loopback")
	pe, ok := errors.AsType[*proto.Error](err)
	if !ok || pe.Code != proto.CodeForbidden || pe.Fix != f.refuse.Fix || pe.Message != f.refuse.Message {
		t.Errorf("Finish(refused) = %v, want %v", err, f.refuse)
	}
}
