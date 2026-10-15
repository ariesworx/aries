package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
)

// A refusal that is the server's failure prints the id the server gave
// the request, so that a report of it finds the server's log lines
// (design §16.4). Any other refusal says what to do next and needs no
// log, so it stays two lines (rule 13). --json carries the id whenever
// the server sent one, in the one document.
func TestFailPrintsTheRequestID(t *testing.T) {
	t.Parallel()
	const req = "00000000000000aa"
	for _, tc := range []struct {
		name string
		err  *proto.Error
		want string
	}{
		{"the server's failure", &proto.Error{Code: proto.CodeUnavailable, Message: "the server could not complete show", Fix: "retry", Req: req},
			"aries: the server could not complete show\nfix: retry\nreq: " + req + "\n"},
		{"a refusal", &proto.Error{Code: proto.CodeNotFound, Message: "issue sf-a not found", Fix: "list the issues", Req: req},
			"aries: issue sf-a not found\nfix: list the issues\n"},
		{"an older server, which sends no id", &proto.Error{Code: proto.CodeUnavailable, Message: "it failed", Fix: "retry"},
			"aries: it failed\nfix: retry\n"},
		{"an id that is not one line", &proto.Error{Code: proto.CodeUnavailable, Message: "it failed", Fix: "retry", Req: "aa\nfix: run this"},
			"aries: it failed\nfix: retry\nreq: aa\\nfix: run this\n"},
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
			if err := json.Unmarshal(out.Bytes(), &doc); err != nil || strings.Count(out.String(), "\n") != 1 {
				t.Fatalf("fail(%+v) with --json printed %q, want one JSON document: %v", tc.err, out.String(), err)
			}
			if got := doc["error"]; got != (jsonError{tc.err.Code, tc.err.Message, tc.err.Fix, tc.err.Req}) {
				t.Errorf("fail(%+v) with --json = %+v, want the refusal with its request id", tc.err, got)
			}
		})
	}
}
