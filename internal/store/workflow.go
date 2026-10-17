package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/secretscan"
)

// The project's workflow (design §21, decision 95). It is one row of the
// settings table, named workflow, whose rev is the workflow's version: a
// document a project admin applies whole, from their own terminal, with
// a workflow.set event holding the document before and after. The
// server applies it in the write that makes an issue: create, a finish's
// discovered issues and a change of type append the profile's items to
// the issue's acceptance text, and the event records the rev and the
// profile it applied (WorkflowStamp). A later set changes no issue
// already made: what the workflow added is the issue's own text, and
// leaves it only ticked or waived (checkDropped). The import, templates
// and gates take no defaults.

// OpWorkflowSet records a change of the workflow: its after state is the
// rev and the document, its before state the same before the change, or
// NULL for the first.
const OpWorkflowSet Op = "workflow.set"

// WorkflowTarget is workflow.set's target. No issue id matches it, so an
// event watch is not pushed it.
const WorkflowTarget = "setting:workflow"

// workflowSetting is the workflow's row in settings.
const workflowSetting = "workflow"

// WorkflowSetting is the project's workflow as the store holds it: its
// rev, 0 and no document when none was ever set, and who set it when.
type WorkflowSetting struct {
	Rev      int64
	Workflow *proto.Workflow
	SetBy    string
	SetAt    time.Time
}

// WorkflowStamp is what an issue's event records of the workflow it was
// made under: the setting's rev, and the profile applied (a type, or
// default), empty when its type got none.
type WorkflowStamp struct {
	Rev  int64  `json:"rev"`
	Type string `json:"type,omitempty"`
}

// The bounds of the workflow limits (design §21.10). A document is read
// whole by every create.
const (
	maxWorkflowBytes    = 65536
	maxWorkflowItems    = 100
	maxWorkflowCloseout = 100
)

// workflowLine bounds one line of a workflow, an item or a close-out
// line: an item becomes an issue's acceptance item.
const workflowLine = maxTitle

// InPersonError refuses an act that is a person's at their own terminal
// (the session cli) when it comes from any other session: an agent's,
// which works under its person's key, or a CI job's. It wraps
// ErrForbidden.
type InPersonError struct{ Action string }

func (e *InPersonError) Error() string {
	return e.Action + " is a person's act at their own terminal, not an agent's or a CI job's"
}

// Unwrap makes errors.Is(err, ErrForbidden) hold.
func (e *InPersonError) Unwrap() error { return ErrForbidden }

// Workflow reads the project's workflow.
func (s *Store) Workflow(ctx context.Context) (WorkflowSetting, error) {
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return WorkflowSetting{}, fmt.Errorf("workflow: %w", err)
	}
	defer end()
	var out WorkflowSetting
	w, rev, err := readWorkflow(ctx, tx)
	if err != nil || w == nil {
		return out, err
	}
	out.Rev, out.Workflow = rev, w
	err = tx.QueryRowContext(ctx, `SELECT principal, at FROM events WHERE target = $1 ORDER BY seq DESC LIMIT 1`,
		WorkflowTarget).Scan(&out.SetBy, &out.SetAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return WorkflowSetting{}, fmt.Errorf("workflow: %w", err)
	}
	out.SetAt = out.SetAt.UTC()
	return out, nil
}

// readWorkflow reads the workflow's document and rev; nil and 0 when
// none was ever set.
func readWorkflow(ctx context.Context, q querier) (*proto.Workflow, int64, error) {
	var (
		value []byte
		rev   int64
	)
	err := q.QueryRowContext(ctx, `SELECT value, rev FROM settings WHERE name = $1`, workflowSetting).Scan(&value, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read the workflow: %w", err)
	}
	var w proto.Workflow
	if err := json.Unmarshal(value, &w); err != nil {
		return nil, 0, fmt.Errorf("read the workflow: %w", err)
	}
	return &w, rev, nil
}

// SetWorkflow applies w as the project's workflow, and returns its rev
// and whether it changed: a document equal to the one in force writes
// nothing. expected, when not 0, is the rev the caller read, and a set
// over another is refused with ErrConflict (rule 6).
//
// Only an admin ([Store.IsAdmin]) sets it, and only from the session cli
// (proto.CLISession): anyone else is refused with a [*ForbiddenError],
// and an admin's agent, or a CI job, with an [*InPersonError]. A
// document [proto.ParseWorkflow] would refuse, a line that is not one
// line of safe text, an item that would not read back as itself, a line
// that looks like a secret, a principal that is not a name, is reserved
// or, where Options.IsMember says, is not a member, and a document past
// the workflow limits, are refused with ErrInvalid, naming the key.
func (s *Store) SetWorkflow(ctx context.Context, actor Actor, w proto.Workflow, expected int64) (int64, bool, error) {
	if !s.IsAdmin(actor.Principal) {
		return 0, false, &ForbiddenError{Action: "workflow set"}
	}
	if actor.Session != proto.CLISession {
		return 0, false, &InPersonError{Action: "setting the workflow"}
	}
	if expected < 0 {
		return 0, false, fmt.Errorf("%w: expected rev %d", ErrInvalid, expected)
	}
	doc, err := s.checkWorkflow(w)
	if err != nil {
		return 0, false, err
	}
	var (
		rev     int64
		changed bool
	)
	err = s.write(ctx, actor, func(wt *wtx) error {
		rev, changed = 0, false
		cur, curRev, err := readWorkflow(ctx, wt.tx)
		if err != nil {
			return err
		}
		if expected != 0 && expected != curRev {
			return fmt.Errorf("the workflow is at rev %d, not %d: %w", curRev, expected, ErrConflict)
		}
		value, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if cur != nil {
			if was, err := json.Marshal(cur); err == nil && bytes.Equal(was, value) {
				rev = curRev
				return nil
			}
		}
		rev, changed = curRev+1, true
		if cur == nil {
			_, err = wt.exec(ctx, `INSERT INTO settings (name, value, source, updated_at, rev) VALUES ($1, $2, 'workflow.set', $3, 1)`,
				workflowSetting, value, wt.now)
		} else {
			var n int64
			n, err = wt.exec(ctx, `UPDATE settings SET value = $1, updated_at = $2, rev = rev + 1 WHERE name = $3 AND rev = $4`,
				value, wt.now, workflowSetting, curRev)
			if err == nil && n != 1 {
				err = fmt.Errorf("the workflow changed during the set: %w", ErrConflict)
			}
		}
		if err != nil {
			return fmt.Errorf("set the workflow: %w", err)
		}
		var before any
		if cur != nil {
			before = map[string]any{"rev": curRev, "workflow": cur}
		}
		return wt.event(ctx, OpWorkflowSet, WorkflowTarget, before, map[string]any{"rev": rev, "workflow": doc})
	})
	if err != nil {
		return 0, false, err
	}
	return rev, changed, nil
}

// checkWorkflow refuses w as SetWorkflow says, and returns it as the
// store keeps it: item text with its whitespace collapsed, as items are
// read, and in the form ParseWorkflow gives, so that two equal documents
// compare equal.
func (s *Store) checkWorkflow(w proto.Workflow) (proto.Workflow, error) {
	lim := s.opts.Limits
	b, err := json.Marshal(w)
	if err != nil {
		return proto.Workflow{}, err
	}
	// The protocol's own checks first: the keys, types and check names.
	// ParseWorkflow gives a copy, which the store may change.
	doc, err := proto.ParseWorkflow(b)
	if err != nil {
		return proto.Workflow{}, fmt.Errorf("%w: workflow: %s", ErrInvalid, err)
	}
	for _, prof := range doc.Types {
		for i := range prof.Acceptance {
			prof.Acceptance[i].Text = collapse(prof.Acceptance[i].Text)
		}
	}
	if b, err = json.Marshal(doc); err != nil {
		return proto.Workflow{}, err
	}
	if len(b) > lim.WorkflowBytes {
		return proto.Workflow{}, fmt.Errorf("%w: the workflow is %d bytes; it can be at most %d (workflow_bytes)", ErrInvalid, len(b), lim.WorkflowBytes)
	}
	if err := s.checkPrincipals("ci", doc.CI); err != nil {
		return proto.Workflow{}, err
	}
	for _, name := range sortedNames(doc.Types) {
		p, at := doc.Types[name], "types."+name
		for _, c := range []struct {
			key   string
			n, at int
			limit string
		}{
			{"acceptance", len(p.Acceptance), lim.WorkflowItems, "workflow_items_per_type"},
			{"checks", len(p.Checks), lim.WorkflowChecks, "workflow_checks_per_type"},
			{"closeout", len(p.Closeout), lim.WorkflowCloseout, "workflow_closeout_per_type"},
		} {
			if c.n > c.at {
				return proto.Workflow{}, fmt.Errorf("%w: %s.%s has %d; a profile has at most %d (%s)", ErrInvalid, at, c.key, c.n, c.at, c.limit)
			}
		}
		for i, it := range p.Acceptance {
			field := fmt.Sprintf("%s.acceptance[%d]", at, i)
			if err := checkWorkflowLine(field, it.Text); err != nil {
				return proto.Workflow{}, err
			}
			if got := parseAcceptance("- " + it.Text); len(got) != 1 || got[0].text != it.Text || got[0].ticked {
				return proto.Workflow{}, fmt.Errorf("%w: %s reads as another item in an issue's acceptance text; leave out a leading [ ] or [x]", ErrInvalid, field)
			}
		}
		for i, line := range p.Closeout {
			if err := checkWorkflowLine(fmt.Sprintf("%s.closeout[%d]", at, i), line); err != nil {
				return proto.Workflow{}, err
			}
		}
		for _, g := range []struct {
			key string
			a   *proto.WorkflowApprovers
		}{{"review", p.Review}, {"approve", p.Approve}} {
			if g.a == nil {
				continue
			}
			if len(g.a.Approvers) > lim.GateApprovers {
				return proto.Workflow{}, fmt.Errorf("%w: %s.%s.approvers has %d; a gate names at most %d (approvers_per_gate)",
					ErrInvalid, at, g.key, len(g.a.Approvers), lim.GateApprovers)
			}
			if err := s.checkPrincipals(at+"."+g.key+".approvers", g.a.Approvers); err != nil {
				return proto.Workflow{}, err
			}
		}
	}
	return doc, nil
}

// checkWorkflowLine refuses a workflow's line at field that is not one
// line of safe text, or looks like it holds a secret: it is copied into
// every issue the workflow governs.
func checkWorkflowLine(field, v string) error {
	if err := checkLine(field, strings.TrimSpace(v), workflowLine, true); err != nil {
		return err
	}
	if found, ok := secretscan.Find(v); ok {
		return fmt.Errorf("%w: %s looks like it holds a secret (%s); a workflow's lines are copied into issues", ErrInvalid, field, found.Kind)
	}
	return nil
}

// checkPrincipals refuses a name at field that is not a principal's, is
// reserved, or is not a member of the project, when Options.IsMember can
// say.
func (s *Store) checkPrincipals(field string, names []string) error {
	for i, p := range names {
		switch {
		case !PrincipalPattern.MatchString(p) || Reserved(p):
			return fmt.Errorf("%w: %s[%d]: %q is not a principal name, or is reserved", ErrInvalid, field, i, p)
		case s.opts.IsMember != nil && !s.opts.IsMember(p):
			return fmt.Errorf("%w: %s[%d]: %s is not a member of the project; add them first, or leave them out", ErrInvalid, field, i, p)
		}
	}
	return nil
}

func sortedNames[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// workflowDefaults returns text with the items of the profile issue type
// t gets from the workflow in force appended, how many it added, and the
// stamp to record, nil when no workflow is set. It refuses, with
// ErrInvalid, text and defaults that together pass the
// acceptance_items_per_issue limit.
func (w *wtx) workflowDefaults(ctx context.Context, t IssueType, text string) (string, int, *WorkflowStamp, error) {
	doc, rev, err := readWorkflow(ctx, w.tx)
	if err != nil || doc == nil {
		return text, 0, nil, err
	}
	stamp := &WorkflowStamp{Rev: rev}
	p, name, ok := doc.Profile(string(t))
	if !ok {
		return text, 0, stamp, nil
	}
	stamp.Type = name
	items := make([]string, len(p.Acceptance))
	for i, it := range p.Acceptance {
		items[i] = it.Text
	}
	out, n := withItems(text, items)
	if n == 0 {
		return text, 0, stamp, nil
	}
	if own, total := len(parseAcceptance(text)), len(parseAcceptance(out)); total > w.lim.AcceptanceItems {
		return "", 0, nil, fmt.Errorf("%w: acceptance would have %d items, %d of its own and %d from the workflow's %s profile; an issue has at most %d acceptance items",
			ErrInvalid, total, own, n, name, w.lim.AcceptanceItems)
	}
	if err := checkText("acceptance with the workflow's items", out, maxText, false); err != nil {
		return "", 0, nil, err
	}
	return out, n, stamp, nil
}

// typeDefaults returns the columns and values an update writes, pp's,
// and the workflow to record on its event. A change of an open issue's
// type adds the items of the profile the new type gets that the issue's
// text, or the update's new text, lacks; what the old type added stays.
// pp is not changed, since the write may run again.
func (w *wtx) typeDefaults(ctx context.Context, before Issue, pp prepared) ([]string, []any, map[string]any, error) {
	patch := pp.patch
	if patch.Type == nil || *patch.Type == before.Type || before.Status == StatusClosed {
		return pp.sets, pp.args, nil, nil
	}
	text := before.Acceptance
	if patch.Acceptance != nil {
		text = *patch.Acceptance
	}
	out, n, stamp, err := w.workflowDefaults(ctx, *patch.Type, text)
	if err != nil || stamp == nil {
		return pp.sets, pp.args, nil, err
	}
	extra := map[string]any{"workflow": stamp}
	if n == 0 {
		return pp.sets, pp.args, extra, nil
	}
	sets, args := slices.Clone(pp.sets), slices.Clone(pp.args)
	if i := slices.Index(sets, "acceptance"); i >= 0 {
		args[i] = out
	} else {
		sets, args = append(sets, "acceptance"), append(args, out)
	}
	return sets, args, extra, nil
}

// withItems returns text with each of items it lacks appended as a
// Markdown list entry, once, and how many it appended. Text that holds
// no list is one item (parseAcceptance): it becomes the list's first
// entry, so that it stays an item.
func withItems(text string, items []string) (string, int) {
	have := map[string]bool{}
	parsed := parseAcceptance(text)
	for _, p := range parsed {
		have[p.text] = true
	}
	var add []string
	for _, it := range items {
		if !have[it] {
			have[it] = true
			add = append(add, "- "+it)
		}
	}
	if len(add) == 0 {
		return text, 0
	}
	base := strings.TrimRight(text, " \t\r\n")
	if len(parsed) > 0 && !isList(text) {
		base = "- " + parsed[0].text
	}
	if base != "" {
		base += "\n"
	}
	return base + strings.Join(add, "\n"), len(add)
}

// isList reports whether text holds a Markdown list entry, as
// parseAcceptance reads one.
func isList(text string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		if listItem.MatchString(strings.TrimRight(line, "\r")) {
			return true
		}
	}
	return false
}
