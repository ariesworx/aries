package store

import (
	"context"
	"fmt"
)

// IssueView is all that a show of an issue reads from the store.
type IssueView struct {
	Issue Issue
	// Deps are the edges into and out of the issue, as [Store.Deps].
	Deps []Dep
	// Claim is the live claim on the issue, or nil, as [Store.ClaimOf].
	Claim *Claim
	// Handoff is the latest handoff note, or nil, as [Store.LastHandoff].
	Handoff *Handoff
	// Items are the acceptance items, as [Store.AcceptanceItems].
	Items []AcceptanceItem
	// Usage is as [Store.IssueUsage].
	Usage IssueUsage
	// Files are the issue's likely files, as [Store.IssueFiles] with n.
	Files Files
	// Brief is the issue's latest brief that was not replaced, or nil
	// (design §22.3).
	Brief *Brief
}

// ShowIssue reads an issue and what is shown beside it, all from one
// snapshot and as of one time, so that no part pairs the issue with a
// change made after another part was read. Each part is what the read
// named in its field gives; actor and n are IssueFiles'. A missing issue
// is ErrNotFound.
func (s *Store) ShowIssue(ctx context.Context, actor Actor, id IssueID, n int) (IssueView, error) {
	if err := id.Validate(); err != nil {
		return IssueView{}, err
	}
	q, end, err := s.beginRead(ctx)
	if err != nil {
		return IssueView{}, fmt.Errorf("show %s: %w", id, err)
	}
	defer end()
	now := s.now()
	var v IssueView
	if v.Issue, err = loadIssue(ctx, q, id); err != nil {
		return IssueView{}, err
	}
	if v.Deps, err = issueDeps(ctx, q, id); err != nil {
		return IssueView{}, err
	}
	c, err := loadClaim(ctx, q, id)
	if err != nil {
		return IssueView{}, err
	}
	if c.active(now) {
		v.Claim = &c.Claim
	}
	if v.Handoff, err = lastHandoff(ctx, q, id); err != nil {
		return IssueView{}, err
	}
	if v.Items, err = acceptanceItems(ctx, q, v.Issue); err != nil {
		return IssueView{}, err
	}
	if v.Usage, err = issueUsage(ctx, q, id, now); err != nil {
		return IssueView{}, err
	}
	if v.Files, err = issueFiles(ctx, q, actor, id, n, now); err != nil {
		return IssueView{}, err
	}
	if v.Brief, err = latestBrief(ctx, q, id); err != nil {
		return IssueView{}, err
	}
	return v, nil
}
