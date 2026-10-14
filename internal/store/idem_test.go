package store

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Each create-type operation, repeated with the same key by the same
// principal, returns its first result and writes nothing more.
func TestIdempotentReplay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// do runs the operation once with key and returns its result.
		do func(t *testing.T, s *Store, id IssueID, key string) (any, error)
	}{
		{"create", func(t *testing.T, s *Store, _ IssueID, key string) (any, error) {
			return s.CreateIssue(t.Context(), alice, NewIssue{Title: "once", IdempotencyKey: key, Labels: []string{"area:db"}})
		}},
		{"comment", func(t *testing.T, s *Store, id IssueID, key string) (any, error) {
			return s.AddComment(t.Context(), alice, id, "looked into it", key)
		}},
		{"handoff with release", func(t *testing.T, s *Store, id IssueID, key string) (any, error) {
			return s.HandoffIssue(t.Context(), alice, id, 0, HandoffNote{Note: "over to you",
				HandoffFields: HandoffFields{State: HandoffPartial}}, true, key, nil)
		}},
		{"finish with discovered work", func(t *testing.T, s *Store, id IssueID, key string) (any, error) {
			is, ids, err := s.FinishIssue(t.Context(), alice, id, 0, Finish{Reason: "done", IdempotencyKey: key,
				Handoff:    HandoffNote{Note: "shipped"},
				Discovered: []NewIssue{{Title: "follow-up"}, {Title: "another"}}})
			return struct {
				Issue Issue
				IDs   []IssueID
			}{is, ids}, err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			is := mustCreate(t, s, NewIssue{Title: "work"})
			mustStart(t, s, alice, is.ID)
			first, err := tc.do(t, s, is.ID, "k-1")
			if err != nil {
				t.Fatalf("first: %v", err)
			}
			seq := lastSeq(t, s)
			again, err := tc.do(t, s, is.ID, "k-1")
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if !reflect.DeepEqual(again, first) {
				t.Errorf("replay = %+v\nwant     %+v", again, first)
			}
			if got := lastSeq(t, s); got != seq {
				t.Errorf("replay wrote events: seq %d, want %d", got, seq)
			}
			evs, err := s.Events(t.Context(), 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			keyed := 0
			for _, e := range evs {
				if e.IdemKey == "k-1" {
					keyed++
				}
			}
			if keyed != 1 {
				t.Errorf("%d events carry the key, want 1", keyed)
			}
			assertGapless(t, s)
		})
	}
}

// queuedOps are the ops protocol 6 gives a key, so that the outbox can
// queue them (design §18.5): each runs once as a, with key, on the issue
// id, which has two acceptance items and the label "old", and returns
// its result.
var queuedOps = []struct {
	name string
	do   func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error)
	// other is the same op with other arguments.
	other func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error)
}{
	{"update",
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			return s.UpdateIssue(ctx, a, id, 1, IssuePatch{Title: ptr("renamed"), IdempotencyKey: key})
		},
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			return s.UpdateIssue(ctx, a, id, 1, IssuePatch{Title: ptr("renamed otherwise"), IdempotencyKey: key})
		}},
	{"label.add",
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			return nil, s.AddLabel(ctx, a, id, "new", key)
		},
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			return nil, s.AddLabel(ctx, a, id, "newer", key)
		}},
	{"label.rm",
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			return nil, s.RemoveLabel(ctx, a, id, "old", key)
		},
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			return nil, s.RemoveLabel(ctx, a, id, "other", key)
		}},
	{"dep.add",
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			to, err := s.CreateIssue(ctx, a, NewIssue{Title: "blocker", IdempotencyKey: "blocker-of-" + key})
			if err != nil {
				return nil, err
			}
			return nil, s.AddDep(ctx, a, id, to.ID, DepBlocks, key)
		},
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			to, err := s.CreateIssue(ctx, a, NewIssue{Title: "another blocker"})
			if err != nil {
				return nil, err
			}
			return nil, s.AddDep(ctx, a, id, to.ID, DepBlocks, key)
		}},
	{"accept",
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			return s.Accept(ctx, a, id, Acceptance{Tick: []int{1}, IdempotencyKey: key})
		},
		func(ctx context.Context, s *Store, a Actor, id IssueID, key string) (any, error) {
			return s.Accept(ctx, a, id, Acceptance{Tick: []int{2}, IdempotencyKey: key})
		}},
	{"usage",
		func(ctx context.Context, s *Store, a Actor, _ IssueID, key string) (any, error) {
			return s.AddUsage(ctx, a, []UsageRecord{req("r1", usageAt, 10, 1), req("r2", usageAt, 20, 2)}, key)
		},
		func(ctx context.Context, s *Store, a Actor, _ IssueID, key string) (any, error) {
			return s.AddUsage(ctx, a, []UsageRecord{req("r3", usageAt, 5, 5)}, key)
		}},
}

// usageAt is when the queued ops' usage records were made: one time for
// every sending, as a client's record has.
var usageAt = time.Now().Add(-time.Hour).Truncate(time.Second)

// alicesOther is another session of alice's: the outbox's replay goes on
// whichever connection of the principal comes next.
var alicesOther = Actor{Principal: "alice", Session: "sess-other", Machine: "laptop-b"}

// queuedTarget is the issue the queued ops act on.
func queuedTarget(t *testing.T, s *Store) IssueID {
	t.Helper()
	return mustCreate(t, s, NewIssue{Title: "work", Labels: []string{"old"}, Acceptance: "- one\n- two"}).ID
}

// Each op protocol 6 gives a key, repeated with the same key by the same
// principal, from another session, returns its first result and writes
// nothing more: the event log, the issue and the usage rows are as the
// first left them.
func TestIdempotentReplayOfQueuedOps(t *testing.T) {
	t.Parallel()
	for _, op := range queuedOps {
		t.Run(op.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			ctx := t.Context()
			id := queuedTarget(t, s)
			first, err := op.do(ctx, s, alice, id, "k-1")
			if err != nil {
				t.Fatalf("first: %v", err)
			}
			seq, issue, rows := lastSeq(t, s), mustShow(t, s, id), usageRows(t, s, "alice")
			again, err := op.do(ctx, s, alicesOther, id, "k-1")
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if !reflect.DeepEqual(again, first) {
				t.Errorf("replay = %+v\nwant     %+v", again, first)
			}
			if got := lastSeq(t, s); got != seq {
				t.Errorf("replay wrote events: seq %d, want %d", got, seq)
			}
			if got := mustShow(t, s, id); got.Rev != issue.Rev || !slices.Equal(got.Labels, issue.Labels) {
				t.Errorf("replay changed the issue: rev %d, labels %v; want rev %d, labels %v", got.Rev, got.Labels, issue.Rev, issue.Labels)
			}
			if got := usageRows(t, s, "alice"); got != rows {
				t.Errorf("replay stored usage: %d rows, want %d", got, rows)
			}
			// The same key for other arguments is still a clash.
			var ie *IdemError
			if _, err := op.other(ctx, s, alice, id, "k-1"); !errors.As(err, &ie) || ie.Key != "k-1" || ie.Expired || !errors.Is(err, ErrConflict) {
				t.Errorf("same key, other arguments: %v; want an IdemError that is not expired", err)
			}
			assertGapless(t, s)
		})
	}
}

// mustShow reads issue id.
func mustShow(t *testing.T, s *Store, id IssueID) Issue {
	t.Helper()
	is, err := s.GetIssue(t.Context(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return is
}

// TestIdempotencyWindow pins how long a key replays its request (design
// §18.5, decision 41): for the store's window, 7 days unless set, and
// after a restart; past it the key is refused as expired, which the
// server answers stale, and nothing is written. A key is never applied
// twice, in the window or past it.
func TestIdempotencyWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		window time.Duration // 0 is the default
		after  time.Duration
		// replays is whether the repeat returns the first result;
		// otherwise it is refused as expired.
		replays bool
	}{
		{"a day and a second, by default", 0, 24*time.Hour + time.Second, true},
		{"the whole default window", 0, DefaultIdemWindow, true},
		{"past the default window", 0, DefaultIdemWindow + time.Second, false},
		{"within a window of two days", 48 * time.Hour, 47 * time.Hour, true},
		{"past a window of two days", 48 * time.Hour, 49 * time.Hour, false},
		{"past a window of thirty days", 30 * 24 * time.Hour, 31 * 24 * time.Hour, false},
	} {
		for _, op := range []string{"comment", "update"} {
			// Not in parallel: each reopens its store, and Open's read of
			// information_schema failed, as "no root value found in
			// session", while sibling subtests made and dropped their
			// databases.
			t.Run(tc.name+", "+op, func(t *testing.T) {
				dsn, clk := newDSN(t), &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
				opts := Options{Now: clk.now, IdemWindow: tc.window}
				s := openStore(t, dsn, opts)
				ctx := t.Context()
				id := queuedTarget(t, s)
				do := func(s *Store) (any, error) {
					if op == "comment" {
						return s.AddComment(ctx, alice, id, "written once", "k-1")
					}
					return s.UpdateIssue(ctx, alice, id, 1, IssuePatch{Title: ptr("renamed"), IdempotencyKey: "k-1"})
				}
				first, err := do(s)
				if err != nil {
					t.Fatal(err)
				}
				clk.add(tc.after)
				// The server restarted meanwhile: the key is the database's.
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				s = openStore(t, dsn, opts)
				seq := lastSeq(t, s)
				again, err := do(s)
				var ie *IdemError
				switch {
				case tc.replays && (err != nil || !reflect.DeepEqual(again, first)):
					t.Errorf("repeat after %s = %+v, %v; want the first result %+v", tc.after, again, err, first)
				case !tc.replays && (!errors.As(err, &ie) || !ie.Expired || ie.Key != "k-1" || !ie.Used.Equal(clk.t.Add(-tc.after)) ||
					ie.Window != cmp.Or(tc.window, DefaultIdemWindow) || !errors.Is(err, ErrConflict)):
					t.Errorf("repeat after %s = %v; want the key refused as expired, used %s, in a window of %s",
						tc.after, err, clk.t.Add(-tc.after), cmp.Or(tc.window, DefaultIdemWindow))
				}
				if got := lastSeq(t, s); got != seq {
					t.Errorf("the repeat wrote events: seq %d, want %d", got, seq)
				}
			})
		}
	}
}

// An op that changes nothing records no event, so its key is not kept:
// sent again it is decided again on the state then, as a request
// without a key is.
func TestIdempotencyOfANoOp(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	id := queuedTarget(t, s)
	seq := lastSeq(t, s)
	if err := s.AddLabel(ctx, alice, id, "old", "k-1"); err != nil {
		t.Fatal(err)
	}
	if got := lastSeq(t, s); got != seq {
		t.Errorf("adding a label the issue has wrote events: seq %d, want %d", got, seq)
	}
	if err := s.RemoveLabel(ctx, alice, id, "old", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLabel(ctx, alice, id, "old", "k-1"); err != nil {
		t.Fatal(err)
	}
	if got := mustShow(t, s, id).Labels; !slices.Contains(got, "old") {
		t.Errorf("labels = %v; want old added by the key's second sending", got)
	}
}

// TestIdemWindowOption: a window that is not positive is refused, and
// none is the default.
func TestIdemWindowOption(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	if _, err := Open(t.Context(), dsn, Options{CommitInterval: -1, IdemWindow: -time.Hour}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Open with a negative window = %v, want ErrInvalid", err)
	}
	if got := openStore(t, dsn, Options{}).IdemWindow(); got != 7*24*time.Hour {
		t.Errorf("IdemWindow() with none set = %s, want %s, 7 days", got, DefaultIdemWindow)
	}
}

// A write whose first attempt loses to the same request, committed with
// the same key by another store, replays that request's result exactly:
// nothing the failed attempt put in its result survives, not even a field
// the stored result leaves out.
func TestIdempotentReplayAfterRetry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// do runs the operation once, with the key k-1.
		do func(ctx context.Context, s *Store, id IssueID) (any, error)
	}{
		{"handoff", func(ctx context.Context, s *Store, id IssueID) (any, error) {
			return s.HandoffIssue(ctx, alice, id, 0, HandoffNote{Note: "over to you"}, false, "k-1", nil)
		}},
		{"finish", func(ctx context.Context, s *Store, id IssueID) (any, error) {
			is, ids, err := s.FinishIssue(ctx, alice, id, 0, Finish{Reason: "done", IdempotencyKey: "k-1"})
			return struct {
				Issue Issue
				IDs   []IssueID
			}{is, ids}, err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newDSN(t)
			s1 := openStore(t, dsn, Options{})
			s2 := openStore(t, dsn, Options{})
			is := mustCreate(t, s1, NewIssue{Title: "work", Labels: []string{"area:db"}})
			var want any
			retried := false
			s1.beforeCommit = func(ctx context.Context) error {
				if retried {
					return nil
				}
				retried = true
				// The first attempt's result has the label; the request
				// that commits first, from s2, finds it gone, so its
				// stored result has no labels field at all.
				if err := s2.RemoveLabel(ctx, alice, is.ID, "area:db", ""); err != nil {
					return err
				}
				var err error
				if want, err = tc.do(ctx, s2, is.ID); err != nil {
					return err
				}
				return errRetry
			}
			got, err := tc.do(t.Context(), s1, is.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !retried {
				t.Fatal("the first attempt never reached its commit")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s replayed after a retry = %+v\nwant the stored result %+v", tc.name, got, want)
			}
		})
	}
}

// A key names one request: reusing it for another request, or after it
// expired, is refused with the key named; another principal's key of the
// same spelling is a different key.
func TestIdempotencyRefusals(t *testing.T) {
	t.Parallel()
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	if _, err := s.AddComment(ctx, alice, is.ID, "first", "k-1"); err != nil {
		t.Fatal(err)
	}

	var ie *IdemError
	_, err := s.AddComment(ctx, alice, is.ID, "different", "k-1")
	if !errors.As(err, &ie) || ie.Key != "k-1" || ie.Expired || !errors.Is(err, ErrConflict) {
		t.Errorf("same key, other body: %v", err)
	}
	_, err = s.CreateIssue(ctx, alice, NewIssue{Title: "first", IdempotencyKey: "k-1"})
	if !errors.As(err, &ie) || ie.Key != "k-1" {
		t.Errorf("same key, other op: %v", err)
	}
	if _, err := s.AddComment(ctx, bob, is.ID, "different", "k-1"); err != nil {
		t.Errorf("bob's k-1 is his own: %v", err)
	}

	clk.add(DefaultIdemWindow + time.Second)
	_, err = s.AddComment(ctx, alice, is.ID, "first", "k-1")
	if !errors.As(err, &ie) || !ie.Expired || !errors.Is(err, ErrConflict) {
		t.Errorf("expired key: %v", err)
	}

	for _, key := range []string{"-starts-with-dash", "has space", strings.Repeat("x", 65), "k\n"} {
		if _, err := s.AddComment(ctx, alice, is.ID, "x", key); !errors.Is(err, ErrInvalid) {
			t.Errorf("key %q: err = %v, want ErrInvalid", key, err)
		}
	}
	assertGapless(t, s)
}
