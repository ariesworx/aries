package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The remote MCP endpoint (transport.md §7): Streamable HTTP, stateless
// as MCP 2026-07-28 has it, behind a bearer token of scope mcp from the
// server's own authorization server. One tool stands in for Aries's.

type whoamiOut struct {
	Principal string   `json:"principal"`
	Scopes    []string `json:"scopes"`
	Session   string   `json:"session"`
	Ends      string   `json:"ends"`
}

func (s *server) mcpHandler() http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "aries-spike", Version: "0.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "whoami", Description: "Who this connection is, and its session"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, whoamiOut, error) {
			ti := req.Extra.TokenInfo
			g, _ := ti.Extra["grant"].(*grant)
			return nil, whoamiOut{Principal: ti.UserID, Scopes: ti.Scopes, Session: connectorSession(g, req.Extra.Header), Ends: g.end.UTC().Format(time.RFC3339)}, nil
		})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	verify := func(ctx context.Context, tok string, r *http.Request) (*auth.TokenInfo, error) {
		g, e := s.authenticate(r, s.base+"/mcp")
		if e != nil {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{Scopes: g.scopes, Expiration: g.end, UserID: g.principal, Extra: map[string]any{"grant": g}}, nil
	}
	protected := auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.base + "/.well-known/oauth-protected-resource/mcp",
		Scopes:              []string{scopeMCP},
	})(h)
	return s.origins(protected)
}

// connectorSession is what Aries calls a remote connector's session: MCP
// 2026-07-28 has none, so the server derives one from the grant (a
// person's sign-in through one client) and the client's name, stable
// across the conversation's requests (transport.md §7.4).
func connectorSession(g *grant, h http.Header) string {
	sum := sha256.Sum256([]byte(g.id + "\x00" + h.Get("User-Agent")))
	return "mcp-" + hex.EncodeToString(sum[:4])
}

// origins refuses a browser's request from an origin that is not the
// server's own or a configured client's (the Streamable HTTP
// transport's DNS rebinding rule). A request with no Origin, which no
// browser sends cross-site, is a non-browser client and passes to the
// token check.
func (s *server) origins(next http.Handler) http.Handler {
	allowed := []string{s.base, "https://claude.ai"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What a connector sends, for the maintainer's run with Claude
		// (RESULTS.md, section 8): never the token, only whether there is
		// one, and whether a DPoP proof came with it.
		log.Printf("mcp: %s %s from %s; protocol %q; method %q; agent %q; origin %q; authorization %v; dpop %v",
			r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("Mcp-Protocol-Version"), r.Header.Get("Mcp-Method"),
			r.Header.Get("User-Agent"), r.Header.Get("Origin"), r.Header.Get("Authorization") != "", r.Header.Get("DPoP") != "")
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || !slices.Contains(allowed, strings.ToLower(u.Scheme+"://"+u.Host)) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
