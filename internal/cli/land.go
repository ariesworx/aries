package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
)

const landUsage = "land [--from COMMIT] [--branch NAME] [--dry-run] [--hook]"

// maxLand bounds the commits one `aries land` reads.
const maxLand = 500

// landDoc is `aries land --json`.
type landDoc struct {
	Reported []landReport `json:"reported"`
	Skipped  []landSkip   `json:"skipped"`
	Head     string       `json:"head,omitempty"`
	DryRun   bool         `json:"dry_run,omitempty"`
}

type landReport struct {
	Commit string `json:"commit"`
	Issue  string `json:"issue"`
	Gate   string `json:"gate,omitempty"`
}

type landSkip struct {
	Commit string `json:"commit"`
	Reason string `json:"reason"`
}

// cmdLand reports `landed` for each commit of the default branch since
// the last it reported (design §21.8), for a server that CI cannot reach.
// The commit names its issue as `gate pass --check landed --from-commit`
// reads it. It is a person's act: the report is made from the session
// cli, whatever the environment says. The last commit reported is kept
// in the data directory, per server and project; nothing is kept for one
// that did not reach the server, and a report of a gate already passed
// changes nothing, so a rerun, or a second machine, is harmless.
func cmdLand(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("land")
	var from, branch string
	var dry, hook bool
	fs.StringVar(&from, "from", "", "report the commits after COMMIT, in place of the ones after the last reported")
	fs.StringVar(&branch, "branch", "", "the default branch's name, when git cannot tell")
	fs.BoolVar(&dry, "dry-run", false, "say what would be reported; report and keep nothing")
	fs.BoolVar(&hook, "hook", false, "run as git's post-merge hook: say nothing off the default branch, and exit 0 whatever happens")
	pos, err := parse(fs, args, landUsage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(landUsage, "land takes no arguments")
	}
	r.session = client.CLISession
	if !hook {
		return r.land(ctx, from, branch, dry, false)
	}
	r.hook = true
	if err := r.land(ctx, from, branch, dry, true); err != nil {
		pe, _ := errors.AsType[*proto.Error](err)
		msg := esc(err.Error())
		if pe != nil && pe.Fix != "" {
			msg += " (fix: " + esc(pe.Fix) + ")"
		}
		_, _ = fmt.Fprintf(r.env.Stderr, "aries land: %s\n", msg)
	}
	return nil
}

// land is cmdLand after its flags. quietOff says to return quietly when
// the checkout is not on the default branch.
func (r *runner) land(ctx context.Context, from, branch string, dry, quietOff bool) error {
	cur, err := gitx.CurrentBranch(ctx, r.dir)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "run it in a checkout of the project's repository", fmt.Sprintf("read the branch: %v", err))
	}
	def := branch
	if def == "" {
		if def, err = gitx.DefaultBranch(ctx, r.dir); err != nil {
			return proto.Errf(proto.CodeInvalid, "give it with --branch NAME", fmt.Sprintf("read the default branch: %v", err))
		}
	}
	switch {
	case def == "":
		return proto.Errf(proto.CodeInvalid, "give it with --branch NAME", "git does not say which branch is the default")
	case cur != def && quietOff:
		return nil
	case cur != def:
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("`git switch %s` and pull it, then run aries land again", def),
			fmt.Sprintf("aries land reports what the default branch (%s) holds, and this checkout is on %s", def, branchText(cur)))
	}
	local, _, err := r.away().Local()
	if err != nil {
		return err
	}
	if from == "" {
		if from, err = local.Landed(); err != nil {
			return err
		}
	}
	commits, err := gitx.Since(ctx, r.dir, from, maxLand)
	switch {
	case gitx.IsUnknownCommit(err):
		return proto.Errf(proto.CodeInvalid, "give an earlier commit with --from COMMIT",
			fmt.Sprintf("the commit after which to read, %s, is not in this repository", short(from)))
	case err != nil:
		return proto.Errf(proto.CodeInvalid, "give a nearer commit with --from COMMIT", err.Error())
	}
	doc := landDoc{Reported: []landReport{}, Skipped: []landSkip{}, DryRun: dry}
	if len(commits) > 0 {
		doc.Head = commits[len(commits)-1].SHA
	}
	if !r.json && from == "" && len(commits) > 0 {
		r.notef("nothing was reported before: reading HEAD alone; --from COMMIT reports the commits after COMMIT")
	}
	for _, c := range commits {
		id, br, ok := gitx.IssueOfMessage(c.Message)
		if !ok {
			doc.Skipped = append(doc.Skipped, landSkip{short(c.SHA), "it names no issue: it has no Aries: trailer and merged no branch"})
			r.keepPlace(local, c.SHA, dry)
			continue
		}
		target := id
		if target == "" {
			target = br
		}
		if dry {
			doc.Reported = append(doc.Reported, landReport{Commit: short(c.SHA), Issue: target})
			continue
		}
		var out proto.Gate
		err := r.call(ctx, proto.OpGatePass, proto.GateArgs{Check: "landed", Subject: id, Branch: br, SHA: c.SHA}, &out)
		if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeNotFound {
			doc.Skipped = append(doc.Skipped, landSkip{short(c.SHA), pe.Message})
			r.keepPlace(local, c.SHA, dry)
			continue
		}
		if err != nil {
			r.printLand(doc)
			return err
		}
		doc.Reported = append(doc.Reported, landReport{Commit: short(c.SHA), Issue: cmp.Or(out.Subject, target), Gate: out.ID})
		r.keepPlace(local, c.SHA, dry)
	}
	r.printLand(doc)
	if len(commits) == 0 && !r.json {
		if from == "" {
			_, _ = fmt.Fprintln(r.env.Stdout, "nothing to report: the repository has no commits")
		} else {
			_, _ = fmt.Fprintf(r.env.Stdout, "nothing new since %s\n", short(from))
		}
	}
	return nil
}

// keepPlace keeps sha as the last commit reported, unless this is a dry
// run. A failure to keep it costs a rerun that reports it again, which
// changes nothing, so it is a note.
func (r *runner) keepPlace(l *offline.Local, sha string, dry bool) {
	if dry {
		return
	}
	if err := l.SetLanded(sha); err != nil {
		r.notef("could not keep the last commit landed on this machine, so the next aries land reads it again: %s", esc(err.Error()))
	}
}

// printLand prints what land reported and skipped: with --json one
// document, else a line on Stdout for each report and a note on Stderr
// for each commit skipped.
func (r *runner) printLand(d landDoc) {
	if r.json {
		r.emit(d)
		return
	}
	verb := "landed"
	if d.DryRun {
		verb = "would report landed"
	}
	for _, c := range d.Reported {
		line := fmt.Sprintf("%s %s %s", c.Commit, esc(c.Issue), verb)
		if c.Gate != "" {
			line = fmt.Sprintf("%s %s %s (gate %s)", c.Commit, esc(c.Issue), verb, esc(c.Gate))
		}
		_, _ = fmt.Fprintln(r.env.Stdout, line)
	}
	for _, s := range d.Skipped {
		_, _ = fmt.Fprintf(r.env.Stderr, "aries: skipped %s: %s\n", s.Commit, esc(s.Reason))
	}
}

// short is a commit hash as people read it.
func short(sha string) string { return sha[:min(len(sha), 12)] }

func branchText(b string) string {
	if b == "" {
		return "no branch (a detached HEAD)"
	}
	return b
}
