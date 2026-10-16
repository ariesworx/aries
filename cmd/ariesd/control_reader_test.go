package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/server"
)

// `ariesd control init` warns that a key it took into the store is
// still let in by the file it read only when something reads that file:
// sshd, with SSH on, or ariesd's key proof over HTTPS, when the file is
// its authorized_keys. With SSH off the installer gives the first
// admin's key in /etc/aries/admin.keys, which nothing else reads, and
// the warning was printed there on every install and rerun (P5, run in
// systemd containers of Ubuntu 24.04 and 26.04).
func TestControlInitSaysWhoReadsTheFile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, transports string
		// hand: the file read is ariesd's authorized_keys.
		hand bool
		want string // "" for no warning
	}{
		{name: "ssh, a file of its own", transports: "[ssh]", want: "sshd reads that file too"},
		{name: "ssh, ariesd's authorized_keys", transports: "[ssh]", hand: true, want: "sshd reads that file too"},
		{name: "https and ssh", transports: "[https, ssh]", want: "sshd reads that file too"},
		{name: "https, a file of its own", transports: "[https]"},
		{name: "https, ariesd's authorized_keys", transports: "[https]", hand: true,
			want: "ariesd reads that file too, for a key that signs in over HTTPS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSite(t, "transports: "+tc.transports+"\n")
			line := forced(t, "dana", "dana-1")
			keys := writeKeys(t, line)
			if tc.hand {
				// The site's hand-maintained file, beside its keys file.
				keys = filepath.Join(filepath.Dir(s.keysFile), "authorized_keys")
				if keys != s.env[server.EnvAuthorizedKeys] {
					t.Fatalf("the site's authorized_keys is %s, not %s", s.env[server.EnvAuthorizedKeys], keys)
				}
				if err := os.WriteFile(keys, []byte(line+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := s.run(t, "", controlCmd, "init", "--name", "acme", "--authorized-keys", keys)
			if got.err != nil {
				t.Fatalf("control init: %v\n%s", got.err, got.stderr)
			}
			warning := "1 key in the store is also a line of " + filepath.Clean(keys)
			has := strings.Contains(got.stdout, warning)
			switch {
			case tc.want == "" && has:
				t.Errorf("stdout warns of a file nothing reads:\n%s", got.stdout)
			case tc.want != "" && !strings.Contains(got.stdout, warning+": "+tc.want+", so `aries admin key remove` does not keep the key out until its line is deleted too\n"):
				t.Errorf("stdout = %q\nwant the line %q with %q", got.stdout, warning, tc.want)
			}
		})
	}
}
