package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// cmdMCP is a remote MCP client, the Go SDK's, going through the MCP
// authorization flow against the server as a connector would: a 401
// pointing at the protected resource metadata, the authorization
// server's metadata, dynamic client registration, PKCE with S256 and the
// resource indicator, the stand-in browser through the provider, and
// then a tool call with the bearer token.
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	base := fs.String("aries", "https://aries.internal:8443", "the server")
	ca := fs.String("ca", "", "the internal CA")
	state := fs.String("state", "", "the spike's state directory")
	_ = fs.Parse(args)
	tc, err := clientTLS(*ca, "")
	if err != nil {
		return err
	}
	pass, err := readSecret(filepath.Join(*state, "alice.pass"))
	if err != nil {
		return err
	}
	// The Go SDK refuses, by default, to fetch metadata from a private
	// address (its SSRF guard), which is every address on an internal
	// network. A dialer of our own opts out (RESULTS.md, section 5).
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}, Timeout: 15 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	got := make(chan *auth.AuthorizationResult, 1)
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			select {
			case got <- &auth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}:
			default:
			}
			_, _ = io.WriteString(w, "Connected. You can close this tab.")
		}))
	}()
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	bu, _ := url.Parse(*base)
	b := newBrowser(tc, "alice", pass, bu.Host, "keycloak.internal:8443")
	var authURL string
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		RedirectURL: redirect,
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "spike connector", RedirectURIs: []string{redirect}, GrantTypes: []string{"authorization_code", "refresh_token"},
			TokenEndpointAuthMethod: "none",
		}},
		AuthorizationCodeFetcher: func(ctx context.Context, a *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			authURL = a.URL
			if _, status, body, err := b.open(ctx, a.URL); err != nil {
				return nil, err
			} else if status >= 400 {
				return nil, fmt.Errorf("browser stopped at %d: %.200s", status, body)
			}
			select {
			case r := <-got:
				return r, nil
			case <-time.After(5 * time.Second):
				return nil, errors.New("no redirect reached the listener")
			}
		},
		Client: hc,
	})
	if err != nil {
		return err
	}
	var c check
	t0 := time.Now()
	client := mcp.NewClient(&mcp.Implementation{Name: "spike-connector", Version: "0.0.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: *base + "/mcp", HTTPClient: hc, OAuthHandler: h}, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	if err != nil {
		return fmt.Errorf("whoami: %w", err)
	}
	out, _ := json.Marshal(res.StructuredContent)
	au, _ := url.Parse(authURL)
	q := au.Query()
	c.expect("the MCP authorization flow ends in a tool call as alice", strings.Contains(string(out), `"principal":"alice"`),
		fmt.Sprintf("in %s; client %s registered; PKCE %s; resource %q; provider pages %q; whoami %s",
			time.Since(t0).Round(time.Millisecond), q.Get("client_id"), q.Get("code_challenge_method"), q.Get("resource"), b.Pages, out))
	res2, _ := sess.CallTool(ctx, &mcp.CallToolParams{Name: "whoami", Arguments: map[string]any{}})
	out2, _ := json.Marshal(res2.StructuredContent)
	var w1, w2 whoamiOut
	_ = json.Unmarshal(out, &w1)
	_ = json.Unmarshal(out2, &w2)
	c.expect("two stateless calls name the same derived session", w1.Session != "" && w1.Session == w2.Session, w1.Session+" and "+w2.Session)

	ts, _ := h.TokenSource(ctx)
	tok, err := ts.Token()
	if err != nil {
		return err
	}
	c.expect("the connector's token", tok.RefreshToken != "" && tok.TokenType == "Bearer",
		fmt.Sprintf("%s, ends in %s, with a refresh token that ends with the sign-in", tok.TokenType, time.Until(tok.Expiry).Round(time.Minute)))

	// What the endpoint refuses.
	resp := mcpPost(ctx, hc, *base+"/mcp", "", "")
	c.expect("no token at /mcp", resp.status == 401 && strings.Contains(resp.wwwAuth, "resource_metadata="), fmt.Sprintf("%d, WWW-Authenticate: %s", resp.status, resp.wwwAuth))
	resp = mcpPost(ctx, hc, *base+"/mcp", "Bearer "+tok.AccessToken, "https://evil.example.com")
	c.expect("a cross-site origin at /mcp", resp.status == 403, fmt.Sprintf("%d", resp.status))
	resp = mcpPost(ctx, hc, *base+"/mcp", "Bearer "+tok.AccessToken, "https://claude.ai")
	c.expect("Claude's origin at /mcp passes the origin and token checks", resp.status != 403 && resp.status != 401, fmt.Sprintf("%d %s", resp.status, resp.body))
	_, _, err = dialWSHeader(ctx, *base+"/v1/connect", tc, http.Header{"Authorization": {"Bearer " + tok.AccessToken}})
	c.refused("the connector's token at the pipe (/v1/connect)", err)
	return failures(c)
}

type mcpResp struct {
	status  int
	wwwAuth string
	body    string
}

func mcpPost(ctx context.Context, hc *http.Client, u, authz, origin string) mcpResp {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/list")
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return mcpResp{}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return mcpResp{status: resp.StatusCode, wwwAuth: resp.Header.Get("WWW-Authenticate"), body: strings.TrimSpace(string(b))}
}
