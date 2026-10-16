package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/ariesworx/aries/internal/proto"
)

// `aries admin trust` (docs/design/identity.md §7, slice N5): the trust
// rules by which a service principal signs in with its platform's
// token, `aries login --workload`. A server admin adds, changes and
// removes them, never for their own principal; a service's owner lists
// and shows its rules.

const trustUsage = "admin trust add NAME --principal SERVICE --provider P --claim NAME=VALUE... [--token-lifetime D]" +
	" | list [SERVICE] | show NAME | set NAME [--claim NAME=VALUE...] [--token-lifetime D] | disable NAME | enable NAME | remove NAME"

func adminTrust(ctx context.Context, r *runner, args []string) error {
	const usage = trustUsage
	fs := r.newFlags("admin trust")
	principal := fs.String("principal", "", "the service principal the rule signs in as")
	provider := fs.String("provider", "", "the workload provider of the server's settings whose tokens it takes")
	var claims repeated
	fs.Var(&claims, "claim", "a claim the platform's token must carry with exactly this value, NAME=VALUE; repeat for each")
	lifetime := fs.String("token-lifetime", "", "how long a sign-in by the rule lasts, such as 1h (default: the server's workload: token_lifetime)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin trust needs add, list, show, set, disable, enable or remove")
	}
	action := pos[0]
	one := func() (string, error) {
		if len(pos) != 2 {
			return "", usagef(usage, "trust %s needs one rule's name, as `aries admin trust list` prints it", action)
		}
		return pos[1], nil
	}
	parsed := func() (map[string]string, error) {
		if len(claims) == 0 {
			return nil, nil
		}
		out := map[string]string{}
		for _, c := range claims {
			k, v, ok := strings.Cut(c, "=")
			if !ok || k == "" || v == "" {
				return nil, usagef(usage, "--claim takes NAME=VALUE, such as repository_id=123456789, not %q", c)
			}
			if _, dup := out[k]; dup {
				return nil, usagef(usage, "--claim %s is given twice; a claim has one value", k)
			}
			out[k] = v
		}
		return out, nil
	}
	switch action {
	case "add":
		if err := only(fs, usage, "trust add", "principal", "provider", "claim", "token-lifetime"); err != nil {
			return err
		}
		name, err := one()
		if err != nil {
			return err
		}
		cs, err := parsed()
		switch {
		case err != nil:
			return err
		case *principal == "" || *provider == "" || len(cs) == 0:
			return usagef(usage, "trust add needs --principal, --provider and at least one --claim")
		}
		var out proto.TrustResult
		if err := r.call(ctx, proto.OpTrustAdd, proto.TrustAddArgs{Name: name, Principal: *principal, Provider: *provider, Claims: cs,
			TokenLifetime: *lifetime}, &out); err != nil {
			return err
		}
		return r.printTrust(out, "added")
	case "list":
		if err := only(fs, usage, "trust list"); err != nil {
			return err
		}
		if len(pos) > 2 {
			return usagef(usage, "trust list takes at most one service principal")
		}
		var in proto.TrustRulesArgs
		if len(pos) == 2 {
			in.Principal = pos[1]
		}
		var out proto.TrustRulesResult
		if err := r.call(ctx, proto.OpTrustRules, in, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printTrustRules(r.env.Stdout, out.Rules)
		return nil
	case "show":
		if err := only(fs, usage, "trust show"); err != nil {
			return err
		}
		name, err := one()
		if err != nil {
			return err
		}
		var out proto.TrustResult
		if err := r.call(ctx, proto.OpTrustShow, proto.TrustArgs{Name: name}, &out); err != nil {
			return err
		}
		return r.printTrust(out, "")
	case "set", "disable", "enable":
		allowed := []string{}
		if action == "set" {
			allowed = []string{"claim", "token-lifetime"}
		}
		if err := only(fs, usage, "trust "+action, allowed...); err != nil {
			return err
		}
		name, err := one()
		if err != nil {
			return err
		}
		in := proto.TrustSetArgs{Name: name, TokenLifetime: *lifetime}
		if in.Claims, err = parsed(); err != nil {
			return err
		}
		switch action {
		case "set":
			if in.Claims == nil && in.TokenLifetime == "" {
				return usagef(usage, "trust set needs --claim or --token-lifetime; --claim replaces every claim of the rule")
			}
		case "disable":
			in.State = proto.TrustDisabled
		case "enable":
			in.State = proto.TrustActive
		}
		var out proto.TrustResult
		if err := r.call(ctx, proto.OpTrustSet, in, &out); err != nil {
			return err
		}
		return r.printTrust(out, "changed")
	case "remove":
		if err := only(fs, usage, "trust remove"); err != nil {
			return err
		}
		name, err := one()
		if err != nil {
			return err
		}
		var out proto.TrustResult
		if err := r.call(ctx, proto.OpTrustRemove, proto.TrustArgs{Name: name}, &out); err != nil {
			return err
		}
		return r.printTrust(out, "removed")
	}
	return usagef(usage, "unknown trust action %q", action)
}

// claimList is claims as NAME=VALUE, by name.
func claimList(claims map[string]string) string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(claims)) {
		out = append(out, k+"="+claims[k])
	}
	return strings.Join(out, " ")
}

// printTrust reports a trust rule after a write of kind ("added",
// "changed", "removed"), or as trust show reads it ("").
func (r *runner) printTrust(out proto.TrustResult, kind string) error {
	if r.json {
		r.emit(out)
		return nil
	}
	w := r.env.Stdout
	ended := ""
	if out.Ended > 0 {
		ended = fmt.Sprintf("; %d sign-ins it gave ended, and their connections closed", out.Ended)
	}
	switch {
	case kind == "removed":
		_, _ = fmt.Fprintf(w, "trust rule %s removed%s\n", esc(out.Name), ended)
		return nil
	case kind == "changed" && out.Change == "":
		_, _ = fmt.Fprintf(w, "trust rule %s was already so\n", esc(out.Name))
		return nil
	case kind != "":
		_, _ = fmt.Fprintf(w, "trust rule %s %s%s\n", esc(out.Name), kind, ended)
	}
	_, _ = fmt.Fprintf(w, "%s signs in as %s by a token of %s carrying %s, for %s (%s, rev %d)\n", esc(out.Name), esc(out.Principal),
		esc(out.Provider), esc(claimList(out.Claims)), esc(out.TokenLifetime), esc(out.State), out.Rev)
	if kind == "" {
		_, _ = fmt.Fprintf(w, "added by %s %s, changed %s\n", esc(out.CreatedBy), when(out.CreatedAt), when(out.UpdatedAt))
		if len(out.Required) > 0 {
			_, _ = fmt.Fprintf(w, "a rule at %s must pin %s\n", esc(out.Provider), esc(strings.Join(out.Required, " and ")))
		}
	}
	return nil
}

// printTrustRules lists trust rules by name.
func printTrustRules(w io.Writer, rules []proto.TrustRule) {
	if len(rules) == 0 {
		_, _ = fmt.Fprintln(w, "no trust rules")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tPRINCIPAL\tPROVIDER\tSTATE\tLIFETIME\tCLAIMS")
	for _, t := range rules {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", esc(t.Name), esc(t.Principal), esc(t.Provider), esc(t.State), esc(t.TokenLifetime),
			esc(claimList(t.Claims)))
	}
	_ = tw.Flush()
}
