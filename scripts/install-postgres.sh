#!/bin/sh
# Install the Postgres release the tests need (version.Postgres,
# internal/version) for this machine, and print the directory of its
# programs.
#
#   scripts/install-postgres.sh
#   export PATH="$(scripts/install-postgres.sh):$PATH"
#
# On Linux, when the PostgreSQL project's packages of the pinned major
# version are installed (apt.postgresql.org puts them in
# /usr/lib/postgresql/MAJOR/bin), it prints their directory and builds
# nothing. Otherwise, and always on macOS, it downloads the official
# source archive of the pinned release, checks its SHA-256 against the
# one pinned in internal/version/postgres.sha256, and builds it, without
# ICU or readline, into the cache directory.
#
# Run with -h for the options. Needs curl or wget, tar, gzip, awk,
# sha256sum or shasum, make and a C compiler. Never uses sudo, and never
# replaces a Postgres you installed.
set -eu

usage() {
	cat <<'EOF'
usage: install-postgres.sh [--dir DIR]

Prints the directory of the pinned Postgres's programs (postgres, initdb,
psql), alone, on standard output; everything else goes to standard error.
It uses the packaged Postgres of the pinned major version on Linux when
it is installed, or else builds the pinned release into
DIR/postgres-VERSION, once: when it is already there it downloads
nothing.

  --dir DIR   the cache directory (default: $XDG_CACHE_HOME/aries, or
              ~/.cache/aries; env ARIES_POSTGRES_DIR)
  -h, --help  show this help

For testing and mirrors, ARIES_POSTGRES_BASE_URL replaces
https://ftp.postgresql.org/pub/source; it must serve /vX.Y/FILE. The
checksum is still the pinned one. ARIES_POSTGRES_PACKAGED replaces
/usr/lib/postgresql, where the packages are looked for.
EOF
}

say() { printf '%s\n' "$*" >&2; }
die() { # message fix
	printf 'install-postgres.sh: %s\n' "$1" >&2
	if [ -n "${2:-}" ]; then printf 'fix: %s\n' "$2" >&2; fi
	exit 1
}
have() { _have=$(command -v "$1" 2>&1) && [ -n "$_have" ]; }

dir=${ARIES_POSTGRES_DIR:-}
while [ $# -gt 0 ]; do
	case $1 in
	--dir) [ $# -ge 2 ] || die "--dir needs a value" "--dir \$HOME/.cache/aries"; dir=$2; shift ;;
	--dir=*) dir=${1#--dir=} ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option $1" "run with -h for the options" ;;
	esac
	shift
done
[ -n "$dir" ] || dir=${XDG_CACHE_HOME:-${HOME:?HOME is not set}/.cache}/aries
dir=${dir%/}

# The pin: one "SHA256  vVERSION/ARCHIVE" line.
sums=$(dirname "$0")/../internal/version/postgres.sha256
[ -r "$sums" ] || die "cannot read $sums" "run this script from a checkout of aries"
have awk || die "awk is not installed" "install awk"
version=$(awk '$2 ~ /^v[0-9][0-9.]*\/postgresql-/ {sub(/\/.*/, "", $2); print substr($2, 2); exit}' "$sums")
[ -n "$version" ] || die "$sums names no release" "restore it: git checkout internal/version/postgres.sha256"
major=${version%%.*}
archive=postgresql-$version.tar.gz
want=$(awk -v n="v$version/$archive" '$2 == n {print $1; exit}' "$sums")
[ -n "$want" ] || die "no checksum is pinned for v$version/$archive in $sums" "restore it: git checkout internal/version/postgres.sha256"

# release prints the release of the postgres program in directory $1,
# such as 18.6, or nothing when there is none that runs. It prints
# "postgres (PostgreSQL) 18.6", which a distribution's build follows with
# words of its own.
release() {
	[ -x "$1/postgres" ] || return 0
	"$1/postgres" --version 2>/dev/null | awk 'NR == 1 {print $3}'
}

os=$(uname -s)
case $os in
Linux | Darwin) ;;
MINGW* | MSYS* | CYGWIN* | Windows*)
	die "this script does not install on Windows" "run it in WSL, or install Postgres $major from https://www.postgresql.org/download/" ;;
*) die "unsupported OS $os" "install Postgres $major from https://www.postgresql.org/download/ and put its bin directory on PATH" ;;
esac

if [ "$os" = Linux ]; then
	packaged=${ARIES_POSTGRES_PACKAGED:-/usr/lib/postgresql}/$major/bin
	got=$(release "$packaged")
	case $got in
	"$major".*)
		say "using the packaged Postgres $got in $packaged"
		printf '%s\n' "$packaged"
		exit 0
		;;
	esac
fi

dest=$dir/postgres-$version
if [ -e "$dest" ]; then
	[ "$(release "$dest/bin")" = "$version" ] ||
		die "$dest exists and its bin/postgres is not Postgres $version; nothing was changed" "remove $dest and run this again"
	say "Postgres $version is already in $dest/bin"
	printf '%s\n' "$dest/bin"
	exit 0
fi

if have curl; then
	fetch() { curl -fsSL --retry 2 -o "$2" "$1"; }
elif have wget; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "neither curl nor wget is installed" "install curl"
fi
have tar || die "tar is not installed" "install tar"
have make || die "make is not installed" "install make and a C compiler (on macOS: xcode-select --install)"
have cc || die "no C compiler (cc) is installed" "install one (on macOS: xcode-select --install)"
if have sha256sum; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif have shasum; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "neither sha256sum nor shasum is installed" "install coreutils"
fi
if have nproc; then
	jobs=$(nproc)
elif have sysctl; then
	jobs=$(sysctl -n hw.ncpu 2>/dev/null || echo 2)
else
	jobs=2
fi

# With a template: mktemp -d alone ignores TMPDIR on macOS.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aries-postgres.XXXXXX" 2>&1) || die "cannot make a temporary directory: $tmp" "set TMPDIR to a writable directory"
stage=
cleanup() { rm -rf "$tmp"; [ -z "$stage" ] || rm -rf "$stage"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

base=${ARIES_POSTGRES_BASE_URL:-https://ftp.postgresql.org/pub/source}
url=${base%/}/v$version/$archive
say "downloading $url"
fetch "$url" "$tmp/$archive" ||
	die "cannot download $url" "check the network, and that release $version has $archive"
got=$(sha256 "$tmp/$archive")
[ "$got" = "$want" ] || die "checksum mismatch for $archive (got $got, want $want); nothing was installed" \
	"retry; if it persists, do not use this download, and report it at https://github.com/ariesworx/aries/issues"

mkdir "$tmp/src"
tar -xzf "$tmp/$archive" -C "$tmp/src" || die "cannot unpack $archive" "retry the download"
src=$tmp/src/postgresql-$version
[ -f "$src/configure" ] || die "$archive holds no postgresql-$version/configure" "report it at https://github.com/ariesworx/aries/issues"

# Built for its final directory, which macOS writes into the programs'
# references to libpq, and installed into a stage beside it (DESTDIR)
# that is then renamed, so a run that is interrupted leaves nothing that
# looks installed.
mkdir -p "$dir" || die "cannot create $dir" "pass --dir with a directory you can write"
stage=$dir/.postgres-$version.$$
log=$tmp/build.log
say "building Postgres $version into $dest (a few minutes; the log is kept on failure)"
(
	cd "$src" &&
		./configure --prefix="$dest" --without-icu --without-readline >"$log" 2>&1 &&
		make -j "$jobs" >>"$log" 2>&1 &&
		make install DESTDIR="$stage" >>"$log" 2>&1
) || {
	kept=$dir/postgres-$version.build.log
	cp "$log" "$kept" 2>/dev/null || kept=$log
	die "building Postgres $version failed; nothing was installed (the log is $kept)" \
		"install what configure asked for in the log, then run this again"
}
built=$stage$dest
[ "$(release "$built/bin")" = "$version" ] ||
	die "the postgres built does not run here, or is not Postgres $version; nothing was installed" \
		"run $built/bin/postgres --version by hand to see why"
if [ -e "$dest" ]; then
	# Another run, such as a gate in a second worktree, installed it meanwhile.
	[ "$(release "$dest/bin")" = "$version" ] ||
		die "$dest appeared meanwhile and its bin/postgres is not Postgres $version; nothing was changed" "remove $dest and run this again"
else
	mv "$built" "$dest" || die "cannot create $dest" "pass --dir with a directory you can write"
fi

say "installed Postgres $version to $dest/bin"
say "the tests find postgres and initdb on PATH; scripts/gate.sh finds this one by itself. For go test, run:"
say "  export PATH=\"$dest/bin:\$PATH\""
printf '%s\n' "$dest/bin"
