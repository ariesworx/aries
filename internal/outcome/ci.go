package outcome

import (
	"slices"
	"time"
)

// CI is a record's ci field (docs/design/outcome-record.md §5): the
// change's attempts up to the merge, the merge queue's on their own, and
// each failure with its cause. Its shape is no host's.
type CI struct {
	Host          string    `json:"host"`
	Attempts      int       `json:"attempts"`
	FirstPass     bool      `json:"first_pass"`
	Failures      []Failure `json:"failures"`
	QueueAttempts int       `json:"queue_attempts"`
}

// Failure is one failed attempt. Run is the host's run as evidence,
// "ci:ID"; SHA the commit; Event "change" or "queue".
type Failure struct {
	Run     string    `json:"run"`
	Attempt int       `json:"attempt"`
	SHA     string    `json:"sha"`
	Event   string    `json:"event"`
	Jobs    []string  `json:"jobs,omitempty"`
	Cause   string    `json:"cause"`
	At      time.Time `json:"at"`
}

// Causes, decided by rule and never by reading a log.
const (
	CauseInfra   = "infra"
	CauseFlake   = "flake"
	CauseBase    = "base"
	CauseChange  = "change"
	CauseQueue   = "queue"
	CauseUnknown = "unknown"
)

// attempt is the runs of every workflow on one commit with one attempt
// number: it passes when all of them pass.
type attempt struct {
	sha         string
	n           int
	queue       bool
	conclusion  string
	start, end  time.Time
	failedRun   string
	failedJobs  []string
	commitIndex int // the commit's place in the change, -1 if not in it
}

// attempts groups runs that started by until into attempts, oldest
// first, leaving out those not finished and those cancelled by a newer
// push (a cancelled attempt followed by an attempt on another commit:
// the host's concurrency rule cancelled it, nothing failed).
func attempts(runs []Run, commits []Commit, until time.Time) []attempt {
	type key struct {
		queue bool
		sha   string
		n     int
	}
	index := map[string]int{}
	for i, c := range commits {
		index[c.SHA] = i
	}
	byKey := map[key]*attempt{}
	var all []*attempt
	for _, r := range runs {
		if r.Started.After(until) {
			continue
		}
		k := key{r.Queue, r.SHA, r.Attempt}
		a := byKey[k]
		if a == nil {
			ci, ok := index[r.SHA]
			if !ok {
				ci = -1
			}
			a = &attempt{sha: r.SHA, n: r.Attempt, queue: r.Queue, conclusion: Pass, start: r.Started, commitIndex: ci}
			byKey[k] = a
			all = append(all, a)
		}
		a.start = minTime(a.start, r.Started)
		a.end = maxTime(a.end, r.Finished)
		if rank(r.Conclusion) > rank(a.conclusion) {
			a.conclusion = r.Conclusion
		}
		if r.Conclusion != Pass && r.Conclusion != Pending && a.failedRun == "" {
			a.failedRun = r.ID
		}
		a.failedJobs = append(a.failedJobs, r.FailedJobs...)
	}
	slices.SortStableFunc(all, func(x, y *attempt) int { return x.start.Compare(y.start) })
	var out []attempt
	for i, a := range all {
		if a.conclusion == Pending {
			continue
		}
		if a.conclusion == Cancelled && !a.queue && slices.ContainsFunc(all[i+1:], func(b *attempt) bool { return !b.queue && b.sha != a.sha }) {
			continue
		}
		slices.Sort(a.failedJobs)
		a.failedJobs = slices.Compact(a.failedJobs)
		if len(a.failedJobs) == 0 {
			a.failedJobs = nil
		}
		out = append(out, *a)
	}
	return out
}

// rank orders conclusions: an attempt takes its worst run's.
func rank(c string) int {
	switch c {
	case Pass:
		return 0
	case Cancelled:
		return 1
	case Infra:
		return 2
	case Fail:
		return 3
	default: // Pending: an attempt with a run still going is not finished
		return 4
	}
}

// ciOf is the ci field for a change's runs and commits, up to until
// (the merge, or the change's close).
func ciOf(host string, runs []Run, commits []Commit, until time.Time) CI {
	all := attempts(runs, commits, until)
	var change, queue []attempt
	for _, a := range all {
		if a.queue {
			queue = append(queue, a)
		} else {
			change = append(change, a)
		}
	}
	ups := map[string]string{} // catch-up merge → its first parent
	for _, c := range catchUps(commits) {
		ups[c.SHA] = c.Parents[0]
	}
	out := CI{Host: host, Attempts: len(change), QueueAttempts: len(queue), Failures: []Failure{}}
	for _, a := range all {
		if a.conclusion == Pass {
			continue
		}
		list := change
		if a.queue {
			list = queue
		}
		f := Failure{Run: "ci:" + a.failedRun, Attempt: a.n, SHA: a.sha, Event: "change", Jobs: a.failedJobs, At: a.end}
		if a.queue {
			f.Event = "queue"
			f.Cause = queueCause(a, queue, change)
		} else {
			f.Cause = changeCause(a, list, ups)
		}
		out.Failures = append(out.Failures, f)
	}
	slices.SortStableFunc(out.Failures, func(x, y Failure) int { return x.At.Compare(y.At) })
	out.FirstPass = len(change) > 0 && change[0].conclusion == Pass && len(out.Failures) == 0
	return out
}

// later returns the attempts of list that started after a.
func later(a attempt, list []attempt) []attempt {
	i := slices.IndexFunc(list, func(b attempt) bool { return b.start.After(a.start) })
	if i < 0 {
		return nil
	}
	return list[i:]
}

// flaked says a later attempt on a's commit passed, with no attempt on
// another commit between.
func flaked(a attempt, list []attempt) bool {
	for _, b := range later(a, list) {
		if b.sha != a.sha {
			return false
		}
		if b.conclusion == Pass {
			return true
		}
	}
	return false
}

// changeCause applies §5's rules to a failure of the change's own:
// infra, flake, base, change, in that order, the first that holds.
func changeCause(a attempt, list []attempt, ups map[string]string) string {
	switch {
	case a.conclusion == Infra || a.conclusion == Cancelled:
		return CauseInfra
	case flaked(a, list):
		return CauseFlake
	}
	if parent, ok := ups[a.sha]; ok && slices.ContainsFunc(list, func(b attempt) bool {
		return b.sha == parent && b.conclusion == Pass && !b.start.After(a.start)
	}) {
		return CauseBase
	}
	if slices.ContainsFunc(later(a, list), func(b attempt) bool {
		return b.conclusion == Pass && b.sha != a.sha && newer(b, a)
	}) {
		return CauseChange
	}
	return CauseUnknown
}

// newer says b's commit comes after a's in the change; for commits the
// change does not list, the later attempt is taken as the newer commit.
func newer(b, a attempt) bool {
	if a.commitIndex >= 0 && b.commitIndex >= 0 {
		return b.commitIndex > a.commitIndex
	}
	return true
}

// queueCause applies the rules to a merge queue's failure: infra, flake
// (a rerun of the same queue commit passed), queue (the change's own
// last attempt before it passed).
func queueCause(a attempt, queue, change []attempt) string {
	switch {
	case a.conclusion == Infra || a.conclusion == Cancelled:
		return CauseInfra
	case flaked(a, queue):
		return CauseFlake
	}
	var last *attempt
	for i := range change {
		if !change[i].start.After(a.start) {
			last = &change[i]
		}
	}
	if last != nil && last.conclusion == Pass {
		return CauseQueue
	}
	return CauseUnknown
}

// catchUps returns the change's merges of another branch: commits with
// two parents whose second parent is not one of the change's commits.
// Under the workflow (merge origin/main, never rebase) that is the
// default branch (§6).
func catchUps(commits []Commit) []Commit {
	own := map[string]bool{}
	for _, c := range commits {
		own[c.SHA] = true
	}
	var out []Commit
	for _, c := range commits {
		if len(c.Parents) == 2 && !own[c.Parents[1]] {
			out = append(out, c)
		}
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
