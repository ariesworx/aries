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
// A server admin may add a key to any principal. A project admin may
// only while every project the principal belongs to is one they
// administer; a principal in no project, and a server admin, are out of
// their reach. Otherwise they could add a key of their own to someone
// with access elsewhere and act as them there.
//
// A key the principal already has is returned as it is. A key another
// principal has is refused with a [*TakenError], which does not say
// whose; a line that is not one public key with ErrInvalid, without
// repeating it; and a key past ControlLimits.Keys with a
// [*ControlLimitError].
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
		sc, err := w.scope(ctx, OpKeyAdd, principal)
		if err != nil {
			return err
		}
		if _, err := readPrincipal(ctx, w.tx, principal); err != nil {
			return err
		}
		if err := w.within(ctx, sc, OpKeyAdd, principal); err != nil {
			return err
		}
		switch cur, err := readKey(ctx, w.tx, fp); {
		case err == nil && cur.Principal == principal:
			out = cur
			return nil
		case err == nil:
			return &TakenError{Kind: TakenKey, Value: fp}
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if err := w.room(ctx, limitKeys, w.lim.Keys, `SELECT COUNT(*) FROM principal_keys WHERE principal = ?`, principal); err != nil {
			return err
		}
		out = PrincipalKey{Fingerprint: fp, Principal: principal, Key: key, Comment: comment, AddedBy: w.actor.Principal, AddedAt: w.now}
		if _, err := w.exec(ctx, "INSERT INTO principal_keys (fingerprint, principal, `key`, comment, added_by, added_at) VALUES (?, ?, ?, ?, ?, ?)",
			fp, principal, key, comment, out.AddedBy, w.now); err != nil {
			return fmt.Errorf("add key %s: %w", fp, err)
		}
		w.indexed(func(ix *index) { ix.byFingerprint[fp] = indexedKey{principal: principal, key: key} })
		return w.event(ctx, OpKeyAdd, fp, nil, out)
	})
	if err != nil {
		return PrincipalKey{}, err
	}
	return out, nil
}

// RemoveKey removes the key with the given fingerprint and returns it.
// Who may is as for [Control.AddKey], by the key's principal. A
// fingerprint no principal has is ErrNotFound.
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
		if err != nil {
			return err
		}
		if err := w.within(ctx, sc, OpKeyRemove, cur.Principal); err != nil {
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
	rows, err := c.s.r.QueryContext(ctx, keySelect+` WHERE principal = ? ORDER BY added_at, fingerprint`, principal)
	if err != nil {
		return nil, fmt.Errorf("keys of %s: %w", principal, err)
	}
	defer func() { _ = rows.Close() }()
	var out []PrincipalKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, fmt.Errorf("keys of %s: %w", principal, err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("keys of %s: %w", principal, err)
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
