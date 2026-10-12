package store

import "testing"

// LastEventSeq is where a reader of the control store's log starts when
// it wants only what happens from now on: starfixd's audit stream.
func TestControlLastEventSeq(t *testing.T) {
	c := newControl(t)
	ctx := t.Context()
	if seq, err := c.LastEventSeq(ctx); err != nil || seq != 0 {
		t.Fatalf("LastEventSeq of an empty log = %d, %v, want 0", seq, err)
	}
	mustProject(t, c, projA)
	mustPrincipal(t, c, "alice", projA+"=admin")
	evs, err := c.Events(ctx, 0, 1000)
	if err != nil || len(evs) == 0 {
		t.Fatalf("Events = %d events, %v", len(evs), err)
	}
	seq, err := c.LastEventSeq(ctx)
	if want := evs[len(evs)-1].Seq; err != nil || seq != want {
		t.Fatalf("LastEventSeq = %d, %v, want the newest event's %d", seq, err, want)
	}
	if later, err := c.Events(ctx, seq, 10); err != nil || len(later) != 0 {
		t.Errorf("Events after LastEventSeq = %d events, %v, want none", len(later), err)
	}
}
