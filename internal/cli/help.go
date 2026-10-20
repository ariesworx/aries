package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"time"
)

// helpWidth is the width help is wrapped to, a terminal's least.
const helpWidth = 80

// helpGroup is a heading of `aries help` and the commands under it, each
// with the one line the list prints.
type helpGroup struct {
	title    string
	commands []helpLine
}

type helpLine struct{ name, brief string }

// helpGroups are the headings of `aries help`, by task. Every command is
// under one, once (TestHelpGroups).
var helpGroups = []helpGroup{
	{"Issues", []helpLine{
		{"create", "create an issue and print its id"},
		{"show", "show an issue with its checklist, dependencies and gates"},
		{"list", "list issues, the open ones unless --status or --all"},
		{"ready", "list the issues nothing holds back, best first"},
		{"blocked", "list the issues held back, and the gates among them"},
		{"update", "change an issue's fields"},
		{"close", "close an issue"},
		{"reopen", "reopen a closed issue"},
		{"dep", "add or remove a dependency between two issues"},
		{"label", "add or remove an issue's labels"},
		{"comment", "add a comment to an issue"},
		{"comments", "list an issue's comments"},
		{"history", "list an issue's changes"},
	}},
	{"Coordination", []helpLine{
		{"prime", "orient a session: your work, inbox, ready issues, memories"},
		{"start", "claim an issue (the top ready one without ID) and show it"},
		{"finish", "close your issue with a handoff note and discovered work"},
		{"accept", "tick, untick or waive an issue's acceptance items"},
		{"handoff", "leave a note for whoever continues; --release lets it go"},
		{"away", "extend all your claims, before you go offline"},
		{"lock", "take, renew, release or list a lock; run a command under one"},
		{"dispatch", "experimental: start agent runs on ready issues, within a budget"},
		{"inbox", "list your unread lost claims, handoffs, mentions, assignments"},
		{"watch", "print inbox items and, with --events, issue events"},
		{"who", "list the agents at work and the issues each holds"},
		{"digest", "summarize what closed, started, stalled and was handed off"},
		{"tui", "show a live board of ready, held and blocked issues"},
	}},
	{"Memory", []helpLine{
		{"remember", "keep a memory for later sessions"},
		{"recall", "search memories by text, key or tag"},
		{"memories", "list the memories you can see"},
		{"forget", "delete a memory"},
		{"pin", "pin a memory, so prime always shows it"},
		{"unpin", "unpin a memory"},
	}},
	{"Cost and usage", []helpLine{
		{"cost", "report the cost of tokens, and the hours logged"},
		{"hours", "log your hours on an issue, undo an entry, or list entries"},
		{"usage", "send a session's token counts (run by the agent's hooks)"},
		{"outcome", "experimental: record what an issue's outcome took, once"},
	}},
	{"Gates", []helpLine{
		{"gate", "hold issues until a person passes a gate or CI reports a check"},
		{"land", "report landed for the default branch's commits, after a pull"},
		{"workflow", "check that the server's workflow is the one in a file"},
	}},
	{"Offline and cache", []helpLine{
		{"outbox", "list, retry or drop the writes waiting for the server"},
		{"cache", "show or clear what this machine keeps of what was read"},
		{"conflicts", "list offline updates parked against others' changes"},
		{"resolve", "resolve a conflict: mine, theirs, new text, or as proposed"},
	}},
	{"Setup", []helpLine{
		{"setup", "set agents up: MCP config, instruction pointer, session hook"},
		{"login", "sign in at the server's identity provider, for the day"},
		{"logout", "end your sign-in, and remove it from this machine"},
		{"whoami", "say who the server takes you for, and how you signed in"},
		{"mcp", "serve the MCP tools for an agent on stdin and stdout"},
		{"upgrade", "replace aries with the latest verified release"},
		{"version", "print the aries version"},
	}},
	{"Admin", []helpLine{
		{"admin", "prices, plans, projects, principals, keys, workflow (admins)"},
	}},
}

// example is one example in a command's help: what it does, and the
// command line. Examples name only example.com, alice and bob, and each
// one's flags and subcommand are held to the command's (TestHelpExamples).
type example struct{ does, line string }

// examples are each command's, two or three.
var examples = map[string][]example{
	"create": {
		{"A bug at priority 1", `aries create "Fix the login redirect" -t bug -p 1`},
		{"A task under an epic, its body read from stdin", `aries create "Write the upgrade notes" --parent ar-a1b2c3d4 --body -`},
		{"A labeled feature with a due date", `aries create "Export to CSV" -t feature --label ui --due 2026-12-01`},
	},
	"show": {
		{"An issue in full", "aries show ar-a1b2c3d4"},
		{"Its fields alone, without comments and history", "aries show ar-a1b2c3d4 --compact"},
		{"As one JSON document, for a script", "aries --json show ar-a1b2c3d4"},
	},
	"list": {
		{"Open bugs at priority 0 or 1", "aries list -t bug -p 0,1"},
		{"Everything assigned to alice, closed issues too", "aries list --assignee alice --all"},
		{"An epic's children in progress", "aries list --status in_progress --parent ar-a1b2c3d4"},
	},
	"dispatch": {
		{"Say what it would start, skip and queue, and why, once", "aries dispatch run --dry-run --once"},
		{"Scope a trial to cheap, labeled work", "aries dispatch run --models haiku --labels trial"},
		{"Stop it, interrupting its runs", "aries dispatch stop --now"},
	},
	"ready": {
		{"What to work on next", "aries ready"},
		{"The top five", "aries ready -n 5"},
	},
	"blocked": {
		{"What waits, and on what", "aries blocked"},
		{"The first twenty", "aries blocked -n 20"},
	},
	"update": {
		{"Raise the priority and assign it to bob", "aries update ar-a1b2c3d4 -p 0 --assignee bob"},
		{"Rename it, refused if it changed since rev 3", `aries update ar-a1b2c3d4 --rev 3 --title "Fix the redirect loop"`},
		{"Keep it out of ready until a date", "aries update ar-a1b2c3d4 --defer-until 2026-11-02"},
	},
	"close": {
		{"Close it, saying why", `aries close ar-a1b2c3d4 --reason "fixed in the next release"`},
		{"As an admin, despite open acceptance items", `aries close ar-a1b2c3d4 --force --reason "duplicate"`},
	},
	"reopen": {
		{"Reopen it", "aries reopen ar-a1b2c3d4"},
		{"Refused if it changed since rev 7", "aries reopen ar-a1b2c3d4 --rev 7"},
	},
	"dep": {
		{"ar-a1b2c3d4 waits until ar-e5f6a7b8 closes", "aries dep add ar-a1b2c3d4 ar-e5f6a7b8"},
		{"Drop that dependency", "aries dep remove ar-a1b2c3d4 ar-e5f6a7b8"},
	},
	"label": {
		{"Add two labels", "aries label add ar-a1b2c3d4 ui needs-review"},
		{"Remove one", "aries label remove ar-a1b2c3d4 needs-review"},
	},
	"comment": {
		{"Say what you found", `aries comment ar-a1b2c3d4 "It loops only behind the proxy"`},
		{"Read the comment from stdin", "aries comment ar-a1b2c3d4 -"},
	},
	"comments": {
		{"All of an issue's comments", "aries comments ar-a1b2c3d4"},
		{"The newest five", "aries comments ar-a1b2c3d4 -n 5"},
	},
	"history": {
		{"All of an issue's changes", "aries history ar-a1b2c3d4"},
		{"The newest ten", "aries history ar-a1b2c3d4 -n 10"},
	},
	"prime": {
		{"Orient yourself at the start of a session", "aries prime"},
		{"As Claude Code's SessionStart hook runs it", "aries prime --hook"},
		{"As Codex's hook runs it", "aries prime --hook=codex"},
	},
	"start": {
		{"Claim the top ready issue", "aries start"},
		{"Claim one and check its branch out here", "aries start ar-a1b2c3d4 --new-branch"},
		{"Claim it for 4 hours, in a worktree of its own", "aries start ar-a1b2c3d4 --for 4h --worktree ../login-fix"},
	},
	"finish": {
		{"Close it with a note for whoever comes next", `aries finish ar-a1b2c3d4 --reason fixed --handoff "PR open; CI green"`},
		{"Tick items 1 and 3, waive 2, and file a follow-up", `aries finish ar-a1b2c3d4 --tick 1,3 --waive "2=no UI" --discovered "Test it"`},
	},
	"accept": {
		{"Tick items 1 and 2", "aries accept ar-a1b2c3d4 1 2"},
		{"Untick item 2", "aries accept ar-a1b2c3d4 2 --undo"},
		{"Waive item 4, saying why", `aries accept ar-a1b2c3d4 4 --waive "no docs change"`},
	},
	"handoff": {
		{"Say where you stopped and what is next", `aries handoff ar-a1b2c3d4 "Tests pass" --state partial --next "Fix the docs"`},
		{"Hand it to bob and let the claim go", `aries handoff ar-a1b2c3d4 "Over to you" --to bob --release`},
	},
	"away": {
		{"Keep your claims for a long weekend", "aries away 3d"},
		{"Overnight", "aries away 16h"},
	},
	"lock": {
		{"Deploy under the lock, waiting while another holds it", "aries lock run deploy --wait -- make deploy"},
		{"Take it by hand for 10 minutes; it prints the epoch", "aries lock acquire deploy --for 10m"},
		{"Release it at the epoch acquire printed", "aries lock release deploy 12"},
	},
	"inbox": {
		{"Your unread items", "aries inbox"},
		{"Mark two read", "aries inbox --ack 12,13"},
		{"Read items too, fifty of them", "aries inbox --all -n 50"},
	},
	"watch": {
		{"Print inbox items as they arrive", "aries watch"},
		{"One JSON object a line, for a script", "aries --json watch"},
		{"Also every issue event, your own included", "aries --json watch --events"},
	},
	"who": {
		{"Who is at work now", "aries who"},
		{"Who was in the last two hours", "aries who --since 2h"},
	},
	"digest": {
		{"The last day", "aries digest"},
		{"alice's week", "aries digest --since 7d --by alice"},
		{"Since a date, of one label", "aries digest --since 2026-10-01 --label ui"},
	},
	"tui": {
		{"The board of this repository's project", "aries tui"},
		{"Of another repository's", "aries -C ../billing tui"},
	},
	"remember": {
		{"Keep what the next session needs", `aries remember deploy-steps "Deploy from main only" --tag deploy`},
		{"Replace revision 2 with stdin, and pin it", "aries remember deploy-steps - --rev 2 --pin"},
		{"A note of your own, linked to an issue", `aries remember login "Stale cookie" --scope user --issue ar-a1b2c3d4`},
	},
	"recall": {
		{"Memories that mention deploy", "aries recall deploy"},
		{"The five newest tagged deploy", "aries recall --tag deploy -n 5"},
		{"One by its key", "aries recall --key deploy-steps"},
	},
	"memories": {
		{"Every memory you can see", "aries memories"},
		{"Your own, fifty of them", "aries memories --scope user -n 50"},
	},
	"forget": {
		{"Delete a memory", "aries forget deploy-steps"},
		{"Refused if it changed since revision 3", "aries forget deploy-steps --rev 3"},
	},
	"pin": {
		{"Pin a project memory", "aries pin deploy-steps"},
		{"Pin one of your own", "aries pin style --scope user"},
	},
	"unpin": {
		{"Unpin a project memory", "aries unpin deploy-steps"},
		{"Unpin one of your own", "aries unpin style --scope user"},
	},
	"cost": {
		{"The last week's cost by account", "aries cost --since 7d"},
		{"October's, by person", "aries cost --since 2026-10-01 --until 2026-11-01 --group-by person"},
		{"The ten costliest epics of the week", "aries cost --since 7d --group-by epic -n 10"},
	},
	"outcome": {
		{"Record what a landed issue took, from Aries and the CI host", "aries outcome ar-a1b2c3d4"},
		{"Print the record of every outcome of the last day, posting nothing", "aries outcome --since 24h --dry-run"},
		{"Record the past 30 days' outcomes, marked as a backfill", "aries outcome --since 30d --backfill"},
	},
	"hours": {
		{"Log an hour and a half on an issue", `aries hours 1.5h ar-a1b2c3d4 --note "paired with bob"`},
		{"Log 90 minutes on an earlier day", "aries hours 90m ar-a1b2c3d4 --on 2026-10-16"},
		{"List what was logged on an issue", "aries hours --issue ar-a1b2c3d4"},
	},
	"usage": {
		{"As Claude Code's hooks run it, with the hook's JSON on stdin", "aries usage --hook"},
		{"Named outright", "aries usage --hook=claude-code"},
	},
	"gate": {
		{"Hold an issue until alice approves it", "aries gate add --blocks ar-a1b2c3d4 --approver alice"},
		{"Pass a gate, as its approver", `aries gate pass ar-e5f6a7b8 --reason "reviewed"`},
		{"Report from CI that the build passed on this branch", "aries gate pass --check build --from-branch --url https://ci.example.com/7"},
	},
	"land": {
		{"After git pull, report what landed", "aries land"},
		{"Say what it would report, and report nothing", "aries land --dry-run"},
		{"Start from a commit, on a branch of another name", "aries land --from a1b2c3d --branch trunk"},
	},
	"workflow": {
		{"Check the server's workflow against the file", "aries workflow check workflow.yaml"},
		{"The same, as one JSON document", "aries --json workflow check workflow.yaml"},
	},
	"outbox": {
		{"What waits for the server, and what it refused", "aries outbox"},
		{"Send refused entry 3 again", "aries outbox retry 3"},
		{"The counts of what was queued, and the outages", "aries outbox stats"},
	},
	"cache": {
		{"What this machine keeps for the project", "aries cache"},
		{"Delete it", "aries cache clear"},
	},
	"conflicts": {
		{"Every open conflict", "aries conflicts"},
		{"An issue's", "aries conflicts ar-a1b2c3d4"},
		{"Conflict 3, whole", "aries conflicts 3"},
	},
	"resolve": {
		{"Keep the server's value", "aries resolve 3 --theirs"},
		{"Set new text", `aries resolve 3 --text "Fix the login redirect loop"`},
		{"Do what its proposal says", "aries resolve 3 --accept"},
	},
	"setup": {
		{"See what setting Claude Code up would change", "aries setup claude-code"},
		{"Then change it", "aries setup claude-code --write"},
		{"Check every agent's setup", "aries setup --all --check"},
	},
	"login": {
		{"Sign in, in the browser that opens", "aries login"},
		{"No browser here: print the URL to open elsewhere", "aries login --url"},
		{"A CI job, with its platform's token", "aries login --workload github-actions"},
	},
	"logout": {
		{"End your sign-in", "aries logout"},
		{"For another repository's server", "aries -C ../billing logout"},
	},
	"whoami": {
		{"Who the server takes you for", "aries whoami"},
		{"As one JSON document", "aries --json whoami"},
	},
	"mcp": {
		{"What an agent's MCP config runs", "aries mcp"},
		{"For another repository's project", "aries -C ../billing mcp"},
	},
	"upgrade": {
		{"Say whether there is a newer release", "aries upgrade --check"},
		{"Install it", "aries upgrade"},
		{"Put the replaced binary back", "aries upgrade --rollback"},
	},
	"version": {
		{"This binary's version", "aries version"},
		{"With the protocol it speaks", "aries --json version"},
	},
	"admin": {
		{"Add bob as a principal, with a public key", "aries admin principal add bob --key bob.pub"},
		{"Make bob an admin of the project", "aries admin member add bob --role admin"},
		{"What the server knows of itself", "aries admin status"},
	},
}

// help prints the commands to w, by task.
func (r *runner) help(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: aries [-C DIR] [--json] [--no-queue] COMMAND [ARGS]")
	for _, g := range helpGroups {
		_, _ = fmt.Fprintf(w, "\n%s:\n", g.title)
		for _, l := range g.commands {
			_, _ = fmt.Fprintf(w, "  %-10s %s\n", l.name, l.brief)
		}
	}
	_, _ = fmt.Fprint(w, `
Global flags (--json and --no-queue may also follow the command):
  -C DIR      run as if started in DIR
  --json      print exactly one JSON document, errors included
  --no-queue  fail, rather than queue, a write that cannot reach the server

`+"`aries help COMMAND`, or `aries COMMAND --help`, shows its flags and examples."+`
Exit codes: 0 ok, 1 failure, 2 usage, 3 protocol refused.
`)
}

// helpFlags are the ways of asking for help that the flag package takes.
var helpFlags = []string{"-h", "--h", "-help", "--help"}

// helpAsked reports whether args ask for help before any "--".
func helpAsked(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		for _, h := range helpFlags {
			if a == h {
				return true
			}
		}
	}
	return false
}

// commandHelp prints c's help and returns ExitOK. args are what followed
// the command's name, a subcommand among them (`aries help admin member`,
// `aries gate add --help`): c runs with them and -h, which its flag set,
// or its subcommand's, answers before anything else is done, so the help
// lists the flags of the one asked. A command that dispatches before it
// parses (gate, lock, admin) gets its own help, without flags.
func (r *runner) commandHelp(ctx context.Context, c *command, args []string) int {
	r.helping = true
	var asked []string
	for i, a := range args {
		if a == "--" {
			break
		}
		if !helpAsked([]string{a}) {
			asked = append(asked, args[i])
		}
	}
	err := c.run(ctx, r, append(asked, "-h"))
	usage, fs := c.usage, (*flag.FlagSet)(nil)
	if ue, ok := errors.AsType[*usageError](err); ok && ue.flags != nil {
		usage, fs = ue.usage, ue.flags
	}
	r.printCommandHelp(r.env.Stdout, c, usage, fs, asked)
	return ExitOK
}

// printCommandHelp prints a command's usage, summary, flags and examples:
// those of the subcommand sub names, when it has its own.
func (r *runner) printCommandHelp(w io.Writer, c *command, usage string, fs *flag.FlagSet, sub []string) {
	for i, piece := range splitUsage(usage, c.name) {
		lead := "usage: aries "
		if i > 0 {
			lead = "       aries "
		}
		writeUnits(w, lead, strings.Repeat(" ", len(lead)+2), usageUnits(piece))
	}
	_, _ = fmt.Fprintln(w)
	writeWrapped(w, "", "", c.summary)
	if fs != nil {
		lines := flagLines(fs, usage)
		if len(lines) > 0 {
			_, _ = fmt.Fprintln(w, "\nflags:")
			width := 0
			for _, l := range lines {
				width = max(width, len(l[0]))
			}
			width = min(width, 22)
			for _, l := range lines {
				if len(l[0]) > width {
					_, _ = fmt.Fprintf(w, "  %s\n", l[0])
					writeWrapped(w, strings.Repeat(" ", width+4), strings.Repeat(" ", width+4), l[1])
					continue
				}
				writeWrapped(w, fmt.Sprintf("  %-*s  ", width, l[0]), strings.Repeat(" ", width+4), l[1])
			}
		}
	}
	if ex := commandExamples(c, sub); len(ex) > 0 {
		_, _ = fmt.Fprintln(w, "\nexamples:")
		for i, e := range ex {
			if i > 0 {
				_, _ = fmt.Fprintln(w)
			}
			_, _ = fmt.Fprintf(w, "  # %s\n  %s\n", e.does, e.line)
		}
	}
}

// commandExamples are the command's examples of the subcommand sub
// names (admin member, gate add), or all of them when sub names none, or
// none has an example.
func commandExamples(c *command, sub []string) []example {
	all := examples[c.name]
	if len(sub) == 0 || !isSubcommand(c.usage, sub[0]) {
		return all
	}
	var of []example
	for _, e := range all {
		if strings.HasPrefix(e.line, "aries "+c.name+" "+sub[0]+" ") {
			of = append(of, e)
		}
	}
	if len(of) == 0 {
		return all
	}
	return of
}

// isSubcommand reports whether word is one of the literal words of a
// usage line ("add" in "dep add|remove FROM TO"), not a placeholder or an
// argument such as an issue's id.
func isSubcommand(usage, word string) bool {
	for _, f := range strings.Fields(usage) {
		if slices.Contains(strings.Split(f, "|"), word) {
			return true
		}
	}
	return false
}

// splitUsage splits a usage line into its forms, at each " | " that
// starts the command's name again ("hours … | hours --undo ENTRY").
func splitUsage(usage, name string) []string {
	forms := strings.Split(usage, " | "+name+" ")
	for i := 1; i < len(forms); i++ {
		forms[i] = name + " " + forms[i]
	}
	return forms
}

// flagLines are a flag set's flags, but --json and --no-queue, which the
// top help names: each one's names with its value's placeholder, and what
// it does with its default. Two names of one value (-t and --type) are one
// line, the short name first.
func flagLines(fs *flag.FlagSet, usage string) [][2]string {
	type entry struct {
		flags []*flag.Flag
		does  string
	}
	var entries []*entry
	byValue := map[uintptr]*entry{}
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "json" || f.Name == "no-queue" {
			return
		}
		var key uintptr
		if v := reflect.ValueOf(f.Value); v.Kind() == reflect.Pointer {
			key = v.Pointer()
		}
		if e := byValue[key]; key != 0 && e != nil {
			e.flags = append(e.flags, f)
			if len(f.Usage) > len(e.does) {
				e.does = f.Usage
			}
			return
		}
		e := &entry{flags: []*flag.Flag{f}, does: f.Usage}
		entries = append(entries, e)
		if key != 0 {
			byValue[key] = e
		}
	})
	lines := make([][2]string, 0, len(entries))
	for _, e := range entries {
		slices.SortStableFunc(e.flags, func(a, b *flag.Flag) int { return len(a.Name) - len(b.Name) })
		var names []string
		value := ""
		for _, f := range e.flags {
			if len(f.Name) == 1 {
				names = append(names, "-"+f.Name)
			} else {
				names = append(names, "--"+f.Name)
			}
			if v, named := placeholder(f, usage); value == "" || named {
				value = v
			}
		}
		name := strings.Join(names, ", ")
		if value != "" {
			name += " " + value
		}
		does := e.does
		switch def := e.flags[0].DefValue; def {
		case "", "0", "false", "[]":
		default:
			does += " (default " + def + ")"
		}
		lines = append(lines, [2]string{name, does})
	}
	return lines
}

// placeholder is what a flag's value is called: the word after the flag
// in the usage line (--for DURATION, --role member|admin), with named
// true, else its type
// (STRING, INT). A boolean flag has none.
func placeholder(f *flag.Flag, usage string) (value string, named bool) {
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "", false
	}
	words := strings.Fields(usage)
	for i, word := range words[:max(len(words)-1, 0)] {
		word = strings.TrimLeft(word, "[")
		if word != "-"+f.Name && word != "--"+f.Name {
			continue
		}
		next := strings.TrimRight(strings.TrimSuffix(strings.TrimRight(words[i+1], "]"), "..."), "]")
		if next != "" && !strings.HasPrefix(next, "-") && !strings.HasPrefix(next, "|") &&
			(strings.ContainsAny(next, "ABCDEFGHIJKLMNOPQRSTUVWXYZ|")) {
			return next, true
		}
	}
	if g, ok := f.Value.(flag.Getter); ok {
		switch g.Get().(type) {
		case string:
			return "TEXT", false
		case int, int64, uint, uint64:
			return "N", false
		case time.Duration:
			return "DURATION", false
		}
	}
	return "VALUE", false
}

// usageUnits are a usage line's words, with each bracketed group one
// unit, so that a line is not broken inside "[--worktree DIR]".
func usageUnits(usage string) []string {
	var units []string
	depth := 0
	for _, word := range strings.Fields(usage) {
		if depth > 0 {
			units[len(units)-1] += " " + word
		} else {
			units = append(units, word)
		}
		depth += strings.Count(word, "[") - strings.Count(word, "]")
	}
	return units
}

// writeWrapped writes text to w in lines of at most helpWidth, broken at
// spaces: the first after lead, the rest after indent.
func writeWrapped(w io.Writer, lead, indent, text string) {
	writeUnits(w, lead, indent, strings.Fields(text))
}

// writeUnits writes units to w, a space between each, in lines of at most
// helpWidth: the first after lead, the rest after indent. A unit longer
// than a line is broken into its words.
func writeUnits(w io.Writer, lead, indent string, units []string) {
	var words []string
	for _, u := range units {
		if len(indent)+len(u) > helpWidth {
			words = append(words, strings.Fields(u)...)
		} else {
			words = append(words, u)
		}
	}
	line := lead
	empty := true
	for _, word := range words {
		if !empty && len(line)+1+len(word) > helpWidth {
			_, _ = fmt.Fprintln(w, line)
			line, empty = indent, true
		}
		if !empty {
			line += " "
		}
		line += word
		empty = false
	}
	_, _ = fmt.Fprintln(w, line)
}
