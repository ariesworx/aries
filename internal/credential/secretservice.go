package credential

import (
	"context"
	"strings"

	"github.com/ariesworx/aries/internal/proto"
)

// secretTool is the freedesktop Secret Service, through secret-tool(1)
// from libsecret. `secret-tool store` reads the secret from stdin, so it
// is in no argv. Items carry the attributes service and account, as the
// keychain's do.
type secretTool struct {
	service string
	x       execer
}

// probeName is the account Open looks up to learn whether a Secret
// Service answers; no secret is ever kept under it, since it is not a
// valid name.
const probeName = ".probe"

// secretService returns the Secret Service store when secret-tool is
// installed and a service answers it within o.probeTimeout: a lookup of
// nothing exits 1 and prints nothing there, and prints why on stderr
// where no service runs or none can be started.
func secretService(ctx context.Context, o Options, x execer) (Store, bool) {
	if _, err := o.LookPath("secret-tool"); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, o.probeTimeout)
	defer cancel()
	s := &secretTool{service: o.Service, x: x}
	r, err := x.run(ctx, "", "", "secret-tool", s.attrs("lookup", probeName)...)
	if err != nil || r.stderr != "" || (r.code != 0 && r.code != 1) {
		return nil, false
	}
	return s, true
}

func (s *secretTool) Kind() Kind   { return KindSecretService }
func (s *secretTool) Name() string { return "Secret Service" }

func (s *secretTool) attrs(verb, name string, more ...string) []string {
	return append(append([]string{verb}, more...), "service", s.service, "account", name)
}

func (s *secretTool) Get(ctx context.Context, name string) (string, bool, error) {
	if err := checkName(name); err != nil {
		return "", false, err
	}
	r, err := s.x.run(ctx, "", "", "secret-tool", s.attrs("lookup", name)...)
	if err != nil {
		return "", false, s.unavailable(err)
	}
	switch {
	case r.code == 1 && r.stderr == "" && len(r.stdout) == 0:
		return "", false, nil
	case r.code != 0:
		return "", false, s.unavailable(failed("secret-tool lookup", r))
	}
	// It ends the secret with a newline only on a terminal; trim one all
	// the same.
	secret := strings.TrimSuffix(string(r.stdout), "\n")
	if checkSecret(secret) != nil {
		return "", false, proto.Errf(proto.CodeInvalid,
			"remove it with `secret-tool clear service "+s.service+" account "+name+"`, and keep the secret again",
			"the Secret Service's item "+name+" holds something other than a secret aries kept")
	}
	return secret, true, nil
}

func (s *secretTool) Set(ctx context.Context, name, secret string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkSecret(secret); err != nil {
		return err
	}
	r, err := s.x.run(ctx, secret, secret, "secret-tool", s.attrs("store", name, "--label="+s.service+" "+name)...)
	if err != nil {
		return s.unavailable(err)
	}
	if r.code != 0 {
		return s.unavailable(failed("secret-tool store", r))
	}
	return nil
}

func (s *secretTool) Remove(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	r, err := s.x.run(ctx, "", "", "secret-tool", s.attrs("clear", name)...)
	if err != nil {
		return s.unavailable(err)
	}
	// clear exits 1, printing nothing, when there was nothing to clear.
	if r.code != 0 && (r.code != 1 || r.stderr != "") {
		return s.unavailable(failed("secret-tool clear", r))
	}
	return nil
}

func (s *secretTool) unavailable(err error) error {
	return proto.Errf(proto.CodeUnavailable,
		"unlock your keyring (GNOME Keyring or KWallet) and try again",
		"the Secret Service could not be used: "+err.Error())
}
