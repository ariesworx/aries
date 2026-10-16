package proto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestKeyed pins which ops a server of each protocol deduplicates by
// their idempotency key: the eight keyed before protocol 6 at any
// version this build speaks, and the six protocol 6 adds (design §18.5)
// only from 6 on. An op that takes no key is keyed at none.
func TestKeyed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		op     string
		since  int // 0 is never
		inArgs any // the op's *Args, which must have an idem field from since on
	}{
		{OpCreate, 5, CreateArgs{}},
		{OpComment, 5, CommentArgs{}},
		{OpHandoff, 5, HandoffArgs{}},
		{OpFinish, 5, FinishArgs{}},
		{OpRemember, 5, RememberArgs{}},
		{OpForget, 5, ForgetArgs{}},
		{OpHoursLog, 5, HoursLogArgs{}},
		{OpHoursDelete, 5, HoursDeleteArgs{}},
		{OpUpdate, 6, UpdateArgs{}},
		{OpLabelAdd, 6, LabelArgs{}},
		{OpLabelRm, 6, LabelArgs{}},
		{OpDepAdd, 6, DepArgs{}},
		{OpAccept, 6, AcceptArgs{}},
		{OpUsage, 6, UsageArgs{}},
		{OpStart, 0, StartArgs{}},
		{OpClose, 0, CloseArgs{}},
		{OpDepRm, 0, nil},
	} {
		t.Run(tc.op, func(t *testing.T) {
			t.Parallel()
			for v := ProtoMin; v <= Proto; v++ {
				want := tc.since != 0 && v >= tc.since
				if got := Keyed(tc.op, v); got != want {
					t.Errorf("Keyed(%q, %d) = %v, want %v", tc.op, v, got, want)
				}
			}
			if tc.since == 0 || tc.inArgs == nil {
				return
			}
			if _, ok := reflect.TypeOf(tc.inArgs).FieldByName("Idem"); !ok {
				t.Errorf("%T has no Idem field, but %s is keyed from protocol %d", tc.inArgs, tc.op, tc.since)
			}
		})
	}
}

// TestForProtocol: a request for a server of protocol 5 goes without the
// key protocol 6 added to its op, which that server would refuse as an
// unknown argument; for 6 it goes as it is, as does every op 5 knows
// the key of. A replayed update's park, which protocol 7 added, goes to
// 7 alone.
func TestForProtocol(t *testing.T) {
	t.Parallel()
	title := "renamed"
	update := UpdateArgs{ID: "ar-1", Rev: 3, Title: &title, Idem: "cli-1"}
	replayed := update
	replayed.Park = true
	comment := CommentArgs{ID: "ar-1", Body: "b", Idem: "cli-2"}
	raw := json.RawMessage(`{"id":"ar-1","label":"x","idem":"cli-3"}`)
	for _, tc := range []struct {
		name string
		v    int
		op   string
		args any
		want string // the arguments as sent
	}{
		{"update to 6", 6, OpUpdate, update, `{"id":"ar-1","rev":3,"title":"renamed","idem":"cli-1"}`},
		{"update to 5", 5, OpUpdate, update, `{"id":"ar-1","rev":3,"title":"renamed"}`},
		{"a pointer to update, to 5", 5, OpUpdate, &update, `{"id":"ar-1","rev":3,"title":"renamed"}`},
		{"raw label.add to 5", 5, OpLabelAdd, raw, `{"id":"ar-1","label":"x"}`},
		{"raw label.add to 6", 6, OpLabelAdd, raw, `{"id":"ar-1","label":"x","idem":"cli-3"}`},
		{"comment to 5", 5, OpComment, comment, `{"id":"ar-1","body":"b","idem":"cli-2"}`},
		{"update with no key, to 5", 5, OpUpdate, UpdateArgs{ID: "ar-1", Rev: 3}, `{"id":"ar-1","rev":3}`},
		{"a replayed update to 7", 7, OpUpdate, replayed, `{"id":"ar-1","rev":3,"title":"renamed","idem":"cli-1","park":true}`},
		{"a replayed update to 6", 6, OpUpdate, replayed, `{"id":"ar-1","idem":"cli-1","rev":3,"title":"renamed"}`},
		{"a replayed update to 5", 5, OpUpdate, replayed, `{"id":"ar-1","rev":3,"title":"renamed"}`},
		{"a raw replayed update to 6", 6, OpUpdate, json.RawMessage(`{"id":"ar-1","rev":3,"park":true}`), `{"id":"ar-1","rev":3}`},
		{"a replayed finish to 8", 8, OpFinish, FinishArgs{ID: "ar-1", Epoch: 2, Idem: "cli-4", Park: true},
			`{"id":"ar-1","epoch":2,"idem":"cli-4","park":true}`},
		{"a replayed finish to 7", 7, OpFinish, FinishArgs{ID: "ar-1", Epoch: 2, Idem: "cli-4", Park: true},
			`{"epoch":2,"id":"ar-1","idem":"cli-4"}`},
		{"a raw replayed handoff to 7", 7, OpHandoff, json.RawMessage(`{"id":"ar-1","note":"n","park":true}`), `{"id":"ar-1","note":"n"}`},
		{"a raw replayed handoff to 8", 8, OpHandoff, json.RawMessage(`{"id":"ar-1","note":"n","park":true}`), `{"id":"ar-1","note":"n","park":true}`},
		{"a replayed update to 8", 8, OpUpdate, replayed, `{"id":"ar-1","rev":3,"title":"renamed","idem":"cli-1","park":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(ForProtocol(tc.v, tc.op, tc.args))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("ForProtocol(%d, %q, %+v) sends %s, want %s", tc.v, tc.op, tc.args, got, tc.want)
			}
		})
	}
}
