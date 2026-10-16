package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

func TestPrintGates(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	tests := []struct {
		name  string
		gates []proto.Gate
		want  string
	}{
		{"none", nil, "no open gates\n"},
		{"open and passed", []proto.Gate{
			{ID: "ar-g1", Title: "Approve the plan", Kind: "human", Approvers: []string{"carol", "erin"}, Holds: []string{"ar-e1"}},
			{ID: "ar-g2", Title: "Approve the deploy", Kind: "human", Approvers: []string{}, Holds: []string{"ar-d1", "ar-d2"},
				Outcome: "passed", By: "dana", At: &at},
		}, "ar-g1  open                                    carol, erin           holds ar-e1         Approve the plan\n" +
			"ar-g2  passed by dana at 2026-10-15 09:30 UTC  the project's admins  holds ar-d1, ar-d2  Approve the deploy\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printGates(&b, tc.gates)
			if b.String() != tc.want {
				t.Errorf("printGates:\n%s\nwant:\n%s", b.String(), tc.want)
			}
		})
	}
}

// show says what is a gate, and what holds an issue, with who may pass
// it and the command; and an issue's dates.
func TestPrintIssueGatesAndDates(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	due := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	from := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	s := proto.ShowResult{
		Issue: proto.Issue{ID: "ar-e1", Title: "Upgrade", Status: "open", Type: "task", Rev: 2, CreatedBy: "alice",
			CreatedAt: created, UpdatedAt: created, DueAt: &due, DeferUntil: &from},
		Gates: []proto.Gate{{ID: "ar-g1", Kind: "human", Approvers: []string{"carol"}}},
	}
	var b bytes.Buffer
	printIssue(&b, s)
	for _, want := range []string{
		"not before 2026-10-18 00:00 UTC; due 2026-10-20 17:00 UTC\n",
		"held by gate ar-g1, which carol may pass: `aries gate pass ar-g1`\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("printIssue:\n%s\nwant a line %q", b.String(), want)
		}
	}
	g := proto.ShowResult{Issue: proto.Issue{ID: "ar-g1", Title: "Approve", Status: "open", Type: "gate", Rev: 1,
		CreatedBy: "alice", CreatedAt: created, UpdatedAt: created},
		Gate: &proto.Gate{ID: "ar-g1", Kind: "human", Approvers: []string{}, Holds: []string{"ar-e1"}}}
	b.Reset()
	printIssue(&b, g)
	if want := "human gate, passed by the project's admins; holds ar-e1\n"; !strings.Contains(b.String(), want) {
		t.Errorf("printIssue(gate):\n%s\nwant a line %q", b.String(), want)
	}
}

// list shows an issue's dates after its title.
func TestPrintListDates(t *testing.T) {
	t.Parallel()
	due := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	printList(&b, []proto.Summary{{ID: "ar-a1", Title: "dated", Status: "open", Priority: 2, DueAt: &due},
		{ID: "ar-a2", Title: "plain", Status: "open", Priority: 2}})
	want := "ar-a1  P2  open  dated  due 2026-10-20 17:00 UTC\nar-a2  P2  open  plain\n"
	if b.String() != want {
		t.Errorf("printList:\n%s\nwant:\n%s", b.String(), want)
	}
}

// A ci gate in a list says what it checks and its last report; an
// issue's checks say how each stands, and a closed issue whether it
// landed.
func TestPrintCIGates(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 15, 9, 30, 0, 0, time.UTC)
	failed := &proto.GateReport{Result: "failed", By: "ci-example", At: at, SHA: "0123456789abcdef", URL: "https://ci.example.com/runs/7", Reason: "2 tests failed"}
	var b bytes.Buffer
	printGates(&b, []proto.Gate{{ID: "ar-g1", Title: "build of ar-a1", Kind: "ci", Approvers: []string{"ci-example"}, Check: "build",
		Subject: "ar-a1", Holds: []string{"ar-b1"}, Report: failed}})
	if want := "ar-g1  failed at 2026-10-15 09:30 UTC  ci-example  holds ar-b1  build of ar-a1\n"; b.String() != want {
		t.Errorf("printGates(ci):\n%s\nwant:\n%s", b.String(), want)
	}
	created := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		status string
		checks []proto.Gate
		want   []string
	}{
		{"a failed build", "in_progress", []proto.Gate{{ID: "ar-g1", Kind: "ci", Check: "build", Approvers: []string{"ci-example"}, Report: failed}},
			[]string{"check build: failed at 0123456789ab: https://ci.example.com/runs/7 2 tests failed (gate ar-g1, reported by ci-example)\n"}},
		{"closed, not landed", "closed", []proto.Gate{
			{ID: "ar-g2", Kind: "ci", Check: "landed", Approvers: []string{"ci-example"}}},
			[]string{"check landed: waits for ci-example (gate ar-g2)\n", "closed, not landed\n"}},
		{"closed and landed", "closed", []proto.Gate{
			{ID: "ar-g2", Kind: "ci", Check: "landed", Approvers: []string{"ci-example"}, Outcome: "passed", By: "ci-example", At: &at,
				Report: &proto.GateReport{Result: "passed", By: "ci-example", At: at, SHA: "fedcba9"}}},
			[]string{"check landed: passed at fedcba9 (gate ar-g2, reported by ci-example)\n", "closed, landed 2026-10-15 09:30 UTC\n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := proto.ShowResult{Issue: proto.Issue{ID: "ar-a1", Title: "Do it", Status: tc.status, Type: "task", Rev: 3,
				CreatedBy: "alice", CreatedAt: created, UpdatedAt: created}, Checks: tc.checks}
			var b bytes.Buffer
			printIssue(&b, s)
			for _, want := range tc.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("printIssue:\n%s\nwant a line %q", b.String(), want)
				}
			}
		})
	}
}
