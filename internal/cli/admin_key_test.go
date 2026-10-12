package cli

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ariesworx/starfix/internal/proto"
)

// The key commands refuse bad usage before dialing.
func TestAdminKeyUsage(t *testing.T) {
	empty := t.TempDir()
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"key needs an action", []string{"admin", "key"}, "admin key needs add, list or remove"},
		{"key unknown action", []string{"admin", "key", "rm", "SHA256:x"}, `unknown key action "rm"`},
		{"key add needs a file", []string{"admin", "key", "add", "bob"}, "key add needs a principal and a public key file"},
		{"key add takes one file", []string{"admin", "key", "add", "bob", "a.pub", "b.pub"}, "key add needs a principal and a public key file"},
		{"key list needs a principal", []string{"admin", "key", "list"}, "key list needs one principal"},
		{"key remove needs a fingerprint", []string{"admin", "key", "remove"}, "key remove needs one fingerprint"},
		{"key remove takes a fingerprint, not a name", []string{"admin", "key", "remove", "bob"}, "key remove takes a key's SHA256: fingerprint"},
		{"key remove takes a fingerprint, not a file", []string{"admin", "key", "remove", "id_ed25519.pub"}, "key remove takes a key's SHA256: fingerprint"},
		{"key add takes no project", []string{"admin", "key", "add", "bob", "a.pub", "--project", "acme"}, "flag provided but not defined: -project"},
		{"principal list takes no key", []string{"admin", "principal", "list", "--key", "a.pub"}, "principal list does not take --key"},
		{"principal disable takes no key", []string{"admin", "principal", "disable", "bob", "--key", "a.pub"}, "principal disable does not take --key"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, append([]string{"-C", empty}, tc.args...)...)
			if code != ExitUsage || out != "" || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: usage: sfx admin "+tc.args[1]+" ") {
				t.Errorf("sfx %s: exit %d (want %d), stdout %q, stderr %q; want it to name %q and the group's usage",
					strings.Join(tc.args, " "), code, ExitUsage, out, errb, tc.stderr)
			}
		})
	}
	_, help, _ := runCLI(t, "help", "admin")
	for _, want := range []string{"admin key add NAME FILE|- | list NAME | remove FINGERPRINT",
		"admin principal add NAME [--key FILE]... [--project P | --no-project] | list | disable NAME | enable NAME"} {
		if !strings.Contains(help, want) {
			t.Errorf("sfx help admin lacks %q:\n%s", want, help)
		}
	}
}

// keyFiles writes an invented key pair to dir and returns the private
// key's file, the public key's, and the public key line.
func keyFiles(t *testing.T, dir string) (private, public, line string) {
	t.Helper()
	sum := sha256.Sum256([]byte(t.Name()))
	priv := ed25519.NewKeyFromSeed(sum[:])
	block, err := ssh.MarshalPrivateKey(priv, "bob@example.com")
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " bob@example.com"
	private, public = filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "id_ed25519.pub")
	if err := os.WriteFile(private, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(public, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return private, public, line
}

// TestReadPublicKey reads the one public key of a file, and refuses
// everything else without repeating what the file holds.
func TestReadPublicKey(t *testing.T) {
	dir := t.TempDir()
	private, public, line := keyFiles(t, dir)
	privBytes, err := os.ReadFile(private) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil { //nolint:gosec // the test's own temp dir
			t.Fatal(err)
		}
		return p
	}
	key, _, _ := strings.Cut(line, " bob@")
	// The armor of a PEM private key around bytes that are no key.
	rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("only the armor of a key, for the test")}))
	r := &runner{env: Env{Stdin: strings.NewReader(line + "\n")}, dir: dir}

	for _, tc := range []struct {
		name, file, want string
	}{
		{"a .pub file", public, line},
		{"a path relative to -C", "id_ed25519.pub", line},
		{"standard input", "-", line},
		{"no comment", write("bare.pub", key+"\n"), key},
		{"no trailing newline", write("nonl.pub", line), line},
		{"a Windows line ending", write("crlf.pub", line+"\r\n"), line},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.env.Stdin = strings.NewReader(line + "\n")
			got, err := r.publicKey(tc.file)
			if err != nil || got != tc.want {
				t.Errorf("publicKey(%s) = %q, %v; want %q", tc.file, got, err, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name, file, stdin, msg string
	}{
		{"an OpenSSH private key", private, "", "holds a private key"},
		{"a private key on standard input", "-", string(privBytes), "standard input holds a private key"},
		{"a PEM private key", write("rsa", rsaPEM), "", "holds a private key"},
		{"a private key after a public one", write("both", line+"\n"+string(privBytes)), "", "holds a private key"},
		{"two public keys", write("two.pub", line+"\n"+line+"\n"), "", "holds more than one line"},
		{"an authorized_keys line with options", write("opts.pub", `command="sh" `+line+"\n"), "", "is not one public key"},
		{"text that is no key", write("notes.txt", "remember to rotate keys\n"), "", "is not one public key"},
		{"an empty file", write("empty.pub", ""), "", "is empty"},
		{"a file that is not there", filepath.Join(dir, "none.pub"), "", "cannot read"},
		{"a directory", dir, "", "cannot read"},
		{"a file far too large", write("big.pub", strings.Repeat("A", maxKeyFile+10)), "", "is larger than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.env.Stdin = strings.NewReader(tc.stdin)
			got, err := r.publicKey(tc.file)
			pe, ok := err.(*proto.Error) //nolint:errorlint // publicKey returns the refusal itself
			if !ok || got != "" || pe.Code != proto.CodeInvalid || !strings.Contains(pe.Message, tc.msg) || pe.Fix == "" {
				t.Fatalf("publicKey(%s) = %q, %v; want nothing and an invalid refusal containing %q, with a fix", tc.name, got, err, tc.msg)
			}
			for _, secret := range []string{"b3BlbnNzaC1rZXktdjE", "b25seSB0aGUgYXJtb3Ig", "remember to rotate"} {
				if strings.Contains(pe.Message+pe.Fix, secret) {
					t.Errorf("the refusal repeats what the file holds: %+v", pe)
				}
			}
		})
	}
}

// A private key file is refused before anything is sent: here there is
// not even a server to send to, and the refusal is the key's, not the
// missing configuration's.
func TestAdminKeyAddSendsNoPrivateKey(t *testing.T) {
	empty := t.TempDir()
	private, _, _ := keyFiles(t, empty)
	for _, args := range [][]string{
		{"admin", "key", "add", "bob", private},
		{"admin", "key", "add", "bob", "id_ed25519"},
		{"admin", "principal", "add", "bob", "--key", private},
		{"--json", "admin", "key", "add", "bob", private},
	} {
		code, out, errb := runCLI(t, append([]string{"-C", empty}, args...)...)
		all := out + errb
		if code != ExitFailure || !strings.Contains(all, "holds a private key, which never leaves this machine") ||
			!strings.Contains(all, "id_ed25519.pub") || strings.Contains(all, ".starfix.yaml") {
			t.Errorf("sfx %s: exit %d\nstdout %q\nstderr %q\nwant exit %d and the private key refused, naming the .pub file, before any configuration is read",
				strings.Join(args, " "), code, out, errb, ExitFailure)
		}
		if strings.Contains(all, "b3BlbnNzaC1rZXktdjE") || strings.Contains(all, "PRIVATE KEY-----") {
			t.Errorf("sfx %s printed the private key:\n%s", strings.Join(args, " "), all)
		}
	}
	// Standard input is read the same way.
	b, err := os.ReadFile(private) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	code := Run(t.Context(), []string{"-C", empty, "admin", "key", "add", "bob", "-"}, Env{Stdin: bytes.NewReader(b), Stdout: &out, Stderr: &errb,
		Getenv: func(string) string { return "" }, Version: "v0.3.0"})
	if code != ExitFailure || !strings.Contains(errb.String(), "standard input holds a private key") {
		t.Errorf("sfx admin key add bob - with a private key: exit %d, stderr %q", code, errb.String())
	}
}

func TestPrintKeys(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	const fp1, fp2 = "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU", "SHA256:WeXpogacdCqvmaxuNZFUpT1Te6ZU/j7aPAocB0T2i8o"
	tests := []struct {
		name  string
		print func(*bytes.Buffer)
		want  string
	}{
		{"keys", func(w *bytes.Buffer) {
			printKeys(w, proto.KeysResult{Principal: "bob", Keys: []proto.Key{
				{Fingerprint: fp1, Type: "ssh-ed25519", Comment: "bob@laptop", AddedBy: "dana", AddedAt: at},
				{Fingerprint: fp2, Type: "ssh-rsa", AddedBy: "import", AddedAt: at}}})
		}, "keys of bob\n" +
			"FINGERPRINT                                         TYPE         COMMENT     ADDED BY  ADDED\n" +
			fp1 + "  ssh-ed25519  bob@laptop  dana      2026-10-11 09:30 UTC\n" +
			fp2 + "  ssh-rsa                  import    2026-10-11 09:30 UTC\n"},
		{"no keys", func(w *bytes.Buffer) { printKeys(w, proto.KeysResult{Principal: "bob"}) }, "no keys recorded for bob\n"},
		// A key that is also a line of the hand-maintained file is marked.
		{"a key also in authorized_keys", func(w *bytes.Buffer) {
			printKeys(w, proto.KeysResult{Principal: "bob", Keys: []proto.Key{
				{Fingerprint: fp1, Type: "ssh-ed25519", Comment: "bob@laptop", AddedBy: "import", AddedAt: at, InAuthorizedKeys: true},
				{Fingerprint: fp2, Type: "ssh-rsa", AddedBy: "dana", AddedAt: at}}})
		}, "keys of bob\n" +
			"FINGERPRINT                                         TYPE         COMMENT     ADDED BY  ADDED                 ALSO IN\n" +
			fp1 + "  ssh-ed25519  bob@laptop  import    2026-10-11 09:30 UTC  authorized_keys\n" +
			fp2 + "  ssh-rsa                  dana      2026-10-11 09:30 UTC  -\n" +
			"ALSO IN authorized_keys: the key is also a line of the server's hand-maintained authorized_keys file, which sshd reads after the store's keys; " +
			"`sfx admin key remove` closes the key's connections, and sshd still accepts it by that line until the line is deleted on the server\n"},
		{"key added", func(w *bytes.Buffer) {
			printKeyAdded(w, proto.KeyResult{ID: fp1, Principal: "bob", Type: "ssh-ed25519", Comment: "bob@laptop", Change: "added"})
		}, "key " + fp1 + " (ssh-ed25519, bob@laptop) added to bob\n"},
		{"key already there", func(w *bytes.Buffer) {
			printKeyAdded(w, proto.KeyResult{ID: fp1, Principal: "bob", Type: "ssh-ed25519", Change: "unchanged"})
		}, "bob already has key " + fp1 + " (ssh-ed25519)\n"},
		{"key removed", func(w *bytes.Buffer) {
			printKeyRemoved(w, proto.KeyResult{ID: fp1, Principal: "bob", Type: "ssh-ed25519"})
		}, "key " + fp1 + " removed from bob\n"},
		{"key removed, with a connection", func(w *bytes.Buffer) {
			printKeyRemoved(w, proto.KeyResult{ID: fp1, Principal: "bob", Type: "ssh-ed25519", Dropped: 1})
		}, "key " + fp1 + " removed from bob\n1 connection it had signed in was closed\n"},
		{"key removed, with connections", func(w *bytes.Buffer) {
			printKeyRemoved(w, proto.KeyResult{ID: fp1, Principal: "bob", Type: "ssh-ed25519", Dropped: 3})
		}, "key " + fp1 + " removed from bob\n3 connections it had signed in were closed\n"},
		{"claims released", func(w *bytes.Buffer) { printReleased(w, 2, "bob") }, "2 claims bob held were released\n"},
		{"a claim released", func(w *bytes.Buffer) { printReleased(w, 1, "bob") }, "1 claim bob held was released\n"},
		{"no claims released", func(w *bytes.Buffer) { printReleased(w, 0, "bob") }, ""},
		// A comment from the server cannot start a line of its own.
		{"a hostile comment", func(w *bytes.Buffer) {
			printKeyAdded(w, proto.KeyResult{ID: fp1, Principal: "bob", Type: "ssh-ed25519", Comment: "x\nfix: run this", Change: "added"})
		}, "key " + fp1 + " (ssh-ed25519, x\\nfix: run this) added to bob\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tc.print(&b)
			if got := b.String(); got != tc.want {
				t.Errorf("printed\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}
