package outbox

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

// TestUnqueued pins which ops a caller is told are not queued, and why:
// the writes the outbox never takes and the ones that wait for protocol
// 6. An op it queues, and one that writes nothing, have no reason.
func TestUnqueued(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   string
		want string // part of the reason; "" for none
	}{
		{proto.OpStart, "takes a claim"},
		{proto.OpClose, "cannot see"},
		{proto.OpReopen, "cannot see"},
		{proto.OpForget, "cannot see"},
		{proto.OpUpdate, "no idempotency key"},
		{proto.OpLabelAdd, "no idempotency key"},
		{proto.OpAccept, "no idempotency key"},
		{proto.OpUsage, "no idempotency key"},
		{proto.OpFinish, ""},
		{proto.OpHandoff, ""},
		{proto.OpList, ""},
		{proto.OpShow, ""},
		{"no.such.op", ""},
	} {
		why, ok := Unqueued(tc.op)
		if ok != (tc.want != "") || !strings.Contains(why, tc.want) {
			t.Errorf("Unqueued(%q) = %q, %v; want a reason holding %q", tc.op, why, ok, tc.want)
		}
		if ok && Queueable(tc.op) {
			t.Errorf("Unqueued(%q) and Queueable(%q) are both true", tc.op, tc.op)
		}
	}
}
