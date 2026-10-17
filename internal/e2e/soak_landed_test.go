package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// Close means landed in the soak (design §21.5, slice W1). The first
// project's workflow requires landed of a bug, reported by root from a
// CI job's session (the lander), which passes it on bugs at random and
// now and then fails it, as a revert would. The sessions file some of
// their issues there as bugs and finish, close and hand them off as any
// other: a finish or close of one is refused until it lands. The replay
// holds every issue.close of an issue whose workflow requires a check to
// a pass of that check before it, or to an admin's force, on the record,
// and every reopen for a failed check to a failure in its transaction.

// soakWorkflow is the first project's workflow: a bug requires landed,
// which root reports.
func (s *soak) soakWorkflow() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"version": 1, "ci": [%q], "types": {"bug": {"checks": ["landed"]}}}`, s.cfg.root))
}

// setWorkflow applies the first project's workflow, as root at a
// terminal, before any session starts.
func (s *soak) setWorkflow() {
	ac := s.newACall(s.cfg.root, proto.CLISession, s.a)
	defer ac.close()
	var r proto.WorkflowSetResult
	if out, perr, _ := ac.do(proto.OpWorkflowSet, proto.WorkflowSetArgs{Workflow: s.soakWorkflow()}, &r); out != acked {
		s.t.Fatalf("soak setup: workflow set: %v", perr)
	}
}

// land is the lander: root's CI job on the first project, reporting
// landed on its bugs until ctx ends.
func (s *soak) land(ctx context.Context) {
	rng := rand.New(rand.NewPCG(s.cfg.seed, 13)) //nolint:gosec // reproducible choices, not security
	ac := s.newACall(s.cfg.root, proto.CISession, s.a)
	defer ac.close()
	for n := 0; ctx.Err() == nil; n++ {
		pause(ctx, time.Duration(50+rng.IntN(200))*time.Millisecond)
		id, ok := s.a.bugs.pick(rng)
		if !ok || ctx.Err() != nil {
			continue
		}
		op := proto.OpGatePass
		if rng.IntN(5) == 0 {
			op = proto.OpGateFail
		}
		args := proto.GateArgs{Subject: id, Check: "landed", SHA: fmt.Sprintf("%040x", n+1)}
		var g proto.Gate
		_, _, _ = ac.do(op, args, &g)
	}
}

// landing is what the replay knows of the workflow and the checks.
type landing struct {
	// docs are the workflow's documents by rev, from workflow.set.
	docs map[int64]json.RawMessage
	// required are the checks each issue's workflow requires.
	required map[string][]string
	// gateOf is each ci gate's subject and check, and by the principals
	// it names; passed whether that gate is passed now.
	gateOf map[string][2]string
	by     map[string][]string
	passed map[[2]string]bool
	// landed counts the closing passes, overridden the forced closes of
	// such issues, and reopened the reopens for a failed check.
	landed, overridden, reopened int
}

func (r *replay) landingState() *landing {
	if r.land == nil {
		r.land = &landing{docs: map[int64]json.RawMessage{}, required: map[string][]string{}, gateOf: map[string][2]string{},
			by: map[string][]string{}, passed: map[[2]string]bool{}}
	}
	return r.land
}

// workflowSet applies a workflow.set.
func (r *replay) workflowSet(e sevent) {
	var a struct {
		Rev      int64           `json:"rev"`
		Workflow json.RawMessage `json:"workflow"`
	}
	if err := json.Unmarshal(e.after, &a); err != nil || a.Rev == 0 {
		r.fail(e.target, "event %d: workflow.set's after state %s holds no rev", e.seq, e.after)
		return
	}
	r.landingState().docs[a.Rev] = a.Workflow
}

// stampWorkflow applies the workflow stamp of an issue.create or issue.update:
// the checks of the profile it names join those the issue requires.
func (r *replay) stampWorkflow(e sevent) {
	var a struct {
		Workflow *struct {
			Rev  int64  `json:"rev"`
			Type string `json:"type"`
		} `json:"workflow"`
	}
	if json.Unmarshal(e.after, &a) != nil || a.Workflow == nil || a.Workflow.Type == "" {
		return
	}
	l := r.landingState()
	raw, ok := l.docs[a.Workflow.Rev]
	if !ok {
		r.fail(e.target, "event %d stamps workflow rev %d, which no workflow.set made", e.seq, a.Workflow.Rev)
		return
	}
	var doc proto.Workflow
	if err := json.Unmarshal(raw, &doc); err != nil {
		r.fail(e.target, "event %d: workflow rev %d: %v", e.seq, a.Workflow.Rev, err)
		return
	}
	for _, c := range doc.Types[a.Workflow.Type].Checks {
		if !slices.Contains(l.required[e.target], c) {
			l.required[e.target] = append(l.required[e.target], c)
		}
	}
}

// gateEvent applies gate.add, gate.pass and gate.fail.
func (r *replay) gateEvent(e sevent) {
	l := r.landingState()
	switch e.op {
	case "gate.add":
		var a struct {
			Kind      string   `json:"kind"`
			Check     string   `json:"check"`
			Subject   string   `json:"subject"`
			Approvers []string `json:"approvers"`
		}
		_ = json.Unmarshal(e.after, &a)
		if a.Kind != "ci" || a.Subject == "" {
			r.fail(e.target, "event %d adds a gate the soak does not make: %s", e.seq, e.after)
			return
		}
		l.gateOf[e.target], l.by[e.target] = [2]string{a.Subject, a.Check}, a.Approvers
	case "gate.pass", "gate.fail":
		k, ok := l.gateOf[e.target]
		if !ok {
			r.fail(e.target, "event %d reports on %s, which no gate.add made", e.seq, e.target)
			return
		}
		if !slices.Contains(l.by[e.target], e.actor.principal) {
			r.fail(e.target, "event %d: %s reported %s on %s, which names %v", e.seq, e.actor, k[1], k[0], l.by[e.target])
		}
		l.passed[k] = e.op == "gate.pass"
	}
}

// landedClose checks an issue.close of an issue whose workflow requires
// checks: each passed, or the close an admin's force that records them
// overridden; and a closing pass (reason landed) in a transaction with a
// pass on the issue.
func (r *replay) landedClose(e sevent, tx []sevent) {
	l := r.landingState()
	req := l.required[e.target]
	if len(req) == 0 {
		return
	}
	var a struct {
		CloseReason string   `json:"close_reason"`
		Overridden  []string `json:"checks_overridden"`
	}
	_ = json.Unmarshal(e.after, &a)
	for _, c := range req {
		if !l.passed[[2]string{e.target, c}] && !slices.Contains(a.Overridden, c) {
			r.fail(e.target, "event %d closes %s, whose workflow requires %s, which has not passed", e.seq, e.target, c)
		}
	}
	switch {
	case len(a.Overridden) > 0:
		if !r.admins[e.actor.principal] && !r.roles {
			r.fail(e.target, "event %d: %s closed %s over its checks, and is no admin", e.seq, e.actor, e.target)
		}
		l.overridden++
	case a.CloseReason == "landed":
		if !slices.ContainsFunc(tx, func(o sevent) bool { return o.op == "gate.pass" && l.gateOf[o.target][0] == e.target }) {
			r.fail(e.target, "event %d closes %s as landed, with no pass of its check in the transaction", e.seq, e.target)
		}
		l.landed++
	}
}

// landedReopen checks a reopen for a failed check: a failure of a check
// on the issue is in its transaction.
func (r *replay) landedReopen(e sevent, tx []sevent) {
	var a struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(e.after, &a) != nil || a.Reason != "check failed" {
		return
	}
	l := r.landingState()
	if !slices.ContainsFunc(tx, func(o sevent) bool { return o.op == "gate.fail" && l.gateOf[o.target][0] == e.target }) {
		r.fail(e.target, "event %d reopens %s for a failed check, with no failure in the transaction", e.seq, e.target)
	}
	l.reopened++
}

// reported reports whether the transaction being applied reports a
// check that issue's workflow requires: what it does to the issue is the
// report's, by the principal the workflow names, whoever holds it.
func (r *replay) reported(issue string) bool {
	if r.land == nil {
		return false
	}
	return slices.ContainsFunc(r.cur, func(o sevent) bool {
		k, ok := r.land.gateOf[o.target]
		return (o.op == "gate.pass" || o.op == "gate.fail") && ok && k[0] == issue && slices.Contains(r.land.required[issue], k[1])
	})
}

// reportLanding adds the first project's landing to the report.
func (s *soak) reportLanding(b *bytes.Buffer) {
	r := s.a.rep
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.landingState()
	fmt.Fprintf(b, "  landing: %d issues require a check; %d passes, %d failures reported; %d closed by a pass, %d forced, %d reopened for a failed check\n",
		len(l.required), r.ops["gate.pass"], r.ops["gate.fail"], l.landed, l.overridden, l.reopened)
}
