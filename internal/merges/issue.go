package merges

import (
	"context"
	"slices"
	"strings"

	"github.com/ariesworx/aries/internal/proto"
)

// Store reads the issues a verdict needs, through the ops a client sends.
type Store interface {
	Show(ctx context.Context, id string) (proto.ShowResult, error)
	Comments(ctx context.Context, id string) ([]proto.Comment, error)
}

// maxDepth is the most parents ReadIssue climbs.
const maxDepth = 8

// maxBrief is the most of a brief's first line a verdict carries.
const maxBrief = 160

// ReadIssue reads issue id for the epic and opener conditions: whether
// it, or an epic above it, has a human gate that blocks it and passed
// (the workflow's approval, decision 68), and no open gate holds it; and
// the fields of its last `brief:` comment. Anything it cannot read is a
// reason it is not approved.
func ReadIssue(ctx context.Context, s Store, id string) Issue {
	is := Issue{ID: id}
	sh, err := s.Show(ctx, id)
	if err != nil {
		is.Err = err.Error()
		return is
	}
	if cs, err := s.Comments(ctx, id); err == nil {
		is.Brief = lastBrief(cs)
	}
	if len(sh.Gates) > 0 {
		is.Epic.Detail = "open gate " + sh.Gates[0].ID + " holds " + id
		return is
	}
	seen := []string{}
	for range maxDepth {
		cur := sh.Issue.ID
		if slices.Contains(seen, cur) {
			break
		}
		seen = append(seen, cur)
		if sh.Issue.Type == "epic" {
			g, detail := approval(ctx, s, sh)
			if detail != "" {
				is.Epic.Detail = detail
				return is
			}
			if g != "" {
				is.Epic = Epic{Approved: true, Detail: "epic " + cur + ", gate " + g + " passed"}
				return is
			}
		}
		if sh.Issue.Parent == "" {
			break
		}
		next, err := s.Show(ctx, sh.Issue.Parent)
		if err != nil {
			is.Epic.Detail = sh.Issue.Parent + " unreadable: " + err.Error()
			return is
		}
		sh = next
	}
	is.Epic.Detail = "no epic above " + id + " has a passed approval gate"
	return is
}

// approval returns the passed human gate that blocks epic, or "" with
// none; detail is set when it cannot tell.
func approval(ctx context.Context, s Store, epic proto.ShowResult) (gate, detail string) {
	for _, d := range epic.Deps {
		if d.From != epic.Issue.ID || d.Type != "blocks" {
			continue
		}
		g, err := s.Show(ctx, d.To)
		if err != nil {
			return "", d.To + " unreadable: " + err.Error()
		}
		if g.Gate != nil && g.Gate.Kind == "human" && g.Gate.Outcome == "passed" {
			return d.To, ""
		}
	}
	if epic.DepsMore > 0 {
		return "", "epic " + epic.Issue.ID + " has more dependencies than show lists"
	}
	return "", ""
}

// lastBrief is the fields of the last comment whose first line begins
// "brief: ", cut to maxBrief.
func lastBrief(cs []proto.Comment) string {
	for _, c := range slices.Backward(cs) {
		first, _, _ := strings.Cut(c.Body, "\n")
		if f, ok := strings.CutPrefix(first, "brief: "); ok {
			f = strings.TrimSpace(f)
			if len(f) > maxBrief {
				f = f[:maxBrief]
			}
			return f
		}
	}
	return ""
}
