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
// dash. Sessions the server could not read are "unknown", with a line
// below the rows that says why.
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
		if c := s.Clock; c != nil {
			p("%s", clockLines(*c))
		}
		if tr := s.Traces; tr != nil {
			p("%s", tracesLine(*tr))
		}
		p("starfixd's memory %d MiB in use, %d MiB from the system; %d goroutines\n", mib(s.HeapBytes), mib(s.SysBytes), s.Goroutines)
		if d := s.Dolt; d != nil {
			p("%s", doltLines(*d))
		}
		p("projects: %s; principals: %s\n\n", byState(s.Projects), byState(s.Principals))
	}
	if len(res.Projects) == 0 {
		p("no projects\n")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PROJECT\tSTATE\tSTORE\tCONNS\tBY HAND\tSESSIONS\tWRITES/MIN\tP50\tP99\tLAST USED")
	byFile := false
	// Why sessions are unknown, in the order met, and for how many
	// projects each.
	var causes []string
	unknown := map[string]int64{}
	for _, pr := range res.Projects {
		store, file, sessions, writes, p50, p99, used := "closed", "-", "-", "-", "-", "-", "-"
		if pr.Open {
			store, file = "open", strconv.Itoa(pr.FileConns)
		}
		byFile = byFile || pr.FileConns > 0
		switch {
		case pr.Sessions != nil:
			sessions = strconv.Itoa(*pr.Sessions)
		case pr.SessionsUnknown != "":
			sessions = "unknown"
			if unknown[pr.SessionsUnknown] == 0 {
				causes = append(causes, pr.SessionsUnknown)
			}
			unknown[pr.SessionsUnknown]++
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
	for _, cause := range causes {
		p("sessions: unknown for %s (%s); the rest is what starfixd holds in memory. Check Dolt on the server (docs/server.md, Troubleshooting)\n", plural(unknown[cause], "project"), esc(cause))
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

// clockLines says what the server has seen of its clock, and nothing
// when it has seen no step and is not behind its history.
// tracesLine says how people appear in the server's traces: loudly when
// they are left out because the stand-in key is unavailable, which
// nobody chose.
func tracesLine(tr proto.TracesStatus) string {
	switch tr.People {
	case "pseudonym":
		return "traces: people are named by stand-ins (telemetry: principals: pseudonym)\n"
	case "name":
		return "traces: people are named by name (telemetry: principals: name)\n"
	case "omit":
		return "traces: nothing says who (telemetry: principals: omit)\n"
	case "unavailable":
		state := "is unavailable"
		if tr.Damaged {
			state = "CANNOT BE TRUSTED"
		}
		return fmt.Sprintf("TRACES SAY NOTHING ABOUT WHO: the key for stand-ins, %s, %s: %s. starfixd's log has the fix (`telemetry: spans say nothing about who`), and starfixd looks again every half minute\n",
			esc(tr.KeyFile), state, esc(tr.Cause))
	}
	return "traces: " + esc(tr.People) + "\n"
}

func clockLines(c proto.ClockStatus) string {
	var b strings.Builder
	if n := c.StepsForward + c.StepsBackward; n > 0 {
		last := ""
		if c.LastStepAt != nil {
			dir, by := "forward", c.LastStepSeconds
			if by < 0 {
				dir, by = "back", -by
			}
			last = fmt.Sprintf(", the last %s by %s at %s", dir, time.Duration(by*float64(time.Second)).Round(time.Second), when(*c.LastStepAt))
		}
		times := "times"
		if n == 1 {
			times = "time"
		}
		fmt.Fprintf(&b, "clock: set %d %s since starfixd started (%d forward, %d back)%s; see starfixd's log for `clock stepped`\n",
			n, times, c.StepsForward, c.StepsBackward, last)
	}
	if h := c.Behind; h != nil {
		restart := ""
		if h.RefusesStart {
			restart = " STARFIXD WILL NOT START AGAIN until the clock is corrected (clock_behind: stop)."
		}
		fmt.Fprintf(&b, "CLOCK BEHIND HISTORY: the server's clock is %s behind the latest event of %s (%s), since %s. starfixd's time runs on from there at half speed until the clock has caught up.%s On the server, check `timedatectl`\n",
			(time.Duration(h.Seconds) * time.Second).String(), esc(h.Log), when(h.History), when(h.Since), restart)
	}
	return b.String()
}

// What the collector's cycles for each 100 writes said of a Go memory
// limit in one set of measurements (spike/multidb/RESULTS.md, section
// 5: Linux on arm64 in a container, synthetic issues of one shape):
// every run whose limit cost at most twice the CPU made fewer than
// gcRoom, and every run past the cliff more than gcTooLow.
const (
	gcRoom   = 3
	gcTooLow = 14
	// gcMinWrites is how many writes a window needs before the cycles
	// are set against them. An idle Dolt still collects about every two
	// minutes, five times in a window of ten: against fewer writes than
	// this, those alone would be more than one for each 100. Cycles
	// past gcTooLow for each 100 of this many writes are judged whatever
	// was written: on the guide's virtual machine a Dolt of 168 MiB
	// under a limit of 120 MiB made 500 cycles in 15 seconds while
	// nothing wrote to it.
	gcMinWrites = 500
)

// doltLines says what starfixd last read of Dolt's own metrics: one
// line of Dolt's figures, with "unknown" for each it did not give, and
// one of its collector's cycles against starfixd's writes, once two
// reads have succeeded. When the latest read failed it is one line
// saying so, with the last read that succeeded.
func doltLines(d proto.DoltStatus) string {
	if d.Unknown {
		if d.ReadAt == nil {
			return "Dolt: unknown: starfixd has not read Dolt's metrics since it started; see starfixd's log\n"
		}
		return fmt.Sprintf("Dolt: unknown: starfixd could not read Dolt's metrics, and last could at %s; see starfixd's log\n", when(*d.ReadAt))
	}
	parts := []string{"Dolt, version unknown", "uptime unknown", "connections unknown", "memory unknown", "memory limit unknown"}
	if d.Version != "" {
		parts[0] = "Dolt " + esc(d.Version)
	}
	if d.UptimeSeconds != nil {
		parts[1] = "up " + uptime(*d.UptimeSeconds)
	}
	if d.Conns != nil {
		parts[2] = plural(int64(*d.Conns), "connection")
	}
	if d.ResidentBytes != nil {
		parts[3] = fmt.Sprintf("%d MiB resident", mib(*d.ResidentBytes))
	}
	limited := false
	if l := d.MemLimitBytes; l != nil {
		parts[4] = "no memory limit"
		if limited = *l > 0; limited {
			parts[4] = fmt.Sprintf("memory limit %d MiB", mib(*l))
		}
	}
	out := strings.Join(parts, ", ") + "\n"
	g := d.GC
	if g == nil {
		return out
	}
	out += fmt.Sprintf("Dolt's collector: %s for %s in %s", plural(g.Cycles, "cycle"), plural(g.Writes, "write"), uptime(g.WindowSeconds))
	const tooLow = "AS WITH A MEMORY LIMIT TOO LOW in the measurements: Dolt is spending its CPU collecting. Raise GOMEMLIMIT or restart Dolt (docs/server.md, Dolt's memory)"
	switch {
	case g.Writes < gcMinWrites || g.Per100Writes == nil:
		switch {
		case !limited:
		// A limit far too low has Dolt collecting without pause even
		// when nothing is written: past what gcMinWrites writes would
		// excuse, the writes are not needed to say so.
		case g.Cycles*100 > gcTooLow*gcMinWrites:
			out += fmt.Sprintf(": over %d for each 100 even counted against %d writes, %s", gcTooLow, gcMinWrites, tooLow)
		default:
			out += fmt.Sprintf(": too few writes to judge the memory limit by (it takes %d)", gcMinWrites)
		}
		return out + "\n"
	case *g.Per100Writes < 10:
		out += fmt.Sprintf(", %.1f for each 100 writes", *g.Per100Writes)
	default:
		out += fmt.Sprintf(", %.0f for each 100 writes", *g.Per100Writes)
	}
	switch r := *g.Per100Writes; {
	case !limited:
	case r < gcRoom:
		out += fmt.Sprintf(": under %d, as with a memory limit that had room in the measurements (over %d was one too low)", gcRoom, gcTooLow)
	case r > gcTooLow:
		out += fmt.Sprintf(": OVER %d, %s", gcTooLow, tooLow)
	default:
		out += fmt.Sprintf(": between the %d of a memory limit with room and the %d of one too low, in the measurements", gcRoom, gcTooLow)
	}
	return out + "\n"
}

// plural prints n and noun, with an s unless n is 1.
func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.FormatInt(n, 10) + " " + noun + "s"
}
