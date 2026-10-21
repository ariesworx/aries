package guard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvPolicy names a headless run's snapshot: the dispatcher sets it in
// the run's environment, which a tool call cannot change for the
// harness (docs/design/guard.md §8).
const EnvPolicy = "ARIES_GUARD_POLICY"

// snapshotMagic begins a snapshot's first line.
const snapshotMagic = "# aries guard snapshot 1 "

// Snapshot is the file a session's guard reads: a first line saying the
// session's mode and whether origin/main had a policy file, then the
// file's bytes as they were.
func Snapshot(m Mode, policy []byte, present bool) []byte {
	state := "none"
	if present {
		state = "present"
	} else {
		policy = nil
	}
	return append([]byte(snapshotMagic+"mode="+m.String()+" policy="+state+"\n"), policy...)
}

// ReadSnapshot reads a snapshot: the policy, or the built-in one when
// origin/main had none, and the session's mode.
func ReadSnapshot(b []byte) (*Policy, Mode, error) {
	line, rest, _ := bytes.Cut(b, []byte("\n"))
	fields, ok := strings.CutPrefix(string(line), snapshotMagic)
	if !ok {
		return nil, Desktop, errors.New("not a snapshot of this aries's: take it again at the session's start")
	}
	var m Mode
	var present bool
	for _, f := range strings.Fields(fields) {
		switch f {
		case "mode=desktop":
		case "mode=headless":
			m = Headless
		case "policy=present":
			present = true
		case "policy=none":
		default:
			return nil, Desktop, fmt.Errorf("a snapshot field this aries does not know: %q", f)
		}
	}
	if !present {
		return BuiltinPolicy(), m, nil
	}
	p, err := Load(rest)
	if err != nil {
		return nil, m, fmt.Errorf("%s: %w", File, err)
	}
	return p, m, nil
}

// SnapshotDir is where sessions' snapshots are kept, under aries's data
// directory, outside every worktree.
func SnapshotDir(dataDir string) string { return filepath.Join(dataDir, "guard") }

// SnapshotPath is the snapshot of the session with id session.
func SnapshotPath(dataDir, session string) (string, error) {
	if session == "" || len(session) > 128 || session[0] == '.' ||
		strings.Trim(session, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") != "" {
		return "", errors.New("the session id cannot name a snapshot")
	}
	return filepath.Join(SnapshotDir(dataDir), session+".yaml"), nil
}

// WriteSnapshot writes b to path, 0600 in a 0700 directory, whole or not
// at all.
func WriteSnapshot(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
