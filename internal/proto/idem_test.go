package proto

import (
	"reflect"
	"testing"
)

// TestKeyedArgs pins which ops take an idempotency key: the arguments of
// each have an Idem field, so a repeat of the op returns its first result
// and writes nothing more. The outbox queues only ops of this list
// (internal/outbox), and dep.rm, which it never queues, honors its key
// too (decision 88).
func TestKeyedArgs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op   string
		args any
	}{
		{OpCreate, CreateArgs{}},
		{OpComment, CommentArgs{}},
		{OpHandoff, HandoffArgs{}},
		{OpFinish, FinishArgs{}},
		{OpRemember, RememberArgs{}},
		{OpForget, ForgetArgs{}},
		{OpHoursLog, HoursLogArgs{}},
		{OpHoursDelete, HoursDeleteArgs{}},
		{OpUpdate, UpdateArgs{}},
		{OpLabelAdd, LabelArgs{}},
		{OpLabelRm, LabelArgs{}},
		{OpDepAdd, DepArgs{}},
		{OpDepRm, DepArgs{}},
		{OpAccept, AcceptArgs{}},
		{OpUsage, UsageArgs{}},
	} {
		if f, ok := reflect.TypeOf(tc.args).FieldByName("Idem"); !ok || f.Tag.Get("json") != "idem,omitempty" {
			t.Errorf("%T, the arguments of %s, has no field idem", tc.args, tc.op)
		}
	}
}
