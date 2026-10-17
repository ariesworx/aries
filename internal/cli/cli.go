package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/board"
	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/mcpserver"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/safetext"
)

// Exit codes, as [Run] returns them.
const (
	// ExitOK: the command succeeded.
	ExitOK = 0
	// ExitFailure: the command failed, or the server refused it.
	ExitFailure = 1
	// ExitUsage: a mistake on the command line. Bare aries prints help and
	// exits with it too.
	ExitUsage = 2
	// ExitVersion: this client's protocol version is outside the server's
	// range.
	ExitVersion = 3
)

// Env is the process environment Run works in: everything the package
// takes from the process. A field left nil or empty takes the default its
// comment names; Stdout and Stderr must be set.
type Env struct {
	// Stdin is read for a text given as "-", by prime --hook, usage
	// --hook and aries mcp. Default empty.
	Stdin io.Reader
	// Stdout and Stderr receive all output.
	Stdout, Stderr io.Writer
	// Getenv reads an environment variable. Default os.Getenv.
	Getenv func(string) string
	// Hostname names this machine to the server. Default os.Hostname.
	Hostname func() (string, error)
	// UserHomeDir finds the home directory, for the default SSH identities, `setup --global` and a
	// desktop app's config. Default os.UserHomeDir.
	UserHomeDir func() (string, error)
	// UserCacheDir finds the cache directory, where `usage --hook` keeps
	// its transcript offsets. Default os.UserCacheDir.
	UserCacheDir func() (string, error)
	// GOOS and Executable are used only by `setup claude-desktop`: the
	// platform, and the absolute path of this aries. Defaults runtime.GOOS,
	// and aries's entry on PATH when that is this program, else
	// os.Executable.
	GOOS       string
	Executable func() (string, error)
	// Version is this binary's build version.
	Version string
	// Upgrader overrides what `upgrade` uses. Nil uses the releases
	// $ARIES_RELEASES names, or the default source, the built-in
	// release keys and this executable.
	Upgrader *Upgrader
	// Now is the time in the user's time zone, whose date `hours` sends as
	// the day worked. Default time.Now.
	Now func() time.Time
	// Terminal reports whether a person is at Stdin and Stdout, so that a
	// command may ask them something. Default: whether both are
	// terminals.
	Terminal func() bool
	// Environ is the environment `lock run` gives the command it runs,
	// with ARIES_LOCK_EPOCH added. Default os.Environ.
	Environ func() []string
	// After is the clock `lock` waits on: how often it renews a place in
	// a queue or a hold, and how long it gives a command to stop. Default
	// time.After.
	After func(time.Duration) <-chan time.Time
	// OpenBrowser opens a URL in the person's browser, for `aries login`.
	// Nil opens none: login prints the URL instead. cmd/aries sets
	// OpenBrowser(runtime.GOOS).
	OpenBrowser func(url string) error
}

// usageError is a mistake on the command line. usage is the usage line
// its fix shows; Run fills in the command's own when it is empty.
type usageError struct{ msg, usage string }

func (e *usageError) Error() string { return e.msg }

// refusedWith is a refusal that has something to show with it. With
// --json the one document printed holds the error and, under key, value.
type refusedWith struct {
	err   *proto.Error
	key   string
	value any
}

func (e *refusedWith) Error() string { return e.err.Error() }

func (e *refusedWith) Unwrap() error { return e.err }

// usagef returns a usageError with a formatted message.
func usagef(usage, format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...), usage: usage}
}

// command is one aries command: its name, its usage line without "aries ",
// the summary help prints, and the function that runs it.
type command struct {
	name    string
	usage   string
	summary string
	run     func(ctx context.Context, r *runner, args []string) error
}

// commands lists the commands in the order help prints them.
var commands []command

func init() {
	commands = []command{
		{"create", "create TITLE... [-p N] [-t TYPE] [--body TEXT|-] [--parent ID] [--label L]... [--defer-until DATE] [--due DATE]", "create an issue and print its id", cmdCreate},
		{"show", "show ID [--compact]", "show an issue and its dependencies", cmdShow},
		{"list", "list [--status S,S] [--all] [-t TYPE] [-p N,N] [--assignee A] [--parent ID] [--label L]... [-n N] [--cursor C]", "list issues (open ones unless --status or --all)", cmdList},
		{"ready", "ready [-n N]", "list issues nothing holds back", cmdReady},
		{"blocked", "blocked [-n N]", "list issues held back by open blockers", cmdBlocked},
		{"update", "update ID [--rev N] [--title T] [--body TEXT|-] [--status S] [-p N] [-t TYPE] [--assignee A] [--parent ID] ...", "change an issue's fields", cmdUpdate},
		{"close", "close ID [--reason TEXT] [--rev N] [--force]", "close an issue (--force, admins only: despite open acceptance items)", cmdClose},
		{"reopen", "reopen ID [--rev N]", "reopen a closed issue", cmdReopen},
		{"land", landUsage, "report landed for the commits of the default branch since the last reported, for a server CI cannot reach; run it after git pull", cmdLand},
		{"gate", gateUsage, "hold issues back until a person passes a gate, or CI reports a check: add, pass, fail, waive, rearm or list gates", cmdGate},
		{"workflow", workflowCheckUsage, "check that the server's workflow is the one in FILE; exits 1 when it differs", cmdWorkflow},
		{"lock", lockUsage, "take, renew, release or list a lock, a leased mutex or semaphore for a deploy or a merge; run a command under one", cmdLock},
		{"dep", depUsage, "add or remove a dependency: FROM depends on TO", cmdDep},
		{"label", labelUsage, "add or remove labels", cmdLabel},
		{"comment", "comment ID TEXT...|-", "add a comment", cmdComment},
		{"comments", "comments ID [-n N]", "list an issue's comments", cmdComments},
		{"history", "history ID [-n N]", "list an issue's changes", cmdHistory},
		{"start", startUsage, "claim an issue (the top ready one without ID) and show it", cmdStart},
		{"finish", "finish ID [--reason TEXT] [--handoff TEXT|-] [--state S] [--next TEXT] [--branch B] [--worktree DIR] [--to P] [--discovered TITLE]... [--tick N,N] [--waive N=REASON]... [--epoch N]", "close your issue with a handoff note and discovered work", cmdFinish},
		{"accept", "accept ID N... [--undo | --waive REASON]", "tick acceptance items; --undo unticks, --waive waives them for a reason", cmdAccept},
		{"handoff", "handoff ID NOTE...|- [--state S] [--next TEXT] [--branch B] [--worktree DIR] [--to P] [--release] [--epoch N]", "leave a note for whoever continues; --release lets it go", cmdHandoff},
		{"inbox", "inbox [--all] [-n N] [--ack ID]... [--ack-all]", "list your unread lost claims, handoffs, mentions and assignments; --ack marks read", cmdInbox},
		{"watch", "watch", "print inbox items as they arrive, until interrupted", cmdWatch},
		{"tui", "tui", "show a live board: ready, held and blocked issues and recent events, until q", cmdTUI},
		{"away", "away DURATION", "extend all your claims, e.g. before going offline (1m to the server's away_max, 7d by default)", cmdAway},
		{"digest", "digest [--since 24h|7d|DATE|TIME] [--by PRINCIPAL] [--label L]", "summarize what closed, started, stalled, is blocked and was handed off", cmdDigest},
		{"who", "who [--since DURATION] [-n N]", "list the agents at work and the issues each holds (seen in the last 5m)", cmdWho},
		{"whoami", "whoami", "say which principal the server takes you for, and which sign-in or key let you in", cmdWhoami},
		{"login", loginUsage, "sign in at the server's identity provider, for the day; --url when no browser opens here", cmdLogin},
		{"logout", "logout", "end your sign-in at the server, and remove it from this machine", cmdLogout},
		{"cost", costUsage, "report the list-price and amortized cost of tokens, and the hours logged, by account, issue, epic, person or model", cmdCost},
		{"hours", hoursUsage, "log your hours on an issue (1.5h, 90m), undo an entry, or list entries", cmdHours},
		{"remember", "remember KEY TEXT...|- [--scope S] [--tag T]... [--issue ID] [--rev N] [--pin]", "keep a memory for later sessions; --rev replaces one", cmdRemember},
		{"recall", "recall [TEXT...] [--key K] [--tag T] [--scope S] [-n N]", "search memories by text, key or tag, pinned then newest first", cmdRecall},
		{"memories", "memories [--scope S] [-n N]", "list the memories you can see, pinned then newest first", cmdMemories},
		{"forget", "forget KEY [--scope S] [--rev N]", "delete a memory", cmdForget},
		{"pin", "pin KEY [--scope S]", "pin a memory, so prime always shows it", cmdPin},
		{"unpin", "unpin KEY [--scope S]", "unpin a memory", cmdUnpin},
		{"prime", "prime [--hook[=AGENT]]", "orient a session: your in-progress issues, top ready work, memories, notices", cmdPrime},
		{"usage", "usage --hook[=AGENT]", "send the session's token counts from the agent's transcript (run by its hooks)", cmdUsage},
		{"conflicts", conflictsUsage, "list the fields of replayed updates parked against others' changes, or show one whole", cmdConflicts},
		{"resolve", resolveUsage, "resolve a conflict: the replayed update's value, the server's, new text, or as proposed", cmdResolve},
		{"outbox", outboxUsage, "list the writes that wait for the server and the ones it refused; retry or drop one; show the counts", cmdOutbox},
		{"cache", cacheUsage, "say what this machine keeps of what was read, shown while the server cannot be reached; clear deletes it", cmdCache},
		{"mcp", "mcp", "serve the MCP tools for an agent on stdin and stdout", cmdMCP},
		{"setup", "setup AGENT|--all|post-merge [--write|--check|--remove] [--global [--key-file PATH]] [--command PATH]", "set agents up: MCP config, instruction pointer, session hook", cmdSetup},
		{"admin", priceUsage + " | " + planUsage + " | " + adminUsage,
			"list model prices and subscription plans, or set one; manage a server's projects, principals, members and keys, and a project's workflow (admins only)", cmdAdmin},
		{"upgrade", "upgrade [--check] [--rollback]", "replace aries with the latest verified release; --rollback undoes it", cmdUpgrade},
		{"version", "version", "print the aries version", cmdVersion},
	}
}

// aliases are other names of commands, which help does not list: log,
// hours's name before 1.0, which is kept through 1.x.
var aliases = map[string]string{"log": "hours"}

// lookup returns the command named name, or by an alias name, or nil.
func lookup(name string) *command {
	if a, ok := aliases[name]; ok {
		name = a
	}
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// runner carries one invocation's settings and its lazily opened
// connection.
type runner struct {
	// env is Run's Env, with defaults filled in and, once wrapOutput has
	// run, Stdout and Stderr escaped.
	env Env
	// json is --json, given before or after the command name.
	json bool
	// noQueue is --no-queue, given before or after the command name: a
	// write that does not reach the server fails, and is not queued.
	noQueue bool
	// hook says the command runs as an agent's hook (prime --hook, usage
	// --hook), which does as it did before there was an outbox: it sends
	// nothing that waits, and notes nothing on the disk but who the
	// server took its key for, and the cache: what it read, and the
	// deletion of a principal's cache when access ended.
	hook bool
	// cacheable says the command's reads are answered from this
	// machine's copy while the server cannot be reached (show, inbox,
	// digest; prime has its own); cachedAt, once one was, is when that
	// copy was kept.
	cacheable bool
	cachedAt  time.Time
	// moved is the line that says the server took this connection's key
	// for another principal than before, whose queued writes it leaves
	// unsent; "" when it did not.
	moved string
	// dir is the directory commands run in, where the search for
	// .aries.yaml starts: -C's, else the hook input's cwd for prime
	// --hook, else ".".
	dir string
	// conn is the connection connect opened, if any; Run closes it.
	conn *mcpserver.RepoConn
	// session overrides the environment's session id (prime and usage
	// --hook).
	session string
	// quiet drops the server's version warning: a hook's stderr carries
	// one note at most (usage --hook).
	quiet bool
	// onPush, if set before connecting, takes the events the server
	// pushes (aries watch).
	onPush func(proto.Push)
	// rawOut and rawErr are standard output and error before wrapOutput:
	// rawOut for JSON, and both for the command `lock run` runs, whose
	// output is its own. outs are the escaping writers wrapOutput put in
	// place.
	rawOut, rawErr io.Writer
	outs           []*safetext.Writer
}

// wrapOutput routes everything printed through safetext writers. Issue
// text, names, server messages and server stderr come from other people
// and from the server; a control or bidi character in them is shown as an
// escape (\x1b, \u202e), so it cannot clear the screen, set the clipboard,
// hide a link or rewrite a line (C-3). Printers also escape single-line
// fields with safetext.Line, so a newline in one cannot start a line of
// its own. JSON goes to the raw output, escaped by emit.
func (r *runner) wrapOutput() {
	r.rawOut, r.rawErr = r.env.Stdout, r.env.Stderr
	out, errw := safetext.NewWriter(r.env.Stdout), safetext.NewWriter(r.env.Stderr)
	r.env.Stdout, r.env.Stderr, r.outs = out, errw, []*safetext.Writer{out, errw}
}

// mcpOut is standard output for a protocol, not for text: aries mcp's
// JSON-RPC, which encoding/json already escapes.
func (r *runner) mcpOut() io.Writer {
	if r.rawOut != nil {
		return r.rawOut
	}
	return r.env.Stdout
}

// esc is safetext.Line, for a single-line field in text output: a title,
// a name, a label, a reason, an id from the server.
func esc(s string) string { return safetext.Line(s) }

// escAll is esc on each of xs.
func escAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = esc(x)
	}
	return out
}

// flush writes out anything the escaping writers hold.
func (r *runner) flush() {
	for _, w := range r.outs {
		_ = w.Flush()
	}
}

// Run executes one aries command line, without the program name, and
// returns its exit code: [ExitUsage] for a mistake on the command line or
// a bare aries, which prints help; [ExitVersion] when this client's protocol
// version is outside the server's range; [ExitFailure] for any other
// failure. It prints a failure on Stderr as an `aries:` line and, when the
// error has one, a `fix:` line, or with --json as one JSON document on
// Stdout. Run keeps no state between calls.
func Run(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Hostname == nil {
		env.Hostname = os.Hostname
	}
	if env.UserHomeDir == nil {
		env.UserHomeDir = os.UserHomeDir
	}
	if env.UserCacheDir == nil {
		env.UserCacheDir = os.UserCacheDir
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	if env.GOOS == "" {
		env.GOOS = runtime.GOOS
	}
	if env.Executable == nil {
		env.Executable = ariesPath
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	if env.Terminal == nil {
		in, out := env.Stdin, env.Stdout
		env.Terminal = func() bool { return board.IsTerminal(in, out) }
	}
	if env.Environ == nil {
		env.Environ = os.Environ
	}
	if env.After == nil {
		env.After = time.After
	}
	r := &runner{env: env, dir: "."}
	r.wrapOutput()
	defer r.flush()
	args, err := r.globals(args)
	if err != nil {
		return r.fail(err)
	}
	if len(args) == 0 || args[0] == "help" {
		if len(args) > 1 {
			if c := lookup(args[1]); c != nil {
				_, _ = fmt.Fprintf(env.Stdout, "usage: aries %s\n\n%s\n", c.usage, c.summary)
				return ExitOK
			}
		}
		r.help(env.Stdout)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	c := lookup(args[0])
	if c == nil {
		return r.fail(usagef("", "unknown command %q", args[0]))
	}
	err = c.run(ctx, r, args[1:])
	if r.conn != nil {
		_ = r.conn.Close()
	}
	if err != nil {
		if ee, ok := errors.AsType[exitError](err); ok {
			return ee.code // the command lock run ran said all it had to
		}
		var ue *usageError
		if errors.As(err, &ue) && ue.usage == "" {
			ue.usage = c.usage
		}
		return r.fail(err)
	}
	return ExitOK
}

// globalUsage is the usage line of a mistake before the command name.
const globalUsage = "aries [-C DIR] [--json] [--no-queue] COMMAND"

// globals consumes -C DIR, --json, --no-queue and -h before the command
// name.
func (r *runner) globals(args []string) ([]string, error) {
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "-C" || a == "--C":
			if len(args) < 2 {
				return nil, usagef(globalUsage, "-C needs a directory")
			}
			r.dir, args = args[1], args[2:]
		case strings.HasPrefix(a, "-C="):
			r.dir, args = strings.TrimPrefix(a, "-C="), args[1:]
		case a == "--json" || a == "-json":
			r.json, args = true, args[1:]
		case a == "--no-queue" || a == "-no-queue":
			r.noQueue, args = true, args[1:]
		case a == "-h" || a == "--help" || a == "-help":
			return []string{"help"}, nil
		case a == "--version" || a == "-version":
			return []string{"version"}, nil
		case strings.HasPrefix(a, "-"):
			return nil, usagef(globalUsage, "unknown flag %s", a)
		default:
			return args, nil
		}
	}
	return args, nil
}

// help prints the list of commands to w.
func (r *runner) help(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: aries [-C DIR] [--json] [--no-queue] COMMAND [ARGS]")
	_, _ = fmt.Fprintln(w)
	for _, c := range commands {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "`aries help COMMAND` shows a command's usage. --json prints one JSON document.")
	_, _ = fmt.Fprintln(w, "A write that cannot reach the server is queued and sent later (`aries outbox`); --no-queue makes it fail.")
}

// fail reports err and returns its exit code. A usage error's fix is the
// command's usage line; a *proto.Error keeps its code, message and fix,
// but for a principal at its connection limit, whose fix is
// proto.FixConnLimit whatever server sent it, and prints no fix: line
// when the fix is empty; any other error is
// reported as unavailable, with a fix to retry and check the config.
//
// A refusal the server answers with unavailable is the server's own
// failure, and prints a third line, req:, the id the server gave the
// request: its log has the cause under that id. --json carries the id
// of any refusal that has one.
func (r *runner) fail(err error) int {
	code, exit := proto.CodeUnavailable, ExitFailure
	msg, fix, req := err.Error(), "retry; if it persists, run with -C pointing at the repository and check "+client.ConfigFile, ""
	var ue *usageError
	var pe *proto.Error
	switch {
	case errors.As(err, &ue):
		code, exit, msg = proto.CodeInvalid, ExitUsage, ue.msg
		fix = "run `aries help` for the commands"
		if ue.usage != "" {
			fix = "usage: aries " + ue.usage
		}
	case errors.As(err, &pe):
		code, msg, fix, req = pe.Code, pe.Message, pe.Fix, pe.Req
		if pe.Code == proto.CodeVersion {
			exit = ExitVersion
		}
		if proto.AtConnLimit(pe) {
			fix = proto.FixConnLimit // a released server's fix is the one it gives its own limit
		}
	}
	if r.json {
		doc := map[string]any{"error": jsonError{Code: code, Message: msg, Fix: fix, Req: req}}
		if rw, ok := errors.AsType[*refusedWith](err); ok {
			doc[rw.key] = rw.value
		}
		r.emit(doc)
	} else {
		// The message and fix may be the server's: one line each.
		_, _ = fmt.Fprintf(r.env.Stderr, "aries: %s\n", safetext.Line(msg))
		if fix != "" {
			_, _ = fmt.Fprintf(r.env.Stderr, "fix: %s\n", safetext.Line(fix))
		}
		if req != "" && code == proto.CodeUnavailable {
			_, _ = fmt.Fprintf(r.env.Stderr, "req: %s\n", safetext.Line(req))
		}
	}
	return exit
}

// jsonError is a refusal as --json prints it: the shape the MCP server
// gives an agent, with the request's id, rather than the wire's
// one-letter keys, which a script should not need to know.
type jsonError struct {
	Code    proto.Code `json:"code"`
	Message string     `json:"message"`
	Fix     string     `json:"fix,omitempty"`
	Req     string     `json:"req,omitempty"`
}

// emit writes v as one JSON document. encoding/json escapes the C0
// controls; safetext.JSON escapes DEL, C1 and bidi characters as \uXXXX
// too, so the document is the same value and safe on a terminal.
func (r *runner) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		_, _ = fmt.Fprintf(r.env.Stderr, "aries: encode output: %v\n", err)
		return
	}
	out := r.rawOut
	if out == nil {
		out = r.env.Stdout
	}
	_, _ = fmt.Fprintf(out, "%s\n", safetext.JSON(r.withCached(b)))
}

// notef prints a line for the person on standard error, as aries's.
func (r *runner) notef(format string, a ...any) {
	_, _ = fmt.Fprintf(r.env.Stderr, "aries: "+format+"\n", a...)
}

// warn prints a warning the server sent with a result, on standard
// error; an empty one prints nothing. With --json the warning is in the
// document, and this is not called.
func (r *runner) warn(warning string) {
	if warning != "" {
		_, _ = fmt.Fprintf(r.env.Stderr, "aries: warning: %s\n", esc(warning))
	}
}

// connect dials the server on first use, and prints the server's version
// warning and the config's, if there are any, on Stderr. A new connection
// first sends what waits in the outbox (delivered), before the request
// that opened it.
func (r *runner) connect(ctx context.Context) (*mcpserver.RepoConn, error) {
	if r.conn != nil {
		return r.conn, nil
	}
	c, err := mcpserver.DialRepo(ctx, r.dir, r.clientOptions())
	if err != nil {
		r.missed(err)
		return nil, err
	}
	if w := c.Warning(r.env.Version); w != "" && !r.quiet {
		_, _ = fmt.Fprintf(r.env.Stderr, "aries: %s\n", esc(w))
	}
	if !r.quiet {
		r.warn(c.ConfigWarning())
	}
	r.conn = c
	if err := r.delivered(ctx); err != nil {
		return nil, err
	}
	return r.conn, nil
}

// clientOptions describe this machine and session to the server. The
// session id is r.session (prime --hook's), else the harness's from the
// environment, else a CI job's or a person's (client.DefaultSession).
func (r *runner) clientOptions() client.Options {
	host, err := r.env.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	session := r.session
	if session == "" {
		session = client.DefaultSession(r.env.Getenv)
	}
	// The trace context is the environment's, read once for the process:
	// right for a command, which is one piece of work. `aries mcp` lives
	// for many agent turns: a tool call that brings a trace context of
	// its own, in its request's metadata, sends that one instead
	// (mcpserver, client.WithTrace), and every other request of the
	// connection carries this one.
	// A sign-in kept in the data directory is presented in place of a
	// key over HTTPS; with no data directory, there is none to find.
	data, _ := offline.DataDir(r.env.GOOS, r.env.Getenv, r.env.UserHomeDir)
	return client.Options{Version: r.env.Version, Session: session,
		Machine: host, Harness: client.HarnessFromEnv(r.env.Getenv), Getenv: r.env.Getenv, UserHomeDir: r.env.UserHomeDir,
		Trace: client.TraceFromEnv(r.env.Getenv), OnPush: r.onPush, DataDir: data, Now: r.env.Now}
}

// call runs one operation, and keeps its answer when the cache keeps
// such answers (design §18.6). When the server is not reached, a command
// that reads from the cache gets this machine's copy (answered); a read
// that is never answered from it says so (refusedOffline); and a write
// the outbox does not queue says so in its fix (unqueued).
func (r *runner) call(ctx context.Context, op string, args, result any) error {
	c, err := r.connect(ctx)
	if err == nil {
		err = c.Call(ctx, op, args, result)
		r.missed(err)
		if err == nil {
			r.keep(c, op, args, result)
			return nil
		}
	}
	if r.answered(op, args, result, err) {
		return nil
	}
	return r.unqueued(op, r.refusedOffline(op, err))
}

// newFlags returns a flag set that also accepts --json after the command.
func (r *runner) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&r.json, "json", r.json, "print one JSON document")
	fs.BoolVar(&r.noQueue, "no-queue", r.noQueue, "fail, and queue nothing, when the server cannot be reached")
	return fs
}

// parse parses flags anywhere among the arguments; everything after "--" is
// positional.
func parse(fs *flag.FlagSet, args []string, usage string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, usagef(usage, "help requested")
			}
			return nil, usagef(usage, "%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, tail...), nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// set reports whether the flag was given.
func set(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// listFlag collects a repeatable or comma-separated flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// priority parses "1", "p1" or "P1".
func priority(s string) (int, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "P"), "p")
	if len(s) != 1 || s[0] < '0' || s[0] > '4' {
		return 0, errors.New("priority must be 0-4 (or P0-P4)")
	}
	return int(s[0] - '0'), nil
}
