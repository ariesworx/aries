package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/store"
)

// The control store's side of the soak: a replay of its own event log,
// compared with its tables as each project's is, which also answers what
// the projects' checks ask of it: who had access to what, and who was an
// admin of what, at a given time.

// cstamp is a value from a time on, and from an event of the log on.
type cstamp struct {
	at  time.Time
	seq int64
	v   string
}

// history is the values something took, oldest first.
type history []cstamp

// at returns the value at t: that of the last change no later than t,
// and "" before the first.
func (h history) at(t time.Time) string {
	i := sort.Search(len(h), func(i int) bool { return h[i].at.After(t) })
	if i == 0 {
		return ""
	}
	return h[i-1].v
}

// changes returns the times in (t0, t1] at which the value changed.
func (h history) changes(t0, t1 time.Time) []time.Time {
	var out []time.Time
	for _, c := range h {
		if c.at.After(t0) && !c.at.After(t1) {
			out = append(out, c.at)
		}
	}
	return out
}

// asOf returns the value once the log's events up to seq were applied:
// that of the last change no later in the log, and "" before the first.
func (h history) asOf(seq int64) string {
	i := sort.Search(len(h), func(i int) bool { return h[i].seq > seq })
	if i == 0 {
		return ""
	}
	return h[i-1].v
}

// changed returns the events after lo, up to hi, that changed the value.
func (h history) changed(lo, hi int64) []int64 {
	var out []int64
	for _, c := range h {
		if c.seq > lo && c.seq <= hi {
			out = append(out, c.seq)
		}
	}
	return out
}

// mkey names a membership.
type mkey struct{ principal, project string }

// cevent is a control event, as far as the checks ask who did what.
type cevent struct {
	at     time.Time
	by     string
	op     string
	target string
}

// ctlReplay rebuilds the control store from its event log.
type ctlReplay struct {
	fail func(issue, format string, args ...any)
	// admins are the server admins, who are the config file's.
	admins map[string]bool

	mu     sync.Mutex // guards the rest
	seq    int64
	lastAt time.Time
	ops    map[string]int
	evs    []cevent

	// The rows as the log leaves them.
	projects   map[string]store.Project
	principals map[string]store.Principal
	members    map[mkey]store.Member
	keys       map[string]store.PrincipalKey // by fingerprint

	// What each was, over time: a project's and a principal's state, a
	// membership's role ("" for none), and the principal a key belongs
	// to ("" for none).
	projState, prinState map[string]history
	roles                map[mkey]history
	keyOf                map[string]history
	// settles are the log's clock.step events.
	settles []settled
}

func newCtlReplay(fail func(string, string, ...any), admins []string) *ctlReplay {
	c := &ctlReplay{fail: fail, admins: map[string]bool{}, ops: map[string]int{},
		projects: map[string]store.Project{}, principals: map[string]store.Principal{}, members: map[mkey]store.Member{},
		keys: map[string]store.PrincipalKey{}, projState: map[string]history{}, prinState: map[string]history{},
		roles: map[mkey]history{}, keyOf: map[string]history{}}
	for _, a := range admins {
		c.admins[a] = true
	}
	return c
}

// feed applies events, which continue the log. Each must follow from the
// rows as the events before it left them: a change with no event of its
// own shows as a before state the log does not explain.
func (c *ctlReplay) feed(evs []sevent) {
	for _, e := range evs {
		if e.seq != c.seq+1 {
			c.fail("", "control event seq %d follows %d: the log has a gap", e.seq, c.seq)
		}
		if e.at.Before(c.lastAt) {
			c.fail("", "control event %d at %s is earlier than event %d at %s", e.seq, stamp(e.at), c.seq, stamp(c.lastAt))
		}
		c.seq, c.lastAt = e.seq, e.at
		c.ops[e.op]++
		c.evs = append(c.evs, cevent{at: e.at, by: e.actor.principal, op: e.op, target: e.target})
		c.apply(e)
	}
}

func (c *ctlReplay) apply(e sevent) {
	bad := func(format string, args ...any) {
		c.fail("", "control event %d (%s on %s): %s", e.seq, e.op, e.target, fmt.Sprintf(format, args...))
	}
	switch store.Op(e.op) {
	case store.OpProjectAdd, store.OpProjectActivate, store.OpProjectArchive:
		var before, after store.Project
		_ = json.Unmarshal(e.after, &after)
		cur, had := c.projects[e.target]
		switch {
		case e.op == string(store.OpProjectAdd) && had:
			bad("the project exists")
		case e.op != string(store.OpProjectAdd):
			_ = json.Unmarshal(e.before, &before)
			if !had || before.State != cur.State || before.Rev != cur.Rev {
				bad("changes it from %s at rev %d, but the log left it %s at rev %d", before.State, before.Rev, cur.State, cur.Rev)
			}
		}
		if after.ID != e.target {
			bad("its after state is project %q", after.ID)
		}
		c.projects[e.target] = after
		c.projState[e.target] = append(c.projState[e.target], cstamp{e.at, e.seq, string(after.State)})
	case store.OpPrincipalAdd, store.OpPrincipalDisable, store.OpPrincipalEnable:
		var before, after store.Principal
		_ = json.Unmarshal(e.after, &after)
		cur, had := c.principals[e.target]
		switch {
		case e.op == string(store.OpPrincipalAdd) && had:
			bad("the principal exists")
		case e.op != string(store.OpPrincipalAdd):
			_ = json.Unmarshal(e.before, &before)
			if !had || before.State != cur.State || before.Rev != cur.Rev {
				bad("changes it from %s at rev %d, but the log left it %s at rev %d", before.State, before.Rev, cur.State, cur.Rev)
			}
		}
		if after.Name != e.target {
			bad("its after state is principal %q", after.Name)
		}
		c.principals[e.target] = after
		c.prinState[e.target] = append(c.prinState[e.target], cstamp{e.at, e.seq, string(after.State)})
	case store.OpKeyAdd:
		var after store.PrincipalKey
		_ = json.Unmarshal(e.after, &after)
		if _, had := c.keys[e.target]; had {
			bad("the key is recorded already")
		}
		if after.Fingerprint != e.target {
			bad("its after state is key %q", after.Fingerprint)
		}
		c.keys[e.target] = after
		c.keyOf[e.target] = append(c.keyOf[e.target], cstamp{e.at, e.seq, after.Principal})
	case store.OpKeyRemove:
		var before store.PrincipalKey
		_ = json.Unmarshal(e.before, &before)
		if cur, had := c.keys[e.target]; !had || cur.Principal != before.Principal {
			bad("removes a key of %q, but the log left it with %q (recorded %t)", before.Principal, cur.Principal, had)
		}
		delete(c.keys, e.target)
		c.keyOf[e.target] = append(c.keyOf[e.target], cstamp{e.at, e.seq, ""})
	case store.OpMemberSet, store.OpMemberRemove:
		var before, after store.Member
		hasBefore := len(e.before) > 0
		if hasBefore {
			_ = json.Unmarshal(e.before, &before)
		}
		row := before
		if e.op == string(store.OpMemberSet) {
			_ = json.Unmarshal(e.after, &after)
			row = after
		}
		k := mkey{row.Principal, row.Project}
		if e.target != row.Project+"/"+row.Principal {
			bad("its state is the membership of %s in %s", row.Principal, row.Project)
		}
		cur, had := c.members[k]
		switch {
		case hasBefore != had:
			bad("says the membership existed: %t, and the log says %t", hasBefore, had)
		case had && (before.Role != cur.Role || before.Rev != cur.Rev):
			bad("changes it from %s at rev %d, but the log left it %s at rev %d", before.Role, before.Rev, cur.Role, cur.Rev)
		}
		if e.op == string(store.OpMemberSet) {
			c.members[k] = after
			c.roles[k] = append(c.roles[k], cstamp{e.at, e.seq, string(after.Role)})
		} else {
			delete(c.members, k)
			c.roles[k] = append(c.roles[k], cstamp{e.at, e.seq, ""})
		}
	case store.OpClockStep:
		// The daemon settled a forward step of its clock: no row changes.
		c.settles = append(c.settles, settledOf(e))
	default:
		bad("an op the soak does not expect")
	}
}

// ctlTables are the control store's rows, from one snapshot.
type ctlTables struct {
	projects   map[string]store.Project
	principals map[string]store.Principal
	members    map[mkey]store.Member
	keys       map[string]store.PrincipalKey
}

func readCtlTables(ctx context.Context, q querier) (ctlTables, error) {
	tb := ctlTables{projects: map[string]store.Project{}, principals: map[string]store.Principal{}, members: map[mkey]store.Member{},
		keys: map[string]store.PrincipalKey{}}
	reads := []struct {
		query string
		row   func(*sql.Rows) error
	}{
		{`SELECT id, name, prefix, account, db_name, state, rev FROM projects`, func(rows *sql.Rows) error {
			var p store.Project
			err := rows.Scan(&p.ID, &p.Name, &p.Prefix, &p.Account, &p.DBName, &p.State, &p.Rev)
			tb.projects[p.ID] = p
			return err
		}},
		{`SELECT name, state, rev FROM principals`, func(rows *sql.Rows) error {
			var p store.Principal
			err := rows.Scan(&p.Name, &p.State, &p.Rev)
			tb.principals[p.Name] = p
			return err
		}},
		{`SELECT principal, project, role, rev FROM members`, func(rows *sql.Rows) error {
			var m store.Member
			err := rows.Scan(&m.Principal, &m.Project, &m.Role, &m.Rev)
			tb.members[mkey{m.Principal, m.Project}] = m
			return err
		}},
		{"SELECT fingerprint, principal, `key` FROM principal_keys", func(rows *sql.Rows) error {
			var k store.PrincipalKey
			err := rows.Scan(&k.Fingerprint, &k.Principal, &k.Key)
			tb.keys[k.Fingerprint] = k
			return err
		}},
	}
	for _, rd := range reads {
		if err := scan(ctx, q, rd.query, nil, rd.row); err != nil {
			return ctlTables{}, err
		}
	}
	return tb, nil
}

// compare checks the replayed rows against the tables: every project,
// principal, membership and key in the tables is one the log explains,
// as it is, and the reverse.
func (c *ctlReplay) compare(tb ctlTables) {
	for id, want := range c.projects {
		got, ok := tb.projects[id]
		w := store.Project{ID: want.ID, Name: want.Name, Prefix: want.Prefix, Account: want.Account, DBName: want.DBName, State: want.State, Rev: want.Rev}
		if !ok || got != w {
			c.fail("", "project %s: the control table has %+v (there: %t), its event log rebuilds %+v", id, got, ok, w)
		}
	}
	for id := range tb.projects {
		if _, ok := c.projects[id]; !ok {
			c.fail("", "project %s is in the control table, but no event added it", id)
		}
	}
	for name, want := range c.principals {
		got, ok := tb.principals[name]
		if !ok || got.State != want.State || got.Rev != want.Rev {
			c.fail("", "principal %s: the control table has it %s at rev %d (there: %t), its event log %s at rev %d", name, got.State, got.Rev, ok, want.State, want.Rev)
		}
	}
	for name := range tb.principals {
		if _, ok := c.principals[name]; !ok {
			c.fail("", "principal %s is in the control table, but no event added it", name)
		}
	}
	for k, want := range c.members {
		got, ok := tb.members[k]
		if !ok || got.Role != want.Role || got.Rev != want.Rev {
			c.fail("", "membership of %s in %s: the control table has %s at rev %d (there: %t), its event log %s at rev %d", k.principal, k.project,
				got.Role, got.Rev, ok, want.Role, want.Rev)
		}
	}
	for k := range tb.members {
		if _, ok := c.members[k]; !ok {
			c.fail("", "membership of %s in %s is in the control table, but the event log holds none", k.principal, k.project)
		}
	}
	for fp, want := range c.keys {
		got, ok := tb.keys[fp]
		if !ok || got.Principal != want.Principal || got.Key != want.Key {
			c.fail("", "key %s: the control table has it for %q (there: %t), its event log for %q", fp, got.Principal, ok, want.Principal)
		}
	}
	for fp := range tb.keys {
		if _, ok := c.keys[fp]; !ok {
			c.fail("", "key %s is in the control table, but the event log holds none", fp)
		}
	}
}

// snapshotControl reads the control store's events since the last
// snapshot and its tables in one read-only transaction, replays the
// events and compares.
func (s *soak) snapshotControl() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tx, err := s.cdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	c := s.ctl
	c.mu.Lock()
	defer c.mu.Unlock()
	evs, err := readEvents(ctx, tx, c.seq)
	if err != nil {
		return err
	}
	c.feed(evs)
	tb, err := readCtlTables(ctx, tx)
	if err != nil {
		return err
	}
	c.compare(tb)
	return nil
}

// Why a claim ended with its holder's access, as the claim.release says.
const (
	lostMembership = "membership removed"
	lostDisabled   = "principal disabled"
	lostArchived   = "project archived"
)

// lacks reports whether principal was without access to project at t
// for reason, by the log.
func (c *ctlReplay) lacks(principal, project, reason string, t time.Time) bool {
	switch reason {
	case lostArchived:
		return c.projState[project].at(t) != string(store.ProjectActive)
	case lostDisabled:
		return c.prinState[principal].at(t) == string(store.PrincipalDisabled)
	case lostMembership:
		return !c.admins[principal] && c.roles[mkey{principal, project}].at(t) == ""
	}
	return false
}

// admitted reports whether a handshake of principal for project would be
// let in once the log's events up to seq were applied: the project is
// active, the principal is not disabled, and it is a server admin or an
// active member. key, when set, is the fingerprint of the only key the
// principal signs in with, which the store must then hold for it.
func (c *ctlReplay) admitted(principal, project, key string, seq int64) bool {
	switch {
	case c.projState[project].asOf(seq) != string(store.ProjectActive):
		return false
	case c.prinState[principal].asOf(seq) == string(store.PrincipalDisabled):
		return false
	case key != "" && c.keyOf[key].asOf(seq) != principal:
		return false
	case c.admins[principal]:
		return true
	}
	return c.prinState[principal].asOf(seq) == string(store.PrincipalActive) && c.roles[mkey{principal, project}].asOf(seq) != ""
}

// adminOf reports whether principal was an admin in project at t: a
// server admin who is not disabled, or an active principal whose role
// there is admin.
func (c *ctlReplay) adminOf(principal, project string, t time.Time) bool {
	state := c.prinState[principal].at(t)
	if c.admins[principal] {
		return state != string(store.PrincipalDisabled)
	}
	return state == string(store.PrincipalActive) && c.roles[mkey{principal, project}].at(t) == string(store.RoleAdmin)
}

// instants returns t0 and every time in (t0, t1] at which something
// about principal, project or key changed: the times at which a
// question about them can change its answer.
func (c *ctlReplay) instants(principal, project, key string, t0, t1 time.Time) []time.Time {
	out := []time.Time{t0}
	out = append(out, c.projState[project].changes(t0, t1)...)
	out = append(out, c.prinState[principal].changes(t0, t1)...)
	out = append(out, c.roles[mkey{principal, project}].changes(t0, t1)...)
	if key != "" {
		out = append(out, c.keyOf[key].changes(t0, t1)...)
	}
	return out
}

// lastBy returns who made the latest change of op to target no later
// than t, or "" when there is none.
func (c *ctlReplay) lastBy(op store.Op, target string, t time.Time) string {
	i := slices.IndexFunc(c.evs, func(e cevent) bool { return e.at.After(t) })
	if i < 0 {
		i = len(c.evs)
	}
	for _, e := range slices.Backward(c.evs[:i]) {
		if e.op == string(op) && e.target == target {
			return e.by
		}
	}
	return ""
}
