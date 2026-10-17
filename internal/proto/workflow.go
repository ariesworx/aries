package proto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// A project's workflow (design §21): one document a project admin
// applies whole, which names for each issue type the acceptance items
// the server adds to every issue it creates, the checks that must pass
// before it closes, a review and an approval, and the close-out lines
// the harness carries out. Both ops are the CLI's: no MCP tool sets or
// reads it (rule 12).
const (
	OpWorkflowSet = "workflow.set" // WorkflowSetArgs → WorkflowSetResult; project admins, from the session cli
	OpWorkflow    = "workflow"     // WorkflowArgs → WorkflowResult
)

// WorkflowVersion is the version of the document this protocol speaks.
const WorkflowVersion = 1

// WorkflowDefault is the profile of every type the document does not
// name.
const WorkflowDefault = "default"

// WorkflowTypes are the issue types a profile may name, in the order a
// summary lists them. gate is not one: a gate's lifecycle is its own.
var WorkflowTypes = []string{"bug", "feature", "task", "epic", "chore"}

// Workflow is the document: its version, the principals that may report
// a required check, and a profile for each type it names.
type Workflow struct {
	Version int                        `json:"version"`
	CI      []string                   `json:"ci,omitempty"`
	Types   map[string]WorkflowProfile `json:"types,omitempty"`
}

// WorkflowProfile is what one issue type gets: acceptance items added at
// create, required checks (design §21.5), a review before close and an
// approval before start (§21.6), each a human gate, and close-out lines
// (§21.7). This version of the server adds the items; the rest is
// recorded and shown, and enforced by later slices.
type WorkflowProfile struct {
	Acceptance []WorkflowItem     `json:"acceptance,omitempty"`
	Checks     []string           `json:"checks,omitempty"`
	Review     *WorkflowApprovers `json:"review,omitempty"`
	Approve    *WorkflowApprovers `json:"approve,omitempty"`
	Closeout   []string           `json:"closeout,omitempty"`
}

// WorkflowApprovers names who passes a review or an approval.
type WorkflowApprovers struct {
	Approvers []string `json:"approvers"`
}

// WorkflowItem is one default acceptance item: its text, and the check
// whose pass ticks it, if any. In the document it is a line, or an object
// {text, check} when it is bound to a check.
type WorkflowItem struct {
	Text  string `json:"text"`
	Check string `json:"check,omitempty"`
}

// MarshalJSON writes an item bound to no check as its text alone.
func (it WorkflowItem) MarshalJSON() ([]byte, error) {
	if it.Check == "" {
		return json.Marshal(it.Text)
	}
	type plain WorkflowItem
	return json.Marshal(plain(it))
}

// UnmarshalJSON reads an item as MarshalJSON writes it, for a result.
// A document a person wrote is read by [ParseWorkflow], which is strict.
func (it *WorkflowItem) UnmarshalJSON(b []byte) error {
	if json.Unmarshal(b, &it.Text) == nil {
		it.Check = ""
		return nil
	}
	type plain WorkflowItem
	return json.Unmarshal(b, (*plain)(it))
}

// WorkflowSetArgs applies Workflow, the document, to the project. Rev,
// when not 0, is the rev the admin read: a set over another is refused
// as a conflict (rule 6). Scope is project, the default; server is the
// organization's default of a later slice (design §21.9), refused here.
type WorkflowSetArgs struct {
	Workflow json.RawMessage `json:"workflow"`
	Rev      int64           `json:"rev,omitempty"`
	Scope    string          `json:"scope,omitempty"`
}

// WorkflowSetResult is the workflow's rev after the set; Changed is false
// when the document was the one in force, which a set leaves as it is.
type WorkflowSetResult struct {
	Rev     int64 `json:"rev"`
	Changed bool  `json:"changed"`
}

// WorkflowArgs reads the project's workflow.
type WorkflowArgs struct{}

// WorkflowResult is the project's workflow: its rev (0 when none was
// ever set), the document, who set it and when, and the profile each
// issue type gets from it, by type, leaving out types that get none.
type WorkflowResult struct {
	Rev       int64                      `json:"rev"`
	Workflow  *Workflow                  `json:"workflow,omitempty"`
	SetBy     string                     `json:"set_by,omitempty"`
	SetAt     string                     `json:"set_at,omitempty"`
	Effective map[string]WorkflowProfile `json:"effective,omitempty"`
}

// ParseWorkflow decodes a workflow document from JSON, refusing what it
// does not know so that a typo is not ignored: an unknown key, a type
// that is not one of [WorkflowTypes] or default, a version other than
// [WorkflowVersion], a value of the wrong kind, a check whose name is not
// [a-z0-9][a-z0-9._-]{0,63}, an item bound to a check its profile does
// not require, and a name given twice in one list. Its errors name the
// key, as types.default.acceptance[2]. Text, principals and sizes are
// the server's to check.
func ParseWorkflow(b []byte) (Workflow, error) {
	var top map[string]json.RawMessage
	if err := decodeObject(b, "workflow", &top); err != nil {
		return Workflow{}, err
	}
	var w Workflow
	if err := knownKeys(top, "", "version", "ci", "types"); err != nil {
		return Workflow{}, err
	}
	raw, ok := top["version"]
	if !ok {
		return Workflow{}, fmt.Errorf("version is missing; give version: %d", WorkflowVersion)
	}
	if err := json.Unmarshal(raw, &w.Version); err != nil || w.Version != WorkflowVersion {
		return Workflow{}, fmt.Errorf("version must be %d, the version this aries speaks", WorkflowVersion)
	}
	if raw, ok := top["ci"]; ok {
		if err := decodeNames(raw, "ci", &w.CI); err != nil {
			return Workflow{}, err
		}
	}
	if raw, ok := top["types"]; ok {
		var types map[string]json.RawMessage
		if err := decodeObject(raw, "types", &types); err != nil {
			return Workflow{}, err
		}
		w.Types = map[string]WorkflowProfile{}
		for _, name := range sortedKeys(types) {
			if name != WorkflowDefault && !slices.Contains(WorkflowTypes, name) {
				return Workflow{}, fmt.Errorf("types.%s: not an issue type; name one of %s or %s", name,
					strings.Join(WorkflowTypes, ", "), WorkflowDefault)
			}
			p, err := parseProfile(types[name], "types."+name)
			if err != nil {
				return Workflow{}, err
			}
			w.Types[name] = p
		}
	}
	return w, nil
}

// parseProfile decodes one profile at path.
func parseProfile(b []byte, path string) (WorkflowProfile, error) {
	var keys map[string]json.RawMessage
	if err := decodeObject(b, path, &keys); err != nil {
		return WorkflowProfile{}, err
	}
	if err := knownKeys(keys, path+".", "acceptance", "checks", "review", "approve", "closeout"); err != nil {
		return WorkflowProfile{}, err
	}
	var p WorkflowProfile
	if raw, ok := keys["checks"]; ok {
		if err := decodeNames(raw, path+".checks", &p.Checks); err != nil {
			return WorkflowProfile{}, err
		}
		for i, c := range p.Checks {
			if !CheckName.MatchString(c) {
				return WorkflowProfile{}, fmt.Errorf("%s.checks[%d]: %q is not a check's name: a lowercase letter or digit, then up to 63 of a-z 0-9 . _ -", path, i, c)
			}
		}
	}
	if raw, ok := keys["acceptance"]; ok {
		var items []json.RawMessage
		if err := decodeList(raw, path+".acceptance", &items); err != nil {
			return WorkflowProfile{}, err
		}
		for i, r := range items {
			at := fmt.Sprintf("%s.acceptance[%d]", path, i)
			it, err := parseItem(r, at)
			if err != nil {
				return WorkflowProfile{}, err
			}
			if it.Check != "" && !slices.Contains(p.Checks, it.Check) {
				return WorkflowProfile{}, fmt.Errorf("%s: check %s is not among %s.checks; add it there, or bind the item to none", at, it.Check, path)
			}
			p.Acceptance = append(p.Acceptance, it)
		}
	}
	for _, f := range []struct {
		key string
		dst **WorkflowApprovers
	}{{"review", &p.Review}, {"approve", &p.Approve}} {
		raw, ok := keys[f.key]
		if !ok {
			continue
		}
		var a map[string]json.RawMessage
		at := path + "." + f.key
		if err := decodeObject(raw, at, &a); err != nil {
			return WorkflowProfile{}, err
		}
		if err := knownKeys(a, at+".", "approvers"); err != nil {
			return WorkflowProfile{}, err
		}
		*f.dst = &WorkflowApprovers{Approvers: []string{}}
		if r, ok := a["approvers"]; ok {
			if err := decodeNames(r, at+".approvers", &(*f.dst).Approvers); err != nil {
				return WorkflowProfile{}, err
			}
		}
	}
	if raw, ok := keys["closeout"]; ok {
		if err := decodeStrings(raw, path+".closeout", &p.Closeout); err != nil {
			return WorkflowProfile{}, err
		}
	}
	return p, nil
}

// parseItem decodes one acceptance item at path: a string, or an object
// {text, check}.
func parseItem(b []byte, path string) (WorkflowItem, error) {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return WorkflowItem{Text: s}, nil
	}
	var keys map[string]json.RawMessage
	if decodeObject(b, path, &keys) != nil {
		return WorkflowItem{}, fmt.Errorf("%s: an item is a line of text, or {text, check}", path)
	}
	if err := knownKeys(keys, path+".", "text", "check"); err != nil {
		return WorkflowItem{}, err
	}
	var it WorkflowItem
	for _, f := range []struct {
		key string
		dst *string
	}{{"text", &it.Text}, {"check", &it.Check}} {
		if raw, ok := keys[f.key]; ok {
			if err := json.Unmarshal(raw, f.dst); err != nil {
				return WorkflowItem{}, fmt.Errorf("%s.%s: must be a string", path, f.key)
			}
		}
	}
	if it.Check != "" && !CheckName.MatchString(it.Check) {
		return WorkflowItem{}, fmt.Errorf("%s.check: %q is not a check's name", path, it.Check)
	}
	return it, nil
}

// decodeObject decodes b, a JSON object at path, into dst.
func decodeObject(b []byte, path string, dst *map[string]json.RawMessage) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' || json.Unmarshal(b, dst) != nil {
		return fmt.Errorf("%s: must be a mapping of keys to values", path)
	}
	return nil
}

// decodeList decodes b, a JSON array at path, into dst; null is empty.
func decodeList(b []byte, path string, dst *[]json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("%s: must be a list", path)
	}
	return nil
}

// decodeStrings decodes b, a JSON array of strings at path, into dst.
func decodeStrings(b []byte, path string, dst *[]string) error {
	var items []json.RawMessage
	if err := decodeList(b, path, &items); err != nil {
		return err
	}
	for i, r := range items {
		var s string
		if err := json.Unmarshal(r, &s); err != nil {
			return fmt.Errorf("%s[%d]: must be a line of text", path, i)
		}
		*dst = append(*dst, s)
	}
	return nil
}

// decodeNames is decodeStrings for names, each given once.
func decodeNames(b []byte, path string, dst *[]string) error {
	if err := decodeStrings(b, path, dst); err != nil {
		return err
	}
	for i, s := range *dst {
		if slices.Contains((*dst)[:i], s) {
			return fmt.Errorf("%s[%d]: %s is named twice", path, i, s)
		}
	}
	return nil
}

// knownKeys refuses a key of m not among keys, naming it after prefix.
func knownKeys(m map[string]json.RawMessage, prefix string, keys ...string) error {
	for _, k := range sortedKeys(m) {
		if !slices.Contains(keys, k) {
			return fmt.Errorf("%s%s: unknown key; a %s takes %s", prefix, k, keyKind(prefix), strings.Join(keys, ", "))
		}
	}
	return nil
}

// keyKind names what holds the keys at prefix, for knownKeys.
func keyKind(prefix string) string {
	switch {
	case prefix == "":
		return "workflow"
	case strings.Count(prefix, ".") == 2:
		return "profile"
	}
	return "mapping"
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// CheckName is a required check's name, as a ci gate's (design §20.3).
var CheckName = lazyregexp.New(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Profile is the profile issue type t gets from w: the one named t, else
// default, else none. A gate gets none.
func (w *Workflow) Profile(t string) (WorkflowProfile, string, bool) {
	if w == nil || t == "gate" {
		return WorkflowProfile{}, "", false
	}
	if p, ok := w.Types[t]; ok {
		return p, t, true
	}
	if p, ok := w.Types[WorkflowDefault]; ok {
		return p, WorkflowDefault, true
	}
	return WorkflowProfile{}, "", false
}

// Effective is the profile each issue type gets from w, by type, leaving
// out the types that get none.
func (w *Workflow) Effective() map[string]WorkflowProfile {
	out := map[string]WorkflowProfile{}
	for _, t := range WorkflowTypes {
		if p, _, ok := w.Profile(t); ok {
			out[t] = p
		}
	}
	return out
}

// Summary is r's workflow in one line, for prime and `aries admin
// workflow show`: its rev, and for each group of types that get the same
// profile what it holds, as "workflow rev 4: bug, feature, task: 4 items,
// landed; epic: approval". It holds no text of the document, only counts,
// type names and check names, which are Aries's own words. "" when no
// workflow is set.
func (r WorkflowResult) Summary() string {
	if r.Rev == 0 || r.Workflow == nil {
		return ""
	}
	var groups []string
	byWhat := map[string][]string{}
	for _, t := range WorkflowTypes {
		p, ok := r.Effective[t]
		if !ok {
			continue
		}
		what := profileSummary(p)
		if _, seen := byWhat[what]; !seen {
			groups = append(groups, what)
		}
		byWhat[what] = append(byWhat[what], t)
	}
	if len(groups) == 0 {
		return fmt.Sprintf("workflow rev %d: no defaults", r.Rev)
	}
	parts := make([]string, len(groups))
	for i, what := range groups {
		parts[i] = strings.Join(byWhat[what], ", ") + ": " + what
	}
	return fmt.Sprintf("workflow rev %d: %s", r.Rev, strings.Join(parts, "; "))
}

// profileSummary is p in a few words.
func profileSummary(p WorkflowProfile) string {
	var w []string
	switch n := len(p.Acceptance); n {
	case 0:
	case 1:
		w = append(w, "1 item")
	default:
		w = append(w, fmt.Sprintf("%d items", n))
	}
	w = append(w, p.Checks...)
	if p.Review != nil {
		w = append(w, "review")
	}
	if p.Approve != nil {
		w = append(w, "approval")
	}
	if len(w) == 0 {
		return "nothing"
	}
	return strings.Join(w, ", ")
}
