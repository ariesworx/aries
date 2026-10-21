package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/cli"
	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
)

// Prices and cost, through the CLI to a real daemon: an admin sets a
// price and anyone lists it, a non-admin is refused with the fix, and
// show, digest and cost price the tokens an agent's session reported.
func TestCostCLI(t *testing.T) {
	t.Parallel()
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, dana := w.newUser("alice", ""), w.newUser("dana", "")
	alice.env["ARIES_SESSION"] = "s-agent"
	epic := strings.TrimSpace(alice.ok("create", "Client work", "-t", "epic", "--account", "acme"))
	task := strings.TrimSpace(alice.ok("create", "Do the work", "--parent", epic))
	alice.ok("start", task)

	rates := []string{"--input", "4", "--output", "20", "--cache-write", "5", "--cache-write-1h", "8", "--cache-read", "0.4"}
	set := append([]string{"admin", "price", "set", "example-large", "--from", "2026-01-01"}, rates...)
	if got := dana.ok(set...); got != "example-large from 2026-01-01: added\n" {
		t.Fatalf("admin price set: %q", got)
	}
	if got := decode[proto.PriceSetResult](t, dana.ok(append(set, "--json")...)); got.Change != "unchanged" {
		t.Fatalf("admin price set again: %+v, want unchanged", got)
	}
	r := alice.run("v0.2.0", set...)
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "aries: prices set is for aries admins\n") ||
		!strings.Contains(r.stderr, "fix: ask an aries admin") {
		t.Fatalf("admin price set by alice: exit %d\n%s", r.code, r.stderr)
	}
	if got := alice.ok("admin", "price", "list"); !strings.Contains(got, "example-large  2026-01-01  4      20      5            8               0.4         dana\n") {
		t.Fatalf("admin price list:\n%s", got)
	}

	// A price for long prompts: all its rates go with the threshold.
	tier := append([]string{"admin", "price", "set", "example-tiered", "--from", "2026-01-01"}, rates...)
	if r := dana.run("v0.2.0", append(tier, "--over", "100000")...); r.code != cli.ExitUsage || !strings.Contains(r.stderr, "--over-input") {
		t.Fatalf("admin price set --over alone: exit %d\n%s, want usage naming the rates it lacks", r.code, r.stderr)
	}
	tier = append(tier, "--over", "100000", "--over-input", "20", "--over-output", "100", "--over-cache-write", "25",
		"--over-cache-write-1h", "40", "--over-cache-read", "2")
	if got := dana.ok(tier...); got != "example-tiered from 2026-01-01: added\n" {
		t.Fatalf("admin price set with a tier: %q", got)
	}
	if got := alice.ok("admin", "price", "list"); !strings.Contains(got, "  prompt over 100000              20     100     25           40              2\n") {
		t.Fatalf("admin price list after a tier:\n%s", got)
	}

	at := time.Now().UTC()
	in, out, cw, cw1h, cr := int64(1_000_000), int64(100_000), int64(200_000), int64(50_000), int64(3_000_000)
	unknown := int64(42)
	raw := alice.rawDial("s-agent")
	raw.usageCall(1, []proto.UsageRecord{
		// 4 + 2 + 0.15×5 + 0.05×8 + 3×0.4 = 8.35 USD
		{Harness: "claude-code", RequestID: "msg_01", Model: "example-large", At: at, Granularity: "request",
			Tokens: proto.Tokens{Input: &in, Output: &out, CacheWrite: &cw, CacheWrite1h: &cw1h, CacheRead: &cr}},
		{Harness: "claude-code", RequestID: "msg_02", Model: "example-unpriced", At: at, Granularity: "request",
			Tokens: proto.Tokens{Input: &unknown}},
	})

	if s := alice.ok("show", task); !strings.Contains(s, "\ncost $8.35 list price; some tokens unpriced\n") {
		t.Errorf("show lacks the cost line:\n%s", s)
	}
	if d := alice.ok("digest"); !strings.Contains(d, "\ncost $8.35 list price; some tokens unpriced\n") {
		t.Errorf("digest lacks the cost line:\n%s", d)
	}
	report := alice.ok("cost", "--since", "1h")
	for _, want := range []string{
		"cost by account, ",
		"  acme  $8.35  4.3M tokens  unpriced\n",
		"total $8.35, 4.3M tokens\n",
		"unpriced: no price for example-unpriced; an admin sets one with `aries admin price set`\n",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("cost lacks %q:\n%s", want, report)
		}
	}
	js := decode[proto.CostResult](t, alice.ok("cost", "--since", "1h", "--group-by", "epic", "--json"))
	if len(js.Groups) != 1 || js.Groups[0].Key != epic || js.Groups[0].Title != "Client work" || js.Groups[0].CostUSD != "8.35" ||
		js.Total.CostUSD != "8.35" {
		t.Errorf("cost --by epic --json = %+v", js)
	}
	ag := alice.mcp("v0.2.0")
	if is := decode[mcpserver.Issue](t, ag.ok("show", map[string]any{"id": task})); !strings.Contains(is.Usage, "; cost $8.35, some unpriced; ") {
		t.Errorf("mcp show usage %q, want the cost", is.Usage)
	}

	if r := alice.run("v0.2.0", "cost", "--since", "2026-02-01", "--until", "2026-01-01"); r.code != cli.ExitFailure ||
		!strings.Contains(r.stderr, "is before since") || !strings.Contains(r.stderr, "fix: correct it and retry; `aries cost -h` lists the options") {
		t.Errorf("cost with until before since: exit %d\n%s", r.code, r.stderr)
	}
}
