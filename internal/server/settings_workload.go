package server

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
)

// WorkloadSettings are the workload: settings (docs/design/identity.md
// §10.1, transport.md §7, slice N5): the platforms whose tokens service
// principals sign in with, and what such a sign-in gives.
type WorkloadSettings struct {
	// Audience is what a job asks its platform for a token for, bound to
	// its key where the platform lets it choose. Default the first of
	// https: public_urls.
	Audience string `yaml:"audience"`
	// TokenLifetime is how long a sign-in by a trust rule that names no
	// lifetime lasts, and MaxTokenLifetime the most any rule may give.
	// Defaults DefaultWorkloadLifetime and DefaultMaxWorkloadLifetime,
	// the second at most a day.
	TokenLifetime    Duration `yaml:"token_lifetime"`
	MaxTokenLifetime Duration `yaml:"max_token_lifetime"`
	// Fresh bounds how old a workload's token may be, from its iat, when
	// it is exchanged. Default DefaultWorkloadFresh, from 30s to 10m.
	Fresh Duration `yaml:"fresh"`
	// Providers are the platforms service principals sign in from. None
	// signs no service principal in by a token.
	Providers []oidc.WorkloadSettings `yaml:"providers"`
}

// The defaults and bounds of workload:'s times (identity.md §7.4).
const (
	DefaultWorkloadLifetime    = time.Hour
	DefaultMaxWorkloadLifetime = 6 * time.Hour
	DefaultWorkloadFresh       = 2 * time.Minute
)

// resolveWorkload fills w's defaults and refuses a time out of its
// bounds, or two providers of one name. The providers are opened by
// OpenWorkload.
func resolveWorkload(w WorkloadSettings) (WorkloadSettings, error) {
	w.TokenLifetime = cmp.Or(w.TokenLifetime, Duration(DefaultWorkloadLifetime))
	w.MaxTokenLifetime = cmp.Or(w.MaxTokenLifetime, Duration(DefaultMaxWorkloadLifetime))
	w.Fresh = cmp.Or(w.Fresh, Duration(DefaultWorkloadFresh))
	const fix = "; fix: set it within that under workload: in the config file, or leave it out for the default"
	span := func(d Duration) string {
		if time.Duration(d) < time.Minute {
			return time.Duration(d).String()
		}
		return proto.Span(time.Duration(d))
	}
	switch {
	case time.Duration(w.MaxTokenLifetime) > store.MaxTrustLifetime:
		return w, fmt.Errorf("workload: max_token_lifetime is %s; it can be at most %s%s", span(w.MaxTokenLifetime), proto.Span(store.MaxTrustLifetime), fix)
	case w.TokenLifetime > w.MaxTokenLifetime:
		return w, fmt.Errorf("workload: token_lifetime is %s; it can be at most max_token_lifetime, %s%s", span(w.TokenLifetime), span(w.MaxTokenLifetime), fix)
	case time.Duration(w.TokenLifetime) < store.MinTrustLifetime:
		return w, fmt.Errorf("workload: token_lifetime is %s; it must be at least %s%s", span(w.TokenLifetime), proto.Span(store.MinTrustLifetime), fix)
	case time.Duration(w.Fresh) < 30*time.Second || time.Duration(w.Fresh) > 10*time.Minute:
		return w, fmt.Errorf("workload: fresh is %s; it must be from 30s to 10m%s", span(w.Fresh), fix)
	}
	seen := map[string]bool{}
	for _, p := range w.Providers {
		if seen[p.Name] {
			return w, fmt.Errorf("workload: two providers are named %s; fix: give each provider a name of its own under workload: providers", p.Name)
		}
		seen[p.Name] = true
	}
	return w, nil
}

// Workload is what the server signs service principals in with: its
// workload providers, and the lives of what it gives.
type Workload struct {
	// Providers are the workload providers, by name.
	Providers []*oidc.Workload
	// TokenLifetime is a rule's lifetime when it names none, and
	// MaxTokenLifetime the most a sign-in by any rule lasts.
	TokenLifetime, MaxTokenLifetime time.Duration
	// Fresh bounds a workload token's age at the exchange, and how long
	// one is remembered as spent; Skew is what is allowed between a
	// platform's clock and the server's, login:'s.
	Fresh, Skew time.Duration
}

// OpenWorkload opens the workload providers of w, which ResolveSettings
// has read, reading their files and fetching nothing; nil when w names
// no provider. login gives the skew and the least time between reads of
// a provider's keys, which workload providers share with login's;
// audience is the server's workload audience; now is the store's clock
// (rule 19).
func OpenWorkload(w WorkloadSettings, login LoginSettings, audience string, now func() time.Time) (*Workload, error) {
	if len(w.Providers) == 0 {
		return nil, nil
	}
	out := &Workload{TokenLifetime: time.Duration(w.TokenLifetime), MaxTokenLifetime: time.Duration(w.MaxTokenLifetime), Fresh: time.Duration(w.Fresh), Skew: time.Duration(login.Skew)}
	audience = cmp.Or(w.Audience, audience)
	for _, ps := range w.Providers {
		p, err := oidc.OpenWorkload(ps, audience, oidc.Options{Now: now, Skew: time.Duration(login.Skew), MaxAge: out.Fresh,
			Refetch: time.Duration(login.JWKSRefetch)})
		if fix := providerFileFix(err, appArmorProfile()); fix != "" {
			return nil, fmt.Errorf("%w%s", err, fix)
		}
		if err != nil {
			return nil, fmt.Errorf("%w; fix: correct the provider under workload: providers in the config file (docs/server.md, Service principals)", err)
		}
		// A token is given to the provider its issuer names, so two
		// providers of one issuer could not be told apart.
		if i := slices.IndexFunc(out.Providers, func(o *oidc.Workload) bool { return o.Issuer() == p.Issuer() }); i >= 0 {
			return nil, fmt.Errorf("workload: providers %s and %s have one issuer, %s; fix: keep one provider for each issuer under workload: providers",
				out.Providers[i].Name(), p.Name(), p.Issuer())
		}
		out.Providers = append(out.Providers, p)
	}
	return out, nil
}

// checkWorkload refuses workload providers serve cannot serve: they need
// a server of many projects, whose control store holds the trust rules,
// and HTTPS, which the exchange is made over.
func (s Settings) checkWorkload() error {
	if len(s.Workload.Providers) == 0 {
		return nil
	}
	switch {
	case s.ControlSchema == "":
		return errors.New("workload: providers are set on a server of one project; trust rules live in the control store of a server of many (control_schema); fix: set control_schema (docs/server.md, Serving many projects), or remove workload: providers")
	case !s.HTTPSOn():
		return errors.New("workload: providers are set and HTTPS is off; a workload's token is exchanged over HTTPS; fix: add https to transports: and set https:, or remove workload: providers")
	}
	return nil
}
