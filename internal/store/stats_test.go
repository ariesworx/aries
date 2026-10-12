package store

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"
)

// writeStats is fed the time by its caller, so nothing here waits.
func TestWriteStatsSnapshot(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	ms := time.Millisecond
	type write struct {
		after time.Duration // since t0
		took  time.Duration
	}
	seq := func(n int, every, took time.Duration) []write {
		var out []write
		for i := range n {
			out = append(out, write{time.Duration(i) * every, took})
		}
		return out
	}
	tests := []struct {
		name   string
		writes []write
		at     time.Duration // when the snapshot is taken, since t0
		want   WriteStats
	}{
		{name: "no writes", at: time.Hour},
		{name: "one write", writes: []write{{0, 7 * ms}}, at: time.Second,
			want: WriteStats{LastMinute: 1, Samples: 1, P50: 7 * ms, P99: 7 * ms}},
		{name: "the median and the slowest of a hundred",
			writes: func() []write {
				var out []write
				for i := range 100 {
					out = append(out, write{time.Duration(i) * 100 * ms, time.Duration(i+1) * ms})
				}
				return out
			}(), at: 30 * time.Second,
			want: WriteStats{LastMinute: 100, Samples: 100, P50: 50 * ms, P99: 99 * ms}},
		{name: "writes older than a minute are not counted, and still sampled",
			writes: []write{{0, 3 * ms}, {10 * time.Second, 5 * ms}, {90 * time.Second, 9 * ms}}, at: 100 * time.Second,
			want: WriteStats{LastMinute: 1, Samples: 3, P50: 5 * ms, P99: 9 * ms}},
		{name: "a write exactly a minute old has left the minute",
			writes: []write{{0, 3 * ms}, {time.Second, 3 * ms}}, at: 60 * time.Second,
			want: WriteStats{LastMinute: 1, Samples: 2, P50: 3 * ms, P99: 3 * ms}},
		{name: "a second reused a minute later starts from zero",
			writes: []write{{0, ms}, {0, ms}, {60 * time.Second, 2 * ms}}, at: 60 * time.Second,
			want: WriteStats{LastMinute: 1, Samples: 3, P50: ms, P99: 2 * ms}},
		{name: "only the latest samples are kept",
			writes: append(seq(writeSamples, ms, time.Second), seq(writeSamples, ms, 2*ms)...), at: time.Second,
			want: WriteStats{LastMinute: 2 * writeSamples, Samples: writeSamples, P50: 2 * ms, P99: 2 * ms}},
		{name: "long idle", writes: seq(10, time.Second, 4*ms), at: 24 * time.Hour,
			want: WriteStats{Samples: 10, P50: 4 * ms, P99: 4 * ms}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ws writeStats
			for _, w := range tc.writes {
				ws.record(t0.Add(w.after), w.took)
			}
			if got := ws.snapshot(t0.Add(tc.at)); got != tc.want {
				t.Errorf("snapshot after %d writes, at %s = %+v, want %+v", len(tc.writes), tc.at, got, tc.want)
			}
		})
	}
}

// A store counts the writes that changed something, and none that were
// refused or changed nothing.
func TestStoreWriteStats(t *testing.T) {
	s := newStore(t)
	if got := s.WriteStats(); got != (WriteStats{}) {
		t.Fatalf("WriteStats() of a store just opened = %+v, want zero", got)
	}
	is := mustCreate(t, s, NewIssue{Title: "one"})
	if _, err := s.AddComment(t.Context(), bob, is.ID, "a note", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComment(t.Context(), bob, "tst-nosuchid", "refused", ""); err == nil {
		t.Fatal("a comment on an issue that is not there was accepted")
	}
	// Nothing has lapsed, so the reaper's transaction changes nothing.
	if _, err := s.ReapClaims(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := s.WriteStats()
	if got.LastMinute != 2 || got.Samples != 2 || got.P50 <= 0 || got.P99 < got.P50 {
		t.Errorf("WriteStats() after two writes, a refusal and an empty reap = %+v; want 2 in the last minute, 2 samples and their latency", got)
	}
}

// Writes counts every write since the store was opened, past the minute
// and the samples WriteStats keeps, and none that changed nothing.
func TestStoreWrites(t *testing.T) {
	var ws writeStats
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	for i := range 3 * writeSamples {
		ws.record(t0.Add(time.Duration(i)*time.Second), time.Millisecond)
	}
	if got := ws.total(); got != 3*writeSamples {
		t.Errorf("total() after %d writes over %d seconds = %d", 3*writeSamples, 3*writeSamples, got)
	}

	s := newStore(t)
	if got := s.Writes(); got != 0 {
		t.Fatalf("Writes() of a store just opened = %d, want 0", got)
	}
	is := mustCreate(t, s, NewIssue{Title: "one"})
	if _, err := s.AddComment(t.Context(), bob, is.ID, "a note", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComment(t.Context(), bob, "tst-nosuchid", "refused", ""); err == nil {
		t.Fatal("a comment on an issue that is not there was accepted")
	}
	if _, err := s.ReapClaims(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.Writes(); got != 2 {
		t.Errorf("Writes() after two writes, a refusal and an empty reap = %d, want 2", got)
	}
}

func TestControlCountsAndProjects(t *testing.T) {
	c := newControl(t)
	if got := c.Counts(); len(got.Projects) != 0 || len(got.Principals) != 0 {
		t.Fatalf("Counts() of an empty store = %+v, want none", got)
	}
	mustProject(t, c, projB)
	mustProject(t, c, projA)
	if _, err := c.AddProject(t.Context(), dana, newProject(projC)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ArchiveProject(t.Context(), dana, projB); err != nil {
		t.Fatal(err)
	}
	mustPrincipal(t, c, "alice", projA+"=admin")
	mustPrincipal(t, c, "bob", projA+"=member")
	mustPrincipal(t, c, "carol")
	if _, err := c.DisablePrincipal(t.Context(), dana, "bob"); err != nil {
		t.Fatal(err)
	}
	check := func(when string, c *Control) {
		t.Helper()
		got := c.Counts()
		wantProjects := map[ProjectState]int{ProjectActive: 1, ProjectArchived: 1, ProjectCreating: 1}
		wantPrincipals := map[PrincipalState]int{PrincipalActive: 2, PrincipalDisabled: 1}
		if !maps.Equal(got.Projects, wantProjects) || !maps.Equal(got.Principals, wantPrincipals) {
			t.Errorf("%s: Counts() = %+v, want projects %v and principals %v", when, got, wantProjects, wantPrincipals)
		}
		var names []string
		for _, p := range c.KnownProjects() {
			names = append(names, p.Name+"="+string(p.State))
		}
		if want := []string{"proj-a=active", "proj-b=archived", "proj-c=creating"}; !slices.Equal(names, want) {
			t.Errorf("%s: KnownProjects() = %v, want %v, by name", when, names, want)
		}
	}
	check("after the writes", c)
	// The same from a store that only loaded them.
	if err := c.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}
	check("after a reload", c)
}

// SessionsSeen counts what Who lists, in the same windows.
func TestSessionsSeen(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	if err := s.TouchAgent(ctx, Actor{Principal: "carol", Session: "sess-c", Machine: "laptop-c"}, ""); err != nil {
		t.Fatal(err)
	}
	clk.add(50 * time.Minute)
	if err := s.TouchAgent(ctx, bob, ""); err != nil {
		t.Fatal(err)
	}
	clk.add(4 * time.Minute)
	if err := s.TouchAgent(ctx, alice, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		since time.Duration
		want  int
	}{{0, 2}, {time.Minute, 1}, {time.Hour, 3}, {MaxAgentWindow, 3}} {
		got, err := s.SessionsSeen(ctx, tc.since)
		who, werr := s.Who(ctx, tc.since)
		if err != nil || werr != nil || got != tc.want || got != len(who) {
			t.Errorf("SessionsSeen(%s) = %d, %v; want %d, as many as Who lists (%d, %v)", tc.since, got, err, tc.want, len(who), werr)
		}
	}
	for _, bad := range []time.Duration{-time.Second, MaxAgentWindow + time.Second} {
		if _, err := s.SessionsSeen(ctx, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("SessionsSeen(%s) = %v, want ErrInvalid", bad, err)
		}
	}
}
