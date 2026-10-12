package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Event operations on keys. Each one's target is the key's fingerprint;
// key.add's after state and key.remove's before state are the key.
const (
	OpKeyAdd    Op = "key.add"
	OpKeyRemove Op = "key.remove"
)

// PrincipalKey is one public SSH key of a principal. Only public keys
// are stored.
type PrincipalKey struct {
	// Fingerprint is the key's SHA-256 fingerprint, as
	// ssh.FingerprintSHA256 gives it ("SHA256:…"). A key belongs to one
	// principal.
	Fingerprint string `json:"fingerprint"`
	Principal   string `json:"principal"`
	// Key is the key in authorized_keys form: its type and base64, as
	// the parser wrote them, and nothing of what the caller sent.
	Key string `json:"key"`
	// Comment is the caller's comment, one line of safe text, for
	// display only: nothing builds an authorized_keys line from it.
	Comment string    `json:"comment,omitempty"`
	AddedBy string    `json:"added_by"`
	AddedAt time.Time `json:"added_at"`
}

const (
	// maxKeyLine bounds the line AddKey parses; a 16,384-bit RSA key,
	// the largest OpenSSH makes, is under 3 KiB.
	maxKeyLine = 16 << 10
	// maxKeyComment bounds a key's comment; the column holds it.
	maxKeyComment = 255
)

// fingerprintPattern is a SHA-256 fingerprint: unpadded base64 of 32
// bytes.
var fingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)

// ValidFingerprint reports whether s has the form of a key's SHA-256
// fingerprint, as ssh.FingerprintSHA256 gives it: "SHA256:" and 43
// characters of unpadded base64. Nothing in it needs quoting in a shell
// or in an authorized_keys option.
func ValidFingerprint(s string) bool { return fingerprintPattern.MatchString(s) }

// errKeyForm is what parseKey says of a line that is no key. It never
// repeats the line: what was pasted may be a private key.
var errKeyForm = fmt.Errorf("%w: the key must be one public key in authorized_keys form: TYPE BASE64 [COMMENT], with no options", ErrInvalid)

// parseKey reads one public key in authorized_keys form and returns its
// fingerprint, the key as the parser writes it back (type and base64),
// and its comment. It refuses, with ErrInvalid, anything else: a second
// line, options before the key, a type that is not the key's (the parser
// checks it), a certificate, and a comment that is not one line of safe
// text. One trailing newline is allowed, as a .pub file ends with one.
func parseKey(line string) (fingerprint, key, comment string, err error) {
	if len(line) > maxKeyLine {
		return "", "", "", fmt.Errorf("%w: the key is longer than %d bytes", ErrInvalid, maxKeyLine)
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if strings.ContainsAny(line, "\r\n") {
		return "", "", "", errKeyForm
	}
	pub, comment, options, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(options) > 0 {
		return "", "", "", errKeyForm
	}
	if _, cert := pub.(*ssh.Certificate); cert {
		return "", "", "", fmt.Errorf("%w: the key is a certificate; give the public key itself", ErrInvalid)
	}
	if err := checkLine("key comment", comment, maxKeyComment, false); err != nil {
		return "", "", "", err
	}
	return ssh.FingerprintSHA256(pub), strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), comment, nil
}

// AddKey adds a public key to a principal, from one line in
// authorized_keys form (TYPE BASE64 [COMMENT]). It stores the type and
// base64 the parser gives back, never the caller's text, and keeps the
// comment for display only.
//
// A server admin may add a key to any principal but themselves. A
// project admin may only while every project the principal belongs to
// is one they administer; a principal in no project, and a server
// admin, are out of their reach. Otherwise they could add a key of
// their own to someone with access elsewhere and act as them there.
// Nobody adds a key to their own account (decision 19): the refusal is
// an [*AccessError] whose reason is [AccessOwnKeys].
//
// A key the principal already has is returned as it is. A line that is
// not one public key is refused with ErrInvalid, without repeating it,
// and a key past ControlLimits.Keys with a [*ControlLimitError].
//
// A server admin is told with ErrNotFound that no principal has the
// name, and with a [*TakenError], which does not say whose, that another
// principal has the key. A project admin is told neither: a name no one
// has is refused as one out of reach is ([AccessReach]), and a key
// another principal has with an [*AccessError] whose reason is
// [AccessKey], whoever has it. A project admin can still tell that a key
// is on the server, since a new key for a principal within reach is
// added and this one is not; the refusal only does not say so.
//
// The order of the checks holds two things at once. Who the actor is,
// and whether the principal is within their reach, are decided first, so
// that nothing about a name or a key is said to someone who may not act
// on it. Then, in the same write and before the row is inserted, the key
// is looked for in the store and in the keys held elsewhere
// ([Control.KeysElsewhere], the hand-maintained authorized_keys): neither
// lookup can be skipped or raced, and a project admin gets the one
// [AccessKey] refusal for either, so it cannot tell a key in that file
// from a key in the store.
func (c *Control) AddKey(ctx context.Context, actor Actor, principal, line string) (PrincipalKey, error) {
	if err := validPrincipal(principal); err != nil {
		return PrincipalKey{}, err
	}
	fp, key, comment, err := parseKey(line)
	if err != nil {
		return PrincipalKey{}, err
	}
	var out PrincipalKey
	err = c.write(ctx, actor, func(w *cwtx) error {
		if err := w.ownKeys(OpKeyAdd, principal); err != nil {
			return err
		}
		sc, err := w.scope(ctx, OpKeyAdd, principal)
		if err != nil {
			return err
		}
		// Reach before existence: a project admin is refused alike for a
		// name no one has and for one out of reach.
		if err := w.within(ctx, sc, OpKeyAdd, principal); err != nil {
			return err
		}
		if _, err := readPrincipal(ctx, w.tx, principal); err != nil {
			return err
		}
		out, err = w.addKey(ctx, principal, parsedKey{fp, key, comment})
		return err
	})
	if err != nil {
		return PrincipalKey{}, err
	}
	return out, nil
}

// parsedKey is a key as parseKey read it.
type parsedKey struct{ fingerprint, key, comment string }

// addKey gives principal the key k and records it, or returns the key as
// it is when principal has it already. It refuses a key another
// principal has, in the store or elsewhere (Control.KeysElsewhere), and
// one past ControlLimits.Keys. The caller has checked
// the actor and that the principal exists.
func (w *cwtx) addKey(ctx context.Context, principal string, k parsedKey) (PrincipalKey, error) {
	switch cur, err := readKey(ctx, w.tx, k.fingerprint); {
	case err == nil && cur.Principal == principal:
		return cur, nil
	case err == nil && !w.admin:
		// A project admin is not told that the key is anyone's.
		return PrincipalKey{}, w.denied(OpKeyAdd, principal, "", AccessKey)
	case err == nil:
		return PrincipalKey{}, &TakenError{Kind: TakenKey, Value: k.fingerprint}
	case !errors.Is(err, ErrNotFound):
		return PrincipalKey{}, err
	}
	if w.elsewhere != nil {
		if owner, ok := w.elsewhere(k.fingerprint); ok && owner != principal {
			if !w.admin {
				// As for a key the store holds: not told that it is
				// anyone's, or where.
				return PrincipalKey{}, w.denied(OpKeyAdd, principal, "", AccessKey)
			}
			return PrincipalKey{}, &TakenError{Kind: TakenKey, Value: k.fingerprint, Elsewhere: true}
		}
	}
	if err := w.room(ctx, limitKeys, w.lim.Keys, `SELECT COUNT(*) FROM principal_keys WHERE principal = ?`, principal); err != nil {
		return PrincipalKey{}, err
	}
	out := PrincipalKey{Fingerprint: k.fingerprint, Principal: principal, Key: k.key, Comment: k.comment, AddedBy: w.actor.Principal, AddedAt: w.now}
	if _, err := w.exec(ctx, "INSERT INTO principal_keys (fingerprint, principal, `key`, comment, added_by, added_at) VALUES (?, ?, ?, ?, ?, ?)",
		out.Fingerprint, principal, out.Key, out.Comment, out.AddedBy, w.now); err != nil {
		return PrincipalKey{}, fmt.Errorf("add key %s: %w", out.Fingerprint, err)
	}
	w.indexed(func(ix *index) { ix.byFingerprint[out.Fingerprint] = indexedKey{principal: principal, key: out.Key} })
	return out, w.event(ctx, OpKeyAdd, out.Fingerprint, nil, out)
}

// RemoveKey removes the key with the given fingerprint and returns it.
// Who may is as for [Control.AddKey], by the key's principal, and nobody
// removes a key of their own account.
//
// A server admin is told with ErrNotFound that no principal has the
// fingerprint. A project admin is refused the same way, with an
// [*AccessError] that names no principal, for a key that does not exist
// as for one of a principal out of reach: the refusal does not say
// whether a key is on the server, or whose it is.
func (c *Control) RemoveKey(ctx context.Context, actor Actor, fingerprint string) (PrincipalKey, error) {
	if !fingerprintPattern.MatchString(fingerprint) {
		return PrincipalKey{}, fmt.Errorf("%w: a key is named by its SHA256: fingerprint", ErrInvalid)
	}
	var out PrincipalKey
	err := c.write(ctx, actor, func(w *cwtx) error {
		sc, err := w.scope(ctx, OpKeyRemove, "")
		if err != nil {
			return err
		}
		cur, err := readKey(ctx, w.tx, fingerprint)
		switch {
		case errors.Is(err, ErrNotFound) && !sc.server:
			return w.denied(OpKeyRemove, "", "", AccessReach)
		case err != nil:
			return err
		}
		// The actor's own key is theirs to know of.
		if err := w.ownKeys(OpKeyRemove, cur.Principal); err != nil {
			return err
		}
		if err := w.within(ctx, sc, OpKeyRemove, cur.Principal); err != nil {
			if _, refused := errors.AsType[*AccessError](err); refused {
				return w.denied(OpKeyRemove, "", "", AccessReach)
			}
			return err
		}
		if _, err := w.exec(ctx, `DELETE FROM principal_keys WHERE fingerprint = ?`, fingerprint); err != nil {
			return fmt.Errorf("remove key %s: %w", fingerprint, err)
		}
		out = cur
		w.indexed(func(ix *index) { delete(ix.byFingerprint, fingerprint) })
		return w.event(ctx, OpKeyRemove, fingerprint, cur, nil)
	})
	if err != nil {
		return PrincipalKey{}, err
	}
	return out, nil
}

// Keys returns a principal's keys, oldest first; none for a principal
// that has none or does not exist. Reads are not guarded: the caller
// decides who may list them.
func (c *Control) Keys(ctx context.Context, principal string) ([]PrincipalKey, error) {
	if err := validPrincipal(principal); err != nil {
		return nil, err
	}
	return keysOf(ctx, c.s.r, principal)
}

// KeysAddedBy returns the keys that adder added and that are still in
// the store, whatever principal each belongs to, by fingerprint. Reads
// are not guarded: the caller decides who may list them.
func (c *Control) KeysAddedBy(ctx context.Context, adder string) ([]PrincipalKey, error) {
	if err := validActorName(adder); err != nil {
		return nil, err
	}
	return queryKeys(ctx, c.s.r, "keys added by "+adder, ` WHERE added_by = ? ORDER BY fingerprint`, adder)
}

// keysOf reads principal's keys, oldest first.
func keysOf(ctx context.Context, q querier, principal string) ([]PrincipalKey, error) {
	return queryKeys(ctx, q, "keys of "+principal, ` WHERE principal = ? ORDER BY added_at, fingerprint`, principal)
}

// queryKeys reads the keys that where, a constant clause with a
// placeholder for arg, selects. what names them in an error.
func queryKeys(ctx context.Context, q querier, what, where, arg string) ([]PrincipalKey, error) {
	rows, err := q.QueryContext(ctx, keySelect+where, arg) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	defer func() { _ = rows.Close() }()
	var out []PrincipalKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

const keySelect = "SELECT fingerprint, principal, `key`, comment, added_by, added_at FROM principal_keys"

func scanKey(r scanner) (PrincipalKey, error) {
	var k PrincipalKey
	if err := r.Scan(&k.Fingerprint, &k.Principal, &k.Key, &k.Comment, &k.AddedBy, &k.AddedAt); err != nil {
		return PrincipalKey{}, err
	}
	k.AddedAt = k.AddedAt.UTC()
	return k, nil
}

// readKey reads one key, or returns ErrNotFound.
func readKey(ctx context.Context, q querier, fingerprint string) (PrincipalKey, error) {
	k, err := scanKey(q.QueryRowContext(ctx, keySelect+` WHERE fingerprint = ?`, fingerprint))
	if errors.Is(err, sql.ErrNoRows) {
		return PrincipalKey{}, fmt.Errorf("key %s: %w", fingerprint, ErrNotFound)
	}
	if err != nil {
		return PrincipalKey{}, fmt.Errorf("key %s: %w", fingerprint, err)
	}
	return k, nil
}
