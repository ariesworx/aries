package store

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestReadyAndBlocked(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	mk := func(title string, in NewIssue) IssueID {
		in.Title = title
		return mustCreate(t, s, in).ID
	}
	dep := func(from, to IssueID, typ DepType) {
		t.Helper()
		if err := s.AddDep(ctx, alice, from, to, typ, ""); err != nil {
			t.Fatalf("dep %s -> %s: %v", from, to, err)
		}
	}

	free := mk("free", NewIssue{})
	blocker := mk("blocker", NewIssue{Priority: prio(P1)})
	blockedOpen := mk("blocked by open", NewIssue{})
	closedBlocker := mk("closed blocker", NewIssue{})
	blockedClosed := mk("blocked by closed", NewIssue{})
	condTarget := mk("cond target", NewIssue{Priority: prio(P3)})
	condBlocked := mk("cond blocked", NewIssue{})
	relatedOnly := mk("related only", NewIssue{})
	epic := mk("epic", NewIssue{Type: TypeEpic})
	child := mk("child", NewIssue{ParentID: epic})
	grandchild := mk("grandchild", NewIssue{ParentID: child})
	deferFuture := mk("defer future", NewIssue{DeferUntil: &future})
	deferPast := mk("defer past", NewIssue{DeferUntil: &past})
	deferParent := mk("defer parent", NewIssue{DeferUntil: &future})
	deferChild := mk("defer child", NewIssue{ParentID: deferParent})
	statusDeferred := mk("status deferred", NewIssue{Status: StatusDeferred})
	inProgress := mk("in progress", NewIssue{Status: StatusInProgress})
	template := mk("template", NewIssue{Template: true})
	done := mk("done", NewIssue{})
	urgent := mk("urgent", NewIssue{Priority: prio(P0)})

	dep(blockedOpen, blocker, DepBlocks)
	dep(blockedClosed, closedBlocker, DepBlocks)
	dep(condBlocked, condTarget, DepConditionalBlocks)
	dep(relatedOnly, blocker, DepRelated)
	dep(epic, blocker, DepBlocks)
	for _, id := range []IssueID{closedBlocker, done} {
		if _, err := s.CloseIssue(ctx, alice, id, 0, ""); err != nil {
			t.Fatal(err)
		}
	}

	ready := readyIDs(t, s)
	// Priority first, then creation order.
	want := []IssueID{urgent, blocker, free, blockedClosed, relatedOnly, deferPast, condTarget}
	if !slices.Equal(ready, want) {
		t.Errorf("ready =\n  %v\nwant\n  %v", ready, want)
	}

	blocked, err := s.Blocked(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[IssueID]BlockedIssue{}
	for _, b := range blocked {
		got[b.ID] = b
	}
	cases := []struct {
		name     string
		id       IssueID
		by       []IssueID
		via      IssueID
		children int
	}{
		{"direct blocks", blockedOpen, []IssueID{blocker}, "", 0},
		{"conditional blocks", condBlocked, []IssueID{condTarget}, "", 0},
		{"blocked epic", epic, []IssueID{blocker}, "", 1},
		{"child of blocked epic", child, []IssueID{blocker}, epic, 1},
		{"grandchild of blocked epic", grandchild, []IssueID{blocker}, epic, 0},
		{"deferred parent", deferParent, nil, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, ok := got[tc.id]
			if !ok {
				t.Fatalf("%s not in Blocked", tc.id)
			}
			if !slices.Equal(b.BlockedBy, tc.by) || b.Via != tc.via || b.Children != tc.children {
				t.Errorf("blocked by %v via %q, %d open children; want %v via %q, %d",
					b.BlockedBy, b.Via, b.Children, tc.by, tc.via, tc.children)
			}
		})
	}
	if len(blocked) != len(cases) {
		t.Errorf("Blocked has %d issues, want %d", len(blocked), len(cases))
	}
	for _, id := range []IssueID{deferFuture, deferChild, statusDeferred, inProgress, template, done} {
		if slices.Contains(ready, id) {
			t.Errorf("%s should not be ready", id)
		}
	}

	// Closing the blocker frees everything it held, through the epic,
	// except the epic and the child, which wait on their open children.
	if _, err := s.CloseIssue(ctx, alice, blocker, 0, ""); err != nil {
		t.Fatal(err)
	}
	ready = readyIDs(t, s)
	for _, id := range []IssueID{blockedOpen, grandchild} {
		if !slices.Contains(ready, id) {
			t.Errorf("%s should be ready once the blocker closed", id)
		}
	}
	for _, id := range []IssueID{epic, child} {
		if slices.Contains(ready, id) {
			t.Errorf("%s has an open child and should not be ready", id)
		}
	}
	if slices.Contains(ready, condBlocked) {
		t.Errorf("conditional-blocks target still open; %s should not be ready", condBlocked)
	}
}

func readyIDs(t *testing.T, s *Store) []IssueID {
	t.Helper()
	rs, err := s.Ready(t.Context(), Actor{}, 100)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]IssueID, len(rs))
	for i, r := range rs {
		ids[i] = r.ID
	}
	return ids
}

func TestCycleRefusal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	type edge struct {
		from, to int
		typ      DepType
	}
	cases := []struct {
		name     string
		parents  map[int]int // child -> parent, set at creation
		setup    []edge
		add      edge
		reparent *[2]int // child, new parent, via UpdateIssue
		want     error
	}{
		{name: "self", add: edge{0, 0, DepBlocks}, want: ErrCycle},
		{name: "self related", add: edge{0, 0, DepRelated}, want: ErrInvalid},
		{name: "two-cycle", setup: []edge{{0, 1, DepBlocks}}, add: edge{1, 0, DepBlocks}, want: ErrCycle},
		{name: "three-cycle mixed types", setup: []edge{{0, 1, DepBlocks}, {1, 2, DepWaitsFor}},
			add: edge{2, 0, DepConditionalBlocks}, want: ErrCycle},
		{name: "related back-edge allowed", setup: []edge{{0, 1, DepBlocks}}, add: edge{1, 0, DepRelated}},
		{name: "discovered-from back-edge allowed", setup: []edge{{0, 1, DepBlocks}}, add: edge{1, 0, DepDiscoveredFrom}},
		{name: "chain without cycle", setup: []edge{{0, 1, DepBlocks}}, add: edge{1, 2, DepBlocks}},
		{name: "parent blocked by its child", parents: map[int]int{1: 0}, add: edge{0, 1, DepBlocks}, want: ErrCycle},
		{name: "grandparent blocked by grandchild", parents: map[int]int{1: 0, 2: 1}, add: edge{0, 2, DepBlocks}, want: ErrCycle},
		{name: "child blocked by parent's sibling", parents: map[int]int{1: 0}, add: edge{1, 2, DepBlocks}},
		{name: "parent loop", parents: map[int]int{1: 0}, reparent: &[2]int{0, 1}, want: ErrCycle},
		// A parent waits on its open children, so a child that waits on
		// its parent, or on anything under it, would never be ready.
		{name: "child blocked by its parent", parents: map[int]int{1: 0}, add: edge{1, 0, DepBlocks}, want: ErrCycle},
		{name: "grandchild blocked by grandparent", parents: map[int]int{1: 0, 2: 1}, add: edge{2, 0, DepBlocks}, want: ErrCycle},
		{name: "siblings in order", parents: map[int]int{1: 0, 2: 0}, add: edge{2, 1, DepBlocks}},
		{name: "child blocked by a blocker of its parent", parents: map[int]int{1: 0}, setup: []edge{{0, 2, DepBlocks}},
			add: edge{1, 2, DepBlocks}},
		{name: "blocker's child blocked by the issue it holds", parents: map[int]int{2: 1}, setup: []edge{{0, 1, DepBlocks}},
			add: edge{2, 0, DepBlocks}, want: ErrCycle},
		{name: "reparent under what it blocks on", setup: []edge{{0, 1, DepBlocks}}, reparent: &[2]int{0, 1}, want: ErrCycle},
		{name: "reparent under a sibling's parent", parents: map[int]int{1: 0}, reparent: &[2]int{2, 0}},
		{name: "reparent across a block", setup: []edge{{1, 0, DepBlocks}}, reparent: &[2]int{0, 1}, want: ErrCycle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids := make([]IssueID, 3)
			for i := range ids {
				in := NewIssue{Title: tc.name}
				if p, ok := tc.parents[i]; ok {
					in.ParentID = ids[p]
				}
				ids[i] = mustCreate(t, s, in).ID
			}
			for _, e := range tc.setup {
				if err := s.AddDep(ctx, alice, ids[e.from], ids[e.to], e.typ, ""); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			var err error
			if tc.reparent != nil {
				is, gerr := s.GetIssue(ctx, ids[tc.reparent[0]])
				if gerr != nil {
					t.Fatal(gerr)
				}
				_, err = s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{ParentID: &ids[tc.reparent[1]]})
			} else {
				err = s.AddDep(ctx, alice, ids[tc.add.from], ids[tc.add.to], tc.add.typ, "")
			}
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if err := s.AddDep(ctx, alice, "tst-nope", "tst-nada", DepBlocks, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("dep on missing issues: %v, want ErrNotFound", err)
	}
}

// A parent, of any type, waits on its unclosed children: it is not ready
// while one is open, and Blocked lists it with how many. Its children are
// not held by that, and once the last closes it is ready again, its own
// work being to check the whole and close it.
func TestParentWaitsOnChildren(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	closeAll := func(ids ...IssueID) {
		t.Helper()
		for _, id := range ids {
			if _, err := s.CloseIssue(ctx, alice, id, 0, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	// held is what Blocked should say of an issue; nil means it is not
	// listed.
	type held struct {
		by       []IssueID
		via      IssueID
		children int
	}
	type want struct {
		ready bool
		held  *held
	}
	cases := []struct {
		name string
		// build makes the issues and returns them by name.
		build func() map[string]IssueID
		want  map[string]want
	}{
		{name: "open children", build: func() map[string]IssueID {
			e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic, Priority: prio(P0)}).ID
			a := mustCreate(t, s, NewIssue{Title: "a", ParentID: e}).ID
			b := mustCreate(t, s, NewIssue{Title: "b", ParentID: e, Status: StatusInProgress}).ID
			return map[string]IssueID{"epic": e, "a": a, "b": b}
		}, want: map[string]want{
			"epic": {held: &held{children: 2}},
			"a":    {ready: true},
		}},
		{name: "some closed", build: func() map[string]IssueID {
			e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic}).ID
			a := mustCreate(t, s, NewIssue{Title: "a", ParentID: e}).ID
			b := mustCreate(t, s, NewIssue{Title: "b", ParentID: e}).ID
			closeAll(a)
			return map[string]IssueID{"epic": e, "b": b}
		}, want: map[string]want{
			"epic": {held: &held{children: 1}},
			"b":    {ready: true},
		}},
		{name: "all closed", build: func() map[string]IssueID {
			e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic}).ID
			a := mustCreate(t, s, NewIssue{Title: "a", ParentID: e}).ID
			b := mustCreate(t, s, NewIssue{Title: "b", ParentID: e}).ID
			closeAll(a, b)
			return map[string]IssueID{"epic": e}
		}, want: map[string]want{
			"epic": {ready: true},
		}},
		{name: "deferred child", build: func() map[string]IssueID {
			e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic}).ID
			mustCreate(t, s, NewIssue{Title: "later", ParentID: e, Status: StatusDeferred})
			return map[string]IssueID{"epic": e}
		}, want: map[string]want{
			"epic": {held: &held{children: 1}},
		}},
		{name: "nested", build: func() map[string]IssueID {
			e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic}).ID
			f := mustCreate(t, s, NewIssue{Title: "feature", Type: TypeFeature, ParentID: e}).ID
			k := mustCreate(t, s, NewIssue{Title: "task", ParentID: f}).ID
			return map[string]IssueID{"epic": e, "feature": f, "task": k}
		}, want: map[string]want{
			"epic":    {held: &held{children: 1}},
			"feature": {held: &held{children: 1}},
			"task":    {ready: true},
		}},
		{name: "blocked as well", build: func() map[string]IssueID {
			x := mustCreate(t, s, NewIssue{Title: "blocker"}).ID
			e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic}).ID
			a := mustCreate(t, s, NewIssue{Title: "a", ParentID: e}).ID
			if err := s.AddDep(ctx, alice, e, x, DepBlocks, ""); err != nil {
				t.Fatal(err)
			}
			return map[string]IssueID{"blocker": x, "epic": e, "a": a}
		}, want: map[string]want{
			"blocker": {ready: true},
			"epic":    {held: &held{by: []IssueID{"blocker"}, children: 1}},
			"a":       {held: &held{by: []IssueID{"blocker"}, via: "epic"}},
		}},
		{name: "a task with subtasks", build: func() map[string]IssueID {
			p := mustCreate(t, s, NewIssue{Title: "task"}).ID
			c := mustCreate(t, s, NewIssue{Title: "subtask", ParentID: p}).ID
			return map[string]IssueID{"task": p, "subtask": c}
		}, want: map[string]want{
			"task":    {held: &held{children: 1}},
			"subtask": {ready: true},
		}},
		{name: "closed parent", build: func() map[string]IssueID {
			e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic}).ID
			a := mustCreate(t, s, NewIssue{Title: "a", ParentID: e}).ID
			closeAll(e)
			return map[string]IssueID{"epic": e, "a": a}
		}, want: map[string]want{
			"a": {ready: true},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ids := tc.build()
			ready := readyIDs(t, s)
			blocked, err := s.Blocked(ctx, 500)
			if err != nil {
				t.Fatal(err)
			}
			got := map[IssueID]BlockedIssue{}
			for _, b := range blocked {
				got[b.ID] = b
			}
			for name, id := range ids {
				w := tc.want[name]
				if r := slices.Contains(ready, id); r != w.ready {
					t.Errorf("%s ready = %t, want %t", name, r, w.ready)
				}
				b, listed := got[id]
				if listed != (w.held != nil) {
					t.Errorf("%s in Blocked = %t, want %t", name, listed, w.held != nil)
					continue
				}
				if !listed {
					continue
				}
				var by []IssueID
				for _, n := range w.held.by {
					by = append(by, ids[string(n)])
				}
				via := ids[string(w.held.via)]
				if !slices.Equal(b.BlockedBy, by) || b.Via != via || b.Children != w.held.children {
					t.Errorf("%s blocked by %v via %q, %d open children; want %v via %q, %d",
						name, b.BlockedBy, b.Via, b.Children, by, via, w.held.children)
				}
			}
		})
	}
}

// start with no id never takes a parent whose children are open, even
// one ranked ahead of them.
func TestStartSkipsWaitingParent(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic, Priority: prio(P0)})
	c := mustCreate(t, s, NewIssue{Title: "child", ParentID: e.ID, Priority: prio(P3)})
	is, _, err := s.StartIssue(t.Context(), alice, "", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if is.ID != c.ID {
		t.Errorf("start took %s, want the child %s", is.ID, c.ID)
	}
	if _, _, err := s.StartIssue(t.Context(), bob, "", 0, false); !errors.Is(err, ErrNothingReady) {
		t.Errorf("start with only the epic left = %v, want ErrNothingReady", err)
	}
}

// The bug's own refusal: an epic made to wait on its child is told it
// already does, as a ParentWaitError.
func TestParentWaitRefusal(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	e := mustCreate(t, s, NewIssue{Title: "epic", Type: TypeEpic})
	c := mustCreate(t, s, NewIssue{Title: "child", ParentID: e.ID})
	g := mustCreate(t, s, NewIssue{Title: "grandchild", ParentID: c.ID})
	for _, to := range []IssueID{c.ID, g.ID} {
		err := s.AddDep(t.Context(), alice, e.ID, to, DepBlocks, "")
		var pw *ParentWaitError
		if !errors.As(err, &pw) || !errors.Is(err, ErrCycle) || pw.Parent != e.ID || pw.Child != to {
			t.Errorf("dep add %s %s = %v, want a ParentWaitError of %s and %s", e.ID, to, err, e.ID, to)
		}
	}
}
