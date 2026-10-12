package store

import "context"

// LastEvent returns the newest event of the control database on target:
// a principal's name, a key's fingerprint, or PROJECT/PRINCIPAL for a
// membership. ok is false when the log holds none. It is one indexed
// read, so a caller may ask it for each line of a file: `starfixd
// control init`, run again, asks whether the newest thing an admin did
// to a key or a membership was to remove it.
func (c *Control) LastEvent(ctx context.Context, target string) (e Event, ok bool, err error) {
	evs, err := c.s.events(ctx, `WHERE target = ? ORDER BY seq DESC LIMIT 1`, target)
	if err != nil || len(evs) == 0 {
		return Event{}, false, err
	}
	return evs[0], true, nil
}
