package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"runtime"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/release"
)

// releasesURL is where a person installs a release by hand.
const releasesURL = "https://github.com/" + release.Repo + "/releases"

// Upgrader is what `sfx upgrade` reads releases from and replaces. The zero
// value uses the source $STARFIX_RELEASES names, or release.DefaultSource,
// the built-in release keys and this executable.
type Upgrader struct {
	// Releases reads and verifies releases. Default: release.Open on
	// $STARFIX_RELEASES, verifying with Verifier.
	Releases *release.Client
	// Verifier checks a release's signature when Releases is not set.
	// Default the release keys built into this binary.
	Verifier release.Verifier
	// Exe is the binary to replace, symlinks resolved. Default this
	// executable.
	Exe string
	// Check runs the staged binary before it is installed. Default runs
	// `<new> version` and wants the release's tag.
	Check func(ctx context.Context, path string, tag string) error
	// GOOS and GOARCH select the archive. Default this build's.
	GOOS, GOARCH string
}

// upgrader returns Env.Upgrader, or the zero Upgrader, with its defaults
// filled in, except Exe and Releases, which exe and releases resolve when
// they are needed: a rollback reads no release.
func (r *runner) upgrader() Upgrader {
	var u Upgrader
	if r.env.Upgrader != nil {
		u = *r.env.Upgrader
	}
	if u.Verifier == nil {
		u.Verifier = release.Ed25519{}
	}
	if u.Check == nil {
		u.Check = func(ctx context.Context, path, tag string) error { return release.VersionCheck(tag)(ctx, path) }
	}
	if u.GOOS == "" {
		u.GOOS = runtime.GOOS
	}
	if u.GOARCH == "" {
		u.GOARCH = runtime.GOARCH
	}
	return u
}

// releases returns u.Releases, or the client for the source that
// $STARFIX_RELEASES names, which is release.DefaultSource when unset.
func (r *runner) releases(u Upgrader) (*release.Client, error) {
	if u.Releases != nil {
		return u.Releases, nil
	}
	c, err := release.Open(r.env.Getenv(release.EnvReleases), u.Verifier)
	if err != nil {
		return nil, proto.Errf(proto.CodeInvalid,
			"unset "+release.EnvReleases+" to read releases from the default, or set it to where your mirror is (docs/install.md)",
			"$"+release.EnvReleases+": "+err.Error())
	}
	return c, nil
}

// exe returns the binary to replace: u.Exe, or this executable with
// symlinks resolved.
func (u Upgrader) exe() (string, error) {
	if u.Exe != "" {
		return u.Exe, nil
	}
	p, err := release.Executable()
	if err != nil {
		return "", proto.Errf(proto.CodeUnavailable, "reinstall sfx from "+releasesURL, err.Error())
	}
	return p, nil
}

func cmdUpgrade(ctx context.Context, r *runner, args []string) error {
	const usage = "upgrade [--check] [--rollback]"
	fs := r.newFlags("upgrade")
	check := fs.Bool("check", false, "print whether an upgrade is available; change nothing")
	rollback := fs.Bool("rollback", false, "restore the binary the last upgrade replaced")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	switch {
	case len(pos) > 0:
		return usagef(usage, "upgrade takes no arguments")
	case *check && *rollback:
		return usagef(usage, "--check and --rollback cannot be combined")
	}
	u := r.upgrader()
	if *rollback {
		return r.rollback(u)
	}

	cur := r.env.Version
	rels, err := r.releases(u)
	if err != nil {
		return err
	}
	rel, err := rels.Latest(ctx)
	if err != nil {
		return proto.Errf(proto.CodeUnavailable,
			"check that this machine can reach "+rels.Location()+" (HTTPS_PROXY is honored) and retry",
			"cannot read the latest starfix release: "+err.Error())
	}
	cmp, ok := proto.CompareVersions(cur, rel.Tag)
	newer := ok && cmp < 0
	if *check {
		state := "up to date"
		switch {
		case !ok:
			state = "dev build; install a release to upgrade"
		case newer:
			state = "run sfx upgrade"
		}
		if r.json {
			r.emit(map[string]any{"current": cur, "latest": rel.Tag, "upgrade": newer})
		} else {
			_, _ = fmt.Fprintf(r.env.Stdout, "starfix %s, latest %s: %s\n", cur, rel.Tag, state)
		}
		return nil
	}
	if !ok {
		return proto.Errf(proto.CodeInvalid, "install a release build from "+releasesURL,
			fmt.Sprintf("this sfx is a development build (%s), which never upgrades itself", cur))
	}
	if !newer {
		if r.json {
			r.emit(map[string]any{"current": cur, "latest": rel.Tag, "upgraded": false})
		} else {
			_, _ = fmt.Fprintf(r.env.Stdout, "starfix %s is up to date\n", cur)
		}
		return nil
	}

	bin, err := rels.Fetch(ctx, rel, "sfx", u.GOOS, u.GOARCH)
	if err != nil {
		return fetchError(rel.Tag, err)
	}
	exe, err := u.exe()
	if err != nil {
		return err
	}
	s := release.Swap{Exe: exe, GOOS: u.GOOS, Check: func(ctx context.Context, p string) error {
		return u.Check(ctx, p, rel.Tag)
	}}
	if err := s.Install(ctx, bin); err != nil {
		return installError(exe, err)
	}
	if r.json {
		r.emit(map[string]any{"current": rel.Tag, "previous": cur, "upgraded": true, "rollback": s.Prev()})
	} else {
		_, _ = fmt.Fprintf(r.env.Stdout, "starfix upgraded %s -> %s; `sfx upgrade --rollback` restores %s\n", cur, rel.Tag, cur)
	}
	return nil
}

// rollback restores the binary the last upgrade replaced.
func (r *runner) rollback(u Upgrader) error {
	exe, err := u.exe()
	if err != nil {
		return err
	}
	s := release.Swap{Exe: exe, GOOS: u.GOOS}
	if err := s.Rollback(); err != nil {
		if errors.Is(err, release.ErrNoPrevious) {
			return proto.Errf(proto.CodeNotFound, "nothing to roll back (already rolled back, or never upgraded); install the release you want from "+releasesURL,
				err.Error())
		}
		return installError(exe, err)
	}
	if r.json {
		r.emit(map[string]any{"rolled_back": true})
	} else {
		_, _ = fmt.Fprintf(r.env.Stdout, "restored the previous sfx at %s\n", exe)
	}
	return nil
}

// fetchError types a failed download or verification.
func fetchError(tag string, err error) error {
	switch {
	case errors.Is(err, release.ErrNoKey):
		return proto.Errf(proto.CodeInvalid, "nothing was installed; this build cannot verify releases, so install one by hand from "+releasesURL+" (README, Install)",
			fmt.Sprintf("cannot verify starfix %s: %v", tag, err))
	case errors.Is(err, release.ErrSignature), errors.Is(err, release.ErrMismatch):
		return proto.Errf(proto.CodeInvalid, "nothing was installed; do not install this release by hand, and report it at "+releasesURL,
			fmt.Sprintf("starfix %s failed verification: %v", tag, err))
	case errors.Is(err, release.ErrNotFound):
		return proto.Errf(proto.CodeNotFound, "nothing was installed; build from source (go install) or pick a release with a build for this platform",
			fmt.Sprintf("starfix %s: %v", tag, err))
	}
	return proto.Errf(proto.CodeUnavailable, "nothing was installed; check your network (HTTPS_PROXY is honored) and retry",
		fmt.Sprintf("download starfix %s: %v", tag, err))
}

// installError types a failed swap.
func installError(exe string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return proto.Errf(proto.CodeUnavailable, "run it as the user who owns "+filepath.Dir(exe)+", or reinstall sfx into a directory you can write",
			fmt.Sprintf("cannot replace %s: %v", exe, err))
	}
	return proto.Errf(proto.CodeUnavailable, "retry; if it persists, reinstall from "+releasesURL,
		fmt.Sprintf("replace %s: %v", exe, err))
}
