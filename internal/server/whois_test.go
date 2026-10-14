package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/telemetry"
)

// aliasOn returns the principal a span of the request op carries.
func aliasOn(t *testing.T, m *telemetry.Manual, op string) string {
	t.Helper()
	for _, sp := range m.Spans() {
		if sp.Name == op && sp.Attrs["principal"] != "" {
			return sp.Attrs["principal"]
		}
	}
	t.Fatalf("no %s span names a principal:\n%s", op, telemetry.SpanList(m.Spans()))
	return ""
}

// A server admin resolves the stand-in a span carries to the principal
// it is for (design §16.4, decision 32). Nobody else may, and a stand-in
// that is nobody's, or made under a key the server no longer has, is
// refused with what to do.
func TestWhois(t *testing.T) {
	t.Parallel()
	tel, m := spanned(t, telemetry.Config{})
	mu := adminWorldTel(t, Limits{}, tel)
	d := mustDialTo(t, mu.Server, dana, projA)
	a := mustDialTo(t, mu.Server, alice, projA)
	mustAsk[proto.ListResult](a, proto.OpReady, proto.LimitArgs{})
	mustAsk[proto.ListResult](d, proto.OpBlocked, proto.LimitArgs{})
	alices, danas := aliasOn(t, m, proto.OpReady), aliasOn(t, m, proto.OpBlocked)
	if alices == danas || !principalAlias.MatchString(alices) {
		t.Fatalf("alice is %q and dana %q on their spans, want two stand-ins", alices, danas)
	}

	whois := func(c *conn, alias string) (proto.PrincipalWhoisResult, *proto.Error) {
		return ask[proto.PrincipalWhoisResult](c, proto.OpPrincipalWhois, proto.PrincipalWhoisArgs{StandIn: alias})
	}
	if got, perr := whois(d, alices); perr != nil || got != (proto.PrincipalWhoisResult{StandIn: alices, Name: "alice", State: "active"}) {
		t.Errorf("whois %s by a server admin = %+v, %v; want alice, active", alices, got, perr)
	}
	// A server admin the config file names signs in without a row.
	if got, perr := whois(d, danas); perr != nil || got.Name != "dana" || got.State != "" {
		t.Errorf("whois %s = %+v, %v; want dana, who has no row", danas, got, perr)
	}
	// A disabled principal's stand-in still resolves, and says so.
	mustAsk[proto.PrincipalResult](d, proto.OpPrincipalDisable, proto.PrincipalArgs{Name: "bob"})
	bobs, _ := tel.StandIn("bob")
	if got, perr := whois(d, bobs); perr != nil || got.Name != "bob" || got.State != "disabled" {
		t.Errorf("whois of a disabled principal = %+v, %v; want bob, disabled", got, perr)
	}

	for _, tc := range []struct {
		name  string
		by    *conn
		alias string
		code  proto.Code
		msg   string
		fix   string
	}{
		{"a project admin", a, bobs, proto.CodeForbidden, "", "server admin"},
		{"a project admin, of their own stand-in", a, alices, proto.CodeForbidden, "", "server admin"},
		{"a name", d, "alice", proto.CodeInvalid, "not a principal's stand-in", "p_ and 16 hex digits"},
		{"a session's stand-in", d, "s_" + alices[2:], proto.CodeInvalid, "not a principal's stand-in", "p_ and 16 hex digits"},
		{"capitals", d, strings.ToUpper(alices), proto.CodeInvalid, "not a principal's stand-in", "p_ and 16 hex digits"},
		{"nothing", d, "", proto.CodeInvalid, "not a principal's stand-in", "p_ and 16 hex digits"},
		{"a stand-in that is nobody's", d, "p_0123456789abcdef", proto.CodeNotFound, "p_0123456789abcdef is the stand-in of no principal", "deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, perr := whois(tc.by, tc.alias)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Errorf("whois %q = %+v, %+v; want %s, a message containing %q and a fix containing %q", tc.alias, got, perr, tc.code, tc.msg, tc.fix)
			}
			// A refusal never says whose the stand-in is.
			if perr != nil && strings.Contains(perr.Message+perr.Fix, "bob") {
				t.Errorf("the refusal names the principal: %+v", perr)
			}
		})
	}

	// A server with no key says so, and why, rather than answering that
	// nobody matches: it is the server that cannot answer, not the
	// stand-in that is wrong.
	t.Run("a server with no key", func(t *testing.T) {
		for name, tc := range map[string]struct {
			tel *telemetry.Telemetry
			why string
		}{
			"no telemetry": {nil, "it exports no telemetry"},
			"traces off":   {mustTelemetry(t, telemetry.Config{}), "it exports no traces"},
		} {
			plain := adminWorldTel(t, Limits{}, tc.tel)
			_, perr := whois(mustDialTo(t, plain.Server, dana, projA), alices)
			if perr == nil || perr.Code != proto.CodeUnavailable || !strings.Contains(perr.Message, "has no key for stand-ins: "+tc.why) || perr.Fix == "" {
				t.Errorf("%s: whois = %+v; want unavailable, saying the server has no key because %s, with a fix", name, perr, tc.why)
			}
		}
	})
}

// A key that cannot be made does not stop the server, and is not papered
// over: requests are served, their spans say nothing about who (no
// stand-in, and never the name), the status report says people are left
// out because the key is unavailable and why, and whois says the same.
// Once the cause is gone the reaper's next tick finds the key, with no
// restart, and spans name people by stand-ins from then on.
func TestSpansWithoutTheKey(t *testing.T) {
	t.Parallel()
	key := filepath.Join(t.TempDir(), "telemetry", "principals.key")
	tel, m := spanned(t, telemetry.Config{KeyFile: key})
	mu := adminWorldTel(t, Limits{}, tel)
	d := mustDialTo(t, mu.Server, dana, projA)
	a := mustDialTo(t, mu.Server, alice, projA)
	whoOn := func(op string, c *conn) (string, string) {
		t.Helper()
		m.ResetSpans()
		traced(c, op, proto.LimitArgs{}, sampledTP, "")
		for _, sp := range m.Spans() {
			if sp.Name == op {
				return sp.Attrs["principal"], sp.Attrs["session"]
			}
		}
		t.Fatalf("no %s span", op)
		return "", ""
	}

	for _, sp := range m.Spans() {
		for k, v := range sp.Attrs {
			if k == "principal" || k == "session" || strings.Contains(v, "alice") || strings.Contains(v, "dana") || principalAlias.MatchString(v) {
				t.Errorf("with the key unavailable, span %s carries %s=%q", sp.Name, k, v)
			}
		}
	}
	if p, sess := whoOn(proto.OpReady, a); p != "" || sess != "" {
		t.Errorf("with the key unavailable a request's span carries principal %q and session %q, want neither", p, sess)
	}
	st := mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
	if tr := st.Server.Traces; tr == nil || tr.People != "unavailable" || !strings.Contains(tr.Cause, "no such file or directory") || tr.KeyFile != key || tr.Damaged {
		t.Errorf("status: traces = %+v; want people unavailable, the cause and the key's file", st.Server.Traces)
	}
	_, perr := ask[proto.PrincipalWhoisResult](d, proto.OpPrincipalWhois, proto.PrincipalWhoisArgs{StandIn: "p_0123456789abcdef"})
	if perr == nil || perr.Code != proto.CodeUnavailable || !strings.Contains(perr.Message, "the key is unavailable") ||
		!strings.Contains(perr.Message, "no such file or directory") || !strings.Contains(perr.Fix, "starfixd's log") {
		t.Errorf("whois with the key unavailable = %+v; want unavailable, with the cause, and a fix that points at the log", perr)
	}

	// The operator makes the directory, and does nothing else.
	if err := os.Mkdir(filepath.Dir(key), 0o700); err != nil {
		t.Fatal(err)
	}
	mu.tick(t.Context(), false)
	p, sess := whoOn(proto.OpBlocked, a)
	if !principalAlias.MatchString(p) || !sessionAlias.MatchString(sess) {
		t.Fatalf("after the directory was made and the reaper ticked, the span carries principal %q and session %q, want stand-ins", p, sess)
	}
	st = mustAsk[proto.StatusResult](d, proto.OpStatus, proto.StatusArgs{})
	if tr := st.Server.Traces; tr == nil || tr.People != "pseudonym" || tr.Cause != "" {
		t.Errorf("status after the fix: traces = %+v, want people pseudonym and no cause", st.Server.Traces)
	}
	if got, perr := ask[proto.PrincipalWhoisResult](d, proto.OpPrincipalWhois, proto.PrincipalWhoisArgs{StandIn: p}); perr != nil || got.Name != "alice" {
		t.Errorf("whois after the fix = %+v, %v; want alice", got, perr)
	}
}

// The status report says how people appear in traces, and nothing of it
// when the server exports none.
func TestStatusTraces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tel  func(t *testing.T) *telemetry.Telemetry
		want *proto.TracesStatus
	}{
		{"no telemetry", func(*testing.T) *telemetry.Telemetry { return nil }, nil},
		{"traces off", func(t *testing.T) *telemetry.Telemetry { return mustTelemetry(t, telemetry.Config{}) }, nil},
		{"stand-ins", func(t *testing.T) *telemetry.Telemetry { tel, _ := spanned(t, telemetry.Config{}); return tel }, &proto.TracesStatus{People: "pseudonym"}},
		{"names", func(t *testing.T) *telemetry.Telemetry {
			tel, _ := spanned(t, telemetry.Config{Principals: telemetry.PrincipalsName})
			return tel
		}, &proto.TracesStatus{People: "name"}},
		{"nobody, by the setting", func(t *testing.T) *telemetry.Telemetry {
			tel, _ := spanned(t, telemetry.Config{Principals: telemetry.PrincipalsOmit})
			return tel
		}, &proto.TracesStatus{People: "omit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu := adminWorldTel(t, Limits{}, tc.tel(t))
			got := mustAsk[proto.StatusResult](mustDialTo(t, mu.Server, dana, projA), proto.OpStatus, proto.StatusArgs{}).Server.Traces
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("status: traces = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// mustTelemetry makes a Telemetry as the daemon does, from cfg alone.
func mustTelemetry(t *testing.T, cfg telemetry.Config) *telemetry.Telemetry {
	t.Helper()
	tel, err := telemetry.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return tel
}
