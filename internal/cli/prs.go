package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ariesworx/aries/internal/gitx"
	"github.com/ariesworx/aries/internal/merges"
	"github.com/ariesworx/aries/internal/proto"
)

const prsUsage = "prs merges [--rules FILE] [PR...]"

// mergesDoc is `aries prs merges --json`.
type mergesDoc struct {
	// Rule is the rule the verdicts come from; nil with none.
	Rule *mergesRule      `json:"rule"`
	PRs  []merges.Verdict `json:"prs"`
}

type mergesRule struct {
	// Source is the file, and for the default the commit it was read at.
	Source  string `json:"source"`
	Version int    `json:"version"`
	SHA256  string `json:"sha256"`
}

// cmdPrs is `aries prs`, the commands over the repository's open pull
// requests. merges is the one so far (docs/design/merges.md, slice M1).
func cmdPrs(ctx context.Context, r *runner, args []string) error {
	args = r.leading(args)
	if len(args) == 0 {
		return usagef(prsUsage, "prs needs merges")
	}
	if args[0] != "merges" {
		return usagef(prsUsage, "unknown prs action %q", args[0])
	}
	return prsMerges(ctx, r, args[1:])
}

// prsMerges prints the merge rule's verdict on each open pull request:
// queue, naming the class that passed it, or hold, naming the condition
// that failed. It is CLI only and read only: it reads the CI host with
// this machine's gh (rule 12) and queues nothing (that is M2).
func prsMerges(ctx context.Context, r *runner, args []string) error {
	fs := r.newFlags("prs merges")
	rulesFile := fs.String("rules", "", "read the rule from FILE (default: "+merges.File+" on the default branch, as the last fetch left it)")
	pos, err := parse(fs, args, prsUsage)
	if err != nil {
		return err
	}
	var want []int
	for _, a := range pos {
		n, err := strconv.Atoi(strings.TrimPrefix(a, "#"))
		if err != nil || n <= 0 {
			return usagef(prsUsage, "%q is not a pull request's number", a)
		}
		want = append(want, n)
	}
	top, err := gitx.Top(ctx, r.dir)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "run it in the repository whose pull requests to read, or give -C DIR", "not in a git repository: "+err.Error())
	}
	base, err := gitx.DefaultBranch(ctx, top)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "fetch the remote, so that origin/HEAD names the default branch", "read the default branch: "+err.Error())
	}
	rules, rule, rulesPath, err := loadMergeRules(ctx, r, top, base, *rulesFile)
	if err != nil {
		return err
	}
	var forge merges.Forge
	switch _, lerr := exec.LookPath("gh"); {
	case r.env.MergesForge != nil:
		forge = r.env.MergesForge(top)
	case lerr == nil:
		forge = &merges.GH{Dir: top}
	default:
		return proto.Errf(proto.CodeInvalid, "install gh and sign it in (`gh auth status`)", "gh is not on PATH: the pull requests cannot be read")
	}
	doc := mergesDoc{Rule: rule}
	doc.PRs, err = mergeVerdicts(ctx, forge, &outcomeStore{r: r, shows: map[string]proto.ShowResult{}}, rules,
		merges.Context{Base: base, RulesPath: rulesPath}, want)
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "check that `gh auth status` passes here", "read the open pull requests: "+err.Error())
	}
	if r.json {
		r.emit(doc)
		return nil
	}
	r.printMerges(doc, rules)
	return nil
}

// loadMergeRules reads the rule: FILE when given, else the default
// branch's merges.File, which may not exist (every pull request is then
// held). rulesPath is FILE from the repository's top when FILE is in it.
func loadMergeRules(ctx context.Context, r *runner, top, base, file string) (*merges.Rules, *mergesRule, string, error) {
	var b []byte
	var source, rulesPath string
	if file != "" {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.dir, path)
		}
		var err error
		if b, err = os.ReadFile(path); err != nil { //nolint:gosec // a file the person names
			return nil, nil, "", proto.Errf(proto.CodeInvalid, "give --rules a readable file", "read the rule: "+err.Error())
		}
		source = file
		if rel, err := filepath.Rel(top, path); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			rulesPath = filepath.ToSlash(rel)
		}
	} else {
		rev := "origin/" + base
		got, ok, err := gitx.ShowFile(ctx, top, rev, merges.File)
		if err != nil {
			return nil, nil, "", proto.Errf(proto.CodeInvalid, "fetch the remote, or give --rules FILE", "read "+merges.File+" at "+rev+": "+err.Error())
		}
		if !ok {
			return nil, nil, "", nil
		}
		b, source = got, merges.File+" at "+rev
	}
	rules, err := merges.Load(b)
	if err != nil {
		return nil, nil, "", proto.Errf(proto.CodeInvalid, "correct the file; docs/cli.md describes it", fmt.Sprintf("%s: %v", source, err))
	}
	return rules, &mergesRule{Source: source, Version: rules.Version, SHA256: rules.SHA}, rulesPath, nil
}

// mergeVerdicts reads each open pull request, or those of want, and its
// issue, and classifies it. A number of want that is not open is held.
func mergeVerdicts(ctx context.Context, f merges.Forge, st merges.Store, rules *merges.Rules, c merges.Context, want []int) ([]merges.Verdict, error) {
	open, err := f.Open(ctx)
	if err != nil {
		return nil, err
	}
	out := []merges.Verdict{}
	for _, pr := range open {
		if len(want) > 0 && !slices.Contains(want, pr.Number) {
			continue
		}
		pr = merges.Read(ctx, f, pr)
		if id, ok := gitx.IDFromBranch(pr.Branch, ""); ok {
			pr.Issue = merges.ReadIssue(ctx, st, id)
		}
		out = append(out, merges.Classify(rules, c, pr))
	}
	for _, n := range want {
		if !slices.ContainsFunc(open, func(p merges.PR) bool { return p.Number == n }) {
			out = append(out, merges.Classify(rules, c, merges.PR{Number: n, Err: "not an open pull request"}))
		}
	}
	return out, nil
}

func (r *runner) printMerges(doc mergesDoc, rules *merges.Rules) {
	w := r.env.Stdout
	if doc.Rule == nil {
		_, _ = fmt.Fprintf(w, "no rule: %s is not on the default branch; every pull request is held\n", merges.File)
	} else {
		_, _ = fmt.Fprintf(w, "rule %s, version %d, sha256 %s\n", esc(doc.Rule.Source), doc.Rule.Version, doc.Rule.SHA256[:12])
	}
	if len(doc.PRs) == 0 {
		_, _ = fmt.Fprintln(w, "no open pull request")
	}
	for _, v := range doc.PRs {
		verdict, rest, _ := strings.Cut(v.String(), " ")
		_, _ = fmt.Fprintf(w, "%s #%d %s\n", verdict, v.Number, esc(rest))
	}
	if rules != nil && slices.ContainsFunc(rules.Classes, func(c merges.Class) bool { return c.Opener != nil }) {
		_, _ = fmt.Fprintln(w, "the opener is unverified until briefs are stored, so no class that asks for it passes")
	}
}
