package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// hoursUsage is aries hours's; log, its name before 1.0, is an alias.
const hoursUsage = "hours DURATION ID [--on DATE] [--note TEXT] | hours --undo ENTRY | hours [--issue ID] [--by PRINCIPAL] [-n N]"

func cmdHours(ctx context.Context, r *runner, args []string) error {
	const usage = hoursUsage
	fs := r.newFlags("hours")
	on := fs.String("on", "", "the day worked, 2006-01-02 (default today, in your time zone)")
	note := fs.String("note", "", "a one-line note")
	undo := fs.String("undo", "", "undo the entry with this id")
	var list proto.HoursArgs
	fs.StringVar(&list.Issue, "issue", "", "list the entries on this issue")
	fs.StringVar(&list.By, "by", "", "list the entries this principal logged")
	fs.IntVar(&list.Limit, "n", 0, "list at most N entries (default 50, at most 500)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	logging, listing := set(fs, "on") || set(fs, "note"), set(fs, "issue") || set(fs, "by") || set(fs, "n")
	switch {
	case set(fs, "undo"):
		if len(pos) > 0 || logging || listing {
			return usagef(usage, "--undo takes no other arguments")
		}
		var out proto.HoursDeleteResult
		if err := r.call(ctx, proto.OpHoursDelete, proto.HoursDeleteArgs{ID: *undo, Idem: proto.NewIdem("cli")}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "undone: entry %s\n", esc(out.ID))
		return nil
	case len(pos) == 0:
		if logging {
			return usagef(usage, "--on and --note are for logging: aries hours DURATION ID")
		}
		var out proto.HoursResult
		if err := r.call(ctx, proto.OpHours, list, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printHours(r.env.Stdout, out)
		return nil
	case len(pos) != 2:
		return usagef(usage, "hours needs a time and an issue id, such as aries hours 1.5h ar-a1b2")
	case listing:
		return usagef(usage, "--issue, --by and -n are for listing; aries hours DURATION ID logs")
	}
	d, err := proto.ParseHours(pos[0])
	if err != nil {
		return usagef(usage, "%q: %v", pos[0], err)
	}
	// The day is the logger's own, which the server cannot know.
	if *on == "" {
		*on = r.env.Now().Format(time.DateOnly)
	}
	if _, err := time.Parse(time.DateOnly, *on); err != nil {
		return usagef(usage, "--on %q must be a date such as 2026-10-08", *on)
	}
	in := proto.HoursLogArgs{Issue: pos[1], Seconds: int64(d / time.Second), On: *on, Note: *note, Idem: proto.NewIdem("cli")}
	var out proto.HoursLogResult
	kept, err := r.send(ctx, proto.OpHoursLog, in, &out)
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
	_, _ = fmt.Fprintf(r.env.Stdout, "logged %s on %s for %s: entry %s", proto.Hours(in.Seconds), esc(in.Issue), esc(out.On), esc(out.ID))
	if out.Undone {
		_, _ = fmt.Fprint(r.env.Stdout, ", since undone")
	}
	_, _ = fmt.Fprintln(r.env.Stdout)
	return nil
}

// printHours renders entries, newest day first, and how many more there
// are.
func printHours(w io.Writer, res proto.HoursResult) {
	if len(res.Entries) == 0 {
		_, _ = fmt.Fprintln(w, "no hours logged")
		return
	}
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ON\tWHO\tISSUE\tHOURS\tENTRY\tNOTE")
	for _, e := range res.Entries {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", esc(e.On), esc(e.Principal), esc(e.Issue), proto.Hours(e.Seconds), esc(e.ID), esc(e.Note))
	}
	_ = tw.Flush()
	for line := range strings.Lines(b.String()) {
		_, _ = fmt.Fprintln(w, strings.TrimRight(line, " \n"))
	}
	if res.More > 0 {
		_, _ = fmt.Fprintf(w, "and %d more; raise -n, or narrow with --issue or --by\n", res.More)
	}
}

// planFlags are the terms plan set needs, in the order its usage names
// them.
var planFlags = []string{"from", "fee", "seats"}

const planUsage = "admin plan list | set NAME --from YYYY-MM --fee USD --seats N [--principal P]..."

func adminPlan(ctx context.Context, r *runner, args []string) error {
	const usage = planUsage
	fs := r.newFlags("admin plan")
	from := fs.String("from", "", "the first month the terms hold, 2006-01")
	fee := fs.String("fee", "", "US dollars a seat a month")
	seats := fs.Int("seats", 0, "seats paid for")
	var principals listFlag
	fs.Var(&principals, "principal", "a principal whose usage the plan pays for (repeatable, or comma-separated)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	given := set(fs, "principal")
	for _, f := range planFlags {
		given = given || set(fs, f)
	}
	switch {
	case len(pos) == 0:
		return usagef(usage, "admin plan needs list or set")
	case pos[0] == "list":
		if len(pos) > 1 {
			return usagef(usage, "plan list takes no arguments")
		}
		if given {
			return usagef(usage, "only plan set takes terms")
		}
		var out proto.PlansResult
		if err := r.call(ctx, proto.OpPlans, proto.PlansArgs{}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printPlans(r.env.Stdout, out)
		return nil
	case pos[0] != "set":
		return usagef(usage, "unknown plan action %q", pos[0])
	case len(pos) != 2:
		return usagef(usage, "plan set needs one name, such as team")
	}
	var missing []string
	for _, f := range planFlags {
		if !set(fs, f) {
			missing = append(missing, "--"+f)
		}
	}
	if len(missing) > 0 {
		return usagef(usage, "plan set needs %s; give --fee 0 --seats 0 to end a plan", strings.Join(missing, ", "))
	}
	if _, err := time.Parse("2006-01", *from); err != nil {
		return usagef(usage, "--from %q must be a month such as 2026-09", *from)
	}
	in := proto.PlanSetArgs{Name: pos[1], From: *from, Seats: *seats, Principals: principals}
	if in.Fee, err = proto.ParseFee(*fee); err != nil {
		return usagef(usage, "--fee: %v", err)
	}
	var out proto.PlanSetResult
	if err := r.call(ctx, proto.OpPlanSet, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s from %s: %s\n", esc(in.Name), esc(in.From), esc(out.Change))
	return nil
}

// printPlans renders the plans table in US dollars a seat a month, with
// each row's monthly total.
func printPlans(w io.Writer, res proto.PlansResult) {
	if len(res.Plans) == 0 {
		_, _ = fmt.Fprintln(w, "no plans; an admin sets one with `aries admin plan set`")
		return
	}
	_, _ = fmt.Fprintln(w, "USD a seat a month")
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tFROM\tFEE\tSEATS\tMONTHLY\tPRINCIPALS\tSET BY")
	for _, p := range res.Plans {
		who := strings.Join(escAll(p.Principals), ", ")
		if who == "" {
			who = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", esc(p.Name), esc(p.From), proto.FormatRate(p.Fee), p.Seats,
			proto.FormatRate(p.Fee*int64(p.Seats)), who, esc(p.SetBy))
	}
	_ = tw.Flush()
	for line := range strings.Lines(b.String()) {
		_, _ = fmt.Fprintln(w, strings.TrimRight(line, " \n"))
	}
}
