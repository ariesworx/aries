package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// The tests here hold the server's own logic to account against fakes
// of its stores, which need no database: what each store error becomes
// on the wire (rule 11), and a project store that fails to open. The
// store's own behavior is tested on Postgres, in internal/store.

// realStore returns the store on Postgres behind st, for a test that reads
// what the server never asks of a store, such as the event log. It
// panics on a fake, which no such test has.
func realStore(st Store) *store.Store { return st.(*store.Store) }

// fakeStore is a project's store that writes nothing. A write returns
// err, which the test sets; the upkeep a server does as it opens and
// serves a store succeeds and finds nothing to do. Any other method is
// the nil Store it embeds, so a test that reaches one panics there, in
// a trace that names it.
type fakeStore struct {
	Store
	err error

	mu     sync.Mutex
	closed int
}

func (f *fakeStore) CreateIssue(context.Context, store.Actor, store.NewIssue) (store.Issue, error) {
	return store.Issue{}, f.err
}

func (f *fakeStore) TouchAgent(context.Context, store.Actor, string) error { return nil }
func (f *fakeStore) Migrated() int                                         { return 0 }
func (f *fakeStore) ClockState() store.ClockState                          { return store.ClockState{} }
func (f *fakeStore) ReapClaims(context.Context) ([]store.Claim, error)     { return nil, nil }
func (f *fakeStore) ReapLocks(context.Context) ([]store.LockEnd, error)    { return nil, nil }

func (f *fakeStore) EndClaims(context.Context, store.Actor, func(string) string) ([]store.Claim, error) {
	return nil, nil
}

func (f *fakeStore) EndLocks(context.Context, store.Actor, func(string) string) ([]store.LockEnd, error) {
	return nil, nil
}

func (f *fakeStore) WriteStats() store.WriteStats { return store.WriteStats{} }

func (f *fakeStore) SessionsSeen(context.Context, time.Duration) (int, error) { return 0, nil }

func (f *fakeStore) Prune(context.Context, time.Duration, time.Duration) (store.Pruned, error) {
	return store.Pruned{}, nil
}

func (f *fakeStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

// closes returns how many times the store was closed.
func (f *fakeStore) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// fakeControl is the control store of a server of many projects that
// holds one active project, projA, whose members are those in members.
// Like fakeStore, it panics in any method a test was not expected to
// reach.
type fakeControl struct {
	Control
	members map[string]bool
}

func (c *fakeControl) KeysElsewhere(func(string) (string, bool)) {}
func (c *fakeControl) Ping(context.Context) error                { return nil }

func (c *fakeControl) Standing(principal string) (known, disabled bool) {
	return c.members[principal], false
}

func (c *fakeControl) Access(principal, project string) store.Access {
	acc := store.Access{}
	if project == projA {
		acc.Active = true
		acc.Project = store.Project{ID: projA, Name: "proj-a", Prefix: "pa", Account: "acct-a", DBName: "aries_pa", State: store.ProjectActive}
	}
	if c.members[principal] {
		acc.Role = store.RoleMember
	}
	return acc
}

// newFakeServer returns a server of one project over st.
func newFakeServer(t *testing.T, st Store) *Server {
	t.Helper()
	s, err := New(Config{Store: st, Project: project, Version: "v0.2.0", ReapInterval: -1, PeerCheck: anyPeer})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestStoreErrorsOnTheWire holds each error a store returns to the
// refusal its client gets: the code, a message that names the cause,
// and a fix that names the next step (rule 11).
func TestStoreErrorsOnTheWire(t *testing.T) {
	t.Parallel()
	until := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		err  error
		code proto.Code
		msg  string // the message contains it
		fix  string // the fix contains it
	}{
		{name: "not found", err: fmt.Errorf("issue sf-x: %w", store.ErrNotFound),
			code: proto.CodeNotFound, msg: "issue sf-x not found", fix: "find the id with `aries list`"},
		{name: "exists", err: fmt.Errorf("issue sf-x: %w", store.ErrExists),
			code: proto.CodeExists, msg: "issue sf-x already exists", fix: "omit --id"},
		{name: "cycle", err: fmt.Errorf("sf-x already depends on sf-y: %w", store.ErrCycle),
			code: proto.CodeCycle, msg: "sf-x already depends on sf-y, so this would make a cycle", fix: "aries dep rm"},
		{name: "invalid", err: fmt.Errorf("%w: title must be 1-500 bytes", store.ErrInvalid),
			code: proto.CodeInvalid, msg: "title must be 1-500 bytes", fix: "`aries create -h`"},
		{name: "busy", err: fmt.Errorf("%w: alice has created 100 issues in the last hour", store.ErrBusy),
			code: proto.CodeBusy, msg: "alice has created 100 issues in the last hour", fix: "retry later"},
		{name: "in progress is set by start", err: store.ErrStatusInProgress,
			code: proto.CodeInvalid, msg: "status in_progress is set only by start", fix: "aries start sf-x"},
		{name: "conflict without a rev", err: fmt.Errorf("issue sf-x: %w", store.ErrConflict),
			code: proto.CodeConflict, msg: "the change lost to concurrent writes", fix: "retry"},
		{name: "schema too new", err: fmt.Errorf("open: %w", store.ErrSchemaTooNew),
			code: proto.CodeUnavailable, msg: "migrated by a newer ariesd", fix: "run the ariesd release that migrated it"},
		{name: "timed out", err: fmt.Errorf("create: %w", context.DeadlineExceeded),
			code: proto.CodeUnavailable, msg: "the server timed out on create", fix: "check the ariesd log"},
		{name: "anything else", err: errors.New("dial unix /run/postgresql/.s.PGSQL.5432: connection refused"),
			code: proto.CodeUnavailable, msg: "the server could not complete create", fix: "check the ariesd log"},
		{name: "stale epoch", err: &store.StaleEpochError{ID: "sf-x", Epoch: 1, Current: 2, By: bob},
			code: proto.CodeConflict, msg: "your claim on sf-x (epoch 1) was lost; it is now epoch 2, held by bob/s-b", fix: "stop work on it"},
		{name: "idempotency key reused", err: &store.IdemError{Key: "k-1"},
			code: proto.CodeConflict, msg: `idempotency key "k-1"`, fix: `a new idempotency key instead of "k-1"`},
		{name: "idempotency key expired", err: &store.IdemError{Key: "k-1", Expired: true, Used: until, Window: 24 * time.Hour},
			code: proto.CodeStale, msg: `idempotency key "k-1"`, fix: "make it again by hand"},
		{name: "held by another", err: &store.ForbiddenError{ID: "sf-x", Holder: bob, Until: until},
			code: proto.CodeForbidden, msg: "sf-x is held by bob (session s-b) until 2026-10-15T12:00:00Z", fix: "aries handoff sf-x --release"},
		{name: "for admins", err: &store.ForbiddenError{ID: "sf-x", Action: "close with open items"},
			code: proto.CodeForbidden, msg: "close with open items is for aries admins", fix: "aries accept sf-x N"},
		{name: "closed", err: &store.StateError{ID: "sf-x", Reason: store.StateClosed},
			code: proto.CodeInvalid, msg: "sf-x", fix: "aries reopen sf-x"},
		{name: "claimed", err: &store.StateError{ID: "sf-x", Reason: store.StateClaimed, Holder: bob},
			code: proto.CodeInvalid, msg: "sf-x", fix: "aries handoff sf-x --release"},
		{name: "acceptance unmet", err: &store.AcceptanceError{ID: "sf-x", Open: []int{2}},
			code: proto.CodeAcceptance, msg: "sf-x", fix: "aries accept sf-x"},
		{name: "gate held", err: &store.GateError{ID: "sf-x", Gate: "sf-g", Reason: store.GateHeld, Op: "start"},
			code: proto.CodeInvalid, msg: "sf-g", fix: "sf-g"},
		{name: "access", err: &store.AccessError{Actor: "alice", Op: store.Op(proto.OpPrincipals), Reason: store.AccessAdmin},
			code: proto.CodeForbidden, msg: "alice may not list the principals", fix: "ask a server admin"},
		{name: "taken", err: &store.TakenError{Kind: store.TakenPrincipal, Value: "bob"},
			code: proto.CodeExists, msg: "bob", fix: "another"},
		{name: "control cap", err: &store.ControlLimitError{Limit: "principals", Max: 3},
			code: proto.CodeInvalid, msg: "3", fix: "raise principals under limits:"},
		{name: "last admin", err: &store.LastAdminError{Principal: "dana"},
			code: proto.CodeInvalid, msg: "dana is the last server admin", fix: "enable another server admin first"},
		{name: "conflict cap", err: &store.ConflictLimitError{Limit: "open_conflicts", Max: 5, Issue: "sf-x", Principal: "alice"},
			code: proto.CodeInvalid, msg: "5", fix: "raise open_conflicts under limits:"},
		{name: "usage batch", err: &store.UsageBatchError{Max: 500, Got: 501},
			code: proto.CodeInvalid, msg: "at most 500 usage records per call, not 501", fix: "send at most 500 records per call"},
		{name: "price cap", err: &store.PriceLimitError{Max: 100},
			code: proto.CodeInvalid, msg: "the server keeps at most 100 prices", fix: "raise prices under limits:"},
		{name: "plan cap", err: &store.PlanLimitError{Max: 100},
			code: proto.CodeInvalid, msg: "the server keeps at most 100 plan rows", fix: "raise plans under limits:"},
		{name: "hours cap", err: &store.HoursLimitError{Principal: "alice", On: until, Max: 20},
			code: proto.CodeInvalid, msg: "20", fix: "aries log --by alice"},
		{name: "a full day of hours", err: &store.HoursDayError{Principal: "alice", On: until, Logged: 23 * time.Hour},
			code: proto.CodeInvalid, msg: "alice", fix: "at most 1h more on 2026-10-15"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newFakeServer(t, &fakeStore{err: tc.err})
			_, perr := call[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{ID: "sf-x", Title: "t"})
			if perr == nil {
				t.Fatalf("create with the store failing with %q succeeded", tc.err)
			}
			if perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Errorf("create with the store failing with %q = %s %q, fix %q; want %s, message containing %q, fix containing %q",
					tc.err, perr.Code, perr.Message, perr.Fix, tc.code, tc.msg, tc.fix)
			}
		})
	}
}

// TestStoreBusyOnAConnection: a write the store refuses at one of its
// caps reaches the client as busy, with a fix and the request's id,
// and the connection serves on.
func TestStoreBusyOnAConnection(t *testing.T) {
	t.Parallel()
	st := &fakeStore{err: fmt.Errorf("%w: alice has created 100 issues in the last hour", store.ErrBusy)}
	s := newFakeServer(t, st)
	c := mustDialTo(t, s, alice, project)
	for range 2 {
		_, perr := ask[proto.WriteResult](c, proto.OpCreate, proto.CreateArgs{Title: "t"})
		if perr == nil {
			t.Fatal("create the store refuses as busy succeeded")
		}
		if perr.Code != proto.CodeBusy || perr.Fix != "retry later" || perr.Req == "" ||
			perr.Message != "alice has created 100 issues in the last hour" {
			t.Fatalf("create the store refuses as busy = %s %q, fix %q, req %q; want busy, the store's message, fix \"retry later\" and a request id",
				perr.Code, perr.Message, perr.Fix, perr.Req)
		}
	}
}

// TestProjectStoreFailsToOpen: a handshake whose project's store does
// not open is refused unavailable, the cause logged and not shown; the
// failure is not kept, so the next handshake opens again, and a store
// that opens is closed when it idles.
func TestProjectStoreFailsToOpen(t *testing.T) {
	t.Parallel()
	var log syncBuffer
	var mu sync.Mutex
	opens := 0
	opened := &fakeStore{}
	open := func(_ context.Context, p store.Project) (Store, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		if opens == 1 {
			return nil, fmt.Errorf("open %s: %w", p.DBName, store.ErrSchemaTooNew)
		}
		return opened, nil
	}
	s, err := New(Config{Control: &fakeControl{members: map[string]bool{"alice": true}}, Open: open, NoAudit: true,
		Version: "v0.2.0", ReapInterval: -1, Logger: slogTo(&log), PeerCheck: anyPeer})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s.now = func() time.Time { return now }

	w, c := dialTo(t, s, alice, projA)
	c.hangUp()
	if w.Err == nil || w.Err.Code != proto.CodeUnavailable || w.Err.Message != `the server could not open project "`+projA+`"` ||
		!strings.Contains(w.Err.Fix, "check the ariesd log") {
		t.Errorf("handshake to a project whose store fails to open = %+v; want unavailable, naming the project, with a fix", w.Err)
	}
	if logged := log.String(); !strings.Contains(logged, "event="+string(logs.ProjectOpenFailed)) || !strings.Contains(logged, store.ErrSchemaTooNew.Error()) {
		t.Errorf("log after a store that failed to open:\n%s\nwant a %s line with the cause", logged, logs.ProjectOpenFailed)
	}
	if s.OpenStores() != 0 {
		t.Errorf("OpenStores() = %d after a store failed to open, want 0", s.OpenStores())
	}

	mustDialTo(t, s, alice, projA).hangUp()
	if s.OpenStores() != 1 || opens != 2 {
		t.Errorf("after a second handshake: OpenStores() = %d, opens = %d; want 1 store open after 2 opens", s.OpenStores(), opens)
	}
	now = now.Add(time.Duration(DefaultLimits.StoreIdle) + time.Second)
	s.CloseIdleStores()
	if s.OpenStores() != 0 || opened.closes() != 1 {
		t.Errorf("after the store idled: OpenStores() = %d, closed %d times; want 0 open, closed once", s.OpenStores(), opened.closes())
	}
}

// anyPeer passes every connection's peer check: a test's net.Pipe has
// no peer to check, and on Linux the server's own check refuses it.
func anyPeer(net.Conn) error { return nil }
