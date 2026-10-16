package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/jose"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
)

// A service principal signs in with the token its platform already
// gives it (docs/design/identity.md §7, transport.md §7, slice N5): a
// CI job asks GitHub Actions for an OIDC token, a cloud workload its
// metadata server, and `aries login --workload` exchanges it at the
// token endpoint, by OAuth 2.0 Token Exchange (RFC 8693), with a DPoP
// proof by a key the job has just made:
//
//	POST /oauth/token
//	DPoP: <proof>
//	grant_type=urn:ietf:params:oauth:grant-type:token-exchange
//	subject_token=<the platform's token>
//	subject_token_type=urn:ietf:params:oauth:token-type:jwt
//
// The server gives the token to the workload provider its issuer names,
// which verifies it (internal/oidc) with the server's audience, bound to
// the proof's key where the platform lets the job choose its audience
// (AUDIENCE/n/THUMBPRINT), and at most workload: fresh old. A token is
// spent once: the server remembers each it exchanged for as long as it
// could still be taken, and refuses one bound to no key (Azure's) that
// was issued before the server started, whose spending a restart would
// have forgotten. The control store then finds the one service
// principal whose trust rules the token's claims satisfy, and grants a
// sign-in of scope ci for the rule's lifetime, DPoP-bound, with no
// refresh token. No secret is stored anywhere.
//
// A workload's sign-in is not announced in the principal's inbox, as a
// person's is: a CI job signs in at every run, and nobody reads a
// service's inbox. The audit stream has each, with the run it was.

// The names of RFC 8693.
//
//nolint:gosec // URNs, not credentials
const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeJWT       = "urn:ietf:params:oauth:token-type:jwt"
	tokenTypeIDToken   = "urn:ietf:params:oauth:token-type:id_token"
	tokenTypeAccess    = "urn:ietf:params:oauth:token-type:access_token"
)

// spentSkew is how long past workload: fresh a token is remembered: the
// most skew login: allows a platform's clock.
const spentSkew = maxSkew

// exchange is the token endpoint's token exchange: a workload's token,
// verified and matched against the trust rules, for a token of scope ci
// bound to the proof's key. The caller has checked the address and read
// the form.
func (a *authServer) exchange(rw http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	now := a.now()
	bad := func(code, why string) {
		a.refused(r, "", "exchange_request")
		oauthError(rw, http.StatusBadRequest, code, proto.Errf(proto.CodeInvalid, proto.FixUpgrade+" aries", why))
	}
	subject := f.Get("subject_token")
	switch stt := f.Get("subject_token_type"); {
	case subject == "" || len(subject) > jose.MaxToken:
		bad("invalid_request", fmt.Sprintf("subject_token, the platform's token of at most %d bytes, is required", jose.MaxToken))
		return
	case stt != tokenTypeJWT && stt != tokenTypeIDToken:
		bad("invalid_request", "subject_token_type must be "+tokenTypeJWT)
		return
	case f.Get("requested_token_type") != "" && f.Get("requested_token_type") != tokenTypeAccess:
		bad("invalid_request", "the server issues an access token alone")
		return
	case f.Get("actor_token") != "":
		bad("invalid_request", "the server takes no actor_token")
		return
	case f.Get("scope") != "" && f.Get("scope") != proto.ScopeCI:
		bad("invalid_scope", "a workload signs in for the scope ci alone")
		return
	}
	resource := a.base()
	for _, k := range []string{"resource", "audience"} {
		if v := f.Get(k); v != "" {
			if !slices.Contains(a.s.cfg.HTTPS.URLs, v) {
				bad("invalid_target", "the "+k+" is not this server")
				return
			}
			resource = v
		}
	}
	proof, err := dpop.Check(r.Header.Get("DPoP"), http.MethodPost, a.at(pathToken), "", now)
	if err != nil {
		e, _ := errors.AsType[*dpop.Error](err)
		a.refused(r, "", "dpop_"+string(e.Reason))
		a.failed(r)
		oauthError(rw, http.StatusBadRequest, "invalid_dpop_proof", proto.Errf(proto.CodeAuth, "run `aries login --workload` again; check this machine's clock", err.Error()))
		return
	}
	p, claims, perr := a.verifyWorkload(r.Context(), subject, proof.JKT, now)
	if perr != nil {
		name := ""
		if p != nil {
			name = p.Name()
		}
		a.refused(r, name, "workload_"+string(perr.Code))
		status, code := http.StatusBadRequest, "invalid_grant"
		if perr.Code == proto.CodeUnavailable || perr.Code == proto.CodeBusy {
			status, code = http.StatusServiceUnavailable, "temporarily_unavailable"
		} else {
			a.failed(r)
		}
		oauthError(rw, status, code, perr)
		return
	}
	if err := a.proofs.Spend(proof, now); err != nil {
		e, _ := errors.AsType[*dpop.Error](err)
		a.refused(r, p.Name(), "dpop_"+string(e.Reason))
		if e.Reason == dpop.ReasonBusy {
			a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitDPoPProofs)
			oauthError(rw, http.StatusServiceUnavailable, "temporarily_unavailable", proto.Errf(proto.CodeBusy, "run `aries login --workload` again in a minute", err.Error()))
			return
		}
		a.failed(r)
		oauthError(rw, http.StatusBadRequest, "invalid_dpop_proof", proto.Errf(proto.CodeAuth, "run `aries login --workload` again", err.Error()))
		return
	}
	a.grantWorkload(rw, r, p, claims, proof.JKT, resource)
}

// verifyWorkload checks subject, a workload's token presented with a
// proof by the key whose thumbprint is jkt, and spends it: the
// provider its issuer names, the provider's verifier, the server's start
// for a token bound to no key, and the tokens already spent. It returns
// the provider, once known, and the token's claims, or a refusal.
func (a *authServer) verifyWorkload(ctx context.Context, subject, jkt string, now time.Time) (*oidc.Workload, *oidc.Claims, *proto.Error) {
	again := "run `aries login --workload` again; if it recurs, ask the server's admin to check the provider under workload: providers"
	iss, err := oidc.PeekIssuer(subject)
	if err != nil {
		return nil, nil, proto.Errf(proto.CodeAuth, again, "the platform's token is not a signed JWT: "+err.Error())
	}
	p := a.workloads[iss]
	if p == nil {
		// The issuer is the token's, unverified: it is the requester's own
		// to see, and is cleaned before it is.
		return nil, nil, proto.Errf(proto.CodeForbidden,
			"ask the server's operator to add the platform under workload: providers (docs/server.md, Service principals)",
			fmt.Sprintf("this server signs in no workload of issuer %s", safetext.Line(iss)))
	}
	c, err := p.Verify(ctx, subject, jkt)
	switch {
	case errors.Is(err, oidc.ErrUnavailable):
		a.workloadFailed(ctx, p, err)
		return p, nil, proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server's admin checks the provider in `aries admin status`",
			fmt.Sprintf("the server could not check the token of %s", p.Name()))
	case err != nil:
		return p, nil, proto.Errf(proto.CodeAuth, again, fmt.Sprintf("the token of %s did not verify: %s", p.Name(), strings.TrimPrefix(err.Error(), oidc.ErrInvalidToken.Error()+": ")))
	}
	if !p.Bound() && time.Unix(c.Iat, 0).Before(a.started) {
		return p, nil, proto.Errf(proto.CodeAuth, "ask the platform for a new token and run `aries login --workload` again",
			fmt.Sprintf("the token of %s was issued before the server started, and is bound to no key, so it may have been exchanged already", p.Name()))
	}
	switch a.spent.spend(hashOf(subject), now, now.Add(a.workload.Fresh+spentSkew)) {
	case errTokenSpent:
		return p, nil, proto.Errf(proto.CodeAuth, "ask the platform for a new token and run `aries login --workload` again",
			fmt.Sprintf("the token of %s was exchanged already", p.Name()))
	case errSpentFull:
		a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, telemetry.LimitWorkloadTokens)
		return p, nil, proto.Errf(proto.CodeBusy, "run `aries login --workload` again in a minute",
			fmt.Sprintf("the server holds the %d workload tokens it remembers as exchanged; the oldest age out within %s",
				a.spent.most, proto.Span(a.workload.Fresh+spentSkew)))
	}
	return p, c, nil
}

// grantWorkload has the store match the token's claims to a trust rule
// and record the sign-in, and answers with its token.
func (a *authServer) grantWorkload(rw http.ResponseWriter, r *http.Request, p *oidc.Workload, c *oidc.Claims, jkt, resource string) {
	machine := cmp.Or(r.PostForm.Get("machine"), "unknown")
	if !validActorPart(machine) || len(machine) > 255 {
		machine = "unknown"
	}
	claims := oidc.FlatClaims(c.All)
	launch := map[string]string{}
	for _, k := range oidc.LaunchClaims(p.Kind()) {
		if v, ok := claims[k]; ok {
			launch[k] = v
		}
	}
	tok := rand.Text() + rand.Text() // 256 random bits and more, base32
	g, rule, err := a.s.cfg.Control.WorkloadSignIn(r.Context(), store.WorkloadSignIn{Provider: p.Name(), Claims: claims, Launch: launch,
		ClientID: ClientCLI, Scopes: []string{proto.ScopeCI}, JKT: jkt, Resource: resource, Machine: machine,
		MaxLifetime: a.workload.MaxTokenLifetime, TokenHash: hashOf(tok)})
	if err != nil {
		e := a.workloadErr(r.Context(), p, claims, err)
		a.refusedSignIn(r.Context(), p.Name(), e)
		status := http.StatusForbidden
		if e.Code == proto.CodeUnavailable || e.Code == proto.CodeBusy {
			status = http.StatusServiceUnavailable
		}
		oauthError(rw, status, "access_denied", e)
		return
	}
	a.s.Audit(r.Context())
	a.s.cfg.Logger.InfoContext(r.Context(), "signed in", "event", logs.SignInGranted, "principal", g.Principal, "provider", g.Provider,
		"grant", g.ID, "flow", g.Flow, "trust_rule", rule.Name)
	writeJSON(rw, http.StatusOK, map[string]any{
		"access_token": tok, "issued_token_type": tokenTypeAccess, "token_type": "DPoP", "expires_in": int(g.EndsAt.Sub(a.now()).Seconds()),
		"expires_at": g.EndsAt.Format(time.RFC3339), "scope": strings.Join(g.Scopes, " "),
		"principal": g.Principal, "provider": g.Provider, "grant": g.ID, "trust_rule": rule.Name,
	})
}

// workloadErr words the store's refusal of a workload's sign-in at p, for
// the job: the claims of its own token that a rule would have to pin.
func (a *authServer) workloadErr(ctx context.Context, p *oidc.Workload, claims map[string]string, err error) *proto.Error {
	if te, ok := errors.AsType[*store.TrustError](err); ok {
		switch te.Reason {
		case store.TrustAmbiguous:
			return proto.Errf(proto.CodeForbidden, "ask a server admin to narrow one of them with `aries admin trust set`, so that one principal's rules match", te.Error())
		case store.TrustDisabledPrincipal:
			return disabledErr(te.Principal)
		case store.TrustNotService:
			return proto.Errf(proto.CodeForbidden, "ask a server admin to make it a service, or to remove the rule", te.Error())
		}
		var pins, flags []string
		for _, k := range oidc.RequiredClaims(p.Kind()) {
			v := claims[k]
			pins = append(pins, k+"="+v)
			flags = append(flags, "--claim "+k+"="+v)
		}
		return proto.Errf(proto.CodeForbidden,
			fmt.Sprintf("ask a server admin to add a trust rule: `aries admin trust add NAME --principal SERVICE --provider %s %s`", p.Name(), strings.Join(flags, " ")),
			fmt.Sprintf("%s; the token's %s", te.Error(), strings.Join(pins, ", ")))
	}
	if ce, ok := errors.AsType[*store.ControlLimitError](err); ok {
		a.s.cfg.Telemetry.Count(telemetry.LimitRefusals, ce.Limit)
		return proto.Errf(proto.CodeBusy,
			fmt.Sprintf("end each job's sign-in with `aries logout` in a step that always runs, or wait for one to end; or ask the server's operator to raise %s under limits: in ariesd's config", ce.Limit),
			strings.TrimPrefix(ce.Error(), store.ErrInvalid.Error()+": "))
	}
	a.s.cfg.Logger.ErrorContext(ctx, "record a workload's sign-in", "event", logs.RequestFailed, "provider", p.Name(), "err", err)
	return proto.Errf(proto.CodeUnavailable, "retry; if it persists, the server's admin should check the ariesd log", "the server could not record the sign-in")
}

// workloadFailed logs a workload provider that could not be reached or
// read.
func (a *authServer) workloadFailed(ctx context.Context, p *oidc.Workload, err error) {
	a.s.refusals.line(ctx, a.s.cfg.Logger, "provider:"+p.Name(), a.s.now(), slog.LevelWarn, logs.SignInProviderFailed,
		"the provider could not be reached or read", "provider", p.Name(), "err", err)
}

// The refusals of spentTokens.spend.
var (
	errTokenSpent = errors.New("spent")
	errSpentFull  = errors.New("full")
)

// spentTokens are the workload tokens exchanged, by hash, each until the
// time it could no longer be taken: bounded by most, and forgotten in the
// order they were spent, which is the order their times come, since
// each is the store's time (rule 19) and a fixed window.
type spentTokens struct {
	most int

	mu    sync.Mutex
	until map[string]time.Time // guarded by mu
	order []string             // guarded by mu: hashes, oldest first, from head
	head  int                  // guarded by mu
}

func newSpentTokens(most int) *spentTokens {
	return &spentTokens{most: most, until: map[string]time.Time{}}
}

// spend remembers hash until until, at now: errTokenSpent when it is
// remembered already, and errSpentFull when most are, none past its
// time.
func (s *spentTokens) spend(hash string, now, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
	if _, ok := s.until[hash]; ok {
		return errTokenSpent
	}
	if len(s.until) >= s.most {
		return errSpentFull
	}
	s.until[hash] = until
	s.order = append(s.order, hash)
	return nil
}

// prune forgets the tokens past their time at now.
func (s *spentTokens) prune(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(now)
}

// expire forgets, from the front, the tokens past their time. The caller
// holds mu.
func (s *spentTokens) expire(now time.Time) {
	for s.head < len(s.order) {
		h := s.order[s.head]
		if now.Before(s.until[h]) {
			break
		}
		delete(s.until, h)
		s.head++
	}
	if s.head > 0 && s.head*2 >= len(s.order) {
		s.order = append(s.order[:0], s.order[s.head:]...)
		s.head = 0
	}
}
