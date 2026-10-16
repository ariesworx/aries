package e2e

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// The checks of stage 3a must catch what they claim to, as the others
// must (soak_replay_test.go): these feed them control logs, project logs
// and handshakes that break one rule each, and some that break none,
// without a store.

const (
	projA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	projB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

var (
	root       = sessKey{"root", "s1"}
	clockActor = sessKey{"ariesd", "clock"}
)

// The states of control rows, as the control store's events carry them.
func projectState(id, state string, rev int) string {
	return fmt.Sprintf(`{"id": %q, "name": "p-%s", "prefix": "p", "account": "internal", "schema": "db_%s", "state": %q, "rev": %d}`, id, id[:1], id[:1], state, rev)
}

func principalState(name, state string, rev int) string {
	return fmt.Sprintf(`{"name": %q, "state": %q, "rev": %d}`, name, state, rev)
}

func memberState(principal, project, role string, rev int) string {
	return fmt.Sprintf(`{"principal": %q, "project": %q, "role": %q, "rev": %d}`, principal, project, role, rev)
}

func keyState(fingerprint, principal string) string {
	return fmt.Sprintf(`{"fingerprint": %q, "principal": %q, "key": "ssh-ed25519 AAAA"}`, fingerprint, principal)
}

// A control log builder: each step is a minute after the last.
func (b *logBuilder) project(id string) *logBuilder {
	return b.add(time.Minute, root, "project.add", id, "", projectState(id, "creating", 1)).
		add(time.Minute, root, "project.activate", id, projectState(id, "creating", 1), projectState(id, "active", 2))
}

func (b *logBuilder) principal(name string) *logBuilder {
	return b.add(time.Minute, root, "principal.add", name, "", principalState(name, "active", 1))
}

func (b *logBuilder) member(who sessKey, principal, project, role string) *logBuilder {
	return b.add(time.Minute, who, "member.set", project+"/"+principal, "", memberState(principal, project, role, 1))
}

func (b *logBuilder) unmember(who sessKey, principal, project, role string, rev int) *logBuilder {
	return b.add(time.Minute, who, "member.remove", project+"/"+principal, memberState(principal, project, role, rev), "")
}

// world is the control log most cases start from: two projects, alice a
// member of both, carol the admin of the first.
func ctlWorld() *logBuilder {
	return newLog().project(projA).project(projB).principal("alice").principal("carol").
		member(root, "alice", projA, "member").member(root, "alice", projB, "member").member(root, "carol", projA, "admin")
}

func TestCtlReplayCatches(t *testing.T) {
	t.Parallel()
	const fp = "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"
	tests := []struct {
		name string
		log  func(b *logBuilder) *logBuilder
		want string // a substring of the one violation; empty: none
	}{
		{name: "a clean story", log: func(b *logBuilder) *logBuilder {
			return b.add(time.Minute, root, "key.add", fp, "", keyState(fp, "alice")).
				add(time.Minute, root, "member.set", projA+"/alice", memberState("alice", projA, "member", 1), memberState("alice", projA, "admin", 2)).
				unmember(root, "alice", projA, "admin", 2).
				add(time.Minute, root, "key.remove", fp, keyState(fp, "alice"), "").
				add(time.Minute, root, "principal.disable", "alice", principalState("alice", "active", 1), principalState("alice", "disabled", 2)).
				add(time.Minute, root, "principal.enable", "alice", principalState("alice", "disabled", 2), principalState("alice", "active", 3)).
				add(time.Minute, root, "project.archive", projB, projectState(projB, "active", 2), projectState(projB, "archived", 3)).
				add(time.Minute, root, "project.activate", projB, projectState(projB, "archived", 3), projectState(projB, "active", 4))
		}},
		{name: "a gap in seq", log: func(b *logBuilder) *logBuilder {
			b.seq++
			return b.principal("bob")
		}, want: "the log has a gap"},
		{name: "time running backward", log: func(b *logBuilder) *logBuilder {
			return b.add(-time.Hour, root, "principal.add", "bob", "", principalState("bob", "active", 1))
		}, want: "is earlier than event"},
		{name: "a role that changed with no event", log: func(b *logBuilder) *logBuilder {
			return b.add(time.Minute, root, "member.set", projA+"/alice", memberState("alice", projA, "admin", 2), memberState("alice", projA, "member", 3))
		}, want: "changes it from admin at rev 2, but the log left it member at rev 1"},
		{name: "a membership removed that the log does not hold", log: func(b *logBuilder) *logBuilder {
			return b.unmember(root, "carol", projB, "member", 1)
		}, want: "says the membership existed: true, and the log says false"},
		{name: "a membership added that the log holds", log: func(b *logBuilder) *logBuilder {
			return b.member(root, "alice", projA, "member")
		}, want: "says the membership existed: false, and the log says true"},
		{name: "a principal disabled that the log left disabled", log: func(b *logBuilder) *logBuilder {
			return b.add(time.Minute, root, "principal.disable", "alice", principalState("alice", "active", 1), principalState("alice", "disabled", 2)).
				add(time.Minute, root, "principal.disable", "alice", principalState("alice", "active", 1), principalState("alice", "disabled", 2))
		}, want: "changes it from active at rev 1, but the log left it disabled at rev 2"},
		{name: "a principal added twice", log: func(b *logBuilder) *logBuilder { return b.principal("alice") }, want: "the principal exists"},
		{name: "a project archived that was never added", log: func(b *logBuilder) *logBuilder {
			const c = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
			return b.add(time.Minute, root, "project.archive", c, projectState(c, "active", 2), projectState(c, "archived", 3))
		}, want: "changes it from active at rev 2, but the log left it  at rev 0"},
		{name: "a key recorded twice", log: func(b *logBuilder) *logBuilder {
			return b.add(time.Minute, root, "key.add", fp, "", keyState(fp, "alice")).add(time.Minute, root, "key.add", fp, "", keyState(fp, "carol"))
		}, want: "the key is recorded already"},
		{name: "a key removed from another principal", log: func(b *logBuilder) *logBuilder {
			return b.add(time.Minute, root, "key.add", fp, "", keyState(fp, "alice")).add(time.Minute, root, "key.remove", fp, keyState(fp, "carol"), "")
		}, want: `removes a key of "carol", but the log left it with "alice"`},
		{name: "an op the soak does not expect", log: func(b *logBuilder) *logBuilder {
			return b.add(time.Minute, root, "principal.rename", "alice", "", "")
		}, want: "an op the soak does not expect"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			c := newCtlReplay(func(_, format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }, []string{"root"})
			c.feed(tc.log(ctlWorld()).evs)
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("replay found %q, want no violation", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("replay found %q, want one violation containing %q", got, tc.want)
			}
		})
	}
}

func TestCtlReplayCompare(t *testing.T) {
	t.Parallel()
	const fp = "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"
	tests := []struct {
		name   string
		change func(*ctlTables)
		want   string
	}{
		{name: "the same", change: func(*ctlTables) {}},
		{name: "a membership no event made", change: func(tb *ctlTables) {
			tb.members[mkey{"carol", projB}] = store.Member{Principal: "carol", Project: projB, Role: store.RoleAdmin, Rev: 1}
		}, want: "membership of carol in " + projB + " is in the control table, but the event log holds none"},
		{name: "a role no event gave", change: func(tb *ctlTables) {
			tb.members[mkey{"alice", projA}] = store.Member{Principal: "alice", Project: projA, Role: store.RoleAdmin, Rev: 1}
		}, want: "the control table has admin at rev 1 (there: true), its event log member at rev 1"},
		{name: "a membership the table lost", change: func(tb *ctlTables) { delete(tb.members, mkey{"alice", projA}) },
			want: "(there: false), its event log member at rev 1"},
		{name: "a principal disabled with no event", change: func(tb *ctlTables) {
			tb.principals["alice"] = store.Principal{Name: "alice", State: store.PrincipalDisabled, Rev: 2}
		}, want: "principal alice: the control table has it disabled at rev 2"},
		{name: "a principal no event added", change: func(tb *ctlTables) { tb.principals["mallory"] = store.Principal{Name: "mallory"} },
			want: "principal mallory is in the control table, but no event added it"},
		{name: "a project archived with no event", change: func(tb *ctlTables) {
			p := tb.projects[projB]
			p.State = store.ProjectArchived
			tb.projects[projB] = p
		}, want: "project " + projB + ": the control table has"},
		{name: "a key no event added", change: func(tb *ctlTables) {
			tb.keys["SHA256:other"] = store.PrincipalKey{Fingerprint: "SHA256:other", Principal: "carol"}
		},
			want: "key SHA256:other is in the control table, but the event log holds none"},
		{name: "a key moved to another principal", change: func(tb *ctlTables) {
			k := tb.keys[fp]
			k.Principal = "carol"
			tb.keys[fp] = k
		}, want: `the control table has it for "carol" (there: true), its event log for "alice"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			c := newCtlReplay(func(_, format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }, []string{"root"})
			c.feed(ctlWorld().add(time.Minute, root, "key.add", fp, "", keyState(fp, "alice")).evs)
			// The tables as the log leaves them, then changed.
			tb := ctlTables{projects: map[string]store.Project{}, principals: map[string]store.Principal{}, members: map[mkey]store.Member{},
				keys: map[string]store.PrincipalKey{}}
			for k, v := range c.projects {
				tb.projects[k] = store.Project{ID: v.ID, Name: v.Name, Prefix: v.Prefix, Account: v.Account, Schema: v.Schema, State: v.State, Rev: v.Rev}
			}
			for k, v := range c.principals {
				tb.principals[k] = v
			}
			for k, v := range c.members {
				tb.members[k] = v
			}
			for k, v := range c.keys {
				tb.keys[k] = v
			}
			tc.change(&tb)
			c.compare(tb)
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("compare found %q, want nothing", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("compare found %q, want one violation containing %q", got, tc.want)
			}
		})
	}
}

// The replay of a project's log knows a claim that ended for lost
// access: the issue returns to open, and the admin's write under the
// holder's claim is no breach of the guard.
func TestReplayLostAccess(t *testing.T) {
	t.Parallel()
	alice, carol := sessKey{"alice", "s1"}, sessKey{"carol", "s1"}
	held := func(b *logBuilder) *logBuilder {
		return b.create(alice, "ar-1").take(alice, "ar-1", 1, 15*time.Minute, "").
			add(0, alice, "issue.update", "ar-1", `{"status": "open"}`, `{"status": "in_progress", "assignee": "alice"}`)
	}
	tests := []struct {
		name            string
		log             func(b *logBuilder) *logBuilder
		want            string
		lost, overrides int
	}{
		{name: "released, and the issue open again", log: func(b *logBuilder) *logBuilder {
			return held(b).add(time.Minute, root, "claim.release", "ar-1", b.claimOf(alice, 1, 17*time.Minute), `{"reason": "membership removed"}`).
				add(0, root, "issue.update", "ar-1", `{"status": "in_progress", "assignee": "alice"}`, `{"status": "open", "assignee": null}`)
		}, lost: 1},
		{name: "released, and the issue left in progress", log: func(b *logBuilder) *logBuilder {
			return held(b).add(time.Minute, root, "claim.release", "ar-1", b.claimOf(alice, 1, 17*time.Minute), `{"reason": "principal disabled"}`)
		}, want: "left the issue in_progress, assigned to \"alice\"; want it open and unassigned", lost: 1},
		{name: "released, and an issue that had moved on is changed", log: func(b *logBuilder) *logBuilder {
			b.create(alice, "ar-1").take(alice, "ar-1", 1, 15*time.Minute, "")
			return b.add(time.Minute, root, "claim.release", "ar-1", b.claimOf(alice, 1, 17*time.Minute), `{"reason": "project archived"}`).
				add(0, root, "issue.update", "ar-1", `{"status": "open"}`, `{"status": "blocked"}`)
		}, want: "though it was not in progress under the holder", lost: 1},
		{name: "a release with no reason is not one for lost access", log: func(b *logBuilder) *logBuilder {
			return held(b).add(time.Minute, alice, "claim.release", "ar-1", b.claimOf(alice, 1, 17*time.Minute), "").
				add(0, alice, "issue.update", "ar-1", `{"status": "in_progress", "assignee": "alice"}`, `{"status": "open", "assignee": null}`)
		}},
		{name: "a project admin's override is kept for the control log", log: func(b *logBuilder) *logBuilder {
			return held(b).add(time.Minute, carol, "admin.override", "ar-1", "", `{"holder": {"principal": "alice", "session": "s1"}, "epoch": 1}`).
				add(0, carol, "issue.update", "ar-1", `{"title": "a"}`, `{"title": "retitled carol-1"}`)
		}, overrides: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			r := newReplay(func(_, format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }, []string{"root"})
			r.roles = true
			r.feed(tc.log(newLog()).evs)
			r.flush()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("replay found %q, want no violation", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("replay found %q, want one violation containing %q", got, tc.want)
			}
			if len(r.lost) != tc.lost || len(r.overrides) != tc.overrides || len(r.guards) != 0 {
				t.Errorf("replay kept %d claims lost, %d overrides and %d guard checks; want %d, %d and 0", len(r.lost), len(r.overrides), len(r.guards), tc.lost, tc.overrides)
			}
		})
	}
}

// accessSoak is a soak with only what the access checks read: a control
// log, and a store clock at the speed of real time, so eps is a second.
func accessSoak(log *logBuilder) (*soak, *sproj) {
	s := &soak{mon: newMonitor(), cfg: soakConfig{speed: 1}, storeKey: map[string]string{}}
	s.ctl = newCtlReplay(s.mon.fail, []string{"root"})
	s.ctl.feed(log.evs)
	return s, &sproj{id: projA, name: "first", rep: newReplay(s.mon.fail, []string{"root"})}
}

func TestCheckLost(t *testing.T) {
	t.Parallel()
	alice, carol, sweep := sessKey{"alice", "s1"}, sessKey{"carol", "s1"}, sessKey{"ariesd", "access"}
	// The control log: at 8m (the world's last event is at 9m; see below)
	// each case removes, disables or archives, an hour in.
	t0 := newLog().t0
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	removed := func() *logBuilder { // alice leaves the first project at 1h, by carol
		b := ctlWorld()
		b.at = time.Hour - time.Minute
		return b.unmember(carol, "alice", projA, "member", 1)
	}
	// The clock jumps forward twelve minutes at 1h19m04s, where the
	// control store settles it at once, so the control log has no time
	// between then and 1h31m04s; then does what each case has it do.
	jumped := func(then func(*logBuilder)) *logBuilder {
		b := ctlWorld()
		b.at = time.Hour + 19*time.Minute + 4*time.Second
		b.add(0, clockActor, "clock.step", "clock", `{"at": `+b.ts(b.at)+`}`,
			`{"at": `+b.ts(b.at+12*time.Minute)+`, "step_us": 720000000, "extended_us": 720000000, "claims": 0}`)
		b.at += 12 * time.Minute
		then(b)
		return b
	}
	// carol removes alice 15 seconds after the jump.
	removedAfter := func() *logBuilder {
		return jumped(func(b *logBuilder) {
			b.add(15*time.Second, carol, "member.remove", projA+"/alice", memberState("alice", projA, "member", 1), "")
		})
	}
	// before is a time of the project's log d after the clock jumped, on
	// the time before the jump, and after one on the time after it.
	before := func(d time.Duration) time.Time { return at(time.Hour + 19*time.Minute + 4*time.Second + d) }
	after := func(d time.Duration) time.Time { return before(12*time.Minute + d) }
	tests := []struct {
		name string
		log  *logBuilder
		lost []lostClaim
		over []override
		want string
	}{
		// Seed 264949260793903814 on a loaded machine: the sweep's write
		// was waiting for the project's writer when the clock jumped, and
		// the store dated its release on the time before, 25 seconds past
		// the jump, and the removal it answers to on the time after. The
		// check allowed that while the store did it. A write now settles a
		// jump it finds once it holds the writer, so the release is dated
		// after the removal, and one dated before it has no cause.
		{name: "released after a jump, for a removal dated after the jump", log: removedAfter(),
			lost: []lostClaim{{seq: 51, issue: "ar-1", holder: alice, reason: lostMembership, by: sweep, took: before(-3 * time.Minute), at: after(16 * time.Second)}}},
		{name: "released after a jump by the admin who removed the member after it", log: removedAfter(),
			lost: []lostClaim{{seq: 51, issue: "ar-1", holder: alice, reason: lostMembership, by: carol, took: before(-3 * time.Minute), at: after(15*time.Second + time.Millisecond)}}},
		{name: "released on the time before a jump, for a removal dated after it", log: removedAfter(),
			lost: []lostClaim{{seq: 48, issue: "ar-1", holder: alice, reason: lostMembership, by: sweep, took: before(-3 * time.Minute), at: before(25 * time.Second)}},
			want: "shows no such loss of access"},
		{name: "released after a jump, for a membership that was never removed", log: jumped(func(*logBuilder) {}),
			lost: []lostClaim{{seq: 51, issue: "ar-1", holder: alice, reason: lostMembership, by: sweep, took: before(-3 * time.Minute), at: after(16 * time.Second)}},
			want: "shows no such loss of access"},
		{name: "an override after a jump by an admin promoted after it", log: jumped(func(b *logBuilder) {
			b.add(10*time.Second, root, "member.set", projA+"/alice", memberState("alice", projA, "member", 1), memberState("alice", projA, "admin", 2))
		}), over: []override{{seq: 51, issue: "ar-1", by: alice, at: after(20 * time.Second)}}},
		{name: "an override on the time before a jump by an admin promoted after it", log: jumped(func(b *logBuilder) {
			b.add(10*time.Second, root, "member.set", projA+"/alice", memberState("alice", projA, "member", 1), memberState("alice", projA, "admin", 2))
		}), over: []override{{seq: 48, issue: "ar-1", by: alice, at: before(20 * time.Second)}}, want: "admin.override by alice/s1"},
		{name: "released by the admin who removed the member", log: removed(),
			lost: []lostClaim{{issue: "ar-1", holder: alice, reason: lostMembership, by: carol, took: at(30 * time.Minute), at: at(time.Hour + time.Millisecond)}}},
		{name: "released by the server's sweep", log: removed(),
			lost: []lostClaim{{issue: "ar-1", holder: alice, reason: lostMembership, by: sweep, took: at(30 * time.Minute), at: at(2 * time.Hour)}}},
		{name: "released in the transaction before the removal's own time", log: removed(),
			lost: []lostClaim{{issue: "ar-1", holder: alice, reason: lostMembership, by: sweep, took: at(30 * time.Minute), at: at(time.Hour - 500*time.Millisecond)}}},
		{name: "released for a membership that was never removed", log: ctlWorld(),
			lost: []lostClaim{{issue: "ar-1", holder: alice, reason: lostMembership, by: sweep, took: at(30 * time.Minute), at: at(time.Hour)}},
			want: `for "membership removed", but the control log shows no such loss of access`},
		{name: "released long before the removal", log: removed(),
			lost: []lostClaim{{issue: "ar-1", holder: alice, reason: lostMembership, by: sweep, took: at(30 * time.Minute), at: at(45 * time.Minute)}},
			want: "shows no such loss of access"},
		{name: "released for a reason that is not the one", log: removed(),
			lost: []lostClaim{{issue: "ar-1", holder: alice, reason: lostDisabled, by: sweep, took: at(30 * time.Minute), at: at(2 * time.Hour)}},
			want: `for "principal disabled", but the control log shows no such loss`},
		{name: "released by someone who did not end the access", log: removed(),
			lost: []lostClaim{{issue: "ar-1", holder: alice, reason: lostMembership, by: root, took: at(30 * time.Minute), at: at(time.Hour + time.Millisecond)}},
			want: `root/s1 released alice/s1's claim on ar-1 for "membership removed", but the control log says the access was last ended by "carol"`},
		{name: "a server admin's claim is never lost with a membership", log: ctlWorld(),
			lost: []lostClaim{{issue: "ar-1", holder: root, reason: lostMembership, by: sweep, took: at(30 * time.Minute), at: at(time.Hour)}},
			want: "shows no such loss of access"},
		{name: "an override by the project's admin", log: ctlWorld(), over: []override{{issue: "ar-1", by: carol, at: at(time.Hour)}}},
		{name: "an override by a member", log: ctlWorld(), over: []override{{issue: "ar-1", by: alice, at: at(time.Hour)}},
			want: "admin.override by alice/s1"},
		{name: "an override by the admin of another project", log: ctlWorld().member(root, "erin", projB, "admin"),
			over: []override{{issue: "ar-1", by: sessKey{"erin", "s1"}, at: at(time.Hour)}}, want: "admin.override by erin/s1"},
		{name: "an override by an admin who was demoted an hour before", log: ctlWorld().
			add(time.Minute, root, "member.set", projA+"/carol", memberState("carol", projA, "admin", 1), memberState("carol", projA, "member", 2)),
			over: []override{{issue: "ar-1", by: carol, at: at(2 * time.Hour)}}, want: "admin.override by carol/s1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, p := accessSoak(tc.log)
			p.rep.lost, p.rep.overrides = tc.lost, tc.over
			s.checkLost(p)
			got := s.mon.violations()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("checkLost found %v, want nothing", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0].msg, tc.want)):
				t.Errorf("checkLost found %v, want one violation containing %q", got, tc.want)
			}
		})
	}
}

// A handshake is judged by the control log's own order, never by its
// times: known is the last event the server had answered for when the
// handshake began, and seen the last the log held when it ended.
func TestCheckDials(t *testing.T) {
	t.Parallel()
	const fp = "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"
	alice := sessKey{"alice", "s1"}
	t0 := newLog().t0
	// The world is events 1 to 9. alice leaves the first project with
	// event 10, at 1h, and comes back with 11, at 2h; her key is recorded
	// with 12, at 3h, and removed with 13, at 4h.
	const world, left, back, keyed, unkeyed = 9, 10, 11, 12, 13
	log := func() *logBuilder {
		b := ctlWorld()
		b.at = time.Hour - time.Minute
		b.unmember(root, "alice", projA, "member", 1)
		b.at = 2*time.Hour - time.Minute
		b.member(root, "alice", projA, "member")
		b.at = 3*time.Hour - time.Minute
		b.add(time.Minute, root, "key.add", fp, "", keyState(fp, "alice"))
		b.at = 4*time.Hour - time.Minute
		return b.add(time.Minute, root, "key.remove", fp, keyState(fp, "alice"), "")
	}
	// at is when the run's own clock says the handshake was made, which
	// the check prints and must not judge by.
	dial := func(known, seen int64, at time.Duration, code proto.Code) dialRec {
		return dialRec{who: alice, start: t0.Add(at), end: t0.Add(at + time.Second), known: known, seen: seen, welcomed: code == "", code: code}
	}
	tests := []struct {
		name     string
		d        dialRec
		storeKey bool // alice signs in with a key only the store holds
		want     string
	}{
		{name: "let in as a member", d: dial(world, world, 30*time.Minute, "")},
		{name: "refused while not a member", d: dial(left, left, 90*time.Minute, proto.CodeForbidden)},
		{name: "let in while not a member", d: dial(left, left, 90*time.Minute, ""), want: "was let into project first"},
		{name: "refused as a member", d: dial(world, world, 30*time.Minute, proto.CodeForbidden), want: "was refused project first (forbidden)"},
		{name: "told the project is not served, as a member", d: dial(back, back, 150*time.Minute, proto.CodeNotFound), want: "was refused project first (not_found)"},
		// The server may have decided before the removal or after it.
		{name: "let in as the removal was made", d: dial(world, left, time.Hour, "")},
		{name: "refused as the removal was made", d: dial(world, left, time.Hour, proto.CodeForbidden)},
		{name: "refused as the membership came back", d: dial(left, back, 2*time.Hour, proto.CodeForbidden)},
		{name: "let in as the membership came back", d: dial(left, back, 2*time.Hour, "")},
		// Once the server has answered for a change, no time excuses it.
		{name: "let in half a second after the removal was answered for", d: dial(left, left, time.Hour+500*time.Millisecond, ""),
			want: "was let into project first"},
		{name: "refused half a second after the return was answered for", d: dial(back, back, 2*time.Hour+500*time.Millisecond, proto.CodeForbidden),
			want: "was refused project first (forbidden)"},
		// The run's clock is ahead of the control store's after a restart
		// with the clock set back, by as long as the daemon was down: these
		// handshakes were made before the change, and are dated after it.
		{name: "let in before the removal, on a clock three minutes ahead of the log's", d: dial(world, world, time.Hour+3*time.Minute, "")},
		{name: "refused before the return, on a clock three minutes ahead of the log's", d: dial(left, left, 2*time.Hour+3*time.Minute, proto.CodeForbidden)},
		// And a clock that agrees with the server hides nothing.
		{name: "let in after the removal, on a clock that says before it", d: dial(left, left, 30*time.Minute, ""), want: "was let into project first"},
		{name: "refused after the return, on a clock that says before it", d: dial(back, back, 90*time.Minute, proto.CodeForbidden),
			want: "was refused project first (forbidden)"},
		{name: "the log's latest event was not read when it ended", d: dial(world, unknownSeq, 30*time.Minute, proto.CodeForbidden)},
		{name: "sshd's refusal is not judged", d: dial(world, world, 30*time.Minute, proto.CodeAuth)},
		{name: "a failure that is no refusal is not judged", d: dial(world, world, 30*time.Minute, proto.CodeUnavailable)},
		{name: "let in by a key the store holds", d: dial(keyed, keyed, 210*time.Minute, ""), storeKey: true},
		{name: "let in before the key was recorded", d: dial(back, back, 150*time.Minute, ""), storeKey: true, want: "was let into project first"},
		{name: "let in after the key was removed", d: dial(unkeyed, unkeyed, 5*time.Hour, ""), storeKey: true, want: "was let into project first"},
		{name: "let in by a line of authorized_keys after the store's key was removed", d: dial(unkeyed, unkeyed, 5*time.Hour, "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, p := accessSoak(log())
			if tc.storeKey {
				s.storeKey["alice"] = fp
			}
			tc.d.p = p
			s.mon.dialed(tc.d)
			s.checkDials()
			got := s.mon.violations()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("checkDials(%+v) found %v, want nothing", tc.d, got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0].msg, tc.want)):
				t.Errorf("checkDials(%+v) found %v, want one violation containing %q", tc.d, got, tc.want)
			}
		})
	}
}

// Nothing of one project shows through a connection to another: an
// answer or a push that names another project's issue fails the run.
func TestCheckForeign(t *testing.T) {
	t.Parallel()
	p := &sproj{name: "first", prefix: "ar", foreign: regexp.MustCompile(`\b(sb|sq)-[a-z2-7]{8}\b`)}
	alice := sessKey{"alice", "s1"}
	const mine, theirs = "ar-abcdefgh", "sb-abcdefgh"
	answers := []struct {
		name string
		cl   call
		want string
	}{
		{name: "its own issue", cl: call{out: acked, op: proto.OpShow, res: proto.ShowResult{Issue: proto.Issue{ID: mine, Title: "task alice-1"}}}},
		{name: "another project's issue", cl: call{out: acked, op: proto.OpShow, res: proto.ShowResult{Issue: proto.Issue{ID: theirs}}},
			want: "was answered show naming sb-abcdefgh, an issue of another project"},
		{name: "another project's issue in a list", cl: call{out: acked, op: proto.OpReady, res: proto.ListResult{Issues: []proto.Summary{{ID: mine}, {ID: "sq-abcdefgh"}}}},
			want: "naming sq-abcdefgh"},
		{name: "another project's claim", cl: call{out: acked, op: proto.OpClaims, res: proto.ClaimsResult{Claims: []proto.Claim{{Issue: theirs, By: "bob"}}}},
			want: "was answered claims naming sb-abcdefgh"},
		{name: "another project's issue in text", cl: call{out: acked, op: proto.OpInbox, res: proto.InboxResult{Items: []proto.InboxItem{{Issue: mine, Body: "see " + theirs + " too"}}}},
			want: "naming sb-abcdefgh"},
		{name: "a refusal is no answer", cl: call{out: refused, op: proto.OpShow, res: proto.ShowResult{Issue: proto.Issue{ID: theirs}}}},
		{name: "text that only looks alike", cl: call{out: acked, op: proto.OpShow, res: proto.ShowResult{Issue: proto.Issue{ID: mine, Title: "task bob-1g0-3, sb-short"}}}},
	}
	for _, tc := range answers {
		t.Run(tc.name, func(t *testing.T) {
			s := &soak{mon: newMonitor()}
			tc.cl.who, tc.cl.p = alice, p
			s.checkForeign(tc.cl)
			got := s.mon.violations()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("checkForeign found %v, want nothing", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0].msg, tc.want)):
				t.Errorf("checkForeign found %v, want one violation containing %q", got, tc.want)
			}
		})
	}

	pushes := []struct {
		name string
		push proto.Push
		want string
	}{
		{name: "its own inbox item", push: proto.Push{Item: &proto.InboxItem{ID: 1, Issue: mine, Body: "mention"}}},
		{name: "another project's inbox item", push: proto.Push{Item: &proto.InboxItem{ID: 2, Issue: theirs, Body: "mention"}},
			want: "was pushed inbox item 2 naming sb-abcdefgh"},
		{name: "another project's event", push: proto.Push{Event: &proto.Event{Seq: 7, Issue: theirs}}, want: "was pushed event 7 naming sb-abcdefgh"},
	}
	for _, tc := range pushes {
		t.Run(tc.name, func(t *testing.T) {
			mon := newMonitor()
			var log inboxLog
			ln := log.newLine(alice, 1, mon, p)
			ln.watching(true)
			ln.watched(time.Now())
			ln.push(tc.push)
			got := mon.violations()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("push found %v, want nothing", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0].msg, tc.want)):
				t.Errorf("push found %v, want one violation containing %q", got, tc.want)
			}
		})
	}
}
