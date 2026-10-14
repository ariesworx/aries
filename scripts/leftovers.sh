#!/bin/sh
# Run a command, usually the tests, and see that it leaves nothing behind:
# no file under the temporary directory, and no process still running.
#
#   scripts/leftovers.sh go test ./...
#
# The command is given a temporary directory of its own (TMPDIR), which is
# removed afterward whatever happened, so a run that was killed, or a test
# that panicked or ran out of time, leaves nothing there either. Run with
# -h for the rest. The gate's tests run under it, in scripts/gate.sh and
# in CI (CONTRIBUTING.md, "Tests that leave nothing behind").
set -eu

usage() {
	cat <<'EOF'
usage: leftovers.sh COMMAND [ARGUMENT...]

Runs COMMAND with TMPDIR set to a new directory, and then reports what
it left: the entries still in that directory, and the processes still
running that were started with it (a dolt sql-server, a test binary, an
ssh-agent). The directory is made under the TMPDIR it is given, and
removed in every case. When GOTMPDIR is set, under which go builds and
go test makes each t.TempDir, COMMAND is given a new one under it too,
which is checked and removed the same way.

The exit status is COMMAND's. When COMMAND succeeded and left something,
it is 1: a test that passes must also clean up after itself. When COMMAND
failed, what it left is listed and the status stays COMMAND's, since a
test binary that is killed, panics or times out cannot remove its
temporary directories.

Each entry left is named with what probably made it: the t.TempDir of
the test it is named for (a subtest's name loses its slash), or
os.MkdirTemp with its prefix.

  STARFIX_LEFTOVERS_WAIT  seconds to wait for processes to end before
                          they count as left (default 15: the watcher of
                          a killed test's Dolt takes up to 10 to end it)
  GITHUB_ACTIONS=true     also print the report as an error annotation
  -h, --help              show this help
EOF
}

case ${1:-} in
-h | --help) usage; exit 0 ;;
'') usage >&2; exit 2 ;;
esac

wait_secs=${STARFIX_LEFTOVERS_WAIT:-15}
case $wait_secs in '' | *[!0-9]*) printf 'leftovers.sh: STARFIX_LEFTOVERS_WAIT is %s, not a number of seconds\n' "$wait_secs" >&2; exit 2 ;; esac

dir=
godir=
trap '[ -z "$dir" ] || rm -rf -- "$dir"; [ -z "$godir" ] || rm -rf -- "$godir"' EXIT
trap 'exit 1' HUP INT TERM

# private BASE makes a directory of the command's own under BASE. A short
# name: the path of a unix socket is at most 104 bytes on macOS, and the
# tests make theirs under here.
private() {
	if ! d=$(mktemp -d "${1%/}/sfl.XXXXXX"); then
		printf 'leftovers.sh: cannot make a temporary directory under %s\nfix: set TMPDIR, and GOTMPDIR if it is set, to writable directories\n' "$1" >&2
		return 2
	fi
	# Others may pass through it, as through /tmp: the tests of the
	# socket's peer check start a process as another user from a
	# directory under it.
	chmod 755 "$d"
	printf '%s\n' "$d"
}
dir=$(private "${TMPDIR:-/tmp}") || exit 2
if [ -n "${GOTMPDIR:-}" ]; then
	godir=$(private "$GOTMPDIR") || exit 2
fi
# Their own names, which a process shows however the system spells the
# path to them (/var and /private/var on macOS, where the longer would
# not fit).
name=${dir##*/}
goname=${godir##*/}

# started lists the processes the command started and did not end, one
# "PID COMMAND" to a line: those with the directory in their environment,
# where /proc shows it (Linux), or either directory in their command line,
# which is all a macOS shows. The tests' own are found either way: a dolt
# sql-server is given its config file, its watcher its directory, and a
# test binary is built in GOTMPDIR. This script is not one of them.
started() {
	set -- -e "/$name"
	if [ -n "$goname" ]; then set -- "$@" -e "/$goname"; fi
	if [ -r /proc/self/environ ]; then
		# A process may end while it is read: the shell's complaint that
		# its files are gone is not this script's to print.
		for p in /proc/[0-9]*; do
			pid=${p#/proc/}
			[ "$pid" != "$$" ] || continue
			# shellcheck disable=SC2002 # cat, so that a file that has gone is cat's complaint to drop, not the shell's
			if cat "$p/environ" 2>/dev/null | tr '\0' '\n' | grep -qxF "TMPDIR=$dir" ||
				cat "$p/cmdline" 2>/dev/null | tr '\0' ' ' | grep -qF "$@"; then
				cmd=$(cat "$p/cmdline" 2>/dev/null | tr '\0' ' ' | cut -c1-200)
				[ -z "$cmd" ] || printf '%s %s\n' "$pid" "$cmd"
			fi
		done
	else
		# shellcheck disable=SC2009 # pgrep prints no command line to match
		ps -axww -o pid=,command= 2>/dev/null | grep -F "$@" | while read -r pid cmd; do
			[ "$pid" != "$$" ] || continue
			printf '%s %s\n' "$pid" "$(printf '%s' "$cmd" | cut -c1-200)"
		done
	fi | grep -v ' grep -F ' || true
}

rc=0
if [ -n "$godir" ]; then
	TMPDIR=$dir GOTMPDIR=$godir "$@" || rc=$?
else
	TMPDIR=$dir "$@" || rc=$?
fi

procs=$(started)
n=0
while [ -n "$procs" ] && [ "$n" -lt "$wait_secs" ]; do
	sleep 1
	n=$((n + 1))
	procs=$(started)
done
files=$(ls -A "$dir")
gofiles=
if [ -n "$godir" ]; then gofiles=$(ls -A "$godir"); fi

if [ -z "$procs" ] && [ -z "$files" ] && [ -z "$gofiles" ]; then
	exit "$rc"
fi
count() { printf '%s\n' "$1" | wc -l | tr -d ' '; }
plural() { if [ "$1" = 1 ]; then printf '%s' "$2"; else printf '%s' "$3"; fi; }
# whose names what probably made an entry: t.TempDir names a test's for
# the test, without a subtest's slash, and os.MkdirTemp names one for its
# prefix; both add random digits.
whose() {
	stem=$(printf '%s' "$1" | sed 's/[0-9]*$//')
	case $1 in
	Test*[0-9]) printf ': t.TempDir of %s' "$stem" ;;
	*[0-9]) printf ': os.MkdirTemp("", "%s")' "$stem" ;;
	esac
}
entries() { # DIR NAMES WHERE
	[ -n "$2" ] || return 0
	c=$(count "$2")
	printf 'leftovers.sh: the command left %s %s under its %s:\n' "$c" "$(plural "$c" entry entries)" "$3"
	printf '%s\n' "$2" | while read -r f; do
		printf '  %s (%s)%s\n' "$f" "$(du -sk "$1/$f" 2>/dev/null | awk '{print $1 " KB"}')" "$(whose "$f")"
	done
}
report() {
	entries "$dir" "$files" "temporary directory"
	entries "$godir" "$gofiles" GOTMPDIR
	if [ -n "$procs" ]; then
		c=$(count "$procs")
		printf 'leftovers.sh: the command left %s %s running:\n' "$c" "$(plural "$c" process processes)"
		printf '%s\n' "$procs" | while read -r pid cmd; do
			printf '  %s %s\n' "$pid" "$cmd"
			# The entry it runs from, which names its test.
			for nm in "$name" "$goname"; do
				[ -n "$nm" ] || continue
				e=$(printf '%s\n' "$cmd" | sed -n "s|.*/$nm/\([^/ ]*\).*|\1|p")
				if [ -n "$e" ]; then
					printf '      in %s%s\n' "$e" "$(whose "$e")"
					break
				fi
			done
		done
	fi
	if [ "$rc" != 0 ]; then
		printf 'leftovers.sh: the command failed (exit status %s), so this may be all it could not clean up; the directories are removed\n' "$rc"
	else
		printf 'fix: a test removes what it makes (t.TempDir, t.Cleanup) and stops what it starts; the name of an entry is its test'"'"'s (CONTRIBUTING.md, "Tests that leave nothing behind")\n'
	fi
}
msg=$(report)
printf '%s\n' "$msg" >&2
if [ "${GITHUB_ACTIONS:-}" = true ]; then
	# One annotation for the whole report: a workflow command is one
	# line, so its newlines are written %0A, and its percent signs %25.
	printf '%s\n' "$msg" | sed -e 's/^leftovers\.sh: //' -e 's/%/%25/g' |
		awk 'NR > 1 { printf "%%0A" } { printf "%s", $0 } END { print "" }' |
		sed 's/^/::error title=leftovers.sh::/' >&2
fi
[ "$rc" != 0 ] || rc=1
exit "$rc"
