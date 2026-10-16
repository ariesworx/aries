package client

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ariesworx/aries/internal/dpop"
	"github.com/ariesworx/aries/internal/proto"
)

// A sign-in kept on this machine (transport.md §6.6): the token the
// server issued at `aries login`, and the DPoP key it is bound to, in
// the user's data directory, under signin/SERVER/, where SERVER is the
// hex of SHA-256 of the server's URL. Both files are 0600 in a 0700
// directory that must be the user's alone. Every `aries` and `aries mcp`
// of the user that reaches that server uses it, whatever the project.

// The files of a sign-in.
const (
	signInTokenFile = "token.json"
	signInKeyFile   = "dpop.key"
)

// signInVersion is the version of token.json's format.
const signInVersion = 1

// SignIn is a sign-in kept on this machine for one server.
type SignIn struct {
	// Server is the server's URL, as .aries.yaml names it.
	Server string `json:"server"`
	// Token is the access token. It is presented with a proof by Key, and
	// is never printed.
	Token string `json:"token"`
	// ExpiresAt is when the sign-in ends.
	ExpiresAt time.Time `json:"expires_at"`
	Scopes    []string  `json:"scopes"`
	// Principal and Provider are who the server took the sign-in for,
	// and at which provider; Grant is the sign-in's id, which an admin's
	// `aries admin signin` names.
	Principal string `json:"principal"`
	Provider  string `json:"provider"`
	Grant     string `json:"grant"`
	// Key is the DPoP key the token is bound to.
	Key *ecdsa.PrivateKey `json:"-"`
}

type signInFile struct {
	Version int `json:"version"`
	SignIn
}

// Live reports whether the sign-in has not ended at now.
func (s *SignIn) Live(now time.Time) bool { return now.Before(s.ExpiresAt) }

// SignInDir is the directory a sign-in for serverURL is kept in under
// dataDir.
func SignInDir(dataDir, serverURL string) string {
	sum := sha256.Sum256([]byte(serverURL))
	return filepath.Join(dataDir, "signin", hex.EncodeToString(sum[:]))
}

// signInRefused is the refusal of a sign-in's files that are not the
// user's alone, or cannot be read.
func signInRefused(why string) error {
	return proto.Errf(proto.CodeAuth, "remove it and run `aries login` again; aries keeps a sign-in only where it is yours alone",
		"the sign-in kept on this machine cannot be used: "+why)
}

// privatePath refuses path, a directory or a file of a sign-in, unless
// it is the user's and no one else may read or write it. On Windows
// nothing is checked: access is in ACLs, which this does not read, and
// the local application data directory is the user's own by default.
func privatePath(path string) error {
	st, err := os.Stat(path)
	switch {
	case err != nil:
		return signInRefused(fmt.Sprintf("%s cannot be read: %v", path, errors.Unwrap(err)))
	case !OwnedByUser(st):
		return signInRefused(path + " belongs to another user")
	case runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0:
		return signInRefused(fmt.Sprintf("%s can be read or written by other users (mode %04o)", path, st.Mode().Perm()))
	}
	return nil
}

// LoadSignIn returns the sign-in kept under dataDir for serverURL, or
// nil when there is none. One whose files others may read, or that does
// not parse, is refused with code auth and a fix to sign in again.
func LoadSignIn(dataDir, serverURL string) (*SignIn, error) {
	dir := SignInDir(dataDir, serverURL)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	tokenPath, keyPath := filepath.Join(dir, signInTokenFile), filepath.Join(dir, signInKeyFile)
	if _, err := os.Stat(tokenPath); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	for _, p := range []string{dir, tokenPath, keyPath} {
		if err := privatePath(p); err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(tokenPath) //nolint:gosec // the user's own data directory, checked above
	if err != nil {
		return nil, signInRefused(fmt.Sprintf("%s: %v", tokenPath, err))
	}
	var f signInFile
	if err := json.Unmarshal(b, &f); err != nil || f.Version != signInVersion || f.Token == "" || f.Server != serverURL {
		return nil, signInRefused(tokenPath + " is not a sign-in this aries reads")
	}
	kb, err := os.ReadFile(keyPath) //nolint:gosec // the user's own data directory, checked above
	if err != nil {
		return nil, signInRefused(fmt.Sprintf("%s: %v", keyPath, err))
	}
	if f.Key, err = dpop.ParseKey(kb); err != nil {
		return nil, signInRefused(fmt.Sprintf("%s: %v", keyPath, err))
	}
	return &f.SignIn, nil
}

// SaveSignIn keeps s under dataDir, in place of any sign-in for the same
// server: each file written whole and renamed into place, 0600, in a
// 0700 directory.
func SaveSignIn(dataDir string, s *SignIn) error {
	dir := SignInDir(dataDir, s.Server)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	if err := privatePath(dir); err != nil {
		return err
	}
	kb, err := dpop.MarshalKey(s.Key)
	if err != nil {
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	tb, err := json.MarshalIndent(signInFile{Version: signInVersion, SignIn: *s}, "", "  ")
	if err != nil {
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	// The key first: a token file names a sign-in only once its key is
	// there.
	if err := writePrivate(filepath.Join(dir, signInKeyFile), kb); err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, signInTokenFile), append(tb, '\n'))
}

// writePrivate writes b to path as a new 0600 file and renames it into
// place.
func writePrivate(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("keep the sign-in: %w", err)
	}
	return nil
}

// RemoveSignIn deletes the sign-in kept under dataDir for serverURL, if
// there is one.
func RemoveSignIn(dataDir, serverURL string) error {
	if err := os.RemoveAll(SignInDir(dataDir, serverURL)); err != nil {
		return fmt.Errorf("remove the sign-in: %w", err)
	}
	return nil
}
