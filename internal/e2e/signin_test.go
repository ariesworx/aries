package e2e

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/oidc/oidctest"
	"github.com/ariesworx/aries/internal/server"
)

// login is the daemon's sign-in, with daemonOpts.signin: the provider
// corp, which is the world's stand-in issuer, made at the first start.
// now is the control store's clock.
func (w *world) login(now func() time.Time) (*server.Login, error) {
	if !w.opts.signin {
		return nil, nil
	}
	if w.issuer == nil {
		w.issuer = oidctest.New(w.t)
		w.secret = filepath.Join(w.t.TempDir(), "corp-secret")
		if err := os.WriteFile(w.secret, []byte(oidctest.Secret), 0o600); err != nil {
			return nil, err
		}
	}
	p, err := oidc.Open(oidc.Settings{Name: "corp", Kind: oidc.KindOIDC, Issuer: w.issuer.URL, ClientID: oidctest.ClientID,
		ClientSecretFile: w.secret, CAFile: w.issuer.CAFile}, oidc.Options{Now: now, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute})
	if err != nil {
		return nil, err
	}
	return &server.Login{Providers: []*oidc.Provider{p}}, nil
}

// browserClient is a person's browser in the world: it trusts the
// front's CA and the provider's, and follows redirects, to the provider
// and back, until stop says to stop at one.
func (w *world) browserClient(stop func(*http.Request) bool) *http.Client {
	w.t.Helper()
	pool := w.web.pool.Clone()
	pem, err := os.ReadFile(w.issuer.CAFile)
	if err != nil {
		w.t.Fatal(err)
	}
	pool.AppendCertsFromPEM(pem)
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if stop != nil && stop(req) {
				return http.ErrUseLastResponse
			}
			return nil
		}}
}

// openBrowser is a browser `aries login` opens: it goes to the URL,
// signs in at the provider as whoever the stand-in signs in, and lands
// on aries's loopback address, as a person's browser would. It returns
// at once, as a browser's opener does; the browsing ends before the test.
func (w *world) openBrowser() func(string) error {
	var wg sync.WaitGroup
	w.t.Cleanup(wg.Wait)
	c := w.browserClient(nil)
	return func(u string) error {
		wg.Go(func() {
			resp, err := c.Get(u)
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		})
		return nil
	}
}

// signInWorld is a server of many projects over HTTPS that signs people
// in at corp, whose server admin is dana; and alice, invited at corp,
// with a key the server does not know.
func signInWorld(t *testing.T) (*world, *user, *user) {
	t.Helper()
	w := newWorld(t, daemonOpts{many: true, keys: true, https: true, signin: true, admins: []string{"dana"}})
	dana := w.newUser("dana", "")
	dana.ok("admin", "principal", "add", "alice", "--invite", "alice@example.com", "--provider", "corp")
	alice := w.newUser("", "")
	alice.browser = w.openBrowser()
	return w, dana, alice
}

// TestSignInEndToEnd: aries login signs alice in through the server at
// the provider, her browser coming back to aries's loopback address;
// every command then connects with the sign-in, in place of a key the
// server does not know; and aries logout ends it.
func TestSignInEndToEnd(t *testing.T) {
	t.Parallel()
	w, dana, alice := signInWorld(t)
	r := alice.run("v0.2.0", "whoami")
	if r.code == 0 || !strings.Contains(r.stderr, "aries login") {
		t.Fatalf("whoami before signing in: exit %d\n%s%s; want a refusal whose fix names aries login", r.code, r.stdout, r.stderr)
	}
	out := alice.ok("login")
	if !strings.HasPrefix(out, "signed in as alice at corp, until ") {
		t.Fatalf("aries login = %q, want alice signed in at corp", out)
	}
	who := alice.ok("whoami")
	if !strings.Contains(who, "principal  alice") || !strings.Contains(who, "signed in  at corp, until") {
		t.Errorf("whoami after signing in =\n%s\nwant alice, signed in at corp", who)
	}
	js := alice.ok("--json", "whoami")
	if !strings.Contains(js, `"signin":{"id":"g-`) {
		t.Errorf("whoami --json = %s, want the sign-in", js)
	}
	alice.ok("create", "signed in, and working")
	// The token and its key are the user's alone, and the token is in
	// no output.
	data := alice.env[offline.EnvDataDir]
	s, err := client.LoadSignIn(data, w.web.url)
	if err != nil || s == nil {
		t.Fatalf("the sign-in kept = %v, %v", s, err)
	}
	if strings.Contains(out+who+js, s.Token) {
		t.Error("aries printed the token")
	}
	// The sign-in is announced in her inbox, with how to end it, and
	// with no token or address.
	inbox := alice.ok("inbox")
	if !strings.Contains(inbox, "signin.granted from ariesd") || !strings.Contains(inbox, "`aries admin signin end "+s.Grant+"`") ||
		!strings.Contains(inbox, "laptop-test") {
		t.Errorf("aries inbox after signing in =\n%s\nwant the sign-in announced, naming %s and laptop-test", inbox, s.Grant)
	}
	for _, secret := range []string{s.Token, "alice@example.com"} {
		if strings.Contains(inbox, secret) {
			t.Errorf("aries inbox holds %q:\n%s", secret, inbox)
		}
	}
	if runtime.GOOS != "windows" {
		for _, f := range []string{"token.json", "dpop.key"} {
			st, err := os.Stat(filepath.Join(client.SignInDir(data, w.web.url), f))
			if err != nil || st.Mode().Perm() != 0o600 {
				t.Errorf("%s: %v, mode %v; want 0600", f, err, st.Mode().Perm())
			}
		}
	}
	list := dana.ok("admin", "signin", "list", "alice")
	if !strings.Contains(list, s.Grant) || !strings.Contains(list, "live") || !strings.Contains(list, "laptop-test") {
		t.Errorf("admin signin list alice =\n%s\nwant her live sign-in from laptop-test", list)
	}
	if out := alice.ok("logout"); !strings.HasPrefix(out, "signed out of ") {
		t.Errorf("aries logout = %q", out)
	}
	if s, _ := client.LoadSignIn(data, w.web.url); s != nil {
		t.Error("the sign-in is still kept after logout")
	}
	if r := alice.run("v0.2.0", "whoami"); r.code == 0 {
		t.Errorf("whoami after logout: exit 0, want a refusal\n%s", r.stdout)
	}
	if list := dana.ok("admin", "signin", "list", "alice"); !strings.Contains(list, "ended (logout)") {
		t.Errorf("admin signin list alice after logout =\n%s\nwant it ended by logout", list)
	}
}

// TestSignInByURL: with no browser here, aries login prints the URL, and
// takes back the address the browser, on any machine, landed on.
func TestSignInByURL(t *testing.T) {
	t.Parallel()
	w, _, alice := signInWorld(t)
	alice.browser = nil
	in, typed := io.Pipe()
	alice.input = in
	errR, errW := io.Pipe()
	var out strings.Builder
	done := make(chan int, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		code := alice.runTo(ctx, &out, errW, "v0.2.0", "login", "--url")
		_ = errW.Close()
		done <- code
	}()
	// The URL is the line of standard error that is one.
	var authorize string
	sc := bufio.NewScanner(errR)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, w.web.url+"/oauth/authorize?") {
			authorize = line
			break
		}
	}
	go func() { _, _ = io.Copy(io.Discard, errR) }()
	if authorize == "" {
		t.Fatal("aries login --url printed no URL")
	}
	// A browser on another machine: it stops at the loopback address,
	// which it cannot open, and the person copies it.
	b := w.browserClient(func(r *http.Request) bool { return r.URL.Scheme == "http" })
	resp, err := b.Get(authorize)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	address := resp.Header.Get("Location")
	if !strings.HasPrefix(address, "http://127.0.0.1:") {
		t.Fatalf("the browser landed on %q, not aries's loopback address", address)
	}
	_, _ = io.WriteString(typed, "not an address\n"+address+"\n")
	if code := <-done; code != 0 || !strings.HasPrefix(out.String(), "signed in as alice at corp") {
		t.Fatalf("aries login --url: exit %d, %q", code, out.String())
	}
	_ = typed.Close()
	if who := alice.ok("whoami"); !strings.Contains(who, "principal  alice") {
		t.Errorf("whoami after signing in by URL =\n%s", who)
	}
}

// TestSignInRefusedUninvited: a person nobody invited is refused, and
// told to ask for an invitation; nothing is kept.
func TestSignInRefusedUninvited(t *testing.T) {
	t.Parallel()
	w, _, _ := signInWorld(t)
	bob := w.newUser("", "")
	bob.browser = w.openBrowser()
	w.issuer.SignIn(oidctest.Person{Subject: "bob-sub", Email: "bob@example.com", Verified: true})
	r := bob.run("v0.2.0", "login")
	if r.code == 0 || !strings.Contains(r.stderr, "no invitation for bob@example.com at corp") || !strings.Contains(r.stderr, "aries admin invite add") {
		t.Fatalf("aries login, uninvited: exit %d\n%s%s; want the refusal, with a fix to ask for an invitation", r.code, r.stdout, r.stderr)
	}
	if s, _ := client.LoadSignIn(bob.env[offline.EnvDataDir], w.web.url); s != nil {
		t.Error("a refused sign-in was kept")
	}
}
