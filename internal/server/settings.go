package server

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/aries/internal/keyproof"
	"github.com/ariesworx/aries/internal/lazyregexp"
	"github.com/ariesworx/aries/internal/logs"
	"github.com/ariesworx/aries/internal/oidc"
	"github.com/ariesworx/aries/internal/proto"
	"github.com/ariesworx/aries/internal/store"
	"github.com/ariesworx/aries/internal/telemetry"
	"go.yaml.in/yaml/v3"
)

// DefaultConfigFile is read when it exists and no --config is given.
const DefaultConfigFile = "/etc/aries/ariesd.yaml"

// Settings configure the ariesd commands: serve, stdio, upgrade, and the
// admin commands control init, import-bd and export-bd.
type Settings struct {
	// DSN is the Postgres database, as a postgres:// URL whose
	// search_path names the schema of the one project (public when it
	// names none; store.ParseDSN). It may hold a password, so it never
	// comes from a command-line flag with one; the PG* environment
	// variables and the role's .pgpass are read too.
	DSN string `yaml:"dsn"`
	// Socket is the daemon's unix socket. Default DefaultSocket.
	Socket string `yaml:"socket"`
	// ControlSchema names the control store's schema, in DSN's database
	// and as its role, and makes this a server of many projects (design
	// §15): each project's schema is then named by its row there, and
	// DSN's, Project, Prefix and Account describe the project `ariesd
	// control init` adopts, which the admin commands act on by default.
	// The daemon names the schema of each project `aries admin project
	// add` makes (store.ProjectSchema), so this may not start as those
	// do. Empty, the default, is a server of the one project in DSN.
	ControlSchema string `yaml:"control_schema"`
	// KeysFile is the file a server of many projects writes the control
	// database's keys to, in authorized_keys form, for sshd to read
	// (design §15.2). AuthorizedKeys is the file of that form the
	// operator maintains by hand, which the server reads to say which
	// keys are in both, and never writes. A relative path is under the
	// home directory of the user ariesd runs as ([Settings.KeyFiles]).
	// Defaults DefaultKeysFile and DefaultAuthorizedKeys. A server of
	// one project uses neither.
	KeysFile       string `yaml:"keys_file"`
	AuthorizedKeys string `yaml:"authorized_keys"`
	// Bridge is the path of ariesd in the forced command of each line
	// of KeysFile. Default DefaultBridge.
	Bridge string `yaml:"bridge"`
	// Project is the UUID of the project served.
	Project string `yaml:"project"`
	// Prefix is the issue-ID prefix for generated IDs. Default store.DefaultPrefix.
	Prefix string `yaml:"prefix"`
	// SystemdUnit is the unit `ariesd upgrade` restarts. Empty means
	// upgrade installs the binary and leaves the restart to the admin,
	// unless --restart is given.
	SystemdUnit string `yaml:"systemd_unit"`
	// Releases is where `ariesd upgrade` reads releases from, when the
	// environment's ARIES_RELEASES does not say: a location
	// release.Open accepts. Empty means the default compiled in. Upgrade
	// checks it; the daemon never reads it.
	Releases string `yaml:"releases"`
	// LogLevel is the least severe level logged: debug, info, warn or
	// error. Default info; debug adds a line per request.
	LogLevel string `yaml:"log_level"`
	// LogFormat is text (slog's key=value) or json. Default text.
	LogFormat string `yaml:"log_format"`
	// Admins are the principals who may change issues others hold and
	// force a close (decision D2). They are set here, or in
	// ARIESD_ADMINS as a comma-separated list, and nowhere else:
	// nothing over the protocol reads or changes them. None may be
	// reserved (store.ReservedPrincipals).
	Admins []string `yaml:"admins"`
	// AllowUnsafeAccount opens the store even when its database role is
	// unsafe (store.Options.AllowUnsafeAccount, S-2). Only a command-line
	// flag sets it, and only with --dev (decision D3); no config file or
	// environment variable can.
	AllowUnsafeAccount bool `yaml:"-"`
	// Limits bound what one principal can make the server do; see
	// [Limits]. Zero fields take the defaults.
	Limits Limits `yaml:"limits"`
	// ConflictAge is how long a conflict stays open before digest lists
	// it (design §18.7). Default store.DefaultConflictAge.
	ConflictAge Duration `yaml:"conflict_age"`
	// DueSoon is how far ahead of its due date digest, dates and prime
	// list an issue as due soon (design §20.3), at most
	// store.MaxDueSoon. Default store.DefaultDueSoon. Neither it nor
	// ConflictAge bounds anything, so neither is under limits:.
	DueSoon Duration `yaml:"due_soon"`
	// Account is the project's default account (design §12.1): what an
	// issue reports time and tokens against when neither it nor an
	// ancestor sets one. A code name: lowercase letters, digits and inner
	// hyphens. Default DefaultAccount.
	Account string `yaml:"account"`
	// ClockBehind is what serve does when it finds its clock behind the
	// history of a database it serves by more than ClockBehindTolerance
	// (design §14, decision 37): ClockStop, the default, refuses to
	// start, and ClockWarn starts with a warning that repeats.
	// ClockBehindTolerance defaults to DefaultBehindTolerance.
	ClockBehind          string    `yaml:"clock_behind"`
	ClockBehindTolerance Tolerance `yaml:"clock_behind_tolerance"`
	// ClockStepTolerance is how far elapsed wall-clock time and elapsed
	// monotonic time may differ before the daemon calls it a step of the
	// clock. Default store.DefaultStepTolerance.
	ClockStepTolerance Tolerance `yaml:"clock_step_tolerance"`
	// ClockClientTolerance is how far past the server's clock a time a
	// client supplies, a usage record's, may be. Default
	// store.DefaultClientAhead.
	ClockClientTolerance Tolerance `yaml:"clock_client_tolerance"`
	// AcceptClockBehind lets serve start once although its clock is
	// behind a history no later than this (Config.AcceptHistory). Only a
	// command-line flag sets it: an answer to one refusal must not be
	// left in a file to answer the next.
	AcceptClockBehind time.Time `yaml:"-"`
	// Telemetry is what is Aries's own of the daemon's telemetry
	// (design §16.3); where metrics are pushed is in the standard OTEL_*
	// environment variables, not here.
	Telemetry TelemetrySettings `yaml:"telemetry"`
	// Transports are the transports serve serves (transport.md §11.1):
	// TransportHTTPS, TransportSSH or both. Default [https]: SSH is off
	// until an operator turns it on.
	Transports []string `yaml:"transports"`
	// HTTPS is the HTTPS listener's, which serve needs when Transports
	// names https (CheckServe).
	HTTPS HTTPSSettings `yaml:"https"`
	// Login is how people sign in (docs/design/identity.md §10.1), on a
	// server of many projects.
	Login LoginSettings `yaml:"login"`
	// Workload is how service principals sign in with their platform's
	// token (docs/design/identity.md §7), on a server of many projects.
	Workload WorkloadSettings `yaml:"workload"`
}

// LoginSettings are the login: settings: invitations (slice N1), and
// the providers people sign in at and what a sign-in gives (slice T2,
// transport.md §6 and §8).
type LoginSettings struct {
	// InviteTTL is how long an invitation stays open unused, from
	// store.MinInviteTTL to store.MaxInviteTTL. Default
	// store.DefaultInviteTTL.
	InviteTTL Duration `yaml:"invite_ttl"`
	// TokenLifetime is how long a sign-in lasts (decision 58), from a
	// minute to MaxTokenLifetime, which is at most a week. Defaults
	// DefaultTokenLifetime and MaxTokenLifetime.
	TokenLifetime    Duration `yaml:"token_lifetime"`
	MaxTokenLifetime Duration `yaml:"max_token_lifetime"`
	// FreshAuth bounds how long before a sign-in the person last
	// authenticated at the provider (auth_time), and is sent as max_age;
	// from a minute to an hour. Default DefaultFreshAuth.
	FreshAuth Duration `yaml:"fresh_auth"`
	// MaxTokenAge bounds how old a provider's ID token may be, from its
	// iat. Default 10 minutes.
	MaxTokenAge Duration `yaml:"max_token_age"`
	// Skew is allowed between a provider's clock and the server's, at
	// most five minutes. Default 30 seconds.
	Skew Duration `yaml:"skew"`
	// JWKSRefetch is the least time between two reads of a provider's
	// keys, from its URL or its file. Default a minute.
	JWKSRefetch Duration `yaml:"jwks_refetch"`
	// PruneAfter is how long a sign-in that ended is kept, for `aries
	// admin signin list`, before it is deleted. Default DefaultSignInKeep.
	PruneAfter Duration `yaml:"prune_after"`
	// Providers are where people sign in. None signs no one in; a
	// connection over HTTPS then proves a key.
	Providers []oidc.Settings `yaml:"providers"`
}

// The defaults and bounds of login:'s times.
const (
	defaultMaxTokenAge = 10 * time.Minute
	defaultSkew        = 30 * time.Second
	maxSkew            = 5 * time.Minute
	defaultRefetch     = time.Minute
	minTokenLifetime   = time.Minute
)

// resolveLogin fills l's defaults and refuses a time out of its bounds,
// or two providers of one name. The providers are opened, and their
// files read, by OpenLogin.
func resolveLogin(l LoginSettings) (LoginSettings, error) {
	l.TokenLifetime = cmp.Or(l.TokenLifetime, Duration(DefaultTokenLifetime))
	l.MaxTokenLifetime = cmp.Or(l.MaxTokenLifetime, Duration(MaxTokenLifetime))
	l.FreshAuth = cmp.Or(l.FreshAuth, Duration(DefaultFreshAuth))
	l.MaxTokenAge = cmp.Or(l.MaxTokenAge, Duration(defaultMaxTokenAge))
	l.Skew = cmp.Or(l.Skew, Duration(defaultSkew))
	l.JWKSRefetch = cmp.Or(l.JWKSRefetch, Duration(defaultRefetch))
	l.PruneAfter = cmp.Or(l.PruneAfter, Duration(DefaultSignInKeep))
	const fix = "; fix: set it within that under login: in the config file, or leave it out for the default"
	span := func(d Duration) string {
		if time.Duration(d) < time.Minute {
			return time.Duration(d).String()
		}
		return proto.Span(time.Duration(d))
	}
	switch {
	case time.Duration(l.MaxTokenLifetime) > MaxTokenLifetime:
		return l, fmt.Errorf("login: max_token_lifetime is %s; it can be at most %s%s", span(l.MaxTokenLifetime), proto.Span(MaxTokenLifetime), fix)
	case l.TokenLifetime > l.MaxTokenLifetime:
		return l, fmt.Errorf("login: token_lifetime is %s; it can be at most max_token_lifetime, %s%s", span(l.TokenLifetime), span(l.MaxTokenLifetime), fix)
	case time.Duration(l.TokenLifetime) < minTokenLifetime:
		return l, fmt.Errorf("login: token_lifetime is %s; it must be at least %s%s", span(l.TokenLifetime), proto.Span(minTokenLifetime), fix)
	case time.Duration(l.FreshAuth) < time.Minute || time.Duration(l.FreshAuth) > time.Hour:
		return l, fmt.Errorf("login: fresh_auth is %s; it must be from 1m to 1h%s", span(l.FreshAuth), fix)
	case time.Duration(l.MaxTokenAge) < time.Minute || time.Duration(l.MaxTokenAge) > time.Hour:
		return l, fmt.Errorf("login: max_token_age is %s; it must be from 1m to 1h%s", span(l.MaxTokenAge), fix)
	case time.Duration(l.Skew) > maxSkew:
		return l, fmt.Errorf("login: skew is %s; it can be at most %s%s", span(l.Skew), proto.Span(maxSkew), fix)
	case time.Duration(l.JWKSRefetch) < 10*time.Second:
		return l, fmt.Errorf("login: jwks_refetch is %s; it must be at least 10s%s", span(l.JWKSRefetch), fix)
	case time.Duration(l.PruneAfter) < time.Minute || time.Duration(l.PruneAfter) > 30*24*time.Hour:
		return l, fmt.Errorf("login: prune_after is %s; it must be from 1m to 30d%s", span(l.PruneAfter), fix)
	}
	seen := map[string]bool{}
	for _, p := range l.Providers {
		if seen[p.Name] {
			return l, fmt.Errorf("login: two providers are named %s; fix: give each provider a name of its own under login: providers", p.Name)
		}
		seen[p.Name] = true
	}
	return l, nil
}

// OpenLogin opens the providers of l, which ResolveSettings has read,
// reading their files (a client secret, a CA, a key set) and fetching
// nothing, and returns what the server signs people in with; nil when l
// names no provider. now is the store's clock (rule 19).
func OpenLogin(l LoginSettings, now func() time.Time) (*Login, error) {
	return openLogin(l, now, appArmorProfile())
}

// openLogin is OpenLogin under the AppArmor profile profile, or none
// when it is "".
func openLogin(l LoginSettings, now func() time.Time, profile string) (*Login, error) {
	if len(l.Providers) == 0 {
		return nil, nil
	}
	out := &Login{TokenLifetime: time.Duration(l.TokenLifetime), FreshAuth: time.Duration(l.FreshAuth), PruneAfter: time.Duration(l.PruneAfter)}
	for _, ps := range l.Providers {
		p, err := oidc.Open(ps, oidc.Options{Now: now, Skew: time.Duration(l.Skew), MaxAge: time.Duration(l.MaxTokenAge),
			Refetch: time.Duration(l.JWKSRefetch)})
		if fix := providerFileFix(err, profile); fix != "" {
			return nil, fmt.Errorf("%w%s", err, fix)
		}
		if err != nil {
			return nil, fmt.Errorf("%w; fix: correct the provider under login: providers in the config file (docs/server.md, Sign-in)", err)
		}
		// A bound identity is matched by (issuer, subject) alone, one
		// person per account at an issuer, so a second provider of an
		// issuer would sign in the first's people, past its own require
		// and domains.
		if i := slices.IndexFunc(out.Providers, func(o *oidc.Provider) bool { return o.Issuer() == p.Issuer() }); i >= 0 {
			return nil, fmt.Errorf("login: providers %s and %s have one issuer, %s; fix: keep one provider for each issuer under login: providers",
				out.Providers[i].Name(), p.Name(), p.Issuer())
		}
		out.Providers = append(out.Providers, p)
	}
	return out, nil
}

// HTTPSSettings are the https: settings (transport.md §4.2).
type HTTPSSettings struct {
	// Listen is the listener's address, HOST:PORT; ":443" listens on
	// every address.
	Listen string `yaml:"listen"`
	// PublicURLs are the server's URLs as clients have them, in
	// server.url: what a key proof names, checked by the daemon in place
	// of a request's Host.
	PublicURLs []string `yaml:"public_urls"`
	// TLS is where the listener's certificate comes from.
	TLS TLSSettings `yaml:"tls"`
	// RefuseKeyProofV1 refuses the key proof of protocol 8's aries
	// (v0.5.0's), which binds nothing of the TLS session: off by
	// default while ProtoMin is 8, so that those clients keep working.
	RefuseKeyProofV1 bool `yaml:"refuse_key_proof_v1"`
}

// TLSSettings are https: tls:. Mode is acme, files or proxy: a
// certificate from an ACME server, one from the operator's files, or
// plain HTTP behind the operator's proxy, which terminates TLS. Only the
// mode's own block is read. MinVersion is "1.2", the default, or "1.3".
type TLSSettings struct {
	Mode       string        `yaml:"mode"`
	MinVersion string        `yaml:"min_version"`
	ACME       ACMESettings  `yaml:"acme"`
	Files      TLSFiles      `yaml:"files"`
	Proxy      ProxySettings `yaml:"proxy"`
}

// ACMESettings are https: tls: acme:, for golang.org/x/crypto's
// autocert: the names to get a certificate for, an address the ACME
// server may write to, the directory the account key and certificates
// are kept in (0700, made if missing), and the ACME server's directory
// URL: empty for Let's Encrypt, or an internal one's (step-ca, say),
// with which ACME needs no internet. The challenge is TLS-ALPN-01, on
// the listener's own port, which must be 443 to the ACME server.
type ACMESettings struct {
	Domains   []string `yaml:"domains"`
	Email     string   `yaml:"email"`
	Cache     string   `yaml:"cache"`
	Directory string   `yaml:"directory"`
}

// TLSFiles are https: tls: files:: the certificate chain and its key, in
// PEM. The key must be readable by no one but its owner. Both are read
// again when either changes.
type TLSFiles struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// ProxySettings are https: tls: proxy:: the addresses, or networks in
// CIDR form, of the proxy that terminates TLS. A connection from any
// other is closed unanswered.
type ProxySettings struct {
	Trusted []string `yaml:"trusted"`
}

// The TLS modes of https: tls: mode:.
const (
	TLSModeACME  = "acme"
	TLSModeFiles = "files"
	TLSModeProxy = "proxy"
)

// DefaultACMECache is where ACME's account key and certificates are kept
// when https: tls: acme: cache: is not set.
const DefaultACMECache = "/var/lib/aries/acme"

// HTTPSOn reports whether serve serves HTTPS.
func (s Settings) HTTPSOn() bool { return slices.Contains(s.Transports, TransportHTTPS) }

// SSH reports whether serve serves SSH's bridge.
func (s Settings) SSH() bool { return slices.Contains(s.Transports, TransportSSH) }

// Trusted returns the proxy's addresses as prefixes: an address alone
// is a prefix of its own length. An IPv4 address or network written in
// IPv6's form (::ffff:10.0.0.0/104) is returned as IPv4's, since a
// peer's address is compared unmapped (peerAddr), and would otherwise
// match no peer.
func (h HTTPSSettings) Trusted() ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(h.TLS.Proxy.Trusted))
	for _, t := range h.TLS.Proxy.Trusted {
		p, err := netip.ParsePrefix(t)
		if err != nil {
			a, aerr := netip.ParseAddr(t)
			if aerr != nil {
				return nil, fmt.Errorf("https: tls: proxy: trusted: %q is not an address or a network in CIDR form; fix: list the proxy's addresses, such as 10.0.0.5 or 10.0.0.0/24", t)
			}
			p = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// resolveTransports checks transports: and https:, filling the
// defaults: transports [https], TLS 1.2 at least, and the ACME cache.
// What must be there for serve to start, it leaves to CheckServe, so
// that the admin commands, which serve nothing, need none of it.
func (s *Settings) resolveTransports() error {
	if s.Transports == nil {
		s.Transports = []string{TransportHTTPS}
	}
	if len(s.Transports) == 0 {
		return errors.New("transports: names none; at least one must be on; fix: set transports: [https], [https, ssh] or [ssh]")
	}
	for i, t := range s.Transports {
		switch {
		case t != TransportHTTPS && t != TransportSSH:
			return fmt.Errorf("transports: %q is not https or ssh; fix: set transports: [https], [https, ssh] or [ssh]", t)
		case slices.Contains(s.Transports[:i], t):
			return fmt.Errorf("transports: names %s twice; fix: name each once", t)
		}
	}
	h := &s.HTTPS
	if h.Listen != "" {
		if _, _, err := net.SplitHostPort(h.Listen); err != nil {
			return fmt.Errorf("https: listen: %q is not HOST:PORT; fix: set it to such as \":443\" or 127.0.0.1:8443", h.Listen)
		}
	}
	for i, u := range h.PublicURLs {
		n, err := keyproof.NormalizeURL(u)
		if err != nil {
			return fmt.Errorf("https: public_urls: %w; fix: list the https:// URLs clients use, such as https://aries.example.com", err)
		}
		h.PublicURLs[i] = n
	}
	tl := &h.TLS
	switch tl.MinVersion {
	case "":
		tl.MinVersion = "1.2"
	case "1.2", "1.3":
	default:
		return fmt.Errorf("https: tls: min_version: %q is not 1.2 or 1.3; fix: leave it out for 1.2, or set \"1.3\"", tl.MinVersion)
	}
	switch tl.Mode {
	case "":
	case TLSModeFiles:
		for _, f := range []struct{ name, path string }{{"cert", tl.Files.Cert}, {"key", tl.Files.Key}} {
			if !filepath.IsAbs(f.path) {
				return fmt.Errorf("https: tls: files: %s: %q is not an absolute path; fix: set it to the PEM file's absolute path, such as /etc/aries/tls/%s.pem", f.name, f.path, f.name)
			}
		}
	case TLSModeACME:
		a := &tl.ACME
		if len(a.Domains) == 0 {
			return errors.New("https: tls: acme: domains: names none; fix: list the server's DNS names, those of public_urls")
		}
		for _, d := range a.Domains {
			if !hostPattern.MatchString(d) {
				return fmt.Errorf("https: tls: acme: domains: %q is not a DNS name; fix: list the server's DNS names", d)
			}
		}
		a.Cache = cmp.Or(a.Cache, DefaultACMECache)
		if !filepath.IsAbs(a.Cache) {
			return fmt.Errorf("https: tls: acme: cache: %q is not an absolute path; fix: leave it out for %s", a.Cache, DefaultACMECache)
		}
		if a.Directory != "" {
			if u, err := url.Parse(a.Directory); err != nil || u.Scheme != "https" || u.Host == "" {
				return fmt.Errorf("https: tls: acme: directory: %q is not an https:// URL; fix: leave it out for Let's Encrypt, or give the ACME server's directory URL", a.Directory)
			}
		}
	case TLSModeProxy:
		if len(tl.Proxy.Trusted) == 0 {
			return errors.New("https: tls: proxy: trusted: names no one; fix: list the addresses of the proxy that terminates TLS, which alone may connect")
		}
		if _, err := h.Trusted(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("https: tls: mode: %q is not acme, files or proxy; fix: set it to one of them", tl.Mode)
	}
	return nil
}

// hostPattern is a DNS name ACME may get a certificate for.
var hostPattern = lazyregexp.New(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// CheckServe refuses settings serve cannot start with: HTTPS on without
// a listen address, a URL or a TLS mode. ResolveSettings has checked the
// shape of what is set.
func (s Settings) CheckServe() error {
	if len(s.Login.Providers) > 0 {
		switch {
		case s.ControlSchema == "":
			return errors.New("login: providers are set on a server of one project; sign-in needs a server of many (control_schema), whose identities say who signs in; fix: set control_schema (docs/server.md, Serving many projects), or remove login: providers")
		case !s.HTTPSOn():
			return errors.New("login: providers are set and HTTPS is off; a sign-in's tokens come over HTTPS; fix: add https to transports: and set https:, or remove login: providers")
		}
	}
	if err := s.checkWorkload(); err != nil {
		return err
	}
	if !s.HTTPSOn() {
		return nil
	}
	const fix = "fix: set https: listen:, public_urls: and tls: mode: in ariesd's config (docs/server.md, The HTTPS listener), or set transports: [ssh] to serve SSH alone"
	h := s.HTTPS
	switch {
	case h.Listen == "" && len(h.PublicURLs) == 0 && h.TLS.Mode == "":
		return errors.New("transports: names https, and https: is not set; " + fix)
	case h.Listen == "":
		return errors.New("https: listen: is not set; " + fix)
	case len(h.PublicURLs) == 0:
		return errors.New("https: public_urls: names no URL; " + fix)
	case h.TLS.Mode == "":
		return errors.New("https: tls: mode: is not set; " + fix)
	}
	return nil
}

// Tolerance is a YAML duration that must be more than zero: a tolerance
// of nothing would call every reading of the clock wrong. Left out, it
// takes its default.
type Tolerance time.Duration

// UnmarshalText reads a duration in proto.ParseDuration's form, which
// refuses one that is not positive.
func (t *Tolerance) UnmarshalText(b []byte) error {
	v, err := proto.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("a clock tolerance of %q is not a duration of more than zero; fix: set it to one such as 5s or 1m, or leave it out for the default", b)
	}
	*t = Tolerance(v)
	return nil
}

// TelemetrySettings are the telemetry: settings of ariesd's config.
type TelemetrySettings struct {
	// Listen is the loopback address, HOST:PORT, of the listener that
	// serves /metrics, /healthz and /readyz. Empty, the default, opens no
	// listener. An address that is not loopback is refused: the listener
	// has no authentication.
	Listen string `yaml:"listen"`
	// MaxRequests caps the requests the listener serves at once; one
	// more is answered 503. Twice it caps the connections the listener
	// holds open. Default telemetry.DefaultRequests.
	MaxRequests int `yaml:"max_requests"`
	// PerProject says whether metrics name the project on the
	// instruments that have that attribute. Nil, the default, is on;
	// false leaves the project off every instrument, for a server with
	// many projects.
	PerProject *bool `yaml:"per_project"`
	// Principals is how a person appears on a span and on a log record
	// exported over OTLP (design §16.4, decision 32):
	// telemetry.PrincipalsPseudonym, the default, a stable stand-in for
	// the principal's name and for the session's id; PrincipalsName, the
	// names themselves; or PrincipalsOmit, nothing. It does not change
	// the daemon's log on stderr, nor the audit stream there.
	Principals string `yaml:"principals"`
	// PrincipalsKey is the file that holds the key the stand-ins are
	// made with. ariesd makes it, mode 0600, the first time it exports
	// traces with stand-ins. A relative path is under the home directory
	// of the user ariesd runs as ([Settings.PrincipalsKey]). Default
	// DefaultPrincipalsKey.
	PrincipalsKey string `yaml:"principals_key"`
	// AuditLog says whether each event of the control store's log that
	// changes who has access to what is also written as a log line with
	// audit=true (design §16.4, decision 30). Nil, the default, is on.
	// The event log is the record whatever this says.
	AuditLog *bool `yaml:"audit_log"`
}

// DefaultPrincipalsKey is where the stand-in key is kept, under the home
// directory of the user ariesd runs as: a directory of its own, so
// that the unit can let the daemon write there and nowhere else in the
// home, where authorized_keys is.
const DefaultPrincipalsKey = "telemetry/principals.key"

// ByProject reports whether metrics name the project: unless
// per_project is false.
func (t TelemetrySettings) ByProject() bool { return t.PerProject == nil || *t.PerProject }

// Audit reports whether the audit stream is written: unless audit_log
// is false.
func (t TelemetrySettings) Audit() bool { return t.AuditLog == nil || *t.AuditLog }

// DefaultAccount is the account an issue reports against when nothing
// sets one.
const DefaultAccount = "internal"

// UnitPattern is what a systemd unit name may look like. It cannot start
// with "-", so it never reaches systemctl as an option.
var UnitPattern = lazyregexp.New(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,127}$`)

// Environment variables read by ResolveSettings.
const (
	EnvDSN     = "ARIESD_DSN"
	EnvSocket  = "ARIESD_SOCKET"
	EnvProject = "ARIESD_PROJECT"
	EnvConfig  = "ARIESD_CONFIG"
	// EnvLogLevel and EnvLogFormat set LogLevel and LogFormat.
	EnvLogLevel  = "ARIESD_LOG_LEVEL"
	EnvLogFormat = "ARIESD_LOG_FORMAT"
	// EnvAdmins sets Admins, comma-separated.
	EnvAdmins = "ARIESD_ADMINS"
	// EnvControlSchema sets ControlSchema.
	EnvControlSchema = "ARIESD_CONTROL_SCHEMA"
	// EnvKeysFile, EnvAuthorizedKeys and EnvBridge set KeysFile,
	// AuthorizedKeys and Bridge.
	EnvKeysFile       = "ARIESD_KEYS_FILE"
	EnvAuthorizedKeys = "ARIESD_AUTHORIZED_KEYS"
	EnvBridge         = "ARIESD_BRIDGE"
	// EnvClockBehind sets ClockBehind.
	EnvClockBehind = "ARIESD_CLOCK_BEHIND"
	// EnvTelemetryListen sets Telemetry.Listen.
	EnvTelemetryListen = "ARIESD_TELEMETRY_LISTEN"
)

// ResolveSettings merges, highest precedence first: flags, the environment,
// the config file, and defaults. configPath "" means ARIESD_CONFIG, else
// DefaultConfigFile if it exists; a named file must exist. Limits come
// from the file only, Admins from the file unless ARIESD_ADMINS replaces
// them, Telemetry from the file with its listen address from
// ARIESD_TELEMETRY_LISTEN when that is set, and AllowUnsafeAccount from
// flags only. It refuses a password in
// flags.DSN, which the process list would show; a config file with an
// unknown key, or with a password and any access for group or others; a
// password file or client certificate key the DSN would read that group
// or others may read (checkDatabaseFiles); an
// admin name that is invalid or reserved; an invalid limit; an account
// that is not a code name; a control schema that is not a schema name,
// is the DSN's own, or starts as a project's does; a systemd
// unit that is not a unit name; a bridge path that would need quoting in
// a forced command; a keys file that is the authorized_keys file; a
// telemetry listen address that is not HOST:PORT or is an IP address
// that is not loopback (a host name is resolved, and checked, when the
// daemon starts); a telemetry principals that is not pseudonym, name or
// omit; a clock_behind that is not stop or warn; a clock tolerance out of its
// range; and an unknown log level or format. The clock tolerances come
// from the file only.
func ResolveSettings(flags Settings, configPath string, getenv func(string) string) (Settings, error) {
	if flags.DSN != "" {
		d, err := store.ParseDSN(flags.DSN)
		if err != nil {
			return Settings{}, fmt.Errorf("--dsn: %w", err)
		}
		if d.Password {
			return Settings{}, fmt.Errorf("--dsn holds a password, which would show in the process list; fix: set %s or dsn: in the config file instead", EnvDSN)
		}
	}
	file, err := configFile(configPath, getenv)
	if err != nil {
		return Settings{}, err
	}
	env := Settings{DSN: getenv(EnvDSN), Socket: getenv(EnvSocket), Project: getenv(EnvProject),
		LogLevel: getenv(EnvLogLevel), LogFormat: getenv(EnvLogFormat), ControlSchema: getenv(EnvControlSchema),
		KeysFile: getenv(EnvKeysFile), AuthorizedKeys: getenv(EnvAuthorizedKeys), Bridge: getenv(EnvBridge),
		ClockBehind: getenv(EnvClockBehind)}
	out := Settings{Socket: DefaultSocket, Prefix: store.DefaultPrefix, LogLevel: "info", LogFormat: "text", Account: DefaultAccount,
		KeysFile: DefaultKeysFile, AuthorizedKeys: DefaultAuthorizedKeys, Bridge: DefaultBridge, ClockBehind: ClockStop}
	for _, s := range []Settings{file, env, flags} {
		for _, f := range []struct {
			dst *string
			v   string
		}{
			{&out.DSN, s.DSN}, {&out.Socket, s.Socket}, {&out.Project, s.Project},
			{&out.Prefix, s.Prefix}, {&out.SystemdUnit, s.SystemdUnit},
			{&out.Releases, s.Releases},
			{&out.LogLevel, s.LogLevel}, {&out.LogFormat, s.LogFormat}, {&out.Account, s.Account},
			{&out.ControlSchema, s.ControlSchema}, {&out.KeysFile, s.KeysFile}, {&out.AuthorizedKeys, s.AuthorizedKeys},
			{&out.Bridge, s.Bridge}, {&out.ClockBehind, s.ClockBehind},
		} {
			if f.v != "" {
				*f.dst = f.v
			}
		}
	}
	out.AllowUnsafeAccount = flags.AllowUnsafeAccount
	out.AcceptClockBehind = flags.AcceptClockBehind
	if err := out.resolveClock(file); err != nil {
		return Settings{}, err
	}
	out.Transports, out.HTTPS = file.Transports, file.HTTPS
	if err := out.resolveTransports(); err != nil {
		return Settings{}, err
	}
	out.Limits = file.Limits
	out.ConflictAge = cmp.Or(file.ConflictAge, Duration(store.DefaultConflictAge))
	out.DueSoon = cmp.Or(file.DueSoon, Duration(store.DefaultDueSoon))
	if d := time.Duration(out.DueSoon); d > store.MaxDueSoon {
		return Settings{}, fmt.Errorf("due_soon is %s; it can be at most %s; fix: set it within that in the config file, or leave it out for the default, %s",
			proto.Span(d), proto.Span(store.MaxDueSoon), proto.Span(store.DefaultDueSoon))
	}
	out.Login, err = resolveLogin(file.Login)
	if err != nil {
		return Settings{}, err
	}
	if out.Workload, err = resolveWorkload(file.Workload); err != nil {
		return Settings{}, err
	}
	out.Login.InviteTTL = cmp.Or(out.Login.InviteTTL, Duration(store.DefaultInviteTTL))
	if d := time.Duration(out.Login.InviteTTL); d < store.MinInviteTTL || d > store.MaxInviteTTL {
		return Settings{}, fmt.Errorf("login: invite_ttl is %s; it must be from %s to %s; fix: set it within that in the config file, or leave it out for the default, %s",
			proto.Span(d), proto.Span(store.MinInviteTTL), proto.Span(store.MaxInviteTTL), proto.Span(store.DefaultInviteTTL))
	}
	out.Telemetry = file.Telemetry
	if out.Telemetry.MaxRequests < 0 {
		return Settings{}, fmt.Errorf("telemetry: max_requests is %d; fix: give a positive number, or leave it out for the default, %d",
			out.Telemetry.MaxRequests, telemetry.DefaultRequests)
	}
	out.Telemetry.MaxRequests = cmp.Or(out.Telemetry.MaxRequests, telemetry.DefaultRequests)
	if v := getenv(EnvTelemetryListen); v != "" {
		out.Telemetry.Listen = v
	}
	if out.Telemetry.Listen != "" {
		if err := telemetry.CheckAddr(out.Telemetry.Listen); err != nil {
			return Settings{}, err
		}
	}
	if out.Telemetry.Principals == "" {
		out.Telemetry.Principals = telemetry.PrincipalsPseudonym
	}
	if !telemetry.ValidPrincipals(out.Telemetry.Principals) {
		return Settings{}, fmt.Errorf("telemetry: principals: %q is not %s, %s or %s; fix: set it to one of them, or leave it out for %s, a stable stand-in for each name",
			out.Telemetry.Principals, telemetry.PrincipalsPseudonym, telemetry.PrincipalsName, telemetry.PrincipalsOmit, telemetry.PrincipalsPseudonym)
	}
	if out.Telemetry.PrincipalsKey == "" {
		out.Telemetry.PrincipalsKey = DefaultPrincipalsKey
	}
	out.Admins = file.Admins
	if v := getenv(EnvAdmins); v != "" {
		out.Admins = nil
		for a := range strings.SplitSeq(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				out.Admins = append(out.Admins, a)
			}
		}
	}
	for _, a := range out.Admins {
		if !PrincipalPattern.MatchString(a) || store.Reserved(a) {
			return Settings{}, fmt.Errorf("admin %q is not a principal name, or is reserved for the server; fix: list the admins' principal names under admins: or in %s", a, EnvAdmins)
		}
	}
	if err := out.Limits.Validate(); err != nil {
		return Settings{}, err
	}
	out.Limits = out.Limits.WithDefaults()
	if !store.ValidAccount(out.Account) {
		return Settings{}, fmt.Errorf("account %q is not a code name; fix: set account: to 1-64 lowercase letters, digits and inner hyphens, such as %s", out.Account, DefaultAccount)
	}
	if err := out.checkControlSchema(); err != nil {
		return Settings{}, err
	}
	if err := checkDatabaseFiles(out.DSN, getenv); err != nil {
		return Settings{}, err
	}
	if !ValidBridge(out.Bridge) {
		return Settings{}, fmt.Errorf("bridge %q cannot be written into a forced command; fix: set bridge: or %s to the absolute path of ariesd, of letters, digits and ._+-, such as %s",
			out.Bridge, EnvBridge, DefaultBridge)
	}
	if filepath.Clean(out.KeysFile) == filepath.Clean(out.AuthorizedKeys) {
		return Settings{}, fmt.Errorf("keys_file %q is the authorized_keys file, which ariesd reads and never writes; fix: set keys_file: or %s to a file of its own beside it, such as %s",
			out.KeysFile, EnvKeysFile, DefaultKeysFile)
	}
	if out.SystemdUnit != "" && !UnitPattern.MatchString(out.SystemdUnit) {
		return Settings{}, fmt.Errorf("systemd_unit %q is not a unit name; fix: set it to the service's name, for example ariesd.service", out.SystemdUnit)
	}
	if _, err := NewLogger(io.Discard, out.LogLevel, out.LogFormat); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// resolveClock takes the clock tolerances from file, each with its
// default, and refuses a clock_behind that is not stop or warn and a
// tolerance past its bound. A bound is refused, not warned about: a
// tolerance of days turns its check off without saying so, and
// clock_behind: warn is the way to start whatever the clock says.
func (s *Settings) resolveClock(file Settings) error {
	if s.ClockBehind != ClockStop && s.ClockBehind != ClockWarn {
		return fmt.Errorf("clock_behind %q is not %s or %s; fix: set clock_behind: or %s to %s, to refuse to start when the clock is behind the server's own history, or to %s, to start with a warning",
			s.ClockBehind, ClockStop, ClockWarn, EnvClockBehind, ClockStop, ClockWarn)
	}
	for _, f := range []struct {
		name     string
		dst      *Tolerance
		v        Tolerance
		def, max time.Duration
	}{
		{"clock_behind_tolerance", &s.ClockBehindTolerance, file.ClockBehindTolerance, DefaultBehindTolerance, maxBehindTolerance},
		{"clock_step_tolerance", &s.ClockStepTolerance, file.ClockStepTolerance, store.DefaultStepTolerance, maxStepTolerance},
		{"clock_client_tolerance", &s.ClockClientTolerance, file.ClockClientTolerance, store.DefaultClientAhead, store.MaxClientAhead},
	} {
		*f.dst = cmp.Or(f.v, Tolerance(f.def))
		if d := time.Duration(*f.dst); d < minClockTolerance || d > f.max {
			return fmt.Errorf("%s is %s; it must be from %s to %s; fix: set it within that range in the config file, or leave it out for the default, %s",
				f.name, d, minClockTolerance, f.max, f.def)
		}
	}
	return nil
}

// checkControlSchema refuses a control schema that is not a schema name,
// that is the schema the DSN names, which holds a project, or that
// starts as the schemas the daemon names for projects do.
func (s Settings) checkControlSchema() error {
	if s.ControlSchema == "" {
		return nil
	}
	switch d, err := store.ParseDSN(s.DSN); {
	case !store.ValidSchemaName(s.ControlSchema):
		return fmt.Errorf("control_schema %q is not a schema name; fix: set control_schema: or %s to a lowercase letter, then lowercase letters, digits and underscores, not starting pg_, such as aries_control",
			s.ControlSchema, EnvControlSchema)
	case err == nil && d.Schema == s.ControlSchema:
		return fmt.Errorf("control_schema %q is the schema the DSN names, which holds a project; fix: set control_schema: to a schema of its own, such as aries_control",
			s.ControlSchema)
	case strings.HasPrefix(s.ControlSchema, store.ProjectSchemaPrefix):
		return fmt.Errorf("control_schema %q starts with %s, as the schemas ariesd makes for projects do; fix: set control_schema: to a schema of its own, such as aries_control",
			s.ControlSchema, store.ProjectSchemaPrefix)
	}
	return nil
}

// KeyFiles returns the absolute paths of the keys file and of the
// hand-maintained authorized_keys file: a relative setting is under
// home, the home directory of the user ariesd runs as, whose
// authorized_keys sshd reads.
func (s Settings) KeyFiles(home string) (keysFile, authorizedKeys string) {
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(home, p)
	}
	return abs(s.KeysFile), abs(s.AuthorizedKeys)
}

// PrincipalsKey returns the absolute path of the stand-in key's file
// (telemetry: principals_key:): a relative setting is under home, the
// home directory of the user ariesd runs as.
func (s Settings) PrincipalsKey(home string) string {
	if p := s.Telemetry.PrincipalsKey; filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(home, s.Telemetry.PrincipalsKey)
}

// ControlDSN returns the DSN of the control store's schema (ControlSchema),
// which must be set.
func (s Settings) ControlDSN() (string, error) { return store.SchemaDSN(s.DSN, s.ControlSchema) }

// NewLogger returns ariesd's logger, writing to w at level (debug,
// info, warn or error) in format (text or json). Its handler is a
// [*logs.Handler]: a line logged with a request's context has the
// request's id and the ids of its span, and the lines can be given to a
// second handler ([logs.Handler.Tap]), which is how they are exported.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil || !slices.Contains([]string{"debug", "info", "warn", "error"}, level) {
		return nil, fmt.Errorf("log level %q is not debug, info, warn or error; fix: set log_level: or %s to one of them", level, EnvLogLevel)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(logs.NewHandler(slog.NewTextHandler(w, opts), LogIDs)), nil
	case "json":
		return slog.New(logs.NewHandler(slog.NewJSONHandler(w, opts), LogIDs)), nil
	}
	return nil, fmt.Errorf("log format %q is not text or json; fix: set log_format: or %s to text or json", format, EnvLogFormat)
}

// ResolveSocket returns the daemon's socket for `ariesd stdio`, from
// where ResolveSettings takes it: flag, then $ARIESD_SOCKET, then
// socket: in the config file, then DefaultSocket. It reads the file as
// ResolveSettings does, and refuses what that refuses of the file itself
// (an unknown key, a password in a file others can read), but checks no
// other setting. The bridge uses none of them, and sshd starts one for
// every connection: checking them compiled regular expressions that each
// bridge then held (docs/design/profile-ariesd.md, proposal 2).
func ResolveSocket(flag, configPath string, getenv func(string) string) (string, error) {
	file, err := configFile(configPath, getenv)
	if err != nil {
		return "", err
	}
	for _, s := range []string{flag, getenv(EnvSocket), file.Socket} {
		if s != "" {
			return s, nil
		}
	}
	return DefaultSocket, nil
}

// configFile reads the config file at configPath, or else at
// $ARIESD_CONFIG, either of which must exist, or else at
// DefaultConfigFile, which may not.
func configFile(configPath string, getenv func(string) string) (Settings, error) {
	required := true
	if configPath == "" {
		configPath = getenv(EnvConfig)
	}
	if configPath == "" {
		configPath, required = DefaultConfigFile, false
	}
	return loadSettings(configPath, required)
}

// loadSettings reads the config file at path, refusing unknown keys. A
// missing file is not an error unless required. A file whose DSN holds a
// password is refused if its group or others have any access to it.
func loadSettings(path string, required bool) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path) //nolint:gosec // the admin names the config file
	if errors.Is(err, fs.ErrNotExist) && !required {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return s, fmt.Errorf("config %s: %w", path, err)
	}
	if s.DSN != "" {
		if d, err := store.ParseDSN(s.DSN); err == nil && d.Password {
			fi, err := os.Stat(path)
			if err != nil {
				return s, fmt.Errorf("config: %w", err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				return s, fmt.Errorf("config %s holds a database password but is mode %04o; fix: chmod 600 %s", path, fi.Mode().Perm(), path)
			}
		}
	}
	return s, nil
}
