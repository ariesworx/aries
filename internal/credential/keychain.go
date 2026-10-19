package credential

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ariesworx/aries/internal/proto"
)

// errNotFound is security(1)'s exit code for an item that is not there
// (errSecItemNotFound's low byte).
const errNotFound = 44

// keychain is the macOS keychain, through /usr/bin/security. The secret
// is written through `security -i`, which reads its command from stdin,
// with the secret in hexadecimal (-X), so that it is in no argv: `-w` on
// the command line would put it in every process listing. An item it
// adds trusts security(1) alone, which is how it reads the item back.
type keychain struct {
	service string
	file    string // a keychain of its own, for tests; "" is the default keychain
	x       execer
}

func (k *keychain) Kind() Kind   { return KindKeychain }
func (k *keychain) Name() string { return "macOS keychain" }

// args are the arguments that name the item, and the keychain last.
func (k *keychain) args(verb, name string, more ...string) []string {
	a := append([]string{verb, "-s", k.service, "-a", name}, more...)
	if k.file != "" {
		a = append(a, k.file)
	}
	return a
}

func (k *keychain) Get(ctx context.Context, name string) (string, bool, error) {
	if err := checkName(name); err != nil {
		return "", false, err
	}
	r, err := k.x.run(ctx, "", "", "security", k.args("find-generic-password", name, "-w")...)
	if err != nil {
		return "", false, k.unavailable(err)
	}
	switch r.code {
	case 0:
	case errNotFound:
		return "", false, nil
	default:
		return "", false, k.unavailable(failed("security find-generic-password", r))
	}
	secret := strings.TrimSuffix(string(r.stdout), "\n")
	if checkSecret(secret) != nil {
		// security prints data that is not text in hexadecimal; aries
		// stores none, so this item is someone else's.
		return "", false, proto.Errf(proto.CodeInvalid,
			fmt.Sprintf("remove the keychain item of service %s and account %s, and keep the secret again", k.service, name),
			"the keychain item "+name+" holds something other than a secret aries kept")
	}
	return secret, true, nil
}

func (k *keychain) Set(ctx context.Context, name, secret string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkSecret(secret); err != nil {
		return err
	}
	// -U replaces an item that is there. Each word is plain (checkName)
	// and the keychain's path holds no quote (Open), so quoting it is
	// enough for security's parser.
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -l \"%s %s\" -X %s",
		k.service, name, k.service, name, hex.EncodeToString([]byte(secret)))
	if k.file != "" {
		line += ` "` + k.file + `"`
	}
	r, err := k.x.run(ctx, secret, line+"\n", "security", "-i")
	if err != nil {
		return k.unavailable(err)
	}
	if r.code != 0 {
		return k.unavailable(failed("security add-generic-password", r))
	}
	// `security -i` exits with its last command's status, but read the
	// item back so that a write it dropped is not taken for one it made.
	got, ok, err := k.Get(ctx, name)
	if err != nil {
		return err
	}
	if !ok || got != secret {
		return k.unavailable(fmt.Errorf("the keychain did not keep %s", name))
	}
	return nil
}

func (k *keychain) Remove(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	r, err := k.x.run(ctx, "", "", "security", k.args("delete-generic-password", name)...)
	if err != nil {
		return k.unavailable(err)
	}
	if r.code != 0 && r.code != errNotFound {
		return k.unavailable(failed("security delete-generic-password", r))
	}
	return nil
}

func (k *keychain) unavailable(err error) error {
	return proto.Errf(proto.CodeUnavailable,
		"unlock your login keychain (Keychain Access, or `security unlock-keychain`) and try again",
		"the macOS keychain could not be used: "+err.Error())
}
