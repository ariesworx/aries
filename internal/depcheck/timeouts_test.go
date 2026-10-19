package depcheck

import (
	"regexp"
	"testing"
)

var (
	jobHeader  = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)
	jobTimeout = regexp.MustCompile(`^    timeout-minutes:\s*\S`)
	jobCalls   = regexp.MustCompile(`^    uses:\s*\S`)
	jobsKey    = regexp.MustCompile(`^jobs:\s*$`)
	topKey     = regexp.MustCompile(`^[A-Za-z_]`)
)

// jobsWithoutTimeout returns the jobs of a workflow that have no
// timeout-minutes. A job that calls a workflow cannot have one.
func jobsWithoutTimeout(workflow string) []string {
	var missing []string
	var cur string
	var has, calls, inJobs bool
	flush := func() {
		if cur != "" && !has && !calls {
			missing = append(missing, cur)
		}
		cur, has, calls = "", false, false
	}
	for _, line := range splitLines(workflow) {
		switch {
		case jobsKey.MatchString(line):
			inJobs = true
		case inJobs && topKey.MatchString(line):
			flush()
			inJobs = false
		case !inJobs:
		case jobHeader.MatchString(line):
			flush()
			cur = jobHeader.FindStringSubmatch(line)[1]
		case jobTimeout.MatchString(line):
			has = true
		case jobCalls.MatchString(line):
			calls = true
		}
	}
	flush()
	return missing
}

func splitLines(s string) []string { return regexp.MustCompile(`\r?\n`).Split(s, -1) }

// noTimeout are jobs that have none, with the reason: release.yml's
// signing steps are the maintainer's (AGENTS.md, rule 3), so an agent
// leaves them, and the maintainer decides whether the job gets one.
var noTimeout = map[string]string{
	".github/workflows/release.yml sign-and-publish": "signing is the maintainer's; a person adds the timeout",
}

// TestJobsHaveTimeouts holds every job of every workflow to a
// timeout-minutes, so that a hang ends there and not at GitHub's six
// hours (AGENTS.md, Commands).
func TestJobsHaveTimeouts(t *testing.T) {
	t.Parallel()
	for _, f := range workflows(t) {
		for _, job := range jobsWithoutTimeout(readFile(t, f)) {
			if _, ok := noTimeout[f+" "+job]; ok {
				continue
			}
			t.Errorf("%s: job %s has no timeout-minutes", f, job)
		}
	}
}

func TestJobsWithoutTimeout(t *testing.T) {
	t.Parallel()
	const wf = `name: x
on: push
jobs:
  a:
    runs-on: ubuntu-24.04
    timeout-minutes: 5
    steps:
      - run: echo
  b:
    runs-on: ubuntu-24.04
    steps:
      - run: echo
  gate:
    uses: ./.github/workflows/ci.yml
  c:
    timeout-minutes: 1
`
	got := jobsWithoutTimeout(wf)
	if len(got) != 1 || got[0] != "b" {
		t.Errorf("jobsWithoutTimeout = %v, want [b]", got)
	}
}
