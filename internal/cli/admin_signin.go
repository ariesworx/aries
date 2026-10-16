package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ariesworx/aries/internal/proto"
)

// `aries admin signin` (transport.md §6.7): a person's sign-ins at the
// server's providers, listed and ended. A server admin may do either for
// anyone; a person for their own.

const signInUsage = "admin signin list NAME | end ID"

func adminSignIn(ctx context.Context, r *runner, args []string) error {
	const usage = signInUsage
	fs := r.newFlags("admin signin")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin signin needs list or end")
	}
	switch pos[0] {
	case "list":
		if len(pos) != 2 {
			return usagef(usage, "signin list needs one principal, such as bob")
		}
		var out proto.SignInsResult
		if err := r.call(ctx, proto.OpSignIns, proto.SignInsArgs{Name: pos[1]}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printSignIns(r.env.Stdout, out)
		return nil
	case "end":
		if len(pos) != 2 {
			return usagef(usage, "signin end needs one sign-in's id, as `aries admin signin list NAME` prints it")
		}
		var out proto.SignInEndResult
		if err := r.call(ctx, proto.OpSignInEnd, proto.SignInEndArgs{ID: pos[1]}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		if out.Change == "" {
			_, _ = fmt.Fprintf(r.env.Stdout, "sign-in %s of %s had ended already\n", esc(out.ID), esc(out.Principal))
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "sign-in %s of %s ended; %d connections closed\n", esc(out.ID), esc(out.Principal), out.Connections)
		return nil
	}
	return usagef(usage, "unknown signin action %q", pos[0])
}

// printSignIns lists a principal's sign-ins, oldest first: never a
// token.
func printSignIns(w io.Writer, res proto.SignInsResult) {
	if len(res.SignIns) == 0 {
		_, _ = fmt.Fprintf(w, "no sign-ins of %s\n", esc(res.Principal))
		return
	}
	_, _ = fmt.Fprintf(w, "sign-ins of %s\n", esc(res.Principal))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tPROVIDER\tMACHINE\tSCOPES\tSTATE\tSIGNED IN\tENDS")
	for _, s := range res.SignIns {
		state := s.State
		if s.EndReason != "" {
			state += " (" + strings.ReplaceAll(s.EndReason, "_", " ") + ")"
		}
		ends := when(s.ExpiresAt)
		if s.EndedAt != nil {
			ends = when(*s.EndedAt)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", esc(s.ID), esc(s.Provider), esc(s.Machine), esc(strings.Join(s.Scopes, " ")),
			esc(state), when(s.CreatedAt), ends)
	}
	_ = tw.Flush()
}
