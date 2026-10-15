package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
)

const (
	conflictsUsage = "conflicts [ISSUE] [-n N] [--after N] | conflicts N"
	resolveUsage   = "resolve N --mine|--theirs|--text TEXT|-|--accept"
)

// cmdConflicts lists the open conflicts, of one issue or all (design
// §18.7), or shows one whole by its number.
func cmdConflicts(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("conflicts")
	var in proto.ConflictsArgs
	fs.IntVar(&in.Limit, "n", 0, "how many to list (default 20, at most 100)")
	fs.Int64Var(&in.After, "after", 0, "list those after this conflict, for the next page")
	pos, err := parse(fs, args, conflictsUsage)
	if err != nil {
		return err
	}
	switch len(pos) {
	case 0:
	case 1:
		if n, err := strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64); err == nil {
			if in.Limit != 0 || in.After != 0 {
				return usagef(conflictsUsage, "-n and --after list; they do not go with a conflict's number")
			}
			in.ID = n
		} else {
			in.Issue = pos[0]
		}
	default:
		return usagef(conflictsUsage, "conflicts takes one issue id or conflict number")
	}
	var out proto.ConflictsResult
	if err := r.call(ctx, proto.OpConflicts, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	now := r.env.Now()
	if in.ID != 0 && len(out.Conflicts) == 1 {
		printConflict(r.env.Stdout, out.Conflicts[0], now)
		return nil
	}
	printConflicts(r.env.Stdout, out, now)
	return nil
}

// cmdResolve resolves a conflict: with the update's value, the server's,
// new text, or as its proposal says. It is never queued.
func cmdResolve(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("resolve")
	mine := fs.Bool("mine", false, "take the replayed update's value")
	theirs := fs.Bool("theirs", false, "keep the server's value")
	accept := fs.Bool("accept", false, "resolve as the conflict's proposal says")
	text := fs.String("text", "", "set the field to this text, or - for standard input")
	pos, err := parse(fs, args, resolveUsage)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(resolveUsage, "resolve takes one conflict number; `sfx conflicts` lists them")
	}
	n, err := strconv.ParseInt(strings.TrimPrefix(pos[0], "#"), 10, 64)
	if err != nil || n < 1 {
		return usagef(resolveUsage, "%q is not a conflict number; `sfx conflicts` lists them", pos[0])
	}
	in := proto.ResolveArgs{ID: n}
	textSet := false // --text "" clears a field, so it is the flag that counts
	fs.Visit(func(f *flag.Flag) { textSet = textSet || f.Name == "text" })
	for _, c := range []struct {
		on     bool
		choice string
	}{{*mine, proto.ChoiceMine}, {*theirs, proto.ChoiceTheirs}, {*accept, proto.ChoiceAccept}, {textSet, proto.ChoiceText}} {
		if c.on {
			if in.Choice != "" {
				return usagef(resolveUsage, "give one of --mine, --theirs, --text and --accept")
			}
			in.Choice = c.choice
		}
	}
	if in.Choice == "" {
		return usagef(resolveUsage, "give one of --mine, --theirs, --text and --accept")
	}
	if in.Choice == proto.ChoiceText {
		if in.Text, err = r.text(*text); err != nil {
			return err
		}
	}
	var out proto.WriteResult
	if err := r.call(ctx, proto.OpResolve, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s rev %d\n", esc(out.ID), out.Rev)
	return nil
}

// conflictHead is a conflict's first line: its number, issue and field,
// who parked it from which rev, and whose change it met.
func conflictHead(c proto.Conflict, now time.Time) string {
	return fmt.Sprintf("#%d %s %s: %s's update from rev %d, parked %s ago, against %s's of %s ago",
		c.ID, esc(c.Issue), esc(c.Field), esc(c.By), c.BaseRev, proto.Span(now.Sub(c.ParkedAt)),
		esc(c.TheirsBy), proto.Span(now.Sub(c.TheirsAt)))
}

// printConflicts lists conflicts, each side on one line, escaped, as the
// server cut them.
func printConflicts(w io.Writer, out proto.ConflictsResult, now time.Time) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	if len(out.Conflicts) == 0 {
		p("no open conflicts\n")
		return
	}
	for _, c := range out.Conflicts {
		p("%s\n", conflictHead(c, now))
		p("  base:   %s\n  mine:   %s\n  theirs: %s\n", esc(c.Base), esc(c.Mine), esc(c.Theirs))
		if c.Advice != "" {
			p("  advice: %s\n", esc(c.Advice))
		}
		if pr := c.Proposal; pr != nil {
			p("  %s\n", proposalLine(pr))
		}
	}
	if out.Next != 0 {
		p("more: `sfx conflicts --after %d`\n", out.Next)
	}
	p("resolve with `sfx resolve N --mine|--theirs|--text TEXT|--accept`; `sfx conflicts N` shows one whole\n")
}

// proposalLine says what a proposal is, by whom, and why.
func proposalLine(pr *proto.Proposal) string {
	line := fmt.Sprintf("proposed by %s: %s", esc(pr.By), esc(pr.Choice))
	if pr.Choice == proto.ChoiceText {
		line += ": " + esc(pr.Text)
	}
	if pr.Why != "" {
		line += " (" + esc(pr.Why) + ")"
	}
	return line
}

// printConflict shows one conflict with each side whole, on lines of its
// own, so a person can compare them or feed them to a diff.
func printConflict(w io.Writer, c proto.Conflict, now time.Time) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("%s\n", conflictHead(c, now))
	if c.ResolvedAt != nil {
		p("resolved: %s, by %s %s ago\n", esc(c.Resolution), esc(c.ResolvedBy), proto.Span(now.Sub(*c.ResolvedAt)))
	}
	if c.Advice != "" {
		p("advice: %s\n", esc(c.Advice))
	}
	if pr := c.Proposal; pr != nil {
		p("%s\n", proposalLine(pr))
	}
	for _, s := range []struct{ head, text string }{
		{fmt.Sprintf("base (rev %d)", c.BaseRev), c.Base},
		{"mine (" + esc(c.By) + ")", c.Mine},
		{"theirs (" + esc(c.TheirsBy) + ")", c.Theirs},
	} {
		p("--- %s\n%s\n", s.head, safetext.Text(s.text))
	}
}
