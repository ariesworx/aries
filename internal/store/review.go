package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/ariesworx/aries/internal/proto"
)

// Review before close and approval before start (design §21.6, decision
// 95; slice W3). A profile's review makes, in the create's write, a human
// gate whose subject is the new issue and that holds nothing: the issue
// can be started and worked, and finish and close are refused, with an
// [*AcceptanceError] naming the gate, until it passes or an admin waives
// it. A profile's approve makes a human gate that holds the new issue
// back, so that it and its descendants stay out of ready until it passes
// (gates.go). Both are passed as any human gate is (decision 68): by a
// principal the profile names, else an admin other than the gate's
// creator, and only from the session cli.
//
// The gate is the record: what an issue was made under is read from the
// gates that exist, never from the workflow in force. "Not the author"
// waits for agent identities (enterprise review E2).

// ReviewWait is a review an issue waits for: its gate, and the approvers
// the gate names, none when the project's admins pass it.
type ReviewWait struct {
	Gate      IssueID  `json:"gate"`
	Approvers []string `json:"approvers"`
}

// reviewsText joins rs as a refusal names them: the gate and who passes it.
func reviewsText(rs []ReviewWait) string {
	s := make([]string, len(rs))
	for i, r := range rs {
		who := "an admin"
		if len(r.Approvers) > 0 {
			who = strings.Join(r.Approvers, " or ")
		}
		s[i] = fmt.Sprintf("gate %s, passed by %s from their terminal", r.Gate, who)
	}
	return strings.Join(s, "; ")
}

// reviewWaits returns the open review gates of issue id, in id order.
func reviewWaits(ctx context.Context, q querier, id IssueID) ([]ReviewWait, error) {
	gates, err := loadGates(ctx, q, `WHERE g.kind = 'human' AND g.subject = $1 AND g.outcome IS NULL`, maxGateChecks, string(id))
	if err != nil {
		return nil, err
	}
	var out []ReviewWait
	for _, g := range gates {
		out = append(out, ReviewWait{Gate: g.ID, Approvers: g.Approvers})
	}
	return out, nil
}

// profileGates makes the gates the profile that the workflow in force
// gives issue id, of type t, asks for: a review and an approval. It
// runs in the write that made the issue, after the issue exists, and
// does nothing where no workflow is set or the profile names neither.
func (s *Store) profileGates(ctx context.Context, w *wtx, id IssueID, t IssueType) error {
	doc, _, err := readWorkflow(ctx, w.tx)
	if err != nil || doc == nil {
		return err
	}
	p, _, ok := doc.Profile(string(t))
	if !ok {
		return nil
	}
	for _, g := range []struct {
		who    *proto.WorkflowApprovers
		review bool
	}{{p.Approve, false}, {p.Review, true}} {
		if g.who == nil {
			continue
		}
		in := NewGate{Kind: GateHuman, Approvers: g.who.Approvers}
		if g.review {
			in.review = id
		} else {
			in.Blocks = id
		}
		if err := in.normalize(s.opts.Limits.GateApprovers); err != nil {
			return err
		}
		gid, err := NewID(s.opts.Prefix)
		if err != nil {
			return err
		}
		if _, err := s.insertGate(ctx, w, gid, in); err != nil {
			return fmt.Errorf("make the %s gate of %s: %w", map[bool]string{true: "review", false: "approval"}[g.review], id, err)
		}
	}
	return nil
}
