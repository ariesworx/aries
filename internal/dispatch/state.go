package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/proto"
)

// stateVersion is the state file's format. A file of another version is
// read as empty: the state is a cache of Aries and git (dispatcher.md
// §9), and runs it no longer knows lapse by themselves.
const stateVersion = 1

// State is what the dispatcher keeps between wakes and restarts: its
// runs, the CI failures in a row, what stopped it, and the last budget
// read. It holds ids, numbers and paths, never a brief, a key or an
// issue's text.
type State struct {
	V       int    `json:"v"`
	Project string `json:"project,omitempty"`
	// PID is the running dispatcher's, while one runs; Since when it
	// started.
	PID    int       `json:"pid,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	DryRun bool      `json:"dry_run,omitempty"`
	Runs   []*Run    `json:"runs"`
	// Failures counts the CI failures in a row on PRs its runs opened.
	Failures int `json:"ci_failures"`
	// Stopped is a stop that lasts until `run --clear`: auth.
	Stopped string `json:"stopped,omitempty"`
	// Hold is what held starting at the last wake, and HoldWhy why.
	Hold    string `json:"hold,omitempty"`
	HoldWhy string `json:"hold_why,omitempty"`
	Budget  Budget `json:"budget"`
	// Wake is the last wake's source, and WokeAt its time.
	Wake   string    `json:"wake,omitempty"`
	WokeAt time.Time `json:"woke_at,omitzero"`
}

// Budget is the last read of the day's Opus output against the cap.
type Budget struct {
	Day    string    `json:"day,omitempty"`
	Output int64     `json:"output"`
	Cap    int64     `json:"cap"`
	At     time.Time `json:"at,omitzero"`
}

// Run is one headless run (dispatcher.md §9).
type Run struct {
	Issue    string    `json:"issue"`
	Session  string    `json:"session"`
	Harness  string    `json:"harness"`
	Model    string    `json:"model"`
	Worktree string    `json:"worktree"`
	Branch   string    `json:"branch"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended,omitzero"`
	Outcome  string    `json:"outcome,omitempty"`
	Code     int       `json:"code,omitempty"`
	// PR is its pull request's URL, once one is found; CI the result of
	// its checks once they finished (ChecksPass or ChecksFail), counted
	// once.
	PR string `json:"pr,omitempty"`
	CI string `json:"ci,omitempty"`
	// Cleaned: its worktree was removed after its issue closed. Left: a
	// removal was refused and reported, and is tried again.
	Cleaned bool `json:"cleaned,omitempty"`
	Left    bool `json:"left,omitempty"`
	// Cleared: a person cleared the stops (`run --clear`) since it
	// failed, so its issue may be started again.
	Cleared bool `json:"cleared,omitempty"`
}

// Live reports whether the run has not ended.
func (r *Run) Live() bool { return r.Ended.IsZero() }

// Clear clears what waits on a person: the stop an auth failure left,
// the CI failures in a row, and the hold on issues a run failed on.
func (s *State) Clear() {
	s.Failures, s.Stopped = 0, ""
	for _, r := range s.Runs {
		if !r.Live() {
			r.Cleared = true
		}
	}
}

// LoadState reads the state file at path, or returns an empty state when
// there is none or it is of another version. It refuses a file others
// may read or write.
func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the dispatcher's own state file
	if errors.Is(err, fs.ErrNotExist) {
		return &State{V: stateVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := private(path); err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil || s.V != stateVersion {
		return &State{V: stateVersion}, nil //nolint:nilerr // a cache: an unreadable one starts empty
	}
	return &s, nil
}

// Write writes the state to path, 0600 in a 0700 directory, replacing it
// whole so a reader never sees half of it.
func (s *State) Write(path string) error {
	s.V = stateVersion
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(path, append(b, '\n'))
}

// writePrivate writes b to path through a file beside it, 0600, renamed
// into place.
func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // gone once renamed
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// private refuses a file whose mode lets others read or write it.
func private(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return proto.Errf(proto.CodeInvalid, fmt.Sprintf("run `chmod 600 %s`", path),
			fmt.Sprintf("%s may be read or written by others (mode %o)", path, fi.Mode().Perm()))
	}
	return nil
}

// Paths are the dispatcher's files for one project, in aries's data
// directory (dispatcher.md §9, §12).
type Paths struct {
	// Dir holds them all, 0700.
	Dir string
	// State is the state file; Log the decisions, one JSON line each;
	// Stop the stop request; Lock the file one dispatcher a project on
	// this machine holds locked; Runs the directory of each run's output.
	State, Log, Stop, Lock, Runs string
}

// PathsFor returns the paths under data, aries's data directory, for
// project, the project's id.
func PathsFor(data, project string) Paths {
	dir := filepath.Join(data, "dispatch", project)
	return Paths{Dir: dir, State: filepath.Join(dir, "state.json"), Log: filepath.Join(dir, "log.jsonl"),
		Stop: filepath.Join(dir, "stop"), Lock: filepath.Join(dir, "lock"), Runs: filepath.Join(dir, "runs")}
}

// ReadStop returns the stop asked for in the file at path, StopDrain or
// StopNow, or "" when none was.
func ReadStop(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // the dispatcher's own stop file
	if err != nil {
		return ""
	}
	switch s := strings.TrimSpace(string(b)); s {
	case StopDrain, StopNow:
		return s
	}
	return ""
}

// WriteStop asks the running dispatcher to stop, as mode says.
func WriteStop(path, mode string) error {
	return writePrivate(path, []byte(mode+"\n"))
}
