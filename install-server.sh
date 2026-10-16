#!/bin/sh
# Set up an Aries server on a fresh Ubuntu 24.04 LTS or 26.04 LTS
# machine: what the steps of docs/server.md do by hand, for a server of
# many projects, with Postgres on the machine or a Postgres the operator
# names, and sshd reading the keys ariesd writes from its control store.
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
#
# The database steps (2, 3 and 5) do not depend on how clients reach
# the server. Step 7 is the SSH transport's: sshd and the keys file. The
# deployment profiles of docs/design/transport.md, section 3, will
# choose the transports with --profile, beside --dsn, and leave the
# database steps as they are.
set -eu

usage() {
	cat <<'EOF'
usage: install-server.sh --admin NAME --admin-key FILE [--host HOST] [--port PORT]
                         [--project NAME] [--version vX.Y.Z] [--releases SOURCE]
                         [--dsn URL [--db-password-file FILE]]
                         [--apparmor] [--dry-run]

Sets up an Aries server on Ubuntu 24.04 LTS or 26.04 LTS, as root,
following docs/server.md: Postgres, its role and database for ariesd,
ariesd and aries, the config file, the systemd unit, sshd, and the first
admin. Prints the .aries.yaml a repository commits.

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
  --dsn URL          use the Postgres 16 or newer this postgres:// URL
                     names, on another machine or a managed service, in
                     place of installing one here; its role and database
                     are made beforehand, as docs/server.md says. No
                     password in it
  --db-password-file FILE
                     the password of the role --dsn names, alone on one
                     line, which is copied to a 0600 file ariesd reads
  --apparmor         also confine ariesd with the AppArmor profiles
  --dry-run          print every action, and change nothing
  -h, --help         show this help

Without --dsn, Postgres comes from the PostgreSQL project's apt
repository (apt.postgresql.org), whose key is checked against the
fingerprint pinned below, unless a Postgres 16 or newer already runs
here, which is used as it is. ariesd signs in to it over its unix
socket as the role of its own user, so there is no password at all.

install.sh and the AppArmor profiles are read from beside this script
when it is run from a checkout of aries, and otherwise downloaded from
https://raw.githubusercontent.com/ariesworx/aries/main, as the guide
downloads them.

No secret is ever an argument or printed: the only password is the one
--db-password-file holds, read from that file and never shown.
EOF
}

# The Postgres release Aries is tested with, and the oldest ariesd opens:
# version.PostgresMajor and version.PostgresMin, which a test keeps
# these equal to.
pg_major=18
pg_min=16
# The PostgreSQL project's apt repository (PGDG), and the fingerprint of
# the key its packages are signed with, which a test keeps equal to CI's.
pgdg_url=https://apt.postgresql.org/pub/repos/apt
pgdg_key_url=https://www.postgresql.org/media/keys/ACCC4CF8.asc
pgdg_key_fingerprint=B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8
pgdg_key=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
pgdg_list=/etc/apt/sources.list.d/pgdg.list

raw_url=https://raw.githubusercontent.com/ariesworx/aries/main
config=/etc/aries/ariesd.yaml
home=/var/lib/aries
authorized=$home/.ssh/authorized_keys
keys_file=$home/.ssh/aries_keys
control_schema=aries_control
pg_socket=/var/run/postgresql
db_files=/etc/aries/postgres
pgpass=$db_files/pgpass
# The connections ariesd may hold: db_conns_per_server, and one for the
# writers' locks. Postgres keeps superuser_reserved_connections for
# itself, and an operator's command or a dump needs a few more.
ariesd_conns=33
spare_conns=10

# The statements of docs/server.md, step 2, which a test keeps equal to
# the guide's block.
sql_role="CREATE ROLE aries LOGIN;"
sql_database="CREATE DATABASE aries OWNER aries TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LOCALE 'C';"
sql_revoke="REVOKE ALL ON DATABASE aries FROM PUBLIC;"

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
dsn_opt='' db_password_file=''
releases=${ARIES_RELEASES:-}
while [ $# -gt 0 ]; do
	case $1 in
	--admin | --admin-key | --host | --port | --project | --version | --releases | --dsn | --db-password-file)
		[ $# -ge 2 ] || usage_die "$1 needs a value" "run with -h for the options"
		_opt=$1 _val=$2
		shift ;;
	--admin=* | --admin-key=* | --host=* | --port=* | --project=* | --version=* | --releases=* | --dsn=* | --db-password-file=*)
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
	--dsn) dsn_opt=$_val ;;
	--db-password-file) db_password_file=$_val ;;
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
# A --dsn is never quoted back: it is checked for a password first, and
# what it holds otherwise is the operator's.
if [ -n "$dsn_opt" ]; then
	case $dsn_opt in postgres://* | postgresql://*) ;;
	*) usage_die "--dsn is not a postgres:// URL" "give ariesd's role and database as postgres://aries@db.example.com/aries?sslmode=verify-full (docs/server.md, Managed services)" ;;
	esac
	_authority=${dsn_opt#*://}
	_authority=${_authority%%[/?]*}
	case $_authority in *:*@*)
		usage_die "--dsn holds a password, which would be on this command line" "leave it out of the URL, and give it in a file with --db-password-file FILE" ;;
	esac
	case $dsn_opt in *[?\&]password=*)
		usage_die "--dsn holds a password, which would be on this command line" "leave it out of the URL, and give it in a file with --db-password-file FILE" ;;
	esac
	case $dsn_opt in *[[:space:]\'\"\\]*)
		usage_die "--dsn holds a space or a quote" "give the plain URL, with any space percent-encoded (%20)" ;;
	esac
fi
if [ -n "$db_password_file" ]; then
	[ -n "$dsn_opt" ] || usage_die "--db-password-file is for the Postgres --dsn names" \
		"give --dsn too; without it this installer runs Postgres here, where ariesd needs no password"
	case $dsn_opt in *[?\&]passfile=*)
		usage_die "--dsn names a passfile, and --db-password-file gives another" "give one: leave passfile= out of the URL, or leave out --db-password-file" ;;
	esac
	if [ ! -f "$db_password_file" ] || [ ! -r "$db_password_file" ]; then
		usage_die "cannot read --db-password-file $db_password_file" "name a file that holds the role's password alone on one line, readable by root alone"
	fi
	_lines=$(grep -c '' "$db_password_file") || _lines=0
	if [ "$_lines" -gt 1 ]; then
		usage_die "--db-password-file $db_password_file holds more than one line" "put the password alone on its one line"
	fi
	[ -n "$(sed -n 1p "$db_password_file")" ] ||
		usage_die "--db-password-file $db_password_file is empty" "put the role's password alone on its one line"
fi

# The machine: an Ubuntu LTS release docs/server.md was run on, which
# scripts/installserver_test.go holds to the guide's list.
ubuntu_versions='24.04 26.04'
releases_text='Ubuntu 24.04 LTS or 26.04 LTS'
os_id=$(sed -n 's/^ID=//p' /etc/os-release 2>/dev/null | tr -d '"')
os_version=$(sed -n 's/^VERSION_ID=//p' /etc/os-release 2>/dev/null | tr -d '"')
os_codename=$(sed -n 's/^VERSION_CODENAME=//p' /etc/os-release 2>/dev/null | tr -d '"')
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
# The local Postgres: its major version and port, once step 2 has found
# or installed it.
pg_ver='' pg_cluster='' pg_status=''
pg_port=5432

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
# file of the repository, where the text is long. It is never given a
# file that holds a secret, since it shows a difference.
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

# file: /etc/systemd/system/ariesd.service
ariesd_unit() {
	cat <<'EOF'
[Unit]
Description=Aries server
After=network.target postgresql.service time-sync.target
Wants=postgresql.service time-sync.target

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

# The apt source of step 2: PGDG's suite for this release of Ubuntu,
# trusting only the key that was checked.
pgdg_source() {
	printf 'deb [signed-by=%s] %s %s-pgdg main\n' "$pgdg_key" "$pgdg_url" "$os_codename"
}

# fetch SOURCE-RELATIVE-PATH FILE copies a file of the repository from
# beside this script, or downloads it.
fetch() {
	case $from in
	https://*) curl -fsSL --proto '=https' --retry 2 -o "$2" "$from/$1" ;;
	*) cp "$from/$1" "$2" ;;
	esac
}

# pg_sql runs SQL from stdin on the local Postgres as its superuser, the
# postgres user, by its unix socket: the way docs/server.md administers
# it. Its output is the query's, unaligned, one row a line.
pg_sql() {
	(cd / && sudo -u postgres psql -X -q -A -t -v ON_ERROR_STOP=1 -p "$pg_port" -d postgres)
}

# installed PACKAGE reports whether apt has PACKAGE installed.
installed() {
	[ "$(dpkg-query -W -f '${Status}' "$1" 2>/dev/null)" = "install ok installed" ]
}

# key_fingerprint FILE prints the fingerprint of the first key in FILE,
# read by gpg into a keyring of this run's alone.
key_fingerprint() {
	install -d -m 0700 "$tmp/gnupg"
	GNUPGHOME=$tmp/gnupg gpg --show-keys --with-colons "$1" 2>/dev/null | awk -F: '$1 == "fpr" { print $10; exit }'
}

# the_dsn prints ariesd's DSN: --dsn, with the password file the run
# writes; or the local Postgres's unix socket, where the role of
# ariesd's own user signs in with no password.
the_dsn() {
	if [ -n "$dsn_opt" ]; then
		if [ -z "$db_password_file" ]; then
			printf '%s\n' "$dsn_opt"
			return 0
		fi
		case $dsn_opt in
		*'?'*) printf '%s&passfile=%s\n' "$dsn_opt" "$pgpass" ;;
		*) printf '%s?passfile=%s\n' "$dsn_opt" "$pgpass" ;;
		esac
		return 0
	fi
	if [ "$pg_port" = 5432 ]; then
		printf 'postgres://aries@/aries?host=%s\n' "$pg_socket"
	else
		printf 'postgres://aries@/aries?host=%s&port=%s\n' "$pg_socket" "$pg_port"
	fi
}

#############################################################################

check_machine() {
	step 1 'Check the machine'
	ok "Ubuntu $os_version ($os_codename)"
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m)" "aries and Postgres's packages are released for amd64 and arm64" ;;
	esac
	ok "$arch"
	[ -d /run/systemd/system ] || die "systemd is not running this machine" \
		"run this on a machine booted with systemd; docs/server.md's units need it"
	ok systemd
	_missing=
	for _c in curl openssl sudo visudo install cmp diff stat useradd usermod getent ssh-keygen systemctl journalctl flock apt-get dpkg-query; do
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
	if [ -n "$dsn_opt" ]; then
		ok "the database: the Postgres --dsn names"
	fi
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

# find_cluster sets pg_ver, pg_cluster, pg_port and pg_status (online or
# down) from the one Postgres cluster this machine runs, if it runs one
# (pg_lsclusters, of Debian's and Ubuntu's packaging). It stops on more
# than one, or on one older than ariesd opens.
find_cluster() {
	_clusters=
	if have pg_lsclusters; then
		_clusters=$(pg_lsclusters -h 2>/dev/null) || _clusters=
	fi
	[ -n "$_clusters" ] || return 0
	if [ "$(printf '%s\n' "$_clusters" | grep -c .)" -gt 1 ]; then
		die "this machine runs more than one Postgres cluster (pg_lsclusters), and this installer does not choose" \
			"name the one for ariesd with --dsn 'postgres://aries@/aries?host=$pg_socket&port=PORT', after making its role and database (docs/server.md, step 2)"
	fi
	# shellcheck disable=SC2086 # the fields of pg_lsclusters's one line
	set -- $_clusters
	if [ "${1%%.*}" -lt "$pg_min" ]; then
		die "this machine runs Postgres $1 (cluster $2), older than $pg_min, the oldest ariesd opens" \
			"upgrade the cluster (pg_upgradecluster), or give another Postgres with --dsn"
	fi
	pg_ver=$1 pg_cluster=$2 pg_port=$3 pg_status=$4
}

# start_cluster starts the cluster find_cluster found, if it is down.
start_cluster() {
	if [ "$pg_status" = online ]; then
		ok "cluster $pg_ver/$pg_cluster runs, on port $pg_port"
	else
		act systemctl start "postgresql@$pg_ver-$pg_cluster"
	fi
}

install_postgres() {
	step 2 'Install Postgres'
	_pkg=postgresql-$pg_major
	if [ -n "$dsn_opt" ]; then
		note "the database is the Postgres --dsn names; this installs only Postgres's client programs, for the checks and the backups"
		_pkg=postgresql-client-$pg_major
	else
		find_cluster
		if [ -n "$pg_ver" ]; then
			ok "Postgres $pg_ver, cluster $pg_cluster, used as it is"
			start_cluster
			return 0
		fi
	fi
	if installed "$_pkg"; then
		ok "$_pkg"
	else
		apt_source
		act env DEBIAN_FRONTEND=noninteractive apt-get install -y "$_pkg"
	fi
	[ -z "$dsn_opt" ] || return 0
	# The package makes a cluster and starts it, unless postgresql-common
	# is told not to (create_main_cluster = false).
	if [ "$dry" = 1 ] && ! installed "$_pkg"; then
		note "the package makes the cluster $pg_major/main, on port $pg_port, and starts it"
		pg_ver=$pg_major
		return 0
	fi
	find_cluster
	if [ -z "$pg_ver" ]; then
		act pg_createcluster --start "$pg_major" main
		pg_ver=$pg_major pg_cluster=main
		return 0
	fi
	start_cluster
}

# apt_source has apt trust PGDG's repository, by the key whose
# fingerprint is pinned above, and reads its index.
apt_source() {
	if [ -f "$pgdg_key" ]; then
		_got=$(key_fingerprint "$pgdg_key")
		[ "$_got" = "$pgdg_key_fingerprint" ] ||
			die "$pgdg_key has the key ${_got:-of no fingerprint}, not $pgdg_key_fingerprint; nothing was changed" \
				"move it aside if you did not put it there, and run this again"
		ok "$pgdg_key, fingerprint $pgdg_key_fingerprint"
	else
		have gpg || die "gpg, which checks the key of PGDG's repository, is missing" "apt-get install -y gnupg, then run this again"
		if [ "$dry" = 1 ]; then
			changes=$((changes + 1))
			say "   would download $pgdg_key_url, check its fingerprint is $pgdg_key_fingerprint, and install it as $pgdg_key"
		else
			say "   download $pgdg_key_url"
			curl -fsSL --proto '=https' --retry 2 -o "$tmp/pgdg.asc" "$pgdg_key_url" || die "cannot download $pgdg_key_url" "check the network"
			_got=$(key_fingerprint "$tmp/pgdg.asc")
			[ "$_got" = "$pgdg_key_fingerprint" ] ||
				die "the key at $pgdg_key_url has fingerprint ${_got:-none}, not $pgdg_key_fingerprint; nothing was installed" \
					"do not trust this download; report it at https://github.com/ariesworx/aries/issues"
			say "   fingerprint $_got, as pinned"
			act install -D -m 0644 "$tmp/pgdg.asc" "$pgdg_key"
		fi
	fi
	put "$pgdg_list" 644 root root pgdg_source
	act apt-get update
}

create_database() {
	step 3 'Create the role and the database'
	if [ -n "$dsn_opt" ]; then
		note "on the Postgres --dsn names, an administrator runs, once (docs/server.md, Managed services):"
		say "     $sql_role"
		say "     GRANT aries TO CURRENT_USER;  -- where the administrator is not a superuser"
		say "     $sql_database"
		say "     $sql_revoke"
		note "step 9 checks them, when ariesd first opens its database"
		return 0
	fi
	if [ "$dry" = 1 ] && { ! getent passwd postgres >/dev/null || ! echo 'SELECT 1;' | pg_sql >/dev/null 2>&1; }; then
		changes=$((changes + 1))
		say "   would run, as postgres, once Postgres runs: sudo -u postgres psql, with on stdin:"
		say "     $sql_role"
		say "     $sql_database"
		say "     $sql_revoke"
		return 0
	fi
	_role=$(echo "SELECT rolsuper::int::text || rolcreaterole::int::text || rolcreatedb::int::text || rolreplication::int::text || rolbypassrls::int::text || rolcanlogin::int::text FROM pg_roles WHERE rolname = 'aries';" | pg_sql) ||
		die "cannot query Postgres as its superuser" "read journalctl -u postgresql@$pg_ver-$pg_cluster"
	_db=$(echo "SELECT pg_get_userbyid(datdba) || ' ' || datlocprovider::text || ' ' || datcollate || ' ' || datctype || ' ' || pg_encoding_to_char(encoding) FROM pg_database WHERE datname = 'aries';" | pg_sql) ||
		die "cannot query Postgres" "read journalctl -u postgresql@$pg_ver-$pg_cluster"
	: >"$tmp/sql"
	case $_role in
	'') printf '%s\n' "$sql_role" >>"$tmp/sql" ;;
	000001) ok "role aries: it may log in, and nothing more" ;;
	*) die "the role aries exists with rights ariesd refuses (superuser, CREATEROLE, CREATEDB, REPLICATION or BYPASSRLS), or may not log in; nothing was changed" \
		"as postgres: ALTER ROLE aries NOSUPERUSER NOCREATEROLE NOCREATEDB NOREPLICATION NOBYPASSRLS LOGIN; then run this again" ;;
	esac
	case $_db in
	'') printf '%s\n%s\n' "$sql_database" "$sql_revoke" >>"$tmp/sql" ;;
	'aries c C C UTF8')
		ok "database aries: owned by aries, collation C"
		_public=$(echo "SELECT EXISTS (SELECT 1 FROM pg_database d, aclexplode(coalesce(d.datacl, acldefault('d', d.datdba))) a WHERE d.datname = 'aries' AND a.grantee = 0);" | pg_sql) ||
			die "cannot query Postgres" "read journalctl -u postgresql@$pg_ver-$pg_cluster"
		if [ "$_public" = f ]; then
			ok "no right on it for every role (PUBLIC)"
		else
			printf '%s\n' "$sql_revoke" >>"$tmp/sql"
		fi
		;;
	*) die "the database aries exists, as '$_db' (owner, locale provider, collation, ctype, encoding), not owned by aries with collation C; nothing was changed" \
		"if nothing uses it, drop it as postgres (DROP DATABASE aries;) and run this again; otherwise follow docs/server.md by hand" ;;
	esac
	[ -s "$tmp/sql" ] || return 0
	sed 's/^/     /' "$tmp/sql"
	changes=$((changes + 1))
	if [ "$dry" = 1 ]; then
		say "   would run the statements above, as postgres: sudo -u postgres psql, on stdin"
		return 0
	fi
	say "   run the statements above, as postgres: sudo -u postgres psql, on stdin"
	pg_sql <"$tmp/sql" >/dev/null || die "Postgres refused the statements" "read the error above, and run this again"
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
# checks the release's signature against the key it holds, which no
# variable replaces, and the checksum. It is given only the variables it
# documents.
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

# the_config prints the config file, with PROJECT.
the_config() {
	printf 'dsn: %s\nproject: %s\nsystemd_unit: ariesd.service\nadmins: [%s]\ncontrol_schema: %s\n' \
		"$(the_dsn)" "$1" "$admin" "$control_schema"
	[ -z "$releases" ] || printf 'releases: %s\n' "$releases"
}

# the_pgpass prints the password file of --db-password-file, as Postgres
# reads one: for any host, port, database and role, with \ and :
# escaped.
the_pgpass() {
	printf '*:*:*:*:'
	sed -n '1{s/\\/\\\\/g;s/:/\\:/g;p;}' "$db_password_file"
}

# write_pgpass writes the password file ariesd's DSN names, 0600 and the
# aries user's, or leaves it if it is already that. It is never shown,
# nor any difference from what is there.
write_pgpass() {
	ensure_dir "$db_files" root aries 750
	(umask 077 && the_pgpass >"$tmp/pgpass") || die "cannot read --db-password-file $db_password_file" "read the error above"
	if [ -e "$pgpass" ]; then
		cmp -s "$tmp/pgpass" "$pgpass" || die "$pgpass exists and holds another password; nothing in it was changed, and nothing of it is shown" \
			"if the role's password changed, move the file aside and run this again"
		ok "$pgpass, with the password of --db-password-file"
		meta "$pgpass" 600 aries aries
		return 0
	fi
	changes=$((changes + 1))
	if [ "$dry" = 1 ]; then
		say "   would write $pgpass, mode 600, owner aries:aries, with the password of --db-password-file"
		return 0
	fi
	say "   write $pgpass, mode 600, owner aries:aries, with the password of --db-password-file"
	pending=$db_files/.pgpass.install-server
	install -o aries -g aries -m 0600 "$tmp/pgpass" "$pending" || die "cannot write $pgpass" "read the error above"
	mv -f "$pending" "$pgpass" || die "cannot write $pgpass" "read the error above"
	pending=
}

write_config() {
	step 5 'Write the config file'
	ensure_dir /etc/aries root aries 750
	[ -z "$db_password_file" ] || write_pgpass
	_dsn=$(the_dsn)
	if [ -e "$config" ]; then
		meta "$config" 600 aries aries
		[ "$(setting dsn)" = "$_dsn" ] || die "$config has a dsn this installer did not write for this database; nothing was changed" \
			"this server was set up another way, or with another --dsn: follow docs/server.md by hand, or give the --dsn it was set up with"
		[ "$(setting control_schema)" = "$control_schema" ] || die "$config names no control schema $control_schema; nothing was changed" \
			"this is a server of one project: follow docs/server.md, Serving many projects, Opt in, by hand"
		case " $(setting admins | tr -d '[]",') " in
		*" $admin "*) ;;
		*) die "$config does not name $admin under admins:; nothing was changed" \
			"run again with --admin set to an admin it names ($(setting admins)), or add $admin there and restart ariesd" ;;
		esac
		ok "$config, project $(setting project)"
	else
		_project=$(cat /proc/sys/kernel/random/uuid)
		changes=$((changes + 1))
		if [ "$dry" = 1 ]; then
			say "   would write $config, mode 600, owner aries:aries, with a generated project:"
			the_config '<generated>' | sed 's/^/     | /'
		else
			say "   write $config, mode 600, owner aries:aries"
			pending=/etc/aries/.ariesd.yaml.install-server
			if ! (umask 077 && the_config "$_project" >"$pending") ||
				! chown aries:aries "$pending" || ! mv -f "$pending" "$config"; then
				die "cannot write $config" "read the error above"
			fi
			pending=
			note "project $_project"
		fi
	fi
	check_connection
}

# check_connection signs in to the database as ariesd will, as its user
# and with its DSN, and checks what Postgres it is and how many
# connections it allows.
check_connection() {
	if [ "$dry" = 1 ] || ! getent passwd aries >/dev/null || ! have psql; then
		note "the next run checks that ariesd's user signs in to the database"
		return 0
	fi
	_got=$( (cd "$home" && sudo -u aries -H psql -X -q -A -t -F ' ' -v ON_ERROR_STOP=1 -d "$(the_dsn)" \
		-c "SELECT current_setting('server_version_num')::int / 10000, current_setting('max_connections'), current_setting('superuser_reserved_connections'), current_user, current_database()") 2>&1) ||
		die "ariesd's user cannot sign in to the database: $_got" \
			"for Postgres on this machine, rerun this so step 3 makes the role; for --dsn, have the role and database made (docs/server.md, Managed services) and check the URL and the password file"
	# shellcheck disable=SC2086 # the five fields of the query's one row
	set -- $_got
	[ "$1" -ge "$pg_min" ] || die "the database is Postgres $1, older than $pg_min, the oldest ariesd opens" \
		"upgrade it to Postgres $pg_min or later (Aries is tested with $pg_major)"
	ok "ariesd's user signs in to Postgres $1, as role $4, to database $5"
	if [ $(($2 - $3)) -lt $((ariesd_conns + spare_conns)) ]; then
		note "warning: Postgres allows $2 connections, $3 of them kept for superusers: ariesd may hold $ariesd_conns, and an operator's command or a dump needs more (docs/server.md, Sizing Postgres)"
	else
		ok "max_connections $2, room for ariesd's $ariesd_conns and more"
	fi
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

# reload_units has systemd read the unit files again when this run wrote
# one, or when systemd says it has not read one since it changed: a run
# interrupted between the two leaves that.
reload_units() {
	if [ "$(systemctl show -p NeedDaemonReload --value ariesd.service 2>/dev/null)" = yes ]; then
		daemon_reload=1
	fi
	if [ "$daemon_reload" = 1 ]; then
		act systemctl daemon-reload
		daemon_reload=0
	fi
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
	# control init makes the control schema, adds what is new and leaves
	# the rest, so it is run every time; it says what it found.
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
	say "   Back up the database: docs/server.md, Backups and restore."
}

check_machine
install_postgres
create_database
install_ariesd
write_config
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
