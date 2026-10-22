package merges

import (
	"context"
	"errors"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

type fakeStore struct {
	shows    map[string]proto.ShowResult
	comments map[string][]proto.Comment
}

func (f *fakeStore) Show(_ context.Context, id string) (proto.ShowResult, error) {
	s, ok := f.shows[id]
	if !ok {
		return proto.ShowResult{}, errors.New("not found")
	}
	return s, nil
}

func (f *fakeStore) Comments(_ context.Context, id string) ([]proto.Comment, error) {
	return f.comments[id], nil
}

func issue(id, typ, parent string, deps ...string) proto.ShowResult {
	s := proto.ShowResult{Issue: proto.Issue{ID: id, Type: typ, Parent: parent}}
	for _, d := range deps {
		s.Deps = append(s.Deps, proto.Dep{From: id, To: d, Type: "blocks"})
	}
	return s
}

func gate(id, kind, outcome string) proto.ShowResult {
	s := issue(id, "gate", "")
	s.Gate = &proto.Gate{ID: id, Kind: kind, Outcome: outcome}
	return s
}

func TestReadIssue(t *testing.T) {
	t.Parallel()
	base := func() *fakeStore {
		return &fakeStore{shows: map[string]proto.ShowResult{
			"ar-task0001": issue("ar-task0001", "task", "ar-epic0001"),
			"ar-epic0001": issue("ar-epic0001", "epic", "", "ar-gate0001"),
			"ar-gate0001": gate("ar-gate0001", "human", "passed"),
		}, comments: map[string][]proto.Comment{"ar-task0001": {
			{Body: "brief: harness=claude-code session=s1 template=brief@2\n\ntext"},
			{Body: "not a brief"},
			{Body: "brief: harness=claude-code-desktop session=local_2 template=brief@3\n\ntext"},
		}}}
	}
	tests := []struct {
		name     string
		edit     func(*fakeStore)
		id       string
		approved bool
		detail   string
	}{
		{"under an approved epic", nil, "ar-task0001", true, "epic ar-epic0001, gate ar-gate0001 passed"},
		{"an approved epic itself", nil, "ar-epic0001", true, "epic ar-epic0001, gate ar-gate0001 passed"},
		{"a waived gate is not an approval", func(f *fakeStore) { f.shows["ar-gate0001"] = gate("ar-gate0001", "human", "waived") },
			"ar-task0001", false, "no epic above ar-task0001 has a passed approval gate"},
		{"a ci gate is not an approval", func(f *fakeStore) { f.shows["ar-gate0001"] = gate("ar-gate0001", "ci", "passed") },
			"ar-task0001", false, "no epic above ar-task0001 has a passed approval gate"},
		{"no epic", func(f *fakeStore) { f.shows["ar-task0001"] = issue("ar-task0001", "task", "") },
			"ar-task0001", false, "no epic above ar-task0001 has a passed approval gate"},
		{"an open gate holds it", func(f *fakeStore) {
			s := f.shows["ar-task0001"]
			s.Gates = []proto.Gate{{ID: "ar-gate0002"}}
			f.shows["ar-task0001"] = s
		}, "ar-task0001", false, "open gate ar-gate0002 holds ar-task0001"},
		{"an unreadable parent", func(f *fakeStore) { delete(f.shows, "ar-epic0001") },
			"ar-task0001", false, "ar-epic0001 unreadable: not found"},
		{"an unreadable gate", func(f *fakeStore) { delete(f.shows, "ar-gate0001") },
			"ar-task0001", false, "ar-gate0001 unreadable: not found"},
		{"deps cut short", func(f *fakeStore) {
			s := issue("ar-epic0001", "epic", "")
			s.DepsMore = 3
			f.shows["ar-epic0001"] = s
		}, "ar-task0001", false, "epic ar-epic0001 has more dependencies than show lists"},
		{"a cycle of parents", func(f *fakeStore) { f.shows["ar-task0001"] = issue("ar-task0001", "task", "ar-task0001") },
			"ar-task0001", false, "no epic above ar-task0001 has a passed approval gate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := base()
			if tt.edit != nil {
				tt.edit(f)
			}
			got := ReadIssue(context.Background(), f, tt.id)
			if got.Err != "" || got.Epic.Approved != tt.approved || got.Epic.Detail != tt.detail {
				t.Errorf("ReadIssue = %+v, want approved %v detail %q", got, tt.approved, tt.detail)
			}
		})
	}
	got := ReadIssue(context.Background(), base(), "ar-task0001")
	if want := "harness=claude-code-desktop session=local_2 template=brief@3"; got.Brief != want {
		t.Errorf("brief %q, want the last one, %q", got.Brief, want)
	}
	if got := ReadIssue(context.Background(), base(), "ar-none0001"); got.Err != "not found" {
		t.Errorf("an unreadable issue: %+v", got)
	}
}
