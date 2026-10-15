#!/bin/sh
# Installs the Postgres the spike pins (pgtest.Version) under the user's
# cache directory, as scripts/install-dolt.sh installs Dolt: nothing
# system-wide. The binaries are the zonky.io embedded-postgres builds from
# Maven Central, a jar holding a .txz; both the jar's and the archive's
# SHA-256 are checked against the values below before anything is
# unpacked. Only macOS on Apple silicon was pinned for the spike.
#
# usage: spike/postgres/install.sh [DIR]   (default ~/.cache/aries)
set -eu

version=18.6
artifact=embedded-postgres-binaries-darwin-arm64v8
jar_sha256=8f6ed6f38195b50a40712ada8b2a12060b6193c1a6977a6eb5cff053bb5a1b89
txz_sha256=f4db5df0b13e3e72149c0547ccd1eee177cfe67accb8c296e8d0e46ef3601f01
txz=postgres-darwin-arm_64.txz

case "$(uname -s)/$(uname -m)" in
Darwin/arm64) ;;
*)
	echo "install.sh: only macOS on Apple silicon is pinned; add the platform's artifact and checksums" >&2
	exit 1
	;;
esac

dir=${1:-${XDG_CACHE_HOME:-${HOME:?HOME is not set}/.cache}/aries}
dest=$dir/postgres-$version
if [ -x "$dest/bin/postgres" ]; then
	echo "$dest/bin"
	exit 0
fi
tmp=$(mktemp -d "$dir/dl-postgres.XXXXXX" 2>/dev/null || { mkdir -p "$dir" && mktemp -d "$dir/dl-postgres.XXXXXX"; })
trap 'rm -rf "$tmp"' EXIT

url=https://repo1.maven.org/maven2/io/zonky/test/postgres/$artifact/$version.0/$artifact-$version.0.jar
curl -sSfL --max-time 600 -o "$tmp/pg.jar" "$url"
echo "$jar_sha256  $tmp/pg.jar" | shasum -a 256 -c - >&2
unzip -q "$tmp/pg.jar" "$txz" -d "$tmp"
echo "$txz_sha256  $tmp/$txz" | shasum -a 256 -c - >&2
mkdir -p "$tmp/out"
tar -xJf "$tmp/$txz" -C "$tmp/out"
"$tmp/out/bin/postgres" --version >&2
mv "$tmp/out" "$dest"
echo "$dest/bin"
