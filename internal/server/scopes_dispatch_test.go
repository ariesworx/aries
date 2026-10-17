package server

import (
	"crypto/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/wsconn"
)

// TestScopesAtDispatch walks every op the protocol names over a real
// connection of each scope, cli, mcp and ci, each in the session it may
// take (transport.md §6.8, §12.4 item 2): an op is refused for its scope
// exactly when opScopes does not open it to that scope, and cli is
// refused none. Each op is sent with empty arguments, so what is not
// refused for its scope is refused for its arguments, or answered; what
// is checked is only whether the refusal is the scopes'.
func TestScopesAtDispatch(t *testing.T) {
	t.Parallel()
	w := newSignInWorld(t, nil)
	ops := protoOps(t)
	for _, tc := range []struct {
		scope, session string
	}{
		{proto.ScopeCLI, proto.CLISession},
		{proto.ScopeMCP, "agent-1"},
		{proto.ScopeCI, proto.CISession},
	} {
		// In turn: the world holds one Postgres server (CONTRIBUTING.md).
		t.Run(tc.scope, func(t *testing.T) {
			c := w.connectScoped(t, tc.scope, tc.session)
			var id uint64
			for _, op := range ops {
				if op == proto.OpWatch {
					continue // below, on a connection of its own
				}
				id++
				f := callFor(t, c, id, op)
				checkScoped(t, tc.scope, op, f)
				if f.Err != nil && f.Err.Code == proto.CodeBusy {
					t.Fatalf("%s with scope %s: %v; the walk ran into a limit, not a scope", op, tc.scope, f.Err)
				}
			}
			c = w.connectScoped(t, tc.scope, tc.session)
			checkScoped(t, tc.scope, proto.OpWatch, callFor(t, c, 1, proto.OpWatch))
		})
	}
}

// checkScoped fails t unless f, the answer to op on a connection of
// scope, is refused for its scope exactly when opScopes says it is.
func checkScoped(t *testing.T, scope, op string, f *proto.Frame) {
	t.Helper()
	want := scope != proto.ScopeCLI && !slices.Contains(opScopes()[op], scope)
	got := f.Err != nil && f.Err.Code == proto.CodeForbidden && strings.Contains(f.Err.Message, "is not allowed to a sign-in of scope")
	switch {
	case want && !got:
		t.Errorf("%s with scope %s = %+v; want it refused for its scope", op, scope, f.Err)
	case !want && got:
		t.Errorf("%s with scope %s refused for its scope (%s); opScopes opens it", op, scope, f.Err.Message)
	}
}

// connectScoped signs alice in with scope alone, by a sign-in made in
// the store as the token exchange makes one, and returns a connection
// past its hello in session.
func (w *signInWorld) connectScoped(t *testing.T, scope, session string) *wsconn.Conn {
	t.Helper()
	w.mustSignIn(t, newAttempt(t)) // alice's identity, which the sign-in names
	k, err := dpop.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tok := rand.Text() + rand.Text()
	if _, err := w.ctl.SignIn(t.Context(), store.SignIn{Provider: "corp", Issuer: w.iss.URL, Subject: "00000000-0000-4000-8000-00000000a11c",
		Address: "alice@example.com", ClientID: ClientCLI, Scopes: []string{scope}, JKT: dpop.Thumbprint(&k.PublicKey), Resource: w.url,
		Flow: store.FlowWorkload, Machine: "laptop-a", Lifetime: time.Hour, TokenHash: hashOf(tok)}); err != nil {
		t.Fatal(err)
	}
	c, perr := w.connectWith(t, &attempt{key: k}, tok, "")
	if perr != nil {
		t.Fatalf("connect with scope %s: %v", scope, perr)
	}
	if wel := helloAs(t, c, session); wel.Err != nil {
		t.Fatalf("hello in %s with scope %s: %v", session, scope, wel.Err)
	}
	return c
}

// callFor sends op with empty arguments on c and returns its answer,
// skipping any frame pushed to a watcher.
func callFor(t *testing.T, c *wsconn.Conn, id uint64, op string) *proto.Frame {
	t.Helper()
	s := wsconn.Stream(c)
	req, err := proto.Request(id, op, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.NewEncoder(s).Encode(req); err != nil {
		t.Fatalf("send %s: %v", op, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(patience))
	dec := proto.NewDecoder(s)
	for {
		f, err := dec.Decode()
		if err != nil {
			t.Fatalf("read the answer to %s: %v", op, err)
		}
		if f.T == proto.FrameRes && f.ID == id {
			return f
		}
	}
}
