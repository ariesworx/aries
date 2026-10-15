package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// A command refused because its principal is at its connection limit
// says what a person can do about it, whichever server refused it: a
// released one sends the fix it shares with the server's own limit.
// Exit 1, as for any busy.
func TestFailAtTheConnectionLimit(t *testing.T) {
	t.Parallel()
	released := proto.Errf(proto.CodeBusy, "close other aries sessions, or wait for them to end, and retry", "alice is at its connection limit (64)")
	server := proto.Errf(proto.CodeBusy, "close other aries sessions, or wait for them to end, and retry", "the server is at its connection limit (1024)")
	for _, tc := range []struct {
		name string
		err  *proto.Error
		want string
	}{
		{"this server", proto.ConnLimit("alice", 64), "aries: alice is at its connection limit (64)\nfix: " + proto.FixConnLimit + "\n"},
		{"a released server", released, "aries: alice is at its connection limit (64)\nfix: " + proto.FixConnLimit + "\n"},
		{"the server's own limit", server, "aries: the server is at its connection limit (1024)\nfix: " + server.Fix + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var errb bytes.Buffer
			r := &runner{env: Env{Stderr: &errb}}
			if code := r.fail(tc.err); code != ExitFailure {
				t.Errorf("fail(%+v) = exit %d, want %d", tc.err, code, ExitFailure)
			}
			if got := errb.String(); got != tc.want {
				t.Errorf("fail(%+v) printed %q, want %q", tc.err, got, tc.want)
			}

			var out bytes.Buffer
			r = &runner{env: Env{Stdout: &out, Stderr: &errb}, json: true}
			r.fail(tc.err)
			var doc map[string]jsonError
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
				t.Fatalf("fail(%+v) with --json printed %q: %v", tc.err, out.String(), err)
			}
			if got := doc["error"]; "aries: "+got.Message+"\nfix: "+got.Fix+"\n" != tc.want || got.Code != proto.CodeBusy {
				t.Errorf("fail(%+v) with --json = %+v, want what the text says", tc.err, got)
			}
		})
	}
}
