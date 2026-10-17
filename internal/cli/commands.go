package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
)

// maxStdin bounds text read from standard input with "-".
const maxStdin = 1 << 20

// text returns v or, when v is "-", standard input without its trailing
// newlines. Input longer than maxStdin is a usage error.
func (r *runner) text(v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.env.Stdin, maxStdin+1))
	if err != nil {
		return "", fmt.Errorf("read standard input: %w", err)
	}
	if len(b) > maxStdin {
		return "", usagef("", "standard input is larger than 1 MiB")
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// fields are the issue fields create and update share.
type fields struct {
	title, body, design, acceptance, notes, status, typ, assignee, owner, parent, prio, account string
	deferUntil, due                                                                             string
}

// register adds the field flags to fs. withTitle adds --title, for
// update; create takes its title as arguments.
func (f *fields) register(fs *flag.FlagSet, withTitle bool) {
	if withTitle {
		fs.StringVar(&f.title, "title", "", "title")
	}
	fs.StringVar(&f.body, "body", "", "description, or - for standard input")
	fs.StringVar(&f.design, "design", "", "design notes, or -")
	fs.StringVar(&f.acceptance, "acceptance", "", "acceptance criteria, or -")
	fs.StringVar(&f.notes, "notes", "", "notes, or -")
	fs.StringVar(&f.status, "status", "", "open, blocked or deferred (`aries start` sets in_progress)")
	fs.StringVar(&f.typ, "t", "", "type: bug, feature, task, epic or chore")
	fs.StringVar(&f.typ, "type", "", "type")
	fs.StringVar(&f.prio, "p", "", "priority 0-4")
	fs.StringVar(&f.prio, "priority", "", "priority 0-4")
	fs.StringVar(&f.assignee, "assignee", "", "assignee (empty clears)")
	fs.StringVar(&f.owner, "owner", "", "owner (empty clears)")
	fs.StringVar(&f.parent, "parent", "", "parent issue id (empty clears)")
	fs.StringVar(&f.account, "account", "", "account code name for time and tokens (empty inherits the parent's)")
	fs.StringVar(&f.deferUntil, "defer-until", "", "keep it out of ready until this date (2026-11-02) or time (RFC 3339); empty clears")
	fs.StringVar(&f.due, "due", "", "due by this date or time; empty clears")
}

// checkDates refuses, as a usage error, a --defer-until or --due that is
// no date or time, before anything is sent.
func (f *fields) checkDates(usage string) error {
	for _, d := range []struct{ name, v string }{{"defer-until", f.deferUntil}, {"due", f.due}} {
		if _, err := proto.ParseDate(d.v); err != nil {
			return usagef(usage, "--%s: %v", d.name, err)
		}
	}
	return nil
}

// readTexts resolves "-" in the long text fields; only one may use it.
func (r *runner) readTexts(f *fields) error {
	n := 0
	for _, p := range []*string{&f.body, &f.design, &f.acceptance, &f.notes} {
		if *p == "-" {
			n++
			if n > 1 {
				return usagef("", "only one field can read standard input")
			}
			v, err := r.text(*p)
			if err != nil {
				return err
			}
			*p = v
		}
	}
	return nil
}

func cmdCreate(ctx context.Context, r *runner, args []string) error {
	const usage = "create TITLE... [-p N] [-t TYPE] [--body TEXT|-] [--parent ID] [--account NAME] [--label L]... [--paths P,P] [--defer-until DATE] [--due DATE]"
	fs := r.newFlags("create")
	var f fields
	f.register(fs, false)
	var labels, paths listFlag
	var id string
	fs.Var(&labels, "label", "label (repeatable, or comma-separated)")
	fs.Var(&paths, "paths", "files the work will touch, relative to here; a directory ends in / (repeatable, or comma-separated)")
	fs.Var(&labels, "l", "label")
	// Hidden: no usage line or guide names --id, which only a script that
	// imports issues needs; it is not part of 1.0's surface (decision 88).
	fs.StringVar(&id, "id", "", "issue id (default: generated)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "create needs a title")
	}
	if err := f.checkDates(usage); err != nil {
		return err
	}
	if err := r.readTexts(&f); err != nil {
		return err
	}
	idem := proto.NewIdem("cli")
	in := proto.CreateArgs{ID: id, Idem: idem, Title: strings.Join(pos, " "), Body: f.body, Design: f.design,
		Acceptance: f.acceptance, Notes: f.notes, Status: f.status, Type: f.typ, Assignee: f.assignee,
		Owner: f.owner, Parent: f.parent, Labels: labels, Account: f.account, DeferUntil: f.deferUntil, Due: f.due}
	if len(paths) > 0 {
		in.Paths = gitx.RepoRelative(ctx, r.dir, paths)
	}
	if f.prio != "" {
		p, err := priority(f.prio)
		if err != nil {
			return usagef(usage, "%v", err)
		}
		in.Priority = &p
	}
	var out proto.CreateResult
	kept, err := r.send(ctx, proto.OpCreate, in, &out)
	if err != nil {
		return err
	}
	if kept != nil {
		r.queued(kept)
		return nil
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintln(r.env.Stdout, esc(out.ID))
	// Standard error, so `id=$(aries create …)` still captures the id alone.
	if out.Items > 0 {
		_, _ = fmt.Fprintf(r.env.Stderr, "the project's workflow added %d acceptance items (`aries show %s`)\n", out.Items, esc(out.ID))
	}
	printSimilar(r.env.Stderr, out.Similar)
	return nil
}

// printSimilar lists similar closed issues, if any.
func printSimilar(w io.Writer, sim []proto.Summary) {
	if len(sim) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "similar closed issues:")
	for _, s := range sim {
		_, _ = fmt.Fprintf(w, "  %s  %s\n", esc(s.ID), esc(s.Title))
	}
}

// printItems prints an issue's acceptance checklist, if it has one.
func printItems(w io.Writer, items []proto.AcceptanceItem) {
	if len(items) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nacceptance:")
	for _, it := range items {
		mark := " "
		switch it.State {
		case "ticked":
			mark = "x"
		case "waived":
			mark = "~"
		}
		line := fmt.Sprintf("  %d [%s] %s", it.N, mark, esc(it.Text))
		switch {
		case it.State == "waived":
			line += fmt.Sprintf(" (waived by %s: %s)", esc(it.By), esc(it.Reason))
		case it.By != "":
			line += " (" + esc(it.By) + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

// one returns the issue id that must be pos's only element; what names
// the command in the usage error.
func one(usage string, pos []string, what string) (string, error) {
	if len(pos) != 1 {
		return "", usagef(usage, "%s needs exactly one issue id", what)
	}
	return pos[0], nil
}

func cmdShow(ctx context.Context, r *runner, args []string) error {
	const usage = "show ID [--compact]"
	fs := r.newFlags("show")
	compact := fs.Bool("compact", false, "cut long text fields")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	id, err := one(usage, pos, "show")
	if err != nil {
		return err
	}
	var out proto.ShowResult
	r.cacheable = true // with the server away, this machine's copy (design §18.6)
	if err := r.call(ctx, proto.OpShow, proto.ShowArgs{ID: id, Full: !*compact}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printIssue(r.env.Stdout, out)
	printIssueUsage(r.env.Stdout, out.Issue.Account, out.Usage)
	printItems(r.env.Stdout, out.Items)
	printHandoff(r.env.Stdout, out.Handoff)
	printFiles(r.env.Stdout, out.Files)
	if len(out.Similar) > 0 {
		_, _ = fmt.Fprintln(r.env.Stdout)
		printSimilar(r.env.Stdout, out.Similar)
	}
	return nil
}

// when formats t for text output: UTC, to the minute.
func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// printIssue prints an issue's header lines, then its text. Single-line
// fields go through esc; the multi-line text is printed as it is, so w
// must escape it, as the runner's Stdout does.
func printIssue(w io.Writer, s proto.ShowResult) {
	is := s.Issue
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("%s  P%d  %s  %s  rev %d\n%s\n", esc(is.ID), is.Priority, esc(is.Status), esc(is.Type), is.Rev, esc(is.Title))
	var who []string
	for _, kv := range [][2]string{{"parent", is.Parent}, {"assignee", is.Assignee}, {"owner", is.Owner}} {
		if kv[1] != "" {
			who = append(who, kv[0]+": "+esc(kv[1]))
		}
	}
	if len(who) > 0 {
		p("%s\n", strings.Join(who, "  "))
	}
	if len(is.Labels) > 0 {
		p("labels: %s\n", strings.Join(escAll(is.Labels), ", "))
	}
	if c := s.Claim; c != nil {
		p("claimed by %s (%s on %s), epoch %d, until %s\n", esc(c.By), esc(c.Session), esc(c.Machine), c.Epoch, when(c.ExpiresAt))
	}
	if len(s.Conflicts) > 0 {
		fields := make([]string, len(s.Conflicts))
		for i, c := range s.Conflicts {
			fields[i] = esc(c.Field)
		}
		p("conflicts: %d open (%s): `aries conflicts %s`\n", len(s.Conflicts), strings.Join(fields, ", "), esc(is.ID))
	}
	var dates []string
	if is.DeferUntil != nil {
		dates = append(dates, "not before "+when(*is.DeferUntil))
	}
	if is.Due != nil {
		dates = append(dates, "due "+when(*is.Due))
	}
	if len(dates) > 0 {
		p("%s\n", strings.Join(dates, "; "))
	}
	if g := s.Gate; g != nil {
		if g.Kind == proto.GateCI {
			p("ci gate: check %s on %s, reported by %s; holds %s\n", esc(g.Check), esc(g.Subject), approverText(g.Approvers),
				strings.Join(escAll(g.Holds), ", "))
			if r := g.Report; r != nil && g.Outcome == "" {
				p("last report: %s%s by %s at %s\n", esc(r.Result), evidenceText(*r), esc(r.By), when(r.At))
			}
		} else {
			p("%s gate, passed by %s; holds %s\n", esc(g.Kind), approverText(g.Approvers), strings.Join(escAll(g.Holds), ", "))
		}
		if g.Outcome != "" {
			p("%s by %s", esc(g.Outcome), esc(g.By))
			if g.Reason != "" {
				p(": %s", esc(g.Reason))
			}
			p("\n")
		}
	}
	for _, g := range s.Gates {
		if g.Kind == proto.GateCI {
			p("held by gate %s until %s reports %s passed on %s\n", esc(g.ID), approverText(g.Approvers), esc(g.Check), esc(g.Subject))
			continue
		}
		p("held by gate %s, which %s may pass: `aries gate pass %s`\n", esc(g.ID), approverText(g.Approvers), esc(g.ID))
	}
	printChecks(w, s)
	p("created %s by %s; updated %s\n", when(is.CreatedAt), esc(is.CreatedBy), when(is.UpdatedAt))
	if is.ClosedAt != nil {
		p("closed %s", when(*is.ClosedAt))
		if is.CloseReason != "" {
			p(": %s", esc(is.CloseReason))
		}
		p("\n")
	}
	var out, in []string
	for _, d := range s.Deps {
		if d.From == is.ID {
			out = append(out, fmt.Sprintf("%s (%s)", esc(d.To), esc(d.Type)))
		} else {
			in = append(in, fmt.Sprintf("%s (%s)", esc(d.From), esc(d.Type)))
		}
	}
	if len(out) > 0 {
		p("depends on: %s\n", strings.Join(out, ", "))
	}
	if len(in) > 0 {
		p("needed by: %s\n", strings.Join(in, ", "))
	}
	if s.DepsMore > 0 {
		p("(%d more dependencies not shown)\n", s.DepsMore)
	}
	if is.Body != "" {
		p("\n%s\n", is.Body)
	}
	acceptance := is.Acceptance
	if len(s.Items) > 0 {
		acceptance = "" // printItems shows it as a checklist
	}
	for _, kv := range [][2]string{{"design", is.Design}, {"acceptance", acceptance}, {"notes", is.Notes}} {
		if kv[1] != "" {
			p("\n%s:\n%s\n", kv[0], kv[1])
		}
	}
	if is.Truncated {
		p("\n(text cut; run without --compact for all of it)\n")
	}
}

// printSummaries prints one aligned line per issue: id, priority, status
// and title, then extra(i) for issues[i] when extra is set.
func printSummaries(w io.Writer, issues []proto.Summary, extra func(i int) string) {
	// Laid out in memory and written once: tabwriter writes each cell and
	// each run of padding on its own, eight writes a row.
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for i, is := range issues {
		line := fmt.Sprintf("%s\tP%d\t%s\t%s", esc(is.ID), is.Priority, esc(is.Status), esc(is.Title))
		if extra != nil {
			line += extra(i)
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
	_, _ = w.Write(b.Bytes())
}

func cmdList(ctx context.Context, r *runner, args []string) error {
	const usage = "list [--status S,S] [--all] [-t TYPE] [-p N,N] [--assignee A] [--parent ID] [--label L]... [-n N] [--cursor C]"
	fs := r.newFlags("list")
	var status, types, prios, labels listFlag
	var in proto.ListArgs
	fs.Var(&status, "status", "statuses (comma-separated)")
	fs.Var(&types, "t", "types")
	fs.Var(&types, "type", "types")
	fs.Var(&prios, "p", "priorities")
	fs.Var(&prios, "priority", "priorities")
	fs.Var(&labels, "label", "labels, all required")
	fs.Var(&labels, "l", "labels")
	all := fs.Bool("all", false, "include closed issues")
	fs.StringVar(&in.Assignee, "assignee", "", "assignee")
	fs.StringVar(&in.Parent, "parent", "", "parent issue id")
	fs.IntVar(&in.Limit, "n", 0, "page size (default 50)")
	fs.StringVar(&in.Cursor, "cursor", "", "continue from a previous page")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "list takes no arguments")
	}
	in.Status, in.Type, in.Labels = status, types, labels
	if len(in.Status) == 0 && !*all {
		in.Status = []string{"open", "in_progress", "blocked", "deferred"}
	}
	for _, s := range prios {
		p, err := priority(s)
		if err != nil {
			return usagef(usage, "%v", err)
		}
		in.Priority = append(in.Priority, p)
	}
	var out proto.ListResult
	if err := r.call(ctx, proto.OpList, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printList(r.env.Stdout, out.Issues)
	if out.Next != "" {
		_, _ = fmt.Fprintf(r.env.Stderr, "more: aries list --cursor %s\n", esc(out.Next))
	}
	return nil
}

// printList is printSummaries with each issue's dates, if it has any.
func printList(w io.Writer, issues []proto.Summary) {
	printSummaries(w, issues, func(i int) string {
		var ds []string
		if t := issues[i].DeferUntil; t != nil {
			ds = append(ds, "not before "+when(*t))
		}
		if t := issues[i].Due; t != nil {
			ds = append(ds, "due "+when(*t))
		}
		if len(ds) == 0 {
			return ""
		}
		return "\t" + strings.Join(ds, "; ")
	})
}

// limitArgs parses the arguments of a command whose only flag is -n.
func limitArgs(r *runner, name string, args []string) (proto.LimitArgs, error) {
	usage := name + " [-n N]"
	fs := r.newFlags(name)
	var in proto.LimitArgs
	fs.IntVar(&in.Limit, "n", 0, "how many")
	pos, err := parse(fs, args, usage)
	if err == nil && len(pos) > 0 {
		err = usagef(usage, "%s takes no arguments", name)
	}
	return in, err
}

func cmdReady(ctx context.Context, r *runner, args []string) error {
	in, err := limitArgs(r, "ready", args)
	if err != nil {
		return err
	}
	var out proto.ListResult
	if err := r.call(ctx, proto.OpReady, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printReady(r.env.Stdout, out.Issues)
	return nil
}

func cmdBlocked(ctx context.Context, r *runner, args []string) error {
	in, err := limitArgs(r, "blocked", args)
	if err != nil {
		return err
	}
	var out proto.BlockedResult
	if err := r.call(ctx, proto.OpBlocked, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printBlocked(r.env.Stdout, out.Issues)
	for _, g := range out.Gates {
		_, _ = fmt.Fprintf(r.env.Stdout, "gate %s: %s may pass it (`aries gate pass %s`)\n", esc(g.ID), approverText(g.Approvers), esc(g.ID))
	}
	return nil
}

// printBlocked lists blocked issues with what holds each back.
func printBlocked(w io.Writer, issues []proto.BlockedIssue) {
	sums := make([]proto.Summary, len(issues))
	for i, b := range issues {
		sums[i] = b.Summary
	}
	printSummaries(w, sums, func(i int) string {
		b := issues[i]
		var why []string
		if len(b.BlockedBy) > 0 {
			s := "blocked by " + strings.Join(escAll(b.BlockedBy), ", ")
			if b.More > 0 {
				s += fmt.Sprintf(" and %d more", b.More)
			}
			if b.Via != "" {
				s += " (via " + esc(b.Via) + ")"
			}
			why = append(why, s)
		}
		switch {
		case b.Children == 1:
			why = append(why, "waiting on 1 open child")
		case b.Children > 1:
			why = append(why, fmt.Sprintf("waiting on %d open children", b.Children))
		}
		return "\t" + strings.Join(why, "; ")
	})
}

func cmdUpdate(ctx context.Context, r *runner, args []string) error {
	const usage = "update ID [--rev N] [--title T] [--body TEXT|-] [--status S] [-p N] [-t TYPE] [--assignee A] [--parent ID] [--account NAME] [--paths P,P] [--defer-until DATE] [--due DATE] ..."
	fs := r.newFlags("update")
	var f fields
	f.register(fs, true)
	var paths listFlag
	fs.Var(&paths, "paths", "files the work will touch, replacing those declared; empty clears them")
	rev := fs.Int64("rev", 0, "revision you read (default: the current one)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	id, err := one(usage, pos, "update")
	if err != nil {
		return err
	}
	if err := f.checkDates(usage); err != nil {
		return err
	}
	if err := r.readTexts(&f); err != nil {
		return err
	}
	in := proto.UpdateArgs{ID: id, Rev: *rev}
	n := 0
	str := func(names []string, v string, dst **string) {
		for _, name := range names {
			if set(fs, name) {
				s := v
				*dst = &s
				n++
				return
			}
		}
	}
	str([]string{"title"}, f.title, &in.Title)
	str([]string{"body"}, f.body, &in.Body)
	str([]string{"design"}, f.design, &in.Design)
	str([]string{"acceptance"}, f.acceptance, &in.Acceptance)
	str([]string{"notes"}, f.notes, &in.Notes)
	str([]string{"status"}, f.status, &in.Status)
	str([]string{"t", "type"}, f.typ, &in.Type)
	str([]string{"assignee"}, f.assignee, &in.Assignee)
	str([]string{"owner"}, f.owner, &in.Owner)
	str([]string{"parent"}, f.parent, &in.Parent)
	str([]string{"account"}, f.account, &in.Account)
	str([]string{"defer-until"}, f.deferUntil, &in.DeferUntil)
	str([]string{"due"}, f.due, &in.Due)
	if set(fs, "p") || set(fs, "priority") {
		p, err := priority(f.prio)
		if err != nil {
			return usagef(usage, "%v", err)
		}
		in.Priority = &p
		n++
	}
	if set(fs, "paths") {
		ps := gitx.RepoRelative(ctx, r.dir, paths)
		in.Paths = &ps
		n++
	}
	if n == 0 {
		return usagef(usage, "nothing to update: give at least one field flag")
	}
	if in.Rev == 0 {
		var cur proto.ShowResult
		if err := r.call(ctx, proto.OpShow, proto.ShowArgs{ID: id}, &cur); err != nil {
			return r.needsRev(err)
		}
		in.Rev = cur.Issue.Rev
	}
	in.Idem = proto.NewIdem("cli")
	return r.write(ctx, proto.OpUpdate, in)
}

// needsRev adds, to the fix of an update without --rev whose read of the
// issue's rev did not reach the server, that it was not queued: an
// update is queued only with the rev it was based on (design §18.3).
func (r *runner) needsRev(err error) error {
	pe, ok := errors.AsType[*proto.Error](err)
	if !ok || r.hook || !client.Unreachable(err) {
		return err
	}
	e := *pe
	if e.Fix != "" {
		e.Fix += "; "
	}
	e.Fix += "this update was not queued, since it needs the issue's rev, which only the server knows: give --rev N, the rev you based it on, to queue it"
	return &e
}

// write runs an issue write and prints "ID rev N".
func (r *runner) write(ctx context.Context, op string, in any) error {
	var out proto.WriteResult
	kept, err := r.send(ctx, op, in, &out)
	if err != nil {
		return err
	}
	if kept != nil {
		r.queued(kept)
		return nil
	}
	if r.json {
		r.emit(out)
	} else {
		_, _ = fmt.Fprintf(r.env.Stdout, "%s rev %d\n", esc(out.ID), out.Rev)
	}
	return nil
}

func cmdClose(ctx context.Context, r *runner, args []string) error {
	const usage = "close ID [--reason TEXT] [--rev N] [--force]"
	fs := r.newFlags("close")
	var in proto.CloseArgs
	fs.StringVar(&in.Reason, "reason", "", "why it was closed")
	fs.BoolVar(&in.Force, "force", false, "admins only: close even with acceptance items open; the event records them")
	fs.Int64Var(&in.Rev, "rev", 0, "refuse if the issue changed since this revision")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if in.ID, err = one(usage, pos, "close"); err != nil {
		return err
	}
	return r.write(ctx, proto.OpClose, in)
}

func cmdReopen(ctx context.Context, r *runner, args []string) error {
	const usage = "reopen ID [--rev N]"
	fs := r.newFlags("reopen")
	var in proto.ReopenArgs
	fs.Int64Var(&in.Rev, "rev", 0, "refuse if the issue changed since this revision")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if in.ID, err = one(usage, pos, "reopen"); err != nil {
		return err
	}
	return r.write(ctx, proto.OpReopen, in)
}

// done prints the empty result of a write with nothing to report.
func (r *runner) done() {
	if r.json {
		r.emit(proto.Empty{})
	}
}

// depUsage and labelUsage name remove; rm, its spelling before 1.0, is
// a hidden alias.
const (
	depUsage   = "dep add|remove FROM TO [--type T]"
	labelUsage = "label add|remove ID LABEL..."
)

func cmdDep(ctx context.Context, r *runner, args []string) error {
	const usage = depUsage
	fs := r.newFlags("dep")
	var in proto.DepArgs
	fs.StringVar(&in.Type, "type", "blocks", "blocks, conditional-blocks, waits-for, related, discovered-from, duplicates or supersedes")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) != 3 {
		return usagef(usage, "dep needs add or remove, then two issue ids")
	}
	op := map[string]string{"add": proto.OpDepAdd, "remove": proto.OpDepRm, "rm": proto.OpDepRm}[pos[0]]
	if op == "" {
		return usagef(usage, "unknown dep action %q", pos[0])
	}
	in.From, in.To, in.Idem = pos[1], pos[2], proto.NewIdem("cli")
	if op == proto.OpDepRm {
		// Never queued (outbox.never), though the server honors its key.
		if err := r.call(ctx, op, in, nil); err != nil {
			return err
		}
		r.done()
		return nil
	}
	kept, err := r.send(ctx, op, in, nil)
	if err != nil {
		return err
	}
	if kept != nil {
		r.queued(kept)
		return nil
	}
	r.done()
	return nil
}

func cmdLabel(ctx context.Context, r *runner, args []string) error {
	const usage = labelUsage
	fs := r.newFlags("label")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return usagef(usage, "label needs add or remove, an issue id and at least one label")
	}
	op := map[string]string{"add": proto.OpLabelAdd, "remove": proto.OpLabelRm, "rm": proto.OpLabelRm}[pos[0]]
	if op == "" {
		return usagef(usage, "unknown label action %q", pos[0])
	}
	// One request for each label, each with a key of its own. Some may be
	// made and the rest queued, when the server goes in between.
	var queued []*offline.Kept
	for _, l := range pos[2:] {
		kept, err := r.send(ctx, op, proto.LabelArgs{ID: pos[1], Label: l, Idem: proto.NewIdem("cli")}, nil)
		if err != nil {
			return err
		}
		if kept != nil {
			queued = append(queued, kept)
		}
	}
	if len(queued) == 0 {
		r.done()
		return nil
	}
	r.queued(queued...)
	return nil
}

func cmdComment(ctx context.Context, r *runner, args []string) error {
	const usage = "comment ID TEXT...|-"
	fs := r.newFlags("comment")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef(usage, "comment needs an issue id and text (or - for standard input)")
	}
	body, err := r.text(strings.Join(pos[1:], " "))
	if err != nil {
		return err
	}
	idem := proto.NewIdem("cli")
	var out proto.CommentResult
	kept, err := r.send(ctx, proto.OpComment, proto.CommentArgs{ID: pos[0], Body: body, Idem: idem}, &out)
	if err != nil {
		return err
	}
	if kept != nil {
		r.queued(kept)
		return nil
	}
	if r.json {
		r.emit(out)
	} else {
		_, _ = fmt.Fprintln(r.env.Stdout, esc(out.ID))
	}
	return nil
}

// pageFlag registers -n, the newest entries to show (0: all).
func pageFlag(r *runner, name string) (*flag.FlagSet, *int) {
	fs := r.newFlags(name)
	n := fs.Int("n", 0, "show only the newest N (default all)")
	return fs, n
}

// pages reads an issue's comments or history newest page first, until it
// has want entries (0: all), and returns them oldest first with how many
// earlier ones it left out. page asks for one page before the cursor and
// returns its entries, the cursor of the page before it and the total.
func pages[T any](want int, page func(before string, limit int) ([]T, string, int, error)) ([]T, int, error) {
	var all []T
	before, total := "", 0
	for {
		limit := 0
		if want > 0 {
			limit = want - len(all)
		}
		got, earlier, n, err := page(before, limit)
		if err != nil {
			return nil, 0, err
		}
		all, total = append(got, all...), max(n, total)
		if earlier == "" || (want > 0 && len(all) >= want) {
			break
		}
		before = earlier
	}
	if want > 0 && len(all) > want {
		all = all[len(all)-want:]
	}
	return all, max(total-len(all), 0), nil
}

func cmdComments(ctx context.Context, r *runner, args []string) error {
	const usage = "comments ID [-n N]"
	fs, n := pageFlag(r, "comments")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	id, err := one(usage, pos, "comments")
	if err != nil {
		return err
	}
	cs, earlier, err := pages(*n, func(before string, limit int) ([]proto.Comment, string, int, error) {
		var out proto.CommentsResult
		err := r.call(ctx, proto.OpComments, proto.PageArgs{ID: id, Before: before, Limit: limit}, &out)
		return out.Comments, out.Earlier, out.Total, err
	})
	if err != nil {
		return err
	}
	if r.json {
		r.emit(proto.CommentsResult{Comments: cs, Total: len(cs) + earlier})
		return nil
	}
	if earlier > 0 {
		_, _ = fmt.Fprintf(r.env.Stdout, "(%d earlier not shown; -n 0 shows all)\n\n", earlier)
	}
	printComments(r.env.Stdout, cs)
	return nil
}

// printComments prints each comment's author, time and kind on one line,
// then its body.
func printComments(w io.Writer, cs []proto.Comment) {
	for i, c := range cs {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		kind := ""
		if c.Kind != "" {
			kind = "  (" + esc(c.Kind) + ")"
		}
		_, _ = fmt.Fprintf(w, "%s  %s%s\n%s\n", esc(c.Author), when(c.CreatedAt), kind, c.Body)
	}
}

func cmdHistory(ctx context.Context, r *runner, args []string) error {
	const usage = "history ID [-n N]"
	fs, n := pageFlag(r, "history")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	id, err := one(usage, pos, "history")
	if err != nil {
		return err
	}
	evs, earlier, err := pages(*n, func(before string, limit int) ([]proto.Event, string, int, error) {
		var out proto.HistoryResult
		err := r.call(ctx, proto.OpHistory, proto.PageArgs{ID: id, Before: before, Limit: limit}, &out)
		return out.Events, out.Earlier, out.Total, err
	})
	if err != nil {
		return err
	}
	if r.json {
		r.emit(proto.HistoryResult{Events: evs, Total: len(evs) + earlier})
		return nil
	}
	if earlier > 0 {
		_, _ = fmt.Fprintf(r.env.Stdout, "(%d earlier not shown; -n 0 shows all)\n", earlier)
	}
	return printHistory(r.env.Stdout, evs)
}

// printHistory prints one line per event.
func printHistory(w io.Writer, evs []proto.Event) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range evs {
		_, _ = fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\n", e.Seq, when(e.At), esc(e.Principal), esc(e.Op), esc(e.Changed()))
	}
	return tw.Flush()
}

func cmdVersion(_ context.Context, r *runner, args []string) error {
	if _, err := parse(r.newFlags("version"), args, "version"); err != nil {
		return err
	}
	if r.json {
		r.emit(map[string]any{"version": r.env.Version, "protocol": proto.Proto})
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "aries %s\n", r.env.Version)
	return nil
}
