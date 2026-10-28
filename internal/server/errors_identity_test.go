package server

import (
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// An admin refused an invitation onto their own principal is told who
// may: another server admin, or, where there is none, the operator on
// the server, by `ariesd control invite`.
func TestAccessErrOwnIdentity(t *testing.T) {
	t.Parallel()
	perr := accessErr(&store.AccessError{Actor: "dana", Op: store.OpIdentityInvite, Principal: "dana", Reason: store.AccessOwnIdentity})
	if perr.Code != proto.CodeForbidden {
		t.Errorf("code = %s, want %s", perr.Code, proto.CodeForbidden)
	}
	for _, want := range []string{"another server admin", "ariesd control invite"} {
		if !strings.Contains(perr.Fix, want) {
			t.Errorf("fix = %q, want it to name %q", perr.Fix, want)
		}
	}
}
