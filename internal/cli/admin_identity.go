package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/ariesworx/aries/internal/proto"
)

// `aries admin invite` and `aries admin identity`: the identities people
// sign in with, and the invitations to them (docs/design/identity.md
// §6.5). A server admin invites an address at a provider for a person;
// the person's first sign-in binds it (slice T2). The server decides
// who may.

const (
	inviteUsage   = "admin invite add NAME ADDRESS --provider P | list [--pending] [-n N] [--after ID] | remove ID"
	identityUsage = "admin identity list NAME | remove ID"
)

func adminInvite(ctx context.Context, r *runner, args []string) error {
	const usage = inviteUsage
	fs := r.newFlags("admin invite")
	provider := fs.String("provider", "", "the provider the address signs in at, as the server's login: settings name it")
	pending := fs.Bool("pending", false, "list only the invitations that have not lapsed")
	list := listFlags(fs)
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin invite needs add, list or remove")
	}
	switch pos[0] {
	case "add":
		if err := only(fs, usage, "invite add", "provider"); err != nil {
			return err
		}
		switch {
		case len(pos) != 3:
			return usagef(usage, "invite add needs a principal and an address, such as bob bob@example.com")
		case *provider == "":
			return usagef(usage, "invite add needs --provider, the provider the address signs in at")
		}
		var out proto.IdentityResult
		if err := r.call(ctx, proto.OpInviteAdd, proto.InviteAddArgs{Name: pos[1], Address: pos[2], Provider: *provider}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printInvited(r.env.Stdout, out)
		return nil
	case "list":
		if err := only(fs, usage, "invite list", "pending", "n", "after"); err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef(usage, "invite list takes no arguments")
		}
		var out proto.InvitesResult
		if err := r.call(ctx, proto.OpInvites, proto.InvitesArgs{Pending: *pending, AdminListArgs: *list}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printInvites(r.env.Stdout, out)
		return nil
	case "remove":
		if err := only(fs, usage, "invite remove"); err != nil {
			return err
		}
		id, ok := identityID(pos)
		if !ok {
			return usagef(usage, "invite remove needs one invitation's id, as `aries admin invite list` prints it")
		}
		var out proto.IdentityResult
		if err := r.call(ctx, proto.OpInviteRemove, proto.IdentityArgs{ID: id}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printIdentityRemoved(r.env.Stdout, "invitation", out)
		return nil
	}
	return usagef(usage, "unknown invite action %q", pos[0])
}

func adminIdentity(ctx context.Context, r *runner, args []string) error {
	const usage = identityUsage
	fs := r.newFlags("admin identity")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin identity needs list or remove")
	}
	switch pos[0] {
	case "list":
		if len(pos) != 2 {
			return usagef(usage, "identity list needs one principal, such as bob")
		}
		var out proto.IdentitiesResult
		if err := r.call(ctx, proto.OpIdentities, proto.IdentitiesArgs{Name: pos[1]}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printIdentities(r.env.Stdout, out)
		return nil
	case "remove":
		id, ok := identityID(pos)
		if !ok {
			return usagef(usage, "identity remove needs one identity's id, as `aries admin identity list NAME` prints it")
		}
		var out proto.IdentityResult
		if err := r.call(ctx, proto.OpIdentityRemove, proto.IdentityArgs{ID: id}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printIdentityRemoved(r.env.Stdout, "identity", out)
		return nil
	}
	return usagef(usage, "unknown identity action %q", pos[0])
}

// identityID is the one id an action's arguments give after the action:
// a positive number.
func identityID(pos []string) (int64, bool) {
	if len(pos) != 2 {
		return 0, false
	}
	id, err := strconv.ParseInt(pos[1], 10, 64)
	return id, err == nil && id > 0
}

// expires is when an invitation lapses, or "-" for a bound identity.
func expires(i proto.Identity) string {
	if i.ExpiresAt == nil {
		return "-"
	}
	return when(*i.ExpiresAt)
}

// printInvited says what invite add did.
func printInvited(w io.Writer, res proto.IdentityResult) {
	how := ""
	if res.Change == "renewed" {
		how = " renewed"
	}
	_, _ = fmt.Fprintf(w, "invitation %d for %s%s: %s at %s, open until %s\n", res.ID, esc(res.Principal), how, esc(res.Address),
		esc(res.Provider), expires(res.Identity))
}

// printIdentityRemoved says which invitation or identity, what, went.
func printIdentityRemoved(w io.Writer, what string, res proto.IdentityResult) {
	_, _ = fmt.Fprintf(w, "%s %d removed: %s at %s, for %s\n", what, res.ID, esc(res.Address), esc(res.Provider), esc(res.Principal))
}

// printInvites lists invitations by id.
func printInvites(w io.Writer, res proto.InvitesResult) {
	if len(res.Invites) == 0 {
		_, _ = fmt.Fprintln(w, "no invitations")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tPRINCIPAL\tPROVIDER\tADDRESS\tSTATE\tEXPIRES\tINVITED BY")
	for _, i := range res.Invites {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", i.ID, esc(i.Principal), esc(i.Provider), esc(i.Address), esc(i.State),
			expires(i), esc(i.InvitedBy))
	}
	_ = tw.Flush()
	more(w, res.More, strconv.FormatInt(res.Invites[len(res.Invites)-1].ID, 10))
}

// printIdentities lists a principal's identities.
func printIdentities(w io.Writer, res proto.IdentitiesResult) {
	if len(res.Identities) == 0 {
		_, _ = fmt.Fprintf(w, "no identities of %s\n", esc(res.Principal))
		return
	}
	_, _ = fmt.Fprintf(w, "identities of %s\n", esc(res.Principal))
	identityTable(w, res.Identities)
}

// identityTable is identities as a table. SINCE is when an invitation
// was made, or when a sign-in bound the identity.
func identityTable(w io.Writer, ids []proto.Identity) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tPROVIDER\tADDRESS\tSTATE\tSINCE\tEXPIRES\tINVITED BY")
	for _, i := range ids {
		since := i.InvitedAt
		if i.BoundAt != nil {
			since = *i.BoundAt
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", i.ID, esc(i.Provider), esc(i.Address), esc(i.State), when(since), expires(i),
			esc(i.InvitedBy))
	}
	_ = tw.Flush()
}

// describe is what a principal is: "a person" or "a service owned by
// NAME", and its display name or that it has none.
func describe(kind, owner, display string) string {
	what := "a " + esc(kind)
	if owner != "" {
		what += " owned by " + esc(owner)
	}
	if display == "" {
		return what + ", with no display name"
	}
	return what + `, shown as "` + esc(display) + `"`
}

// printPrincipalSet says what principal set left.
func printPrincipalSet(w io.Writer, p proto.PrincipalResult) {
	how := "is now"
	if p.Change == "unchanged" {
		how = "is already"
	}
	_, _ = fmt.Fprintf(w, "%s %s %s\n", esc(p.Name), how, describe(p.Kind, p.Owner, p.DisplayName))
}

// printPrincipalShow shows a principal, its identities and its keys.
func printPrincipalShow(w io.Writer, res proto.PrincipalShowResult) {
	p := res.Principal
	_, _ = fmt.Fprintf(w, "%s: %s, %s; added by %s %s\n", esc(p.Name), esc(p.State), describe(p.Kind, p.Owner, p.DisplayName),
		esc(p.CreatedBy), when(p.CreatedAt))
	if len(res.Identities) == 0 {
		_, _ = fmt.Fprintln(w, "no identities")
	} else {
		identityTable(w, res.Identities)
	}
	if len(res.Keys) == 0 {
		_, _ = fmt.Fprintln(w, "no keys")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FINGERPRINT\tTYPE\tCOMMENT\tADDED BY\tADDED")
	for _, k := range res.Keys {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", esc(k.Fingerprint), esc(k.Type), esc(k.Comment), esc(k.AddedBy), when(k.AddedAt))
	}
	_ = tw.Flush()
}
