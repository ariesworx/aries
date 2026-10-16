package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
)

// `aries login` and `aries logout` (transport.md §6.2, §6.6): a sign-in
// at the server's provider, through the server's own authorization
// server, kept in the user's data directory; and its end.

// loginWait bounds how long `aries login` waits for the browser to come
// back: the server forgets a sign-in started 10 minutes before.
const loginWait = 10 * time.Minute

// callbackPath is the loopback redirect's path.
const callbackPath = "/callback"

const loginUsage = "login [--provider NAME] [--url] | login --workload KIND [--provider NAME]"

// signedIn is what `aries login` reports.
type signedIn struct {
	Principal string    `json:"principal"`
	Provider  string    `json:"provider"`
	Server    string    `json:"server"`
	ExpiresAt time.Time `json:"expires_at"`
	SignIn    string    `json:"signin"`
	Scopes    []string  `json:"scopes"`
	TrustRule string    `json:"trust_rule,omitempty"`
}

// cmdLogin signs the person in at a provider of the server, and keeps
// the sign-in for every aries and aries mcp of the user that reaches it.
func cmdLogin(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("login")
	provider := fs.String("provider", "", "the provider to sign in at, when the server has several")
	byURL := fs.Bool("url", false, "print the URL to open in any browser, and take back the address it lands on")
	workload := fs.String("workload", r.env.Getenv("ARIES_WORKLOAD"), "sign in as a service principal with this platform's token: "+
		strings.Join(client.WorkloadKinds, ", ")+" (env ARIES_WORKLOAD)")
	pos, err := parse(fs, args, loginUsage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(loginUsage, "login takes no arguments")
	}
	if *workload != "" && *byURL {
		return usagef(loginUsage, "--url is a person's sign-in in a browser; --workload signs in with the platform's token, and opens none")
	}
	if *workload != "" && !slices.Contains(client.WorkloadKinds, *workload) {
		return usagef(loginUsage, "--workload is one of %s, not %q", strings.Join(client.WorkloadKinds, ", "), *workload)
	}
	if *workload == client.WorkloadGitHubActions && r.env.Getenv("ARIES_DATA_DIR") == "" && r.env.Getenv("RUNNER_TEMP") != "" {
		// A self-hosted runner keeps its home from job to job; the
		// runner's temporary directory is emptied after each.
		return proto.Errf(proto.CodeInvalid, "set ARIES_DATA_DIR: ${{ runner.temp }}/aries in the job's env, so that every step finds the sign-in and none outlives the job (docs/ci.md)",
			"ARIES_DATA_DIR is not set, and a runner's home may outlive the job")
	}
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		return err
	}
	data, err := offline.DataDir(r.env.GOOS, r.env.Getenv, r.env.UserHomeDir)
	if err != nil {
		return err
	}
	info, err := client.FetchServerInfo(ctx, cfg)
	if err != nil {
		return err
	}
	if *workload != "" {
		s, err := client.WorkloadLogin(ctx, cfg, info, *workload, *provider, machineName(r), client.WorkloadEnv{Getenv: r.env.Getenv})
		if err != nil {
			return err
		}
		return r.keepSignIn(ctx, cfg, data, s)
	}
	name, err := chooseProvider(info, *provider)
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen on loopback for the browser's return: %w", err)
	}
	redirect := "http://" + l.Addr().String() + callbackPath
	login, err := client.StartLogin(cfg, name, redirect)
	if err != nil {
		_ = l.Close()
		return err
	}
	// The authorization URL carries the state the browser brings back.
	authorize, err := url.Parse(login.URL)
	if err != nil {
		_ = l.Close()
		return fmt.Errorf("parse the sign-in's URL: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, loginWait)
	defer cancel()
	addresses := make(chan string, 2)
	results := make(chan error, 1)
	srv := &http.Server{Handler: loopbackHandler(authorize.Query().Get("state"), addresses, results), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(l)
	}()
	defer func() {
		_ = srv.Close()
		<-served
	}()

	opened := false
	if !*byURL && r.browser() {
		opened = r.env.OpenBrowser(login.URL) == nil
	}
	if opened {
		r.notef("opened your browser to sign in at %s; finish there (waiting up to 10 minutes)", esc(name))
	} else {
		r.notef("open this URL in a browser on any machine and sign in at %s:\n\n  %s\n\n"+
			"your browser will then fail to open a page at http://127.0.0.1/…; that is expected:\n"+
			"copy the whole address from its address bar and paste it here (waiting up to 10 minutes)", esc(name), login.URL)
		go pasted(r.env.Stdin, addresses)
	}
	var address string
	select {
	case address = <-addresses:
	case <-ctx.Done():
		return proto.Errf(proto.CodeUnavailable, "run `aries login` again, and finish signing in within 10 minutes", "the sign-in did not come back")
	}
	flow := "loopback"
	if !opened {
		flow = "url"
	}
	s, err := login.Finish(ctx, address, machineName(r), flow)
	results <- err
	if err != nil {
		return err
	}
	return r.keepSignIn(ctx, cfg, data, s)
}

// keepSignIn keeps s in data, in place of this machine's last sign-in to
// cfg's server, which it ends at the server, and reports it.
func (r *runner) keepSignIn(ctx context.Context, cfg *client.Config, data string, s *client.SignIn) error {
	if old, _ := client.LoadSignIn(data, cfg.Server.URL); old != nil {
		// A new sign-in replaces this machine's last: end it at the
		// server, so that its token is good for nothing.
		_ = client.Logout(ctx, cfg, old)
	}
	if err := client.SaveSignIn(data, s); err != nil {
		return err
	}
	out := signedIn{Principal: s.Principal, Provider: s.Provider, Server: cfg.Server.URL, ExpiresAt: s.ExpiresAt, SignIn: s.Grant, Scopes: s.Scopes,
		TrustRule: s.TrustRule}
	if r.json {
		r.emit(out)
		return nil
	}
	by := ""
	if out.TrustRule != "" {
		by = " by trust rule " + esc(out.TrustRule)
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "signed in as %s%s at %s, until %s (sign-in %s)\n", esc(out.Principal), by, esc(out.Provider), when(out.ExpiresAt), esc(out.SignIn))
	return nil
}

// chooseProvider is the provider named, or the server's one.
func chooseProvider(info *client.ServerInfo, named string) (string, error) {
	var names []string
	for _, p := range info.Providers {
		names = append(names, p.Name)
		if p.Name == named {
			return named, nil
		}
	}
	switch {
	case len(names) == 0:
		return "", proto.Errf(proto.CodeInvalid, "connect with a key, as `aries whoami` shows; or ask the server's operator to set up sign-in (login: providers)",
			"this server signs no one in at a provider")
	case named != "":
		return "", usagef(loginUsage, "the server has no provider %s; it has %s", named, strings.Join(names, ", "))
	case len(names) > 1:
		return "", usagef(loginUsage, "the server has several providers (%s); choose one with --provider", strings.Join(names, ", "))
	}
	return names[0], nil
}

// loopbackHandler takes the browser's return on callbackPath, once:
// it hands the address to the waiting login, and answers the browser
// with how it went, in plain text. Anything on this machine can reach
// the port, so a request without the sign-in's state is refused and
// leaves the login waiting, rather than spoiling it.
func loopbackHandler(state string, addresses chan<- string, results <-chan error) http.Handler {
	mux := http.NewServeMux()
	taken := make(chan struct{}, 1)
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Query().Get("state") != state {
			http.Error(w, "This is not the return of aries's sign-in; aries is still waiting for it.", http.StatusBadRequest)
			return
		}
		select {
		case taken <- struct{}{}:
		default:
			http.Error(w, "aries has this sign-in already; return to your terminal.", http.StatusConflict)
			return
		}
		addresses <- "http://" + r.Host + r.URL.RequestURI()
		select {
		case err := <-results:
			if err != nil {
				w.WriteHeader(http.StatusForbidden)
				_, _ = fmt.Fprintln(w, "The sign-in was refused; aries says why in your terminal.")
				return
			}
			_, _ = fmt.Fprintln(w, "You are signed in. You can close this tab and return to your terminal.")
		case <-time.After(30 * time.Second):
			_, _ = fmt.Fprintln(w, "aries has the sign-in; return to your terminal.")
		case <-r.Context().Done():
		}
	})
	return mux
}

// pasted reads lines from in, and hands the first that looks like an
// address to addresses.
func pasted(in io.Reader, addresses chan<- string) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "http://") {
			addresses <- line
			return
		}
	}
}

// browser reports whether a browser can be opened on this machine:
// macOS and Windows outside an SSH session, and Linux with a display
// (transport.md §6.2, identity.md §6.2).
func (r *runner) browser() bool {
	if r.env.OpenBrowser == nil || r.env.Getenv("SSH_CONNECTION") != "" {
		return false
	}
	switch r.env.GOOS {
	case "darwin", "windows":
		return true
	}
	return r.env.Getenv("DISPLAY") != "" || r.env.Getenv("WAYLAND_DISPLAY") != ""
}

// OpenBrowser returns what opens a URL in the system's browser on goos,
// waiting at most 10 seconds for the opener to hand it over: what
// cmd/aries gives Env.OpenBrowser.
func OpenBrowser(goos string) func(u string) error {
	return func(u string) error {
		// The URL is the server's, from .aries.yaml, which a repository
		// commits: an opener takes nothing but an https URL, never a path
		// or what reads as an option.
		if !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("not an https URL: %q", u)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var cmd *exec.Cmd
		switch goos {
		case "darwin":
			cmd = exec.CommandContext(ctx, "open", u) //nolint:gosec // an https URL, checked above
		case "windows":
			cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", u) //nolint:gosec // an https URL, checked above
		default:
			cmd = exec.CommandContext(ctx, "xdg-open", u) //nolint:gosec // an https URL, checked above
		}
		return cmd.Run()
	}
}

// machineName is what this machine is called to the server.
func machineName(r *runner) string {
	host, err := r.env.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}

// cmdLogout ends the sign-in kept for the server at the server, which
// closes its connections, and removes it from this machine.
func cmdLogout(ctx context.Context, r *runner, args []string) error {
	const usage = "logout"
	fs := r.newFlags("logout")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "logout takes no arguments")
	}
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		return err
	}
	data, err := offline.DataDir(r.env.GOOS, r.env.Getenv, r.env.UserHomeDir)
	if err != nil {
		return err
	}
	s, err := client.LoadSignIn(data, cfg.Server.URL)
	if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeAuth {
		// A sign-in that cannot be read cannot be ended at the server;
		// removing it is all that is left.
		s, err = nil, client.RemoveSignIn(data, cfg.Server.URL)
	}
	if err != nil {
		return err
	}
	type out struct {
		Server  string `json:"server"`
		Ended   bool   `json:"ended"`
		Removed bool   `json:"removed"`
	}
	if s == nil {
		if r.json {
			r.emit(out{Server: cfg.Server.URL})
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "no sign-in is kept here for %s\n", esc(cfg.Server.URL))
		return nil
	}
	ended := client.Logout(ctx, cfg, s)
	if err := client.RemoveSignIn(data, cfg.Server.URL); err != nil {
		return err
	}
	if ended != nil {
		r.notef("the sign-in was removed from this machine, and the server could not be told (%v): its token stays good until %s; an admin can end it now with `aries admin signin end %s`",
			ended, when(s.ExpiresAt), esc(s.Grant))
	}
	if r.json {
		r.emit(out{Server: cfg.Server.URL, Ended: ended == nil, Removed: true})
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "signed out of %s (sign-in %s)\n", esc(cfg.Server.URL), esc(s.Grant))
	return nil
}
