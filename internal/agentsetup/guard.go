package agentsetup

import (
	"encoding/json"
	"fmt"
	"slices"
)

// The guard (docs/design/guard.md §3, §8) is Claude Code's alone, and a
// project's: `aries guard --hook` as a PreToolUse hook for the shell and
// file tools, and the coarse copy of the policy's denies (Entry.Deny) in
// the settings' permissions.deny, which hold when the guard does not
// run. Setup writes no permissions.allow: the guard's allow is the one
// allow list. It adds the deny rules it is given and keeps the person's;
// a rule the policy no longer holds stays until removed by hand, since a
// rule of the person's reads the same.

// guardHook is the guard's PreToolUse hook: its matcher and timeout, in
// seconds, are the design's (§3, §11).
var guardHook = hookStyle{event: "PreToolUse", verb: "guard", matchers: []string{"Bash|Edit|Write|NotebookEdit"}, timeout: 5}

// guardHarness is the one harness the guard is set up for.
const guardHarness = "claude-code"

// applyGuard puts the guard's hook and e.Deny in content. The result is
// the hook's when it changed, else Updated when a deny rule was added.
func applyGuard(content []byte, e Entry) ([]byte, Result, error) {
	out, res, err := guardHook.apply(content, e, guardHarness)
	if err != nil {
		return nil, Unchanged, err
	}
	out, added, err := editDeny(out, func(have []string) []string {
		for _, d := range e.Deny {
			if !slices.Contains(have, d) {
				have = append(have, d)
			}
		}
		return have
	})
	switch {
	case err != nil:
		return nil, Unchanged, err
	case res == Unchanged && !added:
		return content, Unchanged, nil
	case res == Unchanged:
		res = Updated
	}
	return out, res, nil
}

// removeGuard takes the guard's hook and e.Deny's rules out of content.
func removeGuard(content []byte, e Entry) ([]byte, Result, error) {
	out, res, err := guardHook.remove(content, e, guardHarness)
	if err != nil {
		return nil, Unchanged, err
	}
	out, removed, err := editDeny(out, func(have []string) []string {
		return slices.DeleteFunc(have, func(d string) bool { return slices.Contains(e.Deny, d) })
	})
	switch {
	case err != nil:
		return nil, Unchanged, err
	case res == Unchanged && !removed:
		return content, Unchanged, nil
	}
	return out, Removed, nil
}

// missingDeny lists e.Deny's rules content's permissions.deny lacks.
func missingDeny(content []byte, e Entry) []string {
	have, err := denyRules(content)
	if err != nil {
		return slices.Clone(e.Deny)
	}
	var out []string
	for _, d := range e.Deny {
		if !slices.Contains(have, d) {
			out = append(out, d)
		}
	}
	return out
}

// denyRules reads permissions.deny's strings.
func denyRules(content []byte) ([]string, error) {
	_, _, list, err := openDeny(content)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, raw := range list {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			out = append(out, s)
		}
	}
	return out, nil
}

// openDeny reads content down to permissions.deny; a missing level is
// empty, one of the wrong type an error naming its path.
func openDeny(content []byte) (root, perms object, list []json.RawMessage, err error) {
	if root, err = parseObject(content); err != nil {
		return nil, nil, nil, err
	}
	perms = object{}
	if raw, ok := root.get("permissions"); ok {
		if perms, err = parseObject(raw); err != nil {
			return nil, nil, nil, fmt.Errorf("permissions: %w", err)
		}
	}
	raw, _ := perms.get("deny")
	if list, err = parseArray(raw); err != nil {
		return nil, nil, nil, fmt.Errorf("permissions.deny: %w", err)
	}
	return root, perms, list, nil
}

// editDeny sets permissions.deny's strings to what f makes of them,
// keeping its other elements where they were, and reports whether that
// changed it. Levels left empty are dropped.
func editDeny(content []byte, f func([]string) []string) ([]byte, bool, error) {
	root, perms, list, err := openDeny(content)
	if err != nil {
		return nil, false, err
	}
	var have []string
	var others []json.RawMessage
	for _, raw := range list {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			have = append(have, s)
		} else {
			others = append(others, raw)
		}
	}
	want := f(slices.Clone(have))
	if slices.Equal(want, have) {
		return content, false, nil
	}
	kept := others
	for _, s := range want {
		kept = append(kept, mustJSON(s))
	}
	if len(kept) > 0 {
		perms = perms.set("deny", marshalArray(kept))
	} else {
		perms = perms.del("deny")
	}
	if len(perms) > 0 {
		root = root.set("permissions", perms.marshal())
	} else {
		root = root.del("permissions")
	}
	out, err := indent(root.marshal())
	return out, true, err
}
