package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/statustext"
)

// `sfx admin status`: what the daemon knows of itself and of its
// projects now (design §15.4). A server admin sees the server and every
// project; a project admin the projects they administer. It reads, and
// changes nothing.

func adminStatus(ctx context.Context, r *runner, args []string) error {
	const usage = "admin status [-n N] [--after NAME]"
	fs := r.newFlags("admin status")
	list := listFlags(fs)
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef(usage, "admin status takes no arguments")
	}
	var out proto.StatusResult
	if err := r.call(ctx, proto.OpStatus, proto.StatusArgs{AdminListArgs: *list}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	statustext.Print(r.env.Stdout, out)
	return nil
}

// printFileConns says, under a principal's keys, how many of its open
// connections came in by the server's hand-maintained authorized_keys
// file.
func printFileConns(w io.Writer, principal string, n int) {
	switch {
	case n == 1:
		_, _ = fmt.Fprintf(w, "1 open connection of %s came in by a line of the server's hand-maintained authorized_keys file; removing a key from the store does not close it\n", esc(principal))
	case n > 1:
		_, _ = fmt.Fprintf(w, "%d open connections of %s came in by a line of the server's hand-maintained authorized_keys file; removing a key from the store does not close them\n", n, esc(principal))
	}
}
