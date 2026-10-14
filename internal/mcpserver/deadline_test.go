package mcpserver

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
)

// A tool call whose request the server never answers holds the session
// for the client's deadline (client.RequestTimeout) and no longer: the
// connection is closed and dropped, the call is retried on a new one,
// and a renewal that waited behind it then goes out on that one.
func TestStalledCallLetsRenewalsThrough(t *testing.T) {
	// Serial: a synctest bubble's virtual time takes no real time worth sharing.
	synctest.Test(t, func(t *testing.T) {
		wait := client.RequestTimeout(proto.OpShow)
		// As a *client.Conn does with a request the server leaves
		// unanswered: it waits out the deadline, then fails, lost.
		stalled := &fakeConn{reply: func(string, any) (any, error) {
			time.Sleep(wait)
			return nil, errLost
		}}
		healthy := &fakeConn{reply: func(op string, _ any) (any, error) {
			if op == proto.OpRenew {
				return proto.ClaimsResult{}, nil
			}
			return proto.ShowResult{Issue: proto.Issue{ID: "sf-1", Title: "t"}}, nil
		}}
		cs, d, s := connectServer(t, stalled, healthy)
		start := time.Now()
		shown := make(chan *mcp.CallToolResult, 1)
		go func() {
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "show", Arguments: map[string]any{"id": "sf-1"}})
			if err != nil {
				res = toolError(err)
			}
			shown <- res
		}()
		synctest.Wait() // the call holds the session, stalled
		renewed := make(chan time.Duration, 1)
		go func() {
			s.Renew(t.Context())
			renewed <- time.Since(start)
		}()
		res := <-shown
		if took := time.Since(start); res.IsError || took != wait {
			t.Errorf("show on a stalled connection = %s after %s, want a result after the deadline, %s", text(t, res), took, wait)
		}
		if took := <-renewed; took != wait {
			t.Errorf("the renewal behind the stalled call returned after %s, want %s", took, wait)
		}
		if got, want := healthy.ops(), []string{proto.OpShow, proto.OpRenew}; !slices.Equal(got, want) {
			t.Errorf("the new connection carried %v, want %v", got, want)
		}
		stalled.mu.Lock()
		closes := stalled.closes
		stalled.mu.Unlock()
		if closes != 1 || d.n != 2 {
			t.Errorf("the stalled connection was closed %d times, with %d dials; want once, and one dial more", closes, d.n)
		}
	})
}
