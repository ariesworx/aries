#!/bin/sh
# Set up an Aries server on a fresh Ubuntu 24.04 LTS or 26.04 LTS
# machine: what the steps of docs/server.md do by hand, for a server of
# many projects, with sshd reading the keys ariesd writes from its
# control database.
#
#   curl -fsSLO https://raw.githubusercontent.com/ariesworx/aries/main/install-server.sh
#   sudo sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com
#
# Run with -h for the options. Each step checks before it acts, so a
# second run changes nothing and says so, and a run that was interrupted
# is finished by running it again. --dry-run prints what it would do.
#
# docs/server.md is the reference: "With the installer" maps each step
# below to the section it follows, and scripts/installserver_test.go
# fails when the two drift. The binaries are installed and verified by
# install.sh, which holds the release key; this script holds none.
set -eu

usage() {
	cat <<'EOF'
usage: install-server.sh --admin NAME --admin-key FILE [--host HOST] [--port PORT]
                         [--project NAME] [--version vX.Y.Z] [--releases SOURCE]
                         [--apparmor] [--dry-run]

Sets up an Aries server on Ubuntu 24.04 LTS or 26.04 LTS, as root,
following docs/server.md: Dolt, ariesd and aries, the database, the
config file, the systemd units, sshd, and the first admin. Prints the
.aries.yaml a repository commits.

  --admin NAME       the first server admin's principal name
  --admin-key FILE   that admin's SSH public key (a .pub file)
  --host HOST        the address clients connect to, for .aries.yaml
                     (default: this machine's hostname -f)
  --port PORT        sshd's port, if not 22, for .aries.yaml
  --project NAME     the code name of the first project (default: first)
  --version V        the aries release to install (default: the latest)
  --releases SOURCE  where releases are read from, as install.sh's
                     ARIES_RELEASES; also written to the config file
                     for ariesd upgrade (default: $ARIES_RELEASES, or
                     GitHub)
  --apparmor         also confine ariesd with the AppArmor profiles
  --dry-run          print every action, and change nothing
  -h, --help         show this help

install.sh and the AppArmor profiles are read from beside this script
when it is run from a checkout of aries, and otherwise downloaded from
https://raw.githubusercontent.com/ariesworx/aries/main, as the guide
downloads them. Dolt is downloaded from GitHub (ARIES_DOLT_BASE_URL
replaces https://github.com/dolthub/dolt/releases/download) and checked
against the SHA-256 pinned below.

No secret is ever an argument or printed: the database password is
generated into the 0600 config file, and Dolt's root password is
generated and forgotten.
EOF
}

# The Dolt release Aries is tested with: version.Dolt and
# internal/version/dolt.sha256, which a test keeps these equal to.
dolt_version=2.4.2
dolt_sha256_amd64=5d95a271149dc6ea97f47157af7ba9a0df5e8d4e9bbac4d79c169f2faa3cfb9e
dolt_sha256_arm64=c1c610e22c6bacd4c5b2fbf5697a0f453d759d075c4abff380c6e646553e22d8

raw_url=https://raw.githubusercontent.com/ariesworx/aries/main
config=/etc/aries/ariesd.yaml
home=/var/lib/aries
authorized=$home/.ssh/authorized_keys
keys_file=$home/.ssh/aries_keys
control_db=aries_control
project_dbs_prefix=aries_p
project_dbs_count=100
dolt_metrics_url=http://127.0.0.1:11228/metrics

say() { printf '%s\n' "$*"; }
die() { # message fix
	printf 'install-server.sh: %s\n' "$1" >&2
	if [ -n "${2:-}" ]; then printf 'fix: %s\n' "$2" >&2; fi
	exit 1
}
usage_die() { # message fix
	printf 'install-server.sh: %s\n' "$1" >&2
	printf 'fix: %s\n' "$2" >&2
	exit 2
}
have() { _have=$(command -v "$1" 2>&1) && [ -n "$_have" ]; }

admin='' admin_key='' host='' port='' project=first version='' dry=0 apparmor=0
releases=${ARIES_RELEASES:-}
while [ $# -gt 0 ]; do
	case $1 in
	--admin | --admin-key | --host | --port | --project | --version | --releases)
		[ $# -ge 2 ] || usage_die "$1 needs a value" "run with -h for the options"
		_opt=$1 _val=$2
		shift ;;
	--admin=* | --admin-key=* | --host=* | --port=* | --project=* | --version=* | --releases=*)
		_opt=${1%%=*} _val=${1#*=} ;;
	--apparmor | --dry-run) _opt=$1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; usage_die "unknown option $1" "run with -h for the options" ;;
	esac
	case $_opt in
	--admin) admin=$_val ;;
	--admin-key) admin_key=$_val ;;
	--host) host=$_val ;;
	--port) port=$_val ;;
	--project) project=$_val ;;
	--version) version=$_val ;;
	--releases) releases=$_val ;;
	--apparmor) apparmor=1 ;;
	--dry-run) dry=1 ;;
	esac
	shift
done

# The options are checked before the machine is, so a mistake costs nothing.
[ -n "$admin" ] || usage_die "--admin is required" "--admin alice, the first server admin's principal name"
printf '%s\n' "$admin" | grep -Eq '^[a-z][a-z0-9._-]{0,63}$' ||
	usage_die "--admin $admin is not a principal name" "use a lowercase letter, then up to 63 lowercase letters, digits, '.', '_' or '-'"
case $admin in ariesd | import | ariesd-upgrade | ariesd-status)
	usage_die "--admin $admin is a name the server uses itself" "choose another name" ;;
esac
[ -n "$admin_key" ] || usage_die "--admin-key is required" "--admin-key alice.pub, the admin's SSH public key"
if [ ! -f "$admin_key" ] || [ ! -r "$admin_key" ]; then
	usage_die "cannot read --admin-key $admin_key" "name the admin's public key file, such as id_ed25519.pub"
fi
if grep -q 'PRIVATE KEY' "$admin_key"; then
	usage_die "--admin-key $admin_key is a private key, which this installer never uses" "name the public key, the file that ends in .pub"
fi
# One line, "TYPE BASE64 [COMMENT]", with nothing that would need quoting
# in authorized_keys.
admin_line=$(grep -v -e '^[[:space:]]*$' -e '^#' "$admin_key")
case $admin_line in *'
'*) usage_die "--admin-key $admin_key holds more than one key" "name a file with one public key" ;;
esac
printf '%s\n' "$admin_line" | grep -Eq '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com) [A-Za-z0-9+/]+={0,3}( [A-Za-z0-9@._+=:-]*)?$' ||
	usage_die "--admin-key $admin_key is not an SSH public key line" "name a file such as id_ed25519.pub: TYPE KEY [COMMENT], with a plain comment"
admin_key_type=$(printf '%s\n' "$admin_line" | cut -d' ' -f1)
admin_key_blob=$(printf '%s\n' "$admin_line" | cut -d' ' -f2)
admin_key_comment=$(printf '%s\n' "$admin_line" | cut -s -d' ' -f3)
if ! printf '%s\n' "$project" | grep -Eq '^[a-z0-9]+(-[a-z0-9]+)*$' || [ ${#project} -gt 64 ]; then
	usage_die "--project $project is not a code name" "use lowercase letters and digits, with inner hyphens, such as acme"
fi
if [ -n "$host" ]; then
	printf '%s\n' "$host" | grep -Eq '^[A-Za-z0-9.:-]+$' ||
		usage_die "--host $host is not a host name or an address" "give the name or address clients reach sshd at, such as aries.example.com"
fi
if [ -n "$port" ]; then
	if ! printf '%s\n' "$port" | grep -Eq '^[1-9][0-9]{0,4}$' || [ "$port" -gt 65535 ]; then
		usage_die "--port $port is not a port" "give sshd's port, such as 2222"
	fi
fi
if [ -n "$version" ]; then
	printf '%s\n' "$version" | grep -Eq '^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
		usage_die "--version $version is not a release version" "--version v0.6.0"
fi
case $releases in
'' | github | https://* | http://127.0.0.1* | 'http://[::1]'* | file:///* | file://localhost/* | /*) ;;
*) usage_die "--releases is not github, an https URL, a file:// URL or an absolute directory" "see docs/install.md, Where releases are read from" ;;
esac
case $releases in *[[:space:]\'\"\\]*)
	usage_die "--releases holds a space or a quote" "give the plain URL or path of the mirror" ;;
esac

# The machine: an Ubuntu LTS release docs/server.md was run on, which
# scripts/installserver_test.go holds to the guide's list.
ubuntu_versions='24.04 26.04'
releases_text='Ubuntu 24.04 LTS or 26.04 LTS'
os_id=$(sed -n 's/^ID=//p' /etc/os-release 2>/dev/null | tr -d '"')
os_version=$(sed -n 's/^VERSION_ID=//p' /etc/os-release 2>/dev/null | tr -d '"')
if [ "$os_id" != ubuntu ]; then
	die "this installer runs only on $releases_text, and this machine is not Ubuntu" \
		"run it on one of those, or follow docs/server.md by hand (other distributions are untested)"
fi
case " $ubuntu_versions " in
*" $os_version "*) ;;
*) die "this installer runs only on $releases_text, and this machine is Ubuntu $os_version" \
	"run it on one of those, or follow docs/server.md by hand" ;;
esac
[ "$(id -u)" = 0 ] || die "this installer must run as root" "run it with sudo: sudo sh install-server.sh ..."

# Where install.sh and the AppArmor profiles come from: beside this
# script in a checkout, or else where the guide downloads them.
here=
case $0 in
*/install-server.sh) here=${0%/*} ;;
install-server.sh) here=. ;;
esac
if [ -n "$here" ] && [ -f "$here/install.sh" ]; then
	from=$here
else
	from=$raw_url
fi

tmp=$(mktemp -d /tmp/aries-install-server.XXXXXX) || die "cannot make a temporary directory" "check that /tmp is writable"
pending=
cleanup() { rm -rf "$tmp"; [ -z "$pending" ] || rm -f "$pending"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

changes=0
daemon_reload=0
restart_ariesd=0
aa_exec=

# quote prints its arguments as a shell would need them typed.
quote() {
	_q=
	for _a in "$@"; do
		case $_a in
		'' | *[!A-Za-z0-9_./:=@,+%-]*) _a="'$(printf '%s' "$_a" | sed "s/'/'\\\\''/g")'" ;;
		esac
		_q="$_q${_q:+ }$_a"
	done
	printf '%s' "$_q"
}

step() { printf '\n%s. %s\n' "$1" "$2"; }
ok() { printf '   ok: %s\n' "$*"; }
note() { printf '   %s\n' "$*"; }

# act runs a command that changes the machine, printing it first. No
# command given to act holds a secret.
act() {
	changes=$((changes + 1))
	if [ "$dry" = 1 ]; then
		say "   would run: $(quote "$@")"
		return 0
	fi
	say "   run: $(quote "$@")"
	"$@" || die "$(quote "$@") failed" "read the error above, correct it, and run this again: it skips every step that is done"
}

# act_in DIR CMD... is act, run from DIR: Dolt's and ariesd's own
# commands need a working directory their user can read.
act_in() {
	changes=$((changes + 1))
	_in=$1
	shift
	if [ "$dry" = 1 ]; then
		say "   would run, in $_in: $(quote "$@")"
		return 0
	fi
	say "   run, in $_in: $(quote "$@")"
	(cd "$_in" && "$@") || die "$(quote "$@") failed" "read the error above, correct it, and run this again: it skips every step that is done"
}

# meta PATH MODE OWNER GROUP sets what differs of a file's mode and owner.
meta() {
	[ -e "$1" ] || return 0
	_have_meta=$(stat -c '%a %U %G' "$1")
	if [ "$_have_meta" != "$2 $3 $4" ]; then
		note "$1 is $_have_meta, not $2 $3 $4"
		act chown "$3:$4" "$1"
		act chmod "$2" "$1"
	fi
}

# ensure_dir PATH OWNER GROUP MODE makes a directory, or sets what differs.
ensure_dir() {
	if [ -d "$1" ]; then
		_have_meta=$(stat -c '%U %G %a' "$1")
		if [ "$_have_meta" = "$2 $3 $4" ]; then
			ok "$1"
			return 0
		fi
		note "$1 is $_have_meta, not $2 $3 $4"
	fi
	act install -d -o "$2" -g "$3" -m "$4" "$1"
}

# put PATH MODE OWNER GROUP GENERATOR [SOURCE] writes the file GENERATOR
# prints, or leaves it if it is already that. A file that holds anything
# else is not changed: its difference is shown, and the run stops. Sets
# wrote=1 when it writes. A dry run prints the text, or names SOURCE, a
# file of the repository, where the text is long.
put() {
	wrote=0
	"$5" >"$tmp/new" || die "cannot make $1" "report it at https://github.com/ariesworx/aries/issues"
	if [ -e "$1" ] || [ -L "$1" ]; then
		if [ -f "$1" ] && [ ! -L "$1" ] && cmp -s "$tmp/new" "$1"; then
			ok "$1"
			meta "$1" "$2" "$3" "$4"
			return 0
		fi
		say "   $1 is not what this installer writes:"
		diff -u "$1" "$tmp/new" | sed 's/^/     /' || true
		die "$1 exists and differs from docs/server.md's; nothing in it was changed" \
			"make it the guide's (the + lines above), or move it aside, and run this again"
	fi
	changes=$((changes + 1))
	wrote=1
	if [ "$dry" = 1 ] && [ -n "${6:-}" ]; then
		say "   would write $1, mode $2, owner $3:$4: $6 from $from, $(wc -l <"$tmp/new" | tr -d ' ') lines"
		return 0
	fi
	if [ "$dry" = 1 ]; then
		say "   would write $1, mode $2, owner $3:$4:"
		sed 's/^/     | /' "$tmp/new"
		return 0
	fi
	say "   write $1, mode $2, owner $3:$4"
	# Beside its name and then renamed, so an interrupted run leaves the
	# old file or the new one, never half of one.
	pending=${1%/*}/.${1##*/}.install-server
	install -o "$3" -g "$4" -m "$2" "$tmp/new" "$pending" || die "cannot write $1" "read the error above"
	mv -f "$pending" "$1" || die "cannot write $1" "read the error above"
	pending=
}

# --- The files whose text docs/server.md gives. scripts/installserver_test.go
# --- holds each to the guide's, byte for byte.

# file: /etc/dolt/config.yaml
dolt_config() {
	cat <<'EOF'
log_level: info
listener:
  host: 127.0.0.1
  port: 3306
metrics:
  host: 127.0.0.1
  port: 11228
  labels: {}
data_dir: /var/lib/dolt/data
cfg_dir: /var/lib/dolt/cfg
system_variables:
  secure_file_priv: /var/lib/dolt/no-files
EOF
}

# file: /etc/systemd/system/dolt.service
dolt_unit() {
	cat <<'EOF'
[Unit]
Description=Dolt SQL server for aries
After=network.target

[Service]
User=dolt
Group=dolt
Environment=HOME=/var/lib/dolt
WorkingDirectory=/var/lib/dolt/data
ExecStart=/usr/local/bin/dolt sql-server --config /etc/dolt/config.yaml
ExecStartPost=/bin/bash -c 'for _ in {1..300}; do (exec 3<>/dev/tcp/127.0.0.1/3306) 2>/dev/null && exit 0; sleep 0.2; done; echo "Dolt is not listening on 127.0.0.1:3306" >&2; exit 1'
Restart=on-failure
RestartSec=5
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/dolt
PrivateTmp=yes
PrivateDevices=yes
IPAddressDeny=any
IPAddressAllow=localhost
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
EOF
}

# file: /etc/systemd/system/ariesd.service
ariesd_unit() {
	cat <<'EOF'
[Unit]
Description=Aries server
After=network.target dolt.service time-sync.target
Wants=time-sync.target
Requires=dolt.service

[Service]
Type=notify
NotifyAccess=main
TimeoutStartSec=600
WatchdogSec=120
User=aries
Group=aries
ExecStart=/opt/aries/bin/ariesd serve
Restart=on-failure
RestartSec=5
RestartPreventExitStatus=75
RuntimeDirectory=aries
RuntimeDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
EOF
}

# file: /etc/systemd/system/ariesd.service.d/keys.conf
keys_dropin() {
	cat <<'EOF'
[Service]
ReadWritePaths=/var/lib/aries/.ssh
EOF
}

# file: /etc/sudoers.d/aries
sudoers() {
	cat <<'EOF'
aries ALL=(root) NOPASSWD: /usr/bin/systemctl restart ariesd.service
EOF
}

# file: /etc/ssh/sshd_config.d/00-aries.conf
sshd_dropin() {
	cat <<'EOF'
MaxStartups 10:30:60
ClientAliveInterval 60
Match User aries
    AuthenticationMethods publickey
    DisableForwarding yes
    PermitTTY no
    AuthorizedKeysFile .ssh/aries_keys .ssh/authorized_keys
    AuthorizedKeysCommand none
    TrustedUserCAKeys none
    AuthorizedPrincipalsFile none
EOF
}

# file: /etc/systemd/system/ariesd.service.d/apparmor.conf
apparmor_dropin() {
	cat <<'EOF'
[Service]
AppArmorProfile=ariesd-serve
EOF
}

# The profiles come from beside this script or from the repository, and
# are the guide's because they are deploy/apparmor's.
apparmor_profile() { cat "$tmp/apparmor/ariesd"; }
apparmor_tunables() { cat "$tmp/apparmor/tunables"; }

# fetch SOURCE-RELATIVE-PATH FILE copies a file of the repository from
# beside this script, or downloads it.
fetch() {
	case $from in
	https://*) curl -fsSL --proto '=https' --retry 2 -o "$2" "$from/$1" ;;
	*) cp "$from/$1" "$2" ;;
	esac
}

# dolt_sql runs SQL from stdin as Dolt's own user, from the data
# directory, which reaches the running server as Dolt's local account:
# the way docs/server.md administers Dolt once root's password is
# forgotten. Its output is the query's, as CSV.
dolt_sql() {
	(cd /var/lib/dolt/data && sudo -u dolt -H dolt sql -r csv)
}

dolt_running() { systemctl is-active --quiet dolt 2>/dev/null; }

#############################################################################

check_machine() {
	step 1 'Check the machine'
	ok "Ubuntu $os_version"
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 dolt_sha256=$dolt_sha256_amd64 ;;
	aarch64 | arm64) arch=arm64 dolt_sha256=$dolt_sha256_arm64 ;;
	*) die "unsupported architecture $(uname -m)" "aries and Dolt release amd64 and arm64" ;;
	esac
	ok "$arch"
	[ -d /run/systemd/system ] || die "systemd is not running this machine" \
		"run this on a machine booted with systemd; docs/server.md's units need it"
	ok systemd
	_missing=
	for _c in curl openssl sudo visudo tar sha256sum install cmp diff stat useradd usermod getent ssh-keygen systemctl journalctl flock; do
		have "$_c" || _missing="$_missing $_c"
	done
	[ -x /usr/sbin/sshd ] || _missing="$_missing sshd"
	[ -z "$_missing" ] || die "missing:$_missing" \
		"apt-get install -y openssh-server curl openssl sudo, then run this again"
	ok "the commands it needs"
	grep -Eqi '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*\.conf' /etc/ssh/sshd_config ||
		die "/etc/ssh/sshd_config does not include /etc/ssh/sshd_config.d/*.conf" \
			"add 'Include /etc/ssh/sshd_config.d/*.conf' at its top, as Ubuntu ships it"
	ok "sshd reads /etc/ssh/sshd_config.d"
	[ -f /etc/ssh/ssh_host_ed25519_key.pub ] ||
		die "sshd has no ED25519 host key" "make the host keys: ssh-keygen -A"
	ok "sshd's ED25519 host key"
	ssh-keygen -lf "$admin_key" >/dev/null 2>&1 ||
		die "ssh-keygen cannot read --admin-key $admin_key" "name the admin's public key file, such as id_ed25519.pub"
	ok "the admin's key: $(ssh-keygen -lf "$admin_key" | cut -d' ' -f2)"
	if [ "$apparmor" = 1 ]; then
		if [ ! -d /sys/kernel/security/apparmor ] || ! have apparmor_parser || ! have aa-exec; then
			die "--apparmor, but AppArmor is not running here, or apparmor_parser or aa-exec is missing" \
				"apt-get install -y apparmor on a kernel with AppArmor, or leave out --apparmor"
		fi
		ok AppArmor
	fi
	if [ "$dry" = 0 ]; then
		exec 9>/run/lock/aries-install-server.lock
		flock -n 9 || die "another run of this installer holds /run/lock/aries-install-server.lock" "wait for it to finish"
	fi
}

install_dolt() {
	step 2 'Install Dolt'
	if [ -e /usr/local/bin/dolt ]; then
		_got=$(HOME=$tmp /usr/local/bin/dolt version 2>/dev/null | head -n 1) || _got=
		[ "$_got" = "dolt version $dolt_version" ] ||
			die "/usr/local/bin/dolt is not Dolt $dolt_version (it says: ${_got:-nothing}); nothing was changed" \
				"aries is tested with Dolt $dolt_version: install it as docs/server.md step 1 says, or move the other aside"
		ok "Dolt $dolt_version in /usr/local/bin"
		return 0
	fi
	_archive=dolt-linux-$arch.tar.gz
	_base=${ARIES_DOLT_BASE_URL:-https://github.com/dolthub/dolt/releases/download}
	_url=${_base%/}/v$dolt_version/$_archive
	if [ "$dry" = 1 ]; then
		changes=$((changes + 1))
		say "   would download $_url, check its SHA-256 is $dolt_sha256, and install its bin/dolt as /usr/local/bin/dolt"
		return 0
	fi
	say "   download $_url"
	curl -fsSL --retry 2 -o "$tmp/$_archive" "$_url" || die "cannot download $_url" "check the network"
	_got=$(sha256sum "$tmp/$_archive" | cut -d' ' -f1)
	[ "$_got" = "$dolt_sha256" ] || die "checksum mismatch for $_archive (got $_got, want $dolt_sha256); nothing was installed" \
		"retry; if it persists, do not use this download, and report it at https://github.com/ariesworx/aries/issues"
	say "   SHA-256 $_got, as pinned"
	tar -xzf "$tmp/$_archive" -C "$tmp" "dolt-linux-$arch/bin/dolt" || die "cannot unpack $_archive" "retry"
	_got=$(HOME=$tmp "$tmp/dolt-linux-$arch/bin/dolt" version 2>/dev/null | head -n 1) || _got=
	[ "$_got" = "dolt version $dolt_version" ] || die "the dolt in $_archive does not run here; nothing was installed" \
		"report it at https://github.com/ariesworx/aries/issues"
	act install -m 0755 "$tmp/dolt-linux-$arch/bin/dolt" /usr/local/bin/dolt
}

run_dolt() {
	step 3 'Run Dolt under its own account'
	if getent passwd dolt >/dev/null; then
		_dhome=$(getent passwd dolt | cut -d: -f6)
		[ "$_dhome" = /var/lib/dolt ] || die "the user dolt exists with home $_dhome, not /var/lib/dolt" \
			"this machine has another Dolt account; follow docs/server.md by hand"
		ok "user dolt"
	else
		act useradd --system --home-dir /var/lib/dolt --shell /usr/sbin/nologin dolt
	fi
	ensure_dir /var/lib/dolt dolt dolt 700
	ensure_dir /var/lib/dolt/data dolt dolt 700
	ensure_dir /var/lib/dolt/cfg dolt dolt 700
	ensure_dir /etc/dolt root root 755
	put /etc/dolt/config.yaml 644 root root dolt_config
	for _kv in 'user.name=aries server' 'user.email=aries@localhost' 'metrics.disabled=true'; do
		_k=${_kv%%=*} _v=${_kv#*=}
		_got=
		if [ -d /var/lib/dolt ] && getent passwd dolt >/dev/null; then
			_got=$(cd /var/lib/dolt && sudo -u dolt -H dolt config --global --get "$_k" 2>/dev/null) || _got=
		fi
		if [ "$_got" = "$_v" ]; then
			ok "dolt config $_k"
		else
			act_in /var/lib/dolt sudo -u dolt -H dolt config --global --add "$_k" "$_v"
		fi
	done
	put /etc/systemd/system/dolt.service 644 root root dolt_unit
	[ "$wrote" = 0 ] || daemon_reload=1
	reload_units
	if systemctl is-enabled --quiet dolt 2>/dev/null; then
		ok "dolt.service enabled"
	else
		act systemctl enable dolt
	fi
	if dolt_running; then
		ok "dolt.service running"
	else
		act systemctl start dolt
	fi
	if [ "$dry" = 0 ]; then
		_n=$(curl -s --retry 5 --retry-connrefused http://127.0.0.1:11228/metrics | grep -c '^dss_') || _n=0
		[ "$_n" -gt 0 ] || die "Dolt's metrics do not answer at http://127.0.0.1:11228/metrics" "read journalctl -u dolt"
		ok "Dolt answers: $_n lines of its SQL server's metrics"
	fi
}

# reload_units has systemd read the unit files again when this run wrote
# one, or when systemd says it has not read one since it changed: a run
# interrupted between the two leaves that.
reload_units() {
	for _u in dolt.service ariesd.service; do
		if [ "$(systemctl show -p NeedDaemonReload --value "$_u" 2>/dev/null)" = yes ]; then
			daemon_reload=1
		fi
	done
	if [ "$daemon_reload" = 1 ]; then
		act systemctl daemon-reload
		daemon_reload=0
	fi
}

install_ariesd() {
	step 4 'Install ariesd'
	if getent passwd aries >/dev/null; then
		_shome=$(getent passwd aries | cut -d: -f6)
		_shell=$(getent passwd aries | cut -d: -f7)
		[ "$_shome" = "$home" ] || die "the user aries exists with home $_shome, not $home" \
			"this machine has another aries account; follow docs/server.md by hand"
		[ "$_shell" = /bin/sh ] || die "the user aries has the shell $_shell; sshd runs each key's forced command through it" \
			"usermod -s /bin/sh aries, then run this again"
		ok "user aries"
	else
		act useradd --system --create-home --home-dir "$home" --shell /bin/sh aries
	fi
	_pw=$(getent shadow aries | cut -d: -f2) || _pw=
	if [ "$_pw" = '*' ]; then
		ok "aries's password is *, which nothing matches"
	else
		act usermod -p '*' aries
	fi
	ensure_dir /opt/aries/bin aries aries 755
	if [ -x /opt/aries/bin/ariesd ]; then
		ok "$(/opt/aries/bin/ariesd version 2>&1 | head -n 1) in /opt/aries/bin (ariesd upgrade replaces it)"
	else
		run_installer aries "$home" --server --dir /opt/aries/bin
	fi
	if [ "$(readlink /usr/local/bin/ariesd 2>/dev/null)" = /opt/aries/bin/ariesd ]; then
		ok "/usr/local/bin/ariesd links to /opt/aries/bin/ariesd"
	elif [ -e /usr/local/bin/ariesd ] || [ -L /usr/local/bin/ariesd ]; then
		die "/usr/local/bin/ariesd exists and is not a link to /opt/aries/bin/ariesd; nothing was changed" \
			"move it aside, and run this again"
	else
		act ln -sfn /opt/aries/bin/ariesd /usr/local/bin/ariesd
	fi
	# aries beside it, for the operator to try the server from the server.
	if [ -x /usr/local/bin/aries ]; then
		ok "$(/usr/local/bin/aries version 2>&1 | head -n 1) in /usr/local/bin"
	else
		run_installer root / --dir /usr/local/bin
	fi
}

# run_installer USER DIR ARGS... runs install.sh as USER in DIR, which
# checks the release's signature and checksum. It is given only the
# variables it documents; ARIES_PUBKEY_PEM is for tests, and install.sh
# warns when it is set.
run_installer() {
	_user=$1 _in=$2
	shift 2
	set -- "$@" --require-signature
	[ -z "$version" ] || set -- "$@" --version "$version"
	_args=$(quote "$@")
	set -- sh -s -- "$@"
	_show=
	if [ -n "$releases" ]; then
		set -- "ARIES_RELEASES=$releases" "$@"
		_show="$_show $(quote "ARIES_RELEASES=$releases")"
	fi
	if [ -n "${ARIES_BASE_URL:-}" ]; then
		set -- "ARIES_BASE_URL=$ARIES_BASE_URL" "$@"
		_show="$_show $(quote "ARIES_BASE_URL=$ARIES_BASE_URL")"
	fi
	if [ -n "${ARIES_PUBKEY_PEM:-}" ]; then
		set -- "ARIES_PUBKEY_PEM=$ARIES_PUBKEY_PEM" "$@"
		_show="$_show ARIES_PUBKEY_PEM=(as set)"
	fi
	_show="as $_user, in $_in: ${_show:+env$_show }sh install.sh $_args"
	changes=$((changes + 1))
	if [ "$dry" = 1 ]; then
		say "   would run, $_show"
		return 0
	fi
	if [ ! -f "$tmp/install.sh" ]; then
		fetch install.sh "$tmp/install.sh" || die "cannot read install.sh from $from" "check the network, or run this from a checkout of aries"
	fi
	say "   run, $_show"
	# install.sh is read from stdin, so the user need not be able to
	# read this run's temporary directory.
	# shellcheck disable=SC2024 # root opens it, on purpose
	(cd "$_in" && sudo -u "$_user" -H env "$@" <"$tmp/install.sh") ||
		die "install.sh did not install what it was asked to" "read its error above"
}

# setting KEY prints a top-level key's value from the config file.
setting() { sed -n "s/^$1: *//p" "$config"; }

# the_config prints the config file, with PASSWORD and PROJECT.
the_config() {
	printf 'dsn: aries:%s@tcp(127.0.0.1:3306)/aries\nproject: %s\nsystemd_unit: ariesd.service\nadmins: [%s]\n' \
		"$1" "$2" "$admin"
	printf 'control_db: %s\nproject_dbs:\n  prefix: %s\n  count: %s\n' "$control_db" "$project_dbs_prefix" "$project_dbs_count"
	[ -z "$releases" ] || printf 'releases: %s\n' "$releases"
	printf 'telemetry:\n  dolt_metrics_url: %s\n' "$dolt_metrics_url"
}

# the_grants prints the database names ariesd's account is granted.
the_grants() {
	printf '%s\n%s\n' aries "$control_db"
	_i=1
	while [ "$_i" -le "$project_dbs_count" ]; do
		printf '%s%03d\n' "$project_dbs_prefix" "$_i"
		_i=$((_i + 1))
	done
}

create_database() {
	step 5 'Create the databases and the config file'
	ensure_dir /etc/aries root aries 750
	_user_exists=unknown _dbs='' _granted=''
	if [ -e /usr/local/bin/dolt ] && dolt_running; then
		_user_exists=$(echo "SELECT COUNT(*) FROM mysql.user WHERE user = 'aries' AND host = 'localhost';" | dolt_sql | tail -n 1) ||
			die "cannot query Dolt as its own user" "read journalctl -u dolt"
		_dbs=$(echo 'SHOW DATABASES;' | dolt_sql | tail -n +2) || die "cannot query Dolt" "read journalctl -u dolt"
		_granted=$(echo "SELECT db FROM mysql.db WHERE user = 'aries' AND host = 'localhost';" | dolt_sql | tail -n +2) ||
			die "cannot query Dolt" "read journalctl -u dolt"
	fi

	# The config file first, so that the password is kept before Dolt
	# has it; a run interrupted after this reads it back.
	if [ -e "$config" ]; then
		meta "$config" 600 aries aries
		_dsn=$(setting dsn)
		_pw=$(printf '%s\n' "$_dsn" | sed -n 's/^aries:\([0-9a-f]*\)@tcp(127\.0\.0\.1:3306)\/aries$/\1/p')
		[ -n "$_pw" ] || die "$config has a dsn this installer did not write; nothing was changed" \
			"this server was set up another way: follow docs/server.md by hand"
		[ "$(setting control_db)" = "$control_db" ] || die "$config names no control database $control_db; nothing was changed" \
			"this is a server of one project: follow docs/server.md, Serving many projects, Opt in, by hand"
		case " $(setting admins | tr -d '[]",') " in
		*" $admin "*) ;;
		*) die "$config does not name $admin under admins:; nothing was changed" \
			"run again with --admin set to an admin it names ($(setting admins)), or add $admin there and restart ariesd" ;;
		esac
		ok "$config, project $(setting project)"
	else
		[ "$_user_exists" = 0 ] || [ "$_user_exists" = unknown ] ||
			die "Dolt has an account aries but there is no $config; nothing was changed" \
				"restore $config from your backup, or drop the account: cd /var/lib/dolt/data && sudo -u dolt -H dolt sql -q \"DROP USER 'aries'@'localhost'\""
		_pw=$(openssl rand -hex 24) || die "openssl cannot make a password" "read the error above"
		_project=$(cat /proc/sys/kernel/random/uuid)
		changes=$((changes + 1))
		if [ "$dry" = 1 ]; then
			say "   would write $config, mode 600, owner aries:aries, with a generated password and project:"
			the_config '<generated>' '<generated>' | sed 's/^/     | /'
		else
			say "   write $config, mode 600, owner aries:aries, with a generated password"
			pending=/etc/aries/.ariesd.yaml.install-server
			if ! (umask 077 && the_config "$_pw" "$_project" >"$pending") ||
				! chown aries:aries "$pending" || ! mv -f "$pending" "$config"; then
				die "cannot write $config" "read the error above"
			fi
			pending=
			note "project $_project"
		fi
	fi

	if [ "$_user_exists" = unknown ]; then
		changes=$((changes + 1))
		say "   would run, as dolt in /var/lib/dolt/data, once Dolt runs: sudo -u dolt -H dolt sql, with on stdin:"
		say "     CREATE DATABASE aries; CREATE DATABASE $control_db;"
		say "     CREATE USER 'aries'@'localhost' IDENTIFIED BY '<the config's password>';"
		say "     GRANT ALL ON aries.*, $control_db.* and ${project_dbs_prefix}001.* to ${project_dbs_prefix}$(printf %03d "$project_dbs_count").* TO 'aries'@'localhost';"
		say "     ALTER USER 'root'@'localhost' IDENTIFIED BY '<generated, and forgotten>';"
		return 0
	fi

	# Each statement only where it is missing. They travel on stdin; only
	# the ones without a password are printed as they are.
	: >"$tmp/sql"
	for _db in aries "$control_db"; do
		if printf '%s\n' "$_dbs" | grep -qx "$_db"; then
			ok "database $_db"
		else
			printf 'CREATE DATABASE %s;\n' "$_db" >>"$tmp/sql"
		fi
	done
	if [ "$_user_exists" = 0 ]; then
		printf "CREATE USER 'aries'@'localhost' IDENTIFIED BY '%s';\n" "$_pw" >"$tmp/sql.user"
	else
		ok "Dolt account aries"
		: >"$tmp/sql.user"
	fi
	_ng=0
	for _db in $(the_grants); do
		if ! printf '%s\n' "$_granted" | grep -qx "$_db"; then
			printf "GRANT ALL ON %s.* TO 'aries'@'localhost';\n" "$_db" >>"$tmp/sql.grants"
			_ng=$((_ng + 1))
		fi
	done
	if [ "$_ng" = 0 ]; then
		ok "aries's account is granted its $((project_dbs_count + 2)) databases"
	fi
	# Root still opens with no password only until this has run once.
	_rootpw=
	if echo 'SELECT 1;' | (cd /var/lib/dolt && sudo -u dolt -H env DOLT_CLI_PASSWORD= dolt --host 127.0.0.1 --port 3306 --no-tls -u root sql) >/dev/null 2>&1; then
		_rootpw=$(openssl rand -hex 24) || die "openssl cannot make a password" "read the error above"
	else
		ok "Dolt's root has a password"
	fi
	if [ ! -s "$tmp/sql" ] && [ ! -s "$tmp/sql.user" ] && [ "$_ng" = 0 ] && [ -z "$_rootpw" ]; then
		return 0
	fi
	sed 's/^/     /' "$tmp/sql"
	[ ! -s "$tmp/sql.user" ] || say "     CREATE USER 'aries'@'localhost' IDENTIFIED BY '<the config's password>';"
	[ "$_ng" = 0 ] || say "     GRANT ALL ON each of $_ng databases TO 'aries'@'localhost'; (aries, $control_db, ${project_dbs_prefix}001 to ${project_dbs_prefix}$(printf %03d "$project_dbs_count"))"
	[ -z "$_rootpw" ] || say "     ALTER USER 'root'@'localhost' IDENTIFIED BY '<generated, and forgotten>';"
	changes=$((changes + 1))
	if [ "$dry" = 1 ]; then
		say "   would run the statements above, as dolt in /var/lib/dolt/data: sudo -u dolt -H dolt sql, on stdin"
		return 0
	fi
	say "   run the statements above, as dolt in /var/lib/dolt/data: sudo -u dolt -H dolt sql, on stdin"
	{
		cat "$tmp/sql" "$tmp/sql.user"
		[ "$_ng" = 0 ] || cat "$tmp/sql.grants"
		[ -z "$_rootpw" ] || printf "ALTER USER 'root'@'localhost' IDENTIFIED BY '%s';\n" "$_rootpw"
	} | dolt_sql >/dev/null || die "Dolt refused the statements" "read the error above, and run this again"
	rm -f "$tmp/sql.user"
}

run_ariesd_unit() {
	step 6 'Run ariesd under systemd'
	put /etc/systemd/system/ariesd.service 644 root root ariesd_unit
	[ "$wrote" = 0 ] || { daemon_reload=1; restart_ariesd=1; }
	ensure_dir /etc/systemd/system/ariesd.service.d root root 755
	put /etc/systemd/system/ariesd.service.d/keys.conf 644 root root keys_dropin
	[ "$wrote" = 0 ] || { daemon_reload=1; restart_ariesd=1; }
	# The sudo rule, checked by visudo before it is in place. sudo-rs's
	# visudo, Ubuntu 26.04's, says "parsed OK" despite -q; its errors go
	# to stderr.
	if [ ! -e /etc/sudoers.d/aries ]; then
		sudoers >"$tmp/sudoers"
		visudo -c -q -f "$tmp/sudoers" >/dev/null || die "visudo refuses the sudo rule" "report it at https://github.com/ariesworx/aries/issues"
	fi
	put /etc/sudoers.d/aries 440 root root sudoers
	reload_units
}

first_admin() {
	step 7 'Let the first admin in, and have sshd read the keys file'
	ensure_dir "$home/.ssh" aries aries 700
	_line="restrict,command=\"/usr/local/bin/ariesd stdio --principal $admin\" $admin_key_type $admin_key_blob${admin_key_comment:+ $admin_key_comment}"
	_found=
	if [ -f "$authorized" ]; then
		_found=$(awk -v t="$admin_key_type" -v b="$admin_key_blob" '{ for (i = 1; i < NF; i++) if ($i == t && $(i + 1) == b) { print; exit } }' "$authorized")
	fi
	if [ -n "$_found" ]; then
		case $_found in
		*"--principal $admin\""*) ok "$admin's key is in $authorized" ;;
		*) die "$authorized already has the admin's key for another principal; nothing was changed" \
			"name that principal with --admin, or remove its line" ;;
		esac
	else
		changes=$((changes + 1))
		if [ "$dry" = 1 ]; then
			say "   would add to $authorized:"
		else
			say "   add to $authorized:"
		fi
		say "     $_line"
		if [ "$dry" = 0 ]; then
			if [ ! -e "$authorized" ]; then
				install -o aries -g aries -m 0600 /dev/null "$authorized" || die "cannot create $authorized" "read the error above"
			elif [ -s "$authorized" ] && [ -n "$(tail -c 1 "$authorized")" ]; then
				# Its last line has no newline, and would run into this one.
				printf '\n' >>"$authorized" || die "cannot write $authorized" "read the error above"
			fi
			printf '%s\n' "$_line" >>"$authorized" || die "cannot write $authorized" "read the error above"
		fi
	fi
	meta "$authorized" 600 aries aries

	put /etc/ssh/sshd_config.d/00-aries.conf 644 root root sshd_dropin
	_sshd_new=$wrote
	if [ -d /run/sshd ]; then
		ok /run/sshd
	else
		act install -d -m 0755 /run/sshd
	fi
	if [ "$dry" = 1 ]; then
		[ "$_sshd_new" = 0 ] || say "   would run: sshd -t, and only if it passes: systemctl try-reload-or-restart ssh"
		return 0
	fi
	if ! /usr/sbin/sshd -t; then
		if [ "$_sshd_new" = 1 ]; then
			rm -f /etc/ssh/sshd_config.d/00-aries.conf
			die "sshd -t refuses the configuration with 00-aries.conf; the file was removed again and sshd was not reloaded" \
				"correct what sshd says above, and run this again"
		fi
		die "sshd -t refuses the configuration; sshd was not reloaded" "correct what sshd says above, and run this again"
	fi
	ok "sshd -t"
	if [ "$_sshd_new" = 1 ]; then
		act systemctl try-reload-or-restart ssh
	fi
}

confine() {
	step 8 'Confine ariesd with AppArmor'
	if [ -e /etc/systemd/system/ariesd.service.d/apparmor.conf ]; then
		aa_exec="aa-exec -p ariesd-admin --"
	fi
	if [ "$apparmor" = 0 ]; then
		if [ -n "$aa_exec" ]; then
			ok "installed by an earlier run; left as they are"
		else
			note "skipped: --apparmor installs them"
		fi
		return 0
	fi
	mkdir "$tmp/apparmor"
	fetch deploy/apparmor/ariesd "$tmp/apparmor/ariesd" || die "cannot read deploy/apparmor/ariesd from $from" "check the network"
	fetch deploy/apparmor/tunables/ariesd "$tmp/apparmor/tunables" || die "cannot read deploy/apparmor/tunables/ariesd from $from" "check the network"
	put /etc/apparmor.d/tunables/ariesd 644 root root apparmor_tunables deploy/apparmor/tunables/ariesd
	_aa_new=$wrote
	put /etc/apparmor.d/ariesd 644 root root apparmor_profile deploy/apparmor/ariesd
	[ "$wrote" = 0 ] || _aa_new=1
	if [ "$_aa_new" = 0 ] && grep -q '^ariesd-serve ' /sys/kernel/security/apparmor/profiles 2>/dev/null; then
		ok "the profiles are loaded"
	else
		act apparmor_parser -r /etc/apparmor.d/ariesd
	fi
	ensure_dir /etc/systemd/system/ariesd.service.d root root 755
	put /etc/systemd/system/ariesd.service.d/apparmor.conf 644 root root apparmor_dropin
	[ "$wrote" = 0 ] || { daemon_reload=1; restart_ariesd=1; }
	reload_units
	aa_exec="aa-exec -p ariesd-admin --"
}

adopt() {
	step 9 'Adopt the database as the first project'
	# control init adds what is new and leaves the rest, so it is run
	# every time; it says what it found.
	# shellcheck disable=SC2086 # aa_exec is words or none
	set -- sudo -u aries -H $aa_exec ariesd control init --name "$project"
	if [ "$dry" = 1 ]; then
		say "   would run, in $home, which adds only what is new: $(quote "$@")"
		return 0
	fi
	say "   run, in $home: $(quote "$@")"
	(cd "$home" && "$@") || die "ariesd control init failed" "read its error above, and run this again"
}

start_ariesd() {
	step 10 'Start ariesd'
	if systemctl is-enabled --quiet ariesd 2>/dev/null; then
		ok "ariesd.service enabled"
	else
		act systemctl enable ariesd
	fi
	if systemctl is-active --quiet ariesd 2>/dev/null; then
		if [ "$restart_ariesd" = 1 ]; then
			act systemctl restart ariesd
		else
			ok "ariesd.service running"
		fi
	else
		act systemctl start ariesd
	fi
	[ "$dry" = 0 ] || return 0
	[ -f "$keys_file" ] || die "ariesd runs but wrote no $keys_file" "read journalctl -u ariesd"
	_m=$(stat -c '%a %U' "$keys_file")
	[ "$_m" = "600 aries" ] || die "$keys_file is $_m, not 600 aries" "read journalctl -u ariesd"
	ok "$keys_file, keys: $(grep -c '^restrict' "$keys_file")"
	journalctl -u ariesd -n 3 --no-pager -o cat 2>/dev/null | sed 's/^/   | /' || true
}

connect() {
	step 11 'Connect a repository'
	_fpr=$(ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | cut -d' ' -f2)
	_host=${host:-$(hostname -f 2>/dev/null || hostname)}
	if [ -e "$config" ]; then _project=$(setting project); else _project='<made by step 5>'; fi
	say "   Commit this as .aries.yaml in the project's repository:"
	say
	say "project: $_project"
	say "server:"
	say "  host: $_host"
	[ -z "$port" ] || [ "$port" = 22 ] || say "  port: $port"
	say "  host_key: $_fpr"
	say
	say "   Then, as $admin, in that repository: aries whoami"
	say "   Add someone: aries admin principal add NAME --key FILE --project $project"
	say "   Keep $admin's line in $authorized: it is the way in when nothing else is."
}

check_machine
install_dolt
run_dolt
install_ariesd
create_database
run_ariesd_unit
first_admin
confine
adopt
start_ariesd
connect

say
if [ "$dry" = 1 ]; then
	say "Dry run: $changes changes above would be made; nothing was changed."
elif [ "$changes" = 0 ]; then
	say "Nothing to change: the server is set up as docs/server.md says."
else
	say "Done: $changes changes made."
fi
