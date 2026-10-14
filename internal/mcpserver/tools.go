package mcpserver

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/gitx"
	"github.com/ariesworx/starfix/internal/proto"
)

// Values the server accepts. The input schemas carry them as enums, so a
// client sees the choices and the SDK refuses anything else before a
// request is sent.
var (
	issueTypes   = []any{"task", "bug", "feature", "epic", "chore"}
	openStatuses = []any{"open", "in_progress", "blocked", "deferred"}
	// setStatuses are what update may set: start alone sets in_progress.
	setStatuses = []any{"open", "blocked", "deferred"}
	allStatuses = append(append([]any{}, openStatuses...), "closed")
	depTypes    = []any{"blocks", "conditional-blocks", "waits-for", "related", "discovered-from", "duplicates", "supersedes"}
	actions     = []any{"add", "rm"}
	states      = []any{"done", "partial", "blocked"}
)

// Inputs. Field tags are the schema's descriptions; keep them terse, since
// every agent pays for them in every session.

// IDIn names one issue.
type IDIn struct {
	ID string `json:"id"`
}

// ShowIn reads one issue.
type ShowIn struct {
	ID   string `json:"id"`
	Full bool   `json:"full,omitempty"`
}

// LimitIn bounds a list.
type LimitIn struct {
	Limit int `json:"limit,omitempty"`
}

// ListIn filters issues.
type ListIn struct {
	Status   []string `json:"status,omitempty" jsonschema:"default: not closed"`
	Type     []string `json:"type,omitempty"`
	Priority []int    `json:"priority,omitempty" jsonschema:"0 highest"`
	Assignee string   `json:"assignee,omitempty"`
	Parent   string   `json:"parent,omitempty"`
	Labels   []string `json:"labels,omitempty" jsonschema:"must have all"`
	Limit    int      `json:"limit,omitempty"`
	Cursor   string   `json:"cursor,omitempty" jsonschema:"a page's next"`
}

// CreateIn creates an issue.
type CreateIn struct {
	Title      string   `json:"title"`
	Body       string   `json:"body,omitempty"`
	Type       string   `json:"type,omitempty"`
	Priority   *int     `json:"priority,omitempty" jsonschema:"0 highest"`
	Parent     string   `json:"parent,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Assignee   string   `json:"assignee,omitempty"`
	Design     string   `json:"design,omitempty"`
	Acceptance string   `json:"acceptance,omitempty"`

	idem string // the same on every attempt of one call
}

// prepare gives the call its idempotency key before the first attempt, so
// a retry on a new connection returns the first attempt's result instead
// of writing twice. The comment, handoff and finish inputs do the same.
func (in *CreateIn) prepare() error {
	in.idem = proto.NewIdem("mcp")
	return nil
}

func (in *CommentIn) prepare() error {
	in.idem = proto.NewIdem("mcp")
	return nil
}

func (in *HandoffIn) prepare() error {
	in.idem = proto.NewIdem("mcp")
	return nil
}

// The update, dep and label inputs take a key too (protocol 6), so that a
// call that does not reach the server can be queued (design §18.5). A
// label call's labels each go with the key and their place in the call.
func (in *UpdateIn) prepare() error {
	in.idem = proto.NewIdem("mcp")
	return nil
}

func (in *DepIn) prepare() error {
	in.idem = proto.NewIdem("mcp")
	return nil
}

func (in *LabelIn) prepare() error {
	in.idem = proto.NewIdem("mcp")
	return nil
}

// prepare also checks waived's keys, which the schema leaves as strings.
func (in *FinishIn) prepare() error {
	for k := range in.Waived {
		if n, err := strconv.Atoi(k); err != nil || n < 1 || strconv.Itoa(n) != k {
			return proto.Errf(proto.CodeInvalid, "", fmt.Sprintf("waived key %q is not an acceptance item number", k))
		}
	}
	in.idem = proto.NewIdem("mcp")
	return nil
}

// UpdateIn changes the fields given.
type UpdateIn struct {
	ID         string  `json:"id"`
	Rev        int64   `json:"rev,omitempty"`
	Title      *string `json:"title,omitempty"`
	Body       *string `json:"body,omitempty"`
	Design     *string `json:"design,omitempty"`
	Acceptance *string `json:"acceptance,omitempty"`
	Notes      *string `json:"notes,omitempty"`
	Status     *string `json:"status,omitempty"`
	Priority   *int    `json:"priority,omitempty"`
	Type       *string `json:"type,omitempty"`
	Assignee   *string `json:"assignee,omitempty"`
	Owner      *string `json:"owner,omitempty"`
	Parent     *string `json:"parent,omitempty"`

	idem string
}

// CloseIn closes an issue.
type CloseIn struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
	Rev    int64  `json:"rev,omitempty"`
}

// ReopenIn reopens an issue.
type ReopenIn struct {
	ID  string `json:"id"`
	Rev int64  `json:"rev,omitempty"`
}

// DepIn adds or removes an edge.
type DepIn struct {
	Action    string `json:"action"`
	ID        string `json:"id"`
	DependsOn string `json:"depends_on"`
	Type      string `json:"type,omitempty"`

	idem string
}

// LabelIn adds or removes labels.
type LabelIn struct {
	Action string   `json:"action"`
	ID     string   `json:"id"`
	Labels []string `json:"labels"`

	idem string
}

// CommentIn adds a comment; with Conflict, it proposes how to resolve
// that conflict instead (design §18.7): Propose is mine, theirs, or the
// new text, and Body says why. Proposing rides comment, not a tool of
// its own, to keep the schema set within its budget (rule 12); a person
// resolves.
type CommentIn struct {
	ID       string `json:"id"`
	Body     string `json:"body"`
	Conflict int64  `json:"conflict,omitempty"`
	Propose  string `json:"propose,omitempty" jsonschema:"mine, theirs or new text"`

	idem string
}

// proposal is the conflict.propose request of a comment with a conflict:
// propose mine or theirs is that choice, and anything else new text.
func (in CommentIn) proposal() (proto.ProposeArgs, error) {
	out := proto.ProposeArgs{ID: in.Conflict, Choice: proto.ChoiceText, Text: in.Propose, Why: in.Body}
	switch in.Propose {
	case "":
		return out, proto.Errf(proto.CodeInvalid, "give propose: mine, theirs or the new text", "a comment with a conflict proposes, and propose is empty")
	case proto.ChoiceMine, proto.ChoiceTheirs:
		out.Choice, out.Text = in.Propose, ""
	}
	return out, nil
}

// PageIn names an issue and bounds a list of its records.
type PageIn struct {
	ID    string `json:"id"`
	Limit int    `json:"limit,omitempty"`
}

// StartIn takes an issue.
type StartIn struct {
	ID   string `json:"id,omitempty"`
	Take bool   `json:"take,omitempty"`
}

// FinishIn closes an issue with what the next person needs.
type FinishIn struct {
	ID         string            `json:"id"`
	Reason     string            `json:"reason,omitempty"`
	Handoff    string            `json:"handoff,omitempty"`
	Discovered []DiscoveredIn    `json:"discovered,omitempty"`
	Ticked     []int             `json:"ticked,omitempty"`
	Waived     map[string]string `json:"waived,omitempty" jsonschema:"item: reason"`
	HandoffFieldsIn

	idem string
}

// HandoffFieldsIn are a handoff's structured fields. The worktree field
// is left to the CLI: a path on this machine means little to the next
// agent, and the schema budget is better spent elsewhere.
type HandoffFieldsIn struct {
	State  string `json:"state,omitempty"`
	Next   string `json:"next,omitempty"`
	Branch string `json:"branch,omitempty"`
	To     string `json:"to,omitempty" jsonschema:"principal"`
}

// wire converts f to the protocol's fields.
func (f HandoffFieldsIn) wire() proto.HandoffFields {
	return proto.HandoffFields{State: f.State, Next: f.Next, Branch: f.Branch, To: f.To}
}

// DiscoveredIn is one piece of discovered work.
type DiscoveredIn struct {
	Title    string `json:"title"`
	Type     string `json:"type,omitempty"`
	Priority *int   `json:"priority,omitempty"`
}

// HandoffIn records a handoff note.
type HandoffIn struct {
	ID      string `json:"id"`
	Note    string `json:"note"`
	Release bool   `json:"release,omitempty" jsonschema:"let others start it"`
	HandoffFieldsIn

	idem string
}

// DigestIn selects a digest.
type DigestIn struct {
	Since string `json:"since,omitempty" jsonschema:"24h, 7d or a time"`
	By    string `json:"by,omitempty" jsonschema:"principal"`
	Label string `json:"label,omitempty"`
}

// WhoIn widens who's window.
type WhoIn struct {
	Since string `json:"since,omitempty"`
}

// Outputs.

// Started is the issue start took, with what working on it needs.
type Started struct {
	untrusted
	ID         string `json:"id"`
	Rev        int64  `json:"rev"`
	Title      string `json:"title"`
	Type       string `json:"type"`
	Priority   int    `json:"priority"`
	Body       string `json:"body,omitempty"`
	Acceptance string `json:"acceptance,omitempty"`
	// Items are the acceptance criteria, "N [x] text"; finish ticks
	// them. They replace Acceptance when the server lists them.
	Items   []string `json:"items,omitempty"`
	Handoff *Handoff `json:"handoff,omitempty"`
	// Memories are those relevant to the issue: linked to it, or tagged
	// with one of its labels.
	Memories []Memory `json:"memories,omitempty"`
	// Branch is the suggested git branch; start does not create it.
	Branch string `json:"branch"`
	// Truncated: long text was cut; show with full: true has it all.
	Truncated bool `json:"truncated,omitempty"`
}

// Handoff is the latest handoff note on an issue and its fields.
type Handoff struct {
	By       string `json:"by"`
	At       string `json:"at"`
	Note     string `json:"note"`
	State    string `json:"state,omitempty"`
	Next     string `json:"next,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	To       string `json:"to,omitempty"`
}

// Ref is the result of a write that has no revision.
type Ref struct {
	ID string `json:"id"`
}

// Issues is a list result. Next continues a list; More says ready or
// blocked had more than fit.
type Issues struct {
	untrusted
	Issues []proto.Summary `json:"issues"`
	Next   string          `json:"next,omitempty"`
	More   bool            `json:"more,omitempty"`
}

// Blocked is the blocked result.
type Blocked struct {
	untrusted
	Issues []proto.BlockedIssue `json:"issues"`
	More   bool                 `json:"more,omitempty"`
}

// Issue is the compact form of show: no timestamps or authorship, and
// edges as "ID type".
type Issue struct {
	untrusted
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Type        string   `json:"type"`
	Rev         int64    `json:"rev"`
	Parent      string   `json:"parent,omitempty"`
	Assignee    string   `json:"assignee,omitempty"`
	Owner       string   `json:"owner,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Body        string   `json:"body,omitempty"`
	Design      string   `json:"design,omitempty"`
	Acceptance  string   `json:"acceptance,omitempty"`
	Notes       string   `json:"notes,omitempty"`
	CloseReason string   `json:"close_reason,omitempty"`
	Items       []string `json:"items,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
	NeededBy    []string `json:"needed_by,omitempty"`
	// DepsMore counts the edges the server left out.
	DepsMore int `json:"deps_more,omitempty"`
	// Similar names similar closed issues: "ID title; ID title".
	Similar string `json:"similar,omitempty"`
	// Account is the account the issue's time and tokens report against.
	Account string `json:"account,omitempty"`
	// Usage is the time held and tokens by model, in one line.
	Usage string `json:"usage,omitempty"`
	// Files are the likely files: declared paths (a trailing / is a
	// directory), then those its commits touched, most recent first.
	// FilesMore counts the rest.
	Files     []string `json:"files,omitempty"`
	FilesMore int      `json:"files_more,omitempty"`
	// Overlaps are the issues others hold now whose files overlap these:
	// "ID by principal/session".
	Overlaps []string `json:"overlaps,omitempty"`
	// Conflicts are the issue's open conflicts: fields of replayed
	// updates parked against others' changes, for a person to resolve
	// and an agent to propose for (design §18.7).
	Conflicts []Conflict `json:"conflicts,omitempty"`
	// Truncated: long text was cut; full: true returns more.
	Truncated bool `json:"truncated,omitempty"`
}

// Conflict is one open conflict, compact: base is the field at the rev
// mine was written against, mine the replayed update's value by By, and
// theirs the server's by TheirsBy. Proposed is the proposal waiting for a
// person, if any.
type Conflict struct {
	ID       int64  `json:"id"`
	Field    string `json:"field"`
	Base     string `json:"base"`
	Mine     string `json:"mine"`
	Theirs   string `json:"theirs"`
	By       string `json:"by"`
	TheirsBy string `json:"theirs_by"`
	Advice   string `json:"advice,omitempty"`
	Proposed string `json:"proposed,omitempty"`
}

// maxConflictText is how much of each side of a conflict show gives,
// before it cuts more to fit.
const maxConflictText = 300

// Commented is comment's result: the comment's id, or for a proposal
// what happens next.
type Commented struct {
	ID   string `json:"id,omitempty"`
	Next string `json:"next,omitempty"`
}

// Created is create's result: the write's {id, rev} and similar closed
// issues in one line.
type Created struct {
	untrusted
	ID      string `json:"id"`
	Rev     int64  `json:"rev"`
	Similar string `json:"similar,omitempty"`
}

// maxItemText is how much of each acceptance item start and show give.
const maxItemText = 300

// itemLines renders acceptance items one line each: "1 [x] text",
// "2 [ ] text", "3 [waived: reason] text".
func itemLines(items []proto.AcceptanceItem) []string {
	var out []string
	for _, it := range items {
		mark := " "
		switch it.State {
		case "ticked":
			mark = "x"
		case "waived":
			mark = "waived: " + it.Reason
		}
		t, _ := cut(it.Text, maxItemText)
		out = append(out, fmt.Sprintf("%d [%s] %s", it.N, mark, t))
	}
	return out
}

// similarLine names similar issues in one line: "ID title; ID title".
func similarLine(sim []proto.Summary) string {
	parts := make([]string, len(sim))
	for i, x := range sim {
		t, _ := cut(x.Title, primeTitleLen)
		parts[i] = x.ID + " " + t
	}
	return strings.Join(parts, "; ")
}

// Comment is one comment, compact.
type Comment struct {
	Author string `json:"author"`
	At     string `json:"at"`
	Kind   string `json:"kind,omitempty"`
	Body   string `json:"body"`
}

// Comments lists an issue's newest comments, oldest first. Omitted counts
// the older ones left out.
type Comments struct {
	untrusted
	Comments []Comment `json:"comments"`
	Omitted  int       `json:"omitted,omitempty"`
}

// Event is one history entry, compact.
type Event struct {
	Seq     int64  `json:"seq"`
	At      string `json:"at"`
	By      string `json:"by"`
	Op      string `json:"op"`
	Changed string `json:"changed,omitempty"`
}

// History lists an issue's newest events, oldest first.
type History struct {
	untrusted
	Events  []Event `json:"events"`
	Omitted int     `json:"omitted,omitempty"`
}

// DigestItem is one issue in a digest section. For in progress and
// stalled, For is how long it has been held or idle, and At is left out.
type DigestItem struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Priority  int      `json:"priority"`
	By        string   `json:"by,omitempty"`
	At        string   `json:"at,omitempty"`
	For       string   `json:"for,omitempty"`
	Note      string   `json:"note,omitempty"`
	From      string   `json:"from,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

// Digest is the digest result, held under MaxDigestTokens. Totals count
// everything; the sections list the first few.
type Digest struct {
	untrusted
	Since      string             `json:"since"`
	Until      string             `json:"until"`
	Totals     proto.DigestTotals `json:"totals"`
	Closed     []DigestItem       `json:"closed,omitempty"`
	Started    []DigestItem       `json:"started,omitempty"`
	InProgress []DigestItem       `json:"in_progress,omitempty"`
	Stalled    []DigestItem       `json:"stalled,omitempty"`
	Blocked    []DigestItem       `json:"blocked,omitempty"`
	HandedOff  []DigestItem       `json:"handed_off,omitempty"`
	Created    []DigestItem       `json:"created,omitempty"`
	Discovered []DigestItem       `json:"discovered,omitempty"`
	// Usage is the window's time held and tokens by model, in one line.
	Usage string `json:"usage,omitempty"`
	// Truncated: items were left out to fit, or a total is a lower bound.
	Truncated bool `json:"truncated,omitempty"`
}

// Agent is one session in who. Seen is how long ago it was last seen.
type Agent struct {
	Principal string   `json:"principal"`
	Session   string   `json:"session"`
	Machine   string   `json:"machine"`
	Harness   string   `json:"harness,omitempty"`
	Seen      string   `json:"seen"`
	Claims    []string `json:"claims,omitempty"`
}

// Who lists the agents seen recently, most recently first. More says
// some were left out to fit.
type Who struct {
	untrusted
	Agents []Agent `json:"agents"`
	More   bool    `json:"more,omitempty"`
}

func (s *Server) register() {
	add(s, tool{name: "prime", desc: "Start here: your work, inbox, top ready issues.", ann: readOnly, retry: true,
		showsInbox: true, cached: true},
		func(ctx context.Context, c Conn, _ struct{}) (*Prime, error) {
			if fc, ok := c.(*fromCache); ok {
				p, at, ok := CachedPrime(fc.cache, fc.session, s.now(), s.notices(fc)...)
				if !ok {
					return nil, fc.err
				}
				fc.at, fc.told = at, true
				return p, nil
			}
			p, err := BuildPrime(ctx, c, s.opts.Version, s.notices(c)...)
			if err == nil && s.opts.Cache != nil {
				_ = KeepPrime(s.opts.Cache.CacheOf(c.Principal()), p) // a cache that cannot be written costs a later prime, not this one
			}
			return p, err
		})
	add(s, tool{name: "inbox", desc: "Lost claims, handoffs, mentions, assignments; ack marks read.", ann: idem, retry: true,
		showsInbox: true, cached: true},
		func(ctx context.Context, c Conn, in InboxIn) (Inbox, error) { return inbox(ctx, c, in) })
	add(s, tool{name: "start", desc: "Take an issue (default: top ready).", ann: write},
		func(ctx context.Context, c Conn, in StartIn) (Started, error) {
			out, epoch, err := start(ctx, c, in)
			if err == nil && epoch > 0 {
				s.claims.take(out.ID, epoch)
			}
			return out, err
		})
	add(s, tool{name: "finish", desc: "Close your issue: tick items, hand off, file new work.", ann: write, retry: true,
		enums: enums{"discovered.type": issueTypes, "state": states}},
		func(ctx context.Context, c Conn, in FinishIn) (proto.FinishResult, error) {
			args := proto.FinishArgs{ID: in.ID, Epoch: s.claims.epoch(in.ID), Reason: in.Reason, Handoff: in.Handoff,
				HandoffFields: in.wire(), Ticked: in.Ticked, Idem: in.idem, Paths: s.paths(ctx, in.ID)}
			for k, r := range in.Waived {
				n, _ := strconv.Atoi(k) // checked by prepare
				if args.Waived == nil {
					args.Waived = map[int]string{}
				}
				args.Waived[n] = r
			}
			for _, d := range in.Discovered {
				args.Discovered = append(args.Discovered, proto.Discovered{Title: d.Title, Type: d.Type, Priority: d.Priority})
			}
			var out proto.FinishResult
			paths := args.Paths
			err := proto.RetryWithoutPaths(paths != nil, func(withPaths bool) error {
				args.Paths = nil
				if withPaths {
					args.Paths = paths
				}
				return c.Call(ctx, proto.OpFinish, args, &out)
			})
			if err == nil {
				s.claims.drop(in.ID)
			}
			return out, err
		})
	add(s, tool{name: "handoff", desc: "Note for whoever continues.", ann: write, retry: true,
		enums: enums{"state": states}},
		func(ctx context.Context, c Conn, in HandoffIn) (proto.WriteResult, error) {
			var out proto.WriteResult
			args := proto.HandoffArgs{ID: in.ID, Note: in.Note, Release: in.Release, HandoffFields: in.wire(), Idem: in.idem,
				Paths: s.paths(ctx, in.ID)}
			if in.Release {
				args.Epoch = s.claims.epoch(in.ID)
			}
			paths := args.Paths
			err := proto.RetryWithoutPaths(paths != nil, func(withPaths bool) error {
				args.Paths = nil
				if withPaths {
					args.Paths = paths
				}
				return c.Call(ctx, proto.OpHandoff, args, &out)
			})
			if err == nil && in.Release {
				s.claims.drop(in.ID)
			}
			return out, err
		})
	add(s, tool{name: "digest", desc: "Recent work: closed, started, stalled, blocked.",
		ann: readOnly, retry: true, cached: true},
		func(ctx context.Context, c Conn, in DigestIn) (Digest, error) { return digest(ctx, c, in) })
	add(s, tool{name: "who", desc: "Agents at work and what they hold.", ann: readOnly, retry: true, refused: true},
		func(ctx context.Context, c Conn, in WhoIn) (Who, error) {
			var r proto.WhoResult
			if err := c.Call(ctx, proto.OpWho, proto.WhoArgs{Since: in.Since}, &r); err != nil {
				return Who{}, err
			}
			out := Who{Agents: []Agent{}, More: r.More > 0}
			for _, a := range r.Agents {
				out.Agents = append(out.Agents, Agent{Principal: a.Principal, Session: a.Session, Machine: a.Machine,
					Harness: a.Harness, Seen: proto.Span(max(r.Now.Sub(a.LastSeen), 0)), Claims: a.Claims})
			}
			out.fit()
			return out, nil
		})
	add(s, tool{name: "ready", desc: "Open issues nothing blocks, best first.", ann: readOnly, retry: true, refused: true},
		func(ctx context.Context, c Conn, in LimitIn) (Issues, error) {
			var r proto.ListResult
			err := c.Call(ctx, proto.OpReady, proto.LimitArgs{Limit: orDefault(in.Limit, DefaultLimit)}, &r)
			out := Issues{Issues: r.Issues}
			out.fit()
			return out, err
		})
	add(s, tool{name: "blocked", desc: "Issues held back, and by what.", ann: readOnly, retry: true, refused: true},
		func(ctx context.Context, c Conn, in LimitIn) (Blocked, error) {
			var r proto.BlockedResult
			err := c.Call(ctx, proto.OpBlocked, proto.LimitArgs{Limit: orDefault(in.Limit, DefaultLimit)}, &r)
			out := Blocked{Issues: r.Issues}
			out.fit()
			return out, err
		})
	add(s, tool{name: "list", desc: "Issues matching filters.", ann: readOnly, retry: true, refused: true,
		enums: enums{"status": allStatuses, "type": issueTypes}},
		func(ctx context.Context, c Conn, in ListIn) (Issues, error) { return list(ctx, c, in) })
	add(s, tool{name: "show", desc: "One issue with its dependencies.", ann: readOnly, retry: true, cached: true},
		func(ctx context.Context, c Conn, in ShowIn) (Issue, error) { return show(ctx, c, in) })
	add(s, tool{name: "create", desc: "Create an issue.", ann: write, retry: true, enums: enums{"type": issueTypes}},
		func(ctx context.Context, c Conn, in CreateIn) (Created, error) {
			args := proto.CreateArgs{Idem: in.idem, Title: in.Title, Body: in.Body, Type: in.Type, Priority: in.Priority,
				Parent: in.Parent, Labels: in.Labels, Assignee: in.Assignee, Design: in.Design, Acceptance: in.Acceptance}
			var r proto.CreateResult
			err := c.Call(ctx, proto.OpCreate, args, &r)
			return Created{ID: r.ID, Rev: r.Rev, Similar: similarLine(r.Similar)}, err
		})
	add(s, tool{name: "update", desc: "Change the fields given; empty clears assignee, owner, parent.", ann: write,
		enums: enums{"status": setStatuses, "type": issueTypes}},
		func(ctx context.Context, c Conn, in UpdateIn) (proto.WriteResult, error) { return update(ctx, c, in) })
	add(s, tool{name: "close", desc: "Close a finished issue.", ann: write},
		func(ctx context.Context, c Conn, in CloseIn) (proto.WriteResult, error) {
			var out proto.WriteResult
			return out, c.Call(ctx, proto.OpClose, proto.CloseArgs{ID: in.ID, Rev: in.Rev, Reason: in.Reason}, &out)
		})
	add(s, tool{name: "reopen", desc: "Reopen a closed issue.", ann: write},
		func(ctx context.Context, c Conn, in ReopenIn) (proto.WriteResult, error) {
			var out proto.WriteResult
			return out, c.Call(ctx, proto.OpReopen, proto.ReopenArgs{ID: in.ID, Rev: in.Rev}, &out)
		})
	add(s, tool{name: "dep", desc: "id depends on depends_on; blocks keeps id out of ready.", ann: idem,
		enums: enums{"action": actions, "type": depTypes}},
		func(ctx context.Context, c Conn, in DepIn) (Ref, error) {
			args := proto.DepArgs{From: in.ID, To: in.DependsOn, Type: in.Type, Idem: in.idem}
			op := proto.OpDepAdd
			if in.Action == "rm" {
				op, args.Idem = proto.OpDepRm, ""
			}
			return Ref{ID: in.ID}, c.Call(ctx, op, args, nil)
		})
	add(s, tool{name: "label", desc: "Add or remove labels.", ann: idem, enums: enums{"action": actions}},
		func(ctx context.Context, c Conn, in LabelIn) (Ref, error) {
			if len(in.Labels) == 0 {
				return Ref{}, proto.Errf(proto.CodeInvalid, "", "labels is empty")
			}
			op := proto.OpLabelAdd
			if in.Action == "rm" {
				op = proto.OpLabelRm
			}
			// Once one label does not reach the server, the rest are still
			// tried, so that each is kept for later with the first (away).
			var lost error
			for i, l := range in.Labels {
				err := c.Call(ctx, op, proto.LabelArgs{ID: in.ID, Label: l, Idem: in.idem + "." + strconv.Itoa(i+1)}, nil)
				switch {
				case err == nil:
				case client.Unreachable(err):
					lost = cmp.Or(lost, err)
				default:
					return Ref{}, err
				}
			}
			if lost != nil {
				return Ref{}, lost
			}
			return Ref{ID: in.ID}, nil
		})
	// With conflict, a proposal: agents propose and a person resolves
	// (decision 42), so there is no resolve tool.
	add(s, tool{name: "comment", desc: "What changed, was decided or verified; with conflict, a proposal, body why.",
		ann: write, retry: true},
		func(ctx context.Context, c Conn, in CommentIn) (Commented, error) {
			if in.Conflict != 0 {
				args, err := in.proposal()
				if err == nil {
					err = c.Call(ctx, proto.OpPropose, args, nil)
				}
				return Commented{Next: "proposed; a person accepts it or resolves otherwise: carry on"}, err
			}
			var out proto.CommentResult
			err := c.Call(ctx, proto.OpComment, proto.CommentArgs{ID: in.ID, Body: in.Body, Idem: in.idem}, &out)
			return Commented{ID: out.ID}, err
		})
	add(s, tool{name: "comments", desc: "An issue's newest comments.", ann: readOnly, retry: true},
		func(ctx context.Context, c Conn, in PageIn) (Comments, error) { return comments(ctx, c, in) })
	add(s, tool{name: "history", desc: "An issue's newest changes.", ann: readOnly, retry: true},
		func(ctx context.Context, c Conn, in PageIn) (History, error) { return history(ctx, c, in) })
	s.registerMemory()
	s.registerCost()
}

// list is the list tool. A page that would pass MaxResultTokens is asked
// for again with a smaller limit, so its cursor stays exact.
func list(ctx context.Context, c Conn, in ListIn) (Issues, error) {
	args := proto.ListArgs{Status: in.Status, Type: in.Type, Priority: in.Priority, Assignee: in.Assignee,
		Parent: in.Parent, Labels: in.Labels, Limit: orDefault(in.Limit, DefaultLimit), Cursor: in.Cursor}
	if len(args.Status) == 0 {
		args.Status = []string{"open", "in_progress", "blocked", "deferred"}
	}
	for {
		var r proto.ListResult
		if err := c.Call(ctx, proto.OpList, args, &r); err != nil {
			return Issues{}, err
		}
		n := listLimit(r.Issues, r.Next)
		if n == len(r.Issues) {
			return Issues{Issues: r.Issues, Next: r.Next}, nil
		}
		// Ask again for as many as fit, so the cursor resumes after the
		// last one returned.
		args.Limit = n
	}
}

// listLimit is how many of a page of issues, with the cursor next, fit
// MaxResultTokens: all of them, or the most that fit, but at least one.
func listLimit(issues []proto.Summary, next string) int {
	return fitPrefix(len(issues), 1, func(k int) bool {
		return size(Issues{Issues: issues[:k], Next: next}) <= MaxResultTokens
	})
}

// fit drops the last issues, leaving at least one, until r is under
// MaxResultTokens, and sets More if it dropped any.
func (r *Issues) fit() {
	dropLast(&r.Issues, &r.More, func() bool { return size(r) <= MaxResultTokens })
}

// fit drops the last issues, leaving at least one, until r is under
// MaxResultTokens, and sets More if it dropped any.
func (r *Blocked) fit() {
	dropLast(&r.Issues, &r.More, func() bool { return size(r) <= MaxResultTokens })
}

// fit drops the last agents, leaving at least one, until r is under
// MaxResultTokens, and sets More if it dropped any.
func (r *Who) fit() {
	dropLast(&r.Agents, &r.More, func() bool { return size(r) <= MaxResultTokens })
}

// start takes an issue for Lease and returns it and the claim's epoch (0
// from a server without claims).
func start(ctx context.Context, c Conn, in StartIn) (Started, int64, error) {
	var r proto.StartResult
	if err := c.Call(ctx, proto.OpStart, proto.StartArgs{ID: in.ID, Lease: Lease, Take: in.Take}, &r); err != nil {
		return Started{}, 0, err
	}
	is := r.Issue
	out := Started{ID: is.ID, Rev: is.Rev, Title: is.Title, Type: is.Type, Priority: is.Priority, Body: is.Body,
		Acceptance: is.Acceptance, Items: itemLines(r.Items), Branch: gitx.Branch(is.Type, is.ID, is.Title)}
	if len(out.Items) > 0 {
		out.Acceptance = ""
	}
	fields := []*string{&out.Body, &out.Acceptance}
	if h := r.Handoff; h != nil {
		out.Handoff = &Handoff{By: h.Author, At: stamp(h.CreatedAt), Note: h.Body, State: h.State, Next: h.Next,
			Branch: h.Branch, Worktree: h.Worktree, To: h.To}
		fields = append(fields, &out.Handoff.Note)
	}
	// Memories are context; the issue is the work, so they go first.
	out.Memories, _ = compactMemories(r.Memories, startMemoryText)
	out.fitMemories()
	out.Truncated = fitTexts(func() bool { return size(out) <= MaxResultTokens }, fields...)
	var epoch int64
	if r.Claim != nil {
		epoch = r.Claim.Epoch
	}
	return out, epoch, nil
}

// show is the show tool: long text is cut, then dependents dropped, to fit
// MaxResultTokens.
func show(ctx context.Context, c Conn, in ShowIn) (Issue, error) {
	var r proto.ShowResult
	if err := c.Call(ctx, proto.OpShow, proto.ShowArgs{ID: in.ID, Full: in.Full}, &r); err != nil {
		return Issue{}, err
	}
	is := r.Issue
	out := Issue{ID: is.ID, Title: is.Title, Status: is.Status, Priority: is.Priority, Type: is.Type, Rev: is.Rev,
		Parent: is.ParentID, Assignee: is.Assignee, Owner: is.Owner, Labels: is.Labels, Body: is.Body,
		Design: is.Design, Acceptance: is.Acceptance, Notes: is.Notes, CloseReason: is.CloseReason, Truncated: is.Truncated,
		Items: itemLines(r.Items), Similar: similarLine(r.Similar), DepsMore: r.DepsMore}
	if u := r.Usage; u != nil {
		out.Account = u.Account
		out.Usage = issueUsageLine(*u)
	}
	if f := r.Files; f != nil {
		for _, p := range f.Paths {
			out.Files = append(out.Files, p.Path)
		}
		out.FilesMore = f.More
		for _, o := range f.Overlaps {
			out.Overlaps = append(out.Overlaps, o.ID+" by "+o.By+"/"+o.Session)
		}
	}
	if len(out.Items) > 0 {
		out.Acceptance = ""
	}
	texts := []*string{&out.Body, &out.Design, &out.Acceptance, &out.Notes}
	for _, c := range r.Conflicts {
		mc := Conflict{ID: c.ID, Field: c.Field, By: c.By, TheirsBy: c.TheirsBy, Advice: c.Advice}
		mc.Base, _ = cut(c.Base, maxConflictText)
		mc.Mine, _ = cut(c.Mine, maxConflictText)
		mc.Theirs, _ = cut(c.Theirs, maxConflictText)
		if p := c.Proposal; p != nil {
			mc.Proposed = p.Choice + " by " + p.By
			if p.Choice == proto.ChoiceText {
				t, _ := cut(p.Text, maxConflictText)
				mc.Proposed += ": " + t
			}
		}
		out.Conflicts = append(out.Conflicts, mc)
	}
	for i := range out.Conflicts {
		c := &out.Conflicts[i]
		texts = append(texts, &c.Base, &c.Mine, &c.Theirs)
	}
	for _, d := range r.Deps {
		if d.From == is.ID {
			out.DependsOn = append(out.DependsOn, d.To+" "+d.Type)
		} else {
			out.NeededBy = append(out.NeededBy, d.From+" "+d.Type)
		}
	}
	if fitTexts(func() bool { return size(out) <= MaxResultTokens }, texts...) {
		out.Truncated = true
	}
	for size(out) > MaxResultTokens && len(out.Conflicts) > 0 {
		out.Conflicts, out.Truncated = out.Conflicts[:len(out.Conflicts)-1], true
	}
	out.fitNeededBy()
	// One at a time: files_more is the server's count, and one below
	// zero would make the search's assumption false (see fitPrefix).
	// There are at most proto.MaxShowPaths.
	for size(out) > MaxResultTokens && len(out.Files) > 0 {
		out.Files, out.FilesMore = out.Files[:len(out.Files)-1], out.FilesMore+1
	}
	return out, nil
}

// fitMemories drops the last memories, all of them if need be, until r is
// under MaxResultTokens.
func (r *Started) fitMemories() {
	all := r.Memories
	keep := func(k int) { r.Memories = all[:k] }
	keep(fitPrefix(len(all), 0, func(k int) bool {
		keep(k)
		return size(r) <= MaxResultTokens
	}))
}

// fitNeededBy drops the last dependents, all of them if need be, until r
// is under MaxResultTokens, and sets Truncated if it dropped any.
func (r *Issue) fitNeededBy() {
	all, truncated := r.NeededBy, r.Truncated
	keep := func(k int) { r.NeededBy, r.Truncated = all[:k], truncated || k < len(all) }
	keep(fitPrefix(len(all), 0, func(k int) bool {
		keep(k)
		return size(r) <= MaxResultTokens
	}))
}

// update is the update tool. Without a rev it reads the current one
// first, so the write is refused as stale only if the issue changes in
// between.
func update(ctx context.Context, c Conn, in UpdateIn) (proto.WriteResult, error) {
	args := proto.UpdateArgs{ID: in.ID, Rev: in.Rev, Title: in.Title, Body: in.Body, Design: in.Design,
		Acceptance: in.Acceptance, Notes: in.Notes, Status: in.Status, Priority: in.Priority, Type: in.Type,
		Assignee: in.Assignee, Owner: in.Owner, Parent: in.Parent, Idem: in.idem}
	if args.Title == nil && args.Body == nil && args.Design == nil && args.Acceptance == nil && args.Notes == nil &&
		args.Status == nil && args.Priority == nil && args.Type == nil && args.Assignee == nil && args.Owner == nil &&
		args.Parent == nil {
		return proto.WriteResult{}, proto.Errf(proto.CodeInvalid, "", "nothing to update: give at least one field")
	}
	if args.Rev == 0 {
		var cur proto.ShowResult
		if err := c.Call(ctx, proto.OpShow, proto.ShowArgs{ID: in.ID}, &cur); err != nil {
			if client.Unreachable(err) {
				return proto.WriteResult{}, notQueued(c, err,
					"an update is kept for later only with the rev it was based on, which only the server knows: give rev")
			}
			return proto.WriteResult{}, err
		}
		args.Rev = cur.Issue.Rev
	}
	var out proto.WriteResult
	return out, c.Call(ctx, proto.OpUpdate, args, &out)
}

// maxCommentBody is how much of each comment a list shows.
const maxCommentBody = 1000

// omitted is how many of total a page of n left out, keeping the newest
// keep.
func omitted(n, total, keep int) int { return total - min(n, keep) }

// comments is the comments tool: the newest in.Limit (DefaultLimit if
// unset), oldest first, each cut to maxCommentBody, with the oldest
// dropped to fit MaxResultTokens.
func comments(ctx context.Context, c Conn, in PageIn) (Comments, error) {
	var r proto.CommentsResult
	limit := orDefault(in.Limit, DefaultLimit)
	if err := c.Call(ctx, proto.OpComments, proto.PageArgs{ID: in.ID, Limit: limit}, &r); err != nil {
		return Comments{}, err
	}
	all := r.Comments
	keep := min(len(all), limit)
	out := Comments{Comments: []Comment{}, Omitted: omitted(len(all), r.Total, keep)}
	for _, cm := range all[len(all)-keep:] {
		body, _ := cut(cm.Body, maxCommentBody)
		out.Comments = append(out.Comments, Comment{Author: cm.Author, At: stamp(cm.CreatedAt), Kind: cm.Kind, Body: body})
	}
	out.fit()
	return out, nil
}

// fit drops the oldest comments, leaving at least one, until r is under
// MaxResultTokens, and counts them in Omitted.
func (r *Comments) fit() {
	dropFirst(&r.Comments, &r.Omitted, func() bool { return size(r) <= MaxResultTokens })
}

// history is the history tool, by the comments tool's rules; each
// event's change is cut to 200 bytes.
func history(ctx context.Context, c Conn, in PageIn) (History, error) {
	var r proto.HistoryResult
	limit := orDefault(in.Limit, DefaultLimit)
	if err := c.Call(ctx, proto.OpHistory, proto.PageArgs{ID: in.ID, Limit: limit}, &r); err != nil {
		return History{}, err
	}
	all := r.Events
	keep := min(len(all), limit)
	out := History{Events: []Event{}, Omitted: omitted(len(all), r.Total, keep)}
	for _, e := range all[len(all)-keep:] {
		changed, _ := cut(e.Changed(), 200)
		out.Events = append(out.Events, Event{Seq: e.Seq, At: stamp(e.At), By: e.Principal, Op: e.Op, Changed: changed})
	}
	out.fit()
	return out, nil
}

// fit drops the oldest events, leaving at least one, until r is under
// MaxResultTokens, and counts them in Omitted.
func (r *History) fit() {
	dropFirst(&r.Events, &r.Omitted, func() bool { return size(r) <= MaxResultTokens })
}

// digest is the digest tool: titles are cut, in-progress and stalled
// items say for how long instead of when, and fit drops items to stay
// under MaxDigestTokens.
func digest(ctx context.Context, c Conn, in DigestIn) (Digest, error) {
	var r proto.DigestResult
	if err := c.Call(ctx, proto.OpDigest, proto.DigestArgs{Since: in.Since, By: in.By, Label: in.Label}, &r); err != nil {
		return Digest{}, err
	}
	conv := func(items []proto.DigestItem, held bool) []DigestItem {
		var out []DigestItem
		for _, it := range items {
			d := DigestItem{ID: it.ID, Priority: it.Priority, By: it.By, Note: it.Note, From: it.From, BlockedBy: it.BlockedBy}
			d.Title, _ = cut(it.Title, primeTitleLen)
			switch {
			case held:
				d.For = proto.Span(r.Until.Sub(it.At))
			case !it.At.IsZero():
				d.At = stamp(it.At)
			}
			out = append(out, d)
		}
		return out
	}
	out := Digest{Since: stamp(r.Since), Until: stamp(r.Until), Totals: r.Totals, Truncated: r.Truncated,
		Closed: conv(r.Closed, false), Started: conv(r.Started, false), InProgress: conv(r.InProgress, true),
		Stalled: conv(r.Stalled, true), Blocked: conv(r.Blocked, false), HandedOff: conv(r.HandedOff, false),
		Created: conv(r.Created, false), Discovered: conv(r.Discovered, false)}
	if u := r.Usage; u != nil {
		out.Usage = digestUsageLine(*u)
	}
	out.fit()
	return out, nil
}

// fit drops the last item of the longest section, the first of equals,
// until d is under MaxDigestTokens, so every section keeps its first items.
// The order of the drops depends only on the sections' lengths, so it is
// worked out first, and fitPrefix finds how many items to keep: keeping k
// makes the first len(drops)-k drops. Each drop takes an item and nothing
// grows, so keeping fewer is never longer (see fitPrefix).
func (d *Digest) fit() {
	sections := []*[]DigestItem{&d.Closed, &d.Started, &d.InProgress, &d.Stalled, &d.Blocked, &d.HandedOff,
		&d.Created, &d.Discovered}
	all := make([][]DigestItem, len(sections))
	lens := make([]int, len(sections))
	for i, sec := range sections {
		all[i], lens[i] = *sec, len(*sec)
	}
	// drops[j] is the section the j+1th drop takes from.
	var drops []int
	for {
		longest := -1
		for i, n := range lens {
			if n > 0 && (longest < 0 || n > lens[longest]) {
				longest = i
			}
		}
		if longest < 0 {
			break
		}
		lens[longest]--
		drops = append(drops, longest)
	}
	truncated := d.Truncated
	keep := func(k int) {
		for i, sec := range sections {
			*sec = all[i]
		}
		for _, i := range drops[:len(drops)-k] {
			*sections[i] = (*sections[i])[:len(*sections[i])-1]
		}
		d.Truncated = truncated || k < len(drops)
	}
	keep(fitPrefix(len(drops), 0, func(k int) bool {
		keep(k)
		return size(d) <= MaxDigestTokens
	}))
}

// stamp formats t for a result: UTC, to the minute.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04Z") }

// orDefault returns v, or def when v is the zero value.
func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
