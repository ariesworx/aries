package mcpserver

import (
	"encoding/json"
	"sort"
	"unicode/utf8"
)

// Token budgets (design §1 principle 7, §9).
const (
	// MaxResultTokens caps every tool result.
	MaxResultTokens = 2000
	// MaxPrimeTokens caps prime.
	MaxPrimeTokens = 1500
	// MaxDigestTokens caps digest (design §5).
	MaxDigestTokens = 1500
	// DefaultLimit is how many issues a list returns when the agent does
	// not say.
	DefaultLimit = 10
)

// Tokens estimates how many tokens b costs a model. It counts one token per
// three bytes, which overestimates for English and JSON (about four bytes a
// token), so a result under budget here is under budget in practice.
func Tokens(b []byte) int { return (len(b) + 2) / 3 }

// size is the estimated token cost of v as JSON.
func size(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return Tokens(b)
}

// fitPrefix returns how many of n items a result keeps: all n if fits(n)
// reports true or n is at most least, and otherwise the largest k, with
// least < k < n, for which fits(k) does, or least if none does. fits(k)
// reports whether the result with only k of the items is under budget.
// It is what dropping one item at a time until the result fits, and never
// below least, would keep, found in about log2(n) calls of fits instead
// of n.
//
// That is so only if fits is monotone below n: whenever fits(k) is true,
// so is fits(j) for every j with least < j < k, which holds when keeping
// fewer of the same items never makes the result longer. fits(n) is asked
// apart from the rest because a result changes form once anything is left
// out ("more" or "truncated" set), and need not be monotone across that
// step; the search is only over results that have all changed form. Among
// those, dropping an item takes its JSON with it, and the only field a
// caller computes from k is a count of what was left out, one more for
// each item dropped: that adds at most one digit, while the item and its
// comma are at least two bytes. A count that starts below zero passes
// zero, where omitempty drops its key and the next item writes it again,
// so its key must also be shorter than any item. A field that could grow
// by more than an item shrinks, such as a summary of the dropped items,
// breaks this; its caller drops one item at a time instead (cost and
// show's files).
func fitPrefix(n, least int, fits func(k int) bool) int {
	if n <= least || fits(n) {
		return n
	}
	// The candidates are least+1 to n-1, and the ones that fit are a
	// prefix of them: count it.
	return least + sort.Search(n-1-least, func(i int) bool { return !fits(least + 1 + i) })
}

// dropLast keeps as many of the first of *items as fit, but at least one,
// and sets *more if it left any out. fits reports whether the result that
// holds them is under budget.
func dropLast[T any](items *[]T, more *bool, fits func() bool) {
	all, was := *items, *more
	keep := func(k int) { *items, *more = all[:k], was || k < len(all) }
	keep(fitPrefix(len(all), 1, func(k int) bool {
		keep(k)
		return fits()
	}))
}

// dropFirst keeps as many of the last of *items as fit, but at least one,
// and adds those it left out to *omitted. fits reports whether the result
// that holds them is under budget.
func dropFirst[T any](items *[]T, omitted *int, fits func() bool) {
	all, was := *items, *omitted
	keep := func(k int) { *items, *omitted = all[len(all)-k:], was+len(all)-k }
	keep(fitPrefix(len(all), 1, func(k int) bool {
		keep(k)
		return fits()
	}))
}

// cut shortens s, when it is longer than n bytes, to at most n bytes on a
// rune boundary followed by "…", and reports whether it did.
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…", true
}

// fitTexts halves the longest of fields until fits reports true or no
// field is longer than 64 bytes. It reports whether anything was cut.
func fitTexts(fits func() bool, fields ...*string) bool {
	cutAny := false
	for !fits() {
		var longest *string
		for _, f := range fields {
			if len(*f) > 64 && (longest == nil || len(*f) > len(*longest)) {
				longest = f
			}
		}
		if longest == nil {
			return cutAny
		}
		*longest, _ = cut(*longest, len(*longest)/2)
		cutAny = true
	}
	return cutAny
}
