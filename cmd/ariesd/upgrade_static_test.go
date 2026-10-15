package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ariesworx/aries/internal/release"
	"github.com/ariesworx/aries/internal/release/releasetest"
)

// newStaticRig is newRig with a static release tree in place of the fake
// GitHub: ariesd finds it through $ARIES_RELEASES, and no release
// client is given to it. The tree holds an ariesd for linux/amd64 at
// each tag, whose contents are the tag.
func newStaticRig(t *testing.T, current, configExtra string, tags ...string) (*upgradeRig, *releasetest.Tree) {
	t.Helper()
	r := newRig(t, current, "v0.0.1", configExtra)
	tr := releasetest.NewTree(t)
	for _, tag := range tags {
		tr.AddBinary(t, tag, "ariesd", "linux", "amd64", []byte(tag))
	}
	r.d.releases, r.d.verifier = nil, tr.Verifier()
	r.vars = map[string]string{release.EnvReleases: tr.URL}
	return r, tr
}

// ariesd upgrade, start to finish, from a static tree: over loopback
// HTTP named by the variable, and from a directory named by the config
// file's releases setting.
func TestUpgradeServerFromStaticTree(t *testing.T) {
	t.Parallel()
	for _, via := range []string{"variable", "config file"} {
		t.Run(via, func(t *testing.T) {
			r, tr := newStaticRig(t, "v0.2.0", "systemd_unit: ariesd.service\n", "v0.2.0", "v0.3.0", "v0.4.0-rc.1")
			if via == "config file" {
				dir := t.TempDir()
				tr.WriteDir(t, dir)
				r.vars = nil
				cfg, err := os.ReadFile(r.cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(r.cfg, append(cfg, "releases: "+dir+"\n"...), 0o600); err != nil { //nolint:gosec // the test's own config file
					t.Fatal(err)
				}
			}
			out, err := r.run(t, "--check")
			if want := "ariesd v0.2.0, latest v0.3.0: run ariesd upgrade\n"; err != nil || out != want {
				t.Fatalf("upgrade --check = %q, %v, want %q", out, err, want)
			}
			out, err = r.run(t)
			want := "ariesd upgraded v0.2.0 -> v0.3.0 (ariesd.service restarted and healthy)\n"
			if err != nil || out != want {
				t.Fatalf("upgrade = %q, %v, want %q", out, err, want)
			}
			if r.exeHolds(t) != "v0.3.0" || r.running != "v0.3.0" {
				t.Errorf("exe %q, running %q, want v0.3.0", r.exeHolds(t), r.running)
			}
			// A prerelease is installed only by name.
			r.d.current = "v0.3.0"
			if out, err := r.run(t, "--to", "v0.4.0-rc.1"); err != nil || r.exeHolds(t) != "v0.4.0-rc.1" {
				t.Errorf("upgrade --to v0.4.0-rc.1 = %q, %v; exe %q", out, err, r.exeHolds(t))
			}
		})
	}
}

// The variable wins over the config file, and a location ariesd cannot
// use is refused with what to do, before anything is asked of anyone.
func TestUpgradeServerSourceSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		env     string // $ARIES_RELEASES; "" unset
		config  string // releases: in the config file; "" unset
		args    []string
		want    string   // in stdout, when the upgrade must succeed
		wantErr []string // in the error
		noErr   string
	}{
		{name: "the variable alone", env: "TREE", want: "latest v0.3.0"},
		{name: "the config file alone", config: "TREE", want: "latest v0.3.0"},
		// The config file's value is one upgrade would refuse, were it read.
		{name: "the variable wins", env: "TREE", config: "mirror", want: "latest v0.3.0"},
		{name: "plain http to a host", env: "http://releases.example.com/aries",
			wantErr: []string{"ARIES_RELEASES", "not an https URL", "fix: "}},
		{name: "a relative directory in the config file", config: "mirror",
			wantErr: []string{"releases:", "absolute path", "fix: "}},
		{name: "a password in the URL", env: "https://alice:hunter2@releases.example.com/", //nolint:gosec // an invented password, which the refusal must not repeat
			wantErr: []string{"user name or password", "fix: "}, noErr: "hunter2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, tr := newStaticRig(t, "v0.2.0", "", "v0.3.0")
			sub := func(v string) string { return strings.ReplaceAll(v, "TREE", tr.URL) }
			r.vars = map[string]string{release.EnvReleases: sub(tc.env)}
			if tc.config != "" {
				cfg, err := os.ReadFile(r.cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(r.cfg, append(cfg, "releases: "+sub(tc.config)+"\n"...), 0o600); err != nil { //nolint:gosec // the test's own config file
					t.Fatal(err)
				}
			}
			out, err := r.run(t, "--check")
			if tc.want != "" {
				if err != nil || !strings.Contains(out, tc.want) {
					t.Errorf("upgrade --check = %q, %v, want output holding %q", out, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("upgrade --check = %q, want an error", out)
			}
			for _, w := range tc.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %v, want it to hold %q", err, w)
				}
			}
			if tc.noErr != "" && strings.Contains(err.Error(), tc.noErr) {
				t.Errorf("err = %v, which repeats %q", err, tc.noErr)
			}
			if got := tr.Requests(); len(got) != 0 {
				t.Errorf("the tree was asked for %q, though the location named something else", got)
			}
		})
	}
}

// What a hostile or broken release host gets out of ariesd upgrade: an
// error, or nothing. In no case is the binary replaced, the store tagged
// or the daemon restarted.
func TestUpgradeServerHostileStaticHost(t *testing.T) {
	t.Parallel()
	var asked atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		_, _ = w.Write([]byte(`{"format": 1, "releases": [{"tag": "v0.9.0"}]}`))
	}))
	t.Cleanup(elsewhere.Close)

	tests := []struct {
		name    string
		edit    func(t *testing.T, tr *releasetest.Tree)
		args    []string
		want    string // stdout, when ariesd must say it is up to date
		wantErr string
	}{
		{name: "index names only an older signed release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.2.0"}]}`)
			},
			want: "ariesd v0.3.0 is up to date (newer than the latest release, v0.2.0)\n"},
		{name: "an older release asked for by name", args: []string{"--to", "v0.2.0"},
			edit:    func(*testing.T, *releasetest.Tree) {},
			wantErr: "migrations only go forward"},
		{name: "an older signed release in a newer one's directory",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Move("v0.2.0", "v0.9.0")
				tr.Set("v0.9.0/ariesd_0.9.0_linux_amd64.tar.gz", tr.File("v0.2.0/ariesd_0.2.0_linux_amd64.tar.gz"))
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.9.0"}]}`)
			},
			wantErr: "does not list ariesd_0.9.0_linux_amd64.tar.gz"},
		{name: "index names a tag that climbs out of the tree",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "../v0.4.0"}]}`)
			},
			wantErr: "not of the form vMAJOR.MINOR.PATCH"},
		{name: "index names a URL",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "` + elsewhere.URL + `/v0.9.0"}]}`)
			},
			wantErr: "not of the form vMAJOR.MINOR.PATCH"},
		{name: "huge index",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.4.0"}]}` + strings.Repeat(" ", release.MaxIndexBytes))
			},
			wantErr: "larger than"},
		{name: "malformed index",
			edit:    func(_ *testing.T, tr *releasetest.Tree) { tr.SetIndex(`<html>`) },
			wantErr: "index.json"},
		{name: "prerelease served as the latest release",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.SetIndex(`{"format": 1, "releases": [{"tag": "v0.5.0-rc.1", "prerelease": false}]}`)
			},
			wantErr: "prerelease"},
		{name: "index redirects elsewhere",
			edit: func(_ *testing.T, tr *releasetest.Tree) {
				tr.Handle(func(w http.ResponseWriter, r *http.Request) bool {
					http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound) //nolint:gosec // the test sends its own client to its own server
					return true
				})
			},
			wantErr: "a redirect, and none is followed"},
		{name: "archive changed after signing",
			edit: func(t *testing.T, tr *releasetest.Tree) {
				tr.Set("v0.4.0/ariesd_0.4.0_linux_amd64.tar.gz", releasetest.TarGz(t, "ariesd", []byte("evil")))
			},
			wantErr: "checksum mismatch"},
		{name: "no signature",
			edit:    func(_ *testing.T, tr *releasetest.Tree) { tr.Delete("v0.4.0/checksums.txt.sig") },
			wantErr: "failed verification"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, tr := newStaticRig(t, "v0.3.0", "systemd_unit: ariesd\n", "v0.2.0", "v0.3.0", "v0.4.0", "v0.5.0-rc.1")
			tc.edit(t, tr)
			asked.Store(0)
			out, err := r.run(t, tc.args...)
			switch {
			case tc.want != "":
				if err != nil || out != tc.want {
					t.Errorf("upgrade = %q, %v, want %q", out, err, tc.want)
				}
			case err == nil:
				t.Errorf("upgrade = %q, want an error holding %q", out, tc.wantErr)
			default:
				if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "fix: ") {
					t.Errorf("err = %v, want it to hold %q and a fix", err, tc.wantErr)
				}
				if len(err.Error()) > 1000 {
					t.Errorf("the error is %d bytes long; it repeats what the host sent", len(err.Error()))
				}
			}
			if r.exeHolds(t) != "v0.3.0" || len(r.restarts) != 0 {
				t.Errorf("exe %q, restarts %v: want nothing touched", r.exeHolds(t), r.restarts)
			}
			if _, err := os.Stat(r.exe + ".prev"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused upgrade left a .prev: %v", err)
			}
			if n := asked.Load(); n != 0 {
				t.Errorf("ariesd followed a redirect: %d requests reached the other host", n)
			}
		})
	}
}
