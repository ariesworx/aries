#!/bin/sh
# Install the Dolt release the tests need (version.Dolt, internal/version)
# for this machine, after checking the archive's sha256 against the one
# pinned in internal/version/dolt.sha256.
#
#   scripts/install-dolt.sh
#   export PATH="$(scripts/install-dolt.sh):$PATH"
#
# Run with -h for the options. Needs curl or wget, tar, awk, and sha256sum
# or shasum. Never uses sudo, and never replaces a dolt you installed.
set -eu

usage() {
	cat <<'EOF'
usage: install-dolt.sh [--dir DIR]

Installs the pinned Dolt into DIR/dolt-VERSION/bin and prints that
directory, alone, on standard output; everything else goes to standard
error. When the pinned Dolt is already there it downloads nothing.

  --dir DIR   the cache directory (default: $XDG_CACHE_HOME/aries, or
              ~/.cache/aries; env ARIES_DOLT_DIR)
  -h, --help  show this help

For testing and mirrors, ARIES_DOLT_BASE_URL replaces
https://github.com/dolthub/dolt/releases/download; it must serve
/vX.Y.Z/FILE. The checksum is still the pinned one.
EOF
}

say() { printf '%s\n' "$*" >&2; }
die() { # message fix
	printf 'install-dolt.sh: %s\n' "$1" >&2
	if [ -n "${2:-}" ]; then printf 'fix: %s\n' "$2" >&2; fi
	exit 1
}
have() { _have=$(command -v "$1" 2>&1) && [ -n "$_have" ]; }

dir=${ARIES_DOLT_DIR:-}
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

# The pins: one "SHA256  vVERSION/ARCHIVE" line for each platform.
sums=$(dirname "$0")/../internal/version/dolt.sha256
[ -r "$sums" ] || die "cannot read $sums" "run this script from a checkout of aries"
have awk || die "awk is not installed" "install awk"
version=$(awk '$2 ~ /^v[0-9][0-9.]*\/dolt-/ {sub(/\/.*/, "", $2); print substr($2, 2); exit}' "$sums")
[ -n "$version" ] || die "$sums names no release" "restore it: git checkout internal/version/dolt.sha256"

dest=$dir/dolt-$version
is_pinned() { [ "$("$1" version 2>/dev/null | head -n 1)" = "dolt version $version" ]; }
if [ -e "$dest" ]; then
	is_pinned "$dest/bin/dolt" ||
		die "$dest exists and its bin/dolt is not Dolt $version; nothing was changed" "remove $dest and run this again"
	say "Dolt $version is already in $dest/bin"
	printf '%s\n' "$dest/bin"
	exit 0
fi

os=$(uname -s)
case $os in
Linux) os=linux ;;
Darwin) os=darwin ;;
MINGW* | MSYS* | CYGWIN* | Windows*)
	die "this script does not install on Windows" \
		"run it in WSL, or install Dolt $version from https://github.com/dolthub/dolt/releases/tag/v$version" ;;
*) die "unsupported OS $os" "install Dolt $version from https://github.com/dolthub/dolt/releases/tag/v$version" ;;
esac
arch=$(uname -m)
case $arch in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) die "unsupported architecture $arch" "install Dolt $version from https://github.com/dolthub/dolt/releases/tag/v$version" ;;
esac

archive=dolt-$os-$arch.tar.gz
want=$(awk -v n="v$version/$archive" '$2 == n {print $1; exit}' "$sums")
[ -n "$want" ] || die "no checksum is pinned for v$version/$archive in $sums" \
	"install Dolt $version from https://github.com/dolthub/dolt/releases/tag/v$version"

if have curl; then
	fetch() { curl -fsSL --retry 2 -o "$2" "$1"; }
elif have wget; then
	fetch() { wget -q -O "$2" "$1"; }
else
	die "neither curl nor wget is installed" "install curl"
fi
have tar || die "tar is not installed" "install tar"
if have sha256sum; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif have shasum; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
else
	die "neither sha256sum nor shasum is installed" "install coreutils"
fi

# With a template: mktemp -d alone ignores TMPDIR on macOS.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aries-dolt.XXXXXX" 2>&1) || die "cannot make a temporary directory: $tmp" "set TMPDIR to a writable directory"
stage=
cleanup() { rm -rf "$tmp"; [ -z "$stage" ] || rm -rf "$stage"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

base=${ARIES_DOLT_BASE_URL:-https://github.com/dolthub/dolt/releases/download}
url=${base%/}/v$version/$archive
say "downloading $url"
fetch "$url" "$tmp/$archive" ||
	die "cannot download $url" "check the network, and that Dolt's release v$version has $archive"
got=$(sha256 "$tmp/$archive")
[ "$got" = "$want" ] || die "checksum mismatch for $archive (got $got, want $want); nothing was installed" \
	"retry; if it persists, do not use this download, and report it at https://github.com/ariesworx/aries/issues"

member=dolt-$os-$arch/bin/dolt
mkdir "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x" "$member" || die "cannot unpack $member from $archive" "retry the download"
[ -f "$tmp/x/$member" ] || die "$archive holds no $member" "report it at https://github.com/ariesworx/aries/issues"

# Build the directory beside its final name and rename it, so a run that
# is interrupted leaves nothing that looks installed.
mkdir -p "$dir" || die "cannot create $dir" "pass --dir with a directory you can write"
stage=$dir/.dolt-$version.$$
mkdir -p "$stage/bin" || die "cannot write to $dir" "pass --dir with a directory you can write"
cp "$tmp/x/$member" "$stage/bin/dolt" || die "cannot write to $dir" "pass --dir with a directory you can write"
chmod 0755 "$stage/bin/dolt"
is_pinned "$stage/bin/dolt" ||
	die "the dolt in $archive does not run here, or is not Dolt $version; nothing was installed" \
		"run $member from the archive by hand to see why"
if [ -e "$dest" ]; then
	# Another run, such as a gate in a second worktree, installed it meanwhile.
	is_pinned "$dest/bin/dolt" ||
		die "$dest appeared meanwhile and its bin/dolt is not Dolt $version; nothing was changed" "remove $dest and run this again"
else
	mv "$stage" "$dest" || die "cannot create $dest" "pass --dir with a directory you can write"
	stage=
fi

say "installed Dolt $version to $dest/bin"
say "the tests find dolt on PATH; scripts/gate.sh finds this one by itself. For go test, run:"
say "  export PATH=\"$dest/bin:\$PATH\""
printf '%s\n' "$dest/bin"
