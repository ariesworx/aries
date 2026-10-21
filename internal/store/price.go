package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Prices (design §12.1). An admin records each model's list rates from an
// effective time on; a cost report prices each usage record by the newest
// rates for its model in effect at the record's time (cost.go). Rates are
// integer micro-dollars per million tokens, so a cost is an exact integer
// sum: rate × tokens is picodollars (10⁻¹² USD).

// Rates are a model's list rates, in micro-dollars (10⁻⁶ USD) per million
// tokens. CacheWrite is a five-minute cache write and CacheWrite1h a
// one-hour one.
type Rates struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheWrite   int64 `json:"cache_write"`
	CacheWrite1h int64 `json:"cache_write_1h"`
	CacheRead    int64 `json:"cache_read"`
}

// PromptTier is the rates that price a request whose prompt is longer
// than Over tokens: its input, cache writes and cache reads together. A
// request of exactly Over tokens is not past it.
type PromptTier struct {
	Over int64
	Rates
}

// Price is a model's rates from From on, and who last set them. Tier,
// when set, replaces the rates for a request with a long prompt.
type Price struct {
	Model string
	From  time.Time
	Rates
	Tier  *PromptTier
	SetBy string
	SetAt time.Time
}

// PriceChange says what a SetPrice did.
type PriceChange string

// The changes SetPrice reports.
const (
	PriceAdded     PriceChange = "added"
	PriceReplaced  PriceChange = "replaced"
	PriceUnchanged PriceChange = "unchanged"
)

// OpPriceSet records a price added or replaced. Its after state is the
// model, the effective time and the rates; a replace's before state has
// the rates replaced.
const OpPriceSet Op = "price.set"

// PriceTarget is the target of price.set events. A model name can look
// like an issue ID, so the model is in the states, not the target.
const PriceTarget = "prices"

// Price bounds.
const (
	// MaxRate bounds a rate: a million US dollars per million tokens, far
	// past any list price, so an extra digit is refused.
	MaxRate = proto.MaxRate
	// MaxPromptOver bounds a tier's threshold, as MaxUsageCount bounds a
	// record's counts: no request has a longer prompt.
	MaxPromptOver = MaxUsageCount
	// PriceLead is how far ahead of the server's clock a price may take
	// effect, so an announced change can be entered early.
	PriceLead = 366 * 24 * time.Hour
)

// PriceLimitError refuses a new price when the table holds
// Limits.Prices (Max) rows. It wraps ErrInvalid.
type PriceLimitError struct{ Max int }

func (e *PriceLimitError) Error() string {
	return fmt.Sprintf("%v: the server keeps at most %d prices", ErrInvalid, e.Max)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *PriceLimitError) Unwrap() error { return ErrInvalid }

// validate refuses, with ErrInvalid, a rate out of range.
func (r Rates) validate() error {
	for _, c := range []struct {
		name string
		v    int64
	}{
		{"input", r.Input}, {"output", r.Output}, {"cache_write", r.CacheWrite},
		{"cache_write_1h", r.CacheWrite1h}, {"cache_read", r.CacheRead},
	} {
		if c.v < 0 || c.v > MaxRate {
			return fmt.Errorf("%w: %s rate must be from 0 to %d micro-dollars per million tokens", ErrInvalid, c.name, int64(MaxRate))
		}
	}
	return nil
}

// validate refuses, with ErrInvalid, a tier with no threshold or one out
// of range, or a rate out of range.
func (t PromptTier) validate() error {
	if t.Over < 1 || t.Over > MaxPromptOver {
		return fmt.Errorf("%w: over must be from 1 to %d prompt tokens", ErrInvalid, int64(MaxPromptOver))
	}
	return t.Rates.validate()
}

// SetPrice is SetPriceTier with no tier.
func (s *Store) SetPrice(ctx context.Context, actor Actor, model string, from time.Time, r Rates) (PriceChange, error) {
	return s.SetPriceTier(ctx, actor, model, from, r, nil)
}

// SetPriceTier records model's rates from from on, which is kept to the
// microsecond, and says whether it added a price, replaced the one with
// the same model and time, or found it already so. A change records a
// price.set event; an unchanged price writes nothing. Only an admin may
// set a price: anyone else is refused with a [*ForbiddenError]. A model
// out of proto.UsageModel's shape, a time before 2020-01-01 or more than
// PriceLead ahead, and a rate out of range are refused with ErrInvalid,
// and a new price past Limits.Prices with a [*PriceLimitError]. A tier
// is a second set of rates for requests whose prompt is over tier.Over
// tokens; nil is none, and replaces a tier the price had.
func (s *Store) SetPriceTier(ctx context.Context, actor Actor, model string, from time.Time, r Rates, tier *PromptTier) (PriceChange, error) {
	if !s.IsAdmin(actor.Principal) {
		return "", &ForbiddenError{Action: "prices set"}
	}
	from = from.UTC().Truncate(time.Microsecond)
	switch now := s.now(); {
	case !proto.UsageModel.MatchString(model):
		return "", fmt.Errorf("%w: model %q must be 1-128 ASCII letters, digits or _.:/+@-, starting with a letter or digit, as the harness reports it", ErrInvalid, model)
	case from.Before(usageEpoch):
		return "", fmt.Errorf("%w: from must be on or after %s", ErrInvalid, usageEpoch.Format(time.DateOnly))
	case from.After(now.Add(PriceLead)):
		return "", fmt.Errorf("%w: from %s is more than a year ahead", ErrInvalid, from.Format(time.DateOnly))
	}
	if err := r.validate(); err != nil {
		return "", err
	}
	if tier != nil {
		if err := tier.validate(); err != nil {
			return "", err
		}
	}
	var out PriceChange
	err := s.write(ctx, actor, func(w *wtx) error {
		out = ""
		var cur Rates
		var curTier PromptTier
		err := w.tx.QueryRowContext(ctx, `SELECT input, output, cache_write, cache_write_1h, cache_read,
  over_prompt, tier_input, tier_output, tier_cache_write, tier_cache_write_1h, tier_cache_read FROM prices
  WHERE model = $1 AND effective_at = $2`, model, from).Scan(&cur.Input, &cur.Output, &cur.CacheWrite, &cur.CacheWrite1h, &cur.CacheRead,
			&curTier.Over, &curTier.Input, &curTier.Output, &curTier.CacheWrite, &curTier.CacheWrite1h, &curTier.CacheRead)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read price: %w", err)
		}
		var t PromptTier // the zero tier is none
		after := map[string]any{"model": model, "from": from, "rates": r}
		if tier != nil {
			t = *tier
			after["tier"] = t
		}
		switch {
		case exists && cur == r && curTier == t:
			out = PriceUnchanged
			return nil
		case exists:
			if _, err := w.exec(ctx, `UPDATE prices SET input = $1, output = $2, cache_write = $3, cache_write_1h = $4, cache_read = $5,
  over_prompt = $6, tier_input = $7, tier_output = $8, tier_cache_write = $9, tier_cache_write_1h = $10, tier_cache_read = $11,
  set_by = $12, set_at = $13 WHERE model = $14 AND effective_at = $15`,
				r.Input, r.Output, r.CacheWrite, r.CacheWrite1h, r.CacheRead,
				t.Over, t.Input, t.Output, t.CacheWrite, t.CacheWrite1h, t.CacheRead,
				w.actor.Principal, w.now, model, from); err != nil {
				return fmt.Errorf("replace price: %w", err)
			}
			out = PriceReplaced
			before := map[string]any{"rates": cur}
			if curTier.Over > 0 {
				before["tier"] = curTier
			}
			return w.event(ctx, OpPriceSet, PriceTarget, before, after)
		}
		var n int
		if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM prices`).Scan(&n); err != nil {
			return fmt.Errorf("count prices: %w", err)
		}
		if n >= w.lim.Prices {
			return &PriceLimitError{Max: w.lim.Prices}
		}
		if _, err := w.exec(ctx, `INSERT INTO prices
  (model, effective_at, input, output, cache_write, cache_write_1h, cache_read,
   over_prompt, tier_input, tier_output, tier_cache_write, tier_cache_write_1h, tier_cache_read, set_by, set_at)
  VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			model, from, r.Input, r.Output, r.CacheWrite, r.CacheWrite1h, r.CacheRead,
			t.Over, t.Input, t.Output, t.CacheWrite, t.CacheWrite1h, t.CacheRead, w.actor.Principal, w.now); err != nil {
			return fmt.Errorf("add price: %w", err)
		}
		out = PriceAdded
		return w.event(ctx, OpPriceSet, PriceTarget, nil, after)
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// Prices lists every price, by model and then effective time. The table
// holds at most Limits.Prices rows.
func (s *Store) Prices(ctx context.Context) ([]Price, error) {
	return loadPrices(ctx, s.r)
}

// loadPrices reads every price on q, by model and then effective time.
func loadPrices(ctx context.Context, q querier) ([]Price, error) {
	var out []Price
	err := scanAll(ctx, q, "prices", `SELECT model, effective_at, input, output, cache_write, cache_write_1h, cache_read,
  over_prompt, tier_input, tier_output, tier_cache_write, tier_cache_write_1h, tier_cache_read, set_by, set_at
  FROM prices ORDER BY model, effective_at`, nil, func(rs *sql.Rows) error {
		var p Price
		var t PromptTier
		if err := rs.Scan(&p.Model, &p.From, &p.Input, &p.Output, &p.CacheWrite, &p.CacheWrite1h, &p.CacheRead,
			&t.Over, &t.Input, &t.Output, &t.CacheWrite, &t.CacheWrite1h, &t.CacheRead, &p.SetBy, &p.SetAt); err != nil {
			return err
		}
		if t.Over > 0 {
			p.Tier = &t
		}
		p.From, p.SetAt = p.From.UTC(), p.SetAt.UTC()
		out = append(out, p)
		return nil
	})
	return out, err
}
