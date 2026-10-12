package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
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
	printStatus(r.env.Stdout, out)
	return nil
}

// printStatus renders a status report: the server's lines, when the
// caller was shown them, then a row for each project. A figure the
// server left out, as it does for a project whose store is closed, is a
// dash.
func printStatus(w io.Writer, res proto.StatusResult) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	if s := res.Server; s != nil {
		p("starfixd %s, protocol %d to %d, up %s\n", esc(s.Version), s.ProtoMin, s.ProtoMax, uptime(s.UptimeSeconds))
		p("stores %d of %d, connections %d of %d, key lookups %d of %d\n", s.OpenStores, s.OpenStoresMax, s.Conns, s.ConnsMax, s.KeyLookups, s.KeyLookupsMax)
		p("starfixd's memory %d MiB in use, %d MiB from the system; %d goroutines\n", mib(s.HeapBytes), mib(s.SysBytes), s.Goroutines)
		p("projects: %s; principals: %s\n\n", byState(s.Projects), byState(s.Principals))
	}
	if len(res.Projects) == 0 {
		p("no projects\n")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tSTATE\tSTORE\tCONNS\tBY FILE\tSESSIONS\tWRITES/MIN\tP50\tP99\tLAST USED")
	byFile := false
	for _, pr := range res.Projects {
		store, file, sessions, writes, p50, p99, used := "closed", "-", "-", "-", "-", "-", "-"
		if pr.Open {
			store, file = "open", strconv.Itoa(pr.FileConns)
		}
		byFile = byFile || pr.FileConns > 0
		if pr.Sessions != nil {
			sessions = strconv.Itoa(*pr.Sessions)
		}
		if ws := pr.Writes; ws != nil {
			writes = strconv.Itoa(ws.LastMinute)
			if ws.Samples > 0 {
				p50, p99 = latency(ws.P50Micros), latency(ws.P99Micros)
			}
		}
		if pr.LastUsed != nil {
			used = when(*pr.LastUsed)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", esc(pr.Name), esc(pr.State), store, pr.Conns, file, sessions, writes, p50, p99, used)
	}
	_ = tw.Flush()
	more(w, res.More, res.Projects[len(res.Projects)-1].Name)
	if byFile {
		p("BY FILE: connections sshd let in by the server's authorized_keys file, not by a key from the store; `sfx admin key remove` does not close them\n")
	}
}

// mib is n bytes in mebibytes, to the nearest one.
func mib(n uint64) uint64 { return (n + 1<<19) >> 20 }

// latency prints a latency given in microseconds: to a tenth of a
// millisecond below a second, and to a hundredth of a second from there.
func latency(micros int64) string {
	d := time.Duration(micros) * time.Microsecond
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	}
	return d.String()
}

// uptime prints a number of seconds in its two largest units, such as
// "3d 4h"; a unit that is zero is left out.
func uptime(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	units := []struct {
		name string
		secs int64
	}{{"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}}
	var parts []string
	for i, u := range units {
		n := secs / u.secs
		if n == 0 {
			continue
		}
		parts = append(parts, strconv.FormatInt(n, 10)+u.name)
		if i+1 < len(units) {
			if rest := secs % u.secs / units[i+1].secs; rest > 0 {
				parts = append(parts, strconv.FormatInt(rest, 10)+units[i+1].name)
			}
		}
		break
	}
	return strings.Join(parts, " ")
}

// byState prints counts by state, such as "2 active, 1 archived", in
// the order of the states' names.
func byState(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	var parts []string
	for _, state := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%d %s", counts[state], esc(state)))
	}
	return strings.Join(parts, ", ")
}

// printFileConns says, under a principal's keys, how many of its open
// connections came in by the server's authorized_keys file.
func printFileConns(w io.Writer, principal string, n int) {
	switch {
	case n == 1:
		_, _ = fmt.Fprintf(w, "1 open connection of %s came in by the server's authorized_keys file, not by a key listed here; removing a key does not close it\n", esc(principal))
	case n > 1:
		_, _ = fmt.Fprintf(w, "%d open connections of %s came in by the server's authorized_keys file, not by a key listed here; removing a key does not close them\n", n, esc(principal))
	}
}
