package mcpserver

import (
	"encoding/json"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// The results below are cut to the budget by fitPrefix's search. Each
// site's old loop, which dropped one item at a time, is kept here as the
// reference the search must match byte for byte.

// trimSite is one place a result is cut to the budget.
type trimSite struct {
	name string
	// budget is the site's cap in tokens, if not MaxResultTokens.
	budget int
	// build makes the untrimmed result, one item for each text; extra
	// seeds the counts and flags the result starts with.
	build func(texts []string, extra int) any
	// fit cuts the result as the tool does now, and old as it did before.
	fit, old func(v any)
}

// limit is the site's cap in tokens.
func (s trimSite) limit() int {
	if s.budget > 0 {
		return s.budget
	}
	return MaxResultTokens
}

var trimSites = []trimSite{
	{
		name: "who",
		build: func(texts []string, extra int) any {
			out := &Who{Agents: []Agent{}, More: extra%2 == 0}
			for i, s := range texts {
				out.Agents = append(out.Agents, Agent{Principal: "alice", Session: fmt.Sprint("s-", i), Machine: s, Seen: "1m"})
			}
			return out
		},
		fit: func(v any) { v.(*Who).fit() },
		old: func(v any) {
			out := v.(*Who)
			for size(out) > MaxResultTokens && len(out.Agents) > 1 {
				out.Agents, out.More = out.Agents[:len(out.Agents)-1], true
			}
		},
	},
	{
		name: "ready",
		build: func(texts []string, _ int) any {
			return &Issues{Issues: trimSummaries(texts)}
		},
		fit: func(v any) { v.(*Issues).fit() },
		old: func(v any) {
			out := v.(*Issues)
			for size(out) > MaxResultTokens && len(out.Issues) > 1 {
				out.Issues, out.More = out.Issues[:len(out.Issues)-1], true
			}
		},
	},
	{
		name: "list",
		build: func(texts []string, extra int) any {
			next := ""
			if extra%2 == 0 {
				next = "cursor-" + strings.Repeat("x", extra%40)
			}
			return &Issues{Issues: trimSummaries(texts), Next: next}
		},
		fit: func(v any) {
			out := v.(*Issues)
			out.Issues = out.Issues[:listLimit(out.Issues, out.Next)]
		},
		old: func(v any) {
			out := v.(*Issues)
			if size(out) <= MaxResultTokens || len(out.Issues) <= 1 {
				return
			}
			n := len(out.Issues) - 1
			for n > 1 && size(Issues{Issues: out.Issues[:n], Next: out.Next}) > MaxResultTokens {
				n--
			}
			out.Issues = out.Issues[:n]
		},
	},
	{
		name: "blocked",
		build: func(texts []string, _ int) any {
			out := &Blocked{}
			for i, s := range trimSummaries(texts) {
				out.Issues = append(out.Issues, proto.BlockedIssue{Summary: s, BlockedBy: []string{fmt.Sprint("sf-b", i)}})
			}
			return out
		},
		fit: func(v any) { v.(*Blocked).fit() },
		old: func(v any) {
			out := v.(*Blocked)
			for size(out) > MaxResultTokens && len(out.Issues) > 1 {
				out.Issues, out.More = out.Issues[:len(out.Issues)-1], true
			}
		},
	},
	{
		name: "start memories",
		build: func(texts []string, extra int) any {
			return &Started{ID: "sf-1", Title: "t", Type: "task", Body: strings.Repeat("b", extra%1000), Memories: trimMemories(texts)}
		},
		fit: func(v any) { v.(*Started).fitMemories() },
		old: func(v any) {
			out := v.(*Started)
			for size(out) > MaxResultTokens && len(out.Memories) > 0 {
				out.Memories = out.Memories[:len(out.Memories)-1]
			}
		},
	},
	{
		name: "show needed by",
		build: func(texts []string, extra int) any {
			return &Issue{ID: "sf-1", Title: "t", Status: "open", Type: "task", Truncated: extra%2 == 0,
				Body: strings.Repeat("b", extra%1000), NeededBy: append([]string(nil), texts...), Files: []string{"a.go"}}
		},
		fit: func(v any) { v.(*Issue).fitNeededBy() },
		old: func(v any) {
			out := v.(*Issue)
			for size(out) > MaxResultTokens && len(out.NeededBy) > 0 {
				out.NeededBy = out.NeededBy[:len(out.NeededBy)-1]
				out.Truncated = true
			}
		},
	},
	{
		name: "comments",
		build: func(texts []string, extra int) any {
			// Omitted is the server's total less what it returned, and an
			// old or wrong total can make it negative.
			out := &Comments{Comments: []Comment{}, Omitted: extra%50 - len(texts)}
			for i, s := range texts {
				out.Comments = append(out.Comments, Comment{Author: fmt.Sprint("p", i), At: "2026-10-13T09:00Z", Body: s})
			}
			return out
		},
		fit: func(v any) { v.(*Comments).fit() },
		old: func(v any) {
			out := v.(*Comments)
			for size(out) > MaxResultTokens && len(out.Comments) > 1 {
				out.Comments, out.Omitted = out.Comments[1:], out.Omitted+1
			}
		},
	},
	{
		name: "history",
		build: func(texts []string, extra int) any {
			out := &History{Events: []Event{}, Omitted: extra%50 - len(texts)}
			for i, s := range texts {
				out.Events = append(out.Events, Event{Seq: int64(i), At: "2026-10-13T09:00Z", By: "bob", Op: "update", Changed: s})
			}
			return out
		},
		fit: func(v any) { v.(*History).fit() },
		old: func(v any) {
			out := v.(*History)
			for size(out) > MaxResultTokens && len(out.Events) > 1 {
				out.Events, out.Omitted = out.Events[1:], out.Omitted+1
			}
		},
	},
	{
		name: "recall",
		build: func(texts []string, extra int) any {
			return &Memories{Memories: trimMemories(texts), More: extra%30 - 10, Truncated: extra%3 == 0}
		},
		fit: func(v any) { v.(*Memories).fit() },
		old: func(v any) {
			out := v.(*Memories)
			for size(out) > MaxResultTokens && len(out.Memories) > 1 {
				out.Memories, out.More, out.Truncated = out.Memories[:len(out.Memories)-1], out.More+1, true
			}
		},
	},
	{
		name: "inbox",
		build: func(texts []string, extra int) any {
			out := &Inbox{Items: []InboxItem{}, Unread: extra % 40, Acked: extra % 3}
			for i, s := range texts {
				out.Items = append(out.Items, InboxItem{ID: int64(i), Kind: "mention", From: "bob", At: "2026-10-13T09:00Z", Body: s})
			}
			out.More = len(out.Items) < out.Unread
			return out
		},
		fit: func(v any) { v.(*Inbox).fit() },
		old: func(v any) {
			out := v.(*Inbox)
			for size(out) > MaxResultTokens && len(out.Items) > 1 {
				out.Items, out.More = out.Items[:len(out.Items)-1], true
			}
		},
	},
	{
		name:   "digest",
		budget: MaxDigestTokens,
		build: func(texts []string, extra int) any {
			out := &Digest{Since: "2026-10-06T09:00Z", Until: "2026-10-13T09:00Z", Truncated: extra%3 == 0}
			sections := []*[]DigestItem{&out.Closed, &out.Started, &out.InProgress, &out.Stalled, &out.Blocked,
				&out.HandedOff, &out.Created, &out.Discovered}
			// A stride of 2 or 4 leaves some sections empty and the rest
			// long, so the longest changes as items are dropped.
			stride := extra%5 + 1
			for i, s := range texts {
				sec := sections[(i*stride+extra)%len(sections)]
				*sec = append(*sec, DigestItem{ID: fmt.Sprint("sf-", i), Title: s, Priority: i % 5})
			}
			return out
		},
		fit: func(v any) { v.(*Digest).fit() },
		old: func(v any) {
			d := v.(*Digest)
			sections := []*[]DigestItem{&d.Closed, &d.Started, &d.InProgress, &d.Stalled, &d.Blocked, &d.HandedOff,
				&d.Created, &d.Discovered}
			for size(d) > MaxDigestTokens {
				var longest *[]DigestItem
				for _, sec := range sections {
					if len(*sec) > 0 && (longest == nil || len(*sec) > len(*longest)) {
						longest = sec
					}
				}
				if longest == nil {
					return
				}
				*longest = (*longest)[:len(*longest)-1]
				d.Truncated = true
			}
		},
	},
}

func trimSummaries(texts []string) []proto.Summary {
	out := []proto.Summary{}
	for i, s := range texts {
		out = append(out, proto.Summary{ID: fmt.Sprint("sf-", i), Title: s, Status: "open", Priority: i % 5})
	}
	return out
}

func trimMemories(texts []string) []Memory {
	out := []Memory{}
	for i, s := range texts {
		out = append(out, Memory{Key: fmt.Sprint("k", i), Scope: "project", Body: s})
	}
	return out
}

// checkTrim cuts the result of texts both ways, reports a difference, and
// returns the tokens of what was kept.
func checkTrim(t *testing.T, site trimSite, texts []string, extra int) int {
	t.Helper()
	got, want := site.build(texts, extra), site.build(texts, extra)
	before, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	site.fit(got)
	site.old(want)
	gb, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gb) != string(wb) || !reflect.DeepEqual(got, want) {
		t.Errorf("%s fit of %d items (%d tokens, extra %d) = %d tokens %.300s, want %d tokens %.300s",
			site.name, len(texts), Tokens(before), extra, Tokens(gb), gb, Tokens(wb), wb)
	}
	return Tokens(gb)
}

// padTo lengthens the last of texts until the untrimmed result is target
// tokens, and reports whether it could.
func padTo(site trimSite, texts []string, extra, target int) bool {
	if len(texts) == 0 {
		return false
	}
	last := len(texts) - 1
	tokens := func(pad int) int {
		texts[last] = strings.Repeat("a", pad)
		return size(site.build(texts, extra))
	}
	lo, hi := 0, 4*site.limit()
	for lo < hi {
		mid := (lo + hi) / 2
		if tokens(mid) < target {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return tokens(lo) == target
}

func repeatTexts(n, length int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Repeat("x", length)
	}
	return out
}

func TestTrimMatchesOneAtATime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		texts []string
		// pad, when set, pads the last text until the untrimmed result is
		// the site's budget plus offset in tokens.
		pad    bool
		offset int
	}{
		{name: "empty"},
		{name: "one small", texts: repeatTexts(1, 10)},
		{name: "one too large", texts: repeatTexts(1, 10000)},
		{name: "all fit", texts: repeatTexts(30, 20)},
		{name: "none fit", texts: repeatTexts(5, 7000)},
		{name: "very large items", texts: repeatTexts(5, 50000)},
		{name: "one under", texts: repeatTexts(15, 200), pad: true, offset: -1},
		{name: "at", texts: repeatTexts(15, 200), pad: true},
		{name: "one over", texts: repeatTexts(15, 200), pad: true, offset: 1},
		{name: "two at", texts: repeatTexts(2, 10), pad: true},
		{name: "two one over", texts: repeatTexts(2, 10), pad: true, offset: 1},
		{name: "many one over", texts: repeatTexts(50, 1), pad: true, offset: 1},
		{name: "escaped", texts: slices.Repeat([]string{strings.Repeat("<é\n", 60)}, 40)},
		{name: "first huge", texts: append([]string{strings.Repeat("y", 9000)}, repeatTexts(30, 30)...)},
		{name: "last huge", texts: append(repeatTexts(30, 30), strings.Repeat("y", 9000))},
	}
	for _, site := range trimSites {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			for _, tc := range cases {
				for _, extra := range []int{0, 1, 2999} {
					t.Run(fmt.Sprintf("%s/%d", tc.name, extra), func(t *testing.T) {
						texts := append([]string(nil), tc.texts...)
						if target := site.limit() + tc.offset; tc.pad && !padTo(site, texts, extra, target) {
							t.Fatalf("no padding makes the result %d tokens", target)
						}
						checkTrim(t, site, texts, extra)
					})
				}
			}
		})
	}
}

// TestTrimAtEveryBoundary sweeps the size of equal items, so that at each
// site some results are cut to a prefix exactly at the budget, and others
// to one a token or two under it, with the next prefix just over.
func TestTrimAtEveryBoundary(t *testing.T) {
	t.Parallel()
	for _, site := range trimSites {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			kept := map[int]int{}
			for length := 100; length < 350; length++ {
				kept[checkTrim(t, site, repeatTexts(40, length), length)]++
			}
			for _, tokens := range []int{site.limit(), site.limit() - 1} {
				if kept[tokens] == 0 {
					t.Errorf("%s: no result was cut to %d tokens, so the sweep missed that boundary", site.name, tokens)
				}
			}
		})
	}
}

// randomTexts are n texts of mixed lengths: some empty or short, some
// long, a few far over the budget, some needing JSON escapes.
func randomTexts(r *rand.Rand, n int) []string {
	out := make([]string, n)
	for i := range out {
		var length int
		switch r.IntN(12) {
		case 0, 1:
			length = r.IntN(4)
		case 2, 3, 4, 5:
			length = r.IntN(200)
		case 6, 7, 8, 9, 10:
			length = r.IntN(600)
		default:
			length = r.IntN(8000)
		}
		unit := "a"
		if r.IntN(5) == 0 {
			unit = "<é\n"
		}
		out[i] = strings.Repeat(unit, length/len(unit)+1)[:length]
	}
	return out
}

// trimRandom compares the search with the old loop at site on a result
// of n random items.
func trimRandom(t *testing.T, site trimSite, seed uint64, n int) {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, seed>>1)) //nolint:gosec // reproducible test inputs, not security
	checkTrim(t, site, randomTexts(r, n), r.IntN(5000))
}

// TestTrimRandom compares the search with the old loop on seeded random
// results.
func TestTrimRandom(t *testing.T) {
	t.Parallel()
	for _, site := range trimSites {
		t.Run(site.name, func(t *testing.T) {
			t.Parallel()
			for seed := range uint64(150) {
				trimRandom(t, site, seed, int(seed%70))
			}
		})
	}
}

// FuzzTrim compares the search with the old loop on random results:
//
//	go test -run '^$' -fuzz FuzzTrim -fuzztime 300000x ./internal/mcpserver/
func FuzzTrim(f *testing.F) {
	for _, s := range []uint64{0, 1, 42, 1 << 40} {
		f.Add(s, uint8(s%200))
	}
	f.Fuzz(func(t *testing.T, seed uint64, n uint8) {
		for _, site := range trimSites {
			trimRandom(t, site, seed, int(n))
		}
	})
}

// TestFitPrefixSearches checks fitPrefix against a scan, and that it asks
// fits about log2(n) times rather than n.
func TestFitPrefixSearches(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 1, 2, 3, 4, 10, 100, 1000, 1 << 20} {
		for _, least := range []int{0, 1} {
			for _, fitting := range []int{0, 1, 2, n / 3, n - 2, n - 1, n} {
				if fitting < 0 || fitting > n {
					continue
				}
				calls := 0
				got := fitPrefix(n, least, func(k int) bool { calls++; return k <= fitting })
				want := n
				if n > least && fitting < n {
					want = max(fitting, least)
				}
				if got != want {
					t.Errorf("fitPrefix(%d, %d, first %d fit) = %d, want %d", n, least, fitting, got, want)
				}
				if most := 2 + bits.Len(uint(n)); calls > most {
					t.Errorf("fitPrefix(%d, %d, first %d fit) called fits %d times, want at most %d", n, least, fitting, calls, most)
				}
			}
		}
	}
}
