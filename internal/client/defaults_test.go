package client

// What sfx offers when no key is configured (decision 35, the
// unconfigured case), and what it does with a key put where a path
// belongs. Every home directory and agent here is the test's own.

import (
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/aries/internal/proto"
)

// sshDir makes home/.ssh and returns it.
func sshDir(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestDefaultIdentities pins the unconfigured case: the default identity
// files of ~/.ssh, in ssh's order, then ssh-agent's other keys, each key
// once, and at most maxOffered in all.
func TestDefaultIdentities(t *testing.T) {
	t.Parallel()
	ecdsaNamed, edNamed, locked, hardware, inAgent, stray := newKeyPair(t), newKeyPair(t), newKeyPair(t), newKeyPair(t), newKeyPair(t), newKeyPair(t)
	fps := func(ks ...keyPair) []string {
		var out []string
		for _, k := range ks {
			out = append(out, k.fingerprint())
		}
		return out
	}
	tests := []struct {
		name    string
		files   func(t *testing.T, dir string)
		agent   []keyPair
		offered []string
		by      []string // how each offered key was chosen
		skipped []string // part of each skipped file's reason
	}{
		{name: "the files, in ssh's order, then the agent's other keys",
			files: func(t *testing.T, dir string) {
				edNamed.write(t, dir, "id_ed25519", "", true)
				ecdsaNamed.write(t, dir, "id_ecdsa", "", true) // the name sets the order, whatever the key
				stray.write(t, dir, "work_key", "", true)      // no default name: not read
			},
			agent: []keyPair{inAgent, edNamed}, offered: fps(ecdsaNamed, edNamed, inAgent),
			by: []string{"default identity", "default identity", "ssh-agent"}},
		{name: "no files: the agent's keys, as before", agent: []keyPair{inAgent, stray}, offered: fps(inAgent, stray), by: []string{"ssh-agent", "ssh-agent"}},
		{name: "a passphrase-protected file whose key the agent holds",
			files:   func(t *testing.T, dir string) { locked.write(t, dir, "id_ed25519", "secret", true) },
			agent:   []keyPair{inAgent, locked},
			offered: fps(locked, inAgent), by: []string{"default identity", "ssh-agent"}},
		{name: "a passphrase-protected file the agent does not hold is skipped",
			files:   func(t *testing.T, dir string) { locked.write(t, dir, "id_ed25519", "secret", true) },
			agent:   []keyPair{inAgent},
			offered: fps(inAgent), by: []string{"ssh-agent"}, skipped: []string{"id_ed25519: it is passphrase-protected, and ssh-agent does not hold its key"}},
		{name: "a hardware-backed key is used through the agent",
			files: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "id_ed25519_sk"), []byte("a handle to a key on a token\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				hardware.writePub(t, dir, "id_ed25519_sk.pub")
			},
			agent: []keyPair{hardware}, offered: fps(hardware), by: []string{"default identity"}},
		{name: "a hardware-backed key the agent does not hold is skipped",
			files: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "id_ecdsa_sk"), []byte("a handle\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				hardware.writePub(t, dir, "id_ecdsa_sk.pub")
				edNamed.write(t, dir, "id_ed25519", "", false)
			},
			offered: fps(edNamed), by: []string{"default identity"}, skipped: []string{"id_ecdsa_sk: it is a hardware-backed key, which sfx uses only through ssh-agent, and ssh-agent does not hold it"}},
		{name: "a file that is no key is skipped, and the rest used",
			files: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "id_rsa"), []byte("not a key\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				edNamed.write(t, dir, "id_ed25519", "", false)
			},
			offered: fps(edNamed), by: []string{"default identity"}, skipped: []string{"id_rsa: it is not a private key sfx can read"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.files != nil {
				tc.files(t, sshDir(t, home))
			}
			env := map[string]string{}
			if tc.agent != nil {
				env["SSH_AUTH_SOCK"] = agentWith(t, tc.agent...)
			}
			id, err := loadIdentity(&Config{Root: t.TempDir()}, func(k string) string { return env[k] }, func() (string, error) { return home, nil })
			if err != nil {
				t.Fatal(err)
			}
			defer id.close()
			var offered, by []string
			for _, k := range id.keys {
				offered, by = append(offered, k.Fingerprint), append(by, k.By)
			}
			if !slices.Equal(offered, tc.offered) || !slices.Equal(by, tc.by) {
				t.Errorf("offers %v, chosen by %v\nwant   %v, chosen by %v", offered, by, tc.offered, tc.by)
			}
			if len(id.skipped) != len(tc.skipped) {
				t.Fatalf("skipped %q, want %d files skipped", id.skipped, len(tc.skipped))
			}
			for i, want := range tc.skipped {
				if !strings.Contains(id.skipped[i], want) {
					t.Errorf("skipped[%d] = %q, want it to say %q", i, id.skipped[i], want)
				}
			}
		})
	}
}

// TestOffersAreCapped pins the cap on the unconfigured case, against the
// stand-in sshd, which like OpenSSH hangs up after six refused keys: sfx
// offers five, so the server answers, and the refusal says what was
// offered, what was left out, and how to name the key.
func TestOffersAreCapped(t *testing.T) {
	t.Parallel()
	mine := newKeyPair(t)
	var crowd []keyPair
	for range 7 {
		crowd = append(crowd, newKeyPair(t))
	}
	d := newFakeSSHD(t, map[string]keyPair{"me": mine})
	crowd[0].write(t, sshDir(t, d.home), "id_ed25519", "", true)
	sock := agentWith(t, append(slices.Clone(crowd[1:]), mine)...) // the server knows the last
	who, offered, err := d.dial(t, "", map[string]string{"SSH_AUTH_SOCK": sock})
	pe, ok := err.(*proto.Error)
	if !ok || who != "" || pe.Code != proto.CodeAuth {
		t.Fatalf("signed in as %q, Dial = %v; want an auth refusal", who, err)
	}
	var want []string
	for _, k := range crowd[:maxOffered] {
		want = append(want, k.fingerprint())
	}
	if !slices.Equal(offered, want) {
		t.Errorf("the server was offered %v\nwant the default identity and then the agent's first keys, %d in all: %v", offered, maxOffered, want)
	}
	for _, s := range []string{"accepted none of the 5 keys sfx offered", crowd[0].fingerprint() + " (" + filepath.Join(d.home, ".ssh", "id_ed25519") + ")",
		crowd[1].fingerprint() + " (ssh-agent)", "3 more keys that ssh-agent holds were not offered"} {
		if !strings.Contains(pe.Message, s) {
			t.Errorf("message %q lacks %q", pe.Message, s)
		}
	}
	if strings.Contains(pe.Message, mine.fingerprint()) || !strings.Contains(pe.Fix, EnvKeyFile) {
		t.Errorf("the refusal lists a key that was not offered, or its fix does not name %s: %v\nfix: %s", EnvKeyFile, pe.Message, pe.Fix)
	}
	// Named, the key the cap left out signs in, alone.
	key := mine.write(t, t.TempDir(), "mine", "", false)
	who, offered, _ = d.dial(t, "", map[string]string{"SSH_AUTH_SOCK": sock, EnvKeyFile: key})
	if who != "me" || !slices.Equal(offered, []string{mine.fingerprint()}) {
		t.Errorf("with %s naming the key: signed in as %q having offered %v; want me, and that key alone", EnvKeyFile, who, offered)
	}
}

// TestAKeyIsNeverTakenForAPath pins rule 2 for the two variables. Some
// deployments keep a private key's text in a secret named STARFIX_KEY:
// sfx never reads that variable, and never prints its value. And
// STARFIX_KEY_FILE holds a path: a value that is plainly a key is
// refused without being repeated.
func TestAKeyIsNeverTakenForAPath(t *testing.T) {
	t.Parallel()
	secret, mine, stranger := newKeyPair(t), newKeyPair(t), newKeyPair(t)
	block, err := ssh.MarshalPrivateKey(secret.priv, "")
	if err != nil {
		t.Fatal(err)
	}
	material := string(pem.EncodeToMemory(block))
	body := strings.Split(material, "\n")[1] // a line of the key itself
	d := newFakeSSHD(t, map[string]keyPair{"me": mine})
	mine.write(t, d.root, "mine", "", false)
	stranger.write(t, d.root, "unknown", "", false)
	leaks := func(t *testing.T, err error) {
		t.Helper()
		pe, ok := err.(*proto.Error)
		said := err.Error()
		if ok {
			said = pe.Message + "\n" + pe.Fix
		}
		for _, part := range []string{body, "BEGIN OPENSSH", "PRIVATE KEY", material[:40]} {
			if strings.Contains(said, part) {
				t.Errorf("the refusal repeats the key in the variable (%q): %s", part, said)
			}
		}
	}

	t.Run("STARFIX_KEY is not read", func(t *testing.T) {
		env := map[string]string{EnvNotRead: material}
		if who, offered, _ := d.dial(t, "mine", env); who != "me" || len(offered) != 1 {
			t.Errorf("with %s holding a key and key: naming another: signed in as %q, %d keys offered; want me, by the configured key", EnvNotRead, who, len(offered))
		}
		for _, tc := range []struct{ name, key, note string }{
			{"the configured key is refused", "unknown", EnvNotRead + " is set, and sfx does not read it"},
			{"no key at all", "", EnvNotRead + " is set, and sfx does not read it"},
		} {
			who, _, err := d.dial(t, tc.key, env)
			pe, ok := err.(*proto.Error)
			if who != "" || !ok || pe.Code != proto.CodeAuth || !strings.Contains(pe.Message, tc.note) || !strings.Contains(pe.Message+pe.Fix, EnvKeyFile) {
				t.Errorf("%s: signed in as %q, Dial = %v; want an auth refusal that says %q and names %s", tc.name, who, err, tc.note, EnvKeyFile)
			}
			leaks(t, err)
		}
		// Unset, nothing is said of it.
		if _, _, err := d.dial(t, "unknown", nil); strings.Contains(err.Error(), EnvNotRead+" is set") {
			t.Errorf("with %s unset, the refusal speaks of it: %v", EnvNotRead, err)
		}
	})

	t.Run("STARFIX_KEY_FILE holding a key", func(t *testing.T) {
		for name, value := range map[string]string{
			"a private key":          material,
			"its first line":         strings.Split(material, "\n")[0],
			"its base64 on one line": strings.Join(strings.Split(material, "\n")[1:3], ""),
			"two lines":              "/tmp/a\n/tmp/b",
			"longer than a path":     "/" + strings.Repeat("a", 5000),
		} {
			who, offered, err := d.dial(t, "mine", map[string]string{EnvKeyFile: value, "SSH_AUTH_SOCK": agentWith(t, mine)})
			pe, ok := err.(*proto.Error)
			if who != "" || len(offered) != 0 || !ok || pe.Code != proto.CodeInvalid || !strings.Contains(pe.Fix, "the file's path, never the key itself") {
				t.Errorf("%s: signed in as %q, %d keys offered, Dial = %v; want it refused as invalid before anything is offered", name, who, len(offered), err)
			}
			leaks(t, err)
			if ok && strings.Contains(pe.Message+pe.Fix, value) {
				t.Errorf("%s: the refusal repeats the variable's value", name)
			}
		}
	})

	t.Run("key: holding a key", func(t *testing.T) {
		_, err := ParseConfig([]byte("project: aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\nserver:\n  host: example.com\n  host_key: SHA256:" + strings.Repeat("A", 43) +
			"\nkey: \"" + strings.Split(material, "\n")[0] + "\"\n"))
		if err == nil || !strings.Contains(err.Error(), "key is not a file path") {
			t.Errorf("ParseConfig with a key's armor as key: = %v, want it refused as no file path", err)
		}
		if err != nil {
			leaks(t, err)
		}
	})
}
