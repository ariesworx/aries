package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/server"
	"github.com/ariesworx/aries/internal/store"
)

// Stage 3a in the soak (design §15): the run's server holds three
// projects, and while the sessions work in two of them an admin, the
// conductor, ends and restores access, as `aries admin` does, over the
// protocol and under the same chaos:
//
//	first   alice, bob, carol (its admin), dave
//	second  alice, carol, dave, erin
//	quiet   frank, when the conductor connects as him
//
// dave and root are server admins. alice's and bob's keys are only in
// the control store, so the keys file the daemon writes is what lets
// them in; erin's is in the store and in authorized_keys, as `ariesd
// control init` leaves a key, and sshd takes the keys file's line while
// the store holds it, since it reads that file first (decision 26); the
// others' are lines of authorized_keys.

// How a principal's key reaches sshd.
const (
	keyFile  = "file"  // a line of authorized_keys
	keyStore = "store" // the control store only
	keyBoth  = "both"  // both, as an import leaves it
)

// soakLayout is who works where: in lists the projects (a, b) a
// principal's sessions work in, the first as its admin when admin is
// set.
var soakLayout = []struct {
	name, in, key string
	admin         bool
}{
	{name: "alice", in: "ab", key: keyStore},
	{name: "bob", in: "a", key: keyStore},
	{name: "carol", in: "ab", key: keyFile, admin: true},
	{name: "dave", in: "ab", key: keyFile},
	{name: "erin", in: "b", key: keyBoth},
}

// setup builds the run's projects and principals as root, through the
// admin ops: nothing is written to the control store behind the daemon.
func (s *soak) setup() {
	t := s.t
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	s.ctl = newCtlReplay(s.mon.fail, []string{s.cfg.admin, s.cfg.root})
	s.cdb = openCheckerDB(t, s.w.controlDSN)

	rootA := s.w.newUser(s.cfg.root, "")
	c, err := mcpserver.DialRepo(ctx, rootA.repo, s.eveOptions("setup"))
	if err != nil {
		t.Fatalf("soak setup: dial as %s: %v", s.cfg.root, err)
	}
	defer func() { _ = c.Close() }()
	call := func(op string, args, res any) {
		t.Helper()
		if err := c.Call(ctx, op, args, res); err != nil {
			t.Fatalf("soak setup: %s %+v: %v", op, args, err)
		}
	}
	s.a = &sproj{id: project, name: "first", prefix: "sf", db: openCheckerDB(t, s.w.dsn)}
	add := func(name, prefix string) *sproj {
		var res proto.ProjectAddResult
		call(proto.OpProjectAdd, proto.ProjectAddArgs{Name: name, Prefix: prefix}, &res)
		dsn, err := server.DatabaseDSN(s.w.dsn, res.Database)
		if err != nil {
			t.Fatalf("soak setup: %v", err)
		}
		return &sproj{id: res.ID, name: name, prefix: prefix, db: openCheckerDB(t, dsn)}
	}
	s.b, s.c = add("second", "sb"), add("quiet", "sq")
	s.projs = []*sproj{s.a, s.b, s.c}
	for _, p := range s.projs {
		// A project admin's override is checked against the control log.
		p.rep = newReplay(s.mon.fail, []string{s.cfg.admin, s.cfg.root})
		p.rep.roles = true
		var others []string
		for _, q := range s.projs {
			if q != p {
				others = append(others, q.prefix)
			}
		}
		p.foreign = regexp.MustCompile(`\b(` + strings.Join(others, "|") + `)-[a-z2-7]{8}\b`)
	}

	byLetter := map[byte]*sproj{'a': s.a, 'b': s.b, 'c': s.c}
	// enroll gives name a key and a repository for each project in in,
	// and, unless it is a server admin, a row and its memberships.
	enroll := func(name, in, key string, admin bool) map[*sproj]*user {
		first := byLetter[in[0]]
		reg := name
		if key == keyStore {
			reg = "" // no line in authorized_keys
		}
		u := s.w.newUserIn(reg, "", first.id)
		users := map[*sproj]*user{first: u}
		for i := 1; i < len(in); i++ {
			p := byLetter[in[i]]
			v := u.in(fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: %d\n  user: aries\n  host_key: %s\n", p.id, s.w.addr.Port, s.w.hostFpr))
			v.pubFile = u.pubFile
			users[p] = v
		}
		if name == s.cfg.admin || name == s.cfg.root {
			return users
		}
		args := proto.PrincipalAddArgs{Name: name, Project: first.id}
		if key != keyFile {
			args.Keys = []string{strings.TrimSpace(readFile(t, u.pub()))}
		}
		if key == keyStore {
			s.storeKey[name] = u.fingerprint()
		}
		call(proto.OpPrincipalAdd, args, nil)
		if admin {
			call(proto.OpMemberSet, proto.MemberSetArgs{Name: name, Project: first.id, Role: string(store.RoleAdmin)}, nil)
		}
		for i := 1; i < len(in); i++ {
			// The setup, as a script: the keys it just gave are accepted.
			call(proto.OpMemberSet, proto.MemberSetArgs{Name: name, Project: byLetter[in[i]].id, AcceptKeys: true}, nil)
		}
		return users
	}
	for _, l := range soakLayout {
		s.users[l.name] = enroll(l.name, l.in, l.key, l.admin)
	}
	s.users[s.cfg.root] = map[*sproj]*user{s.a: rootA}
	for p, u := range enroll(s.cfg.root, "c", keyFile, false) {
		s.users[s.cfg.root][p] = u
	}
	s.users["frank"] = enroll("frank", "c", keyFile, false)
	s.eve = enroll("eve", "abc", keyFile, false)
	// Every request above was answered.
	s.answered()
}

// connSet is the sessions' open connections, by whose they are.
type connSet struct {
	mu sync.Mutex
	by map[mkey][]*mcpserver.RepoConn
}

// add records a connection of principal to p.
func (cs *connSet) add(principal string, p *sproj, c *mcpserver.RepoConn) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.by == nil {
		cs.by = map[mkey][]*mcpserver.RepoConn{}
	}
	k := mkey{principal, p.id}
	// Those that have closed since are forgotten here.
	cs.by[k] = append(slices.DeleteFunc(cs.by[k], func(c *mcpserver.RepoConn) bool { return c.Err() != nil }), c)
}

// of returns the connections that match and have not failed.
func (cs *connSet) of(match func(principal, project string) bool) []*mcpserver.RepoConn {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []*mcpserver.RepoConn
	for k, conns := range cs.by {
		if !match(k.principal, k.project) {
			continue
		}
		for _, c := range conns {
			if c.Err() == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

// dialRec is one handshake a session tried: when, and how it ended.
type dialRec struct {
	who sessKey
	p   *sproj
	// start and end are when it was made, as the run reckons the store's
	// time (vclock.now): for the report, and never for the verdict, since
	// that reckoning is not the control store's own time.
	start, end time.Time
	// known is the last event of the control log that the server had
	// answered for when the handshake began (soak.ctlKnown), and seen
	// the last the log held when it ended, or unknownSeq.
	known, seen int64
	welcomed    bool
	code        proto.Code // the refusal's, when the server or sshd refused
}

// unknownSeq stands for a place in the control log that could not be
// read: later than every event.
const unknownSeq = math.MaxInt64

// dialed records a session's handshake, and its connection when it was
// let in.
func (s *soak) dialed(ss *session, known int64, start time.Time, c *mcpserver.RepoConn, err error) {
	d := dialRec{who: ss.key, p: ss.p, start: start, end: s.clock.now(), known: known, seen: unknownSeq, welcomed: err == nil}
	if seq, ok := s.ctlSeq(); ok {
		d.seen = seq
	}
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		d.code = pe.Code
	}
	s.mon.dialed(d)
	if err == nil {
		s.open.add(ss.key.principal, ss.p, c)
	}
}

// ctlSeq reads the last event of the control log, as it is committed
// now.
func (s *soak) ctlSeq() (int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var seq int64
	err := s.cdb.QueryRowContext(ctx, `SELECT seq FROM events ORDER BY seq DESC LIMIT 1`).Scan(&seq)
	return seq, err == nil
}

// answered notes that the server has answered an admin's request, and
// with it for every event the control log now holds: the server answers
// a change once its memory has taken it, and only the conductor, one
// request at a time, changes what a handshake is answered from. The only
// other writes to the control store are the daemon's clock.step events,
// which change no one's access.
func (s *soak) answered() {
	seq, ok := s.ctlSeq()
	for ok {
		cur := s.ctlKnown.Load()
		if seq <= cur || s.ctlKnown.CompareAndSwap(cur, seq) {
			return
		}
	}
}

// refusedPause is how much longer a session waits before it dials again
// after being refused for want of access: its access comes back when the
// admin restores it, and an SSH handshake is not cheap.
func refusedPause(err error) time.Duration {
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		switch pe.Code {
		case proto.CodeForbidden, proto.CodeNotFound, proto.CodeAuth:
			return 100 * time.Millisecond
		}
	}
	return 0
}

// checkForeign fails the run when an answer names an issue of another
// project than the one it was asked of: no issue, claim, event or inbox
// item of one project is visible through a connection to another.
func (s *soak) checkForeign(cl call) {
	if cl.out != acked || cl.res == nil || cl.p == nil || cl.p.foreign == nil {
		return
	}
	b, err := json.Marshal(cl.res)
	if err != nil {
		return
	}
	if m := cl.p.foreign.Find(b); m != nil {
		s.mon.fail(string(m), "%s, connected to project %s, was answered %s naming %s, an issue of another project", cl.who, cl.p.name, cl.op, m)
	}
}

// foreignIssue returns an issue of another working project than p.
func (s *soak) foreignIssue(p *sproj, rng *rand.Rand) (string, bool) {
	other := s.a
	if p == s.a {
		other = s.b
	}
	return other.pool.pick(rng)
}

// eps is how far apart, on the store's clock, the event of a change to
// the control store and its effect may be recorded: an event carries the
// time its transaction began, and the daemon acts once it has committed.
func (s *soak) eps() time.Duration { return time.Duration(float64(time.Second) * s.cfg.speed) }

// accessStats counts what the conductor did, for the report.
type accessStats struct {
	removals, disables, keyRemovals, fileKeys, demotions, archives, quiet, statuses atomic.Int64
	// archivesHard counts the archives made the hard way: the daemon
	// restarted meanwhile, the answer was lost and the archive sent
	// again, or the project's store was closed or had no connection.
	// Their claims end as promptly as any other's (decision 24).
	archivesHard atomic.Int64
	// closed counts the connections seen to close as access ended, and
	// refusals and admissions the handshakes tried right after a change.
	closed, refusals, admissions atomic.Int64
	// slowest is the longest, in nanoseconds, a principal still held a
	// claim after the change that ended its access was acknowledged.
	slowest atomic.Int64
	// warned counts the removals of a key still in authorized_keys that
	// warned of it.
	warned atomic.Int64
	// shown counts the times a principal was added again only after its
	// keys were shown and accepted (decision 36).
	shown atomic.Int64
}

// acall is the conductor's or a probe's connection as one principal:
// redialed when it breaks, with each request sent again until it is
// answered, as an admin would run the command again.
type acall struct {
	s    *soak
	ss   *session
	conn *mcpserver.RepoConn
}

func (s *soak) newACall(principal, id string, p *sproj) *acall {
	return &acall{s: s, ss: &session{s: s, p: p, key: sessKey{principal, id}, u: s.user(principal, p)}}
}

func (ac *acall) options() client.Options {
	return client.Options{Version: "v0.2.0", Session: ac.ss.key.session, Machine: "laptop-" + ac.ss.key.principal,
		Getenv: func(string) string { return "" }}
}

// do sends one request until it is answered, and reports whether any
// attempt's answer was lost: the request may then have been applied
// before the attempt that was answered.
func (ac *acall) do(op string, args, res any) (out outcome, perr *proto.Error, resent bool) {
	deadline := time.Now().Add(callTimeout)
	for {
		if ac.conn == nil || ac.conn.Err() != nil {
			ac.close()
			dctx, cancel := context.WithTimeout(context.Background(), callTimeout)
			c, err := mcpserver.DialRepo(dctx, ac.ss.u.repo, ac.options())
			cancel()
			if err != nil {
				if time.Now().After(deadline) {
					ac.s.mon.fail("", "%s could not connect to project %s for %s in %s: %v", ac.ss.key, ac.ss.p.name, op, callTimeout, err)
					return lost, nil, resent
				}
				pause(context.Background(), 30*time.Millisecond)
				continue
			}
			ac.conn = c
		}
		if res != nil {
			reflect.ValueOf(res).Elem().SetZero()
		}
		restarts := ac.s.restarting.Load()
		out, perr = ac.s.do(ac.ss, ac.conn, op, args, res)
		// A daemon that is stopping cancels the requests in flight, and
		// one of them may still be answered before its connection
		// closes: `unavailable`, with a fix to retry, which is the
		// daemon saying it does not know whether the change was made
		// (seeds 14245368110280542215 and 15006515746959414509, where
		// it was canceled reading the project, before any write). That
		// is an answer lost, not a refusal, and only while a restart is
		// under way: at any other time it fails the run as before.
		if out == refused && perr.Code == proto.CodeUnavailable &&
			(ac.s.restarting.Load() != restarts || ac.s.restarting.Load() > ac.s.stats.restarts.Load()) {
			out, perr = lost, nil
		}
		if out == acked {
			ac.s.answered()
		}
		if out != lost {
			return out, perr, resent
		}
		resent = true
		ac.close()
		if time.Now().After(deadline) {
			ac.s.mon.fail("", "%s: %s was not answered in %s", ac.ss.key, op, callTimeout)
			return lost, nil, resent
		}
	}
}

func (ac *acall) close() {
	if ac.conn != nil {
		_ = ac.conn.Close()
		ac.conn = nil
	}
}

// conductor is the admin of the run.
type conductor struct {
	s   *soak
	rng *rand.Rand
	// root is a server admin's connection, and carol's the admin of the
	// first project's while she is one.
	root, carol *acall
	carolAdmin  bool
	n           int
}

// conduct ends and restores access until ctx ends. Each kind of change
// is made once first, in an order that fits the short run, and the first
// round is finished even if the work has stopped; after that they come
// at random, the archive of a whole project one time in eight.
func (s *soak) conduct(ctx context.Context) {
	cd := &conductor{s: s, rng: rand.New(rand.NewPCG(s.cfg.seed, 11)), carolAdmin: true, //nolint:gosec // reproducible choices, not security
		root: s.newACall(s.cfg.root, "conductor-root", s.a), carol: s.newACall("carol", "conductor-carol", s.a)}
	defer cd.root.close()
	defer cd.carol.close()
	first := []func(){
		func() { cd.membership("alice", s.b, cd.root) },
		func() { cd.key("bob", s.a) },
		func() { cd.disable("alice") },
		cd.role,
		cd.archive,
		cd.closedStore,
		cd.fileKey,
		func() { cd.membership("bob", s.a, cd.carol) },
	}
	for _, op := range first {
		pause(ctx, time.Duration(50+cd.rng.IntN(100))*time.Millisecond)
		op()
		cd.status()
	}
	for ctx.Err() == nil {
		pause(ctx, time.Duration(100+cd.rng.IntN(400))*time.Millisecond)
		if ctx.Err() != nil {
			return
		}
		switch n := cd.rng.IntN(16); {
		case n < 2:
			cd.membership("alice", []*sproj{s.a, s.b}[cd.rng.IntN(2)], cd.root)
		case n < 4:
			cd.membership("bob", s.a, cd.carol)
		case n < 5:
			cd.membership("erin", s.b, cd.root)
		case n < 7:
			cd.disable([]string{"alice", "bob", "erin"}[cd.rng.IntN(3)])
		case n < 9:
			cd.key([]string{"alice", "bob"}[cd.rng.IntN(2)], s.a)
		case n < 10:
			cd.fileKey()
		case n < 12:
			cd.role()
		case n < 14:
			cd.closedStore()
		default:
			cd.archive()
		}
		cd.status()
	}
}

// hold is how long access stays ended: longer than a reaper tick, so the
// daemon's own sweep has run, and short, so the sessions get on.
func (cd *conductor) hold() {
	pause(context.Background(), time.Duration(250+cd.rng.IntN(250))*time.Millisecond)
}

// accessDeadline bounds each wait for the daemon to act on a change.
const accessDeadline = 20 * time.Second

// closes waits for conns to be closed: the connections that were open
// just before the access that let them in ended, which the server closes
// unless something else did first.
func (cd *conductor) closes(why string, conns []*mcpserver.RepoConn) {
	cd.s.acc.closed.Add(int64(len(conns)))
	deadline := time.Now().Add(accessDeadline)
	for {
		conns = slices.DeleteFunc(conns, func(c *mcpserver.RepoConn) bool { return c.Err() != nil })
		if len(conns) == 0 {
			return
		}
		if time.Now().After(deadline) {
			cd.s.mon.fail("", "%s: %d connections that the access let in are still open after %s", why, len(conns), accessDeadline)
			return
		}
		pause(context.Background(), 10*time.Millisecond)
	}
}

// dial tries a handshake as principal for p, again while it fails for
// a reason that is not the daemon's answer (a dropped connection, a
// daemon restarting, sshd finding no key while it does), and wants the
// refusal code given, or with "" a welcome. It returns the connection
// when it was let in and that was wanted.
func (cd *conductor) dial(principal string, p *sproj, want proto.Code, why string) *mcpserver.RepoConn {
	s := cd.s
	ac := s.newACall(principal, fmt.Sprintf("probe-%s-%d", principal, cd.n), p)
	cd.n++
	deadline := time.Now().Add(accessDeadline)
	var last error
	for time.Now().Before(deadline) {
		dctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		c, err := mcpserver.DialRepo(dctx, ac.ss.u.repo, ac.options())
		cancel()
		last = err
		pe, _ := errors.AsType[*proto.Error](err)
		switch {
		case err == nil && want == "":
			s.acc.admissions.Add(1)
			return c
		case err == nil:
			_ = c.Close()
			s.mon.fail("", "%s: %s was let into project %s; want a refusal (%s)", why, principal, p.name, want)
			return nil
		case pe != nil && want != "" && pe.Code == want:
			s.acc.refusals.Add(1)
			return nil
		case pe != nil && want == "" && (pe.Code == proto.CodeForbidden || pe.Code == proto.CodeNotFound):
			s.mon.fail("", "%s: %s was refused project %s (%s: %s); want it let in", why, principal, p.name, pe.Code, pe.Message)
			return nil
		}
		pause(context.Background(), 30*time.Millisecond)
	}
	s.mon.fail("", "%s: no answer to a handshake of %s for project %s in %s (want %q); the last error: %v", why, principal, p.name, accessDeadline, want, last)
	return nil
}

// held returns the issues principal holds in p, by its claims table;
// every holder's when principal is empty.
func (cd *conductor) held(p *sproj, principal string) []string {
	query, args := `SELECT issue_id FROM claims WHERE principal IS NOT NULL`, []any(nil)
	if principal != "" {
		query, args = `SELECT issue_id FROM claims WHERE principal = ?`, []any{principal}
	}
	var ids []string
	err := scan(context.Background(), p.db, query, args, func(rows *sql.Rows) error {
		var id string
		err := rows.Scan(&id)
		ids = append(ids, id)
		return err
	})
	if err != nil {
		cd.s.t.Errorf("read claims of project %s: %v", p.name, err)
	}
	return ids
}

// noClaims waits until principal holds nothing in p (no one does, when
// principal is empty): the claims its access let it take have ended.
func (cd *conductor) noClaims(why string, p *sproj, principal string, since time.Time) {
	deadline := time.Now().Add(accessDeadline)
	for {
		ids := cd.held(p, principal)
		if len(ids) == 0 {
			if d := int64(time.Since(since)); d > cd.s.acc.slowest.Load() {
				cd.s.acc.slowest.Store(d)
			}
			return
		}
		if time.Now().After(deadline) {
			for _, id := range ids {
				cd.s.mon.fail(id, "%s: %s is still claimed in project %s %s after the access ended", why, id, p.name, accessDeadline)
			}
			return
		}
		pause(context.Background(), 10*time.Millisecond)
	}
}

// stillNone checks, as the access is about to be restored, that the
// principal holds nothing still: a request that was in flight when the
// access ended may have taken a claim after the admin's release, and the
// daemon's sweep ends that one within a reaper tick.
func (cd *conductor) stillNone(why string, p *sproj, principal string) {
	if len(cd.held(p, principal)) == 0 {
		return
	}
	deadline := time.Now().Add(accessDeadline)
	for time.Now().Before(deadline) {
		if len(cd.held(p, principal)) == 0 {
			return
		}
		pause(context.Background(), 10*time.Millisecond)
	}
	for _, id := range cd.held(p, principal) {
		cd.s.mon.fail(id, "%s: %s is claimed in project %s by a holder without access, and no sweep ended it in %s", why, id, p.name, accessDeadline)
	}
}

// answered reports whether an access change took effect: it was
// acknowledged, or, when an earlier attempt's answer was lost, refused
// with code, as the change made twice is.
func answered(out outcome, perr *proto.Error, resent bool, code proto.Code) bool {
	return out == acked || (resent && out == refused && perr.Code == code)
}

// membership removes principal from p as by, checks that its connections
// to p close, that it is refused there and that its claims there end,
// and adds it again.
func (cd *conductor) membership(principal string, p *sproj, by *acall) {
	s := cd.s
	if by == cd.carol && !cd.carolAdmin {
		by = cd.root
	}
	why := fmt.Sprintf("%s removed %s from project %s", by.ss.key.principal, principal, p.name)
	// The connections open now: those still open when the removal
	// commits are the server's to close.
	conns := s.open.of(func(who, project string) bool { return who == principal && project == p.id })
	var rm proto.MemberRemoveResult
	out, perr, resent := by.do(proto.OpMemberRemove, proto.MemberArgs{Name: principal, Project: p.id}, &rm)
	if !answered(out, perr, resent, proto.CodeNotFound) {
		s.mon.fail("", "%s: %v, %v", why, out, perr)
		return
	}
	since := time.Now()
	s.acc.removals.Add(1)
	cd.closes(why, conns)
	cd.dial(principal, p, proto.CodeForbidden, why)
	cd.noClaims(why, p, principal, since)
	cd.hold()
	cd.stillNone(why, p, principal)

	// By a server admin: a project admin may not place a principal that
	// is left in no project and that they did not create (decision 17).
	// Shown its keys and not yet accepted, it is still refused.
	if !cd.join(principal, p, why, func() {
		cd.dial(principal, p, proto.CodeForbidden, why+", and was shown its keys without accepting them")
	}) {
		return
	}
	if c := cd.dial(principal, p, "", why+", and added it again"); c != nil {
		_ = c.Close()
	}
}

// join adds principal to p as root, the way an admin at a terminal does
// (decision 36). The first request accepts no keys: it must add a
// principal that has none, and must not add one that has any, answering
// with every key instead, the one the soak gave it among them. unaccepted,
// if set, runs then, before the second request accepts. It reports
// whether principal was added.
func (cd *conductor) join(principal string, p *sproj, why string, unaccepted func()) bool {
	s := cd.s
	args := proto.MemberSetArgs{Name: principal, Project: p.id}
	var set proto.MemberSetResult
	if out, perr, _ := cd.root.do(proto.OpMemberSet, args, &set); out != acked {
		s.mon.fail("", "%s, and could not add it again: %v, %v", why, out, perr)
		return false
	}
	fp := s.storeKey[principal]
	if set.Change != proto.MemberPending {
		if fp != "" {
			s.mon.fail("", "%s: %s, who has key %s in the store, was added again (%s) by a request that accepted no keys", why, principal, fp, set.Change)
		}
		return true
	}
	s.acc.shown.Add(1)
	if fp != "" && !slices.ContainsFunc(set.Carried, func(k proto.CarriedKey) bool { return k.Fingerprint == fp && k.AddedBy != "" }) {
		s.mon.fail("", "%s: adding it again showed %+v, without its key %s and who added it", why, set.Carried, fp)
	}
	if len(set.Carried) == 0 {
		s.mon.fail("", "%s: adding it again is pending on keys, and none was shown", why)
	}
	if unaccepted != nil {
		unaccepted()
	}
	args.AcceptKeys = true
	if out, perr, _ := cd.root.do(proto.OpMemberSet, args, &set); out != acked {
		s.mon.fail("", "%s, and could not add it again, accepting its keys: %v, %v", why, out, perr)
		return false
	}
	return true
}

// disable disables principal, checks that every connection it has
// closes, that it is refused everywhere and that its claims in every
// project end, and enables it again.
func (cd *conductor) disable(principal string) {
	s := cd.s
	why := fmt.Sprintf("%s disabled %s", s.cfg.root, principal)
	conns := s.open.of(func(who, _ string) bool { return who == principal })
	var res proto.PrincipalResult
	if out, perr, _ := cd.root.do(proto.OpPrincipalDisable, proto.PrincipalArgs{Name: principal}, &res); out != acked {
		s.mon.fail("", "%s: %v, %v", why, out, perr)
		return
	}
	since := time.Now()
	s.acc.disables.Add(1)
	cd.closes(why, conns)
	in := slices.DeleteFunc([]*sproj{s.a, s.b}, func(p *sproj) bool { return s.user(principal, p) == nil })
	for _, p := range in {
		// The keys file has no line for a key of the store whose principal
		// is disabled; a key in authorized_keys reaches the daemon's refusal.
		want := proto.CodeForbidden
		if s.storeKey[principal] != "" {
			want = proto.CodeAuth
		}
		cd.dial(principal, p, want, why)
		cd.noClaims(why, p, principal, since)
	}
	cd.hold()
	for _, p := range in {
		cd.stillNone(why, p, principal)
	}
	if out, perr, _ := cd.root.do(proto.OpPrincipalEnable, proto.PrincipalArgs{Name: principal}, &res); out != acked {
		s.mon.fail("", "%s, and could not enable it again: %v, %v", why, out, perr)
		return
	}
	for _, p := range in {
		if c := cd.dial(principal, p, "", why+", and enabled it again"); c != nil {
			_ = c.Close()
		}
	}
}

// key removes the one key of principal, which only the store holds:
// the connections it let in close and the key lets nobody in, until it
// is added again. It releases no claim: those lapse with their leases.
func (cd *conductor) key(principal string, p *sproj) {
	s := cd.s
	fp := s.storeKey[principal]
	why := fmt.Sprintf("%s removed the key of %s", s.cfg.root, principal)
	conns := s.open.of(func(who, _ string) bool { return who == principal })
	var res proto.KeyResult
	out, perr, resent := cd.root.do(proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fp}, &res)
	if !answered(out, perr, resent, proto.CodeNotFound) {
		s.mon.fail("", "%s: %v, %v", why, out, perr)
		return
	}
	s.acc.keyRemovals.Add(1)
	if out == acked && (res.Warning != "" || res.InAuthorizedKeys) {
		s.mon.fail("", "%s: warned %q (in authorized_keys: %t), when the keys file was written and authorized_keys holds no key of %s",
			why, res.Warning, res.InAuthorizedKeys, principal)
	}
	cd.closes(why, conns)
	cd.dial(principal, p, proto.CodeAuth, why)
	cd.hold()
	line := strings.TrimSpace(readFile(s.t, s.user(principal, p).pub()))
	if out, perr, _ := cd.root.do(proto.OpKeyAdd, proto.KeyAddArgs{Name: principal, Key: line}, &res); out != acked {
		s.mon.fail("", "%s, and could not add it again: %v, %v", why, out, perr)
		return
	}
	if c := cd.dial(principal, p, "", why+", and added it again"); c != nil {
		_ = c.Close()
	}
}

// fileKey removes erin's key from the store while authorized_keys still
// has its line, as after `ariesd control init`. sshd reads the keys
// file first (decision 26), so while the store holds the key erin's
// connections name it, and the removal closes them. The key is not
// revoked: sshd then takes the line of authorized_keys, and the daemon,
// which reads that file, says so on every removal. A connection made
// then names no key: `key list` and `status` count it as let in by that
// file, and a second removal leaves it open.
func (cd *conductor) fileKey() {
	s := cd.s
	const principal = "erin"
	fp := s.user(principal, s.b).fingerprint()
	line := strings.TrimSpace(readFile(s.t, s.user(principal, s.b).pub()))
	why := fmt.Sprintf("%s removed the key of %s from the store, and authorized_keys still holds it", s.cfg.root, principal)
	// remove removes the key, and checks the warning of a removal that
	// was answered and not sent twice (a second sending finds no key).
	remove := func() {
		var res proto.KeyResult
		out, perr, resent := cd.root.do(proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: fp}, &res)
		switch {
		case !answered(out, perr, resent, proto.CodeNotFound):
			s.mon.fail("", "%s: %v, %v", why, out, perr)
		case out != acked || resent:
		case !res.InAuthorizedKeys || !strings.HasPrefix(res.Warning, "sshd will still accept key "+fp+": the connections it had signed in were closed, but it is also a line of the server's authorized_keys file"):
			s.mon.fail("", "%s: the removal answered in authorized_keys %t and the warning %q", why, res.InAuthorizedKeys, res.Warning)
		default:
			s.acc.warned.Add(1)
		}
	}
	add := func() {
		var res proto.KeyResult
		if out, perr, _ := cd.root.do(proto.OpKeyAdd, proto.KeyAddArgs{Name: principal, Key: line}, &res); out != acked {
			s.mon.fail("", "%s, and could not add it again: %v, %v", why, out, perr)
		}
	}
	// The store holds the key and the keys file its line, so this
	// connection names the key, and the removal closes it. When the
	// daemon restarts meanwhile it closes anyway, which proves less.
	byStore := cd.dial(principal, s.b, "", "before "+why)
	remove()
	s.acc.fileKeys.Add(1)
	if byStore != nil {
		cd.closes(why, []*mcpserver.RepoConn{byStore})
		_ = byStore.Close() // the server closed its end; this ends the SSH connection under it
	}
	// The store no longer has the key, and authorized_keys lets erin in.
	// The counts below are checked only if the chaos dropped no
	// connection from before this one was made: a connection it dropped
	// on the server's side may not yet have been seen to close here.
	shaken := s.shaken.Load()
	byFile := cd.dial(principal, s.b, "", why)
	if byFile == nil {
		add()
		return
	}
	defer func() { _ = byFile.Close() }()
	var ks proto.KeysResult
	if out, _, _ := cd.root.do(proto.OpKeys, proto.KeysArgs{Name: principal}, &ks); out == acked && byFile.Err() == nil && s.shaken.Load() == shaken && ks.FileConns < 1 {
		s.mon.fail("", "%s: key list counts %d connections of %s by authorized_keys, with one open", why, ks.FileConns, principal)
	}
	var st proto.StatusResult
	if out, _, _ := cd.root.do(proto.OpStatus, proto.StatusArgs{}, &st); out == acked && byFile.Err() == nil && s.shaken.Load() == shaken {
		for _, ps := range st.Projects {
			if ps.ID == s.b.id && ps.FileConns < 1 {
				s.mon.fail("", "%s: status counts %d connections to project %s by authorized_keys, with one of %s's open", why, ps.FileConns, s.b.name, principal)
			}
		}
	}
	add()
	// In the store again, the key is marked as also in authorized_keys.
	if out, _, _ := cd.root.do(proto.OpKeys, proto.KeysArgs{Name: principal}, &ks); out == acked &&
		!slices.ContainsFunc(ks.Keys, func(k proto.Key) bool { return k.Fingerprint == fp && k.InAuthorizedKeys }) {
		s.mon.fail("", "%s: key list does not mark the key as also in authorized_keys: %+v", why, ks.Keys)
	}
	// The connection authorized_keys let in names no key: the removal
	// leaves it, unless the chaos dropped every connection meanwhile.
	remove()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	var r proto.BlockedResult
	err := byFile.Call(ctx, proto.OpBlocked, proto.LimitArgs{Limit: 1}, &r)
	cancel()
	if err != nil && s.shaken.Load() == shaken {
		s.mon.fail("", "%s, and the second removal closed the connection authorized_keys had let in: %v", why, err)
	}
	add()
}

// role takes the admin role of the first project from carol and gives
// it back. A demotion closes no connection and ends no claim; it ends
// her admin's rights at once.
func (cd *conductor) role() {
	s := cd.s
	why := "carol was made a plain member of project " + s.a.name
	shaken := s.shaken.Load()
	open := cd.dial("carol", s.a, "", "before "+why)
	set := func(role store.Role) bool {
		var res proto.MemberSetResult
		out, perr, _ := cd.root.do(proto.OpMemberSet, proto.MemberSetArgs{Name: "carol", Project: s.a.id, Role: string(role)}, &res)
		if out != acked {
			s.mon.fail("", "%s (%s): %v, %v", why, role, out, perr)
		}
		return out == acked
	}
	if !set(store.RoleMember) {
		return
	}
	cd.carolAdmin = false
	s.acc.demotions.Add(1)
	if open != nil {
		// The connection she had is still hers, and still answers: unless
		// the chaos dropped every connection meanwhile. The read is one
		// whose answer the chaos never drops.
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		var r proto.BlockedResult
		err := open.Call(ctx, proto.OpBlocked, proto.LimitArgs{Limit: 1}, &r)
		cancel()
		if err != nil && s.shaken.Load() == shaken {
			s.mon.fail("", "%s, and the connection she had open no longer answers: %v", why, err)
		}
		_ = open.Close()
	}
	var ms proto.MembersResult
	if out, perr, _ := cd.carol.do(proto.OpMembers, proto.MembersArgs{Project: s.a.id}, &ms); out != refused || perr.Code != proto.CodeForbidden {
		s.mon.fail("", "%s, and still lists its members: %v, %v", why, out, perr)
	}
	var st proto.StatusResult
	if out, perr, _ := cd.carol.do(proto.OpStatus, proto.StatusArgs{}, &st); out != refused || perr.Code != proto.CodeForbidden {
		s.mon.fail("", "%s, and is still shown the status: %v, %v", why, out, perr)
	}
	cd.hold()
	if !set(store.RoleAdmin) {
		return
	}
	cd.carolAdmin = true
	if out, perr, _ := cd.carol.do(proto.OpMembers, proto.MembersArgs{Project: s.a.id}, &ms); out != acked {
		s.mon.fail("", "carol is the admin of project %s again, and may not list its members: %v, %v", s.a.name, out, perr)
	}
	// A project admin's status is her own project, and nothing of the
	// server or of the projects she only works in.
	if out, perr, _ := cd.carol.do(proto.OpStatus, proto.StatusArgs{}, &st); out != acked {
		s.mon.fail("", "carol is the admin of project %s again, and is refused the status: %v, %v", s.a.name, out, perr)
	} else if st.Server != nil || len(st.Projects) != 1 || st.Projects[0].ID != s.a.id {
		s.mon.fail("", "status for carol, admin of project %s alone, shows the server (%t) and %d projects", s.a.name, st.Server != nil, len(st.Projects))
	}
}

// archive archives the second project, checks that every connection to
// it closes, that nobody is let in, a server admin included, that every
// claim in it ends and its store closes, and activates it again.
//
// The claims end at once, whatever happened meanwhile (decisions 21 and
// 24). When the daemon has the project's store open as the archive
// commits, the admin's release ends them, and the sweep as the store
// closes ends one taken by a request in flight. When the store is
// closed, the archive opens it to end them. And a daemon that restarted
// between the archive's control write and its release ends them before
// it listens again, though no one may connect to the project and so
// nothing else opens its store. So once the archive is answered and the
// daemon reports the store closed, the project's table holds no claim,
// with the project still archived.
func (cd *conductor) archive() {
	s, p := cd.s, cd.s.b
	why := fmt.Sprintf("%s archived project %s", s.cfg.root, p.name)
	restarts := s.restarting.Load()
	before, wasOpen := cd.statusOf(p)
	conns := s.open.of(func(_, project string) bool { return project == p.id })
	var res proto.ProjectResult
	out, perr, resent := cd.root.do(proto.OpProjectArchive, proto.ProjectArgs{Project: p.id}, &res)
	if out != acked {
		s.mon.fail("", "%s: %v, %v", why, out, perr)
		return
	}
	since := time.Now()
	s.acc.archives.Add(1)
	cd.closes(why, conns)
	// A member, and a server admin, who is let into every active project.
	cd.dial("alice", p, proto.CodeNotFound, why)
	cd.dial(s.cfg.admin, p, proto.CodeNotFound, why)
	cd.storeCloses(why, p)
	// The time of the project's latest event, which is its store's own:
	// a claim taken later is dated later, whatever the run's reckoning of
	// that time says, which may be minutes ahead of it after a restart.
	// A project archived before anything was written to it has no event,
	// and no claim either, on a run slow enough to get there first.
	var closed time.Time
	if err := p.db.QueryRowContext(context.Background(), `SELECT at FROM events ORDER BY seq DESC LIMIT 1`).Scan(&closed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.t.Errorf("read the latest event of project %s: %v", p.name, err)
	}
	// Every claim has ended, the one a request in flight took after the
	// admin's release included. Before decision 24 this was asked only
	// when the store was open with connections and the one daemon made
	// the change and answered; the other cases were left to the
	// activation (seed 16755269619008477475).
	if !wasOpen || !before.Open || before.Conns == 0 || resent || s.restarting.Load() != restarts {
		s.acc.archivesHard.Add(1)
	}
	cd.noClaims(why, p, "", since)
	cd.hold()
	if out, perr, _ := cd.root.do(proto.OpProjectActivate, proto.ProjectArgs{Project: p.id}, &res); out != acked {
		s.mon.fail("", "%s, and could not activate it again: %v, %v", why, out, perr)
		return
	}
	if c := cd.dial(s.cfg.admin, p, "", why+", and activated it again"); c != nil {
		_ = c.Close()
	}
	var stale []string
	err := scan(context.Background(), p.db, `SELECT issue_id FROM claims WHERE principal IS NOT NULL AND claimed_at <= ?`, []any{closed},
		func(rows *sql.Rows) error {
			var id string
			err := rows.Scan(&id)
			stale = append(stale, id)
			return err
		})
	if err != nil {
		s.t.Errorf("read claims of project %s: %v", p.name, err)
	}
	for _, id := range stale {
		s.mon.fail(id, "%s, and %s, claimed before the archive, is still claimed after the project was activated again", why, id)
	}
}

// statusOf asks the server's status as root and returns p's part.
func (cd *conductor) statusOf(p *sproj) (proto.ProjectStatus, bool) {
	var st proto.StatusResult
	if out, _, _ := cd.root.do(proto.OpStatus, proto.StatusArgs{}, &st); out != acked {
		return proto.ProjectStatus{}, false
	}
	for _, ps := range st.Projects {
		if ps.ID == p.id {
			return ps, true
		}
	}
	cd.s.mon.fail("", "status for a server admin lists no project %s", p.name)
	return proto.ProjectStatus{}, false
}

// storeCloses waits for the daemon to report p's store closed.
func (cd *conductor) storeCloses(why string, p *sproj) bool {
	deadline := time.Now().Add(accessDeadline)
	for time.Now().Before(deadline) {
		if ps, ok := cd.statusOf(p); ok && !ps.Open {
			return true
		}
		pause(context.Background(), 30*time.Millisecond)
	}
	cd.s.mon.fail("", "%s: the store of project %s is still open after %s with no connection", why, p.name, accessDeadline)
	return false
}

// closedStore ends a membership of the quiet project while its store is
// closed. The daemon does not open a store to release claims: the claim
// frank holds there stays in its table until the store next opens, and
// ends then, before the first request, with a claim.release of the
// server's own that gives the reason.
func (cd *conductor) closedStore() {
	s, p := cd.s, cd.s.c
	cd.n++
	frank := s.newACall("frank", fmt.Sprintf("quiet-%d", cd.n), p)
	defer frank.close()
	why := fmt.Sprintf("%s removed frank from project %s while its store was closed", s.cfg.root, p.name)
	title := fmt.Sprintf("task quiet-%d", cd.n)
	var created proto.CreateResult
	if out, perr, _ := frank.do(proto.OpCreate, proto.CreateArgs{Title: title, Idem: fmt.Sprintf("soak-quiet-%d", cd.n)}, &created); out != acked {
		s.mon.fail("", "frank could not create an issue in project %s: %v, %v", p.name, out, perr)
		return
	}
	// A lease the whole run cannot outlast: the claim ends for the lost
	// access, not because it lapsed.
	var started proto.StartResult
	if out, perr, _ := frank.do(proto.OpStart, proto.StartArgs{ID: created.ID, Lease: "24h"}, &started); out != acked || started.Claim == nil {
		s.mon.fail(created.ID, "frank could not start %s in project %s: %v, %v", created.ID, p.name, out, perr)
		return
	}
	frank.close()
	id := created.ID
	if !cd.storeCloses("frank left project "+p.name, p) {
		return
	}

	var rm proto.MemberRemoveResult
	out, perr, resent := cd.root.do(proto.OpMemberRemove, proto.MemberArgs{Name: "frank", Project: p.id}, &rm)
	if !answered(out, perr, resent, proto.CodeNotFound) {
		s.mon.fail(id, "%s: %v, %v", why, out, perr)
		return
	}
	s.acc.quiet.Add(1)
	defer cd.join("frank", p, why, nil)
	if !resent && rm.Released != 0 {
		s.mon.fail(id, "%s: the removal says it released %d claims, in a store that was closed", why, rm.Released)
	}
	if ps, ok := cd.statusOf(p); ok && ps.Open {
		s.mon.fail(id, "%s: the removal opened the store", why)
	}
	if held := cd.held(p, "frank"); !slices.Contains(held, id) {
		s.mon.fail(id, "%s: the claim on %s ended while the store was closed; frank holds %v", why, id, held)
	}

	// The store opens for root's connection, and ends the claim first.
	opener := s.newACall(s.cfg.root, fmt.Sprintf("quiet-root-%d", cd.n), p)
	defer opener.close()
	var cs proto.ClaimsResult
	if out, perr, _ := opener.do(proto.OpClaims, proto.LimitArgs{Limit: 100}, &cs); out != acked {
		s.mon.fail(id, "%s: %s could not read its claims: %v, %v", why, s.cfg.root, out, perr)
		return
	}
	if slices.ContainsFunc(cs.Claims, func(c proto.Claim) bool { return c.ID == id }) {
		s.mon.fail(id, "%s: the first request after the store opened still lists frank's claim on %s", why, id)
	}
	var by, session, after string
	err := scan(context.Background(), p.db, `SELECT principal, session, COALESCE(after_state, '') FROM events
  WHERE target = ? AND op = 'claim.release' ORDER BY seq DESC LIMIT 1`, []any{id}, func(rows *sql.Rows) error {
		return rows.Scan(&by, &session, &after)
	})
	if err != nil {
		s.t.Errorf("read the events of %s: %v", id, err)
		return
	}
	if by != "ariesd" || session != "access" || !strings.Contains(after, lostMembership) {
		s.mon.fail(id, "%s: the claim on %s was last released by %q/%q with %q; want the server's own release at the store's opening, for %q",
			why, id, by, session, after, lostMembership)
	}
}

// status asks the server's status as root and checks what must hold
// whatever the sessions are doing: the server's part is there, every
// project is listed once, and nothing is past its cap.
func (cd *conductor) status() {
	s := cd.s
	var st proto.StatusResult
	if out, _, _ := cd.root.do(proto.OpStatus, proto.StatusArgs{}, &st); out != acked {
		return
	}
	s.acc.statuses.Add(1)
	sv := st.Server
	if sv == nil {
		s.mon.fail("", "status for a server admin has no server part")
		return
	}
	if sv.OpenStores > sv.OpenStoresMax || sv.Conns > sv.ConnsMax || sv.OpenStores < 1 || sv.Conns < 1 {
		s.mon.fail("", "status: %d of %d stores, %d of %d connections", sv.OpenStores, sv.OpenStoresMax, sv.Conns, sv.ConnsMax)
	}
	// Nothing in the run stops the daemon writing its keys file.
	if k := sv.KeysFile; k == nil || k.Failed || k.Path != s.w.keysFile || k.WrittenAt == nil {
		s.mon.fail("", "status: the keys file is %+v; want %s, written, and its last write not failed", k, s.w.keysFile)
	}
	seen := map[string]bool{}
	for _, ps := range st.Projects {
		seen[ps.ID] = true
		if ps.FileConns > ps.Conns || (!ps.Open && (ps.Writes != nil || ps.Sessions != nil || ps.LastUsed != nil)) || (ps.Open && (ps.Writes == nil || ps.LastUsed == nil)) {
			s.mon.fail("", "status of project %s is not of a piece: %+v", ps.Name, ps)
		}
	}
	if len(st.Projects) != len(s.projs) || len(seen) != len(s.projs) || st.More != 0 {
		s.mon.fail("", "status lists %d projects and %d more, want the run's %d", len(st.Projects), st.More, len(s.projs))
	}
}

// checkLost: every claim of p that ended because its holder lost access
// did so for a reason the control store's log bears out, at the hand of
// the admin who ended the access or of the server itself.
//
// It places an event of the project's log against the control log by
// their times, which a server's stores share (store.Clock), give or
// take eps, and allows nothing for a jump of the clock: no write that
// begins after a jump is dated before it, so a release is never dated
// the jump earlier than the loss of access that caused it. It once was,
// when the release had been waiting for its store's writer as the clock
// jumped, and this check allowed for that until the store stopped doing
// it.
func (s *soak) checkLost(p *sproj) {
	c := s.ctl
	c.mu.Lock()
	defer c.mu.Unlock()
	eps := s.eps()
	for _, l := range p.rep.lost {
		who := l.holder.principal
		from := l.took
		if from.IsZero() || from.After(l.at) {
			from = l.at
		}
		until := l.at.Add(eps)
		lacked := slices.ContainsFunc(c.instants(who, p.id, "", from, until), func(t time.Time) bool { return c.lacks(who, p.id, l.reason, t) })
		if !lacked {
			s.mon.fail(l.issue, "event %d released %s's claim on %s for %q, but the control log shows no such loss of access while it was held (%s to %s)",
				l.seq, l.holder, l.issue, l.reason, stamp(from), stamp(until.Add(-eps)))
		}
		if l.by == (sessKey{"ariesd", "access"}) {
			continue
		}
		var admin string
		switch l.reason {
		case lostMembership:
			admin = c.lastBy(store.OpMemberRemove, p.id+"/"+who, until)
		case lostDisabled:
			admin = c.lastBy(store.OpPrincipalDisable, who, until)
		case lostArchived:
			admin = c.lastBy(store.OpProjectArchive, p.id, until)
		}
		if admin != l.by.principal {
			s.mon.fail(l.issue, "event %d: %s released %s's claim on %s for %q, but the control log says the access was last ended by %q",
				l.seq, l.by, l.holder, l.issue, l.reason, admin)
		}
	}
	for _, o := range p.rep.overrides {
		until := o.at.Add(eps)
		ok := slices.ContainsFunc(c.instants(o.by.principal, p.id, "", o.at.Add(-eps), until), func(t time.Time) bool {
			return c.adminOf(o.by.principal, p.id, t)
		})
		if !ok {
			s.mon.fail(o.issue, "event %d: admin.override by %s at %s, who the control log says was no admin of project %s then", o.seq, o.by, stamp(o.at), p.name)
		}
	}
}

// keysFileLines reads the keys file the daemon wrote, as sshd does, and
// returns the principal each key's line forces, by fingerprint.
func (s *soak) keysFileLines() map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(readFile(s.t, s.w.keysFile)) {
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			s.mon.fail("", "the keys file holds a line that is no key: %q", line)
			continue
		}
		if perms := s.w.fileLine(key); perms != nil {
			out[ssh.FingerprintSHA256(key)] = perms.Extensions["principal"]
		}
	}
	return out
}

// checkDials: no session was let into a project while the control log
// says it had no access to it, and none was refused one it had access
// to. A refusal with auth is not judged: it is sshd's, which has no line
// for the key, or the daemon's of a key the store no longer holds.
func (s *soak) checkDials() {
	c := s.ctl
	c.mu.Lock()
	defer c.mu.Unlock()
	s.mon.mu.Lock()
	dials := slices.Clone(s.mon.dials)
	s.mon.mu.Unlock()
	for _, d := range dials {
		if msg := c.dialFault(d, s.storeKey[d.who.principal]); msg != "" {
			s.mon.fail("", "%s", msg)
		}
	}
}

// dialFault returns what is wrong with the answer a handshake got, by
// the control log, or "". key, when set, is the fingerprint of the only
// key the principal signs in with, which the store must hold for it.
//
// It reckons on the log's order and on no time. The server answers a
// handshake from its memory of the control store, which takes each
// change as the change commits, in the log's order. So the memory that
// answered held every event up to d.known, which the server had answered
// an admin for before the handshake began, and none after d.seen, which
// is all the log held when the handshake had ended; where it stood
// between the two is not known. A handshake let in must have had access
// after one of those events, and one refused must have lacked it after
// one.
//
// The times of a handshake are the run's reckoning (vclock.now) and the
// times of the log are the control store's, and the two are not one
// clock: after a restart with the clock set back the run's is ahead by
// as long as the daemon took to stop and start, which on a loaded
// machine is minutes at the soak's speed. Judged by time, a session let
// in just before a project was archived was let in after it.
func (c *ctlReplay) dialFault(d dialRec, key string) string {
	who := d.who.principal
	lo, hi := d.known, max(d.known, min(d.seen, c.seq))
	at := []int64{lo}
	at = append(at, c.projState[d.p.id].changed(lo, hi)...)
	at = append(at, c.prinState[who].changed(lo, hi)...)
	at = append(at, c.roles[mkey{who, d.p.id}].changed(lo, hi)...)
	if key != "" {
		at = append(at, c.keyOf[key].changed(lo, hi)...)
	}
	admitted := func(seq int64) bool { return c.admitted(who, d.p.id, key, seq) }
	between := fmt.Sprintf("after event %d", lo)
	if hi > lo {
		between = fmt.Sprintf("after any of events %d to %d", lo, hi)
	}
	switch {
	case d.welcomed && !slices.ContainsFunc(at, admitted):
		return fmt.Sprintf("%s was let into project %s by a handshake from %s to %s, when the control log gives it no access %s",
			d.who, d.p.name, stamp(d.start), stamp(d.end), between)
	case !d.welcomed && (d.code == proto.CodeForbidden || d.code == proto.CodeNotFound) &&
		!slices.ContainsFunc(at, func(seq int64) bool { return !admitted(seq) }):
		return fmt.Sprintf("%s was refused project %s (%s) by a handshake from %s to %s, when the control log gives it access %s",
			d.who, d.p.name, d.code, stamp(d.start), stamp(d.end), between)
	}
	return ""
}

// checkControl: at rest, the daemon's memory of the control store, which
// every handshake and every key lookup is answered from, is what its
// tables hold and its event log explains; and `aries admin status` counts
// the same.
func (s *soak) checkControl(ctx context.Context) {
	if err := s.snapshotControl(); err != nil {
		s.t.Errorf("final control snapshot: %v", err)
		return
	}
	c := s.ctl
	c.mu.Lock()
	defer c.mu.Unlock()
	s.w.daemonMu.Lock()
	ctl := s.w.ctl
	s.w.daemonMu.Unlock()
	now := c.lastAt.Add(time.Hour)
	names := slices.Collect(maps.Keys(c.principals))
	names = append(names, s.cfg.admin, s.cfg.root, "nobody")
	for _, name := range names {
		for id, p := range c.projects {
			got := ctl.Access(name, id)
			wantRole := store.Role("")
			if c.prinState[name].at(now) != string(store.PrincipalDisabled) {
				switch {
				case c.admins[name]:
					wantRole = store.RoleAdmin
				case c.prinState[name].at(now) == string(store.PrincipalActive):
					wantRole = store.Role(c.roles[mkey{name, id}].at(now))
				}
			}
			if got.Active != (p.State == store.ProjectActive) || got.Role != wantRole ||
				got.Disabled != (c.prinState[name].at(now) == string(store.PrincipalDisabled)) {
				s.mon.fail("", "the daemon's index gives %s in project %s active %t, role %q, disabled %t; the control tables say state %s, role %q, principal %q",
					name, p.Name, got.Active, got.Role, got.Disabled, p.State, wantRole, c.prinState[name].at(now))
			}
		}
	}
	// The keys file sshd reads holds a line for exactly the keys the
	// tables give an active principal, each forcing that principal.
	inFile := s.keysFileLines()
	for fp, h := range c.keyOf {
		want := h.at(now)
		if want != "" && c.prinState[want].at(now) != string(store.PrincipalActive) {
			want = ""
		}
		if got, _, _ := ctl.AuthorizedKey(fp); got != want {
			s.mon.fail("", "the daemon's index authorizes key %s for %q; the control tables say %q", fp, got, want)
		}
		if got := inFile[fp]; got != want {
			s.mon.fail("", "the keys file lets key %s in as %q; the control tables say %q", fp, got, want)
		}
		delete(inFile, fp)
	}
	for fp, who := range inFile {
		s.mon.fail("", "the keys file lets key %s in as %q; the control log never held that key", fp, who)
	}

	root, err := mcpserver.DialRepo(ctx, s.user(s.cfg.root, s.a).repo, s.eveOptions("root-check"))
	if err != nil {
		s.t.Errorf("dial as %s: %v", s.cfg.root, err)
		return
	}
	defer func() { _ = root.Close() }()
	var st proto.StatusResult
	if err := root.Call(ctx, proto.OpStatus, proto.StatusArgs{}, &st); err != nil || st.Server == nil {
		s.t.Errorf("status at rest: %v (server part %t)", err, st.Server != nil)
		return
	}
	projects, principals := map[string]int{}, map[string]int{}
	for _, p := range c.projects {
		projects[string(p.State)]++
	}
	for _, p := range c.principals {
		principals[string(p.State)]++
	}
	if !maps.Equal(st.Server.Projects, projects) || !maps.Equal(st.Server.Principals, principals) {
		s.mon.fail("", "status counts projects %v and principals %v; the control tables hold %v and %v", st.Server.Projects, st.Server.Principals, projects, principals)
	}
	for _, ps := range st.Projects {
		p, ok := c.projects[ps.ID]
		if !ok || ps.Name != p.Name || ps.State != string(p.State) {
			s.mon.fail("", "status lists project %s (%s) as %s; the control tables hold %+v", ps.Name, ps.ID, ps.State, p)
		}
	}
}

// reportAccess adds what the conductor did, and the control log, to the
// report.
func (s *soak) reportAccess(b *bytes.Buffer) {
	c := s.ctl
	c.mu.Lock()
	fmt.Fprintf(b, "  control: %d events;", c.seq)
	for _, op := range slices.Sorted(maps.Keys(c.ops)) {
		fmt.Fprintf(b, " %s %d;", op, c.ops[op])
	}
	c.mu.Unlock()
	byReason, byServer := map[string]int{}, 0
	for _, p := range s.projs {
		p.rep.mu.Lock()
		for _, l := range p.rep.lost {
			byReason[l.reason]++
			if l.by.principal == "ariesd" {
				byServer++
			}
		}
		p.rep.mu.Unlock()
	}
	a := &s.acc
	s.mon.mu.Lock()
	dials, welcomed := len(s.mon.dials), 0
	for _, d := range s.mon.dials {
		if d.welcomed {
			welcomed++
		}
	}
	s.mon.mu.Unlock()
	fmt.Fprintf(b, "\n  access: %d members added only once their keys were shown and accepted\n", a.shown.Load())
	fmt.Fprintf(b, "  access: %d memberships removed and added, %d principals disabled and enabled, %d keys removed and added, %d removals of a key still in authorized_keys (%d warned), %d demotions and promotions, %d archives and activations (%d across a restart, a lost answer or a closed store), %d removals with the store closed; %d status reports\n",
		a.removals.Load(), a.disables.Load(), a.keyRemovals.Load(), a.fileKeys.Load(), a.warned.Load(), a.demotions.Load(), a.archives.Load(), a.archivesHard.Load(), a.quiet.Load(), a.statuses.Load())
	fmt.Fprintf(b, "  access: %d connections seen to close as access ended, %d handshakes refused and %d admitted right after a change; %d session handshakes, %d let in; claims released for lost access %v, %d of them by the server's sweep; slowest release %s after the change was acknowledged\n",
		a.closed.Load(), a.refusals.Load(), a.admissions.Load(), dials, welcomed, byReason, byServer, time.Duration(a.slowest.Load()).Round(time.Millisecond))
}
