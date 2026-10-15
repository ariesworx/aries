package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/aries/internal/proto"
)

// Benchmarks of what `aries mcp` does besides talk to the server
// (docs/design/profile-aries.md): building the tools at start, sending
// their schemas, and holding results under the token budget. Run with:
//
//	go test -run '^$' -bench Client -benchmem ./internal/mcpserver/

// benchConn answers list, ready and comments from what it holds, honoring
// the limit asked for, and counts the calls. It sets the result directly:
// decoding a frame is measured in internal/proto.
type benchConn struct {
	issues   []proto.Summary
	comments []proto.Comment
	calls    int
}

func (c *benchConn) Call(_ context.Context, op string, args, result any) error {
	c.calls++
	switch r := result.(type) {
	case *proto.ListResult:
		limit := len(c.issues)
		switch a := args.(type) {
		case proto.ListArgs:
			limit = a.Limit
		case proto.LimitArgs:
			limit = a.Limit
		}
		n := min(limit, len(c.issues))
		// A copy, as a decoded frame is: the tools cut titles in place.
		r.Issues = append([]proto.Summary(nil), c.issues[:n]...)
		if n < len(c.issues) && op == proto.OpList {
			r.Next = "cursor"
		}
	case *proto.CommentsResult:
		n := min(args.(proto.PageArgs).Limit, len(c.comments))
		r.Comments, r.Total = c.comments[len(c.comments)-n:], len(c.comments)
	}
	return nil
}

func (c *benchConn) Err() error              { return nil }
func (c *benchConn) Close() error            { return nil }
func (c *benchConn) Principal() string       { return "alice" }
func (c *benchConn) Session() string         { return "s-bench" }
func (c *benchConn) Project() string         { return "example" }
func (c *benchConn) Notices(string) []string { return nil }

func benchIssues(n int) []proto.Summary {
	out := make([]proto.Summary, n)
	for i := range out {
		out[i] = proto.Summary{ID: fmt.Sprintf("sf-%08d", i), Status: "open", Priority: i % 5,
			Title: fmt.Sprintf("Reconciler loses the first of two overlapping writes (case %d)", i)}
	}
	return out
}

// BenchmarkClientNew is what `aries mcp` builds before it reads its first
// message: the server and every tool's input schema, inferred by
// reflection.
func BenchmarkClientNew(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		New(Options{Version: "v0.2.0", RenewEvery: -1})
	}
}

// BenchmarkClientToolsList is one tools/list, from the request to the
// client's decoded reply, over the SDK's in-memory transport; schema-bytes
// is the size of the tool list as JSON, which a harness reads at every
// session's start.
func BenchmarkClientToolsList(b *testing.B) {
	s := New(Options{Version: "v0.2.0", RenewEvery: -1})
	ctx := b.Context()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(ctx, st, nil); err != nil {
		b.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "bench"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = cs.Close() })
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		b.Fatal(err)
	}
	doc, err := json.Marshal(res)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cs.ListTools(ctx, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(doc)), "schema-bytes")
	b.ReportMetric(float64(len(res.Tools)), "tools")
}

// BenchmarkClientSize is one estimate of a result's tokens: the result
// marshaled whole and its bytes counted.
func BenchmarkClientSize(b *testing.B) {
	for _, n := range []int{10, 50, 500} {
		b.Run(fmt.Sprintf("issues=%d", n), func(b *testing.B) {
			out := Issues{Issues: benchIssues(n)}
			b.ReportAllocs()
			for b.Loop() {
				size(out)
			}
		})
	}
}

// BenchmarkClientListFit is the list tool on a page of limit issues from
// a project that has more. Past about fifty, the page is over the budget:
// the tool searches for how many fit, marshaling a shorter page for each
// guess, then asks the server for the smaller page (calls/op).
func BenchmarkClientListFit(b *testing.B) {
	for _, limit := range []int{10, 50, 100, 500} {
		b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
			c := &benchConn{issues: benchIssues(1000)}
			ctx := b.Context()
			var kept int
			b.ReportAllocs()
			for b.Loop() {
				out, err := list(ctx, c, ListIn{Limit: limit})
				if err != nil {
					b.Fatal(err)
				}
				kept = len(out.Issues)
			}
			b.ReportMetric(float64(c.calls)/float64(b.N), "calls/op")
			b.ReportMetric(float64(kept), "kept")
		})
	}
}

// BenchmarkClientCommentsFit is the comments tool on an issue with long
// comments: each is cut to maxCommentBody, and the oldest dropped until
// the rest fit.
func BenchmarkClientCommentsFit(b *testing.B) {
	for _, limit := range []int{10, 100} {
		b.Run(fmt.Sprintf("limit=%d", limit), func(b *testing.B) {
			c := &benchConn{}
			at := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
			for i := range 100 {
				c.comments = append(c.comments, proto.Comment{ID: fmt.Sprint(i), Author: "bob", CreatedAt: at,
					Body: strings.Repeat("The second write wins, which loses the first controller's change. ", 30)})
			}
			ctx := b.Context()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := comments(ctx, c, PageIn{ID: "sf-1", Limit: limit}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkClientDigestFit is the largest digest a server sends (every
// section full, with whole titles and notes) built and held under
// MaxDigestTokens, which drops most of its items.
func BenchmarkClientDigestFit(b *testing.B) {
	items := func(n int) []DigestItem {
		out := make([]DigestItem, n)
		for i := range out {
			out[i] = DigestItem{ID: fmt.Sprintf("sf-%08d", i), Title: strings.Repeat("t", primeTitleLen), Priority: 1,
				By: "alice", At: "2026-10-13T09:00Z", Note: strings.Repeat("n", 200), BlockedBy: []string{"sf-1", "sf-2"}}
		}
		return out
	}
	var kept int
	b.ReportAllocs()
	for b.Loop() {
		d := Digest{Since: "2026-10-06T09:00Z", Until: "2026-10-13T09:00Z", Closed: items(10), Started: items(10),
			InProgress: items(10), Stalled: items(10), Blocked: items(10), HandedOff: items(10), Created: items(5),
			Discovered: items(10)}
		d.fit()
		kept = len(d.Closed) + len(d.Started) + len(d.InProgress) + len(d.Stalled) + len(d.Blocked) +
			len(d.HandedOff) + len(d.Created) + len(d.Discovered)
	}
	b.ReportMetric(float64(kept), "kept")
}

// BenchmarkClientPrime is prime built and held under its budget from a
// project with work in it.
func BenchmarkClientPrime(b *testing.B) {
	c := &benchConn{issues: benchIssues(1000)}
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := BuildPrime(ctx, c, "v0.2.0"); err != nil {
			b.Fatal(err)
		}
	}
}
