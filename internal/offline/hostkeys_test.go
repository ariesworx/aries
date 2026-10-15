package offline

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/proto"
)

// newKey is the host key a server rotates to from oldKey.
const newKey = "SHA256:newnewnewnewnewnewnewnewnewnewnewnewnewnewn"

// dirs lists the directories of root: one for each server and project
// this machine keeps anything for.
func dirs(t *testing.T, root string) []string {
	t.Helper()
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestFindAcrossARotation follows a host key rotation as the operator
// makes it: the config pins the old key, then the old and the new, then
// the new alone. Throughout, the server is one server to this machine:
// one directory, the same outbox and the same record of who each key
// is, whichever pinned key it presented, and nothing is left under the
// old key for a person to settle.
func TestFindAcrossARotation(t *testing.T) {
	t.Parallel()
	root := ownDir(t)
	o := Options{Now: func() time.Time { return t0 }, Address: "example.com:22"}
	find := func(pinned ...string) *Local { return Find(root, pinned, project, o) }

	before := find(oldKey)
	if _, err := before.Reached("alice", keyA, true, proto.Proto); err != nil {
		t.Fatal(err)
	}
	if _, err := before.Keep("alice", entry(proto.OpComment, "sf-a"), errReach); err != nil {
		t.Fatal(err)
	}

	// The new key is committed beside the old; a command reaches the
	// server, which still presents the old one.
	both := find(oldKey, newKey)
	if both.Dir() != before.Dir() {
		t.Fatalf("Find(old, new).Dir() = %s, want the old key's %s", both.Dir(), before.Dir())
	}
	if _, err := both.Reached("alice", keyA, true, proto.Proto); err != nil {
		t.Fatal(err)
	}
	// The order of the list is the config's, and changes nothing.
	if d := find(newKey, oldKey).Dir(); d != before.Dir() {
		t.Errorf("Find(new, old).Dir() = %s, want %s", d, before.Dir())
	}

	// The server presents the new key, and the old one is removed from
	// the config. The server is away for the first command.
	after := find(newKey)
	after.Missed()
	if after.Dir() != before.Dir() {
		t.Fatalf("after the rotation, Find(new).Dir() = %s, want the directory of before it, %s", after.Dir(), before.Dir())
	}
	if got := dirs(t, root); len(got) != 1 {
		t.Errorf("after the rotation, the data directory holds %v, want the one directory", got)
	}
	if who, ok := after.Principal([]string{keyA}, true); !ok || who != "alice" {
		t.Errorf("after the rotation, Principal(keyA) = %q, %v; want alice, as recorded before it", who, ok)
	}
	if n, err := after.Waiting("alice"); err != nil || n.Pending != 1 {
		t.Errorf("after the rotation, alice's outbox holds %+v, %v; want the entry queued before it", n, err)
	}
	if f, err := after.Rekeyed(); err != nil || len(f) != 0 {
		t.Errorf("after the rotation, Rekeyed() = %v, %v; want nothing under another key", f, err)
	}
	// The next handshake, with the new key alone, keeps it found.
	if _, err := after.Reached("alice", keyA, true, proto.Proto); err != nil {
		t.Fatal(err)
	}
	if d := find(newKey).Dir(); d != before.Dir() {
		t.Errorf("after a handshake with the new key alone, Find(new).Dir() = %s, want %s", d, before.Dir())
	}
}

// TestClientAcrossARotation is TestFindAcrossARotation through the
// committed config, as sfx reads it on every command: host_key, then
// host_keys with both, then host_key with the new one. What alice queued
// before is sent on a connection after, and nothing is said of an old
// host key.
func TestClientAcrossARotation(t *testing.T) {
	t.Parallel()
	r := newRepo(t)
	c := r.newClient(t, nil)
	pin := func(server string) {
		t.Helper()
		cfg := fmt.Sprintf("project: %s\nserver:\n  host: 127.0.0.1\n  port: 2222\n%skey: id_mine\n", project, server)
		if err := os.WriteFile(filepath.Join(r.dir, client.ConfigFile), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	alice := func() *keyedConn { return &keyedConn{fakeConn: fakeConn{who: "alice"}, key: r.mine} }
	args := proto.CommentArgs{ID: "sf-a", Body: "a note", Idem: proto.NewIdem("cli")}

	c.Connected(alice()) // newRepo pins hostKey
	if _, err := c.Keep("", proto.OpComment, args, errReach); err != nil {
		t.Fatal(err)
	}
	pin("  host_keys:\n    - " + hostKey + "\n    - " + newKey + "\n")
	c.Connected(alice())
	pin("  host_key: " + newKey + "\n")
	if _, err := c.Keep("", proto.OpComment, args, errReach); err != nil {
		t.Fatalf("Keep after the rotation = %v, want it queued as alice's, as before it", err)
	}
	if got := c.Rekeyed(); got != "" {
		t.Errorf("Rekeyed() after the rotation = %q, want nothing under an old host key", got)
	}
	conn := alice()
	if rep, err := c.Replay(t.Context(), conn); err != nil || rep.Delivered != 2 {
		t.Errorf("Replay after the rotation = %+v, %v; want both entries, from before and after it, sent", rep, err)
	}
}

// TestFindKeepsAChangedKeyApart: what was kept under a host key is the
// pinned server's only when a handshake was made while the config
// pinned that key beside the one pinned now. A key that was never
// pinned with it, as when a server is rebuilt and the config changed
// to its new key at once, gets a directory of its own, and what was
// kept before is the old key's, which is never sent by itself.
func TestFindKeepsAChangedKeyApart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// between is what happened after oldKey's directory queued its
		// entry, and before the config pinned newKey alone.
		between func(t *testing.T, root string, o Options)
	}{
		{"the config changed to the new key at once", func(*testing.T, string, Options) {}},
		{"both were pinned, but no command reached the server then", func(_ *testing.T, root string, o Options) {
			Find(root, []string{oldKey, newKey}, project, o).Missed()
		}},
		{"both were pinned with another key, which replaced them", func(t *testing.T, root string, o Options) {
			if _, err := Find(root, []string{oldKey, keyX}, project, o).Reached("alice", keyA, true, proto.Proto); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := ownDir(t)
			o := Options{Now: func() time.Time { return t0 }, Address: "example.com:22"}
			old := queuedAt(t, root, oldKey, project, o.Address, 1)
			tc.between(t, root, o)
			cur := Find(root, []string{newKey}, project, o)
			if cur.Dir() == old.Dir() {
				t.Fatalf("Find(new) = the old key's directory %s, want one of its own", cur.Dir())
			}
			got, err := cur.Rekeyed()
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, f := range got {
				keys = append(keys, f.HostKey)
			}
			if !slices.Equal(keys, []string{oldKey}) {
				t.Errorf("Rekeyed() = %v, want what was kept under %s", keys, oldKey)
			}
		})
	}
}
