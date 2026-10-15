package store

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// Taking work: StartIssue leases an issue to the actor (claims.go) and
// marks it in_progress, assigned to the actor's principal, in one
// transaction, so of two principals racing for one issue exactly one gets
// it. FinishIssue and a releasing HandoffIssue end the claim; they refuse
// a stale epoch, and an issue another principal holds unless the actor is
// an admin.

// MaxDiscovered bounds the issues one FinishIssue may file.
const MaxDiscovered = 20

// StartIssue takes an issue for the actor for lease (DefaultLease when
// zero, MinLease to MaxClaimLease otherwise): it claims the issue and sets
// it in_progress, assigned to the actor's principal, and returns both.
// With an empty id it takes the first issue [Store.Ready] would list, or
// returns [ErrNothingReady]. Taking an issue the actor's own session
// holds extends the lease and changes nothing else. StartIssue refuses:
//   - an issue another session of the actor's principal holds under a
//     live claim, with a [*HeldError] whose Own is set, unless take is
//     set, which takes the claim over under a new epoch;
//   - an issue another principal holds, with a [*HeldError];
//   - a closed issue, or a lease out of range, with ErrInvalid.
func (s *Store) StartIssue(ctx context.Context, actor Actor, id IssueID, lease time.Duration, take bool) (Issue, Claim, error) {
	if id != "" {
		if err := id.Validate(); err != nil {
			return Issue{}, Claim{}, err
		}
	}
	if lease == 0 {
		lease = DefaultLease
	}
	if err := checkLease(lease, MaxClaimLease); err != nil {
		return Issue{}, Claim{}, err
	}
	if id != "" {
		return s.start(ctx, actor, id, nil, lease, take)
	}
	// With no id, the ready issues are ranked on a read, so the project's
	// writer is not held for a query that grows with the project. The
	// write takes the first of them that is still ready then; another
	// session may have taken one, or it may have changed, in between. If
	// none is, the write ranks them again under the writer, as it did
	// before, where no one can get ahead of it.
	if err := actor.validate(); err != nil {
		return Issue{}, Claim{}, err
	}
	now, err := s.settledNow(ctx)
	if err != nil {
		return Issue{}, Claim{}, err
	}
	ranked, err := s.topReady(ctx, actor, now)
	if err != nil {
		return Issue{}, Claim{}, err
	}
	if len(ranked) == 0 {
		return Issue{}, Claim{}, ErrNothingReady
	}
	return s.start(ctx, actor, "", ranked, lease, take)
}

// startCandidates is how many of the issues ranked on a read StartIssue
// with no id tries, in order, under the writer before it ranks there.
const startCandidates = 5

// topReady returns the first startCandidates issues Ready lists for
// actor at now, from one snapshot.
func (s *Store) topReady(ctx context.Context, actor Actor, now time.Time) ([]IssueID, error) {
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	defer end()
	ids, _, err := rankReady(ctx, tx, actor, now, startCandidates)
	return ids, err
}

// start is StartIssue's write. With an id it takes that issue. Without
// one it takes the first of ranked, the issues ranked on a read, that is
// still ready, and if none is, the issue rankReady ranks first under the
// writer.
func (s *Store) start(ctx context.Context, actor Actor, id IssueID, ranked []IssueID, lease time.Duration, take bool) (Issue, Claim, error) {
	var out Issue
	var claim Claim
	err := s.write(ctx, actor, func(w *wtx) error {
		target := id
		for _, c := range ranked {
			ok, err := readyNow(ctx, w.tx, c, w.now)
			if err != nil {
				return err
			}
			if ok {
				target = c
				break
			}
		}
		if target == "" {
			ids, _, err := rankReady(ctx, w.tx, w.actor, w.now, 1)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				return ErrNothingReady
			}
			target = ids[0]
		}
		before, err := loadIssue(ctx, w.tx, target)
		if err != nil {
			return err
		}
		if before.Status == StatusClosed {
			return &StateError{ID: target, Reason: StateClosed}
		}
		if before.Type == TypeGate {
			return isGateError(ctx, w.tx, target, "start")
		}
		gates, err := holdingGates(ctx, w.tx, target)
		if err != nil {
			return err
		}
		if len(gates) > 0 {
			g, err := loadGate(ctx, w.tx, gates[0])
			if err != nil {
				return err
			}
			return heldError(target, g, "start")
		}
		c, err := loadClaim(ctx, w.tx, target)
		if err != nil {
			return err
		}
		if c.active(w.now) && c.Holder != w.actor {
			own := c.Holder.Principal == w.actor.Principal
			if !own || !take {
				return &HeldError{ID: target, By: c.Holder.Principal, Own: own, Session: c.Holder.Session}
			}
		}
		if claim, err = takeClaim(ctx, w, c, lease); err != nil {
			return err
		}
		out = before
		if before.Status == StatusInProgress && before.Assignee == w.actor.Principal {
			return nil
		}
		out, err = casUpdate(ctx, w, before, []string{"status", "assignee"},
			[]any{string(StatusInProgress), w.actor.Principal})
		if err != nil {
			return err
		}
		return w.issueEvent(ctx, OpIssueUpdate, before, out, nil)
	})
	if err != nil {
		return Issue{}, Claim{}, err
	}
	return out, claim, nil
}

// Finish is what FinishIssue records besides closing the issue.
type Finish struct {
	// Reason is the close reason.
	Reason string
	// Handoff, if its note is set, is recorded as a handoff note with
	// its fields.
	Handoff HandoffNote
	// Discovered are new issues, each linked discovered-from the finished
	// one. IDs are generated; ID and IdempotencyKey must be empty.
	Discovered []NewIssue
	// Accept ticks and waives acceptance items before the close, which
	// refuses while any is left open.
	Accept Acceptance
	// IdempotencyKey makes a retried finish return the first result.
	IdempotencyKey string
	// Paths are the paths the issue's work touched, most recent first,
	// recorded as its commit paths.
	Paths []string
}

// finished is FinishIssue's result, as its idempotency stamp stores it
// and a replay returns it; Lapsed and Comment are a replayed finish's
// ([Store.ReplayFinish]).
type finished struct {
	Issue   Issue     `json:"issue"`
	IDs     []IssueID `json:"ids"`
	Lapsed  bool      `json:"lapsed,omitempty"`
	Comment string    `json:"comment,omitempty"`
}

// FinishIssue closes an issue, ends its claim, records its handoff note
// and files the work discovered while doing it, all in one transaction:
// either everything is written or nothing is. It returns the closed issue
// and the new IDs, in the order given. FinishIssue refuses:
//   - an issue another principal holds, with a [*ForbiddenError], unless
//     the actor is an admin;
//   - a non-zero epoch that is not the claim's current one, with a
//     [*StaleEpochError];
//   - acceptance items left neither ticked nor waived once f.Accept is
//     applied, with an [*AcceptanceError];
//   - an idempotency key reused for another request, with an
//     [*IdemError];
//   - a closed issue, or invalid input, with ErrInvalid.
func (s *Store) FinishIssue(ctx context.Context, actor Actor, id IssueID, epoch int64, f Finish) (Issue, []IssueID, error) {
	out, err := s.finish(ctx, actor, id, epoch, f, false)
	if err != nil {
		return Issue{}, nil, err
	}
	return out.Issue, out.IDs, nil
}

// finish is FinishIssue, and with late [Store.ReplayFinish].
func (s *Store) finish(ctx context.Context, actor Actor, id IssueID, epoch int64, f Finish, late bool) (finished, error) {
	if err := id.Validate(); err != nil {
		return finished{}, err
	}
	if err := checkLine("reason", f.Reason, maxReason, false); err != nil {
		return finished{}, err
	}
	if err := f.Handoff.validate(); err != nil {
		return finished{}, err
	}
	if err := validIdem(f.IdempotencyKey); err != nil {
		return finished{}, err
	}
	if err := f.Accept.validate(s.opts.Limits.AcceptanceItems); err != nil {
		return finished{}, err
	}
	if len(f.Discovered) > MaxDiscovered {
		return finished{}, fmt.Errorf("%w: at most %d discovered issues", ErrInvalid, MaxDiscovered)
	}
	paths, err := checkPaths(f.Paths, false)
	if err != nil {
		return finished{}, err
	}
	// Paths are what the client's git showed when it sent the request,
	// so a retry may carry others; they are not part of the request an
	// idempotency key stands for.
	f.Paths = nil
	// normalize fills in defaults in place, so work on a copy: the
	// caller's slice shares its backing array with f.Discovered.
	f.Discovered = slices.Clone(f.Discovered)
	ids := make([]IssueID, len(f.Discovered))
	metas := make([]any, len(f.Discovered))
	for i := range f.Discovered {
		d := &f.Discovered[i]
		if d.ID != "" || d.IdempotencyKey != "" {
			return finished{}, fmt.Errorf("%w: a discovered issue takes a generated id", ErrInvalid)
		}
		if err := d.normalize(); err != nil {
			return finished{}, err
		}
		var err error
		if metas[i], err = nullJSON(d.Metadata); err != nil {
			return finished{}, err
		}
		if ids[i], err = NewID(s.opts.Prefix); err != nil {
			return finished{}, err
		}
	}
	// The request as the client sent it, before generated IDs.
	req := struct {
		ID    IssueID
		Epoch int64
		F     Finish
	}{id, epoch, f}
	var out finished
	err = s.write(ctx, actor, func(w *wtx) error {
		if done, err := replay(ctx, w, f.IdempotencyKey, "finish", req, &out); done || err != nil {
			return err
		}
		before, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		c, err := loadClaim(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if why := moved(w, before, c, epoch, true); late && why != "" {
			for i, d := range f.Discovered {
				if err := fileDiscovered(ctx, w, id, ids[i], d, metas[i]); err != nil {
					return err
				}
			}
			cid, err := keepLate(ctx, w, before, c, epoch, "finish", why, finishText(f))
			if err != nil {
				return err
			}
			out = finished{Issue: before, IDs: ids, Comment: cid}
			return w.settle(out)
		}
		if before.Status == StatusClosed {
			return &StateError{ID: id, Reason: StateAlreadyClosed}
		}
		if err := w.guard(ctx, c, "finish"); err != nil {
			return err
		}
		if err := checkEpoch(c, epoch); err != nil {
			return err
		}
		lapsed, err := markLapsed(ctx, w, c, epoch, "finish")
		if err != nil {
			return err
		}
		if err := recordCommitPaths(ctx, w, id, paths); err != nil {
			return err
		}
		if !f.Accept.empty() {
			if _, err := applyAcceptance(ctx, w, before, f.Accept); err != nil {
				return err
			}
		}
		for i, d := range f.Discovered {
			if err := fileDiscovered(ctx, w, id, ids[i], d, metas[i]); err != nil {
				return err
			}
		}
		if f.Handoff.Note != "" {
			if err := insertHandoff(ctx, w, id, f.Handoff); err != nil {
				return err
			}
		}
		closed, err := closeTx(ctx, w, before, f.Reason, false)
		if err != nil {
			return err
		}
		out = finished{Issue: closed, IDs: ids, Lapsed: lapsed}
		return w.settle(out)
	})
	if err != nil {
		return finished{}, err
	}
	return out, nil
}

// fileDiscovered files d, work found while doing issue from, as issue
// id, discovered from it.
func fileDiscovered(ctx context.Context, w *wtx, from, id IssueID, d NewIssue, meta any) error {
	if _, err := insertIssue(ctx, w, id, d, meta); err != nil {
		return fmt.Errorf("discovered %q: %w", d.Title, err)
	}
	return insertDep(ctx, w, id, from, DepDiscoveredFrom)
}

// HandoffIssue records a handoff note, with its fields, on an issue
// without closing it, telling the principal it is handed to and those its
// note mentions. With release it also lets the issue go, so another can
// start it: the claim ends, in_progress becomes open and the assignee is
// cleared. paths, the paths the issue's work touched, most recent first,
// are recorded as its commit paths. With an idempotency key (idem), a
// repeat returns the first result and writes nothing. HandoffIssue
// refuses:
//   - a handoff on an issue another principal holds, with a
//     [*ForbiddenError], unless the actor is an admin ([Store.AddComment]
//     needs no hold);
//   - a release naming a stale epoch, with a [*StaleEpochError];
//   - an idempotency key reused for another request, with an
//     [*IdemError];
//   - a release of a closed issue, or invalid input, with ErrInvalid.
func (s *Store) HandoffIssue(ctx context.Context, actor Actor, id IssueID, epoch int64, h HandoffNote, release bool, idem string, paths []string) (Issue, error) {
	out, err := s.handoff(ctx, actor, id, epoch, h, release, idem, paths, false)
	if err != nil {
		return Issue{}, err
	}
	return out.Issue, nil
}

// handedOff is HandoffIssue's result, as its idempotency stamp stores
// it: the issue, with Lapsed and Comment for a replayed handoff
// ([Store.ReplayHandoff]). The issue is inline, so that a stamp written
// before those fields reads as it did.
type handedOff struct {
	Issue
	Lapsed  bool   `json:"lapsed,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// handoff is HandoffIssue, and with late [Store.ReplayHandoff].
func (s *Store) handoff(ctx context.Context, actor Actor, id IssueID, epoch int64, h HandoffNote, release bool, idem string,
	paths []string, late bool) (handedOff, error) {
	if err := id.Validate(); err != nil {
		return handedOff{}, err
	}
	if err := validIdem(idem); err != nil {
		return handedOff{}, err
	}
	req := struct {
		ID      IssueID
		Epoch   int64
		Note    HandoffNote
		Release bool
	}{id, epoch, h, release}
	if err := validBody(h.Note); err != nil {
		return handedOff{}, err
	}
	if err := h.HandoffFields.validate(); err != nil {
		return handedOff{}, err
	}
	paths, err := checkPaths(paths, false)
	if err != nil {
		return handedOff{}, err
	}
	var out handedOff
	err = s.write(ctx, actor, func(w *wtx) error {
		out = handedOff{}
		if done, err := replay(ctx, w, idem, "handoff", req, &out); done || err != nil {
			return err
		}
		before, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		c, err := loadClaim(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if why := moved(w, before, c, epoch, release); late && why != "" {
			cid, err := keepLate(ctx, w, before, c, epoch, "handoff", why, handoffText(h))
			if err != nil {
				return err
			}
			out = handedOff{Issue: before, Comment: cid}
			return w.settle(out)
		}
		if err := w.guard(ctx, c, "handoff"); err != nil {
			return err
		}
		if release {
			if before.Status == StatusClosed {
				return &StateError{ID: id, Reason: StateClosed}
			}
			if err := checkEpoch(c, epoch); err != nil {
				return err
			}
		}
		if out.Lapsed, err = markLapsed(ctx, w, c, epoch, "handoff"); err != nil {
			return err
		}
		if err := recordCommitPaths(ctx, w, id, paths); err != nil {
			return err
		}
		if release {
			if err := w.ended(ctx, c, "released"); err != nil {
				return err
			}
			if err := endClaim(ctx, w, c); err != nil {
				return err
			}
		}
		if err := insertHandoff(ctx, w, id, h); err != nil {
			return err
		}
		out.Issue = before
		if !release || (before.Status != StatusInProgress && before.Assignee == "") {
			return w.settle(out)
		}
		status := before.Status
		if status == StatusInProgress {
			status = StatusOpen
		}
		if out.Issue, err = casUpdate(ctx, w, before, []string{"status", "assignee"}, []any{string(status), nil}); err != nil {
			return err
		}
		if err := w.issueEvent(ctx, OpIssueUpdate, before, out.Issue, nil); err != nil {
			return err
		}
		return w.settle(out)
	})
	if err != nil {
		return handedOff{}, err
	}
	return out, nil
}
