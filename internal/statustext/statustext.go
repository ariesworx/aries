// Package statustext prints the daemon's status report
// (proto.StatusResult) as text: what `sfx admin status` shows an admin
// over SSH and `starfixd status` shows the operator on the server, so
// the two cannot drift apart. Everything from the server goes through
// safetext, so a name cannot start a line of its own.
package statustext

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
)

// esc makes a string from the server safe to print on one line.
func esc(s string) string { return safetext.Line(s) }

// when prints a time to the minute, in UTC.
func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// Print renders a status report on w: the server's lines, when the
// caller was shown them, then a row for each project. A figure the
// server left out, as it does for a project whose store is closed, is a
// dash.
func Print(w io.Writer, res proto.StatusResult) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	if s := res.Server; s != nil {
		p("starfixd %s, protocol %d to %d, up %s\n", esc(s.Version), s.ProtoMin, s.ProtoMax, uptime(s.UptimeSeconds))
		p("stores %d of %d, connections %d of %d\n", s.OpenStores, s.OpenStoresMax, s.Conns, s.ConnsMax)
		if k := s.KeysFile; k != nil {
			p("keys file %s: %s\n", esc(k.Path), keysFile(*k))
			if k.HeldBack > 0 {
				p("keys held back from it: %d, which the server's authorized_keys file gives to another principal; sshd takes that file's line for them, and starfixd's log names each\n", k.HeldBack)
			}
		}
		p("starfixd's memory %d MiB in use, %d MiB from the system; %d goroutines\n", mib(s.HeapBytes), mib(s.SysBytes), s.Goroutines)
		p("projects: %s; principals: %s\n\n", byState(s.Projects), byState(s.Principals))
	}
	if len(res.Projects) == 0 {
		p("no projects\n")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tSTATE\tSTORE\tCONNS\tBY HAND\tSESSIONS\tWRITES/MIN\tP50\tP99\tLAST USED")
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
	if res.More > 0 {
		p("and %d more; list them with --after %s\n", res.More, esc(res.Projects[len(res.Projects)-1].Name))
	}
	if byFile {
		p("BY HAND: connections sshd let in by the server's hand-maintained authorized_keys file, not by a key from the store; `sfx admin key remove` does not close them\n")
	}
}

// keysFile says what the server last did with the file of keys it
// writes for sshd: how many keys it holds and when it was written, or
// that the latest write failed and sshd may be reading an older list.
func keysFile(k proto.KeysFile) string {
	keys := fmt.Sprintf("%d keys", k.Keys)
	if k.Keys == 1 {
		keys = "1 key"
	}
	switch {
	case k.Failed && k.WrittenAt != nil:
		return fmt.Sprintf("THE LAST WRITE FAILED, so sshd may be reading an older list (%s, written %s); see starfixd's log", keys, when(*k.WrittenAt))
	case k.Failed:
		return "THE LAST WRITE FAILED, so sshd may be reading an older list; see starfixd's log"
	case k.WrittenAt == nil:
		return keys
	}
	return fmt.Sprintf("%s, written %s", keys, when(*k.WrittenAt))
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
