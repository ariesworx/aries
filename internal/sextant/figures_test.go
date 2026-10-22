package sextant

import (
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/telemetry"
)

func ptr(n int64) *int64 { return &n }

// Tokens no price covers are counted: those of a group the server marks
// unpriced, and those of a model it has no price for, which is (other).
func TestTokensByModelCountsUnpriced(t *testing.T) {
	t.Parallel()
	cost := proto.CostResult{Groups: []proto.CostGroup{
		{Key: "example-large", Tokens: proto.Tokens{Input: ptr(10), Output: ptr(5)}, CostUSD: "1"},
		{Key: "example-mixed", Tokens: proto.Tokens{Input: ptr(3), CacheRead: ptr(4)}, CostUSD: "1", Unpriced: true},
		{Key: "example-unknown", Tokens: proto.Tokens{Output: ptr(7)}, CostUSD: "0"},
	}}
	prices := proto.PricesResult{Prices: []proto.Price{{Model: "example-large"}, {Model: "example-mixed"}}}
	got := tokensByModel(cost, prices)
	want := []telemetry.SextantTokens{
		{Model: "example-large", Input: 10, Output: 5},
		{Model: "example-mixed", Input: 3, CacheRead: 4, Unpriced: 7},
		{Model: telemetry.ModelOther, Output: 7, Unpriced: 7},
	}
	if len(got) != len(want) {
		t.Fatalf("tokensByModel = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tokensByModel[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// Accounts carry their cost and their logged hours; a figure that is not
// an exact decimal is left out.
func TestAccounts(t *testing.T) {
	t.Parallel()
	cost := proto.CostResult{Groups: []proto.CostGroup{
		{Key: "team-a", CostUSD: "12.5", LoggedSeconds: 5400},
		{Key: "team-b", CostUSD: "unpriced"},
	}}
	got := accounts(cost)
	if len(got) != 1 || got[0] != (telemetry.SextantAccount{Account: "team-a", USD: 12.5, Hours: 1.5}) {
		t.Errorf("accounts = %v, want team-a at 12.5 and 1.5 hours alone", got)
	}
}
