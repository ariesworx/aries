package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
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
			{ID: "sf-g1", Title: "Approve the plan", Kind: "human", Approvers: []string{"carol", "erin"}, Holds: []string{"sf-e1"}},
			{ID: "sf-g2", Title: "Approve the deploy", Kind: "human", Approvers: []string{}, Holds: []string{"sf-d1", "sf-d2"},
				Outcome: "passed", By: "dana", At: &at},
		}, "sf-g1  open                                    carol, erin           holds sf-e1         Approve the plan\n" +
			"sf-g2  passed by dana at 2026-10-15 09:30 UTC  the project's admins  holds sf-d1, sf-d2  Approve the deploy\n"},
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
		Issue: proto.Issue{ID: "sf-e1", Title: "Upgrade", Status: "open", Type: "task", Rev: 2, CreatedBy: "alice",
			CreatedAt: created, UpdatedAt: created, DueAt: &due, DeferUntil: &from},
		Gates: []proto.Gate{{ID: "sf-g1", Kind: "human", Approvers: []string{"carol"}}},
	}
	var b bytes.Buffer
	printIssue(&b, s)
	for _, want := range []string{
		"not before 2026-10-18 00:00 UTC; due 2026-10-20 17:00 UTC\n",
		"held by gate sf-g1, which carol may pass: `sfx gate pass sf-g1`\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("printIssue:\n%s\nwant a line %q", b.String(), want)
		}
	}
	g := proto.ShowResult{Issue: proto.Issue{ID: "sf-g1", Title: "Approve", Status: "open", Type: "gate", Rev: 1,
		CreatedBy: "alice", CreatedAt: created, UpdatedAt: created},
		Gate: &proto.Gate{ID: "sf-g1", Kind: "human", Approvers: []string{}, Holds: []string{"sf-e1"}}}
	b.Reset()
	printIssue(&b, g)
	if want := "human gate, passed by the project's admins; holds sf-e1\n"; !strings.Contains(b.String(), want) {
		t.Errorf("printIssue(gate):\n%s\nwant a line %q", b.String(), want)
	}
}

// list shows an issue's dates after its title.
func TestPrintListDates(t *testing.T) {
	t.Parallel()
	due := time.Date(2026, 10, 20, 17, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	printList(&b, []proto.Summary{{ID: "sf-a1", Title: "dated", Status: "open", Priority: 2, DueAt: &due},
		{ID: "sf-a2", Title: "plain", Status: "open", Priority: 2}})
	want := "sf-a1  P2  open  dated  due 2026-10-20 17:00 UTC\nsf-a2  P2  open  plain\n"
	if b.String() != want {
		t.Errorf("printList:\n%s\nwant:\n%s", b.String(), want)
	}
}
