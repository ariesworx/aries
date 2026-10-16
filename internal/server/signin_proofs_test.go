package server

import (
	"crypto/rand"
	"net/http"
	"testing"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/proto"
)

// floodLimits is a memory of four DPoP proofs, and no bound on an
// address's refusals, as for a flood from many addresses or through a
// proxy, where auth_failures_per_minute does not apply.
var floodLimits = Limits{DPoPProofs: 4, AuthFailureRate: 1e9, AuthFailureBurst: 1e9}

// TestTokenFloodSpendsNoProofs (issue #160): anyone may sign a DPoP
// proof with a key of their own, so a request to the token endpoint
// without a live code must not be remembered; else a flood of them fills
// dpop_proofs_per_server, and every sign-in and connection is refused
// busy.
func TestTokenFloodSpendsNoProofs(t *testing.T) {
	t.Parallel()
	w := newLimitedSignInWorld(t, floodLimits, nil, nil)
	for i := range 3 * floodLimits.DPoPProofs {
		if ans := w.exchange(t, newAttempt(t), rand.Text(), ""); ans.Error != "invalid_grant" {
			t.Fatalf("anonymous token request %d = %+v, want invalid_grant", i, ans)
		}
	}
	a := newAttempt(t)
	if ans := w.signIn(t, a); ans.AccessToken == "" {
		t.Fatalf("sign-in after a flood of anonymous token requests = %+v, want a token", ans)
	}
	if n := w.auth.proofs.Len(); n != 1 {
		t.Errorf("proofs remembered after the flood and one sign-in = %d, want 1, the sign-in's", n)
	}
}

// TestConnectByAnotherKeySpendsNoProof (issue #160): a token presented
// with a proof by another key than its own is refused before the proof
// is remembered, so that a token without its key cannot fill the memory
// and lock its holder out.
func TestConnectByAnotherKeySpendsNoProof(t *testing.T) {
	t.Parallel()
	w := newLimitedSignInWorld(t, floodLimits, nil, nil)
	a := newAttempt(t)
	tok := w.mustSignIn(t, a).AccessToken
	for i := range 3 * floodLimits.DPoPProofs {
		thief := newAttempt(t)
		if _, perr := w.connectWith(t, thief, tok, ""); perr == nil || perr.Code != proto.CodeAuth {
			t.Fatalf("connect %d with the token and another key = %v, want auth", i, perr)
		}
	}
	c, perr := w.connectWith(t, a, tok, "")
	if perr != nil {
		t.Fatalf("connect with the token's own key after proofs by another = %v, want it upgraded", perr)
	}
	_ = c.Close()
}

// TestTokenRefusalsCountAsAuthFailures (issue #160): a refused request
// to the token endpoint costs its address as a refused key proof does,
// and past auth_failures_per_minute the address is refused at the token
// endpoint and at /v1/connect, busy, before any proof is checked.
func TestTokenRefusalsCountAsAuthFailures(t *testing.T) {
	t.Parallel()
	lim := Limits{AuthFailureRate: 1, AuthFailureBurst: 3}
	w := newLimitedSignInWorld(t, lim, nil, nil)
	for i := range lim.AuthFailureBurst {
		if ans := w.exchange(t, newAttempt(t), rand.Text(), ""); ans.Error != "invalid_grant" {
			t.Fatalf("anonymous token request %d = %+v, want invalid_grant", i, ans)
		}
	}
	if ans := w.exchange(t, newAttempt(t), rand.Text(), ""); ans.Status != http.StatusTooManyRequests || ans.Code != string(proto.CodeBusy) || ans.Fix == "" {
		t.Errorf("a token request past auth_failure_burst = %+v, want 429, busy, with a fix", ans)
	}
	_, resp, err := wsDial(t, w.url, w.pool, http.Header{"Authorization": {"DPoP " + rand.Text()},
		"DPoP": {w.proof(t, newAttempt(t).key, http.MethodGet, w.url+keyproof.ConnectPath, "")}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("connect from the address past auth_failure_burst = %v; want 429", err)
	}
}
