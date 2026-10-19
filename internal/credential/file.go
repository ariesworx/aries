package credential

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ariesworx/aries/internal/client"
	"github.com/ariesworx/aries/internal/offline"
	"github.com/ariesworx/aries/internal/proto"
)

// fileStore keeps each secret as a file of its own, SERVICE.NAME, 0600,
// in the directory "credentials" of aries's data directory, 0700. It
// reads, replaces and removes only a file that is a regular file, the
// user's own and 0600, in directories that are the user's own and that
// no one else may read or write: a file someone else made or opened is
// for its owner to see to, and nothing here changes a mode.
type fileStore struct {
	service   string
	root, dir string
	// owned says the file is the user's; client.OwnedByUser, but for tests.
	owned func(fs.FileInfo) bool
}

func newFileStore(o Options) (Store, error) {
	root, err := offline.DataDir(o.GOOS, o.Getenv, o.Home)
	if err != nil {
		return nil, err
	}
	return &fileStore{service: o.Service, root: root, dir: filepath.Join(root, "credentials"), owned: client.OwnedByUser}, nil
}

func (f *fileStore) Kind() Kind   { return KindFile }
func (f *fileStore) Name() string { return "file in " + f.dir }

func (f *fileStore) path(name string) string { return filepath.Join(f.dir, f.service+"."+name) }

// refuse is a file or directory that is not the user's alone.
func (f *fileStore) refuse(path, why, fix string) error {
	return proto.Errf(proto.CodeUnavailable, fix,
		"aries reads a credential only from a file that is yours alone: "+path+" "+why)
}

// private refuses the data directory and its credentials directory where
// they are there and are not the user's alone. One not there passes.
func (f *fileStore) private() error {
	for _, dir := range []string{f.root, f.dir} {
		st, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return f.refuse(dir, "cannot be read: "+errors.Unwrap(err).Error(), "check that you can read it")
		case !st.IsDir():
			return f.refuse(dir, "is not a directory", "remove it, or set "+offline.EnvDataDir+" to a directory of your own")
		case !f.owned(st):
			return f.refuse(dir, "belongs to another user", "set "+offline.EnvDataDir+" to a directory of your own")
		case st.Mode().Perm()&0o077 != 0:
			return f.refuse(dir, fmt.Sprintf("can be read or written by other users (mode %04o)", st.Mode().Perm()),
				fmt.Sprintf("if it is yours, `chmod 700 %s`", dir))
		}
	}
	return nil
}

// check refuses a file that is there and is not a regular file, the
// user's own and 0600, and says whether it is there.
func (f *fileStore) check(path string, st fs.FileInfo) error {
	switch {
	case !st.Mode().IsRegular():
		return f.refuse(path, "is not a regular file", "remove it, and keep the secret again")
	case !f.owned(st):
		return f.refuse(path, "belongs to another user", "remove it, and keep the secret again")
	case st.Mode().Perm() != 0o600:
		return f.refuse(path, fmt.Sprintf("has mode %04o, not 0600", st.Mode().Perm()),
			fmt.Sprintf("if it is yours and no one else has read it, `chmod 600 %s`; otherwise revoke the secret at its provider", path))
	}
	return nil
}

// existing checks name's file and says whether it is there.
func (f *fileStore) existing(path string) (bool, error) {
	if err := f.private(); err != nil {
		return false, err
	}
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.check(path, st)
}

func (f *fileStore) Get(_ context.Context, name string) (string, bool, error) {
	if err := checkName(name); err != nil {
		return "", false, err
	}
	path := f.path(name)
	there, err := f.existing(path)
	if err != nil || !there {
		return "", false, err
	}
	h, err := os.Open(path) //nolint:gosec // a file checked to be the user's alone
	if err != nil {
		return "", false, err
	}
	defer func() { _ = h.Close() }()
	// What was opened is what was checked: not a file put in its place.
	st, err := h.Stat()
	if err != nil {
		return "", false, err
	}
	lst, err := os.Lstat(path)
	if err != nil || !os.SameFile(st, lst) {
		return "", false, f.refuse(path, "was replaced while it was read", "try again")
	}
	if err := f.check(path, st); err != nil {
		return "", false, err
	}
	b, err := io.ReadAll(io.LimitReader(h, maxSecret+1))
	if err != nil {
		return "", false, err
	}
	if checkSecret(string(b)) != nil {
		return "", false, proto.Errf(proto.CodeInvalid, "remove it, and keep the secret again",
			path+" holds something other than a secret aries kept")
	}
	return string(b), true, nil
}

func (f *fileStore) Set(_ context.Context, name, secret string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkSecret(secret); err != nil {
		return err
	}
	path := f.path(name)
	if _, err := f.existing(path); err != nil {
		return err
	}
	// The data directory's parents are the system's; these two are
	// aries's own.
	if err := os.MkdirAll(f.root, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(f.dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if err := f.private(); err != nil { // what was found there, made by someone else meanwhile
		return err
	}
	t, err := os.CreateTemp(f.dir, "."+f.service+"."+name+"-*.tmp") // mode 0600
	if err != nil {
		return err
	}
	_, err = io.WriteString(t, secret)
	if err == nil {
		err = t.Sync()
	}
	if err = errors.Join(err, t.Close()); err == nil {
		err = os.Rename(t.Name(), path)
	}
	if err != nil {
		_ = os.Remove(t.Name())
	}
	return err
}

func (f *fileStore) Remove(_ context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	path := f.path(name)
	if there, err := f.existing(path); err != nil || !there {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
