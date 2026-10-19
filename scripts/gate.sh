#!/bin/sh
# Run the gate on this machine: the commands in AGENTS.md's Commands
# block, which are the ones CI runs (.github/workflows/ci.yml), in the
# same order. It stops at the first failure, and ends by listing what a
# run here did not cover.
#
#   scripts/gate.sh
#
# Run with -h for the options. Needs go, golangci-lint, shellcheck and
# git (CONTRIBUTING.md, "What you need"); it gets Postgres itself.
set -eu

usage() {
	cat <<'EOF'
usage: gate.sh [--quick] [--keep-going] [--smoke] [--pgtest-lock]

Runs the gate: every command in AGENTS.md's Commands block, in order,
with one line for each and a summary. The tests run against Postgres
at the pinned major version: when the postgres on PATH is another, or
there is none, scripts/install-postgres.sh supplies it, packaged or
built into the per-user cache.

  --keep-going  run every command, rather than stop at the first failure
  --smoke       then run deploy/docker/smoke.sh, which needs Docker and
                is not part of the gate or of CI
  --quick       NOT the gate; for the loop between edits. Runs gofmt,
                go vet, golangci-lint, shellcheck and the tests, but the
                tests without -race or -shuffle, so unchanged packages
                come from Go's test cache. Skips go mod verify, go mod
                tidy -diff, govulncheck and the builds for other systems
  --pgtest-lock hold the Aries lock pgtest/HOST (HOST is this machine's
                name; capacity 2) around each whole-suite test command,
                through aries lock run --wait, so that agents testing in
                parallel on one machine do not run out of System V shared
                memory. The same as ARIES_GATE_PGTEST_LOCK=1. Needs aries
                on PATH and a .aries.yaml; without it nothing changes
  -h, --help    show this help

The exit status is 1 when a command failed and 2 when the gate could not
start. One result is neither a pass nor a failure: govulncheck findings
that are all in the standard library, and all fixed by the Go of
go.mod's toolchain line, which CI installs, come from an older Go
installed here and not from the change. The summary then says which Go
to install, and the exit status is 0.
EOF
}

die() { # message fix
	printf 'gate.sh: %s\n' "$1" >&2
	if [ -n "${2:-}" ]; then printf 'fix: %s\n' "$2" >&2; fi
	exit 2
}
have() { _have=$(command -v "$1" 2>&1) && [ -n "$_have" ]; }

quick=0
keep_going=0
smoke=0
pgtest_lock=0
[ "${ARIES_GATE_PGTEST_LOCK:-}" != 1 ] || pgtest_lock=1
while [ $# -gt 0 ]; do
	case $1 in
	--quick) quick=1 ;;
	--keep-going | -k) keep_going=1 ;;
	--smoke) smoke=1 ;;
	--pgtest-lock) pgtest_lock=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; die "unknown option $1" "run with -h for the options" ;;
	esac
	shift
done

root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
cd "$root"

# As CI: the Go installed here and no other (no toolchain download), and
# go.mod and go.sum as they are, whatever GOFLAGS the environment sets
# (docs/design/supply-chain.md, §7.4).
GOTOOLCHAIN=local
GOFLAGS=-mod=readonly
export GOTOOLCHAIN GOFLAGS

for tool in go gofmt golangci-lint shellcheck git awk; do
	have "$tool" || die "$tool is not installed, or not on PATH" \
		"install it at the version in CONTRIBUTING.md (\"What you need\"); with mise, run: mise install"
done
if [ "$pgtest_lock" = 1 ]; then
	have aries || die "aries is not installed, or not on PATH, and --pgtest-lock needs it" \
		"install aries (docs/install.md), or run the gate without --pgtest-lock"
	pgtest_host=$(hostname 2>/dev/null) && [ -n "$pgtest_host" ] ||
		die "cannot read this machine's name for the lock pgtest/HOST" "run the gate without --pgtest-lock"
fi
goversion=$(go env GOVERSION) ||
	die "go does not run in $root" "install Go at the go line in go.mod; with mise, run: mise install"
goos=$(go env GOOS)
goarch=$(go env GOARCH)
# go.mod's go line, as MAJOR.MINOR, and its toolchain line, the Go CI
# installs and the release is built with.
gomod=$(sed -n 's/^go \([0-9]*\.[0-9]*\).*/\1/p' go.mod)
gotoolchain=$(sed -n 's/^toolchain \(go[0-9.]*\)$/\1/p' go.mod)

# With a template: mktemp -d alone ignores TMPDIR on macOS.
tmp=$(mktemp -d "${TMPDIR:-/tmp}/aries-gate.XXXXXX" 2>&1) || die "cannot make a temporary directory: $tmp" "set TMPDIR to a writable directory"
keep_logs=0
cleanup() { [ "$keep_logs" = 1 ] || rm -rf "$tmp"; }
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
touch "$tmp/notes" "$tmp/failed" "$tmp/notrun" "$tmp/skipped"

# note records one thing this run did not cover, for the summary.
note() { printf '%s\n' "$*" >>"$tmp/notes"; }

# The pinned linter, or a note that CI's may disagree with this one.
lint_pin=$(sed -n '/golangci-lint-action@/,/version:/s/^ *version: v//p' .github/workflows/ci.yml)
lint_here=$(golangci-lint version --short 2>/dev/null) || lint_here=unknown
if [ "$lint_here" != "$lint_pin" ]; then
	note "golangci-lint here is $lint_here and CI pins $lint_pin, so CI's lint may find what this one did not, or the reverse. Install $lint_pin (with mise: mise install)."
fi

if [ "$goversion" != "$gotoolchain" ]; then
	note "Go here is $goversion and go.mod's toolchain line names $gotoolchain, which CI installs and the release is built with. Install $gotoolchain (with mise: mise install)."
fi

# Postgres at the pinned major version: the one on PATH, or else the one
# scripts/install-postgres.sh gives, the packaged one on Linux or the
# pinned release it builds into the per-user cache.
pg_pin=$(awk '$2 ~ /^v[0-9][0-9.]*\/postgresql-/ {sub(/\/.*/, "", $2); print substr($2, 2); exit}' internal/version/postgres.sha256)
pg_release() { postgres --version 2>/dev/null | awk 'NR == 1 {print $3}'; }
pg_here=$(pg_release) || pg_here=
pg_from="on PATH"
if [ "${pg_here%%.*}" != "${pg_pin%%.*}" ]; then
	pg_bin=$(scripts/install-postgres.sh 2>"$tmp/postgres.log") || {
		cat "$tmp/postgres.log" >&2
		die "the tests need Postgres ${pg_pin%%.*}, and the postgres on PATH is ${pg_here:-missing}" \
			"see scripts/install-postgres.sh's message above, or put Postgres ${pg_pin%%.*} on PATH"
	}
	PATH=$pg_bin:$PATH
	export PATH
	pg_from="from $pg_bin; on PATH: ${pg_here:-none}"
	pg_here=$(pg_release) || pg_here=
fi

mode="gate"
[ "$quick" = 0 ] || mode="quick (not the gate)"
printf '%s: %s %s/%s, golangci-lint %s, Postgres %s (%s)\n' \
	"$mode" "$goversion" "$goos" "$goarch" "$lint_here" "$pg_here" "$pg_from"

# stale_go reports whether a failed govulncheck's findings are all in the
# standard library and all fixed in a patch release of go.mod's Go: a
# newer Go clears them. run then checks that CI's, the toolchain line's,
# is new enough. A finding in a dependency, or one with no fix, is a
# failure like any other.
stale_go() { # log
	awk -v fix="@go$gomod." '
		/Found in:/ { found++; if ($NF !~ /@go[0-9]/) other++ }
		/Fixed in:/ { if (index($NF, fix)) fixed++ }
		END { exit !(found > 0 && other == 0 && fixed == found) }' "$1"
}

total=0
passed=0
stopped=0
stale=0
began=$(date +%s)

# run KIND COMMAND runs one command and prints one line for it. KIND is
# plain; empty, for a command that passes only if it prints nothing; or
# vuln, for govulncheck.
run() {
	kind=$1
	cmd=$2
	total=$((total + 1))
	if [ "$stopped" = 1 ]; then
		printf '%s\n' "$cmd" >>"$tmp/notrun"
		return 0
	fi
	log=$tmp/$total.log
	printf '%s ... ' "$cmd"
	t0=$(date +%s)
	rc=0
	case $cmd in
	*"go test"*)
		if [ "$pgtest_lock" = 1 ]; then
			# The whole-suite tests, under the lock pgtest/HOST: at most
			# two such runs on a machine (docs/design/dispatcher.md, §5).
			# aries lock run is the background job, so that a signal
			# reaches it, and it releases the lock as it stops the tests.
			(exec aries lock run "pgtest/$pgtest_host" --capacity 2 --wait -- sh -c "$cmd") >"$log" 2>&1 &
			lockpid=$!
			trap 'kill -TERM "$lockpid" 2>/dev/null; wait "$lockpid" 2>/dev/null; exit 1' HUP INT TERM
			wait "$lockpid" || rc=$?
			trap 'exit 1' HUP INT TERM
		else
			(eval "$cmd") >"$log" 2>&1 || rc=$?
		fi
		;;
	*) (eval "$cmd") >"$log" 2>&1 || rc=$? ;;
	esac
	secs=$(($(date +%s) - t0))
	if [ "$rc" = 0 ] && [ "$kind" = empty ] && [ -s "$log" ]; then rc=1; fi
	if [ "$rc" = 0 ]; then
		passed=$((passed + 1))
		printf 'ok (%ss)\n' "$secs"
		return 0
	fi
	newer=
	if [ "$kind" = vuln ] && stale_go "$log"; then
		newer=$(sed -n 's/.*Fixed in: .*@go\([0-9.]*\)$/\1/p' "$log" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)
		# Only a Go older than CI's: CI installs the toolchain line, and
		# has every finding it does not fix.
		if [ "$(printf '%s\n%s\n' "$newer" "${gotoolchain#go}" | sort -t. -k1,1n -k2,2n -k3,3n | tail -n 1)" = "${gotoolchain#go}" ]; then
			printf 'needs Go %s (%ss)\n' "$newer" "$secs"
			note "govulncheck: every finding is in the standard library of $goversion and is fixed by Go $newer, so this is the Go installed here, not the change; nothing was found in the module's dependencies. CI installs go.mod's toolchain line, $gotoolchain. Install it and run this again (with mise: mise install)."
			stale=1
			return 0
		fi
	fi
	printf 'FAILED (%ss)\n' "$secs"
	printf '%s\n' "$cmd" >>"$tmp/failed"
	keep_logs=1
	tail -n 60 "$log" | sed 's/^/    /'
	[ "$(wc -l <"$log")" -le 60 ] || printf '    (the last 60 lines; all of it is in %s)\n' "$log"
	case $kind:$rc in
	empty:*) printf '    fix: these files are not formatted; run gofmt -w on them\n' ;;
	*:127) printf '    fix: a tool is missing; see CONTRIBUTING.md, "What you need"\n' ;;
	vuln:*) [ -z "$newer" ] || printf "    fix: every finding is in the standard library and fixed by Go %s: move go.mod's toolchain line to go%s, with mise.toml, internal/tools/pin/go.mod and the Docker image (internal/version holds them together)\n" "$newer" "$newer" ;;
	esac
	[ "$keep_going" = 1 ] || stopped=1
}

# The gate. Each "both" and "gate" line is one command of AGENTS.md's
# Commands block, in its order, and a test (scripts/scripts_test.go)
# fails when the two lists differ: change them together, and ci.yml with
# them. "both" also runs under --quick; "gate" does not. "also" runs a
# command that is not CI's.
both() { run "${2:-plain}" "$1"; }
gate() {
	if [ "$quick" = 1 ]; then
		printf '%s\n' "$1" >>"$tmp/skipped"
	else
		run "${2:-plain}" "$1"
	fi
}
also() { run plain "$1"; }

both 'gofmt -l .' empty
both 'go vet ./...'
both 'golangci-lint run ./...'
gate 'go mod verify && go -C internal/tools/pin mod verify'
gate 'go mod tidy -diff && go -C internal/tools/pin mod tidy -diff'
both 'git ls-files -co --exclude-standard -z "*.sh" | xargs -0 shellcheck'
gate 'go tool -modfile=internal/tools/pin/go.mod govulncheck ./...' vuln
gate 'GOOS=darwin GOARCH=amd64 go vet ./...'
gate 'GOOS=darwin GOARCH=amd64 go build ./cmd/...'
gate 'GOOS=windows GOARCH=amd64 go vet ./...'
gate 'GOOS=windows GOARCH=amd64 go build ./cmd/...'
if [ "$goos" != linux ] && [ "$quick" = 0 ]; then
	# CI runs on Linux, so there the commands above cover the code that
	# only Linux builds (ariesd serve's peer check). Here they do not.
	also 'GOOS=linux GOARCH=amd64 go vet ./...'
	also 'GOOS=linux GOARCH=amd64 go build ./cmd/...'
	also 'GOOS=linux GOARCH=amd64 golangci-lint run ./...'
fi
# Under scripts/leftovers.sh, which fails a run that leaves a file in its
# temporary directory or a process running.
gate 'ARIES_REQUIRE_POSTGRES=1 scripts/leftovers.sh go test -race -shuffle=on ./...'
# The end-to-end suite again, its users on HTTPS where the run above has
# them on SSH: the same tests on both transports (transport.md §15).
gate 'ARIES_REQUIRE_POSTGRES=1 ARIES_E2E_TRANSPORT=https scripts/leftovers.sh go test -race -shuffle=on ./internal/e2e/'
if [ "$quick" = 1 ]; then
	also 'ARIES_REQUIRE_POSTGRES=1 scripts/leftovers.sh go test ./...'
fi
if [ "$smoke" = 1 ]; then
	also 'deploy/docker/smoke.sh'
fi

# What this run did not cover, and why.
if [ "$quick" = 1 ]; then
	note "--quick skipped these commands of the gate, and ran the tests without -race and -shuffle: $(paste -s -d ';' "$tmp/skipped" | sed 's/;/; /g'). Run scripts/gate.sh before you open a pull request."
fi
if [ "$goos" != linux ]; then
	note "Tests ran on $goos/$goarch; CI runs them on linux/amd64. The code only Linux builds (internal/server/peer_linux.go) was vetted, built and linted for Linux here, but its tests, TestCheckPeer*, run only on Linux: CI runs them."
elif [ "$(id -u)" != 0 ]; then
	note "TestCheckPeerOtherUID and TestCheckPeerKeepsStatusFromOtherUsers (internal/server) were skipped: each starts a socket peer as another user, which needs root. CI builds the package's test binary as its own user and runs only that binary under sudo, so that go never runs as root, and under scripts/leftovers.sh, since only root could remove what it left: go test -race -c -o server.test ./internal/server && (cd internal/server && sudo env \"PATH=\$PATH\" ARIES_REQUIRE_POSTGRES=1 ../../scripts/leftovers.sh ../../server.test -test.run '^TestCheckPeer' -test.v); rm server.test"
fi
if [ "$smoke" = 0 ]; then
	note "The Docker smoke test (deploy/docker/smoke.sh, or --smoke) was not run; CI does not run it either. Run it after changing the server, the protocol, deploy/docker or the setup steps in docs/server.md."
fi
note "The soak test ran its short form. Run the long one before changing claims, the reaper, the event log, pushes, the write path, the control store or what happens when access ends (CONTRIBUTING.md, \"The soak test\")."

elapsed=$(($(date +%s) - began))
failed=$(wc -l <"$tmp/failed" | tr -d ' ')
notrun=$(wc -l <"$tmp/notrun" | tr -d ' ')
printf '\n'
if [ "$failed" != 0 ]; then
	printf '%s: FAILED in %ss: %s of %s commands passed\n' "$mode" "$elapsed" "$passed" "$total"
	sed 's/^/  failed:  /' "$tmp/failed"
	sed 's/^/  not run: /' "$tmp/notrun"
	[ "$notrun" = 0 ] || printf '  (--keep-going runs every command)\n'
	printf '  logs: %s\n' "$tmp"
elif [ "$stale" = 1 ]; then
	printf '%s: passed in %ss, except govulncheck, which needs a newer Go: %s of %s commands passed\n' "$mode" "$elapsed" "$passed" "$total"
else
	printf '%s: passed in %ss: %s of %s commands\n' "$mode" "$elapsed" "$passed" "$total"
fi
printf 'Not covered by this run:\n'
sed 's/^/  - /' "$tmp/notes"
[ "$failed" = 0 ]
