package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ariesworx/aries/internal/proto"
)

// A project's workflow (design §21.3): a file in the repository,
// `.aries/workflow.yaml` by convention, which a project admin applies
// with `aries workflow set FILE` once the pull request that changed
// it merges. aries reads YAML or JSON, checks it as the server will, and
// sends JSON, so ariesd parses no YAML. No MCP tool sets or reads it.

const workflowUsage = "admin workflow show | set FILE [--rev N] | diff FILE | check FILE"

const workflowCmdUsage = "workflow show | set FILE [--rev N] | diff FILE | check FILE"

// maxWorkflowFile bounds the file aries reads: the server keeps at most
// 64 KiB of JSON, and YAML with its comments may be a few times that.
const maxWorkflowFile = 1 << 18

func adminWorkflow(ctx context.Context, r *runner, args []string) error {
	return runWorkflow(ctx, r, args, "admin workflow", workflowUsage)
}

// cmdWorkflow is `aries workflow show | set | diff | check`. `aries admin
// workflow` is the same command under its older name. show, diff and
// check are for any member, a CI job among them (check exits 1 when the
// server's workflow differs from the file); the server refuses set unless
// the caller is an admin.
func cmdWorkflow(ctx context.Context, r *runner, args []string) error {
	return runWorkflow(ctx, r, args, "workflow", workflowCmdUsage)
}

func runWorkflow(ctx context.Context, r *runner, args []string, name, usage string) error {
	fs := r.newFlags(name)
	rev := fs.Int64("rev", 0, "the rev you read with show: refuse the set if another was applied since")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "%s needs show, set, diff or check", name)
	}
	if set(fs, "rev") && pos[0] != "set" {
		return usagef(usage, "only workflow set takes --rev")
	}
	switch pos[0] {
	case "show":
		if len(pos) != 1 {
			return usagef(usage, "workflow show takes no arguments")
		}
		return showWorkflow(ctx, r)
	case "set", "diff", "check":
		if len(pos) != 2 {
			return usagef(usage, "workflow %s needs one file, such as .aries/workflow.yaml", pos[0])
		}
		switch pos[0] {
		case "set":
			return setWorkflow(ctx, r, pos[1], *rev)
		case "diff":
			return diffWorkflow(ctx, r, pos[1], false)
		}
		return diffWorkflow(ctx, r, pos[1], true)
	}
	return usagef(usage, "unknown workflow action %q", pos[0])
}

// readWorkflow reads the workflow file at file, relative to -C, in YAML
// or JSON, and returns it as JSON with the document it holds. A document
// the server would refuse for its keys or types is refused here, naming
// the key.
func (r *runner) readWorkflow(file string) (json.RawMessage, proto.Workflow, error) {
	path := file
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.dir, path)
	}
	b, err := readUpTo(path, maxWorkflowFile+1)
	const give = "give the workflow file, such as .aries/workflow.yaml"
	switch {
	case err != nil:
		cause := err
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			cause = pe.Err
		}
		return nil, proto.Workflow{}, proto.Errf(proto.CodeInvalid, give, fmt.Sprintf("cannot read %s: %v", file, cause))
	case len(b) > maxWorkflowFile:
		return nil, proto.Workflow{}, proto.Errf(proto.CodeInvalid, "shorten it", fmt.Sprintf("%s is larger than %d KiB", file, maxWorkflowFile>>10))
	}
	var doc any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, proto.Workflow{}, proto.Errf(proto.CodeInvalid, "correct the file", fmt.Sprintf("%s is not YAML or JSON: %v", file, err))
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, proto.Workflow{}, proto.Errf(proto.CodeInvalid, "correct the file; its keys are words, and its values text and lists",
			fmt.Sprintf("%s does not convert to JSON: %v", file, err))
	}
	w, err := proto.ParseWorkflow(raw)
	if err != nil {
		return nil, proto.Workflow{}, proto.Errf(proto.CodeInvalid, "correct the file; docs/concepts.md has the keys",
			fmt.Sprintf("%s: %v", file, err))
	}
	return raw, w, nil
}

func setWorkflow(ctx context.Context, r *runner, file string, rev int64) error {
	raw, _, err := r.readWorkflow(file)
	if err != nil {
		return err
	}
	var out proto.WorkflowSetResult
	if err := r.call(ctx, proto.OpWorkflowSet, proto.WorkflowSetArgs{Workflow: raw, Rev: rev}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	if !out.Changed {
		_, _ = fmt.Fprintf(r.env.Stdout, "workflow unchanged at rev %d: %s is what the server has\n", out.Rev, esc(file))
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "workflow rev %d set from %s; it applies to issues created from now on\n", out.Rev, esc(file))
	return nil
}

func showWorkflow(ctx context.Context, r *runner) error {
	var out proto.WorkflowResult
	if err := r.call(ctx, proto.OpWorkflow, proto.WorkflowArgs{}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	if out.Workflow == nil {
		_, _ = fmt.Fprintln(r.env.Stdout, "no workflow; a project admin applies one with `aries admin workflow set FILE`")
		return nil
	}
	_, _ = fmt.Fprintln(r.env.Stdout, esc(out.Summary()))
	_, _ = fmt.Fprintf(r.env.Stdout, "set by %s at %s\n\n", esc(out.SetBy), esc(out.SetAt))
	b, err := workflowYAML(*out.Workflow)
	if err != nil {
		return err
	}
	// The document is people's text, printed line by line as any is.
	for line := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
		_, _ = fmt.Fprintln(r.env.Stdout, esc(line))
	}
	return nil
}

// workflowYAML is w as a person would write it.
func workflowYAML(w proto.Workflow) ([]byte, error) {
	b, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	setBlockStyle(&doc)
	return yaml.Marshal(&doc)
}

// setBlockStyle turns n and what it holds from JSON's flow style to
// YAML's block style, but for empty lists and mappings.
func setBlockStyle(n *yaml.Node) {
	if len(n.Content) > 0 {
		n.Style &^= yaml.FlowStyle
	}
	if n.Kind == yaml.ScalarNode {
		n.Style &^= yaml.DoubleQuotedStyle
	}
	for _, c := range n.Content {
		setBlockStyle(c)
	}
}

// diffWorkflow prints how the workflow in file differs from the
// server's. As check, a difference exits 1, for a CI job; as diff, it
// is what a set would change, and exits 0.
func diffWorkflow(ctx context.Context, r *runner, file string, check bool) error {
	_, w, err := r.readWorkflow(file)
	if err != nil {
		return err
	}
	var cur proto.WorkflowResult
	if err := r.call(ctx, proto.OpWorkflow, proto.WorkflowArgs{}, &cur); err != nil {
		return err
	}
	changes := workflowDiff(cur.Workflow, &w)
	if r.json {
		r.emit(struct {
			Rev     int64    `json:"rev"`
			Differs bool     `json:"differs"`
			Changes []string `json:"changes"`
		}{cur.Rev, len(changes) > 0, append([]string{}, changes...)})
	} else {
		if len(changes) == 0 {
			_, _ = fmt.Fprintf(r.env.Stdout, "%s matches the server's workflow (rev %d)\n", esc(file), cur.Rev)
		} else {
			what := "the server's workflow (rev %d)"
			if cur.Workflow == nil {
				what = "the server, which has no workflow (rev %d)"
			}
			_, _ = fmt.Fprintf(r.env.Stdout, "%s differs from "+what+":\n", esc(file), cur.Rev)
			for _, c := range changes {
				_, _ = fmt.Fprintf(r.env.Stdout, "  %s\n", esc(c))
			}
			if !check {
				_, _ = fmt.Fprintf(r.env.Stdout, "apply it with `aries admin workflow set %s --rev %d`\n", esc(file), cur.Rev)
			}
		}
	}
	if check && len(changes) > 0 {
		if !r.json {
			_, _ = fmt.Fprintf(r.env.Stderr, "aries: the workflow on the server is not %s; a project admin applies it with `aries admin workflow set %s`\n", esc(file), esc(file))
		}
		return exitError{ExitFailure}
	}
	return nil
}

// workflowDiff lists what changes from was to to, one line each: a key
// added or removed with a + or a -, and in a list the lines added and
// removed, or that its order changed. A nil workflow has nothing.
func workflowDiff(was, to *proto.Workflow) []string {
	if was == nil {
		was = &proto.Workflow{}
	}
	if to == nil {
		to = &proto.Workflow{}
	}
	var out []string
	if was.Version != to.Version {
		out = append(out, fmt.Sprintf("version: %d -> %d", was.Version, to.Version))
	}
	out = append(out, listDiff("ci", was.CI, to.CI)...)
	names := map[string]bool{}
	for n := range was.Types {
		names[n] = true
	}
	for n := range to.Types {
		names[n] = true
	}
	order := append([]string{proto.WorkflowDefault}, proto.WorkflowTypes...)
	for _, n := range order {
		if !names[n] {
			continue
		}
		at := "types." + n
		a, inWas := was.Types[n]
		b, inTo := to.Types[n]
		switch {
		case !inWas:
			out = append(out, "+ "+at)
		case !inTo:
			out = append(out, "- "+at)
		}
		out = append(out, listDiff(at+".acceptance", itemLines(a.Acceptance), itemLines(b.Acceptance))...)
		out = append(out, listDiff(at+".checks", a.Checks, b.Checks)...)
		for _, g := range []struct {
			key  string
			a, b *proto.WorkflowApprovers
		}{{"review", a.Review, b.Review}, {"approve", a.Approve, b.Approve}} {
			key := at + "." + g.key
			switch {
			case g.a == nil && g.b == nil:
			case g.a == nil:
				out = append(out, "+ "+key)
				out = append(out, listDiff(key+".approvers", nil, g.b.Approvers)...)
			case g.b == nil:
				out = append(out, "- "+key)
			default:
				out = append(out, listDiff(key+".approvers", g.a.Approvers, g.b.Approvers)...)
			}
		}
		out = append(out, listDiff(at+".closeout", a.Closeout, b.Closeout)...)
	}
	return out
}

// itemLines is items as diff lines: the text, and the check it is bound
// to.
func itemLines(items []proto.WorkflowItem) []string {
	var out []string
	for _, it := range items {
		line := it.Text
		if it.Check != "" {
			line += " (check " + it.Check + ")"
		}
		out = append(out, line)
	}
	return out
}

// listDiff lists what changes from was to to at key.
func listDiff(key string, was, to []string) []string {
	var out []string
	for _, s := range was {
		if !slices.Contains(to, s) {
			out = append(out, fmt.Sprintf("%s: - %s", key, s))
		}
	}
	for _, s := range to {
		if !slices.Contains(was, s) {
			out = append(out, fmt.Sprintf("%s: + %s", key, s))
		}
	}
	if len(out) == 0 && !slices.Equal(was, to) {
		out = append(out, key+": reordered")
	}
	return out
}
