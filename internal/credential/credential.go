package credential

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/proto"
)

// Kind is a kind of store.
type Kind string

const (
	// KindAuto is the system's own store: the keychain on macOS, Credential
	// Manager on Windows, and elsewhere the Secret Service where one
	// answers, else the file.
	KindAuto Kind = ""
	// KindKeychain is the macOS keychain, through security(1).
	KindKeychain Kind = "keychain"
	// KindSecretService is the freedesktop Secret Service (GNOME Keyring,
	// KWallet), through secret-tool(1).
	KindSecretService Kind = "secret-service"
	// KindFile is a file of its own for each secret, 0600 and the user's,
	// in a directory 0700 and the user's under aries's data directory.
	KindFile Kind = "file"
	// KindWindows is Windows Credential Manager, which refuses every call
	// until it is built.
	KindWindows Kind = "windows"
)

// Store keeps secrets by name, for one service. Its errors never carry the
// secret, and nothing it starts has the secret in its argv.
type Store interface {
	// Kind is the kind of store this is.
	Kind() Kind
	// Name says, for a person, which store this is: "macOS keychain",
	// "Secret Service", "Windows Credential Manager", or "file" and its
	// directory.
	Name() string
	// Get returns the secret kept as name, and false when there is none.
	Get(ctx context.Context, name string) (string, bool, error)
	// Set keeps secret as name, in place of any there.
	Set(ctx context.Context, name, secret string) error
	// Remove removes the secret kept as name; removing none is not an
	// error.
	Remove(ctx context.Context, name string) error
}

// DefaultService is the service a store keeps its secrets under when
// Options names none.
const DefaultService = "aries"

// Options are what Open needs of the process. A zero field takes the
// process's own.
type Options struct {
	// GOOS is the system, runtime.GOOS by default.
	GOOS string
	// Getenv and Home find aries's data directory (offline.DataDir).
	Getenv func(string) string
	Home   func() (string, error)
	// Kind picks a store; KindAuto, the default, picks the system's.
	Kind Kind
	// Service names whose secrets these are: DefaultService by default.
	// Bearings keeps its own under another.
	Service string
	// Keychain is the keychain file the keychain store uses, in place of
	// the user's default keychain. Tests set it.
	Keychain string
	// Command and LookPath start and find the system's commands:
	// exec.CommandContext and exec.LookPath by default. Tests wrap them.
	Command  func(ctx context.Context, name string, args ...string) *exec.Cmd
	LookPath func(string) (string, error)

	// probeTimeout bounds the Secret Service's first answer.
	probeTimeout time.Duration
}

// probeTimeout is how long Open waits for a Secret Service to answer
// before it takes the file instead.
const probeTimeout = 5 * time.Second

// Open returns the store o picks. With KindAuto it is never an error on
// macOS, Linux or Windows, unless aries has no data directory where the
// file is the store.
func Open(ctx context.Context, o Options) (Store, error) {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Home == nil {
		o.Home = os.UserHomeDir
	}
	if o.Service == "" {
		o.Service = DefaultService
	}
	if o.Command == nil {
		o.Command = exec.CommandContext
	}
	if o.LookPath == nil {
		o.LookPath = exec.LookPath
	}
	if o.probeTimeout == 0 {
		o.probeTimeout = probeTimeout
	}
	if !validName.MatchString(o.Service) {
		return nil, proto.Errf(proto.CodeInvalid, "name a service of lowercase letters, digits, '.', '_' and '-'",
			fmt.Sprintf("%q is not a service a credential store keeps secrets under", o.Service))
	}
	x := execer{command: o.Command}
	kind := o.Kind
	if kind == KindAuto {
		switch o.GOOS {
		case "darwin":
			kind = KindKeychain
		case "windows":
			kind = KindWindows
		default:
			if s, ok := secretService(ctx, o, x); ok {
				return s, nil
			}
			kind = KindFile
		}
	}
	refuse := func(why string) (Store, error) {
		return nil, proto.Errf(proto.CodeInvalid, "leave the kind of store to aries, which picks the system's own", why)
	}
	switch kind {
	case KindKeychain:
		if o.GOOS != "darwin" {
			return refuse("the keychain is macOS's, and this is " + o.GOOS)
		}
		if strings.ContainsAny(o.Keychain, "\"\\\n") {
			return refuse("a keychain's path may not hold a quote, a backslash or a newline")
		}
		return &keychain{service: o.Service, file: o.Keychain, x: x}, nil
	case KindSecretService:
		if o.GOOS == "darwin" || o.GOOS == "windows" {
			return refuse("the Secret Service is not " + o.GOOS + "'s")
		}
		return &secretTool{service: o.Service, x: x}, nil
	case KindFile:
		if o.GOOS == "windows" {
			// A file's access is in its ACL there, which this does not read.
			return refuse("a credential file is not kept on Windows, where its mode says nothing of who may read it")
		}
		return newFileStore(o)
	case KindWindows:
		if o.GOOS != "windows" {
			return refuse("Credential Manager is Windows's, and this is " + o.GOOS)
		}
		return windows{}, nil
	}
	return refuse(fmt.Sprintf("%q is not a kind of credential store", kind))
}

// validName is a secret's name or a service: it is part of a file's name
// and of argv, so it is plain.
var validName = lazyregexp.New(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func checkName(name string) error {
	if !validName.MatchString(name) {
		return proto.Errf(proto.CodeInvalid, "name it with up to 64 lowercase letters, digits, '.', '_' and '-', beginning with a letter or digit",
			fmt.Sprintf("%q is not a name a secret is kept under", name))
	}
	return nil
}

// minSecret and maxSecret bound a secret. An API key or a token is
// longer than the one and far shorter than the other; and a secret too
// short would be found by chance in a command line run refuses.
const (
	minSecret = 16
	maxSecret = 4096
)

// checkSecret takes a secret of printable ASCII with no space, which every
// store keeps as it is and every command prints back unchanged. Its
// refusal says where, never what.
func checkSecret(secret string) error {
	refuse := func(why string) error {
		return proto.Errf(proto.CodeInvalid, "give the secret as the provider issued it, with no space or newline around it", why)
	}
	if len(secret) < minSecret {
		return refuse(fmt.Sprintf("the secret is %d bytes, fewer than the %d of any API key or token", len(secret), minSecret))
	}
	if len(secret) > maxSecret {
		return refuse(fmt.Sprintf("the secret is %d bytes, more than the %d a store keeps", len(secret), maxSecret))
	}
	for i := range len(secret) {
		if c := secret[i]; c < 0x21 || c > 0x7e {
			return refuse(fmt.Sprintf("byte %d of the secret is not printable ASCII, or is a space", i+1))
		}
	}
	return nil
}

// execer starts the system's commands for a store. Every one goes through
// run, which refuses an argv carrying the secret.
type execer struct {
	command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// result is what a command did.
type result struct {
	stdout []byte
	stderr string // redacted
	code   int    // its exit code; -1 when it did not exit by itself
}

// run starts name with args and stdin, and returns what it printed and its
// exit code. secret, when there is one, is what the command handles: an
// argv that carries it, or its hexadecimal form, is refused before
// anything starts, and it is taken out of stderr. An error is one of
// starting or waiting; a command that exits non-zero is not one.
func (x execer) run(ctx context.Context, secret, stdin, name string, args ...string) (result, error) {
	if secret != "" {
		h := hex.EncodeToString([]byte(secret))
		for _, a := range append([]string{name}, args...) {
			l := strings.ToLower(a)
			if strings.Contains(a, secret) || strings.Contains(l, h) {
				// A programming error the tests catch; refused all the same.
				return result{}, errors.New("credential: refused to start " + name + " with the secret in its command line")
			}
		}
	}
	cmd := x.command(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	r := result{stdout: out.Bytes(), stderr: redact(errb.String(), secret), code: -1}
	var ee *exec.ExitError
	switch {
	case err == nil:
		r.code = 0
	case errors.As(err, &ee) && ee.Exited():
		r.code = ee.ExitCode()
	case ctx.Err() != nil:
		return r, fmt.Errorf("%s did not finish: %w", name, ctx.Err())
	default:
		return r, fmt.Errorf("%s: %w", name, err)
	}
	return r, nil
}

// redact takes secret, in any of the forms a command is given it, out of
// s, and keeps s's first line, trimmed and bounded, for an error.
func redact(s, secret string) string {
	if secret != "" {
		h := hex.EncodeToString([]byte(secret))
		s = strings.ReplaceAll(s, secret, "[secret]")
		s = strings.ReplaceAll(s, h, "[secret]")
		s = strings.ReplaceAll(s, strings.ToUpper(h), "[secret]")
	}
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// failed is a command's refusal: what it said, or its exit code.
func failed(what string, r result) error {
	if r.stderr != "" {
		return fmt.Errorf("%s: %s", what, r.stderr)
	}
	return fmt.Errorf("%s: exit status %d", what, r.code)
}
