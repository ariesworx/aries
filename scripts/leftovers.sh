#!/bin/sh
# Run a command, usually the tests, and see that it leaves nothing behind:
# no file under the temporary directory, and no process still running.
#
#   scripts/leftovers.sh go test ./...
#
# The command is given a temporary directory of its own (TMPDIR), which is
# removed afterward whatever happened, so a run that was killed, or a test
# that panicked or ran out of time, leaves nothing there either. Run with
# -h for the rest. It is not part of the gate (CONTRIBUTING.md, "Tests
# that leave nothing behind").
set -eu

usage() {
	cat <<'EOF'
usage: leftovers.sh COMMAND [ARGUMENT...]

Runs COMMAND with TMPDIR set to a new directory, and then reports what
it left: the entries still in that directory, and the processes still
running that were started with it (a dolt sql-server, a test binary, an
ssh-agent). The directory is removed in every case.

The exit status is COMMAND's. When COMMAND succeeded and left something,
it is 1: a test that passes must also clean up after itself. When COMMAND
failed, what it left is listed and the status stays COMMAND's, since a
test binary that is killed, panics or times out cannot remove its
temporary directories.

  STARFIX_LEFTOVERS_WAIT  seconds to wait for processes to end before
                          they count as left (default 15: the watcher of
                          a killed test's Dolt takes up to 10 to end it)
  -h, --help              show this help
EOF
}

case ${1:-} in
-h | --help) usage; exit 0 ;;
'') usage >&2; exit 2 ;;
esac

wait_secs=${STARFIX_LEFTOVERS_WAIT:-15}
case $wait_secs in '' | *[!0-9]*) printf 'leftovers.sh: STARFIX_LEFTOVERS_WAIT is %s, not a number of seconds\n' "$wait_secs" >&2; exit 2 ;; esac

# A short name: the path of a unix socket is at most 104 bytes on macOS,
# and the tests make theirs under here.
base=${TMPDIR:-/tmp}
dir=$(mktemp -d "${base%/}/sfl.XXXXXX") || {
	printf 'leftovers.sh: cannot make a temporary directory\nfix: set TMPDIR to a writable directory\n' >&2
	exit 2
}
# Others may pass through it, as through /tmp: the tests of the socket's
# peer check start a process as another user from a directory under it.
chmod 755 "$dir"
# Its own name, which a process shows however the system spells the path
# to it (/var and /private/var on macOS, where the longer would not fit).
name=${dir##*/}
trap 'rm -rf -- "$dir"' EXIT
trap 'exit 1' HUP INT TERM

# started lists the processes the command started and did not end, one
# "PID COMMAND" to a line: those with the directory in their environment,
# where /proc shows it (Linux), or in their command line, which is all a
# macOS shows. The tests' own are found either way: a dolt sql-server is
# given its config file, its watcher its directory, and a test binary is
# built there. This script is not one of them.
started() {
	if [ -r /proc/self/environ ]; then
		# A process may end while it is read: the shell's complaint that
		# its files are gone is not this script's to print.
		for p in /proc/[0-9]*; do
			pid=${p#/proc/}
			[ "$pid" != "$$" ] || continue
			if cat "$p/environ" 2>/dev/null | tr '\0' '\n' | grep -qxF "TMPDIR=$dir" ||
				cat "$p/cmdline" 2>/dev/null | tr '\0' ' ' | grep -qF "/$name"; then
				cmd=$(cat "$p/cmdline" 2>/dev/null | tr '\0' ' ' | cut -c1-200)
				[ -z "$cmd" ] || printf '%s %s\n' "$pid" "$cmd"
			fi
		done
	else
		# shellcheck disable=SC2009 # pgrep prints no command line to match
		ps -axww -o pid=,command= 2>/dev/null | grep -F "/$name" | while read -r pid cmd; do
			[ "$pid" != "$$" ] || continue
			printf '%s %s\n' "$pid" "$(printf '%s' "$cmd" | cut -c1-200)"
		done
	fi | grep -v ' grep -F ' || true
}

rc=0
TMPDIR=$dir "$@" || rc=$?

procs=$(started)
n=0
while [ -n "$procs" ] && [ "$n" -lt "$wait_secs" ]; do
	sleep 1
	n=$((n + 1))
	procs=$(started)
done
files=$(ls -A "$dir")

if [ -z "$procs" ] && [ -z "$files" ]; then
	exit "$rc"
fi
count() { printf '%s\n' "$1" | wc -l | tr -d ' '; }
plural() { if [ "$1" = 1 ]; then printf '%s' "$2"; else printf '%s' "$3"; fi; }
{
	if [ -n "$files" ]; then
		c=$(count "$files")
		printf 'leftovers.sh: the command left %s %s under its temporary directory:\n' "$c" "$(plural "$c" entry entries)"
		printf '%s\n' "$files" | while read -r f; do
			printf '  %s (%s)\n' "$f" "$(du -sk "$dir/$f" 2>/dev/null | awk '{print $1 " KB"}')"
		done
	fi
	if [ -n "$procs" ]; then
		c=$(count "$procs")
		printf 'leftovers.sh: the command left %s %s running:\n' "$c" "$(plural "$c" process processes)"
		printf '%s\n' "$procs" | sed 's/^/  /'
	fi
	if [ "$rc" != 0 ]; then
		printf 'leftovers.sh: the command failed (exit status %s), so this may be all it could not clean up; the directory is removed\n' "$rc"
	else
		printf 'fix: a test removes what it makes (t.TempDir, t.Cleanup) and stops what it starts; the name of an entry is its test'"'"'s (CONTRIBUTING.md, "Tests that leave nothing behind")\n'
	fi
} >&2
[ "$rc" != 0 ] || rc=1
exit "$rc"
