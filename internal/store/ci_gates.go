package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/secretscan"
)

// CI gates (design §20.3 and §20.4; decision 67; stage 6, slice G1). A
// gate of kind ci names a check (build, test, landed, …), the issue whose
// branch the check runs on (its subject), and the principals that report
// it: CI jobs, each a principal of its own that runs aries. ariesd polls
// nothing and listens for nothing; a job reports, and the report is a
// write like any other.
//
// Who reports: only a principal the gate names, from the session of a CI
// job (proto.CISession) or a person's terminal (proto.CLISession), never
// from an agent's. An admin it does not name waives it instead, as a
// human gate is waived. A pass closes the gate, as a human gate's pass
// does, and a pass of a closed gate changes nothing, so a rerun is
// harmless. A failure records its evidence, leaves the gate open, and
// tells the subject's holder, or its assignee; a failure of a passed gate,
// a later commit's, reopens it. A report of an older CI run than the
// gate's last changes nothing (Report.stale). A waived gate takes no
// failure until it is rearmed.
//
// Landed (enterprise review E8, "closed is not landed") is a check like
// any other: the job on the default branch after a merge reports it, and
// a closed issue whose landed gate is open is closed and not landed.

// CheckResult is what a CI report says of a check.
type CheckResult string

// Check results.
const (
	CheckPassed CheckResult = "passed"
	CheckFailed CheckResult = "failed"
)

// GateReport is the last report a ci gate took: the result, who made it
// and when, and the evidence, each part optional: the commit checked, a
// link to the run, a short reason, and the run's number, by which reports
// that arrive out of order are put back in it (see Report.Run).
type GateReport struct {
	Result CheckResult `json:"result"`
	By     string      `json:"by"`
	At     time.Time   `json:"at"`
	SHA    string      `json:"sha,omitempty"`
	URL    string      `json:"url,omitempty"`
	Reason string      `json:"reason,omitempty"`
	Run    int64       `json:"run,omitempty"`
}

// Report is a CI job's report of a check, the input to PassCheck and
// FailCheck. It names the gate, or the subject and the check, which find
// it; SHA, URL and Reason are the evidence.
//
// Run, when not 0, is the number of the CI run that made the report,
// which rises with each run of the workflow and stays the same when a run
// is retried: GitHub Actions' run_number, GitLab CI's pipeline IID. A
// report of a lower run than the gate's last report is older than it,
// and changes nothing (see stale).
type Report struct {
	Gate    IssueID
	Subject IssueID
	Check   string
	SHA     string
	URL     string
	Reason  string
	Run     int64
}

var (
	// checkPattern is a check's name.
	checkPattern = lazyregexp.New(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	// shaPattern is a commit's hash, whole or abbreviated, as git prints
	// it.
	shaPattern = lazyregexp.New(`^[0-9a-f]{7,64}$`)
)

// maxGateChecks bounds the checks_per_issue limit: show reads every check on
// an issue.
const maxGateChecks = 100

// maxGateEvidence bounds the gate_evidence_bytes limit: a report is kept in
// the gate's row, and its URL and reason go on an inbox line.
const maxGateEvidence = 4096

// normalizeCI checks what a ci gate names beyond a human gate.
func (n *NewGate) normalizeCI() error {
	if !checkPattern.MatchString(n.Check) {
		return fmt.Errorf("%w: a ci gate names its check: 1-64 lowercase letters, digits and ._-, such as build or landed", ErrInvalid)
	}
	if n.Subject == "" {
		return fmt.Errorf("%w: a ci gate names its subject, the issue whose branch the check runs on", ErrInvalid)
	}
	if err := n.Subject.Validate(); err != nil {
		return err
	}
	if n.Blocks == n.Subject {
		return fmt.Errorf("%w: a ci gate cannot hold back its own subject, whose work could then never start", ErrInvalid)
	}
	return nil
}

// checkNewCI refuses a ci gate whose subject does not exist, already has
// a gate for the check, or has as many ci gates as checks_per_issue allows.
func checkNewCI(ctx context.Context, w *wtx, in NewGate) error {
	if _, err := loadIssue(ctx, w.tx, in.Subject); err != nil {
		return err
	}
	same, err := loadGates(ctx, w.tx, `WHERE g.kind = 'ci' AND g.subject = $1 AND g.check_name = $2`, 1, string(in.Subject), in.Check)
	if err != nil {
		return err
	}
	if len(same) > 0 {
		return fmt.Errorf("%w: issue %s already has gate %s for check %s; hold another issue behind that gate with `aries dep add ISSUE %s`",
			ErrInvalid, in.Subject, same[0].ID, in.Check, same[0].ID)
	}
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM gates WHERE kind = 'ci' AND subject = $1`, string(in.Subject)).Scan(&n); err != nil {
		return fmt.Errorf("count checks on %s: %w", in.Subject, err)
	}
	if n >= w.lim.GateChecks {
		return fmt.Errorf("%w: issue %s has %d ci gates, its limit (checks_per_issue)", ErrInvalid, in.Subject, n)
	}
	return nil
}

// validate refuses, with ErrInvalid, a report that names no gate, or
// evidence that is malformed, longer than limit bytes in all, or looks
// like it holds a secret (rule 2).
func (r Report) validate(limit int) error {
	if r.Gate == "" && (r.Subject == "" || r.Check == "") {
		return fmt.Errorf("%w: a report names its gate, or the subject and the check", ErrInvalid)
	}
	for _, id := range []IssueID{r.Gate, r.Subject} {
		if id != "" {
			if err := id.Validate(); err != nil {
				return err
			}
		}
	}
	if r.Check != "" && !checkPattern.MatchString(r.Check) {
		return fmt.Errorf("%w: check %q is not a check's name: 1-64 lowercase letters, digits and ._-", ErrInvalid, r.Check)
	}
	if r.Run < 0 {
		return fmt.Errorf("%w: run must be the CI run's number, 1 or more, or 0 for none", ErrInvalid)
	}
	return r.checkEvidence(limit)
}

// checkEvidence is validate's part for the evidence.
func (r Report) checkEvidence(limit int) error {
	if r.SHA != "" && !shaPattern.MatchString(r.SHA) {
		return fmt.Errorf("%w: sha must be a commit's hash: 7 to 64 lowercase hex digits", ErrInvalid)
	}
	if err := checkLine("url", r.URL, maxGateEvidence, false); err != nil {
		return err
	}
	if r.URL != "" {
		u, err := url.Parse(r.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%w: url must be an http or https link to the run", ErrInvalid)
		}
		if u.User != nil {
			return fmt.Errorf("%w: url must not carry a user or a password", ErrInvalid)
		}
	}
	if err := checkLine("reason", r.Reason, maxReason, false); err != nil {
		return err
	}
	if n := len(r.SHA) + len(r.URL) + len(r.Reason); n > limit {
		return fmt.Errorf("%w: the evidence is %d bytes; a report holds at most %d (gate_evidence_bytes)", ErrInvalid, n, limit)
	}
	for _, f := range []struct{ name, v string }{{"url", r.URL}, {"reason", r.Reason}} {
		if found, ok := secretscan.Find(f.v); ok {
			return fmt.Errorf("%w: the report's %s looks like it holds a secret (%s); report a link and a reason without it", ErrInvalid, f.name, found.Kind)
		}
	}
	return nil
}

// PassCheck reports a ci gate's check passed: it closes the gate, which
// lets what it held become ready, tells the assignees of what it held
// (gate.passed), and, when the last report was a failure, the subject's
// holder or assignee. A gate already closed is returned as it is, so a
// rerun changes nothing.
//
// The gate is r.Gate, or the ci gate of r.Subject for r.Check, refused
// with a [*GateError] (no check) wrapping ErrNotFound when there is none.
// Only a principal the gate names may report, and only from the session
// of a CI job or a person's terminal: anyone else is refused with a
// [*GateError]. A human gate, and evidence out of bounds, are refused
// with ErrInvalid.
func (s *Store) PassCheck(ctx context.Context, actor Actor, r Report) (Gate, error) {
	return s.reportCheck(ctx, actor, r, CheckPassed)
}

// FailCheck reports a ci gate's check failed, with its evidence: the gate
// stays open, or is reopened if it had passed, and the subject's holder
// (in the holder's session), or else its assignee, is told
// (gate.failed). A waived gate is refused with ErrInvalid until it is
// rearmed. The gate and who may report are as for PassCheck.
func (s *Store) FailCheck(ctx context.Context, actor Actor, r Report) (Gate, error) {
	return s.reportCheck(ctx, actor, r, CheckFailed)
}

// reportCheck is PassCheck and FailCheck.
func (s *Store) reportCheck(ctx context.Context, actor Actor, r Report, result CheckResult) (Gate, error) {
	if err := r.validate(s.opts.Limits.GateEvidence); err != nil {
		return Gate{}, err
	}
	verb := "pass"
	if result == CheckFailed {
		verb = "fail"
	}
	var out Gate
	err := s.write(ctx, actor, func(w *wtx) error {
		g, err := findCheck(ctx, w.tx, r)
		if err != nil {
			return err
		}
		if g.Kind != GateCI {
			return fmt.Errorf("%w: gate %s is a human gate, which a person passes with `aries gate pass %s`; only a ci gate takes a check's report",
				ErrInvalid, g.ID, g.ID)
		}
		refuse := func(why GateReason) error {
			return &GateError{ID: g.ID, Gate: g.ID, Approvers: g.Approvers, Check: g.Check, Reason: why, Op: verb}
		}
		if !w.approves(g) {
			return refuse(GateNotApprover)
		}
		if why := w.sessionRefusal(g, GatePassed); why != "" {
			return refuse(why)
		}
		before, err := loadIssue(ctx, w.tx, g.ID)
		if err != nil {
			return err
		}
		out, err = w.report(ctx, before, g, r, result)
		return err
	})
	if err != nil {
		return Gate{}, err
	}
	return out, nil
}

// findCheck reads the gate r names: r.Gate, or r.Subject's ci gate for
// r.Check.
func findCheck(ctx context.Context, q querier, r Report) (*gateRow, error) {
	if r.Gate != "" {
		g, err := loadGate(ctx, q, r.Gate)
		if err != nil {
			return nil, err
		}
		switch {
		case g == nil:
			return nil, fmt.Errorf("%w: issue %s is not a gate", ErrInvalid, r.Gate)
		case r.Check != "" && r.Check != g.Check, r.Subject != "" && r.Subject != g.Subject:
			return nil, fmt.Errorf("%w: gate %s is for check %s on %s, not %s on %s; name the gate, or the issue and the check",
				ErrInvalid, g.ID, cmp.Or(g.Check, "none"), cmp.Or(g.Subject, "no issue"), cmp.Or(r.Check, g.Check), cmp.Or(r.Subject, g.Subject))
		}
		return g, nil
	}
	gs, err := loadGates(ctx, q, `WHERE g.kind = 'ci' AND g.subject = $1 AND g.check_name = $2`, 1, string(r.Subject), r.Check)
	if err != nil {
		return nil, err
	}
	if len(gs) == 0 {
		return nil, &GateError{ID: r.Subject, Check: r.Check, Reason: GateNoCheck, Op: "report"}
	}
	return &gs[0], nil
}

// report records r on ci gate g, whose issue is before, as result, once
// who and from where are allowed; see PassCheck and FailCheck.
func (w *wtx) report(ctx context.Context, before Issue, g *gateRow, r Report, result CheckResult) (Gate, error) {
	if err := r.checkEvidence(w.lim.GateEvidence); err != nil {
		return Gate{}, err
	}
	switch {
	case result == CheckPassed && !g.open:
		return g.Gate, nil
	case result == CheckFailed && g.Outcome == GateWaived:
		return Gate{}, fmt.Errorf("%w: gate %s was waived by %s; rearm it (`aries gate rearm %s`) for CI to report on it again",
			ErrInvalid, g.ID, g.By, g.ID)
	case r.stale(g.Report, result):
		return g.Gate, nil
	}
	rep := GateReport{Result: result, By: w.actor.Principal, At: w.now, SHA: r.SHA, URL: r.URL, Reason: r.Reason, Run: r.Run}
	b, err := json.Marshal(rep)
	if err != nil {
		return Gate{}, fmt.Errorf("gate %s report: %w", g.ID, err)
	}
	people, err := w.subjectPeople(ctx, g.Subject)
	if err != nil {
		return Gate{}, err
	}
	evidence := map[string]any{"check": g.Check, "subject": nullStr(g.Subject), "sha": r.SHA, "url": r.URL, "reason": r.Reason}
	if r.Run != 0 {
		evidence["run"] = r.Run
	}
	var told []InboxItem
	if result == CheckPassed {
		// Read before the close: once the gate is closed, the edges into
		// it hold nothing.
		if told, err = heldAssignees(ctx, w.tx, g); err != nil {
			return Gate{}, err
		}
		if _, err := w.exec(ctx, `UPDATE gates SET outcome = $1, decided_by = $2, decided_at = $3, reason = $4, report = $5, rev = rev + 1
  WHERE issue_id = $6`, string(GatePassed), w.actor.Principal, w.now, r.Reason, string(b), string(g.ID)); err != nil {
			return Gate{}, fmt.Errorf("pass gate %s: %w", g.ID, err)
		}
		evidence["outcome"] = GatePassed
		if err := w.event(ctx, OpGatePass, string(g.ID), map[string]any{"outcome": nil}, evidence); err != nil {
			return Gate{}, err
		}
		if _, err := setStatus(ctx, w, before, OpIssueClose, []any{string(StatusClosed), w.now, nullStr(r.Reason)}, nil); err != nil {
			return Gate{}, err
		}
		for i := range told {
			told[i].Body = fmt.Sprintf("gate %s passed by %s: %s", g.ID, w.actor.Principal, told[i].Body)
		}
		if g.Report == nil || g.Report.Result != CheckFailed {
			people = nil // nothing failed that they were told of
		}
		for _, p := range people {
			told = append(told, InboxItem{To: p.To, Session: p.Session, Kind: InboxGatePassed, Issue: g.Subject,
				Body: fmt.Sprintf("%s passed on %s%s, after a failure (gate %s)", g.Check, g.Subject, evidenceText(rep), g.ID)})
		}
	} else {
		if _, err := w.exec(ctx, `UPDATE gates SET outcome = NULL, decided_by = NULL, decided_at = NULL, reason = '', report = $1, rev = rev + 1
  WHERE issue_id = $2`, string(b), string(g.ID)); err != nil {
			return Gate{}, fmt.Errorf("fail gate %s: %w", g.ID, err)
		}
		evidence["outcome"] = nil
		if err := w.event(ctx, OpGateFail, string(g.ID), map[string]any{"outcome": nullStr(g.Outcome)}, evidence); err != nil {
			return Gate{}, err
		}
		if !g.open {
			if _, err := setStatus(ctx, w, before, OpIssueReopen, []any{string(StatusOpen), nil, nil}, nil); err != nil {
				return Gate{}, err
			}
		}
		for _, p := range people {
			told = append(told, InboxItem{To: p.To, Session: p.Session, Kind: InboxGateFailed, Issue: g.Subject,
				Body: fmt.Sprintf("%s failed on %s%s (gate %s)", g.Check, g.Subject, evidenceText(rep), g.ID)})
		}
	}
	for _, it := range told {
		if err := w.notify(ctx, it); err != nil {
			return Gate{}, err
		}
	}
	after, err := loadGate(ctx, w.tx, g.ID)
	if err != nil {
		return Gate{}, err
	}
	return after.Gate, nil
}

// stale reports whether r, reported as result, is older than last, the
// gate's last report, and so changes nothing: both name their run, and
// r's is lower. A slow run of an older commit then neither reopens a
// gate a newer run passed nor passes one a newer run failed. One
// exception fails closed: a failure of the commit last passed reopens
// the gate whatever its run, since that commit has now failed too.
//
// Reports that do not both name a run are taken in the order they
// arrive, as are those of one run, retried. A rearm forgets the last
// report's run (RearmGate), so the order starts again after it, as it
// must when a workflow's numbering does.
func (r Report) stale(last *GateReport, result CheckResult) bool {
	if last == nil || last.Run == 0 || r.Run == 0 || r.Run >= last.Run {
		return false
	}
	return result != CheckFailed || last.Result != CheckPassed || !sameCommit(r.SHA, last.SHA)
}

// sameCommit reports whether two commits' hashes, either abbreviated,
// name one commit.
func sameCommit(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// subjectPeople are whom a report on subject tells: the holder of its
// live claim, in the holder's session, and its assignee when that is
// someone else. None for no subject.
func (w *wtx) subjectPeople(ctx context.Context, subject IssueID) ([]InboxItem, error) {
	if subject == "" {
		return nil, nil
	}
	is, err := loadIssue(ctx, w.tx, subject)
	if err != nil {
		return nil, err
	}
	c, err := loadClaim(ctx, w.tx, subject)
	if err != nil {
		return nil, err
	}
	var out []InboxItem
	if c.active(w.now) {
		out = append(out, InboxItem{To: c.Holder.Principal, Session: c.Holder.Session})
	}
	if PrincipalPattern.MatchString(is.Assignee) && (len(out) == 0 || out[0].To != is.Assignee) {
		out = append(out, InboxItem{To: is.Assignee})
	}
	return out, nil
}

// evidenceText is a report's evidence on an inbox line: the commit,
// shortened, then the link, which a cut line keeps, then the reason.
func evidenceText(r GateReport) string {
	var b strings.Builder
	if r.SHA != "" {
		b.WriteString(" at " + r.SHA[:min(len(r.SHA), 12)])
	}
	if r.URL != "" || r.Reason != "" {
		b.WriteString(":")
	}
	for _, s := range []string{r.URL, r.Reason} {
		if s != "" {
			b.WriteString(" " + s)
		}
	}
	return b.String()
}

// ChecksOf returns the ci gates whose subject is issue id, open or
// closed, by check: how an issue shows its checks, and whether it landed.
func (s *Store) ChecksOf(ctx context.Context, id IssueID) ([]Gate, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	rows, err := loadGates(ctx, s.r, `WHERE g.kind = 'ci' AND g.subject = $1`, maxGateChecks, string(id))
	if err != nil {
		return nil, err
	}
	out := make([]Gate, len(rows))
	for i, g := range rows {
		out[i] = g.Gate
	}
	slices.SortFunc(out, func(a, b Gate) int { return strings.Compare(a.Check, b.Check) })
	return out, nil
}
