package server

import (
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// `sfx away` reaches as far as the server's away_max (rule 18), 7 days
// by default; past it, renew with all is refused invalid, with a fix
// that names the cap and who can raise it.
func TestAwayMax(t *testing.T) {
	t.Parallel()
	aliceCLI := store.Actor{Principal: "alice", Session: proto.CLISession, Machine: "laptop-a"}
	tests := []struct {
		name  string
		max   time.Duration
		lease string
		// fix is what the refusal's fix names; "" wants the away applied.
		fix string
	}{
		{name: "default, a week", lease: "7d"},
		{name: "default, past a week", lease: "8d", fix: "at most 7d"},
		{name: "lowered, at it", max: 2 * 24 * time.Hour, lease: "2d"},
		{name: "lowered, past it", max: 2 * 24 * time.Hour, lease: "3d", fix: "at most 2d"},
		{name: "raised, at it", max: 30 * 24 * time.Hour, lease: "30d"},
		{name: "raised, past it", max: 30 * 24 * time.Hour, lease: "31d", fix: "at most 30d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newServerWith(t, Limits{AwayMax: Duration(tc.max)})
			a := mustCall[proto.WriteResult](t, s, aliceCLI, proto.OpCreate, proto.CreateArgs{Title: "work"})
			mustCall[proto.StartResult](t, s, aliceCLI, proto.OpStart, proto.StartArgs{ID: a.ID})
			r, perr := call[proto.ClaimsResult](t, s, aliceCLI, proto.OpRenew, proto.RenewArgs{Lease: tc.lease, All: true})
			if tc.fix == "" {
				if perr != nil || len(r.Claims) != 1 {
					t.Fatalf("away %s with away_max %s = %+v, %+v; want the claim extended", tc.lease, tc.max, r, perr)
				}
				return
			}
			if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Fix, tc.fix) || !strings.Contains(perr.Fix, "away_max") {
				t.Errorf("away %s with away_max %s = %+v, want invalid with a fix naming %q and away_max", tc.lease, tc.max, perr, tc.fix)
			}
		})
	}
}
