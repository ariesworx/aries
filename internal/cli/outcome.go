package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/outcome"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/secretscan"
)

const outcomeUsage = "outcome ID... | --since 24h|7d|DATE|TIME [--backfill] [--dry-run] [--workflow NAME]..."

// maxOutcomeComment is the most an outcome comment may be: a comment's
// cap (store/text.go). A record is a few KB.
const maxOutcomeComment = 64 << 10

// outcomeDoc is `aries outcome --json`.
type outcomeDoc struct {
	Records  []outcomeRow `json:"records"`
	Unparsed int          `json:"unparsed,omitempty"`
	DryRun   bool         `json:"dry_run,omitempty"`
}

type outcomeRow struct {
	Issue  string `json:"issue"`
	Seq    int    `json:"seq"`
	Action string `json:"action"`
	// Comment is the comment posted, empty for unchanged and in a dry run.
	Comment    string         `json:"comment,omitempty"`
	Supersedes string         `json:"supersedes,omitempty"`
	Record     outcome.Record `json:"record"`
}

// cmdOutcome captures the outcome record of issues that reached an
// outcome, and posts each once as an `outcome:` comment
// (docs/design/outcome-record.md, slice O1). It is experimental and CLI
// only: it reads the CI host with this machine's own `gh`, which no MCP
// tool may do. A record already posted is posted again only when the
// capture now says something else, as a new comment that supersedes it.
func cmdOutcome(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("outcome")
	var since string
	var backfill, dry bool
	var workflows listFlag
	fs.StringVar(&since, "since", "", "every issue that reached an outcome since then: a duration back from now (24h, 7d), a date or a time")
	fs.BoolVar(&backfill, "backfill", false, "mark the records as captured after the fact (with --since)")
	fs.BoolVar(&dry, "dry-run", false, "print the records; post nothing")
	fs.Var(&workflows, "workflow", "count only this workflow's runs as CI attempts (repeatable, or comma-separated; default every workflow)")
	pos, err := parse(fs, args, outcomeUsage)
	if err != nil {
		return err
	}
	switch {
	case since == "" && len(pos) == 0:
		return usagef(outcomeUsage, "outcome needs issue ids or --since")
	case since != "" && len(pos) > 0:
		return usagef(outcomeUsage, "give issue ids or --since, not both")
	case backfill && since == "":
		return usagef(outcomeUsage, "--backfill needs --since")
	}
	var from time.Time
	if since != "" {
		if from, err = sinceTime(since, r.env.Now()); err != nil {
			return usagef(outcomeUsage, "--since: %v", err)
		}
	}
	st := &outcomeStore{r: r, shows: map[string]proto.ShowResult{}}
	c := &outcome.Capture{Store: st, Repo: r.dir, Now: r.env.Now, Backfill: backfill}
	var host outcome.Host
	switch _, err := exec.LookPath("gh"); {
	case r.env.OutcomeHost != nil:
		host = r.env.OutcomeHost(r.dir)
	case err == nil:
		host = &outcome.GitHub{Dir: r.dir, Workflows: workflows}
	case !r.json:
		r.notef("gh is not on PATH: the change, CI, merges and reviews are recorded as missing")
	}
	c.Host = host
	ids := pos
	if since != "" {
		if ids, err = st.closedSince(ctx, from); err != nil {
			return err
		}
		if host != nil {
			closed, err := host.Closed(ctx, from)
			if err != nil {
				return proto.Errf(proto.CodeInvalid, "check that `gh auth status` passes here, or run without gh on PATH",
					fmt.Sprintf("read the closed pull requests: %v", err))
			}
			for _, ch := range closed {
				if id, ok := st.issueOfBranch(ctx, ch.Branch); ok && !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	}
	doc := outcomeDoc{Records: []outcomeRow{}, DryRun: dry}
	var refused error
	for _, id := range ids {
		got, err := c.Issue(ctx, id)
		if err != nil {
			return err
		}
		doc.Unparsed += got.Unparsed
		if got.Unparsed > 0 && !r.json {
			r.notef("%s has %d outcome: comments that do not parse, each a failed capture", esc(id), got.Unparsed)
		}
		if len(got.Records) == 0 && since == "" && !r.json {
			_, _ = fmt.Fprintf(r.env.Stdout, "%s: no outcome yet (it lands, closes, or its change closes unmerged)\n", esc(id))
		}
		for _, rec := range got.Records {
			if since != "" && rec.Times.OutcomeAt.Before(from) {
				continue
			}
			row, err := r.postOutcome(ctx, rec, got.Posted, dry)
			if err != nil {
				refused = errors.Join(refused, err)
				continue
			}
			doc.Records = append(doc.Records, row)
		}
	}
	if r.json {
		r.emit(doc)
	} else {
		r.printOutcomes(doc)
	}
	return refused
}

// postOutcome plans one record against those posted and posts it unless
// it is unchanged or this is a dry run.
func (r *runner) postOutcome(ctx context.Context, rec outcome.Record, posted []outcome.Posted, dry bool) (outcomeRow, error) {
	action, sup := outcome.Plan(rec, posted)
	rec.Supersedes = sup
	row := outcomeRow{Issue: rec.Issue, Seq: rec.Seq, Action: action, Supersedes: sup, Record: rec}
	if action == "unchanged" || dry {
		return row, nil
	}
	body, err := rec.Comment()
	if err != nil {
		return row, err
	}
	if len(body) > maxOutcomeComment {
		return row, proto.Errf(proto.CodeInvalid, "report it as a bug with the issue's id",
			fmt.Sprintf("the record of %s seq %d is %d bytes, more than a comment holds", rec.Issue, rec.Seq, len(body)))
	}
	if f, found := secretscan.Find(body); found {
		return row, proto.Errf(proto.CodeInvalid, "report it as a bug with the issue's id; the record is not posted",
			fmt.Sprintf("the record of %s seq %d looks like it holds a secret (%s)", rec.Issue, rec.Seq, f))
	}
	var out proto.CommentResult
	if err := r.call(ctx, proto.OpComment, proto.CommentArgs{ID: rec.Issue, Body: body, Idem: proto.NewIdem("cli")}, &out); err != nil {
		return row, err
	}
	row.Comment = out.ID
	return row, nil
}

func (r *runner) printOutcomes(doc outcomeDoc) {
	if len(doc.Records) == 0 {
		_, _ = fmt.Fprintln(r.env.Stdout, "no outcome to record")
		return
	}
	for _, row := range doc.Records {
		head := fmt.Sprintf("%s seq %d %s", esc(row.Issue), row.Seq, row.Record.Outcome)
		switch {
		case row.Action == "unchanged":
			_, _ = fmt.Fprintf(r.env.Stdout, "%s: unchanged, nothing posted\n", head)
		case doc.DryRun:
			body, err := row.Record.Comment()
			if err != nil {
				body = err.Error()
			}
			verb := "would post"
			if row.Supersedes != "" {
				verb = "would supersede " + esc(row.Supersedes) + " with"
			}
			_, _ = fmt.Fprintf(r.env.Stdout, "%s: %s\n", head, verb)
			for line := range strings.SplitSeq(body, "\n") {
				_, _ = fmt.Fprintln(r.env.Stdout, esc(line))
			}
		case row.Supersedes != "":
			_, _ = fmt.Fprintf(r.env.Stdout, "%s: posted %s, superseding %s\n", head, esc(row.Comment), esc(row.Supersedes))
		default:
			_, _ = fmt.Fprintf(r.env.Stdout, "%s: posted %s\n", head, esc(row.Comment))
		}
	}
}

// sinceTime reads --since as digest does: a duration back from now
// (7d, 2w, 36h), a date (midnight UTC) or a time.
func sinceTime(s string, now time.Time) (time.Time, error) {
	if d, err := proto.ParseDuration(s); err == nil {
		return now.Add(-d).UTC(), nil
	}
	return proto.ParseDate(s)
}

// outcomeStore reads Aries for a capture through the ops a client
// already sends, keeping each issue's show for the capture.
type outcomeStore struct {
	r     *runner
	shows map[string]proto.ShowResult
}

func (s *outcomeStore) Show(ctx context.Context, id string) (proto.ShowResult, error) {
	if v, ok := s.shows[id]; ok {
		return v, nil
	}
	var out proto.ShowResult
	if err := s.r.call(ctx, proto.OpShow, proto.ShowArgs{ID: id}, &out); err != nil {
		return proto.ShowResult{}, err
	}
	s.shows[id] = out
	return out, nil
}

func (s *outcomeStore) History(ctx context.Context, id string) ([]proto.Event, error) {
	evs, _, err := pages(0, func(before string, limit int) ([]proto.Event, string, int, error) {
		var out proto.HistoryResult
		err := s.r.call(ctx, proto.OpHistory, proto.PageArgs{ID: id, Before: before, Limit: limit}, &out)
		return out.Events, out.Earlier, out.Total, err
	})
	return evs, err
}

func (s *outcomeStore) Comments(ctx context.Context, id string) ([]proto.Comment, error) {
	cs, _, err := pages(0, func(before string, limit int) ([]proto.Comment, string, int, error) {
		var out proto.CommentsResult
		err := s.r.call(ctx, proto.OpComments, proto.PageArgs{ID: id, Before: before, Limit: limit}, &out)
		return out.Comments, out.Earlier, out.Total, err
	})
	return cs, err
}

func (s *outcomeStore) Cost(ctx context.Context, since, until time.Time) (proto.CostResult, error) {
	var out proto.CostResult
	err := s.r.call(ctx, proto.OpCost, proto.CostArgs{By: "issue", Since: since.UTC().Format(time.RFC3339Nano),
		Until: until.UTC().Format(time.RFC3339Nano), Limit: 500}, &out)
	return out, err
}

// closedSince lists the issues closed since from: every closed issue,
// a page at a time, and the time each closed, which only show gives.
func (s *outcomeStore) closedSince(ctx context.Context, from time.Time) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		var out proto.ListResult
		if err := s.r.call(ctx, proto.OpList, proto.ListArgs{Status: []string{"closed"}, Limit: 500, Cursor: cursor}, &out); err != nil {
			return nil, err
		}
		for _, is := range out.Issues {
			sh, err := s.Show(ctx, is.ID)
			if err != nil {
				return nil, err
			}
			if at := sh.Issue.ClosedAt; at != nil && !at.Before(from) {
				ids = append(ids, is.ID)
			}
		}
		if out.Next == "" {
			return ids, nil
		}
		cursor = out.Next
	}
}

// issueOfBranch is the issue a change's branch names (gitx.IDFromBranch,
// for a project prefix without a hyphen), when the server knows it.
func (s *outcomeStore) issueOfBranch(ctx context.Context, branch string) (string, bool) {
	id, ok := gitx.IDFromBranch(branch, "")
	if !ok {
		return "", false
	}
	if _, err := s.Show(ctx, id); err != nil {
		return "", false
	}
	return id, true
}
