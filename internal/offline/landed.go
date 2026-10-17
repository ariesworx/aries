package offline

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// landedFile holds the commit `aries land` last reported (design §21.8),
// as text: a project's directory has one, whichever checkout or worktree
// ran it.
const landedFile = "landed"

// commitHash is a commit's hash as git prints it, SHA-1 or SHA-256.
var commitHash = lazyregexp.New(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// Landed returns the commit `aries land` last reported for this server
// and project, or "" when it never did, or the file holds no hash. A
// directory that is not the user's own is an error, as for every file
// kept here.
func (l *Local) Landed() (string, error) {
	if err := l.ready(false, ""); err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(l.dir, landedFile)) //nolint:gosec // aries's own file, in a directory checked to be the user's
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if sha := strings.TrimSpace(string(b)); commitHash.MatchString(sha) {
		return sha, nil
	}
	return "", nil
}

// SetLanded keeps sha, a commit hash, as the last commit `aries land`
// reported. It replaces the file whole, so a process that dies leaves the
// old hash or the new one.
func (l *Local) SetLanded(sha string) error {
	if !commitHash.MatchString(sha) {
		return fmt.Errorf("keep the last commit landed: %q is no commit hash", sha)
	}
	return l.replace(landedFile, []byte(sha+"\n"))
}
