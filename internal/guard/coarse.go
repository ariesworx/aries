package guard

import "strings"

// Claude Code's own permission rules hold a coarse copy of the policy
// (docs/design/guard.md §8, decision 5): the dispatcher's --allowedTools
// and --disallowedTools, and the permissions.deny `aries setup
// claude-code` writes. Claude Code matches a rule against the command's
// text, `*` standing for any text and a trailing ` *` also matching the
// bare command, and a deny rule matches past a leading VAR=value. The
// guard is the exact check; this copy is the layer that holds when the
// guard does not run, so it errs toward denying, never toward allowing.

// Coarse is the deny rules of p, the built-in ones first, as Claude
// Code's rules: a run: rule of words alone as Bash(words *); one with
// flags as the words followed by each flag, right after them or later in
// the command; one with refs as a push whose last refspec, or its
// destination after `:`, is the ref; one with config as `git -c KEY`; a
// path: rule as Edit(path), which Claude Code also applies to the Write
// tool. Each rule appears once, in order.
func Coarse(p *Policy) []string {
	var out []string
	seen := map[string]bool{}
	add := func(rule string) {
		if !seen[rule] {
			seen[rule] = true
			out = append(out, rule)
		}
	}
	for _, r := range append(append([]Rule(nil), builtinDeny...), p.Deny...) {
		for _, rule := range coarseRule(r) {
			add(rule)
		}
	}
	return out
}

// coarseRule is one deny rule's Claude Code rules.
func coarseRule(r Rule) []string {
	var out []string
	for _, path := range r.Paths {
		out = append(out, "Edit("+path+")")
	}
	if len(r.Words) == 0 {
		return out
	}
	words := strings.Join(r.Words, " ")
	if len(r.Flags)+len(r.Refs)+len(r.Config) == 0 {
		return append(out, "Bash("+words+" *)")
	}
	for _, f := range r.Flags {
		out = append(out, "Bash("+words+" "+f+"*)", "Bash("+words+" * "+f+"*)")
		if f == "--force" && words == "git push" {
			out = append(out, "Bash(git push * +*)") // a +refspec forces too
		}
	}
	for _, ref := range r.Refs {
		out = append(out, "Bash("+words+" * "+ref+")", "Bash("+words+" * "+ref+" *)",
			"Bash("+words+" *:"+ref+")", "Bash("+words+" *:"+ref+" *)",
			"Bash("+words+" *:refs/heads/"+ref+")", "Bash("+words+" *:refs/heads/"+ref+" *)")
	}
	for _, key := range r.Config {
		key = strings.TrimRight(key, "*")
		out = append(out, "Bash("+words+" -c "+key+"*)", "Bash("+words+" * -c "+key+"*)")
	}
	return out
}

// Allowed is the policy's allow list as Claude Code's rules, for the
// dispatcher's --allowedTools: the tools: names, then each run: rule as
// Bash(words *). Claude Code's own matching does not read past a
// variable assignment or a wrapper; the guard's allow covers those.
func Allowed(p *Policy) []string {
	out := append([]string(nil), p.Tools...)
	for _, words := range p.Allow {
		out = append(out, "Bash("+strings.Join(words, " ")+" *)")
	}
	return out
}
