package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/offline"
	"github.com/ariesworx/starfix/internal/outbox"
	"github.com/ariesworx/starfix/internal/proto"
)

// away is this invocation's way to its outboxes (internal/offline).
func (r *runner) away() *offline.Client {
	o := r.clientOptions()
	return &offline.Client{Dir: r.dir, GOOS: r.env.GOOS, Getenv: r.env.Getenv, UserHomeDir: r.env.UserHomeDir,
		Now: r.env.Now, Session: o.Session, Machine: o.Machine}
}

// missed notes, for `sfx outbox`, what err says of the server: that it
// was not reached just now, or that it refused the principal's access to
// the project, which refuses what the principal queued for it, and says
// so on Stderr. A hook notes nothing of that. Access ended also deletes
// the principal's cache of the project, in a hook too, which says
// nothing of it.
func (r *runner) missed(err error) {
	if line := r.away().Forget(err); line != "" && !r.hook {
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: %s\n", esc(line))
	}
	switch {
	case r.hook:
	case client.Unreachable(err):
		r.away().Missed()
	default:
		if line := r.away().Ended(err); line != "" {
			_, _ = fmt.Fprintf(r.env.Stderr, "sfx: %s\n", esc(line))
		}
	}
}

// delivered runs on a new connection, before the request that opened
// it: it notes who the server took the key for, and sends what waits in
// that principal's outbox, within the outbox's replay budget; what is
// left goes with the next command. It says on Stderr, in one line, what
// was delivered and what was refused. A hook sends nothing and says
// nothing.
//
// A replay that ran out of time has closed the connection, as a canceled
// call does; the command's own request then gets a new one.
func (r *runner) delivered(ctx context.Context) error {
	off := r.away()
	r.moved = off.Connected(r.conn)
	if r.hook {
		return nil
	}
	if r.moved != "" {
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: %s\n", esc(r.moved))
	}
	rep, err := off.Replay(ctx, r.conn)
	if err != nil {
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: outbox: %s\n", esc(err.Error()))
	}
	if line := replayLine(rep); line != "" {
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: %s\n", line)
	}
	if r.conn.Err() == nil {
		return nil
	}
	_ = r.conn.Close()
	r.conn = nil
	c, err := mcpserver.DialRepo(ctx, r.dir, r.clientOptions())
	if err != nil {
		r.missed(err)
		return err
	}
	r.conn = c
	return nil
}

// outboxNotices are the lines prime adds for the outbox of c's
// principal: a key the server has given to someone else, and what waits
// or was refused. A person hears of a refused write through them, at a
// session's start, when no person ran the command that queued it.
func (r *runner) outboxNotices(c *mcpserver.RepoConn) []string {
	var out []string
	if r.moved != "" {
		out = append(out, r.moved)
	}
	if n := r.away().Notice(c); n != "" {
		out = append(out, n)
	}
	if n := r.away().Rekeyed(); n != "" {
		out = append(out, n)
	}
	return out
}

// replayLine says what a replay did, or "" when it changed nothing.
func replayLine(rep outbox.Report) string {
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{rep.Delivered, "delivered"}, {rep.Refused, "refused"}, {rep.Stale, "stale"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", p.what, p.n))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	line := "outbox: " + strings.Join(parts, ", ")
	var notes []string
	if rep.Parked > 0 {
		notes = append(notes, fmt.Sprintf("%d parked conflicts for a person to resolve: `sfx conflicts`", rep.Parked))
	}
	if rep.Kept > 0 {
		notes = append(notes, fmt.Sprintf("%d kept as a comment, as the issue moved on while it waited: `sfx inbox` says why", rep.Kept))
	}
	if len(notes) > 0 {
		line = strings.Replace(line, fmt.Sprintf("delivered %d", rep.Delivered),
			fmt.Sprintf("delivered %d (%s)", rep.Delivered, strings.Join(notes, "; ")), 1)
	}
	if rep.Pending > 0 {
		line += fmt.Sprintf(", %d still waiting", rep.Pending)
	}
	if rep.Refused+rep.Stale+rep.Pending > 0 {
		line += " (see `sfx outbox`)"
	}
	return line
}

// unqueued adds, to the fix of a write that did not reach the server and
// that the outbox never queues, that it was not queued and why. Any
// other error is returned as it is, as is every error of a hook.
func (r *runner) unqueued(op string, err error) error {
	why, ok := outbox.Unqueued(op)
	if !ok || r.hook || !client.Unreachable(err) {
		return err
	}
	pe, _ := errors.AsType[*proto.Error](err) // Unreachable says it is one
	e := *pe
	if e.Fix != "" {
		e.Fix += "; "
	}
	e.Fix += "this was not queued for later, since " + why + ": run it again when the server answers"
	return &e
}

// send runs one write. When it does not reach the server and is one the
// outbox queues, it is kept there instead, and send returns it: the
// caller prints that it was queued (queued), never a result. With
// --no-queue or STARFIX_NO_QUEUE the write fails as it did before there
// was an outbox. A write that could be neither sent nor queued fails
// with the outbox's refusal.
func (r *runner) send(ctx context.Context, op string, args, result any) (*offline.Kept, error) {
	err := r.call(ctx, op, args, result)
	if err == nil || !client.Unreachable(err) || !outbox.Queueable(op) || r.noQueue || offline.NoQueue(r.env.Getenv) {
		return nil, err
	}
	// A write lost with its connection is that connection's principal's;
	// with no connection, the keys this process would offer say whose.
	who := ""
	if r.conn != nil {
		who = r.conn.Principal()
	}
	k, err := r.away().Keep(who, op, args, err)
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// queuedDoc is what --json prints for a write that was queued: never an
// id and a rev, as a write that was made has (design §18.2).
type queuedDoc struct {
	Queued bool   `json:"queued"`
	Entry  uint64 `json:"entry"`
	Op     string `json:"op"`
	// Issue is the issue the write is about, or Key the memory's key.
	Issue string    `json:"issue,omitempty"`
	Key   string    `json:"key,omitempty"`
	Since time.Time `json:"since"`
	Note  string    `json:"note,omitempty"`
}

// queued prints that writes were queued: a line for each, or one
// document, which for several writes of one command (a label for each
// of several labels) lists them under entries.
func (r *runner) queued(ks ...*offline.Kept) {
	if r.json {
		docs := make([]queuedDoc, len(ks))
		for i, k := range ks {
			e := k.Entry
			docs[i] = queuedDoc{Queued: true, Entry: e.Seq, Op: e.Op, Since: k.Since.UTC()}
			switch target := offline.Target(e); e.Op {
			case proto.OpRemember:
				docs[i].Key = target
			case proto.OpCreate:
				docs[i].Note = "the issue has no id until it is sent, so nothing queued can refer to it"
			default:
				docs[i].Issue = target
			}
		}
		if len(docs) == 1 {
			r.emit(docs[0])
			return
		}
		r.emit(struct {
			Queued  bool        `json:"queued"`
			Entries []queuedDoc `json:"entries"`
		}{true, docs})
		return
	}
	for _, k := range ks {
		e := k.Entry
		line := fmt.Sprintf("queued (server unreachable since %s): entry %d, %s", when(k.Since), e.Seq, esc(strings.TrimSpace(e.Op+" "+offline.Target(e))))
		line += "; sfx sends it when the server answers (`sfx outbox` lists what waits)"
		if e.Op == proto.OpCreate {
			line += "; the issue has no id until then, so nothing queued can refer to it"
		}
		_, _ = fmt.Fprintln(r.env.Stdout, line)
	}
}

const outboxUsage = "outbox [retry N | drop N | stats] [--principal P] [--host-key FINGERPRINT]"

// cmdOutbox shows and changes what this machine keeps for the server. It
// never connects, so all of it works while the server is away.
func cmdOutbox(_ context.Context, r *runner, args []string) error {
	fs := r.newFlags("outbox")
	as := fs.String("principal", "", "the outbox of this principal, when this machine has queued as several")
	hostKey := fs.String("host-key", "", "the outbox kept for this project under a host key the server had before")
	pos, err := parse(fs, args, outboxUsage)
	if err != nil {
		return err
	}
	action, seq := "", uint64(0)
	if len(pos) > 0 {
		action = pos[0]
	}
	switch {
	case action == "" || action == "stats":
		if len(pos) > 1 {
			return usagef(outboxUsage, "outbox %s takes no arguments", action)
		}
	case action != "retry" && action != "drop":
		return usagef(outboxUsage, "unknown outbox action %q", action)
	case len(pos) != 2:
		return usagef(outboxUsage, "%s needs an entry's number", action)
	default:
		if seq, err = strconv.ParseUint(pos[1], 10, 64); err != nil || seq == 0 {
			return usagef(outboxUsage, "%q is not an entry's number", pos[1])
		}
	}
	off := r.away()
	l, cfg, err := off.Local()
	if err != nil {
		return err
	}
	who := cmp.Or(*as, off.Whose(l, cfg))
	if *hostKey != "" {
		if slices.Contains(cfg.Pinned(), *hostKey) {
			return usagef(outboxUsage, "%s is pinned in %s now: leave out --host-key", *hostKey, client.ConfigFile)
		}
		f, err := former(l, *hostKey)
		if err != nil {
			return err
		}
		switch action {
		case "":
			return r.formerList(cfg, f, who)
		case "stats":
			return r.outboxStats(f.Local, who)
		case "retry":
			return r.adopt(l, cfg, f, who, seq)
		}
		return r.outboxChange(f.Local, who, action, seq)
	}
	switch action {
	case "":
		return r.outboxList(l, cfg, who)
	case "stats":
		return r.outboxStats(l, who)
	}
	return r.outboxChange(l, who, action, seq)
}

// former finds what this machine keeps for the project under hostKey,
// a host key the server no longer has.
func former(l *offline.Local, hostKey string) (offline.Former, error) {
	formers, err := l.Rekeyed()
	if err != nil {
		return offline.Former{}, outboxFailed(l, err)
	}
	for _, f := range formers {
		if f.HostKey == hostKey {
			return f, nil
		}
	}
	return offline.Former{}, proto.Errf(proto.CodeNotFound, "`sfx outbox` names the old host keys this machine keeps writes under",
		fmt.Sprintf("this machine keeps nothing for this project under the host key %s", hostKey))
}

// rekeyedOutbox is an outbox of `sfx outbox --json` kept for the project
// under a host key the server no longer has.
type rekeyedOutbox struct {
	HostKey string `json:"host_key"`
	// Address is where the server was, or "" when sfx did not note it;
	// SameAddress says it is where the checkout names the server now,
	// which `outbox retry N --host-key` needs.
	Address     string `json:"address,omitempty"`
	SameAddress bool   `json:"same_address"`
	otherOutbox
}

// rekeyed lists the outboxes kept for the project under host keys the
// server no longer has, with what waits in them.
func rekeyed(l *offline.Local, cfg *client.Config) []rekeyedOutbox {
	formers, err := l.Rekeyed()
	if err != nil {
		return nil
	}
	var out []rekeyedOutbox
	for _, f := range formers {
		for _, p := range f.Local.Principals() {
			if n, err := f.Local.Waiting(p); err == nil && n.Pending+n.Refused > 0 {
				out = append(out, rekeyedOutbox{HostKey: f.HostKey, Address: f.Address, SameAddress: f.Address != "" && f.Address == offline.Address(cfg),
					otherOutbox: otherOutbox{Principal: p, Pending: n.Pending, Refused: n.Refused}})
			}
		}
	}
	return out
}

// at says where a server rekeyed was, for a person.
func (o rekeyedOutbox) at() string {
	switch {
	case o.Address == "":
		return "at an address sfx did not note"
	case o.SameAddress:
		return "at " + o.Address
	}
	return "at " + o.Address + ", not where this checkout names the server"
}

// adopt moves entry seq of who's outbox under f's host key to who's
// outbox here, for the next command to send: the person's decision that
// the rebuilt server is the same one.
func (r *runner) adopt(l *offline.Local, cfg *client.Config, f offline.Former, who string, seq uint64) error {
	e, err := l.Adopt(f.Local, who, seq)
	switch {
	case errors.Is(err, offline.ErrOtherAddress):
		return proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("if it is the same server, set server.host and server.port back to what they were and retry; otherwise `sfx outbox drop %d --host-key %s` removes it", seq, f.HostKey),
			fmt.Sprintf("entry %d was queued for the server at %s, and this checkout names %s, so it is not known to be the same server",
				seq, cmp.Or(f.Address, "an address sfx did not note"), offline.Address(cfg)))
	case errors.Is(err, offline.ErrNoEntry):
		return proto.Errf(proto.CodeNotFound, "`sfx outbox --host-key "+f.HostKey+"` lists the entries and their numbers",
			fmt.Sprintf("no entry %d in %s's outbox under the host key %s", seq, cmp.Or(who, "this machine"), f.HostKey))
	case errors.Is(err, outbox.ErrBusy):
		return proto.Errf(proto.CodeBusy, "run it again in a few seconds", "another sfx is sending an outbox now, so it cannot be changed")
	case err != nil:
		return outboxFailed(l, err)
	}
	if r.json {
		r.emit(struct {
			Entry uint64 `json:"entry"`
			Now   uint64 `json:"now"`
			State string `json:"state"`
		}{seq, e.Seq, "waiting"})
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "entry %d (%s) of the outbox under the old host key is entry %d of this server's now: the next sfx command that reaches the server sends it\n",
		seq, esc(strings.TrimSpace(e.Op+" "+offline.Target(e))), e.Seq)
	return nil
}

// outboxEntry is one entry of `sfx outbox --json`.
type outboxEntry struct {
	Entry uint64 `json:"entry"`
	Op    string `json:"op"`
	// Issue is the issue the entry is about, or Key the memory's key.
	Issue    string    `json:"issue,omitempty"`
	Key      string    `json:"key,omitempty"`
	QueuedAt time.Time `json:"queued_at"`
	Session  string    `json:"session,omitempty"`
	Machine  string    `json:"machine,omitempty"`
	// Code, Message, Fix and RefusedAt are a refused entry's.
	Code      string    `json:"code,omitempty"`
	Message   string    `json:"message,omitempty"`
	Fix       string    `json:"fix,omitempty"`
	RefusedAt time.Time `json:"refused_at,omitzero"`
}

func newOutboxEntry(e outbox.Entry) outboxEntry {
	out := outboxEntry{Entry: e.Seq, Op: e.Op, QueuedAt: e.At.UTC(), Session: e.Session, Machine: e.Machine}
	if e.Op == proto.OpRemember {
		out.Key = offline.Target(e)
	} else {
		out.Issue = offline.Target(e)
	}
	return out
}

// otherOutbox is another principal's outbox on this machine, for the
// same server and project, with entries in it.
type otherOutbox struct {
	Principal string `json:"principal"`
	Pending   int    `json:"pending"`
	Refused   int    `json:"refused"`
}

// outboxDoc is `sfx outbox --json`.
type outboxDoc struct {
	Principal string `json:"principal"`
	// HostKey and Address are those of `--host-key`: the old host key
	// the outbox is kept under, and where the server was then.
	HostKey string        `json:"host_key,omitempty"`
	Address string        `json:"address,omitempty"`
	Pending []outboxEntry `json:"pending"`
	Refused []outboxEntry `json:"refused"`
	// UnreachableSince is when the outage in progress began, and
	// LastNotReached when the server was last not reached, to the minute.
	UnreachableSince time.Time     `json:"unreachable_since,omitzero"`
	LastNotReached   time.Time     `json:"last_not_reached,omitzero"`
	Others           []otherOutbox `json:"others,omitempty"`
	// Rekeyed are the outboxes kept for the project under host keys the
	// server no longer has, which nothing sends by itself.
	Rekeyed []rekeyedOutbox `json:"rekeyed,omitempty"`
}

// entries reads who's outbox in l into doc, and returns when the outage
// in progress began, as that outbox saw it.
func entries(l *offline.Local, who string, doc *outboxDoc) (since time.Time, err error) {
	doc.Pending, doc.Refused = []outboxEntry{}, []outboxEntry{}
	ob, ok, err := l.Existing(who)
	if err != nil || !ok {
		return time.Time{}, err
	}
	list, err := ob.List()
	if err != nil {
		return time.Time{}, outboxFailed(l, err)
	}
	for _, e := range list.Pending {
		doc.Pending = append(doc.Pending, newOutboxEntry(e))
	}
	for _, f := range list.Refused {
		e := newOutboxEntry(f.Entry)
		e.Code, e.Message, e.Fix, e.RefusedAt = f.Code, f.Message, f.Fix, f.At.UTC()
		doc.Refused = append(doc.Refused, e)
	}
	if st, err := ob.Stats(); err == nil {
		since = st.Outages.Since
	}
	return since, nil
}

// outboxList prints who's outbox: what waits, what was refused and why,
// and when the server was last not reached; and what waits in the
// outboxes of others, and under the server's old host keys.
func (r *runner) outboxList(l *offline.Local, cfg *client.Config, who string) error {
	doc := outboxDoc{Principal: who}
	if t := l.LastMissed(); !t.IsZero() {
		doc.LastNotReached = t.UTC()
	}
	since, err := entries(l, who, &doc)
	if err != nil {
		return err
	}
	if !since.IsZero() {
		doc.UnreachableSince = since.UTC()
	}
	for _, p := range l.Principals() {
		if p == who {
			continue
		}
		if n, err := l.Waiting(p); err == nil && n.Pending+n.Refused > 0 {
			doc.Others = append(doc.Others, otherOutbox{Principal: p, Pending: n.Pending, Refused: n.Refused})
		}
	}
	doc.Rekeyed = rekeyed(l, cfg)
	if r.json {
		r.emit(doc)
		return nil
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	if who == "" && len(doc.Others)+len(doc.Rekeyed) == 0 {
		p("nothing is queued: this machine has not reached the server with this key, so it queues nothing\n")
		return nil
	}
	switch {
	case who == "":
		p("this machine has not reached the server with this key, so it queues nothing with it\n")
	case len(doc.Pending)+len(doc.Refused) == 0:
		p("%s's outbox is empty\n", esc(who))
	default:
		p("%s's outbox: %d waiting, %d refused\n", esc(who), len(doc.Pending), len(doc.Refused))
	}
	switch {
	case !doc.UnreachableSince.IsZero() && doc.LastNotReached.After(doc.UnreachableSince):
		p("server unreachable since %s; last tried %s\n", when(doc.UnreachableSince), when(doc.LastNotReached))
	case !doc.UnreachableSince.IsZero():
		p("server unreachable since %s\n", when(doc.UnreachableSince))
	case !doc.LastNotReached.IsZero():
		p("server last not reached %s\n", when(doc.LastNotReached))
	default:
		p("no failure to reach the server is recorded\n")
	}
	r.printEntries(doc)
	if len(doc.Pending)+len(doc.Refused) > 0 {
		p("`sfx outbox retry N` sends a refused entry again; `sfx outbox drop N` removes an entry for good\n")
	}
	for _, o := range doc.Others {
		p("%s's outbox on this machine: %d waiting, %d refused; sfx sends it on a connection as %s (`sfx outbox --principal %s` lists it)\n",
			esc(o.Principal), o.Pending, o.Refused, esc(o.Principal), esc(o.Principal))
	}
	for _, o := range doc.Rekeyed {
		// The host key and the address are this machine's own record, and
		// a principal's name the server's: escaped all the same.
		lists := "`sfx outbox --host-key " + esc(o.HostKey)
		if o.Principal != who {
			lists += " --principal " + esc(o.Principal)
		}
		p("under the server's old host key %s, %s: %s's outbox, %d waiting, %d refused\n",
			esc(o.HostKey), esc(o.at()), esc(o.Principal), o.Pending, o.Refused)
		p("  not sent by themselves, since the server's host key changed: %s` lists them\n", lists)
	}
	return nil
}

// formerList prints who's outbox under f's host key, which the server no
// longer has, and what a person may do with it.
func (r *runner) formerList(cfg *client.Config, f offline.Former, who string) error {
	doc := outboxDoc{Principal: who, HostKey: f.HostKey, Address: f.Address}
	if _, err := entries(f.Local, who, &doc); err != nil {
		return err
	}
	if r.json {
		r.emit(doc)
		return nil
	}
	o := rekeyedOutbox{HostKey: f.HostKey, Address: f.Address, SameAddress: f.Address != "" && f.Address == offline.Address(cfg)}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	p("%s's outbox under the server's old host key %s, %s: %d waiting, %d refused\n",
		esc(cmp.Or(who, "nobody")), esc(f.HostKey), esc(o.at()), len(doc.Pending), len(doc.Refused))
	r.printEntries(doc)
	if len(doc.Pending)+len(doc.Refused) == 0 {
		return nil
	}
	p("nothing here is sent by itself: the server's host key changed, which only a person can confirm is the same server\n")
	key := esc(f.HostKey)
	if o.SameAddress {
		p("`sfx outbox retry N --host-key %s` moves entry N to this server's outbox, which sends it; `sfx outbox drop N --host-key %s` removes it for good\n", key, key)
	} else {
		p("this checkout names the server at %s, so its entries are not moved to it; `sfx outbox drop N --host-key %s` removes one for good\n",
			esc(offline.Address(cfg)), key)
	}
	return nil
}

// printEntries prints doc's entries, waiting and refused, in one set of
// columns, with each refusal's code, message and fix.
func (r *runner) printEntries(doc outboxDoc) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	// The cells are other people's text: an id an entry names, a
	// session, a machine.
	now := r.env.Now()
	row := func(e outboxEntry) [5]string {
		return [5]string{strconv.FormatUint(e.Entry, 10), esc(e.Op), esc(cmp.Or(e.Issue, e.Key, "-")),
			proto.Span(max(now.Sub(e.QueuedAt), 0)), esc(e.Session + " on " + e.Machine)}
	}
	rows := [][5]string{{"N", "OP", "ON", "AGE", "FROM"}}
	for _, e := range slices.Concat(doc.Pending, doc.Refused) {
		rows = append(rows, row(e))
	}
	var width [4]int
	for _, cells := range rows {
		for i := range width {
			width[i] = max(width[i], len(cells[i]))
		}
	}
	line := func(c [5]string) {
		p("  %-*s  %-*s  %-*s  %-*s  %s\n", width[0], c[0], width[1], c[1], width[2], c[2], width[3], c[3], c[4])
	}
	if len(doc.Pending) > 0 {
		p("waiting:\n")
		line(rows[0])
		for _, e := range doc.Pending {
			line(row(e))
		}
	}
	if len(doc.Refused) > 0 {
		p("refused:\n")
		if len(doc.Pending) == 0 {
			line(rows[0])
		}
		for _, e := range doc.Refused {
			line(row(e))
			p("     %s: %s\n", esc(e.Code), esc(e.Message))
			if e.Fix != "" {
				p("     fix: %s\n", esc(e.Fix))
			}
		}
	}
}

// outboxFailed is a failure of the outbox's own files, with a fix.
func outboxFailed(l *offline.Local, err error) error {
	if _, ok := errors.AsType[*proto.Error](err); ok {
		return err
	}
	return proto.Errf(proto.CodeUnavailable,
		fmt.Sprintf("check that %s can be read and written (%s names another directory)", l.Dir(), offline.EnvDataDir), err.Error())
}

// outboxChange retries or drops entry seq of who's outbox.
func (r *runner) outboxChange(l *offline.Local, who, action string, seq uint64) error {
	of := "this machine's outbox"
	if who != "" {
		of = who + "'s outbox"
	}
	missing := proto.Errf(proto.CodeNotFound, "`sfx outbox` lists the entries and their numbers", fmt.Sprintf("no entry %d in %s", seq, of))
	if action == "retry" {
		missing = proto.Errf(proto.CodeNotFound,
			"`sfx outbox` lists the entries; only a refused one is retried, and one that waits is sent by itself",
			fmt.Sprintf("no refused entry %d in %s", seq, of))
	}
	ob, ok, err := l.Existing(who)
	if err != nil {
		return err
	}
	if !ok {
		return missing
	}
	list, err := ob.List()
	if err != nil {
		return outboxFailed(l, err)
	}
	what := ""
	for _, e := range list.Pending {
		if e.Seq == seq {
			what = strings.TrimSpace(e.Op + " " + offline.Target(e))
		}
	}
	for _, f := range list.Refused {
		if f.Entry.Seq == seq {
			what = strings.TrimSpace(f.Entry.Op + " " + offline.Target(f.Entry))
		}
	}
	state := "dropped"
	if action == "retry" {
		state, err = "waiting", ob.Retry(seq)
	} else {
		err = ob.Drop(seq)
	}
	switch {
	case errors.Is(err, outbox.ErrNotFound):
		return missing
	case errors.Is(err, outbox.ErrBusy):
		return proto.Errf(proto.CodeBusy, "run it again in a few seconds", "another sfx is sending this outbox now, so it cannot be changed")
	case err != nil:
		return outboxFailed(l, err)
	}
	if r.json {
		r.emit(struct {
			Entry uint64 `json:"entry"`
			State string `json:"state"`
		}{seq, state})
		return nil
	}
	if action == "retry" {
		_, _ = fmt.Fprintf(r.env.Stdout, "entry %d (%s) waits again: the next sfx command that reaches the server sends it\n", seq, esc(what))
	} else {
		_, _ = fmt.Fprintf(r.env.Stdout, "dropped entry %d (%s): it will not be sent\n", seq, esc(what))
	}
	return nil
}

// outboxStats prints the counts of who's outbox: for each op the entries
// queued and what became of them, how long the delivered ones waited,
// and the outages. With --json the durations are nanoseconds, as
// outbox.Stats has them.
func (r *runner) outboxStats(l *offline.Local, who string) error {
	var st outbox.Stats
	if ob, ok, err := l.Existing(who); err != nil {
		return err
	} else if ok {
		if st, err = ob.Stats(); err != nil {
			return outboxFailed(l, err)
		}
	}
	if st.Ops == nil {
		st.Ops = map[string]outbox.OpStats{}
	}
	if r.json {
		r.emit(struct {
			Principal string `json:"principal"`
			outbox.Stats
		}{who, st})
		return nil
	}
	if who == "" {
		_, _ = fmt.Fprintln(r.env.Stdout, "nothing is queued: this machine has not reached the server with this key, so it queues nothing")
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s's outbox, since it was made:\n", esc(who))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "OP\tQUEUED\tDELIVERED\tPARKED\tREFUSED\tSTALE\tDROPPED\tMEAN WAIT\tLONGEST")
	for _, op := range slices.Sorted(maps.Keys(st.Ops)) {
		c := st.Ops[op]
		mean, longest := "-", "-"
		if c.Wait.Count > 0 {
			mean = proto.Span(c.Wait.Total / time.Duration(c.Wait.Count)) //nolint:gosec // a count of entries
			longest = proto.Span(c.Wait.Max)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%s\t%s\n", esc(op), c.Queued, c.Delivered, c.Parked, c.Refused, c.Stale, c.Dropped, mean, longest)
	}
	_ = tw.Flush()
	out := st.Outages
	fmt.Fprintf(&b, "outages: %d over, %s in all, the longest %s", out.Count, proto.Span(out.Total), proto.Span(out.Longest))
	if out.Since.IsZero() {
		b.WriteString("; none in progress\n")
	} else {
		fmt.Fprintf(&b, "; one in progress since %s\n", when(out.Since))
	}
	_, _ = fmt.Fprint(r.env.Stdout, b.String())
	return nil
}
