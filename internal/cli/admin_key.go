package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/proto"
)

// `aries admin key`: a principal's public SSH keys, which the server's
// sshd asks ariesd for (design §15.2). An admin adds and removes
// them; nobody adds a key to their own account, though anyone may
// remove one of their own that their connection did not sign in with,
// which the server decides. Only a public key ever leaves this machine: the file is read
// and checked here, and what is sent is the key as the parser writes it
// back, never the file's bytes.

// maxKeyFile bounds a public key file. The largest key OpenSSH makes is
// under 3 KiB.
const maxKeyFile = 16 << 10

// fingerprintForm is a key's SHA-256 fingerprint as ssh-keygen -l and
// `aries admin key list` print it.
var fingerprintForm = lazyregexp.New(`^SHA256:[A-Za-z0-9+/]{43}$`)

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
	f, err := os.Open(path) //nolint:gosec // the person running aries names the file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, n))
}

func adminKey(ctx context.Context, r *runner, args []string) error {
	const usage = "admin key add NAME FILE|- [--expires DURATION|TIME] | list NAME | list --added-by NAME [-n N] [--after FINGERPRINT] | remove FINGERPRINT"
	fs := r.newFlags("admin key")
	addedBy := fs.String("added-by", "", "list the keys this principal added, whoever each belongs to")
	expires := fs.String("expires", "", "end the key after a duration (12h, 30d) counted on the server's clock, or at a time (2026-12-31T17:00:00Z)")
	list := listFlags(fs)
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "admin key needs add, list or remove")
	}
	switch {
	case pos[0] == "list" && *addedBy != "":
		err = only(fs, usage, "key list --added-by", "added-by", "n", "after")
	case pos[0] == "add":
		err = only(fs, usage, "key add, without --added-by,", "expires")
	default:
		err = only(fs, usage, "key "+pos[0]+", without --added-by,")
	}
	if err != nil {
		return err
	}
	switch pos[0] {
	case "add":
		if len(pos) != 3 {
			return usagef(usage, "key add needs a principal and a public key file, such as bob ~/.ssh/id_ed25519.pub; - reads the key from standard input")
		}
		in := proto.KeyAddArgs{Name: pos[1]}
		if *expires != "" {
			if in.ExpiresIn, in.ExpiresAt, err = keyExpires(*expires); err != nil {
				return usagef(usage, "%v", err)
			}
		}
		// Read and checked before anything is sent.
		if in.Key, err = r.publicKey(pos[2]); err != nil {
			return err
		}
		var out proto.KeyResult
		if err := r.call(ctx, proto.OpKeyAdd, in, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		printKeyAdded(r.env.Stdout, out)
		r.warn(out.Warning)
		return nil
	case "list":
		in := proto.KeysArgs{AddedBy: *addedBy, AdminListArgs: *list}
		switch {
		case *addedBy != "" && len(pos) != 1:
			return usagef(usage, "key list takes a principal, or --added-by NAME, not both")
		case *addedBy == "" && len(pos) != 2:
			return usagef(usage, "key list needs one principal, such as bob, or --added-by NAME")
		case *addedBy == "":
			in.Name = pos[1]
		}
		var out proto.KeysResult
		if err := r.call(ctx, proto.OpKeys, in, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		if *addedBy != "" {
			printKeysAddedBy(r.env.Stdout, out)
			return nil
		}
		printKeys(r.env.Stdout, out)
		return nil
	case "remove":
		switch {
		case len(pos) != 2:
			return usagef(usage, "key remove needs one fingerprint, as `aries admin key list NAME` prints it")
		case !fingerprintForm.MatchString(pos[1]):
			return usagef(usage, "key remove takes a key's SHA256: fingerprint, as `aries admin key list NAME` and `ssh-keygen -lf FILE.pub` print it")
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
		r.warn(out.Warning)
		return nil
	}
	return usagef(usage, "unknown key action %q", pos[0])
}

// keyExpires reads --expires: a duration, which is sent as it is for
// the server to count from its own time, or a time in RFC 3339 with its
// zone.
func keyExpires(s string) (in string, at *time.Time, err error) {
	if _, err := proto.ParseDuration(s); err == nil {
		return s, nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "", nil, fmt.Errorf("--expires takes a duration such as 12h or 30d, or a time such as 2026-12-31T17:00:00Z, not %q", s)
	}
	return "", &t, nil
}

// ending is ", ending TIME" for a key with an end, and "" for one with
// none.
func ending(end *time.Time) string {
	if end == nil {
		return ""
	}
	return ", ending " + endWhen(*end)
}

// endWhen is a key's end, to the second, as sshd holds it.
func endWhen(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

// keyLabel is a key's type and comment, in parentheses.
func keyLabel(k proto.KeyResult) string {
	if k.Comment == "" {
		return "(" + esc(k.Type) + ")"
	}
	return "(" + esc(k.Type) + ", " + esc(k.Comment) + ")"
}

func printKeyAdded(w io.Writer, k proto.KeyResult) {
	if k.Change == "unchanged" {
		_, _ = fmt.Fprintf(w, "%s already has key %s %s%s\n", esc(k.Principal), esc(k.Fingerprint), keyLabel(k), ending(k.ExpiresAt))
		return
	}
	_, _ = fmt.Fprintf(w, "key %s %s added to %s%s\n", esc(k.Fingerprint), keyLabel(k), esc(k.Principal), ending(k.ExpiresAt))
}

// printKeyRemoved says a key is gone from the store, and how many
// connections went with it: those the key had signed in by its line of
// the server's keys file. The server's warning, printed after, says when
// the key is also a line of the hand-maintained authorized_keys file,
// which still lets it in.
func printKeyRemoved(w io.Writer, k proto.KeyResult) {
	_, _ = fmt.Fprintf(w, "key %s removed from %s\n", esc(k.Fingerprint), esc(k.Principal))
	switch {
	case k.Dropped == 1:
		_, _ = fmt.Fprintln(w, "1 connection it had signed in was closed")
	case k.Dropped > 1:
		_, _ = fmt.Fprintf(w, "%d connections it had signed in were closed\n", k.Dropped)
	}
}

// printKeysAddedBy lists the keys one principal added, each with whose
// it is: what that principal's account left behind, which disabling it
// does not remove.
func printKeysAddedBy(w io.Writer, res proto.KeysResult) {
	if len(res.Keys) == 0 {
		_, _ = fmt.Fprintf(w, "no keys added by %s\n", esc(res.AddedBy))
		return
	}
	_, _ = fmt.Fprintf(w, "keys added by %s\n", esc(res.AddedBy))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "FINGERPRINT\tPRINCIPAL\tTYPE\tCOMMENT\tADDED")
	for _, k := range res.Keys {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", esc(k.Fingerprint), esc(k.Principal), esc(k.Type), esc(k.Comment), when(k.AddedAt))
	}
	_ = tw.Flush()
	more(w, res.More, res.Keys[len(res.Keys)-1].Fingerprint)
}

// printKeys lists a principal's keys. When any of them has an end, a
// column says each one's; a key past its end is listed until the server
// prunes it. When any of them is also a line of the server's
// hand-maintained authorized_keys file, a last column marks those, and a
// line after the table says what the mark means.
func printKeys(w io.Writer, res proto.KeysResult) {
	defer printFileConns(w, res.Principal, res.FileConns)
	if len(res.Keys) == 0 {
		_, _ = fmt.Fprintf(w, "no keys recorded for %s\n", esc(res.Principal))
		return
	}
	byHand := slices.ContainsFunc(res.Keys, func(k proto.Key) bool { return k.InAuthorizedKeys })
	ends := slices.ContainsFunc(res.Keys, func(k proto.Key) bool { return k.ExpiresAt != nil })
	_, _ = fmt.Fprintf(w, "keys of %s\n", esc(res.Principal))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	header := "FINGERPRINT\tTYPE\tCOMMENT\tADDED BY\tADDED"
	if ends {
		header += "\tENDS"
	}
	if byHand {
		header += "\tALSO IN"
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, k := range res.Keys {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s", esc(k.Fingerprint), esc(k.Type), esc(k.Comment), esc(k.AddedBy), when(k.AddedAt))
		switch {
		case k.ExpiresAt != nil:
			_, _ = fmt.Fprint(tw, "\t"+endWhen(*k.ExpiresAt))
		case ends:
			_, _ = fmt.Fprint(tw, "\t-")
		}
		switch {
		case k.InAuthorizedKeys:
			_, _ = fmt.Fprint(tw, "\tauthorized_keys")
		case byHand:
			_, _ = fmt.Fprint(tw, "\t-")
		}
		_, _ = fmt.Fprintln(tw)
	}
	_ = tw.Flush()
	if byHand {
		_, _ = fmt.Fprintln(w, "ALSO IN authorized_keys: the key is also a line of the server's hand-maintained authorized_keys file, which sshd reads after the store's keys; `aries admin key remove` closes the key's connections, and sshd still accepts it by that line until the line is deleted on the server")
	}
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
