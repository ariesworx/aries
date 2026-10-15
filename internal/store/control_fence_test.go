package store

import (
	"errors"
	"testing"
)

// The control store has one writer too: the daemon's. The operator's
// `ariesd control init`, which runs beside the daemon, takes no lock, nor
// does a store opened to read.
func TestWriterLockOfTheControlStore(t *testing.T) {
	t.Parallel()
	dsn := newDSN(t)
	first, second := openDB(t, dsn, DBOptions{}), openDB(t, dsn, DBOptions{})
	makeSchema(t, dsn, "ctl")
	c, err := first.OpenControl(t.Context(), "ctl", ControlOptions{Admins: []string{"dana"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, err = second.OpenControl(t.Context(), "ctl", ControlOptions{Admins: []string{"dana"}})
	if se, ok := errors.AsType[*ServedError](err); !ok || se.Schema != "ctl" {
		t.Fatalf("a second daemon's OpenControl(ctl) = %v, want a *ServedError for ctl", err)
	}
	for _, opts := range []ControlOptions{{Operator: true}, {ReadOnly: true}} {
		o, err := second.OpenControl(t.Context(), "ctl", opts)
		if err != nil {
			t.Errorf("OpenControl(ctl, %+v) beside the daemon = %v, want it opened", opts, err)
			continue
		}
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
