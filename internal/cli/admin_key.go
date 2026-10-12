package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/proto"
)

// `sfx admin key`: a principal's public SSH keys, which the server's
// sshd asks starfixd for (design §15.2). An admin adds and removes
// them; nobody changes the keys of their own account, which the server
// decides. Only a public key ever leaves this machine: the file is read
// and checked here, and what is sent is the key as the parser writes it
// back, never the file's bytes.

// maxKeyFile bounds a public key file. The largest key OpenSSH makes is
// under 3 KiB.
const maxKeyFile = 16 << 10

// fingerprintForm is a key's SHA-256 fingerprint as ssh-keygen -l and
// `sfx admin key list` print it.
var fingerprintForm = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// fileFlag collects a repeatable flag whose value is a file's path,
// which may hold a comma.
type fileFlag []string

func (f *fileFlag) String() string { return strings.Join(*f, " ") }

func (f *fileFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// publicKey reads the one public key in file, or on standard input when
// file is "-", and returns it in authorized_keys form with its comment:
// the line a .pub file holds. A relative path is relative to the
// directory the command runs in (-C).
//
// It refuses anything else with an invalid refusal that says which file
// and never what it holds: the usual mistake is to name the private key
// beside the .pub file, and a private key must not be sent or shown.
func (r *runner) publicKey(file string) (string, error) {
	what, pub := "standard input", "your public key file, such as ~/.ssh/id_ed25519.pub"
	var b []byte
	var err error
	if file == "-" {
		b, err = io.ReadAll(io.LimitReader(r.env.Stdin, maxKeyFile+1))
	} else {
		what, pub = file, file+".pub"
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(r.dir, path)
		}
		b, err = readUpTo(path, maxKeyFile+1)
	}
	refuse := func(fix, msg string) (string, error) { return "", proto.Errf(proto.CodeInvalid, fix, what+" "+msg) }
	const give = "give a public key file, such as ~/.ssh/id_ed25519.pub, or - to read one from standard input"
	switch {
	case err != nil:
		// The cause alone: the path is named already, and nothing read is.
		cause := err
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			cause = pe.Err
		}
		return "", proto.Errf(proto.CodeInvalid, give, fmt.Sprintf("cannot read %s: %v", what, cause))
	case len(b) > maxKeyFile:
		return refuse(give, fmt.Sprintf("is larger than %d KiB, so it is no public key", maxKeyFile>>10))
	case strings.Contains(string(b), "PRIVATE KEY"):
		return refuse("give the public key instead: "+pub, "holds a private key, which never leaves this machine")
	}
	line := strings.TrimRight(string(b), "\r\n")
	switch {
	case strings.TrimSpace(line) == "":
		return refuse(give, "is empty")
	case strings.ContainsAny(line, "\r\n"):
		return refuse("give a file with one key, as a .pub file is; add each key with its own command", "holds more than one line")
	}
	key, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(options) > 0 {
		return refuse(give, "is not one public key in OpenSSH form (TYPE BASE64 [COMMENT])")
	}
	if _, cert := key.(*ssh.Certificate); cert {
		return refuse("give the public key the certificate was made for", "holds a certificate, not a public key")
	}
	out := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if comment != "" {
		out += " " + comment
	}
	return out, nil
}

// readUpTo reads at most n bytes of the file at path.
func readUpTo(path string, n int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the person running sfx names the file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, n))
}

func adminKey(ctx context.Context, r *runner, args []string) error {
	const usage = "admin key add NAME FILE|- | list NAME | remove FINGERPRINT"
	fs := r.newFlags("admin key")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin key needs add, list or remove")
	}
	switch pos[0] {
	case "add":
		if len(pos) != 3 {
			return usagef(usage, "key add needs a principal and a public key file, such as bob ~/.ssh/id_ed25519.pub; - reads the key from standard input")
		}
		// Read and checked before anything is sent.
		line, err := r.publicKey(pos[2])
		if err != nil {
			return err
		}
		var out proto.KeyResult
		if err := r.call(ctx, proto.OpKeyAdd, proto.KeyAddArgs{Name: pos[1], Key: line}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printKeyAdded(r.env.Stdout, out)
		return nil
	case "list":
		if len(pos) != 2 {
			return usagef(usage, "key list needs one principal, such as bob")
		}
		var out proto.KeysResult
		if err := r.call(ctx, proto.OpKeys, proto.KeysArgs{Name: pos[1]}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printKeys(r.env.Stdout, out)
		return nil
	case "remove":
		switch {
		case len(pos) != 2:
			return usagef(usage, "key remove needs one fingerprint, as `sfx admin key list NAME` prints it")
		case !fingerprintForm.MatchString(pos[1]):
			return usagef(usage, "key remove takes a key's SHA256: fingerprint, as `sfx admin key list NAME` and `ssh-keygen -lf FILE.pub` print it")
		}
		var out proto.KeyResult
		if err := r.call(ctx, proto.OpKeyRemove, proto.KeyRemoveArgs{Fingerprint: pos[1]}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printKeyRemoved(r.env.Stdout, out)
		if out.Warning != "" {
			_, _ = fmt.Fprintf(r.env.Stderr, "sfx: warning: %s\n", esc(out.Warning))
		}
		return nil
	}
	return usagef(usage, "unknown key action %q", pos[0])
}

// keyLabel is a key's type and comment, in parentheses.
func keyLabel(k proto.KeyResult) string {
	if k.Comment == "" {
		return "(" + esc(k.Type) + ")"
	}
	return "(" + esc(k.Type) + ", " + esc(k.Comment) + ")"
}

func printKeyAdded(w io.Writer, k proto.KeyResult) {
	if k.Change == "unchanged" {
		_, _ = fmt.Fprintf(w, "%s already has key %s %s\n", esc(k.Principal), esc(k.ID), keyLabel(k))
		return
	}
	_, _ = fmt.Fprintf(w, "key %s %s added to %s\n", esc(k.ID), keyLabel(k), esc(k.Principal))
}

// printKeyRemoved says a key is gone, and how many connections went
// with it: those the key had signed in. A connection that came in by a
// line of the server's authorized_keys file is not among them; the
// server's warning, printed after, says when the principal has any.
func printKeyRemoved(w io.Writer, k proto.KeyResult) {
	_, _ = fmt.Fprintf(w, "key %s removed from %s\n", esc(k.ID), esc(k.Principal))
	switch {
	case k.Dropped == 1:
		_, _ = fmt.Fprintln(w, "1 connection it had signed in was closed")
	case k.Dropped > 1:
		_, _ = fmt.Fprintf(w, "%d connections it had signed in were closed\n", k.Dropped)
	}
}

func printKeys(w io.Writer, res proto.KeysResult) {
	defer printFileConns(w, res.Principal, res.FileConns)
	if len(res.Keys) == 0 {
		_, _ = fmt.Fprintf(w, "no keys recorded for %s\n", esc(res.Principal))
		return
	}
	_, _ = fmt.Fprintf(w, "keys of %s\n", esc(res.Principal))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FINGERPRINT\tTYPE\tCOMMENT\tADDED BY\tADDED")
	for _, k := range res.Keys {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", esc(k.Fingerprint), esc(k.Type), esc(k.Comment), esc(k.AddedBy), when(k.AddedAt))
	}
	_ = tw.Flush()
}

// printReleased says how many claims ended with the access of who, when
// any did.
func printReleased(w io.Writer, n int, who string) {
	switch {
	case n == 1:
		_, _ = fmt.Fprintf(w, "1 claim %s held was released\n", esc(who))
	case n > 1:
		_, _ = fmt.Fprintf(w, "%d claims %s held were released\n", n, esc(who))
	}
}
