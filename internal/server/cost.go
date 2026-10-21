package server

import (
	"context"
	"fmt"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// parseTime reads a price's from or a cost report's until: a date
// (midnight UTC) or an RFC 3339 time. what names the field in the error.
func parseTime(what, s string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%w: %s %q must be a date (2006-01-02) or a time (RFC 3339)", store.ErrInvalid, what, s)
}

// priceSet sets a model's rates; the store refuses anyone but an admin.
func priceSet(ctx context.Context, pr *served, a store.Actor, in proto.PriceSetArgs) (any, *proto.Error) {
	from, err := parseTime("from", in.From)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpPriceSet, "", 0, err)
	}
	var tier *store.PromptTier
	if in.Tier != nil {
		tier = &store.PromptTier{Over: in.Tier.Over, Rates: store.Rates(in.Tier.Rates)}
	}
	c, err := pr.store.SetPriceTier(ctx, a, in.Model, from, store.Rates(in.Rates), tier)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpPriceSet, "", 0, err)
	}
	return proto.PriceSetResult{Change: string(c)}, nil
}

func prices(ctx context.Context, pr *served, _ store.Actor, _ proto.PricesArgs) (any, *proto.Error) {
	ps, err := pr.store.Prices(ctx)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpPrices, "", 0, err)
	}
	out := proto.PricesResult{Prices: []proto.Price{}}
	for _, p := range ps {
		pp := proto.Price{Model: p.Model, From: p.From, Rates: proto.Rates(p.Rates), SetBy: p.SetBy, SetAt: p.SetAt}
		if p.Tier != nil {
			pp.Tier = &proto.PromptTier{Over: p.Tier.Over, Rates: proto.Rates(p.Tier.Rates)}
		}
		out.Prices = append(out.Prices, pp)
	}
	return out, nil
}

// cost reports the list-price equivalent of a window's tokens by group.
func cost(ctx context.Context, pr *served, _ store.Actor, in proto.CostArgs) (any, *proto.Error) {
	f, err := pr.costFilter(in)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpCost, "", 0, err)
	}
	r, err := pr.store.CostReport(ctx, f)
	if err != nil {
		return nil, pr.mapErr(ctx, proto.OpCost, "", 0, err)
	}
	out := proto.CostResult{By: in.By, Since: r.Since, Until: r.Until, Groups: []proto.CostGroup{}, Total: wireCostGroup(r.Total),
		Truncated: r.Capped}
	for _, g := range r.Groups {
		out.Groups = append(out.Groups, wireCostGroup(g))
	}
	n := min(len(r.Unpriced), proto.MaxUsageModels)
	for _, l := range r.Landed {
		out.Landed = append(out.Landed, wireLanded(l))
	}
	out.Unpriced, out.UnpricedMore = r.Unpriced[:n], len(r.Unpriced)-n
	return out, nil
}

// costFilter reads a cost report's arguments; since is required.
func (pr *served) costFilter(in proto.CostArgs) (store.CostFilter, error) {
	f := store.CostFilter{By: store.CostBy(in.By), Account: pr.account, Limit: in.Limit}
	if in.Since == "" {
		return f, fmt.Errorf("%w: a cost report needs since: a date, a time or a duration such as 7d", store.ErrInvalid)
	}
	var err error
	if f.Since, f.Window, err = parseSince(in.Since); err != nil {
		return f, err
	}
	if in.Until != "" {
		if f.Until, err = parseTime("until", in.Until); err != nil {
			return f, err
		}
	}
	return f, nil
}

func wireCostGroup(g store.CostGroup) proto.CostGroup {
	out := proto.CostGroup{Key: g.Key, Title: g.Title, Tokens: proto.Tokens(g.Tokens), CostUSD: proto.USD(g.Cost.Picodollars),
		Unpriced: g.Cost.Unpriced, Split: g.Split, LoggedSeconds: seconds(g.Logged)}
	if g.Amortized != nil {
		out.AmortizedUSD = proto.USD(g.Amortized)
	}
	out.Issues = g.Issues
	if g.Rework != nil {
		out.Rework = wireRework(*g.Rework)
	}
	if g.PerIssue != nil {
		out.PerIssueUSD = proto.USD(g.PerIssue.Picodollars)
	}
	for _, t := range g.Types {
		out.Types = append(out.Types, proto.CostByType{Type: string(t.Type), Tokens: proto.Tokens(t.Tokens), Issues: t.Issues,
			CostUSD: proto.USD(t.Cost.Picodollars), PerIssueUSD: proto.USD(t.PerIssue.Picodollars), Unpriced: t.Cost.Unpriced})
	}
	return out
}

func wireRework(r store.CostRework) *proto.CostRework {
	return &proto.CostRework{Tokens: proto.Tokens(r.Tokens), CostUSD: proto.USD(r.Cost.Picodollars), Unpriced: r.Cost.Unpriced}
}

func wireLanded(l store.LandedIssue) proto.CostLanded {
	out := proto.CostLanded{ID: string(l.ID), Title: l.Title, Type: string(l.Type), Tokens: proto.Tokens(l.Tokens),
		CostUSD: proto.USD(l.Cost.Picodollars), Unpriced: l.Cost.Unpriced, HeldSeconds: seconds(l.Held),
		CycleSeconds: seconds(l.Cycle), Waivers: l.Waivers, Split: l.Split}
	if l.ReworkTokens != (store.Tokens{}) {
		out.Rework = wireRework(store.CostRework{Tokens: l.ReworkTokens, Cost: l.Rework})
	}
	return out
}
