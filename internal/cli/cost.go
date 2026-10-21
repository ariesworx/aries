package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// costBys are the groupings aries cost takes.
var costBys = []string{"account", "issue", "epic", "person", "model", "outcome"}

const costUsage = "cost --since 7d|DATE|TIME [--until DATE|TIME] [--group-by account|issue|epic|person|model|outcome] [-n N]"

func cmdCost(ctx context.Context, r *runner, args []string) error {
	const usage = costUsage
	fs := r.newFlags("cost")
	var in proto.CostArgs
	fs.StringVar(&in.By, "group-by", "account", "group by account, issue, epic, person, model or outcome")
	fs.StringVar(&in.Since, "since", "", "window start: a duration back from now (7d), a date or an RFC 3339 time")
	fs.StringVar(&in.Until, "until", "", "window end, not included: a date or an RFC 3339 time (default now)")
	fs.IntVar(&in.Limit, "n", 0, "groups to list, the rest summed (default 50)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	switch {
	case len(pos) > 0:
		return usagef(usage, "cost takes no arguments")
	case in.Since == "":
		return usagef(usage, "cost needs --since, such as 7d or 2026-10-01")
	case !slices.Contains(costBys, in.By):
		return usagef(usage, "--group-by must be account, issue, epic, person, model or outcome")
	}
	var out proto.CostResult
	if err := r.call(ctx, proto.OpCost, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printCost(r.env.Stdout, out)
	return nil
}

// costTitle is how many runes of an issue's or epic's title a report
// shows.
const costTitle = 40

// printCost renders a cost report: a header, a line per group with its
// cost, tokens and marks, the total, and what the marks mean.
func printCost(w io.Writer, c proto.CostResult) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("cost by %s, %s to %s (%s)", esc(c.By), when(c.Since), when(c.Until), proto.Span(c.Until.Sub(c.Since)))
	if len(c.Groups) == 0 {
		p(": no tokens\n")
		return
	}
	amortized := c.Total.AmortizedUSD != ""
	logged := slices.ContainsFunc(c.Groups, func(g proto.CostGroup) bool { return g.LoggedSeconds > 0 })
	if amortized {
		p(", list price and amortized\n")
	} else {
		p(", list price\n")
	}
	titled := slices.ContainsFunc(c.Groups, func(g proto.CostGroup) bool { return g.Title != "" })
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, g := range c.Groups {
		_, _ = fmt.Fprintf(tw, "  %s\t", esc(g.Key))
		if titled {
			title := []rune(strings.Join(strings.Fields(g.Title), " "))
			if len(title) > costTitle {
				title = append(title[:costTitle], '…')
			}
			_, _ = fmt.Fprintf(tw, "%s\t", esc(string(title)))
		}
		var marks []string
		if g.Split {
			marks = append(marks, "split")
		}
		usd := proto.Dollars(g.CostUSD)
		switch {
		case g.Unpriced && isZero(g.CostUSD):
			usd = "unpriced" // no token had a price: not free
		case g.Unpriced:
			marks = append(marks, "unpriced")
		}
		_, _ = fmt.Fprintf(tw, "%s\t", usd)
		if amortized {
			_, _ = fmt.Fprintf(tw, "%s amortized\t", proto.Dollars(g.AmortizedUSD))
		}
		_, _ = fmt.Fprintf(tw, "%s tokens\t", proto.TokenCount(tokens(g.Tokens)))
		if logged {
			if g.LoggedSeconds > 0 {
				_, _ = fmt.Fprintf(tw, "%s logged", proto.Hours(g.LoggedSeconds))
			}
			_, _ = fmt.Fprint(tw, "\t")
		}
		_, _ = fmt.Fprintf(tw, "%s\n", strings.Join(marks, " "))
	}
	_ = tw.Flush()
	for line := range strings.Lines(b.String()) {
		p("%s\n", strings.TrimRight(line, " \n"))
	}
	total := proto.Dollars(c.Total.CostUSD)
	if c.Total.Unpriced && isZero(c.Total.CostUSD) {
		total = "unpriced"
	}
	p("total %s", total)
	if amortized {
		p(", %s amortized", proto.Dollars(c.Total.AmortizedUSD))
	}
	p(", %s tokens", proto.TokenCount(tokens(c.Total.Tokens)))
	if c.Total.LoggedSeconds > 0 {
		p(", %s logged", proto.Hours(c.Total.LoggedSeconds))
	}
	p("\n")
	if slices.ContainsFunc(c.Groups, func(g proto.CostGroup) bool { return g.Split }) {
		p("split: shared by time with other work, so an estimate\n")
	}
	if c.By == "outcome" {
		printOutcome(w, c)
	}
	if len(c.Unpriced) > 0 {
		names := strings.Join(escAll(c.Unpriced), ", ")
		if c.UnpricedMore > 0 {
			names += fmt.Sprintf(" and %d more", c.UnpricedMore)
		}
		p("unpriced: no price for %s; an admin sets one with `aries admin price set`\n", names)
	}
	if c.Truncated {
		p("partial: more records than the server reads; narrow the window\n")
	}
}

// tokens is the sum of the counts t knows: input, output, cache write and
// cache read (the one-hour writes are part of cache write).
func tokens(t proto.Tokens) int64 {
	var n int64
	for _, c := range []*int64{t.Input, t.Output, t.CacheWrite, t.CacheRead} {
		if c != nil {
			n += *c
		}
	}
	return n
}

// printCostLine prints show's or digest's cost line: "cost unpriced"
// when no token had a price, never $0.00. A server that sends no cost
// prints nothing.
func printCostLine(w io.Writer, usd string, unpriced bool) {
	switch {
	case usd == "":
		return
	case unpriced && isZero(usd):
		_, _ = fmt.Fprintln(w, "cost unpriced")
		return
	}
	_, _ = fmt.Fprintf(w, "cost %s list price", proto.Dollars(usd))
	if unpriced {
		_, _ = fmt.Fprint(w, "; some tokens unpriced")
	}
	_, _ = fmt.Fprintln(w)
}

// isZero reports whether the decimal usd is zero.
func isZero(usd string) bool { return strings.Trim(usd, "0.") == "" }

// rateFlags are the five rate flags of price set, in the order the
// usage gives them.
var rateFlags = []string{"input", "output", "cache-write", "cache-write-1h", "cache-read"}

// cmdAdmin runs the first of its arguments that names a group (price,
// plan, project, principal, member, key, invite, identity, signin or
// status),
// with the rest, so flags given before the group are its own.
func cmdAdmin(ctx context.Context, r *runner, args []string) error {
	const usage = "admin price|plan|project|principal|member|key|invite|identity|signin|trust|workflow|status ..."
	i := slices.IndexFunc(args, func(a string) bool { return slices.Contains(adminGroups, a) })
	if i < 0 {
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			return usagef(usage, "unknown admin command %q", args[0])
		}
		return usagef(usage, "%s", adminNeeds)
	}
	rest := slices.Delete(slices.Clone(args), i, i+1)
	switch args[i] {
	case "price":
		return adminPrice(ctx, r, rest)
	case "plan":
		return adminPlan(ctx, r, rest)
	case "project":
		return adminProject(ctx, r, rest)
	case "principal":
		return adminPrincipal(ctx, r, rest)
	case "key":
		return adminKey(ctx, r, rest)
	case "invite":
		return adminInvite(ctx, r, rest)
	case "identity":
		return adminIdentity(ctx, r, rest)
	case "signin":
		return adminSignIn(ctx, r, rest)
	case "trust":
		return adminTrust(ctx, r, rest)
	case "workflow":
		return adminWorkflow(ctx, r, rest)
	case "status":
		return adminStatus(ctx, r, rest)
	}
	return adminMember(ctx, r, rest)
}

// tierFlags are the rate flags of a long prompt's tier, in rateFlags'
// order; --over names the prompt length they apply past.
var tierFlags = []string{"over-input", "over-output", "over-cache-write", "over-cache-write-1h", "over-cache-read"}

const priceUsage = "admin price list | set MODEL --from DATE|TIME --input USD --output USD --cache-write USD --cache-write-1h USD --cache-read USD [--over TOKENS --over-input USD --over-output USD --over-cache-write USD --over-cache-write-1h USD --over-cache-read USD]"

func adminPrice(ctx context.Context, r *runner, args []string) error {
	const usage = priceUsage
	fs := r.newFlags("admin price")
	var from string
	fs.StringVar(&from, "from", "", "when the rates take effect: a date (midnight UTC) or an RFC 3339 time")
	rates := map[string]*string{}
	for _, f := range rateFlags {
		rates[f] = fs.String(f, "", "US dollars per million tokens")
	}
	var over int64
	fs.Int64Var(&over, "over", 0, "prompt length in tokens past which the --over-* rates apply")
	tier := map[string]*string{}
	for _, f := range tierFlags {
		tier[f] = fs.String(f, "", "US dollars per million tokens, for a request whose prompt is over --over tokens")
	}
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	tierSet := set(fs, "over") || slices.ContainsFunc(tierFlags, func(f string) bool { return set(fs, f) })
	switch {
	case len(pos) == 0:
		return usagef(usage, "admin price needs list or set")
	case pos[0] == "list":
		if len(pos) > 1 {
			return usagef(usage, "price list takes no arguments")
		}
		if set(fs, "from") || tierSet || slices.ContainsFunc(rateFlags, func(f string) bool { return set(fs, f) }) {
			return usagef(usage, "only price set takes rates and --from")
		}
		return listPrices(ctx, r)
	case pos[0] != "set":
		return usagef(usage, "unknown price action %q", pos[0])
	case len(pos) != 2:
		return usagef(usage, "price set needs one model, spelled as the harness reports it")
	}
	in := proto.PriceSetArgs{Model: pos[1], From: from}
	var missing []string
	for _, f := range rateFlags {
		if !set(fs, f) {
			missing = append(missing, "--"+f)
		}
	}
	if from == "" {
		missing = append([]string{"--from"}, missing...)
	}
	if len(missing) > 0 {
		return usagef(usage, "price set needs %s; give 0 for what the model does not charge", strings.Join(missing, ", "))
	}
	for i, dst := range []*int64{&in.Input, &in.Output, &in.CacheWrite, &in.CacheWrite1h, &in.CacheRead} { // rateFlags' order
		if *dst, err = proto.ParseRate(*rates[rateFlags[i]]); err != nil {
			return usagef(usage, "--%s: %v", rateFlags[i], err)
		}
	}
	if tierSet {
		var missing []string
		if over <= 0 {
			missing = append(missing, "--over (a positive number of tokens)")
		}
		for _, f := range tierFlags {
			if !set(fs, f) {
				missing = append(missing, "--"+f)
			}
		}
		if len(missing) > 0 {
			return usagef(usage, "a price for long prompts needs %s; give 0 for what the model does not charge", strings.Join(missing, ", "))
		}
		in.Tier = &proto.PromptTier{Over: over}
		for i, dst := range []*int64{&in.Tier.Input, &in.Tier.Output, &in.Tier.CacheWrite, &in.Tier.CacheWrite1h, &in.Tier.CacheRead} {
			if *dst, err = proto.ParseRate(*tier[tierFlags[i]]); err != nil {
				return usagef(usage, "--%s: %v", tierFlags[i], err)
			}
		}
	}
	var out proto.PriceSetResult
	if err := r.call(ctx, proto.OpPriceSet, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s from %s: %s\n", esc(in.Model), esc(in.From), esc(out.Change))
	return nil
}

func listPrices(ctx context.Context, r *runner) error {
	var out proto.PricesResult
	if err := r.call(ctx, proto.OpPrices, proto.PricesArgs{}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printPrices(r.env.Stdout, out)
	return nil
}

// printPrices renders the prices table in US dollars per million tokens;
// a price from midnight UTC shows its date alone.
func printPrices(w io.Writer, res proto.PricesResult) {
	if len(res.Prices) == 0 {
		_, _ = fmt.Fprintln(w, "no prices; an admin sets one with `aries admin price set`")
		return
	}
	_, _ = fmt.Fprintln(w, "USD per million tokens")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tFROM\tINPUT\tOUTPUT\tCACHE WRITE\tCACHE WRITE 1H\tCACHE READ\tSET BY")
	for _, p := range res.Prices {
		from := p.From.UTC().Format("2006-01-02 15:04")
		if p.From.Equal(p.From.Truncate(24 * time.Hour)) {
			from = p.From.UTC().Format(time.DateOnly)
		}
		r := p.Rates
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", esc(p.Model), from, proto.FormatRate(r.Input), proto.FormatRate(r.Output),
			proto.FormatRate(r.CacheWrite), proto.FormatRate(r.CacheWrite1h), proto.FormatRate(r.CacheRead), esc(p.SetBy))
		if t := p.Tier; t != nil { // the rates past a prompt length, under the price they replace
			_, _ = fmt.Fprintf(tw, "  prompt over %d\t\t%s\t%s\t%s\t%s\t%s\n", t.Over, proto.FormatRate(t.Input), proto.FormatRate(t.Output),
				proto.FormatRate(t.CacheWrite), proto.FormatRate(t.CacheWrite1h), proto.FormatRate(t.CacheRead))
		}
	}
	_ = tw.Flush()
}

// printOutcome renders what a report by outcome adds to the table of
// groups: per group the issues counted, the cost of a landed one and the
// rework, then the landed issues by type and one by one.
func printOutcome(w io.Writer, c proto.CostResult) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	for _, g := range c.Groups {
		if g.Issues == 0 && g.Rework == nil {
			continue
		}
		p("%s: %s", esc(g.Key), countOf(g.Issues, "issue"))
		if g.PerIssueUSD != "" {
			p(", %s per issue", proto.Dollars(g.PerIssueUSD))
		}
		if g.Rework != nil {
			p(", rework %s (%s tokens)", costOrUnpriced(g.Rework.CostUSD, g.Rework.Unpriced), proto.TokenCount(tokens(g.Rework.Tokens)))
		}
		p("\n")
		for _, t := range g.Types {
			p("  %s: %s, %s per issue\n", esc(t.Type), countOf(t.Issues, "issue"), costOrUnpriced(t.PerIssueUSD, t.Unpriced))
		}
	}
	if len(c.Landed) == 0 {
		return
	}
	p("landed issues, dearest first (held is time held, cycle is creation to last close):\n")
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, l := range c.Landed {
		title := []rune(strings.Join(strings.Fields(l.Title), " "))
		if len(title) > costTitle {
			title = append(title[:costTitle], '…')
		}
		var marks []string
		if l.Rework != nil {
			marks = append(marks, "rework "+costOrUnpriced(l.Rework.CostUSD, l.Rework.Unpriced))
		}
		if l.Waivers > 0 {
			marks = append(marks, countOf(l.Waivers, "waiver"))
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\theld %s of %s\t%s\n", esc(l.ID), esc(l.Type), esc(string(title)),
			costOrUnpriced(l.CostUSD, l.Unpriced), proto.Span(time.Duration(l.HeldSeconds)*time.Second),
			proto.Span(time.Duration(l.CycleSeconds)*time.Second), strings.Join(marks, ", "))
	}
	_ = tw.Flush()
	for line := range strings.Lines(b.String()) {
		p("%s\n", strings.TrimRight(line, " \n"))
	}
}

// costOrUnpriced is a cost as dollars, or "unpriced" when no token had
// a price.
func costOrUnpriced(usd string, unpriced bool) string {
	if unpriced && isZero(usd) {
		return "unpriced"
	}
	return proto.Dollars(usd)
}

// countOf is "1 issue", "2 issues".
func countOf(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
