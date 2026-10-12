package server

import (
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// TestProjectAdminCannotFillTheServersPrincipals pins the cap on what
// one project admin may create (security review of stage 3a, F-8).
// Principals are never deleted and the cap on them is the server's, so
// a project admin adding to their own project could once use the last
// of them, and a server admin's `principal add` for another project was
// refused. Each creator who is no server admin now has an allowance of
// their own (principals_per_creator).
func TestProjectAdminCannotFillTheServersPrincipals(t *testing.T) {
	// adminWorld has five rows already, all a server admin's.
	m := adminWorld(t, Limits{Control: store.ControlLimits{Principals: 9, PrincipalsPerCreator: 2}})
	a := mustDialTo(t, m.Server, alice, projA)
	for _, name := range []string{"filler-1", "filler-2"} {
		mustAsk[proto.PrincipalAddResult](a, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: name})
	}
	_, perr := ask[proto.PrincipalAddResult](a, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "filler-3"})
	const fix = "ask a server admin to add the principal, or the server's operator to raise principals_per_creator under limits: in starfixd's config"
	if perr == nil || perr.Code != proto.CodeInvalid || perr.Message != "a principal who is no server admin creates at most 2 principals" || perr.Fix != fix {
		t.Errorf("principal.add by alice, who created 2, with principals_per_creator 2 = %+v\nwant invalid and the fix %q", perr, fix)
	}
	// Another project's admin, and the server admin, still have room.
	e := mustDialTo(t, m.Server, erin, projB)
	if _, perr := ask[proto.PrincipalAddResult](e, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "erins"}); perr != nil {
		t.Errorf("principal.add by erin, admin of another project = %v, want it added", perr)
	}
	d := mustDialTo(t, m.Server, dana, projB)
	if _, perr := ask[proto.PrincipalAddResult](d, proto.OpPrincipalAdd, proto.PrincipalAddArgs{Name: "newhire", Project: projB}); perr != nil {
		t.Errorf("principal.add by the server admin after alice reached her cap = %v, want it added", perr)
	}
}
