package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/spike/https/dpop"
	"github.com/ariesworx/aries/spike/https/ws"
)

// stored is what `aries login` keeps in the data directory, mode 0600.
type stored struct {
	Server      string    `json:"server"`
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	Expires     time.Time `json:"expires"`
	Scope       string    `json:"scope"`
}

// signIn is `aries login` by the loopback flow: a key for DPoP, PKCE, a
// listener on 127.0.0.1, the stand-in browser through the server and the
// provider, then the code exchanged with a proof of the key.
func signIn(ctx context.Context, base string, tc *tls.Config, user, pass string, key *ecdsa.PrivateKey) (*stored, *browser, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	defer ln.Close()
	type result struct{ code, state, iss string }
	got := make(chan result, 1)
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			select {
			case got <- result{q.Get("code"), q.Get("state"), q.Get("iss")}:
			default:
			}
			_, _ = io.WriteString(w, "Signed in to Aries. You can close this tab.")
		}))
	}()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	verifier := rand.Text() + rand.Text()
	ch := sha256.Sum256([]byte(verifier))
	state := rand.Text()
	u := base + "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {builtinCLI}, "redirect_uri": {redirect}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(ch[:])}, "code_challenge_method": {"S256"},
		"scope": {"cli mcp"}, "resource": {base},
	}.Encode()
	bu, _ := url.Parse(base)
	b := newBrowser(tc, user, pass, bu.Host, "keycloak.internal:8443")
	_, status, body, err := b.open(ctx, u)
	if err != nil {
		return nil, b, err
	}
	var r result
	select {
	case r = <-got:
	case <-time.After(2 * time.Second):
		return nil, b, fmt.Errorf("no redirect reached the listener; the last page was %d: %.200s", status, strings.TrimSpace(body))
	}
	if r.state != state {
		return nil, b, errors.New("state does not match: an address from another sign-in")
	}
	if r.iss != base {
		return nil, b, fmt.Errorf("iss %q is not the server (RFC 9207)", r.iss)
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 15 * time.Second}
	proof, err := dpop.Proof(key, http.MethodPost, base+"/oauth/token", "", time.Now())
	if err != nil {
		return nil, b, err
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
		Refresh     string `json:"refresh_token"`
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {r.code}, "redirect_uri": {redirect}, "client_id": {builtinCLI}, "code_verifier": {verifier}}
	if err := postForm(ctx, hc, base+"/oauth/token", form, http.Header{"Dpop": {proof}}, &tr); err != nil {
		return nil, b, err
	}
	if tr.Refresh != "" {
		return nil, b, errors.New("the server gave aries a refresh token, which decision 58 rules out")
	}
	return &stored{Server: base, AccessToken: tr.AccessToken, TokenType: tr.TokenType, Expires: time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second), Scope: tr.Scope}, b, nil
}

// conn is the client's side of one pipe connection, in Aries's frames.
type conn struct {
	c    *ws.Conn
	next uint64
	evts chan *proto.Frame
	res  chan *proto.Frame
	err  chan error
}

// dialWS opens /v1/connect with the token and a fresh proof, and says
// hello. A refusal comes back as the upgrade's status and body.
func dialWS(ctx context.Context, base string, tc *tls.Config, st *stored, key *ecdsa.PrivateKey) (*conn, *welcome, error) {
	u := base + "/v1/connect"
	h := http.Header{}
	if st != nil {
		h.Set("Authorization", st.TokenType+" "+st.AccessToken)
		if key != nil {
			p, err := dpop.Proof(key, http.MethodGet, u, st.AccessToken, time.Now())
			if err != nil {
				return nil, nil, err
			}
			h.Set("DPoP", p)
		}
	}
	return dialWSHeader(ctx, u, tc, h)
}

type welcome struct{ Principal, Session string }

func dialWSHeader(ctx context.Context, u string, tc *tls.Config, h http.Header) (*conn, *welcome, error) {
	c, resp, err := ws.Dial(ctx, "wss"+strings.TrimPrefix(u, "https"), tc, h)
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			return nil, nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		return nil, nil, err
	}
	cn := &conn{c: c, evts: make(chan *proto.Frame, 1024), res: make(chan *proto.Frame, 16), err: make(chan error, 1)}
	b, _ := json.Marshal(&proto.Frame{T: proto.FrameHello, Version: "spike", Proto: 8, Project: "00000000-0000-4000-8000-000000000001", Machine: "spike-client"})
	if err := c.Write(b); err != nil {
		return nil, nil, err
	}
	m, err := c.Read()
	if err != nil {
		return nil, nil, err
	}
	var w proto.Frame
	if err := json.Unmarshal(m, &w); err != nil || w.T != proto.FrameWelcome {
		return nil, nil, fmt.Errorf("no welcome: %s", m)
	}
	go cn.loop()
	return cn, &welcome{Principal: w.Principal, Session: w.Session}, nil
}

func (cn *conn) loop() {
	for {
		m, err := cn.c.Read()
		if err != nil {
			cn.err <- err
			close(cn.evts)
			return
		}
		var f proto.Frame
		if json.Unmarshal(m, &f) != nil {
			continue
		}
		if f.T == proto.FrameEvent {
			select {
			case cn.evts <- &f:
			default:
			}
			continue
		}
		cn.res <- &f
	}
}

func (cn *conn) call(op string, args any) (*proto.Frame, error) {
	cn.next++
	f := &proto.Frame{T: proto.FrameReq, ID: cn.next, Op: op}
	if args != nil {
		f.Args = mustJSON(args)
	}
	b, _ := json.Marshal(f)
	if err := cn.c.Write(b); err != nil {
		return nil, err
	}
	select {
	case r := <-cn.res:
		return r, nil
	case err := <-cn.err:
		return nil, err
	case <-time.After(10 * time.Second):
		return nil, errors.New("no answer in 10 s")
	}
}

func saveKey(path string, key *ecdsa.PrivateKey) error {
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), 0o600)
}

func loadKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("key: not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("key: not ECDSA")
	}
	return ek, nil
}

func loadStored(dir string) (*stored, *ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(filepath.Join(dir, "token.json"))
	if err != nil {
		return nil, nil, err
	}
	var st stored
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, nil, err
	}
	k, err := loadKey(filepath.Join(dir, "dpop.key"))
	return &st, k, err
}

// check prints one line per expectation and counts the failures.
type check struct{ failed int }

func (c *check) expect(name string, ok bool, detail string) {
	mark := "ok  "
	if !ok {
		mark = "FAIL"
		c.failed++
	}
	fmt.Printf("  %s %s: %s\n", mark, name, detail)
}

func (c *check) refused(name string, err error) {
	detail := "admitted"
	if err != nil {
		detail = "refused: " + err.Error()
	}
	c.expect(name, err != nil, detail)
}

// cmdLogin signs alice in and holds the server to what transport.md §5
// says it does with the token.
func cmdLogin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	base := fs.String("aries", "https://aries.internal:8443", "the server")
	ca := fs.String("ca", "", "the internal CA")
	state := fs.String("state", "", "the spike's state directory")
	user := fs.String("user", "alice", "who signs in")
	expiry := fs.Bool("expiry", false, "against a server whose sign-in lasts seconds: hold a connection past the end")
	only := fs.Bool("only", false, "sign in and store the token; check nothing (the checks end by revoking it)")
	_ = fs.Parse(args)
	tc, err := clientTLS(*ca, "")
	if err != nil {
		return err
	}
	pass, err := readSecret(filepath.Join(*state, *user+".pass"))
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	t0 := time.Now()
	st, b, err := signIn(ctx, *base, tc, *user, pass, key)
	var c check
	if *user != "alice" {
		c.refused(*user+", who has no invitation, signs in", err)
		return failures(c)
	}
	if err != nil {
		return fmt.Errorf("sign-in: %w", err)
	}
	fmt.Printf("login: %s signed in in %s; provider pages %q; token %s, %d characters, scope %q, ends in %s; no refresh token\n",
		*user, time.Since(t0).Round(time.Millisecond), b.Pages, st.TokenType, len(st.AccessToken), st.Scope, time.Until(st.Expires).Round(time.Second))
	dir := filepath.Join(*state, "signin")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sb, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(dir, "token.json"), sb, 0o600); err != nil {
		return err
	}
	if err := saveKey(filepath.Join(dir, "dpop.key"), key); err != nil {
		return err
	}
	if *expiry {
		return holdPastEnd(ctx, *base, tc, st, key)
	}
	if *only {
		return nil
	}

	cn, w, err := dialWS(ctx, *base, tc, st, key)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	r, err := cn.call("whoami", nil)
	c.expect("the token and its proof connect", err == nil && w.Principal == *user, fmt.Sprintf("welcome principal %q, whoami %s", w.Principal, okOf(r)))
	r, err = cn.call("gate.pass", nil)
	c.expect("a cli token may pass a gate", err == nil && r.Err == nil, okOf(r))
	_ = cn.c.Close()

	_, _, err = dialWS(ctx, *base, tc, nil, nil)
	c.refused("no token", err)
	_, _, err = dialWS(ctx, *base, tc, &stored{TokenType: "Bearer", AccessToken: st.AccessToken}, nil)
	c.refused("the token as a bearer token, with no proof", err)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, _, err = dialWS(ctx, *base, tc, st, other)
	c.refused("the token with a proof from another key (copied to another machine)", err)
	proof, _ := dpop.Proof(key, http.MethodGet, *base+"/v1/connect", st.AccessToken, time.Now())
	h := http.Header{"Authorization": {"DPoP " + st.AccessToken}, "Dpop": {proof}}
	if cn, _, err := dialWSHeader(ctx, *base+"/v1/connect", tc, h); err == nil {
		_ = cn.c.Close()
	}
	_, _, err = dialWSHeader(ctx, *base+"/v1/connect", tc, h)
	c.refused("the same proof again (a replay)", err)
	_, _, err = dialWS(ctx, *base, tc, &stored{TokenType: "DPoP", AccessToken: "x" + st.AccessToken[1:]}, key)
	c.refused("a made-up token", err)

	// The token is for the pipes; at /mcp it is another resource's.
	mp, _ := dpop.Proof(key, http.MethodPost, *base+"/mcp", st.AccessToken, time.Now())
	status := post(ctx, tc, *base+"/mcp", http.Header{"Authorization": {"DPoP " + st.AccessToken}, "Dpop": {mp}, "Content-Type": {"application/json"}})
	c.expect("the cli token at /mcp", status == 401, fmt.Sprintf("status %d", status))

	// Smart cards: the internal CA's client certificate, checked against
	// its CRL (software certificates standing in for PIV cards).
	for _, who := range []string{"alice", "mallory"} {
		ctc, err := clientTLS(*ca, filepath.Join(filepath.Dir(*ca), "card-"+who))
		if err != nil {
			return err
		}
		cn, w, err := dialWSHeader(ctx, *base+"/v1/connect", ctc, nil)
		if who == "alice" {
			c.expect("alice's card", err == nil && w.Principal == "alice", fmt.Sprintf("%v %v", w, err))
			if cn != nil {
				_ = cn.c.Close()
			}
		} else {
			c.refused("mallory's card, revoked in the CRL", err)
		}
	}

	// Revocation closes a live connection (decision 59).
	cn, _, err = dialWS(ctx, *base, tc, st, key)
	if err != nil {
		return err
	}
	t1 := time.Now()
	_ = postFormStatus(ctx, tc, *base+"/oauth/revoke", url.Values{"token": {st.AccessToken}})
	select {
	case f := <-cn.evts:
		c.expect("revoking the token closes its connection", f != nil && f.Op == "closed", fmt.Sprintf("closed %s after the revocation, with %s", time.Since(t1).Round(time.Millisecond), eOf(f)))
	case <-time.After(5 * time.Second):
		c.expect("revoking the token closes its connection", false, "still open after 5 s")
	}
	_, _, err = dialWS(ctx, *base, tc, st, key)
	c.refused("the revoked token", err)
	return failures(c)
}

// holdPastEnd keeps a connection open past the sign-in's end, which the
// server under test sets to seconds, and says when it was closed.
func holdPastEnd(ctx context.Context, base string, tc *tls.Config, st *stored, key *ecdsa.PrivateKey) error {
	var c check
	cn, _, err := dialWS(ctx, base, tc, st, key)
	if err != nil {
		return err
	}
	// The sign-in's end, to the nanosecond, as the server holds it.
	r, err := cn.call("whoami", nil)
	if err != nil || r.Err != nil {
		return fmt.Errorf("whoami: %v", err)
	}
	var who struct{ Ends time.Time }
	if err := json.Unmarshal(r.OK, &who); err != nil {
		return err
	}
	st.Expires = who.Ends
	select {
	case f := <-cn.evts:
		late := time.Since(st.Expires)
		c.expect("the connection is closed at the sign-in's end", f != nil && f.Op == "closed" && late > -2*time.Second && late < 2*time.Second,
			fmt.Sprintf("closed %s after the sign-in's end, with %s", late.Round(time.Millisecond), eOf(f)))
	case <-time.After(time.Until(st.Expires) + 10*time.Second):
		c.expect("the connection is closed at the sign-in's end", false, "still open 10 s after")
	}
	_, _, err = dialWS(ctx, base, tc, st, key)
	c.refused("the token after its end", err)
	return failures(c)
}

func failures(c check) error {
	if c.failed > 0 {
		return fmt.Errorf("%d checks failed", c.failed)
	}
	return nil
}

func okOf(f *proto.Frame) string {
	if f == nil {
		return "no answer"
	}
	if f.Err != nil {
		return "error " + string(f.Err.Code) + ": " + f.Err.Message
	}
	return string(f.OK)
}

func eOf(f *proto.Frame) string {
	if f == nil {
		return "nothing"
	}
	return string(f.E)
}

func post(ctx context.Context, tc *tls.Config, u string, h http.Header) int {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header = h
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: tc}}).Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

func postFormStatus(ctx context.Context, tc *tls.Config, u string, form url.Values) int {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: tc}}).Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	return resp.StatusCode
}

// cmdWorkload: a CI job's token (here Keycloak's client-credentials
// token, shaped like GitHub Actions') exchanged for an Aries token
// bound to the job's own key, then used.
func cmdWorkload(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("workload", flag.ExitOnError)
	base := fs.String("aries", "https://aries.internal:8443", "the server")
	kc := fs.String("issuer", "https://keycloak.internal:8443/realms/"+realm, "the platform's issuer")
	ca := fs.String("ca", "", "the internal CA")
	state := fs.String("state", "", "the spike's state directory")
	_ = fs.Parse(args)
	tc, err := clientTLS(*ca, "")
	if err != nil {
		return err
	}
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 15 * time.Second}
	secret, err := readSecret(filepath.Join(*state, "ci-example.secret"))
	if err != nil {
		return err
	}
	var wt struct {
		AccessToken string `json:"access_token"`
	}
	if err := postForm(ctx, hc, *kc+"/protocol/openid-connect/token", url.Values{"grant_type": {"client_credentials"}, "client_id": {"ci-example"}, "client_secret": {secret}}, nil, &wt); err != nil {
		return fmt.Errorf("the platform's token: %w", err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	proof, _ := dpop.Proof(key, http.MethodPost, *base+"/oauth/token", "", time.Now())
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:token-exchange"}, "subject_token": {wt.AccessToken},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"}, "audience": {*base}}
	var c check
	err = postForm(ctx, hc, *base+"/oauth/token", form, http.Header{"Dpop": {proof}}, &tr)
	c.expect("the workload token is exchanged", err == nil, fmt.Sprintf("%s token, scope %q, %d s; err %v", tr.TokenType, tr.Scope, tr.ExpiresIn, err))
	if err != nil {
		return failures(c)
	}
	st := &stored{TokenType: tr.TokenType, AccessToken: tr.AccessToken}
	cn, w, err := dialWS(ctx, *base, tc, st, key)
	if err != nil {
		return err
	}
	c.expect("the job connects as its service principal", w.Principal == "ci-example", "principal "+w.Principal)
	r, _ := cn.call("gate.pass", nil)
	c.expect("a ci token may not pass a gate", r != nil && r.Err != nil, okOf(r))
	_ = cn.c.Close()
	err = postForm(ctx, hc, *base+"/oauth/token", form, http.Header{"Dpop": {proof}}, &tr)
	c.refused("the same DPoP proof at the token endpoint again", err)
	return failures(c)
}

// cmdEgress tries the internet and fails if anything answers.
func cmdEgress(ctx context.Context, _ []string) error {
	var reached []string
	d := net.Dialer{Timeout: 3 * time.Second}
	for _, a := range []string{"1.1.1.1:443", "8.8.8.8:53", "[2606:4700:4700::1111]:443"} {
		if c, err := d.DialContext(ctx, "tcp", a); err == nil {
			reached = append(reached, a)
			c.Close()
		}
	}
	r := net.Resolver{}
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if addrs, err := r.LookupHost(lctx, "example.com"); err == nil && len(addrs) > 0 {
		reached = append(reached, "DNS for example.com")
	}
	if len(reached) > 0 {
		return fmt.Errorf("reached the internet: %v", reached)
	}
	fmt.Println("egress: no route out: 1.1.1.1:443, 8.8.8.8:53 and 2606:4700:4700::1111 unreachable, example.com does not resolve")
	return nil
}
