package e2e

import (
	"strings"
	"testing"
)

// ARIES_SOAK_PARTITION turns the partition mode on, and takes nothing
// but a yes or nothing.
func TestSoakPartitionConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value         string
		want, refused bool
	}{
		{"", false, false}, {"1", true, false}, {"true", true, false}, {"on", true, false},
		{"0", false, true}, {"10m", false, true},
	} {
		cfg, err := soakConfigFrom(func(k string) string {
			if k == "ARIES_SOAK_PARTITION" {
				return tc.value
			}
			return ""
		})
		if (err != nil) != tc.refused || cfg.partition != tc.want {
			t.Errorf("soakConfigFrom with ARIES_SOAK_PARTITION=%q = partition %t, %v; want %t, refused %t", tc.value, cfg.partition, err, tc.want, tc.refused)
		}
	}
}

// The partition check must catch what it claims to: these feed it what
// sessions were told of their queued writes, what their outboxes hold at
// rest, what the server answered the replays, and what the event log
// stamped with each key, breaking one rule each, and some that break
// none.
func TestPartitionFaults(t *testing.T) {
	t.Parallel()
	alice := sessKey{"alice", "mcp-alice-0g0"}
	finish := queuedWrite{who: alice, entry: 1, op: "finish", idem: "k-1", markers: []string{"done alice-0g0-3", "found alice-0g0-3-0"}}
	comment := queuedWrite{who: alice, entry: 2, op: "comment", idem: "k-2", markers: []string{"note alice-0g0-4"}}
	label := queuedWrite{who: alice, entry: 3, op: "label.add", idem: "k-3"}
	applied := func(op string) []stampRec { return []stampRec{{seq: 10, op: op}} }
	tests := []struct {
		name    string
		queued  []queuedWrite
		end     partitionEnd
		want    []string // a part of each violation's text, in order; none for a sound run
		wantNot bool
	}{
		{name: "applied, kept and refused, each as told",
			queued: []queuedWrite{finish, comment, label},
			end: partitionEnd{
				refused: map[string]refusal{"k-3": {code: "not_found", message: "issue sf-x not found"}},
				answers: map[string][]answer{"k-1": {{out: acked, kept: true}}, "k-2": {{out: lost}, {out: acked}}, "k-3": {{out: refused}}},
				stamps:  map[string][]stampRec{"k-1": applied("issue.kept"), "k-2": applied("comment.add")},
				markers: map[string]int{"done alice-0g0-3": 1, "found alice-0g0-3-0": 1, "note alice-0g0-4": 1},
			}},
		{name: "a label that changed nothing is applied with no event",
			queued: []queuedWrite{label},
			end:    partitionEnd{answers: map[string][]answer{"k-3": {{out: acked}}}}},
		{name: "lost: the server answered it, and the log does not have it",
			queued: []queuedWrite{comment},
			end:    partitionEnd{answers: map[string][]answer{"k-2": {{out: acked}}}},
			want:   []string{"lost: comment"}},
		{name: "lost: its marker is not in the log",
			queued: []queuedWrite{finish},
			end: partitionEnd{answers: map[string][]answer{"k-1": {{out: acked}}}, stamps: map[string][]stampRec{"k-1": applied("issue.close")},
				markers: map[string]int{"done alice-0g0-3": 1}},
			want: []string{`"found alice-0g0-3-0" is in the event log 0 times`}},
		{name: "applied twice: two events stamped with its key",
			queued: []queuedWrite{comment},
			end: partitionEnd{answers: map[string][]answer{"k-2": {{out: acked}}}, stamps: map[string][]stampRec{"k-2": {{seq: 10, op: "comment.add"}, {seq: 12, op: "comment.add"}}},
				markers: map[string]int{"note alice-0g0-4": 1}},
			want: []string{"applied 2 times"}},
		{name: "applied twice: its marker is there twice",
			queued: []queuedWrite{comment},
			end: partitionEnd{answers: map[string][]answer{"k-2": {{out: acked}}}, stamps: map[string][]stampRec{"k-2": applied("comment.add")},
				markers: map[string]int{"note alice-0g0-4": 2}},
			want: []string{`"note alice-0g0-4" is in the event log 2 times`}},
		{name: "silently dropped: gone from the outbox, never answered, not in the log",
			queued: []queuedWrite{label},
			want:   []string{"silently dropped"}},
		{name: "still waiting at rest",
			queued: []queuedWrite{comment},
			end:    partitionEnd{pending: map[string]bool{"k-2": true}},
			want:   []string{"still waits"}},
		{name: "refused with no reason the session could see",
			queued: []queuedWrite{label},
			end:    partitionEnd{refused: map[string]refusal{"k-3": {}}, answers: map[string][]answer{"k-3": {{out: refused}}}},
			want:   []string{"refused with no reason"}},
		{name: "refused, and applied all the same",
			queued: []queuedWrite{comment},
			end: partitionEnd{refused: map[string]refusal{"k-2": {code: "invalid", message: "bad"}}, stamps: map[string][]stampRec{"k-2": applied("comment.add")},
				markers: map[string]int{"note alice-0g0-4": 1}},
			want: []string{"refused (invalid), but the log applied it, and no attempt of it went unanswered"}},
		{name: "refused unsent as access ended, applied by an attempt whose answer was lost",
			queued: []queuedWrite{comment},
			end: partitionEnd{refused: map[string]refusal{"k-2": {code: "forbidden", message: "alice is not a member"}}, lost: map[string]bool{"k-2": true},
				stamps: map[string][]stampRec{"k-2": applied("comment.add")}, markers: map[string]int{"note alice-0g0-4": 1}}},
		{name: "refused by the server when replayed, though it had applied it",
			queued: []queuedWrite{comment},
			end: partitionEnd{refused: map[string]refusal{"k-2": {code: "invalid", message: "bad"}}, lost: map[string]bool{"k-2": true},
				answers: map[string][]answer{"k-2": {{out: refused}}},
				stamps:  map[string][]stampRec{"k-2": applied("comment.add")}, markers: map[string]int{"note alice-0g0-4": 1}},
			want: []string{"refused (invalid) when replayed, but the log applied it"}},
		{name: "told it was kept, and the log applied it",
			queued: []queuedWrite{finish},
			end: partitionEnd{answers: map[string][]answer{"k-1": {{out: acked, kept: true}}}, stamps: map[string][]stampRec{"k-1": applied("issue.close")},
				markers: map[string]int{"done alice-0g0-3": 1, "found alice-0g0-3-0": 1}},
			want: []string{"told it was kept as a comment, but the log applied it"}},
		{name: "told it applied, and the log kept it",
			queued: []queuedWrite{finish},
			end: partitionEnd{answers: map[string][]answer{"k-1": {{out: acked}}}, stamps: map[string][]stampRec{"k-1": applied("issue.kept")},
				markers: map[string]int{"done alice-0g0-3": 1, "found alice-0g0-3-0": 1}},
			want: []string{"told it applied, but the log kept it as a comment"}},
		{name: "a replay of a write no session queued",
			end:  partitionEnd{answers: map[string][]answer{"k-9": {{out: acked}}}},
			want: []string{"replayed a write with key k-9, which no session was told it queued"}},
		{name: "queued with no key",
			queued: []queuedWrite{{who: alice, entry: 4, op: "comment"}},
			want:   []string{"no entry 4 in its outbox"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := partitionFaults(tc.queued, tc.end)
			if len(got) != len(tc.want) {
				t.Fatalf("partitionFaults = %v, want %d violations naming %q", got, len(tc.want), tc.want)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i].msg, w) {
					t.Errorf("violation %d = %q, want it to say %q", i, got[i].msg, w)
				}
			}
		})
	}
}
