package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ariesworx/starfix/internal/proto"
)

const gateUsage = "gate add --blocks ID [--approver P]... [--title T] | gate pass|rearm ID [--reason TEXT] | gate waive ID --reason TEXT | gate list [--all] [-n N]"

// cmdGate adds, passes, waives, rearms and lists human gates (design
// §20.3). None of it is queued while the server cannot be reached: a gate
// decides on a state the client cannot see, and a pass is a person's act
// at the moment they make it.
func cmdGate(ctx context.Context, r *runner, args []string) error {
	if len(args) == 0 {
		return usagef(gateUsage, "gate needs add, pass, waive, rearm or list")
	}
	action, rest := args[0], args[1:]
	switch action {
	case "add":
		return gateAdd(ctx, r, rest)
	case "pass", "waive", "rearm":
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
	fs.StringVar(&in.Title, "title", "", "the gate's title (default: Approve ID)")
	pos, err := parse(fs, args, gateUsage)
	if err != nil {
		return err
	}
	if len(pos) > 0 || in.Blocks == "" {
		return usagef(gateUsage, "gate add takes the issue it holds back as --blocks ID")
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
	_, _ = fmt.Fprintf(r.env.Stderr, "holds %s until %s passes it: `sfx gate pass %s`\n", esc(in.Blocks), approverText(out.Approvers), esc(out.ID))
	return nil
}

func gateDecide(ctx context.Context, r *runner, action string, args []string) error {
	fs := r.newFlags("gate " + action)
	var in proto.GateArgs
	fs.StringVar(&in.Reason, "reason", "", "why; a waive needs one")
	pos, err := parse(fs, args, gateUsage)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(gateUsage, "gate %s takes one gate id; `sfx gate list` lists them", action)
	}
	if action == "waive" && strings.TrimSpace(in.Reason) == "" {
		return usagef(gateUsage, "gate waive needs --reason: a waive passes a gate whoever it names, on the record")
	}
	in.ID = pos[0]
	op := map[string]string{"pass": proto.OpGatePass, "waive": proto.OpGateWaive, "rearm": proto.OpGateRearm}[action]
	var out proto.Gate
	if err := r.call(ctx, op, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	state := "open"
	if out.Outcome != "" {
		state = esc(out.Outcome)
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s %s\n", esc(out.ID), state)
	return nil
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
// open or how it was decided, who may pass it, what it holds and its
// title.
func printGates(w io.Writer, gates []proto.Gate) {
	if len(gates) == 0 {
		_, _ = fmt.Fprintln(w, "no open gates")
		return
	}
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, g := range gates {
		state := "open"
		if g.Outcome != "" {
			state = esc(g.Outcome) + " by " + esc(g.By)
			if g.At != nil {
				state += " at " + when(*g.At)
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\tholds %s\t%s\n", esc(g.ID), state, approverText(g.Approvers),
			strings.Join(escAll(g.Holds), ", "), esc(g.Title))
	}
	_ = tw.Flush()
	_, _ = w.Write(b.Bytes())
}
