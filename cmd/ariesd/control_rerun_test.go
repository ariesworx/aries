package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// `starfixd control init`, run again, leaves alone what an admin did
// since (security review of stage 3a, F-7). It is documented as safe to
// run again and the Docker image runs it at every start, and it once
// gave every principal with a line in authorized_keys a membership of
// the adopted project and its key back, whoever had removed them.

// rerunSite is a site after its first `control init`, with what an admin
// then did: the file names dana, the server admin, and
//
//	bob    removed from the adopted project
//	carol  whose key was removed
//	erin   disabled
//	frank  whose key was removed and then added again by an admin
//	gus    removed from the project and then added again by an admin
func rerunSite(t *testing.T) (s *site, keys string) {
	t.Helper()
	s = newSite(t, "")
	keys = writeKeys(t,
		forced(t, "dana", "dana-1"),   // 1
		forced(t, "bob", "bob-1"),     // 2
		forced(t, "carol", "carol-1"), // 3
		forced(t, "erin", "erin-1"),   // 4
		forced(t, "frank", "frank-1"), // 5
		forced(t, "gus", "gus-1"),     // 6
	)
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys); got.err != nil {
		t.Fatalf("control init: %v\n%s", got.err, got.stderr)
	}
	dana := store.Actor{Principal: "dana", Session: "s", Machine: "m"}
	c := s.control(t)
	ctx := t.Context()
	_, carolFP := pubKey(t, "carol-1")
	frankKey, frankFP := pubKey(t, "frank-1")
	for _, step := range []func() error{
		func() error { _, err := c.RemoveMember(ctx, dana, "bob", adopted); return err },
		func() error { _, err := c.RemoveKey(ctx, dana, carolFP); return err },
		func() error { _, err := c.DisablePrincipal(ctx, dana, "erin"); return err },
		func() error { _, err := c.RemoveKey(ctx, dana, frankFP); return err },
		func() error { _, err := c.AddKey(ctx, dana, "frank", frankKey); return err },
		func() error { _, err := c.RemoveMember(ctx, dana, "gus", adopted); return err },
		func() error { _, err := c.SetMember(ctx, dana, "gus", adopted, store.RoleMember); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return s, keys
}

// access is what the control store holds for the principals of
// rerunSite, as one line each.
func access(t *testing.T, s *site) string {
	t.Helper()
	c := s.control(t)
	ctx := t.Context()
	var b strings.Builder
	for _, name := range []string{"bob", "carol", "erin", "frank", "gus"} {
		p, err := c.Principal(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		ks, err := c.Keys(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		_, member, err := c.Role(ctx, name, adopted)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %s keys=%d member=%t\n", name, p.State, len(ks), member)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func events(t *testing.T, s *site) []store.Event {
	t.Helper()
	c := s.control(t)
	evs, err := c.Events(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestControlInitRerunKeepsWhatAdminsRemoved(t *testing.T) {
	t.Parallel()
	s, keys := rerunSite(t)
	const left = "bob active keys=1 member=false\n" +
		"carol active keys=0 member=true\n" +
		"erin disabled keys=1 member=true\n" +
		"frank active keys=1 member=true\n" +
		"gus active keys=1 member=true\n"
	if got := access(t, s); got != left {
		t.Fatalf("before the rerun the store holds\n%swant\n%s", got, left)
	}
	before := events(t, s)
	today := before[len(before)-1].At.Format("2006-01-02")
	_, carolFP := pubKey(t, "carol-1")

	// The operator runs it again, to import a new line, or the container
	// restarts.
	got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys)
	if got.err != nil {
		t.Fatalf("control init again: %v\n%s", got.err, got.stderr)
	}
	wantOut := fmt.Sprintf("project acme (%s) adopted from database %s: active\n", adopted, s.db) +
		"principals: 0 added, 6 already there; keys: 0 added, 4 already there; members: 0 added, 3 already there; 0 lines skipped\n" +
		"left as admins set them: 1 key removed, 1 membership removed, 1 principal disabled; to put them back from this file, run it again with --restore-removed-access\n" +
		"4 keys in the store are also lines of " + keys + ": sshd reads that file too, so `sfx admin key remove` does not keep such a key out until its line is deleted too\n"
	if got.stdout != wantOut {
		t.Errorf("stdout of the rerun = %q\nwant              %q", got.stdout, wantOut)
	}
	wantErr := "authorized_keys line 2: bob was removed from the project by dana on " + today + "; kept removed\n" +
		"authorized_keys line 3: key " + carolFP + " of carol was removed by dana on " + today + "; kept removed from the store, though sshd still accepts it by this line: delete the line to keep the key out\n" +
		"authorized_keys line 4: erin was disabled by dana on " + today + "; kept disabled, and nothing of the line imported\n"
	if got.stderr != wantErr {
		t.Errorf("stderr of the rerun = %q\nwant              %q", got.stderr, wantErr)
	}
	if got := access(t, s); got != left {
		t.Errorf("after the rerun the store holds\n%swant what the admin left\n%s", got, left)
	}
	if after := events(t, s); len(after) != len(before) {
		t.Errorf("the rerun recorded %d events, want none: %+v", len(after)-len(before), after[len(before):])
	}
	// The review's scenario: the daemon still refuses bob.
	sock := serveSite(t, s)
	if w := hello(t, sock, "bob", adopted); w.Err == nil || w.Err.Code != proto.CodeForbidden {
		t.Errorf("bob's handshake after the rerun = %+v, want forbidden: he was removed from the project", w)
	}
	if w := hello(t, sock, "gus", adopted); w.Err != nil {
		t.Errorf("the handshake of gus, whom an admin added again = %v, want a welcome", w.Err)
	}
}

func TestControlInitRestoreRemovedAccess(t *testing.T) {
	t.Parallel()
	s, keys := rerunSite(t)
	before := events(t, s)
	got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys, "--restore-removed-access")
	if got.err != nil {
		t.Fatalf("control init --restore-removed-access: %v\n%s", got.err, got.stderr)
	}
	wantOut := fmt.Sprintf("project acme (%s) adopted from database %s: active\n", adopted, s.db) +
		"principals: 0 added, 6 already there; keys: 1 added, 5 already there; members: 1 added, 4 already there; 0 lines skipped\n" +
		"restored from this file, over what admins had set: 1 key, 1 membership, 1 principal enabled\n" +
		"6 keys in the store are also lines of " + keys + ": sshd reads that file too, so `sfx admin key remove` does not keep such a key out until its line is deleted too\n"
	if got.stdout != wantOut {
		t.Errorf("stdout = %q\nwant     %q", got.stdout, wantOut)
	}
	_, carolFP := pubKey(t, "carol-1")
	today := before[len(before)-1].At.Format("2006-01-02")
	wantErr := "authorized_keys line 2: bob was removed from the project by dana on " + today + "; added again\n" +
		"authorized_keys line 3: key " + carolFP + " of carol was removed by dana on " + today + "; added again\n" +
		"authorized_keys line 4: erin was disabled by dana on " + today + "; enabled\n"
	if got.stderr != wantErr {
		t.Errorf("stderr = %q\nwant     %q", got.stderr, wantErr)
	}
	const restored = "bob active keys=1 member=true\n" +
		"carol active keys=1 member=true\n" +
		"erin active keys=1 member=true\n" +
		"frank active keys=1 member=true\n" +
		"gus active keys=1 member=true\n"
	if got := access(t, s); got != restored {
		t.Errorf("after --restore-removed-access the store holds\n%swant\n%s", got, restored)
	}
	// Each restoration is on the record, under the operator.
	after := events(t, s)[len(before):]
	var ops []string
	for _, e := range after {
		if e.Actor.Principal != store.OperatorPrincipal || !strings.HasPrefix(e.Actor.Session, "control-init-") {
			t.Errorf("event %s on %s by %+v, want the operator's control-init session", e.Op, e.Target, e.Actor)
		}
		ops = append(ops, string(e.Op)+" "+e.Target)
	}
	want := []string{"member.set " + adopted + "/bob", "key.add " + carolFP, "principal.enable erin"}
	if strings.Join(ops, "\n") != strings.Join(want, "\n") {
		t.Errorf("the restoration recorded\n%s\nwant\n%s", strings.Join(ops, "\n"), strings.Join(want, "\n"))
	}
	// Run without the flag after it, it has nothing left to say.
	again := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys)
	if again.err != nil || again.stderr != "" || strings.Contains(again.stdout, "left as admins set them") || strings.Contains(again.stdout, "restored") {
		t.Errorf("a plain run after the restoration = %q, %v, stderr %q; want nothing kept or restored", again.stdout, again.err, again.stderr)
	}
}

// The flag that overrides admins says what it overrides where an
// operator reads it: starfixd's usage, which every mistake on the command
// line prints.
func TestControlInitRestoreFlagIsExplained(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"[--restore-removed-access]", "sfx admin key remove", "sfx admin member remove", "sfx admin principal disable"} {
		if !strings.Contains(usage, want) {
			t.Errorf("starfixd's usage does not mention %q:\n%s", want, usage)
		}
	}
	// It is no flag of anything else, and has no short form to mistype.
	s := newSite(t, "")
	if got := s.run(t, "", controlCmd, "init", "--name", "acme", "--restore"); got.err == nil || !strings.Contains(got.err.Error(), "flag provided but not defined") {
		t.Errorf("control init --restore = %v, want it refused as no flag", got.err)
	}
}
