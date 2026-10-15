package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ariesworx/starfix/internal/gitx"
	"github.com/ariesworx/starfix/internal/proto"
)

const gateUsage = "gate add --blocks ID [--approver P]... [--title T]" +
	" | gate add --kind ci --check NAME --subject ID --by P... [--blocks ID] [--title T]" +
	" | gate pass|rearm ID [--reason TEXT]" +
	" | gate pass|fail [ID] [--check NAME --issue ID|--branch NAME|--from-branch|--from-commit] [--sha SHA] [--url URL] [--reason TEXT] [--optional]" +
	" | gate waive ID --reason TEXT | gate list [--all] [-n N]"

// cmdGate adds, passes, fails, waives, rearms and lists gates (design
// §20.3 and §20.4). None of it is queued while the server cannot be
// reached: a gate decides on a state the client cannot see, a pass is a
// person's act at the moment they make it, and a CI job's report is its
// run's, which ends.
func cmdGate(ctx context.Context, r *runner, args []string) error {
	if len(args) == 0 {
		return usagef(gateUsage, "gate needs add, pass, fail, waive, rearm or list")
	}
	action, rest := args[0], args[1:]
	switch action {
	case "add":
		return gateAdd(ctx, r, rest)
	case "pass", "fail", "waive", "rearm":
		return gateDecide(ctx, r, action, rest)
	case "list", "ls":
		return gateList(ctx, r, rest)
	}
	return usagef(gateUsage, "unknown gate action %q", action)
}

func gateAdd(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("gate add")
	var in proto.GateAddArgs
	var approvers listFlag
	fs.StringVar(&in.Blocks, "blocks", "", "the issue the gate holds back; a gate on an epic holds all of it")
	fs.Var(&approvers, "approver", "a principal who may pass it (repeatable, or comma-separated; none: the project's admins)")
	fs.Var(&approvers, "by", "a ci gate's: a CI principal that reports its check (repeatable, or comma-separated)")
	fs.StringVar(&in.Title, "title", "", "the gate's title (default: Approve ID, or CHECK of SUBJECT)")
	fs.StringVar(&in.Kind, "kind", "", "human (the default), passed by a person, or ci, reported by CI")
	fs.StringVar(&in.Check, "check", "", "a ci gate's check, such as build, test or landed")
	fs.StringVar(&in.Subject, "subject", "", "a ci gate's subject: the issue whose branch the check runs on")
	pos, err := parse(fs, args, gateUsage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(gateUsage, "gate add takes no arguments; name the issue it holds back with --blocks ID")
	}
	switch in.Kind {
	case "", proto.GateHuman:
		if in.Check != "" || in.Subject != "" {
			return usagef(gateUsage, "--check and --subject are a ci gate's: add it with --kind ci")
		}
		if in.Blocks == "" {
			return usagef(gateUsage, "gate add takes the issue it holds back as --blocks ID")
		}
	case proto.GateCI:
		if in.Check == "" || in.Subject == "" || len(approvers) == 0 {
			return usagef(gateUsage, "a ci gate needs --check NAME, --subject ID and --by PRINCIPAL, the CI principal that reports it")
		}
	default:
		return usagef(gateUsage, "--kind %q: give human or ci", in.Kind)
	}
	in.Approvers, in.Idem = approvers, proto.NewIdem("cli")
	var out proto.Gate
	if err := r.call(ctx, proto.OpGateAdd, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintln(r.env.Stdout, esc(out.ID))
	switch {
	case out.Kind != proto.GateCI:
		_, _ = fmt.Fprintf(r.env.Stderr, "holds %s until %s passes it: `sfx gate pass %s`\n", esc(in.Blocks), approverText(out.Approvers), esc(out.ID))
	case in.Blocks != "":
		_, _ = fmt.Fprintf(r.env.Stderr, "holds %s until %s reports %s passed on %s: `sfx gate pass --check %s --issue %s`\n",
			esc(in.Blocks), approverText(out.Approvers), esc(out.Check), esc(out.Subject), esc(out.Check), esc(out.Subject))
	default:
		_, _ = fmt.Fprintf(r.env.Stderr, "records %s of %s, which %s reports: `sfx gate pass --check %s --issue %s`\n",
			esc(out.Check), esc(out.Subject), approverText(out.Approvers), esc(out.Check), esc(out.Subject))
	}
	return nil
}

// reportFlags name the issue a CI report is about, one way of four, and
// say whether a report with nothing to report on is no failure.
type reportFlags struct {
	issue, branch          string
	fromBranch, fromCommit bool
	optional               bool
}

func (f *reportFlags) register(fs interface {
	StringVar(*string, string, string, string)
	BoolVar(*bool, string, bool, string)
}, in *proto.GateArgs) {
	fs.StringVar(&in.Check, "check", "", "the check CI ran, such as build, test or landed")
	fs.StringVar(&f.issue, "issue", "", "the issue the check ran on")
	fs.StringVar(&f.branch, "branch", "", "the branch the check ran on, which names the issue")
	fs.BoolVar(&f.fromBranch, "from-branch", false, "take the branch from the CI job's variables, or else from git")
	fs.BoolVar(&f.fromCommit, "from-commit", false, "take the issue from HEAD's commit: its Starfix: trailer, or the branch a merge commit merged")
	fs.StringVar(&in.SHA, "sha", "", "the commit checked")
	fs.StringVar(&in.URL, "url", "", "a link to the run")
	fs.BoolVar(&f.optional, "optional", false, "exit 0 when the branch or commit names no issue, or the issue has no gate for the check")
}

// ways counts the ways f names the issue.
func (f *reportFlags) ways() int {
	n := 0
	for _, set := range []bool{f.issue != "", f.branch != "", f.fromBranch, f.fromCommit} {
		if set {
			n++
		}
	}
	return n
}

func gateDecide(ctx context.Context, r *runner, action string, args []string) error {
	fs := r.newFlags("gate " + action)
	var in proto.GateArgs
	var rep reportFlags
	fs.StringVar(&in.Reason, "reason", "", "why; a waive needs one")
	reports := action == "pass" || action == "fail"
	if reports {
		rep.register(fs, &in)
	}
	pos, err := parse(fs, args, gateUsage)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usagef(gateUsage, "gate %s takes one gate id; `sfx gate list` lists them", action)
	}
	if len(pos) == 1 {
		in.ID = pos[0]
	}
	if reports {
		return gateReport(ctx, r, action, in, rep)
	}
	if in.ID == "" {
		return usagef(gateUsage, "gate %s takes one gate id; `sfx gate list` lists them", action)
	}
	if action == "waive" && strings.TrimSpace(in.Reason) == "" {
		return usagef(gateUsage, "gate waive needs --reason: a waive passes a gate whoever it names, on the record")
	}
	op := map[string]string{"waive": proto.OpGateWaive, "rearm": proto.OpGateRearm}[action]
	var out proto.Gate
	if err := r.call(ctx, op, in, &out); err != nil {
		return err
	}
	printDecided(r, out)
	return nil
}

// gateReport is gate pass and gate fail: a person's pass of a gate by its
// id, or a report of a check by the gate's id or by the check and the
// issue it ran on.
func gateReport(ctx context.Context, r *runner, action string, in proto.GateArgs, rep reportFlags) error {
	switch {
	case rep.ways() > 1:
		return usagef(gateUsage, "give one of --issue, --branch, --from-branch and --from-commit")
	case in.ID == "" && in.Check == "":
		return usagef(gateUsage, "gate %s takes a gate id, or --check NAME and the issue it ran on; `sfx gate list` lists the gates", action)
	case in.ID == "" && rep.ways() == 0:
		return usagef(gateUsage, "--check needs the issue it ran on: --issue ID, --branch NAME, --from-branch or --from-commit")
	}
	in.Subject, in.Branch = rep.issue, rep.branch
	var err error
	switch {
	case rep.fromBranch:
		err = r.branchOf(ctx, &in)
	case rep.fromCommit:
		err = r.commitOf(ctx, &in)
	}
	if err == nil {
		op := proto.OpGatePass
		if action == "fail" {
			op = proto.OpGateFail
		}
		var out proto.Gate
		if err = r.call(ctx, op, in, &out); err == nil {
			printDecided(r, out)
			return nil
		}
	}
	if pe, ok := errors.AsType[*proto.Error](err); ok && pe.Code == proto.CodeNotFound && rep.optional {
		if r.json {
			r.emit(map[string]any{"reported": false, "reason": pe.Message})
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: nothing reported: %s\n", esc(pe.Message))
		return nil
	}
	return err
}

// branchOf sets in.Branch to the branch a CI job runs on, from its
// variables, or else the one checked out. Neither is not_found.
func (r *runner) branchOf(ctx context.Context, in *proto.GateArgs) error {
	in.Branch = gitx.BranchFromEnv(r.env.Getenv)
	if in.Branch != "" {
		return nil
	}
	b, err := gitx.CurrentBranch(ctx, r.dir)
	if err != nil {
		return fmt.Errorf("read the branch: %w", err)
	}
	if b == "" {
		return proto.Errf(proto.CodeNotFound, "give the branch with --branch NAME",
			"HEAD is detached, and no CI variable names the branch")
	}
	in.Branch = b
	return nil
}

// commitOf names the issue HEAD's commit names: in.Subject from its
// Starfix: trailer, or else in.Branch from a merge commit's subject.
// Neither is not_found.
func (r *runner) commitOf(ctx context.Context, in *proto.GateArgs) error {
	msg, err := gitx.HeadMessage(ctx, r.dir)
	if err != nil {
		return fmt.Errorf("read HEAD's commit: %w", err)
	}
	if id, ok := gitx.IDFromMessage(msg); ok {
		in.Subject = id
		return nil
	}
	if b, ok := gitx.BranchFromMerge(msg); ok {
		in.Branch = b
		return nil
	}
	return proto.Errf(proto.CodeNotFound,
		"end the commit's message with a `Starfix: ID` trailer, merge with a merge commit, or give --issue ID",
		"HEAD's commit names no issue: it has no Starfix: trailer and merged no branch")
}

// printDecided prints a gate after a decision or a report: its id and
// whether it is open, how it was decided, or how its check last failed.
func printDecided(r *runner, out proto.Gate) {
	if r.json {
		r.emit(out)
		return
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s %s\n", esc(out.ID), gateState(out, false))
}

// gateState says whether g is open, how it was decided, or that its
// check failed; long adds who decided and when.
func gateState(g proto.Gate, long bool) string {
	switch {
	case g.Outcome != "":
		s := esc(g.Outcome)
		if long {
			s += " by " + esc(g.By)
			if g.At != nil {
				s += " at " + when(*g.At)
			}
		}
		return s
	case g.Report != nil && g.Report.Result == "failed":
		if long {
			return "failed at " + when(g.Report.At)
		}
		return "failed"
	}
	return "open"
}

func gateList(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("gate list")
	var in proto.GatesArgs
	fs.BoolVar(&in.All, "all", false, "include passed and waived gates")
	fs.IntVar(&in.Limit, "n", 0, "how many (default 50)")
	pos, err := parse(fs, args, gateUsage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(gateUsage, "gate list takes no arguments")
	}
	var out proto.GatesResult
	if err := r.call(ctx, proto.OpGates, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printGates(r.env.Stdout, out.Gates)
	return nil
}

// approverText names who may pass a gate.
func approverText(approvers []string) string {
	if len(approvers) == 0 {
		return "the project's admins"
	}
	return strings.Join(escAll(approvers), ", ")
}

// printGates prints one aligned line per gate: its id, whether it is
// open, how it was decided or that its check failed, who may pass it,
// what it holds and its title.
func printGates(w io.Writer, gates []proto.Gate) {
	if len(gates) == 0 {
		_, _ = fmt.Fprintln(w, "no open gates")
		return
	}
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, g := range gates {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\tholds %s\t%s\n", esc(g.ID), gateState(g, true), approverText(g.Approvers),
			strings.Join(escAll(g.Holds), ", "), esc(g.Title))
	}
	_ = tw.Flush()
	_, _ = w.Write(b.Bytes())
}

// printChecks prints an issue's checks, one line each, and for a closed
// issue whether it landed: its landed check passed.
func printChecks(w io.Writer, s proto.ShowResult) {
	for _, c := range s.Checks {
		line := "check " + esc(c.Check) + ": "
		switch rep := c.Report; {
		case c.Outcome == "" && (rep == nil || rep.Result != "failed"):
			line += "waits for " + approverText(c.Approvers) + " (gate " + esc(c.ID) + ")"
		case c.Outcome == "waived", rep == nil:
			line += esc(c.Outcome) + " by " + esc(c.By) + " (gate " + esc(c.ID) + ")"
		default:
			line += esc(rep.Result) + evidenceText(*rep) + " (gate " + esc(c.ID) + ", reported by " + esc(rep.By) + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
	if s.Issue.Status != "closed" {
		return
	}
	for _, c := range s.Checks {
		if c.Check != "landed" {
			continue
		}
		if c.Outcome == "" {
			_, _ = fmt.Fprintln(w, "closed, not landed")
		} else if c.At != nil {
			_, _ = fmt.Fprintf(w, "closed, landed %s\n", when(*c.At))
		}
	}
}

// evidenceText is a report's evidence on a line: " at" the commit,
// shortened, and the link and the reason.
func evidenceText(r proto.GateReport) string {
	var b strings.Builder
	if r.SHA != "" {
		b.WriteString(" at " + esc(r.SHA[:min(len(r.SHA), 12)]))
	}
	if r.URL != "" || r.Reason != "" {
		b.WriteString(":")
	}
	for _, s := range []string{r.URL, r.Reason} {
		if s != "" {
			b.WriteString(" " + esc(s))
		}
	}
	return b.String()
}
