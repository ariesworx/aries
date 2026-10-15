package release_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/release"
	"github.com/ariesworx/aries/internal/release/releasetest"
)

// installScript is the repository's install.sh, relative to this package.
const installScript = "../../install.sh"

// The PEM in install.sh must be a key in keys.go: the script must never
// trust a key the binaries do not. Which one it holds during a rotation
// is RELEASING.md's concern.
func TestInstallScriptKey(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(installScript)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)-----BEGIN PUBLIC KEY-----.*?-----END PUBLIC KEY-----`).Find(src)
	block, _ := pem.Decode(m)
	if block == nil {
		t.Fatal("install.sh holds no PEM public key")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := release.Keys()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Equal(pub) {
			return
		}
	}
	t.Fatalf("install.sh trusts %v, which internal/release/keys.go does not list", pub)
}

// fakeRelease serves one release the way github.com/OWNER/REPO/releases
// does: /latest redirects to /tag/TAG, files are under /download/TAG/.
type fakeRelease struct {
	tag   string
	files map[string][]byte
}

func (f *fakeRelease) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
	case p == "/latest":
		http.Redirect(w, r, "/tag/"+f.tag, http.StatusFound)
	case p == "/tag/"+f.tag:
		_, _ = w.Write([]byte("release page"))
	case strings.HasPrefix(p, "/download/"+f.tag+"/"):
		b, ok := f.files[strings.TrimPrefix(p, "/download/"+f.tag+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

const fakeAries = "#!/bin/sh\necho 'aries v1.2.3 (protocol 1)'\n"

// newFakeRelease returns a release v1.2.3 holding aries for linux/amd64,
// signed with a fresh key, and that key as a PEM.
func newFakeRelease(t *testing.T) (*fakeRelease, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	name := release.ArchiveName("aries", "v1.2.3", "linux", "amd64")
	archive := releasetest.TarGz(t, "aries", []byte(fakeAries))
	f := &fakeRelease{tag: "v1.2.3", files: map[string][]byte{name: archive}}
	f.setChecksums(priv, fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), name))
	return f, priv, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func (f *fakeRelease) setChecksums(key ed25519.PrivateKey, sums string) {
	f.files[release.ChecksumsName] = []byte(sums)
	f.files[release.SignatureName] = append(release.EncodeSignature(ed25519.Sign(key, []byte(sums))), '\n')
}

// releasesURL is where install.sh reads GitHub's layout from.
const releasesURL = "https://github.com/ariesworx/aries/releases"

// installCopy writes a copy of install.sh for a test and returns its
// path. The shipped script trusts its own key and reads GitHub, with no
// variable to change either, so a test that signs its own release, or
// serves GitHub's layout itself, runs a copy with pubPEM in place of the
// release key and githubBase in place of GitHub's URL; an empty value
// keeps the script's own.
func installCopy(t *testing.T, pubPEM, githubBase string) string {
	t.Helper()
	b, err := os.ReadFile(installScript)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if pubPEM != "" {
		key := regexp.MustCompile(`(?s)-----BEGIN PUBLIC KEY-----.*?-----END PUBLIC KEY-----\n?`)
		if !key.MatchString(src) {
			t.Fatal("install.sh holds no PEM public key to replace")
		}
		src = key.ReplaceAllLiteralString(src, strings.TrimSuffix(pubPEM, "\n"))
	}
	if githubBase != "" {
		line := "base=" + releasesURL + " ;;"
		if !strings.Contains(src, line) {
			t.Fatalf("install.sh has no %q to replace", line)
		}
		src = strings.Replace(src, line, "base="+githubBase+" ;;", 1)
	}
	p := filepath.Join(t.TempDir(), "install.sh")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil { //nolint:gosec // the test's own temp dir
		t.Fatal(err)
	}
	return p
}

// script writes an executable shell script into dir.
func script(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
}

// runInstall runs a copy of install.sh that reads GitHub's layout from
// srv and trusts pubPEM (or, when it is "", the release key), with fake
// (holding at least a uname) first on PATH, and returns its combined
// output and whether it succeeded.
func runInstall(t *testing.T, srv *httptest.Server, pubPEM, fake string, env []string, args ...string) (string, bool) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command("sh", append([]string{installCopy(t, pubPEM, srv.URL)}, args...)...) //nolint:gosec // the test's own arguments
	cmd.Env = append([]string{
		"PATH=" + fake + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"SHELL=/bin/zsh",
		"TMPDIR=" + t.TempDir(),
	}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// opensslEd25519 reports whether the openssl on PATH can verify Ed25519,
// which the signature cases need.
func opensslEd25519(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	key := filepath.Join(dir, "k")
	for _, args := range [][]string{
		{"genpkey", "-algorithm", "ed25519", "-out", key},
		{"pkeyutl", "-sign", "-rawin", "-inkey", key, "-in", key, "-out", filepath.Join(dir, "s")},
	} {
		if exec.Command("openssl", args...).Run() != nil { //nolint:gosec // constant arguments
			return false
		}
	}
	return true
}

func TestInstallScript(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for Linux and macOS")
	}
	for _, tool := range []string{"sh", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s: %v", tool, err)
		}
	}
	if _, err := exec.LookPath("curl"); err != nil {
		if _, err := exec.LookPath("wget"); err != nil {
			t.Skip("neither curl nor wget")
		}
	}
	canVerify := opensslEd25519(t)

	_, otherKey, _ := newFakeRelease(t)
	tests := []struct {
		name      string
		arch      string
		args      []string
		env       []string // "=TEST" in a value stands for the test's key
		noPEM     bool     // use the key embedded in install.sh
		noOpenSSL bool     // put a failing openssl first on PATH
		edit      func(*fakeRelease, ed25519.PrivateKey)
		needSig   bool // needs a working openssl
		ok        bool
		want      []string // in the output
		noOut     []string // not in the output
	}{
		{name: "latest", arch: "x86_64", ok: true,
			want: []string{"installed aries v1.2.3 (protocol 1) to ", `export PATH="`, "~/.zshrc"}},
		{name: "pinned version, aarch64 maps to arm64", arch: "aarch64", args: []string{"--version", "1.2.3"}, ok: true,
			edit: func(f *fakeRelease, k ed25519.PrivateKey) {
				name := release.ArchiveName("aries", "v1.2.3", "linux", "arm64")
				f.files[name] = f.files[release.ArchiveName("aries", "v1.2.3", "linux", "amd64")]
				f.setChecksums(k, fmt.Sprintf("%x  %s\n", sha256.Sum256(f.files[name]), name))
			},
			want: []string{"installed aries v1.2.3"}},
		{name: "bad checksum", arch: "x86_64",
			edit: func(f *fakeRelease, k ed25519.PrivateKey) {
				f.setChecksums(k, fmt.Sprintf("%064x  %s\n", 0, release.ArchiveName("aries", "v1.2.3", "linux", "amd64")))
			},
			want: []string{"install.sh: checksum mismatch for aries_1.2.3_linux_amd64.tar.gz", "nothing was installed", "fix: "}},
		{name: "bad signature", arch: "x86_64", needSig: true,
			edit: func(f *fakeRelease, _ ed25519.PrivateKey) {
				f.setChecksums(otherKey, string(f.files[release.ChecksumsName]))
			},
			want: []string{"install.sh: the signature on v1.2.3's checksums.txt does not verify", "fix: "}},
		{name: "test release against the real key", arch: "x86_64", noPEM: true, needSig: true,
			want: []string{"does not verify"}},
		{name: "a key in the environment is not trusted", arch: "x86_64", noPEM: true, needSig: true,
			env:  []string{"ARIES_PUBKEY_PEM=TEST"},
			want: []string{"does not verify"}, noOut: []string{"ARIES_PUBKEY_PEM"}},
		{name: "unknown arch", arch: "riscv64",
			want: []string{"install.sh: unsupported architecture riscv64", "fix: "}},
		{name: "--server fetches ariesd", arch: "x86_64", args: []string{"--server"},
			want: []string{"cannot download", "ariesd_1.2.3_linux_amd64.tar.gz"}},
		{name: "no openssl warns", arch: "x86_64", noOpenSSL: true, ok: true,
			want: []string{"warning: signature not checked", "installed aries v1.2.3"}},
		{name: "no openssl with --require-signature", arch: "x86_64", noOpenSSL: true, args: []string{"--require-signature"},
			want: []string{"install.sh: openssl cannot check Ed25519 signatures", "fix: "}},
		{name: "bad option", arch: "x86_64", args: []string{"--nope"},
			want: []string{"install.sh: unknown option --nope"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.needSig && !canVerify {
				t.Skip("openssl cannot verify Ed25519 here")
			}
			f, key, pubPEM := newFakeRelease(t)
			if tc.edit != nil {
				tc.edit(f, key)
			}
			srv := httptest.NewServer(f)
			t.Cleanup(srv.Close)
			dir := filepath.Join(t.TempDir(), "bin")
			env := []string{"ARIES_DIR=" + dir}
			for _, e := range tc.env {
				env = append(env, strings.Replace(e, "=TEST", "="+pubPEM, 1))
			}
			if tc.noPEM {
				pubPEM = ""
			}
			fake := t.TempDir()
			script(t, fake, "uname", fmt.Sprintf("case $1 in -s) echo Linux ;; -m) echo %s ;; *) exit 1 ;; esac\n", tc.arch))
			if tc.noOpenSSL {
				script(t, fake, "openssl", "echo 'openssl: unknown option -rawin' >&2; exit 1\n")
			}
			if tc.ok && !canVerify && !tc.noOpenSSL {
				tc.want = append(tc.want, "signature not checked")
			}
			out, ok := runInstall(t, srv, pubPEM, fake, env, tc.args...)
			if ok != tc.ok {
				t.Fatalf("succeeded = %v, want %v; output:\n%s", ok, tc.ok, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.noOut {
				if strings.Contains(out, w) {
					t.Errorf("output holds %q:\n%s", w, out)
				}
			}
			bin := filepath.Join(dir, "aries")
			fi, err := os.Stat(bin)
			if !tc.ok {
				if err == nil {
					t.Errorf("a failed install left %s", bin)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0o755 {
				t.Errorf("mode %v, want 0755", fi.Mode().Perm())
			}
			if b, _ := os.ReadFile(bin); string(b) != fakeAries { //nolint:gosec // test temp dir
				t.Errorf("installed %q, want the archive's aries", b)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Errorf("%s holds %d entries, want only aries", dir, len(entries))
			}
		})
	}
}
