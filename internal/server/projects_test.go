package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/telemetry"
	"github.com/go-sql-driver/mysql"
)

// Invented projects for the servers of many.
const (
	projA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	projB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	projC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

// erin is a project admin in the tests that make her one.
var erin = store.Actor{Principal: "erin", Session: "s-e", Machine: "laptop-e"}

// clock is a time a test moves forward by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now()} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// multi is a server of many projects, with its control store, whose
// server admin is dana, and a count of how often each project's store
// was opened.
type multi struct {
	*Server
	ctl *store.Control
	dsn string
	log *syncBuffer
	// keysFile is the file the server writes the store's keys to, and
	// byHand the authorized_keys file it only reads.
	keysFile, byHand string
	// tel measures the server and its stores; nil in most tests.
	tel *telemetry.Telemetry

	mu    sync.Mutex
	opens map[string]int
}

// syncBuffer is a log sink read while handlers still write to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// newMulti starts a server of many projects that holds none yet. ck, if
// set, is the clock of the server, the control store and every project
// store.
func newMulti(t *testing.T, lim Limits, ck *clock) *multi {
	t.Helper()
	return newMultiAdmins(t, lim, ck, dana.Principal)
}

// newMultiAdmins is newMulti with the server admins given.
func newMultiAdmins(t *testing.T, lim Limits, ck *clock, admins ...string) *multi {
	t.Helper()
	return newMultiTel(t, lim, ck, nil, admins...)
}

// newMultiTel is newMultiAdmins with the server and its stores measured
// by tel, which may be nil.
func newMultiTel(t *testing.T, lim Limits, ck *clock, tel *telemetry.Telemetry, admins ...string) *multi {
	t.Helper()
	if errors.Is(doltErr, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the server tests")
	}
	if doltErr != nil {
		t.Fatalf("dolt sql-server: %v", doltErr)
	}
	dsn, err := dolt.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var now func() time.Time
	if ck != nil {
		now = ck.now
	}
	ctl, err := store.OpenControl(t.Context(), dsn, store.ControlOptions{Admins: admins, CommitInterval: -1, Now: now,
		Limits: lim.Control, Tracer: StoreTracer(tel, "")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	// What sshd reads for the account: the keys file the server writes,
	// and the authorized_keys file a test may write by hand.
	sshDir := filepath.Join(t.TempDir(), ".ssh")
	if err := os.Mkdir(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &multi{ctl: ctl, dsn: dsn, log: &syncBuffer{}, opens: map[string]int{}, tel: tel,
		keysFile: filepath.Join(sshDir, "starfix_keys"), byHand: filepath.Join(sshDir, "authorized_keys")}
	open := ProjectOpener(dsn, ctl, store.Options{CommitInterval: -1, Limits: lim.Limits, Now: now}, tel)
	m.Server, err = New(Config{Control: ctl, Version: "v0.2.0", PeerCheck: func(net.Conn) error { return nil }, Limits: lim,
		KeysFile: m.keysFile, AuthorizedKeys: m.byHand, Telemetry: tel,
		ReapInterval: -1, Logger: slogTo(m.log),
		Open: func(ctx context.Context, p store.Project) (*store.Store, error) {
			m.mu.Lock()
			m.opens[p.ID]++
			m.mu.Unlock()
			return open(ctx, p)
		}})
	if err != nil {
		t.Fatal(err)
	}
	if ck != nil {
		m.now, m.started = ck.now, ck.now()
	}
	// As a daemon does before it listens.
	if err := m.WriteKeys(); err != nil {
		t.Fatal(err)
	}
	// Runs after each connection's cleanup, which ends its handler.
	t.Cleanup(m.stores.closeAll)
	return m
}

// opened returns how often id's store was opened.
func (m *multi) opened(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opens[id]
}

// project adds an active project in a database of its own, named and
// prefixed for its id's last character, with members given as
// "NAME" or "NAME=admin".
func (m *multi) project(t *testing.T, id string, members ...string) store.Project {
	t.Helper()
	dsn, err := dolt.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	n := id[len(id)-1:]
	if _, err := m.ctl.AddProject(t.Context(), dana, store.NewProject{ID: id, Name: "proj-" + n, Prefix: "p" + n,
		Account: "acct-" + n, DBName: cfg.DBName}); err != nil {
		t.Fatal(err)
	}
	p, err := m.ctl.ActivateProject(t.Context(), dana, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, mem := range members {
		m.member(t, id, mem)
	}
	return p
}

// member makes "NAME" or "NAME=admin" a member of project id, adding
// the principal if it has no row.
func (m *multi) member(t *testing.T, id, member string) {
	t.Helper()
	name, role, _ := strings.Cut(member, "=")
	if role == "" {
		role = string(store.RoleMember)
	}
	if _, err := m.ctl.Principal(t.Context(), name); errors.Is(err, store.ErrNotFound) {
		if _, err := m.ctl.AddPrincipal(t.Context(), dana, name, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.ctl.SetMember(t.Context(), dana, name, id, store.Role(role)); err != nil {
		t.Fatal(err)
	}
}

// dialTo connects a to project proj over a pipe and returns the
// welcome, which may be a refusal.
func dialTo(t *testing.T, s *Server, a store.Actor, proj string) (*proto.Frame, *conn) {
	t.Helper()
	srv, cli := net.Pipe()
	c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	t.Cleanup(c.hangUp)
	c.wg.Go(func() { s.handle(t.Context(), srv) })
	c.wg.Go(func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: a.Principal})
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: proj, Session: a.Session, Machine: a.Machine})
	})
	return c.read(), c
}

// mustDialTo is dialTo for a connection that must be welcomed.
func mustDialTo(t *testing.T, s *Server, a store.Actor, proj string) *conn {
	t.Helper()
	w, c := dialTo(t, s, a, proj)
	if w.T != proto.FrameWelcome || w.Err != nil {
		t.Fatalf("%s to project %s: welcome %+v, error %v", a.Principal, proj, w, w.Err)
	}
	return c
}

// hangUp closes the client's end and waits for the server's handler, so
// that what the connection held is given back when it returns.
func (c *conn) hangUp() {
	_ = c.nc.Close()
	c.wg.Wait()
}

// ask sends one request on c and returns its result or its refusal.
func ask[R any](c *conn, op string, args any) (R, *proto.Error) {
	c.t.Helper()
	var out R
	id := c.send(op, args)
	for {
		f := c.read()
		if f.T != proto.FrameRes {
			continue
		}
		if f.ID != id {
			c.t.Fatalf("response to request %d, want %d", f.ID, id)
		}
		if f.Err != nil {
			return out, f.Err
		}
		if err := json.Unmarshal(f.OK, &out); err != nil {
			c.t.Fatalf("%s result: %v", op, err)
		}
		return out, nil
	}
}

// mustAsk is ask for a request that must succeed.
func mustAsk[R any](c *conn, op string, args any) R {
	c.t.Helper()
	out, perr := ask[R](c, op, args)
	if perr != nil {
		c.t.Fatalf("%s: %v", op, perr)
	}
	return out
}

func TestNewModes(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	single := newServer(t)
	open := func(context.Context, store.Project) (*store.Store, error) { return nil, errors.New("unused") }
	tests := []struct {
		name string
		cfg  Config
		err  string
	}{
		{"control with a store", Config{Control: m.ctl, Open: open, Store: single.cfg.Store}, "needs Open, and no Store"},
		{"control without Open", Config{Control: m.ctl}, "needs Open, and no Store"},
		{"neither", Config{Project: project}, "no store"},
		{"store without a project", Config{Store: single.cfg.Store}, "not a lowercase UUID"},
		{"control", Config{Control: m.ctl, Open: open}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
				t.Errorf("New = %v, want error containing %q", err, tc.err)
			}
		})
	}
	// A server of one project keeps no registry and asks no control store.
	if single.stores != nil || single.cfg.Control != nil || single.OpenStores() != 1 {
		t.Errorf("a server of one project has a registry (%v) or a control store (%v)", single.stores, single.cfg.Control)
	}
	single.CloseIdleStores() // nothing to close, and nothing to panic on
}

func TestMultiHandshake(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice", "erin=admin", "frank")
	m.project(t, projB, "bob")
	if _, err := m.ctl.DisablePrincipal(t.Context(), dana, "frank"); err != nil {
		t.Fatal(err)
	}
	// Archived, and one never activated.
	m.project(t, projC, "alice")
	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projC); err != nil {
		t.Fatal(err)
	}
	const creating = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	if _, err := m.ctl.AddProject(t.Context(), dana, store.NewProject{ID: creating, Name: "proj-d", Prefix: "pd",
		Account: "internal", DBName: "not_made_yet"}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		principal string
		project   string
		code      proto.Code // "" means welcomed
	}{
		{"member", "alice", projA, ""},
		{"project admin", "erin", projA, ""},
		{"server admin, in no project", "dana", projA, ""},
		{"server admin, everywhere", "dana", projB, ""},
		{"the upgrade's probe", ProbePrincipal, projA, ""},
		{"member of another project", "alice", projB, proto.CodeForbidden},
		{"project admin elsewhere", "erin", projB, proto.CodeForbidden},
		{"disabled member", "frank", projA, proto.CodeForbidden},
		{"no principal row", "ivy", projA, proto.CodeForbidden},
		{"unknown project", "alice", "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", proto.CodeNotFound},
		{"archived project, a member", "alice", projC, proto.CodeNotFound},
		{"archived project, a server admin", "dana", projC, proto.CodeNotFound},
		{"project still being created", "dana", creating, proto.CodeNotFound},
		{"not a project id", "alice", "nonsense", proto.CodeNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := dialTo(t, m.Server, store.Actor{Principal: tc.principal, Session: "s", Machine: "m"}, tc.project)
			if w.T != proto.FrameWelcome {
				t.Fatalf("got a %q frame, want a welcome", w.T)
			}
			switch {
			case tc.code == "" && (w.Err != nil || w.Principal != tc.principal):
				t.Errorf("handshake(%s, %s) = %+v, error %v; want a welcome", tc.principal, tc.project, w, w.Err)
			case tc.code != "" && (w.Err == nil || w.Err.Code != tc.code || w.Err.Fix == ""):
				t.Errorf("handshake(%s, %s) = %+v; want refusal %s with a fix", tc.principal, tc.project, w.Err, tc.code)
			}
			switch {
			case tc.code != proto.CodeForbidden || w.Err == nil:
			case tc.name == "disabled member":
				// A disabled principal is told so, and what undoes it.
				if w.Err.Message != "frank is disabled on this server" || !strings.Contains(w.Err.Fix, "`sfx admin principal enable frank`") {
					t.Errorf("forbidden = %q, fix %q; want the principal told it is disabled, and the command that enables it", w.Err.Message, w.Err.Fix)
				}
			case tc.name == "no principal row":
				// Refused on the bridge frame, before a project is named
				// (review F-4): it is told it has no principal, and the
				// command that makes one.
				if w.Err.Message != "ivy is not a principal of this server" || !strings.Contains(w.Err.Fix, "`sfx admin principal add ivy`") {
					t.Errorf("forbidden = %q, fix %q; want the principal told the server does not know it, and the command that adds it", w.Err.Message, w.Err.Fix)
				}
			default:
				if !strings.HasPrefix(w.Err.Fix, "ask a server admin, or one of the project's admins, to add you: `sfx admin member add "+tc.principal+"`") ||
					!strings.Contains(w.Err.Message, tc.principal+" is not a member of project") {
					t.Errorf("forbidden = %q, fix %q; want the principal named, whom to ask and the command they run", w.Err.Message, w.Err.Fix)
				}
			}
			if tc.code == proto.CodeNotFound && w.Err != nil && !strings.Contains(w.Err.Message, "is not served by this server") {
				t.Errorf("not found = %q, want the wording of a server of one project", w.Err.Message)
			}
		})
	}
	// A refusal opens no store: only projects someone was let into.
	if got := m.opened(projB); got != 1 {
		t.Errorf("project b's store was opened %d times, want 1 (for dana alone)", got)
	}
	if got := m.opened(projC) + m.opened(creating); got != 0 {
		t.Errorf("stores of projects not served were opened %d times, want 0", got)
	}
}

// TestMultiAccessEnds pins what happens when access ends while a
// connection is open: in this stage nothing over the wire ends it, so
// the open connection goes on, and the next handshake is refused.
func TestMultiAccessEnds(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice", "bob")
	a := mustDialTo(t, m.Server, alice, projA)
	b := mustDialTo(t, m.Server, bob, projA)

	if _, err := m.ctl.RemoveMember(t.Context(), dana, "alice", projA); err != nil {
		t.Fatal(err)
	}
	mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "after the membership ended"})
	if w, _ := dialTo(t, m.Server, alice, projA); w.Err == nil || w.Err.Code != proto.CodeForbidden {
		t.Errorf("alice's next handshake after her membership ended = %+v, want forbidden", w.Err)
	}

	if _, err := m.ctl.ArchiveProject(t.Context(), dana, projA); err != nil {
		t.Fatal(err)
	}
	mustAsk[proto.ListResult](b, proto.OpList, proto.ListArgs{})
	if w, _ := dialTo(t, m.Server, bob, projA); w.Err == nil || w.Err.Code != proto.CodeNotFound {
		t.Errorf("bob's next handshake after the project was archived = %+v, want not found", w.Err)
	}
	// Activated again, it is served again, from the same database.
	if _, err := m.ctl.ActivateProject(t.Context(), dana, projA); err != nil {
		t.Fatal(err)
	}
	c := mustDialTo(t, m.Server, bob, projA)
	if l := mustAsk[proto.ListResult](c, proto.OpList, proto.ListArgs{}); len(l.Issues) != 1 {
		t.Errorf("after reactivation the project lists %d issues, want the 1 written before", len(l.Issues))
	}
}

func TestMultiProjectsApart(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice", "bob")
	m.project(t, projB, "alice")
	a := mustDialTo(t, m.Server, alice, projA)
	b := mustDialTo(t, m.Server, alice, projB)

	inA := mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "in a"})
	inB := mustAsk[proto.CreateResult](b, proto.OpCreate, proto.CreateArgs{Title: "in b"})
	if !strings.HasPrefix(inA.ID, "pa-") || !strings.HasPrefix(inB.ID, "pb-") {
		t.Errorf("ids %q and %q, want each project's prefix (pa-, pb-)", inA.ID, inB.ID)
	}
	if _, perr := ask[proto.ShowResult](b, proto.OpShow, proto.ShowArgs{ID: inA.ID}); perr == nil || perr.Code != proto.CodeNotFound {
		t.Errorf("show of a's issue %s in b = %v, want not found", inA.ID, perr)
	}
	for _, tc := range []struct {
		c     *conn
		name  string
		title string
	}{{a, "a", "in a"}, {b, "b", "in b"}} {
		l := mustAsk[proto.ListResult](tc.c, proto.OpList, proto.ListArgs{})
		if len(l.Issues) != 1 || l.Issues[0].Title != tc.title {
			t.Errorf("project %s lists %+v, want only %q", tc.name, l.Issues, tc.title)
		}
		// Each project's own default account.
		sh := mustAsk[proto.ShowResult](tc.c, proto.OpShow, proto.ShowArgs{ID: l.Issues[0].ID})
		if sh.Usage == nil || sh.Usage.Account != "acct-"+tc.name {
			t.Errorf("project %s shows usage %+v, want its account acct-%s", tc.name, sh.Usage, tc.name)
		}
	}
	// Presence is each project's own.
	if who := mustAsk[proto.WhoResult](b, proto.OpWho, proto.WhoArgs{}); len(who.Agents) != 1 {
		t.Errorf("who in b lists %d agents, want alice's one session there", len(who.Agents))
	}
	// A request with no connection has no project.
	if _, perr := m.Dispatch(t.Context(), alice, proto.OpList, nil); perr == nil || perr.Code != proto.CodeUnavailable {
		t.Errorf("Dispatch on a server of many projects = %v, want unavailable", perr)
	}
}

func TestMultiProjectAdmins(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice", "erin=admin")
	m.project(t, projB, "alice", "erin")

	held := func(c *conn) proto.StartResult {
		t.Helper()
		is := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "held by alice"})
		return mustAsk[proto.StartResult](c, proto.OpStart, proto.StartArgs{ID: is.ID})
	}
	retitle := func(c *conn, id string) *proto.Error {
		t.Helper()
		sh := mustAsk[proto.ShowResult](c, proto.OpShow, proto.ShowArgs{ID: id})
		title := "changed"
		_, perr := ask[proto.WriteResult](c, proto.OpUpdate, proto.UpdateArgs{ID: id, Rev: sh.Issue.Rev, Title: &title})
		return perr
	}
	inA := held(mustDialTo(t, m.Server, alice, projA)).Issue
	inB := held(mustDialTo(t, m.Server, alice, projB)).Issue
	erinA, erinB := mustDialTo(t, m.Server, erin, projA), mustDialTo(t, m.Server, erin, projB)

	if perr := retitle(erinA, inA.ID); perr != nil {
		t.Errorf("erin, an admin of a, changing alice's held issue there: %v", perr)
	}
	if perr := retitle(erinB, inB.ID); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("erin, a member of b, changing alice's held issue there = %v, want forbidden", perr)
	}
	for _, proj := range []struct{ id, issue string }{{projA, inA.ID}, {projB, inB.ID}} {
		if perr := retitle(mustDialTo(t, m.Server, dana, proj.id), proj.issue); perr != nil {
			t.Errorf("dana, a server admin, changing alice's held issue in %s: %v", proj.id, perr)
		}
	}
	// A role given or taken away counts on the connections already open,
	// with the store still open.
	if _, err := m.ctl.SetMember(t.Context(), dana, "erin", projA, store.RoleMember); err != nil {
		t.Fatal(err)
	}
	if perr := retitle(erinA, inA.ID); perr == nil || perr.Code != proto.CodeForbidden {
		t.Errorf("erin, no longer an admin of a, changing alice's held issue = %v, want forbidden", perr)
	}
	if _, err := m.ctl.SetMember(t.Context(), dana, "erin", projB, store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if perr := retitle(erinB, inB.ID); perr != nil {
		t.Errorf("erin, now an admin of b, changing alice's held issue there: %v", perr)
	}
	if got := m.opened(projA) + m.opened(projB); got != 2 {
		t.Errorf("the two stores were opened %d times in all, want once each", got)
	}
}

func TestRegistryOpensOnce(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice")
	const n = 12
	var wg sync.WaitGroup
	conns := make([]*conn, n)
	for i := range n {
		// The dials are set up here, on the test's goroutine; only the
		// handshakes race.
		srv, cli := net.Pipe()
		c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
		t.Cleanup(c.hangUp)
		conns[i] = c
		c.wg.Go(func() { m.handle(t.Context(), srv) })
		wg.Go(func() {
			_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: "alice"})
			_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: projA,
				Session: fmt.Sprintf("s-%d", i), Machine: "m"})
		})
	}
	wg.Wait()
	for i, c := range conns {
		if w := c.read(); w.Err != nil {
			t.Errorf("connection %d refused: %v", i, w.Err)
		}
	}
	if got := m.opened(projA); got != 1 {
		t.Errorf("%d handshakes at once opened the store %d times, want 1", n, got)
	}
	if got := m.OpenStores(); got != 1 {
		t.Errorf("OpenStores() = %d, want 1", got)
	}
}

func TestRegistryIdleClose(t *testing.T) {
	ck := newClock()
	m := newMulti(t, Limits{StoreIdle: Duration(10 * time.Minute)}, ck)
	m.project(t, projA, "alice")
	m.project(t, projB, "alice")
	a := mustDialTo(t, m.Server, alice, projA)
	b := mustDialTo(t, m.Server, alice, projB)
	made := mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "kept"})
	a.hangUp()

	ck.advance(9 * time.Minute)
	m.CloseIdleStores()
	if got := m.OpenStores(); got != 2 {
		t.Fatalf("after 9 idle minutes of 10, %d stores are open, want 2", got)
	}
	ck.advance(2 * time.Minute)
	m.CloseIdleStores()
	if got := m.OpenStores(); got != 1 {
		t.Fatalf("after 11 idle minutes, %d stores are open; want 1: a closed, b kept by its connection", got)
	}
	mustAsk[proto.ListResult](b, proto.OpList, proto.ListArgs{})
	if !strings.Contains(m.log.String(), `msg="project store closed" project=`+projA+` event=project.closed reason=idle`) {
		t.Errorf("no log line for the idle close of a:\n%s", m.log.String())
	}

	// Reopened by the next handshake, with what it held.
	a = mustDialTo(t, m.Server, alice, projA)
	sh := mustAsk[proto.ShowResult](a, proto.OpShow, proto.ShowArgs{ID: made.ID})
	if sh.Issue.Title != "kept" {
		t.Errorf("after the store reopened, show %s = %q, want the issue written before", made.ID, sh.Issue.Title)
	}
	if got := m.opened(projA); got != 2 {
		t.Errorf("a's store was opened %d times, want 2", got)
	}
	// The idle time counts from the last connection's end, not the
	// store's opening.
	ck.advance(time.Hour)
	a.hangUp()
	m.CloseIdleStores()
	if got := m.OpenStores(); got != 2 {
		t.Errorf("a store whose last connection just ended was closed: %d open, want 2", got)
	}
}

func TestRegistryCap(t *testing.T) {
	ck := newClock()
	m := newMulti(t, Limits{OpenStores: 2}, ck)
	for _, id := range []string{projA, projB, projC} {
		m.project(t, id, "alice")
	}
	a := mustDialTo(t, m.Server, alice, projA)
	b := mustDialTo(t, m.Server, alice, projB)

	w, refused := dialTo(t, m.Server, alice, projC)
	if w.Err == nil || w.Err.Code != proto.CodeBusy || !strings.Contains(w.Err.Fix, "open_stores") ||
		!strings.Contains(w.Err.Message, "limit of 2 projects open") {
		t.Fatalf("a third project with two in use = %+v, want busy naming open_stores", w.Err)
	}
	// The refusal gave back its connection slot.
	refused.hangUp()
	m.slots.mu.Lock()
	total := m.slots.total
	m.slots.mu.Unlock()
	if total != 2 {
		t.Errorf("%d connection slots held after the refusal, want 2", total)
	}

	// With both idle, the one idle longest makes room.
	b.hangUp()
	ck.advance(time.Minute)
	a.hangUp()
	c := mustDialTo(t, m.Server, alice, projC)
	if got := m.OpenStores(); got != 2 {
		t.Errorf("OpenStores() = %d, want 2", got)
	}
	if !strings.Contains(m.log.String(), `msg="project store closed" project=`+projB+` event=project.closed reason="to make room"`) {
		t.Errorf("no log line for closing b, idle longest, to make room:\n%s", m.log.String())
	}
	// a was not closed: its next connection opens nothing.
	mustDialTo(t, m.Server, alice, projA)
	if got := m.opened(projA); got != 1 {
		t.Errorf("a's store was opened %d times, want 1: b was idle longer", got)
	}
	mustAsk[proto.ListResult](c, proto.OpList, proto.ListArgs{})
}

// TestReapAtOpen checks that a store is reaped as it opens, before its
// first request: nothing ended the claims that lapsed while it was
// closed.
func TestReapAtOpen(t *testing.T) {
	ck := newClock()
	m := newMulti(t, Limits{StoreIdle: Duration(time.Minute)}, ck)
	m.project(t, projA, "alice", "bob")
	a := mustDialTo(t, m.Server, alice, projA)
	is := mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "left in progress"})
	mustAsk[proto.StartResult](a, proto.OpStart, proto.StartArgs{ID: is.ID, Lease: "10m"})
	a.hangUp()
	ck.advance(2 * time.Minute)
	m.CloseIdleStores()
	if got := m.OpenStores(); got != 0 {
		t.Fatalf("%d stores open, want the idle one closed", got)
	}
	ck.advance(time.Hour) // the lease runs out with no reaper

	b := mustDialTo(t, m.Server, bob, projA)
	// The first request already sees the claim ended: the issue is open
	// again, and ready.
	sh := mustAsk[proto.ShowResult](b, proto.OpShow, proto.ShowArgs{ID: is.ID})
	if sh.Issue.Status != string(store.StatusOpen) || sh.Claim != nil {
		t.Errorf("first request after reopening: %s is %s with claim %+v; want open and unclaimed", is.ID, sh.Issue.Status, sh.Claim)
	}
	if !strings.Contains(m.log.String(), `msg="claim expired" project=`+projA) {
		t.Errorf("no log line for the claim that expired while the store was closed:\n%s", m.log.String())
	}
}

// TestReapEveryOpenStore checks that the reaper and the pruner run for
// every open store.
func TestReapEveryOpenStore(t *testing.T) {
	ck := newClock()
	m := newMulti(t, Limits{}, ck)
	ids := map[string]string{}
	conns := map[string]*conn{}
	for _, proj := range []string{projA, projB} {
		m.project(t, proj, "alice")
		c := mustDialTo(t, m.Server, alice, proj)
		is := mustAsk[proto.CreateResult](c, proto.OpCreate, proto.CreateArgs{Title: "held"})
		mustAsk[proto.StartResult](c, proto.OpStart, proto.StartArgs{ID: is.ID, Lease: "10m"})
		ids[proj], conns[proj] = is.ID, c
	}
	ck.advance(time.Hour)
	m.Reap(t.Context())
	for proj, id := range ids {
		// Read through the store, which no request has touched since.
		sh := mustAsk[proto.ShowResult](conns[proj], proto.OpShow, proto.ShowArgs{ID: id})
		if sh.Issue.Status != string(store.StatusOpen) {
			t.Errorf("after Reap, %s in project %s is %s, want open", id, proj, sh.Issue.Status)
		}
		if !strings.Contains(m.log.String(), `msg="claim expired" project=`+proj) {
			t.Errorf("no claim expired line for project %s", proj)
		}
	}
	// Prune reaches both too: alice's registry row in each is the only
	// one, so it stays, and nothing fails.
	ck.advance(30 * 24 * time.Hour)
	m.Prune(t.Context())
	if strings.Contains(m.log.String(), "level=ERROR") {
		t.Errorf("errors in the log:\n%s", m.log.String())
	}
}

func TestMultiLimitsPerProject(t *testing.T) {
	t.Run("connections", func(t *testing.T) {
		m := newMulti(t, Limits{ConnsPerPrincipal: 1, Conns: 3}, nil)
		m.project(t, projA, "alice", "bob")
		m.project(t, projB, "alice", "bob")
		mustDialTo(t, m.Server, alice, projA)
		mustDialTo(t, m.Server, alice, projB) // her allowance in b is her own
		w, _ := dialTo(t, m.Server, alice, projA)
		if w.Err == nil || w.Err.Code != proto.CodeBusy || !strings.Contains(w.Err.Message, "alice is at its connection limit (1)") {
			t.Errorf("alice's second connection to a = %+v, want busy naming her limit", w.Err)
		}
		mustDialTo(t, m.Server, bob, projA)
		// The server's total is kept across projects.
		w, _ = dialTo(t, m.Server, bob, projB)
		if w.Err == nil || w.Err.Code != proto.CodeBusy || !strings.Contains(w.Err.Message, "server is at its connection limit (3)") {
			t.Errorf("a fourth connection in all = %+v, want busy naming the server's limit", w.Err)
		}
	})
	t.Run("writes", func(t *testing.T) {
		m := newMulti(t, Limits{WriteBurst: 2, WriteRate: 0.001}, nil)
		m.project(t, projA, "alice")
		m.project(t, projB, "alice")
		a, b := mustDialTo(t, m.Server, alice, projA), mustDialTo(t, m.Server, alice, projB)
		for i := range 2 {
			mustAsk[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("w%d", i)})
		}
		_, perr := ask[proto.CreateResult](a, proto.OpCreate, proto.CreateArgs{Title: "one too many"})
		if perr == nil || perr.Code != proto.CodeBusy || !strings.Contains(perr.Message, "alice is over the write limit") {
			t.Fatalf("alice's third write in a = %v, want busy", perr)
		}
		// Her bucket in b is untouched.
		mustAsk[proto.CreateResult](b, proto.OpCreate, proto.CreateArgs{Title: "in b"})
	})
}

func TestMultiLogsNameTheProject(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	p := m.project(t, projA, "alice")
	c := mustDialTo(t, m.Server, alice, projA)
	mustAsk[proto.ListResult](c, proto.OpList, proto.ListArgs{})
	c.hangUp()
	got := m.log.String()
	for _, want := range []string{
		`msg="project store opened" project=` + projA + ` event=project.opened name=proj-a database=` + p.DBName,
		`msg=connected project=` + projA + ` principal=alice`,
		`msg=request project=` + projA + ` principal=alice`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log has no line containing %q:\n%s", want, got)
		}
	}
}

func TestMultiOpenFailure(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	// A project whose database the account was never granted.
	if _, err := m.ctl.AddProject(t.Context(), dana, store.NewProject{ID: projA, Name: "proj-a", Prefix: "pa",
		Account: "internal", DBName: "never_granted"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ctl.ActivateProject(t.Context(), dana, projA); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a failed opening is not remembered
		w, c := dialTo(t, m.Server, dana, projA)
		c.hangUp()
		if w.Err == nil || w.Err.Code != proto.CodeUnavailable || w.Err.Fix == "" {
			t.Fatalf("handshake for a project whose store cannot open = %+v, want unavailable with a fix", w.Err)
		}
		if strings.Contains(w.Err.Message, "never_granted") {
			t.Errorf("the refusal %q names the database, which is the server's business", w.Err.Message)
		}
	}
	if got := m.opened(projA); got != 2 {
		t.Errorf("the store was tried %d times, want 2", got)
	}
	if got := m.OpenStores(); got != 0 {
		t.Errorf("OpenStores() = %d after failed openings, want 0", got)
	}
	m.slots.mu.Lock()
	total := m.slots.total
	m.slots.mu.Unlock()
	if total != 0 {
		t.Errorf("%d connection slots held after the refusals, want 0", total)
	}
	if !strings.Contains(m.log.String(), `msg="open project store" event=project.open_failed project=`+projA+` database=never_granted`) {
		t.Errorf("no log line for the operator:\n%s", m.log.String())
	}
}

// TestControlReloadedOnTick checks that what `starfixd control init`
// wrote from another process counts in the daemon without a restart.
func TestControlReloadedOnTick(t *testing.T) {
	m := newMulti(t, Limits{}, nil)
	m.project(t, projA, "alice")
	other, err := store.OpenControl(t.Context(), m.dsn, store.ControlOptions{Admins: []string{dana.Principal}, CommitInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.AddPrincipal(t.Context(), dana, "bob", projA); err != nil {
		t.Fatal(err)
	}
	if w, _ := dialTo(t, m.Server, bob, projA); w.Err == nil || w.Err.Code != proto.CodeForbidden {
		t.Fatalf("bob before the daemon read the control store again = %+v, want forbidden", w.Err)
	}
	m.refresh(t.Context())
	mustDialTo(t, m.Server, bob, projA)
}

func TestDatabaseDSN(t *testing.T) {
	tests := []struct {
		name, dsn, db, want string
		err                 string
	}{
		{"tcp with a password", "starfix:pw@tcp(127.0.0.1:3306)/starfix", "starfix_control", "starfix:pw@tcp(127.0.0.1:3306)/starfix_control", ""},
		{"socket", "starfix@unix(/run/dolt.sock)/starfix", "p2", "starfix@unix(/run/dolt.sock)/p2", ""},
		{"parameters kept", "starfix@tcp(db:3306)/starfix?tls=true", "p2", "starfix@tcp(db:3306)/p2?tls=true", ""},
		{"quoting refused", "starfix@tcp(db:3306)/starfix", "p2`; DROP", "", "must be a lowercase letter"},
		{"upper case refused", "starfix@tcp(db:3306)/starfix", "Starfix", "", "must be a lowercase letter"},
		{"empty refused", "starfix@tcp(db:3306)/starfix", "", "", "must be a lowercase letter"},
		{"bad dsn", "::::", "p2", "", "parse dsn"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DatabaseDSN(tc.dsn, tc.db)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("DatabaseDSN(%q, %q) = %q, %v; want error containing %q", tc.dsn, tc.db, got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("DatabaseDSN(%q, %q) = %q, %v; want %q", tc.dsn, tc.db, got, err, tc.want)
			}
		})
	}
}

func TestResolveSettingsControl(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const dsn = "dsn: starfix@tcp(127.0.0.1:3306)/starfix\n"
	tests := []struct {
		name  string
		body  string
		env   map[string]string
		check func(Settings) bool
		err   string
	}{
		{name: "no control database by default", body: dsn, check: func(s Settings) bool { return s.ControlDB == "" }},
		{name: "from the file", body: dsn + "control_db: starfix_control\n", check: func(s Settings) bool {
			got, err := s.ControlDSN()
			return s.ControlDB == "starfix_control" && err == nil && got == "starfix@tcp(127.0.0.1:3306)/starfix_control"
		}},
		{name: "environment beats the file", body: dsn + "control_db: starfix_control\n", env: map[string]string{EnvControlDB: "other_control"},
			check: func(s Settings) bool { return s.ControlDB == "other_control" }},
		{name: "not a database name", body: dsn + "control_db: Starfix-Control\n", err: `control_db "Starfix-Control" is not a database name; fix:`},
		{name: "the project's own database", body: dsn + "control_db: starfix\n", err: `control_db "starfix" is the database in the DSN`},
		{name: "limits default", body: dsn, check: func(l Settings) bool {
			return l.Limits.Control == store.DefaultControlLimits && l.Limits.OpenStores == 64 && l.Limits.StoreIdle == Duration(10*time.Minute)
		}},
		{name: "limits set", body: "limits:\n  projects: 9\n  principals: 99\n  principals_per_creator: 7\n  keys_per_principal: 3\n  members_per_project: 12\n  open_stores: 5\n  store_idle: 90s\n",
			check: func(l Settings) bool {
				return l.Limits.Control == store.ControlLimits{Projects: 9, Principals: 99, PrincipalsPerCreator: 7, Keys: 3, Members: 12} &&
					l.Limits.OpenStores == 5 && l.Limits.StoreIdle == Duration(90*time.Second) && l.Limits.Conns == DefaultLimits.Conns
			}},
		{name: "zero takes the default", body: "limits:\n  projects: 0\n  open_stores: 0\n",
			check: func(l Settings) bool { return l.Limits == DefaultLimits }},
		{name: "negative projects", body: "limits:\n  projects: -1\n", err: "limit projects is -1"},
		{name: "negative principals", body: "limits:\n  principals: -1\n", err: "limit principals is -1"},
		{name: "principals_per_creator default", body: dsn, check: func(l Settings) bool { return l.Limits.Control.PrincipalsPerCreator == 500 }},
		{name: "negative principals_per_creator", body: "limits:\n  principals_per_creator: -1\n", err: "limit principals_per_creator is -1"},
		{name: "negative keys_per_principal", body: "limits:\n  keys_per_principal: -1\n", err: "limit keys_per_principal is -1"},
		{name: "negative members_per_project", body: "limits:\n  members_per_project: -1\n", err: "limit members_per_project is -1"},
		{name: "negative open_stores", body: "limits:\n  open_stores: -1\n", err: "limit open_stores must be zero or a positive number; fix:"},
		{name: "control write limits default", body: dsn, check: func(l Settings) bool {
			return l.Limits.ControlWriteRate == 1 && l.Limits.ControlWriteBurst == 30
		}},
		{name: "control write limits set", body: "limits:\n  control_write_rate: 0.5\n  control_write_burst: 7\n",
			check: func(l Settings) bool {
				return l.Limits.ControlWriteRate == 0.5 && l.Limits.ControlWriteBurst == 7 && l.Limits.WriteRate == DefaultLimits.WriteRate
			}},
		{name: "control write limits zero take the default", body: "limits:\n  control_write_rate: 0\n  control_write_burst: 0\n",
			check: func(l Settings) bool { return l.Limits == DefaultLimits }},
		{name: "negative control_write_rate", body: "limits:\n  control_write_rate: -1\n", err: "limit control_write_rate must be zero or a positive number; fix:"},
		{name: "control_write_rate not a number", body: "limits:\n  control_write_rate: .nan\n", err: "limit control_write_rate must be zero or a positive number; fix:"},
		{name: "negative control_write_burst", body: "limits:\n  control_write_burst: -1\n", err: "limit control_write_burst must be zero or a positive number; fix:"},
		{name: "the keys file, the hand-maintained file and the bridge by default", body: dsn + "control_db: starfix_control\n",
			check: func(s Settings) bool {
				return s.KeysFile == ".ssh/starfix_keys" && s.AuthorizedKeys == ".ssh/authorized_keys" && s.Bridge == "/usr/local/bin/starfixd"
			}},
		{name: "the three set in the file", body: "keys_file: /srv/starfix/keys\nauthorized_keys: /srv/starfix/authorized_keys\nbridge: /opt/starfix/bin/starfixd\n",
			check: func(s Settings) bool {
				return s.KeysFile == "/srv/starfix/keys" && s.AuthorizedKeys == "/srv/starfix/authorized_keys" && s.Bridge == "/opt/starfix/bin/starfixd"
			}},
		{name: "the environment wins over the file", body: "keys_file: /srv/starfix/keys\nbridge: /opt/starfix/bin/starfixd\n",
			env: map[string]string{EnvKeysFile: "keys/starfix", EnvAuthorizedKeys: "keys/by_hand", EnvBridge: "/usr/bin/starfixd"},
			check: func(s Settings) bool {
				return s.KeysFile == "keys/starfix" && s.AuthorizedKeys == "keys/by_hand" && s.Bridge == "/usr/bin/starfixd"
			}},
		{name: "a bridge that is not absolute", body: "bridge: starfixd\n", err: `bridge "starfixd" cannot be written into a forced command; fix:`},
		{name: "a bridge that needs quoting", body: "bridge: '/usr/local/bin/starfixd --socket /tmp/x'\n",
			err: `cannot be written into a forced command; fix:`},
		{name: "a bridge that closes the quotes", body: `bridge: '/usr/local/bin/starfixd" ssh-ed25519 AAAA'` + "\n",
			err: `cannot be written into a forced command; fix:`},
		{name: "a keys file that is the authorized_keys file", body: "keys_file: .ssh/authorized_keys\n",
			err: `keys_file ".ssh/authorized_keys" is the authorized_keys file, which starfixd reads and never writes; fix:`},
		{name: "the same file under another spelling", body: "keys_file: /srv/keys\nauthorized_keys: /srv/x/../keys\n",
			err: `is the authorized_keys file, which starfixd reads and never writes; fix:`},
		{name: "no block of project databases by default", body: dsn + "control_db: starfix_control\n",
			check: func(s Settings) bool { return s.ProjectDBs == ProjectDBs{} }},
		{name: "a block of project databases", body: dsn + "control_db: starfix_control\nproject_dbs:\n  prefix: starfix_p\n  count: 100\n",
			check: func(s Settings) bool {
				return s.ProjectDBs == ProjectDBs{Prefix: "starfix_p", Count: 100} && s.ProjectDBs.Name(100) == "starfix_p100"
			}},
		{name: "a block that holds the DSN's database", body: "dsn: starfix@tcp(127.0.0.1:3306)/starfix_p001\ncontrol_db: starfix_control\nproject_dbs:\n  prefix: starfix_p\n  count: 9\n",
			check: func(s Settings) bool { return s.ProjectDBs.Count == 9 }},
		{name: "a block without a control database", body: dsn + "project_dbs:\n  prefix: starfix_p\n  count: 100\n",
			err: "project_dbs is set without control_db; fix:"},
		{name: "a block that holds the control database", body: dsn + "control_db: starfix_p007\nproject_dbs:\n  prefix: starfix_p\n  count: 100\n",
			err: "project_dbs holds starfix_p007, the control database; fix:"},
		{name: "a block with a bad prefix", body: dsn + "control_db: starfix_control\nproject_dbs:\n  prefix: Starfix-P\n  count: 100\n",
			err: `project_dbs prefix "Starfix-P" does not start a database name; fix:`},
		{name: "a block with no count", body: dsn + "control_db: starfix_control\nproject_dbs:\n  prefix: starfix_p\n",
			err: "project_dbs count 0 must be 1 to 999; fix:"},
		{name: "a block past the most", body: dsn + "control_db: starfix_control\nproject_dbs:\n  prefix: starfix_p\n  count: 1000\n",
			err: "project_dbs count 1000 must be 1 to 999; fix:"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ResolveSettings(Settings{}, write("c.yaml", tc.body), func(k string) string { return tc.env[k] })
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || !strings.Contains(err.Error(), "fix:") {
					t.Fatalf("ResolveSettings = %v, want an error containing %q and a fix", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.check(s) {
				t.Errorf("settings = %+v", s)
			}
		})
	}
}
