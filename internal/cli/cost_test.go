package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

func TestPrintPrices(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		res  proto.PricesResult
		want string
	}{
		{"none", proto.PricesResult{}, "no prices; an admin sets one with `aries admin price set`\n"},
		{"some", proto.PricesResult{Prices: []proto.Price{
			{Model: "example-large", From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				Rates: proto.Rates{Input: 4_000_000, Output: 20_000_000, CacheWrite: 5_000_000, CacheWrite1h: 8_000_000, CacheRead: 400_000},
				SetBy: "dana", SetAt: at},
			{Model: "example-small\x1b[2J", From: time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC),
				Rates: proto.Rates{Input: 250_000, Output: 1_250_000}, SetBy: "erin", SetAt: at},
		}},
			"USD per million tokens\n" +
				"MODEL                 FROM              INPUT  OUTPUT  CACHE WRITE  CACHE WRITE 1H  CACHE READ  SET BY\n" +
				"example-large         2026-01-01        4      20      5            8               0.4         dana\n" +
				"example-small\\x1b[2J  2026-06-01 09:30  0.25   1.25    0            0               0           erin\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printPrices(&b, tc.res)
			if b.String() != tc.want {
				t.Errorf("printPrices =\n%s\nwant\n%s", b.String(), tc.want)
			}
		})
	}
}

func TestPrintCost(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		res  proto.CostResult
		want string
	}{
		{"no titles", proto.CostResult{By: "person", Since: since, Until: since.Add(time.Hour), Groups: []proto.CostGroup{
			{Key: "alice", Tokens: proto.Tokens{Input: n(2_000)}, CostUSD: "1"}, {Key: "bob", Tokens: proto.Tokens{Input: n(5)}, CostUSD: "0.01"},
		}, Total: proto.CostGroup{Tokens: proto.Tokens{Input: n(2_005)}, CostUSD: "1.01"}},
			"cost by person, 2026-10-01 00:00 UTC to 2026-10-01 01:00 UTC (1h), list price\n" +
				"  alice  $1.00  2k tokens\n" +
				"  bob    $0.01  5 tokens\n" +
				"total $1.01, 2k tokens\n"},
		{"nothing priced", proto.CostResult{By: "model", Since: since, Until: since.Add(time.Hour), Groups: []proto.CostGroup{
			{Key: "mystery", Tokens: proto.Tokens{Input: n(7)}, CostUSD: "0", Unpriced: true},
		}, Total: proto.CostGroup{Tokens: proto.Tokens{Input: n(7)}, CostUSD: "0", Unpriced: true}, Unpriced: []string{"mystery"}},
			"cost by model, 2026-10-01 00:00 UTC to 2026-10-01 01:00 UTC (1h), list price\n" +
				"  mystery  unpriced  7 tokens\n" +
				"total unpriced, 7 tokens\n" +
				"unpriced: no price for mystery; an admin sets one with `aries admin price set`\n"},
		{"empty", proto.CostResult{By: "account", Since: since, Until: since.Add(7 * 24 * time.Hour), Total: proto.CostGroup{CostUSD: "0"}},
			"cost by account, 2026-10-01 00:00 UTC to 2026-10-08 00:00 UTC (7d): no tokens\n"},
		{"groups", proto.CostResult{By: "issue", Since: since, Until: since.Add(24 * time.Hour), Groups: []proto.CostGroup{
			{Key: "ar-a1", Title: "a long title " + strings.Repeat("x", 60), Tokens: proto.Tokens{Input: n(1_200_000), Output: n(34_000)},
				CostUSD: "12.345", Split: true},
			{Key: proto.CostUnattributed, Tokens: proto.Tokens{Input: n(500)}, CostUSD: "0.0001"},
			{Key: "ar-b2", Title: "odd", Tokens: proto.Tokens{Input: n(50)}, CostUSD: "0", Unpriced: true},
			{Key: "ar-c3", Tokens: proto.Tokens{Input: n(9)}, CostUSD: "1", Unpriced: true}, // partly priced
		}, Total: proto.CostGroup{Tokens: proto.Tokens{Input: n(1_200_559), Output: n(34_000)}, CostUSD: "13.3451", Unpriced: true, Split: true},
			Unpriced: []string{"mystery"}, UnpricedMore: 3, Truncated: true},
			"cost by issue, 2026-10-01 00:00 UTC to 2026-10-02 00:00 UTC (1d), list price\n" +
				"  ar-a1           a long title xxxxxxxxxxxxxxxxxxxxxxxxxxx…  $12.35    1.2M tokens  split\n" +
				"  (unattributed)                                             <$0.01    500 tokens\n" +
				"  ar-b2           odd                                        unpriced  50 tokens\n" +
				"  ar-c3                                                      $1.00     9 tokens     unpriced\n" +
				"total $13.35, 1.2M tokens\n" +
				"split: shared by time with other work, so an estimate\n" +
				"unpriced: no price for mystery and 3 more; an admin sets one with `aries admin price set`\n" +
				"partial: more records than the server reads; narrow the window\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printCost(&b, tc.res)
			if b.String() != tc.want {
				t.Errorf("printCost =\n%s\nwant\n%s", b.String(), tc.want)
			}
		})
	}
}

func TestPrintUsageCost(t *testing.T) {
	t.Parallel()
	opus := []proto.ModelTokens{{Model: "opus", Tokens: proto.Tokens{Input: n(950)}}}
	tests := []struct {
		name string
		u    proto.IssueUsage
		want string
	}{
		{"priced", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus, CostUSD: "1.005"},
			"account: acme\nheld 10m\ncost $1.01 list price\ntokens opus: 950 in\n"},
		{"partly unpriced", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus, CostUSD: "0.5", Unpriced: true},
			"account: acme\nheld 10m\ncost $0.50 list price; some tokens unpriced\ntokens opus: 950 in\n"},
		{"all unpriced", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus, CostUSD: "0", Unpriced: true},
			"account: acme\nheld 10m\ncost unpriced\ntokens opus: 950 in\n"},
		{"older server", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus},
			"account: acme\nheld 10m\ntokens opus: 950 in\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printIssueUsage(&b, "acme", &tc.u)
			if b.String() != tc.want {
				t.Errorf("printIssueUsage(%+v) =\n%s\nwant\n%s", tc.u, b.String(), tc.want)
			}
		})
	}
	var b bytes.Buffer
	printDigestUsage(&b, &proto.DigestUsage{HeldSeconds: 600, Models: opus, CostUSD: "250.5"})
	if want := "held 10m\ncost $250.50 list price\ntokens opus: 950 in\n"; b.String() != want {
		t.Errorf("printDigestUsage =\n%s\nwant\n%s", b.String(), want)
	}
}

// Cost and admin commands refuse bad usage before dialing.
func TestCostUsage(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	rates := []string{"--input", "4", "--output", "20", "--cache-write", "5", "--cache-write-1h", "8", "--cache-read", "0.4"}
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"cost needs since", []string{"-C", empty, "cost", "--group-by", "issue"}, "cost needs --since"},
		{"cost takes no arguments", []string{"-C", empty, "cost", "--since", "7d", "x"}, "cost takes no arguments"},
		{"cost bad group", []string{"-C", empty, "cost", "--since", "7d", "--group-by", "team"}, "--group-by must be account, issue, epic, person, model or outcome"},
		{"admin needs price", []string{"-C", empty, "admin"}, "admin needs price"},
		{"admin unknown", []string{"-C", empty, "admin", "keys"}, `unknown admin command "keys"`},
		{"admin flags alone", []string{"-C", empty, "admin", "--from", "2026-01-01"}, "admin needs price, plan, project, principal, member, key, invite, identity, signin, trust, workflow or status"},
		// Flags before the subcommand are the subcommand's: --from reaches
		// price set, which names only the rates as missing.
		{"flags before price", []string{"-C", empty, "admin", "--from", "2026-01-01", "--input", "1", "price", "set", "m"},
			"price set needs --output, --cache-write, --cache-write-1h, --cache-read;"},
		{"flags before plan", []string{"-C", empty, "admin", "--from", "2026-09", "--fee", "25", "plan", "set", "team"},
			"plan set needs --seats;"},
		{"price unknown action", []string{"-C", empty, "admin", "price", "rm", "m"}, `unknown price action "rm"`},
		{"price list takes no rates", []string{"-C", empty, "admin", "price", "list", "--input", "1"}, "only price set takes rates"},
		{"set needs a model", append([]string{"-C", empty, "admin", "price", "set", "--from", "2026-01-01"}, rates...), "price set needs one model"},
		{"set needs from", append([]string{"-C", empty, "admin", "price", "set", "m"}, rates...), "--from"},
		{"set needs every rate", []string{"-C", empty, "admin", "price", "set", "m", "--from", "2026-01-01", "--input", "1"},
			"--output, --cache-write, --cache-write-1h, --cache-read"},
		{"bad rate", append([]string{"-C", empty, "admin", "price", "set", "m", "--from", "2026-01-01", "--input", "$4"}, rates[2:]...),
			"--input: not a rate"},
		// With two bad rates, the first in the usage's order is named.
		{"two bad rates", []string{"-C", empty, "admin", "price", "set", "m", "--from", "2026-01-01", "--input", "1", "--output", "x",
			"--cache-write", "1", "--cache-write-1h", "1", "--cache-read", "-1"}, "--output: not a rate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, tc.args...)
			if code != ExitUsage || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: usage: aries ") {
				t.Fatalf("exit %d (want %d), stdout %q, stderr %q; want it to name %q and the usage", code, ExitUsage, out, errb, tc.stderr)
			}
		})
	}
}

// admin --json before the subcommand prints the subcommand's one JSON
// document, here its refusal to run outside a repository.
func TestAdminJSONBeforeSubcommand(t *testing.T) {
	t.Parallel()
	empty := t.TempDir()
	for _, sub := range []string{"price", "plan"} {
		code, out, errb := runCLI(t, "-C", empty, "admin", "--json", sub, "list")
		if code != ExitFailure || errb != "" || !strings.Contains(out, `".aries.yaml not found here or in any parent directory"`) {
			t.Errorf("admin --json %s list outside a repository: exit %d, stdout %q, stderr %q; want exit %d and a JSON error", sub, code, out, errb, ExitFailure)
		}
	}
}

// A report by outcome prints the groups, then the issues counted, what
// a landed one cost, the rework and the landed issues one by one.
func TestPrintCostByOutcome(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	res := proto.CostResult{By: "outcome", Since: since, Until: since.Add(24 * time.Hour),
		Groups: []proto.CostGroup{
			{Key: "landed", Issues: 2, Tokens: proto.Tokens{Input: n(150)}, CostUSD: "0.17", PerIssueUSD: "0.085",
				Rework: &proto.CostRework{Tokens: proto.Tokens{Input: n(30)}, CostUSD: "0.03"},
				Types:  []proto.CostByType{{Type: "bug", Issues: 1, CostUSD: "0.05", PerIssueUSD: "0.05"}, {Type: "task", Issues: 1, CostUSD: "0.12", PerIssueUSD: "0.12"}}},
			{Key: "open", Issues: 1, Tokens: proto.Tokens{Input: n(7)}, CostUSD: "0.007"},
			{Key: "(unattributed)", Tokens: proto.Tokens{Input: n(1_000)}, CostUSD: "1"},
		},
		Total: proto.CostGroup{Tokens: proto.Tokens{Input: n(1_157)}, CostUSD: "1.177"},
		Landed: []proto.CostLanded{
			{ID: "t-1", Title: "one", Type: "task", CostUSD: "0.12", HeldSeconds: 300, CycleSeconds: 300, Waivers: 1},
			{ID: "t-2", Title: "two", Type: "bug", CostUSD: "0.05", HeldSeconds: 420, CycleSeconds: 1500,
				Rework: &proto.CostRework{Tokens: proto.Tokens{Input: n(30)}, CostUSD: "0.03"}},
		}}
	var b bytes.Buffer
	printCost(&b, res)
	want := "cost by outcome, 2026-10-01 00:00 UTC to 2026-10-02 00:00 UTC (1d), list price\n" +
		"  landed          $0.17  150 tokens\n" +
		"  open            $0.01  7 tokens\n" +
		"  (unattributed)  $1.00  1k tokens\n" +
		"total $1.18, 1.1k tokens\n" +
		"landed: 2 issues, $0.09 per issue, rework $0.03 (30 tokens)\n" +
		"  bug: 1 issue, $0.05 per issue\n" +
		"  task: 1 issue, $0.12 per issue\n" +
		"open: 1 issue\n" +
		"landed issues, dearest first (held is time held, cycle is creation to last close):\n" +
		"  t-1  task  one  $0.12  held 5m of 5m   1 waiver\n" +
		"  t-2  bug   two  $0.05  held 7m of 25m  rework $0.03\n"
	if b.String() != want {
		t.Errorf("printCost(by outcome) =\n%s\nwant\n%s", b.String(), want)
	}
}
