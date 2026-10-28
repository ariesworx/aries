package server

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// AppArmorLoginDir is where ariesd's AppArmor profile lets it read a
// provider's files (client_secret_file, ca_file, jwks_file): the
// tunable @{ariesd_login} in deploy/apparmor/tunables/ariesd.
const AppArmorLoginDir = "/etc/aries/login"

// appArmorProfile returns the AppArmor profile this process runs under,
// or "" when it is unconfined or the kernel has no AppArmor. It reads
// the AppArmor-specific attribute alone, never /proc/self/attr/current,
// which holds another security module's label where AppArmor is not the
// one in charge.
func appArmorProfile() string {
	b, err := os.ReadFile("/proc/self/attr/apparmor/current")
	if err != nil {
		return ""
	}
	return parseAppArmorLabel(string(b))
}

// parseAppArmorLabel returns the profile of a label as the kernel gives
// it, "NAME (MODE)", or "" for "unconfined".
func parseAppArmorLabel(label string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(label), " ")
	if name == "unconfined" {
		return ""
	}
	return name
}

// providerFileFix is the fix for err, the refusal of a provider's
// settings, when err is a file ariesd may not open and ariesd runs
// under the AppArmor profile profile: the profile reads provider files
// only under AppArmorLoginDir, so a path elsewhere is refused this way
// whatever its owner and mode. It returns "" otherwise.
func providerFileFix(err error, profile string) string {
	if profile == "" || !errors.Is(err, fs.ErrPermission) {
		return ""
	}
	return fmt.Sprintf("; fix: ariesd runs under the AppArmor profile %s, which reads a provider's files only under %s/ (@{ariesd_login}, deploy/apparmor/tunables/ariesd): "+
		"move the file there, owned by aries and mode 0600 for a secret, and name its new path in the config file (docs/server.md, Sign-in)", profile, AppArmorLoginDir)
}
