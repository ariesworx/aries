package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/oidc/oidctest"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/telemetry"
)

// jwtLike is the shape of a JWS in compact form: an ID token, or any
// token signed as one.
var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.`)

// TestNoSecretInLogsSpansOrOutput is rule 2 for the paths v0.5.0 ships
// (aries-afomcff2): a server of many projects over HTTPS, a server
// admin's session of `aries admin` connected by key proof, and a person
// signed in at a provider, whose sign-in is used, forged and ended.
// Everything that leaves the processes is captured: the daemon's log at
// debug, the log records and spans its telemetry would export, and each
// command's standard output and error. None holds a private key, the
// provider's client secret, a code or state of either redirect, an ID
// token, the access token or its DPoP key, or a forged token; and no
// span or exported record holds a person's address.
func TestNoSecretInLogsSpansOrOutput(t *testing.T) {
	t.Parallel()
	logBuf := &lockedBuffer{}
	logger := daemonLog(t, logBuf)
	tel, manual, err := telemetry.NewManual(telemetry.Config{Version: "v0.2.0", Ops: server.Ops(), PerProject: true, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tel.Shutdown(context.Background()) })
	// As `ariesd serve` wires it: every line is also an exported record.
	logger.Handler().(*logs.Handler).Tap(tel.LogHandler())

	w := newWorld(t, daemonOpts{many: true, keys: true, https: true, signin: true, admins: []string{"dana"},
		logger: logger, telemetry: tel, limits: server.Limits{ControlWriteBurst: 500}})
	var out strings.Builder
	run := func(u *user, wantOK bool, args ...string) {
		t.Helper()
		r := u.run("v0.2.0", args...)
		if (r.code == 0) != wantOK {
			t.Fatalf("aries %s: exit %d\n%s%s", strings.Join(args, " "), r.code, r.stdout, r.stderr)
		}
		out.WriteString(r.stdout + r.stderr)
	}

	// The server admin's session, by key proof over HTTPS.
	dana := w.newUser("dana", "")
	bob := w.newUser("", "")
	for _, args := range [][]string{
		{"admin", "principal", "add", "alice", "--invite", "alice@example.com", "--provider", "corp"},
		{"admin", "principal", "add", "bob"},
		{"admin", "key", "add", "bob", bob.pubFile},
		{"admin", "member", "list"},
		{"admin", "invite", "list"},
		{"admin", "status"},
		{"--json", "admin", "status"},
	} {
		run(dana, true, args...)
	}
	run(bob, true, "ready")
	// A key the server does not know is refused at the key proof.
	stranger := w.newUser("", "")
	run(stranger, false, "ready")

	// alice signs in at corp, through a browser whose every address is
	// kept: the provider's code and state, and the server's, are in them.
	alice := w.newUser("", "")
	var mu sync.Mutex
	var visited []string
	c := w.browserClient(func(req *http.Request) bool {
		mu.Lock()
		visited = append(visited, req.URL.String())
		mu.Unlock()
		return false
	})
	var browsing sync.WaitGroup
	t.Cleanup(browsing.Wait)
	alice.browser = func(u string) error {
		mu.Lock()
		visited = append(visited, u)
		mu.Unlock()
		browsing.Go(func() {
			resp, err := c.Get(u) //nolint:noctx // the test's own loopback and stand-in provider
			if err == nil {
				_ = resp.Body.Close()
			}
		})
		return nil
	}
	run(alice, true, "login")
	browsing.Wait()
	run(alice, true, "whoami")
	run(alice, true, "create", "signed in, and working")
	run(dana, true, "admin", "signin", "list", "alice")

	data := alice.env[offline.EnvDataDir]
	s, err := client.LoadSignIn(data, w.web.url)
	if err != nil || s == nil {
		t.Fatalf("the sign-in kept = %v, %v", s, err)
	}
	dir := client.SignInDir(data, w.web.url)
	dpop, err := os.ReadFile(filepath.Join(dir, "dpop.key")) //nolint:gosec // the test user's own data directory
	if err != nil {
		t.Fatal(err)
	}

	// A forged token, in place of hers, is refused, and is logged nowhere.
	const forged = "forged-T0KEN-7f3a9c1e5b2d4086"
	tokenFile := filepath.Join(dir, "token.json")
	kept, err := os.ReadFile(tokenFile) //nolint:gosec // the test user's own data directory
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(strings.Replace(string(kept), s.Token, forged, 1)), 0o600); err != nil { //nolint:gosec // the test user's own data directory
		t.Fatal(err)
	}
	run(alice, false, "whoami")
	if err := os.WriteFile(tokenFile, kept, 0o600); err != nil { //nolint:gosec // the test user's own data directory
		t.Fatal(err)
	}
	run(alice, true, "logout")

	secrets := map[string]string{
		"the provider's client secret": oidctest.Secret,
		"alice's access token":         s.Token,
		"the forged token":             forged,
	}
	addPEM := func(what string, b []byte) {
		secrets[what] = string(b)
		for i, line := range strings.Split(string(b), "\n") {
			if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
				secrets[fmt.Sprintf("%s, line %d", what, i+1)] = line
			}
		}
	}
	addPEM("alice's DPoP key", dpop)
	for name, u := range map[string]*user{"dana": dana, "bob": bob, "the stranger": stranger, "alice": alice} {
		b, err := os.ReadFile(filepath.Join(u.repo, "id_ed25519"))
		if err != nil {
			t.Fatal(err)
		}
		addPEM(name+"'s private key", b)
	}
	codes := 0
	for _, v := range visited {
		i := strings.IndexByte(v, '?')
		if i < 0 {
			continue
		}
		for _, kv := range strings.Split(v[i+1:], "&") {
			k, val, _ := strings.Cut(kv, "=")
			if (k == "code" || k == "state" || k == "nonce" || k == "code_challenge") && len(val) >= 8 {
				secrets[k+" in "+v[:i]] = val
				if k == "code" {
					codes++
				}
			}
		}
	}
	if codes < 2 {
		t.Fatalf("the browser saw %d codes, want the provider's and the server's:\n%s", codes, strings.Join(visited, "\n"))
	}

	// What left the processes.
	var spans, exported strings.Builder
	for _, sp := range manual.Spans() {
		spans.WriteString(sp.Name + " " + sp.StatusNote + " " + sp.TraceState + "\n")
		for k, v := range sp.Attrs {
			spans.WriteString(k + "=" + v + "\n")
		}
	}
	for _, r := range manual.Logs() {
		exported.WriteString(r.Body + " " + r.Event + "\n")
		for k, v := range r.Attrs {
			exported.WriteString(k + "=" + v + "\n")
		}
	}
	if !strings.Contains(spans.String(), "handshake") || !strings.Contains(exported.String(), "request") {
		t.Fatalf("too little was captured to prove anything:\nspans:\n%s\nexported:\n%s", spans.String(), exported.String())
	}
	sinks := map[string]string{
		"the daemon's log":     logBuf.String(),
		"the spans":            spans.String(),
		"the exported records": exported.String(),
		"the commands' output": out.String(),
	}
	for sink, text := range sinks {
		for what, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Errorf("%s holds %s (%q)", sink, what, secret)
			}
		}
		if m := jwtLike.FindString(text); m != "" {
			t.Errorf("%s holds a JWS, such as an ID token: %q", sink, m)
		}
		if strings.Contains(text, "PRIVATE KEY") {
			t.Errorf("%s holds a private key", sink)
		}
	}
	for _, sink := range []string{"the spans", "the exported records"} {
		if strings.Contains(sinks[sink], "alice@example.com") {
			t.Errorf("%s holds alice's address", sink)
		}
	}
}
