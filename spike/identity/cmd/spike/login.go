package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/spike/identity/jose"
	"github.com/ariesworx/aries/spike/identity/oidc"
)

// cmdLogin is `aries login` and the server's enrollment, in one process.
//
//	-mode loopback  authorization code + PKCE + nonce, redirect to a
//	                listener on 127.0.0.1 (a browser on this machine)
//	-mode paste     the same request, redirect to 127.0.0.1 where nothing
//	                listens; the person copies the address the browser
//	                could not open and pastes it (a browser elsewhere)
//	-mode oob       the same request, redirect to the provider's own page
//	                that shows the code, which the person pastes
//	-mode device    the device authorization grant (RFC 8628)
func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	issuer := fs.String("issuer", "http://127.0.0.1:18080/realms/"+realm, "the provider's issuer")
	client := fs.String("client", "aries-cli", "the public client's id")
	user := fs.String("user", "alice", "who signs in at the provider (the stand-in browser)")
	state := fs.String("state", "", "the spike's state directory")
	mode := fs.String("mode", "loopback", "loopback, paste, oob or device")
	wrongVerifier := fs.Bool("wrong-verifier", false, "send a PKCE verifier that is not the one committed to")
	_ = fs.Parse(args)
	if *state == "" {
		return errors.New("-state is required")
	}
	pass, err := readSecret(filepath.Join(*state, *user+".pass"))
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	d, err := oidc.Discover(ctx, hc, *issuer)
	if err != nil {
		return err
	}
	v := oidc.New(oidc.Config{Issuer: *issuer, Audience: *client, IDToken: true, Skew: 30 * time.Second, MaxAge: 10 * time.Minute, Refetch: time.Minute},
		d, hc, time.Now)
	srv := newEnroller(v, time.Now)

	// The client makes its key, and asks the server for a challenge.
	pubRaw, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	pub, err := ssh.NewPublicKey(pubRaw)
	if err != nil {
		return err
	}
	challenge := srv.challenge()
	nonce := nonceFor(challenge, pub)
	au, _ := url.Parse(d.AuthorizationEndpoint)
	b := newBrowser(au.Host, *user, pass, *mode == "loopback")

	var token string
	bound := true
	start := time.Now()
	switch *mode {
	case "loopback", "paste", "oob":
		token, err = codeFlow(ctx, hc, d, *client, nonce, *mode, b, *wrongVerifier, nil)
	case "device":
		bound = false
		token, err = deviceFlow(ctx, hc, d, *client, nonce, b)
	default:
		return fmt.Errorf("unknown -mode %q", *mode)
	}
	if err != nil {
		return err
	}
	fmt.Printf("login %s: signed in at the provider in %s (%s)\n", *mode, time.Since(start).Round(time.Millisecond), b)
	describe(token, nonce)

	res, err := srv.enroll(ctx, token, pub, challenge, bound)
	if err != nil {
		return fmt.Errorf("enroll: refused: %w", err)
	}
	fmt.Printf("login %s: enrolled as %s (identity bound now: %v), key %s, expires %s\n",
		*mode, res.Principal, res.Bound, ssh.FingerprintSHA256(pub), res.Expires.UTC().Format(time.RFC3339))
	fmt.Printf("login %s: key line: %s\n", *mode, shorten(res.Line))

	// The same token again: a stolen token with the thief's own key, and
	// a replay of the same token with the same key.
	thiefRaw, _, _ := ed25519.GenerateKey(rand.Reader)
	thief, _ := ssh.NewPublicKey(thiefRaw)
	if _, err := srv.enroll(ctx, token, thief, srv.challenge(), bound); err != nil {
		fmt.Printf("login %s: the token with another key: refused: %v\n", *mode, err)
	} else {
		fmt.Printf("login %s: the token with another key: ACCEPTED\n", *mode)
	}
	if _, err := srv.enroll(ctx, token, pub, srv.challenge(), bound); err != nil {
		fmt.Printf("login %s: the token again, same key, new challenge: refused: %v\n", *mode, err)
	} else {
		fmt.Printf("login %s: the token again, same key, new challenge: ACCEPTED\n", *mode)
	}

	// Signing in again in the same browser, whose session at the provider
	// is open: what the person sees, and what auth_time says. This is
	// decision 61's "fresh sign-in", and the agent-drives-the-browser
	// threat.
	if *mode == "loopback" {
		for _, again := range []url.Values{{}, {"prompt": {"login"}}} {
			b.Pages = nil
			ch := srv.challenge()
			tok, err := codeFlow(ctx, hc, d, *client, nonceFor(ch, pub), *mode, b, false, again)
			if err != nil {
				fmt.Printf("login again %v: %v\n", again, err)
				continue
			}
			j, _ := jose.Parse(tok)
			var c oidc.Claims
			_ = json.Unmarshal(j.Payload, &c)
			r, err := srv.enroll(ctx, tok, pub, ch, true)
			who := "refused: " + fmt.Sprint(err)
			if err == nil {
				who = fmt.Sprintf("enrolled as %s, bound now %v", r.Principal, r.Bound)
			}
			fmt.Printf("login again %v: sign-in pages shown %d; auth_time %ds before iat; %s\n",
				again, len(b.Pages), c.Iat-c.AuthTime, who)
		}
	}

	// Keep the key and its line for the sshd step.
	block, err := ssh.MarshalPrivateKey(priv, "spike "+*mode)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*state, "login-"+*mode+".key"), pem.EncodeToMemory(block), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*state, "login-"+*mode+".line"), []byte(res.Line+"\n"), 0o600)
}

// codeFlow runs the authorization code grant with PKCE (S256), a state
// and the nonce, and returns the ID token.
func codeFlow(ctx context.Context, hc *http.Client, d *oidc.Discovery, client, nonce, mode string, b *browser, wrongVerifier bool, extra url.Values) (string, error) {
	verifier := rand.Text() + rand.Text() // 52 characters, within RFC 7636's 43 to 128
	sum := sha256.Sum256([]byte(verifier))
	st := rand.Text()

	var redirect string
	var got chan url.Values
	switch mode {
	case "loopback":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		defer ln.Close()
		redirect = "http://" + ln.Addr().String() + "/callback"
		got = make(chan url.Values, 1)
		srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			select {
			case got <- r.URL.Query():
			default:
			}
			fmt.Fprintln(w, "Signed in. You can close this tab and return to the terminal.")
		})}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()
	case "paste":
		// A port nothing listens on: the browser, on another machine,
		// shows that it cannot connect, with the code in its address bar.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", err
		}
		redirect = "http://" + ln.Addr().String() + "/callback"
		ln.Close()
	case "oob":
		redirect = "urn:ietf:wg:oauth:2.0:oob"
	}

	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {client},
		"redirect_uri":          {redirect},
		"scope":                 {"openid email profile"},
		"state":                 {st},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		// Decision 61: a fresh sign-in, whatever session the browser has.
		"max_age": {"600"},
	}
	for k, v := range extra {
		q[k] = v
	}
	authURL := d.AuthorizationEndpoint + "?" + q.Encode()
	fmt.Printf("login %s: the URL `aries login` prints is %d characters\n", mode, len(authURL))

	stoppedAt, page, err := b.open(ctx, authURL)
	if err != nil {
		return "", err
	}
	var code string
	switch mode {
	case "loopback":
		select {
		case v := <-got:
			if v.Get("state") != st {
				return "", errors.New("state does not match")
			}
			if e := v.Get("error"); e != "" {
				return "", fmt.Errorf("provider: %s: %s", e, v.Get("error_description"))
			}
			code = v.Get("code")
		case <-time.After(10 * time.Second):
			return "", errors.New("no redirect reached the listener")
		}
	case "paste":
		// What the person pastes: the whole address, so that the state
		// is checked too.
		fmt.Printf("login paste: the address to paste is %d characters, to %s\n", len(stoppedAt), hostOf(stoppedAt))
		pasted, err := url.Parse(strings.TrimSpace(stoppedAt))
		if err != nil {
			return "", err
		}
		if pasted.Query().Get("state") != st {
			return "", errors.New("the pasted address's state does not match: paste the address from this sign-in")
		}
		code = pasted.Query().Get("code")
	case "oob":
		code = oobCode(page)
		if code == "" {
			return "", fmt.Errorf("no code on the provider's page (stopped at %s)", hostOf(stoppedAt))
		}
		fmt.Printf("login oob: the code to paste is %d characters\n", len(code))
	}
	if code == "" {
		return "", errors.New("no code came back")
	}
	if wrongVerifier {
		verifier = rand.Text() + rand.Text()
	}
	var out struct {
		IDToken string `json:"id_token"`
	}
	err = postForm(ctx, hc, d.TokenEndpoint, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {client}, "code_verifier": {verifier},
	}, &out)
	if err != nil {
		return "", fmt.Errorf("token exchange: %w", err)
	}
	if out.IDToken == "" {
		return "", errors.New("no ID token in the answer")
	}
	return out.IDToken, nil
}

var oobRE = regexp.MustCompile(`(?s)id="code"[^>]*value="([^"]+)"|value="([^"]+)"[^>]*id="code"`)

func oobCode(page string) string {
	m := oobRE.FindStringSubmatch(page)
	if m == nil {
		return ""
	}
	return m[1] + m[2]
}

// deviceFlow runs RFC 8628 with a nonce in the request, to see whether
// the provider puts it in the ID token.
func deviceFlow(ctx context.Context, hc *http.Client, d *oidc.Discovery, client, nonce string, b *browser) (string, error) {
	if d.DeviceAuthorizationEndpoint == "" {
		return "", errors.New("the provider has no device authorization endpoint")
	}
	var da struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	// Keycloak applies the client's PKCE requirement to this grant too:
	// without a challenge it answers "Missing parameter:
	// code_challenge_method". RFC 8628 has no PKCE; the verifier then goes
	// with each poll.
	verifier := rand.Text() + rand.Text()
	sum := sha256.Sum256([]byte(verifier))
	if err := postForm(ctx, hc, d.DeviceAuthorizationEndpoint, url.Values{
		"client_id": {client}, "scope": {"openid email profile"}, "nonce": {nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}, &da); err != nil {
		return "", fmt.Errorf("device authorization: %w", err)
	}
	fmt.Printf("login device: user code %d characters, verification_uri %s, complete URI %v, expires in %ds, interval %ds\n",
		len(da.UserCode), da.VerificationURI, da.VerificationURIComplete != "", da.ExpiresIn, da.Interval)
	poll := func() (string, error) {
		var out struct {
			IDToken string `json:"id_token"`
		}
		err := postForm(ctx, hc, d.TokenEndpoint, url.Values{
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {da.DeviceCode}, "client_id": {client},
			"code_verifier": {verifier},
		}, &out)
		return out.IDToken, err
	}
	if _, err := poll(); err != nil {
		fmt.Printf("login device: first poll before sign-in: %v\n", err)
	}
	if _, _, err := b.open(ctx, da.VerificationURIComplete); err != nil {
		return "", err
	}
	// Poll at the interval the provider gave, slower when told to
	// (RFC 8628 §3.5).
	interval := time.Duration(max(da.Interval, 1)) * time.Second
	var tok string
	for range 20 {
		time.Sleep(interval)
		t, err := poll()
		var oe *oauthError
		switch {
		case err == nil:
			tok = t
		case errors.As(err, &oe) && oe.Code == "authorization_pending":
			continue
		case errors.As(err, &oe) && oe.Code == "slow_down":
			interval += 5 * time.Second
			continue
		default:
			return "", fmt.Errorf("poll after sign-in: %w", err)
		}
		break
	}
	if tok == "" {
		return "", errors.New("no token after polling")
	}
	time.Sleep(interval)
	if _, err := poll(); err != nil {
		fmt.Printf("login device: a second poll with the same device code: refused: %v\n", err)
	} else {
		fmt.Printf("login device: a second poll with the same device code: TOKENS AGAIN\n")
	}
	return tok, nil
}

// describe prints the token's header and the claims that matter, never
// the token itself.
func describe(token, nonce string) {
	j, err := jose.Parse(token)
	if err != nil {
		fmt.Printf("  token: %v\n", err)
		return
	}
	var c oidc.Claims
	_ = json.Unmarshal(j.Payload, &c)
	_ = json.Unmarshal(j.Payload, &c.All)
	fmt.Printf("  token: alg %s, %d bytes; iss %s; aud %v; azp %q; lifetime %s; auth_time present %v; nonce returned %v, equal %v; email %v verified %v\n",
		j.Header.Alg, len(token), c.Iss, []string(c.Aud), c.Azp, time.Duration(c.Exp-c.Iat)*time.Second,
		c.AuthTime != 0, c.Nonce != "", c.Nonce == nonce, c.All["email"], c.All["email_verified"])
}

func shorten(line string) string {
	if i := strings.LastIndex(line, " "); i > 0 && len(line)-i > 24 {
		return line[:i+1] + line[i+1:i+17] + "…"
	}
	return line
}

func hostOf(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "?"
	}
	if p.Host == "" {
		return p.Scheme + ":" + p.Opaque
	}
	return p.Scheme + "://" + p.Host + p.Path
}
