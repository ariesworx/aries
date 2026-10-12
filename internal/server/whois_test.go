package server

import (
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

	// A server that never made a stand-in has no key to resolve one
	// with, and says so rather than answering that nobody matches.
	t.Run("a server with no key", func(t *testing.T) {
		for name, tel := range map[string]*telemetry.Telemetry{"no telemetry": nil, "traces off": mustTelemetry(t, telemetry.Config{})} {
			plain := adminWorldTel(t, Limits{}, tel)
			_, perr := whois(mustDialTo(t, plain.Server, dana, projA), alices)
			if perr == nil || perr.Code != proto.CodeNotFound || !strings.Contains(perr.Message, "no stand-in key") || !strings.Contains(perr.Fix, "unlinkable") {
				t.Errorf("%s: whois = %+v; want not_found, saying the server has no key and that an old stand-in is unlinkable", name, perr)
			}
		}
	})
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
