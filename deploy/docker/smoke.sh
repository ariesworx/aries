#!/usr/bin/env bash
# Smoke test of the Docker test server (docs/deploy/docker.md). It builds
# the image and aries from this checkout, starts a throwaway server with two
# principals, drives aries through the main surfaces end to end, then
# through a server of many projects (a second project, and a person
# whose key only the control database holds, which sshd reads from the
# keys file ariesd writes), runs the commands of docs/ci.md's workflows
# as a CI job would, holds a lock around a command, restarts the server to check that its state
# persists, gives a key an end and sees it end, then removes the server
# and its volume. It exits non-zero on the first failure. The server runs
# fourteen hours east of UTC (ARIES_TZ), where sshd would end a key early
# if its expiry-time lacked the Z.
#
#   deploy/docker/smoke.sh [DOCKER BUILD FLAGS...]
#
# Flags go to `docker build`, for example --secret id=ca,src=FILE behind a
# TLS-intercepting proxy. ARIES names an aries binary to use instead of
# building one. The server serves HTTPS alone, as a fresh server does,
# on 127.0.0.1:ARIES_HTTPS_PORT, 8444 by default, and aries reaches it
# there. ARIES_SMOKE_SSH=1 runs all of it with SSH on beside HTTPS, and
# aries over SSH (ARIES_TRANSPORT=ssh), on 127.0.0.1:ARIES_PORT, 2223 by
# default; the checks of the keys file sshd reads run only then. The
# ports default so that a dev server on 8443 and 2222 is left alone. ARIES_IMAGE names an image to test
# instead of building aries-dev:local: a variant of it, built FROM it,
# such as those docs/deploy/docker.md describes. ARIES_SMOKE_LOG names
# a file to save the server's log to (sshd's and ariesd's, then
# Postgres's) before the server is removed. ARIES_SMOKE_PROJECT names the Compose
# project, aries-smoke by default, so that two runs on one machine,
# each with its own ARIES_PORT, do not remove each other's server.
#
# ARIES_SMOKE_TELEMETRY=1 runs all of it with ariesd's telemetry on
# and the compose file's `telemetry` and `prometheus` profiles, and then
# checks the harness of deploy/observability (telemetry_checks, below):
# what reaches the collector, the alert rules in a real Prometheus,
# /readyz, with Postgres frozen too, and that a collector that is down
# or hangs changes nothing.
# It pulls two more images.
#
# ARIES_SMOKE_IDENTITY=1 also runs the compose file's `identity`
# profile, Keycloak, and then checks it (identity_checks, below): its
# discovery document and keys, from this machine and from the server's
# container, the realm and its clients, a sign-in with PKCE and a nonce
# as a browser would make it, the device grant, and a token for the
# service principal. It prints claims, never a password, a secret or a
# token. Then the server signs people in at it (ARIES_IDENTITY=1), with
# its keys from a file, and bob, invited there, signs in end to end by
# `aries login --url`, works signed in, and signs out; and alice, whom
# nobody invited, is refused (signin_checks). Keycloak is on
# 127.0.0.1:ARIES_IDENTITY_PORT, 8181 by default, so a dev Keycloak on
# 8180 is left alone. It needs curl and openssl, and pulls one more
# image.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
port=${ARIES_PORT:-2223}
https_port=${ARIES_HTTPS_PORT:-8444}
ssh=
case ${ARIES_SMOKE_SSH:-} in
'') ;;
1) ssh=1 ;;
*)
  printf 'smoke: ARIES_SMOKE_SSH is %s\nfix: set it to 1, or leave it unset\n' "$ARIES_SMOKE_SSH" >&2
  exit 2
  ;;
esac
work=$(mktemp -d)
compose=(docker compose -p "${ARIES_SMOKE_PROJECT:-aries-smoke}" -f "$root/deploy/docker/compose.yaml")
telemetry=
sidecars=()
case ${ARIES_SMOKE_TELEMETRY:-} in
'') ;;
1)
  telemetry=1
  sidecars=(collector prometheus)
  compose+=(--profile telemetry --profile prometheus)
  ;;
*)
  printf 'smoke: ARIES_SMOKE_TELEMETRY is %s\nfix: set it to 1, or leave it unset\n' "$ARIES_SMOKE_TELEMETRY" >&2
  exit 2
  ;;
esac
identity=
case ${ARIES_SMOKE_IDENTITY:-} in
'') ;;
1)
  identity=1
  compose+=(--profile identity)
  export ARIES_IDENTITY_PORT=${ARIES_IDENTITY_PORT:-8181}
  ;;
*)
  printf 'smoke: ARIES_SMOKE_IDENTITY is %s\nfix: set it to 1, or leave it unset\n' "$ARIES_SMOKE_IDENTITY" >&2
  exit 2
  ;;
esac
passed=0
restart_took=0
last=

# The test's own ssh-agent (start_agent), never the caller's: its socket,
# on a short path, since a socket's is limited to about 100 bytes, and
# its pid.
agent_sock=/tmp/aries-smoke-$$.sock
agent_pid=
# The zone the server runs in, and the pid of a watch held open on a key
# with an end.
tz='ARIES-14'
key_watch=

cleanup() {
  if [[ -n ${ARIES_SMOKE_LOG:-} ]]; then
    "${compose[@]}" logs --no-log-prefix aries postgres >"$ARIES_SMOKE_LOG" 2>&1 || true
  fi
  "${compose[@]}" down -v >/dev/null 2>&1 || true
  if [[ -n $agent_pid ]]; then
    kill "$agent_pid" 2>/dev/null || true
  fi
  if [[ -n $key_watch ]]; then
    kill "$key_watch" 2>/dev/null || true
  fi
  rm -f "$agent_sock"
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  printf 'smoke: FAIL: %s\n' "$1" >&2
  if [[ -n ${2:-} ]]; then
    printf '%s\n' "$2" | sed 's/^/  | /' >&2
  fi
  exit 1
}

# aries_as WHO ARGS... runs aries in WHO's scratch repository, with its own
# session. WHO's key is named with ARIES_KEY_FILE, so it is the only
# key aries offers (design, decision 35). Nothing of the caller's can sign
# in: the home is a directory of the test's own, with no .ssh, and the
# caller's ssh-agent socket is never passed on. Four variables change
# that for the checks of the rule itself: smoke_key names another key
# file, or none when it is empty; smoke_agent is an ssh-agent socket of
# the test's own; smoke_home is another home directory; and smoke_old is
# a value for ARIES_KEY, the variable aries never reads. smoke_data is a
# data directory of the user's own (ARIES_DATA_DIR), for a sign-in, which
# is kept there; unset, it is the shared home's. smoke_session
# is another session, or none when it is empty, as in a CI job, where
# aries takes the session ci from CI=true. The session variables of an
# agent harness the test may run under are cleared, so they name none.
aries_as() {
  local who=$1
  shift
  local key=${smoke_key-$work/keys/${who%-web}}
  (cd "$work/$who" && HOME="${smoke_home-$work/home}" XDG_CACHE_HOME="$work/home/.cache" SSH_AUTH_SOCK="${smoke_agent-}" \
    ARIES_KEY_FILE="$key" ARIES_KEY="${smoke_old-}" ARIES_SESSION="${smoke_session-smoke-$who}" ARIES_TRANSPORT="${ssh:+ssh}" \
    ARIES_DATA_DIR="${smoke_data-}" \
    CLAUDE_CODE_SESSION_ID='' CLAUDE_SESSION_ID='' "$aries" "$@")
}

# yaml_for PROJECT prints the .aries.yaml of a checkout of PROJECT, as
# the server printed it: its URL and its test CA, and with SSH on its
# address and host key.
yaml_for() {
  printf 'project: %s\nserver:\n  url: %s\n  ca: |\n' "$1" "$url"
  sed 's/^/    /' "$work/ca.pem"
  if [[ -n $ssh ]]; then
    printf '  host: localhost\n  port: %s\n  host_key: %s\n' "$port" "$host_key"
  fi
}

# start_agent runs an ssh-agent of the test's own on agent_sock, and
# agent_add KEYFILE... gives it keys. Neither touches the caller's agent:
# every ssh-add here names the test's socket, after checking that it
# answers.
start_agent() {
  local out
  out=$(ssh-agent -a "$agent_sock") || fail "could not start an ssh-agent on $agent_sock" "$out"
  agent_pid=$(sed -n 's/^SSH_AGENT_PID=\([0-9]*\);.*/\1/p' <<<"$out")
  [[ -n $agent_pid && -S $agent_sock ]] || fail "ssh-agent printed no pid, or made no socket at $agent_sock" "$out"
}

agent_add() {
  local status=0
  SSH_AUTH_SOCK=$agent_sock ssh-add -l >/dev/null 2>&1 || status=$?
  # 0: it holds keys; 1: it holds none; anything else: it did not answer.
  ((status <= 1)) || fail "the test's ssh-agent on $agent_sock does not answer (ssh-add -l exited $status)"
  SSH_AUTH_SOCK=$agent_sock ssh-add -q "$@" 2>/dev/null || fail "could not add a key to the test's ssh-agent"
}

# expect NAME PATTERN WHO ARGS... runs aries as WHO, and fails unless it
# exits 0 and prints a line matching the extended regexp PATTERN.
expect() {
  local name=$1 pattern=$2
  shift 2
  last=$(aries_as "$@" 2>&1) || fail "$name: aries ${*:2} exited $?" "$last"
  grep -Eq -- "$pattern" <<<"$last" || fail "$name: aries ${*:2} printed nothing matching /$pattern/" "$last"
  passed=$((passed + 1))
}

# refuse NAME PATTERN WHO ARGS... is expect for a command that must fail.
refuse() {
  local name=$1 pattern=$2
  shift 2
  if last=$(aries_as "$@" 2>&1); then
    fail "$name: aries ${*:2} succeeded, want a refusal" "$last"
  fi
  grep -Eq -- "$pattern" <<<"$last" || fail "$name: aries ${*:2} printed nothing matching /$pattern/" "$last"
  passed=$((passed + 1))
}

# absent NAME PATTERN fails if the last output has a line matching PATTERN.
absent() {
  if grep -Eq -- "$2" <<<"$last"; then
    fail "$1: output matches /$2/" "$last"
  fi
  passed=$((passed + 1))
}

# server_setting KEY prints KEY's value from the newest .aries.yaml the
# server printed.
server_setting() {
  "${compose[@]}" logs --no-log-prefix aries 2>/dev/null | sed -n "s/^ *$1: //p" | tail -n 1
}

# in_server CMD... runs CMD in the server's container, as root.
in_server() { "${compose[@]}" exec -T aries "$@"; }

# keys_file checks the file ariesd writes for sshd: there, a regular
# file of mode 0600 that aries owns, starting with the comment that
# says who writes it. With SSH off there is no such file, which
# no_ssh_path checks, and these checks of it and the next three do
# nothing.
keys_file=/var/lib/aries/.ssh/aries_keys
check_keys_file() {
  [[ -n $ssh ]] || return 0
  local got
  got=$(in_server stat -L -c '%F %a %U:%G' "$keys_file" 2>&1) || fail "$1: no keys file at $keys_file" "$got"
  [[ $got == "regular file 600 aries:aries" ]] || fail "$1: the keys file is '$got', want a regular file, 600, aries:aries"
  got=$(in_server head -n 1 "$keys_file")
  [[ $got == "# Generated by ariesd"* ]] || fail "$1: the keys file does not start with ariesd's comment" "$got"
  passed=$((passed + 1))
}

# in_keys_file NAME PATTERN / not_in_keys_file NAME PATTERN check the
# keys file for a line matching the extended regexp PATTERN.
in_keys_file() {
  [[ -n $ssh ]] || return 0
  in_server grep -Eq -- "$2" "$keys_file" || fail "$1: the keys file has no line matching /$2/" "$(in_server cut -c1-120 "$keys_file")"
  passed=$((passed + 1))
}

not_in_keys_file() {
  [[ -n $ssh ]] || return 0
  if in_server grep -Eq -- "$2" "$keys_file"; then
    fail "$1: the keys file has a line matching /$2/" "$(in_server cut -c1-120 "$keys_file")"
  fi
  passed=$((passed + 1))
}

# no_failed_write fails if ariesd logged that it could not write the
# keys file, or never logged writing it.
no_failed_write() {
  [[ -n $ssh ]] || return 0
  local log
  log=$("${compose[@]}" logs --no-log-prefix aries 2>&1)
  if grep -q 'keys file is out of date' <<<"$log"; then
    fail "$1: ariesd logged a failed write of the keys file" "$(grep 'keys file' <<<"$log")"
  fi
  grep -q 'msg="keys file written"' <<<"$log" || fail "$1: ariesd never logged writing the keys file" "$(tail -n 20 <<<"$log")"
  passed=$((passed + 1))
}

# no_ssh_path checks, with SSH off, that the server exposes no SSH path
# (docs/server.md, SSH on and off): sshd does not run, ariesd wrote no
# keys file, and the bridge sshd would run is refused by ariesd.
no_ssh_path() {
  [[ -z $ssh ]] || return 0
  if in_server test -e /run/sshd.pid; then
    fail "$1: sshd runs with SSH off"
  fi
  if in_server test -e "$keys_file"; then
    fail "$1: ariesd wrote $keys_file with SSH off"
  fi
  local got
  got=$("${compose[@]}" exec -T -u aries -w /var/lib/aries aries ariesd stdio --principal alice </dev/null 2>&1) || true
  if ! grep -q '"c":"unavailable"' <<<"$got" || ! grep -q 'SSH is off on this server' <<<"$got"; then
    fail "$1: the bridge with SSH off was not refused unavailable, SSH is off" "$got"
  fi
  passed=$((passed + 3))
}

up() {
  ARIES_IMAGE=$image ARIES_KEYS=$keys ARIES_ADMINS=alice ARIES_PORT=$port ARIES_HTTPS_PORT=$https_port ARIES_SSH=$ssh ARIES_TELEMETRY=$telemetry ARIES_TZ=$tz \
    ARIES_IDENTITY=$identity "${compose[@]}" up -d --no-build --wait --wait-timeout 300 >/dev/null 2>&1 ||
    fail "the server did not become healthy" "$("${compose[@]}" logs --no-log-prefix 2>&1 | tail -n 40)"
}

# restart_server restarts the server's container and waits for it. The
# collector and Prometheus live in its network namespace, which a
# restart replaces, so they are restarted after it: at once, so that
# the collector listens again before ariesd, which starts after
# `ariesd control init`, has anything to send. Postgres's container is
# left running, as a machine's Postgres outlives a restart of ariesd.
restart_server() {
  local t0=$SECONDS
  "${compose[@]}" restart aries >/dev/null 2>&1
  restart_took=$((SECONDS - t0))
  if ((${#sidecars[@]})); then
    "${compose[@]}" restart "${sidecars[@]}" >/dev/null 2>&1 || fail "could not restart ${sidecars[*]} after the server"
  fi
  up
}

# What follows, to telemetry_checks, runs only with
# ARIES_SMOKE_TELEMETRY=1.

# http_get PORT PATH prints what the server answers on loopback inside
# its container, headers and all. The image has no curl, so bash asks.
http_get() {
  in_server bash -c "exec 3<>/dev/tcp/127.0.0.1/$1 && printf 'GET %s HTTP/1.0\r\n\r\n' '$2' >&3 && cat <&3" 2>&1
}

# collector_log prints all the collector has printed: every metric and
# span it received, in its debug exporter's text.
collector_log() { "${compose[@]}" logs --no-log-prefix collector 2>&1; }

# ariesd_log prints the server's log since ariesd last started.
ariesd_log() {
  "${compose[@]}" logs --no-log-prefix aries 2>&1 | awk '/msg="telemetry on"/ { n = 0 } { line[n++] = $0 } END { for (i = 0; i < n; i++) print line[i] }'
}

# spans reads the collector's log on stdin and prints each span it
# received as one line: its trace, its parent, its own id, its name and
# its attributes.
spans() {
  awk '
    function emit() {
      if (id != "") print "span trace=" trace " parent=" parent " id=" id " name=" name attrs " "
      id = ""; attrs = ""
    }
    /^Span #/ { emit(); inspan = 1; next }
    /^(Resource|Scope)(Spans|Metrics) #/ || /^Metric #/ || /^[0-9][0-9][0-9][0-9]-/ { emit(); inspan = 0 }
    !inspan { next }
    $1 == "Trace" && $2 == "ID" { trace = $4 }
    $1 == "Parent" && $2 == "ID" { parent = $4 }
    $1 == "ID" { id = $3 }
    $1 == "Name" { name = $3 }
    $1 == "->" {
      key = $2; sub(/:$/, "", key)
      value = $3; sub(/^[A-Za-z]+\(/, "", value); sub(/\)$/, "", value)
      attrs = attrs " " key "=" value
    }
    END { emit() }'
}

# records reads the collector's log on stdin and prints each log record
# it received as one line: its event, its trace and span, and its
# attributes. The record's body, which is a fixed message of ariesd's,
# is left out.
records() {
  awk '
    function emit() {
      if (inrecord) print "record event=" event " trace=" trace " span=" span attrs " "
      inrecord = 0; event = ""; trace = ""; span = ""; attrs = ""
    }
    /^LogRecord #/ { emit(); inrecord = 1; next }
    /^(Resource|Scope)(Spans|Metrics|Logs) #/ || /^Metric #/ || /^Span #/ || /^[0-9][0-9][0-9][0-9]-/ || /^\t\{/ { emit() }
    !inrecord { next }
    $1 == "EventName:" { event = $2 }
    $1 == "Trace" && $2 == "ID:" { trace = $3 }
    $1 == "Span" && $2 == "ID:" { span = $3 }
    $1 == "->" {
      key = $2; sub(/:$/, "", key)
      value = $3; sub(/^[A-Za-z]+\(/, "", value); sub(/\)$/, "", value)
      attrs = attrs " " key "=" value
    }
    END { emit() }'
}

# await LIMIT NAME TEST... runs TEST once a second until it passes, and
# fails after LIMIT tries, with what TEST left in `last`.
await() {
  local limit=$1 name=$2 i
  shift 2
  for ((i = 0; i < limit; i++)); do
    if "$@"; then
      passed=$((passed + 1))
      return 0
    fi
    sleep 1
  done
  fail "$name: not within ${limit}s" "$last"
}

# The tests await runs. shellcheck cannot see that they are called: 0.11
# reports that as SC2329, and the 0.9 on CI's runner as SC2317.

# arrived PREFIX NAME... passes once the collector has printed a metric
# of each NAME; PREFIX is what its debug exporter writes before a name.
# shellcheck disable=SC2317,SC2329
arrived() {
  local log name
  log=$(collector_log)
  for name in "${@:2}"; do
    if ! grep -q -- "^$1$name\$" <<<"$log"; then
      last="the collector has received no metric named $name"
      return 1
    fi
  done
}

# span_arrived PATTERN passes once the collector has printed a span
# whose line (spans) matches PATTERN, and leaves that line in `last`.
# shellcheck disable=SC2317,SC2329
span_arrived() {
  local all
  all=$(collector_log | spans)
  if last=$(grep -E -m 1 -- "$1" <<<"$all"); then
    return 0
  fi
  last="no span matching /$1/ among the $(grep -c '' <<<"$all") the collector received; the last of them:"$'\n'$(tail -n 5 <<<"$all")
  return 1
}

# record_arrived PATTERN is span_arrived for a log record (records),
# and leaves the newest record that matches in `last`.
# shellcheck disable=SC2317,SC2329
record_arrived() {
  local all
  all=$(collector_log | records)
  if last=$(grep -E -- "$1" <<<"$all" | tail -n 1) && [[ -n $last ]]; then
    return 0
  fi
  last="no log record matching /$1/ among the $(grep -c '' <<<"$all") the collector received; the last of them:"$'\n'$(tail -n 5 <<<"$all")
  return 1
}

# dropped SIGNAL prints how many exports of metrics, or spans or log
# records, ariesd has counted as dropped since it started.
dropped() {
  http_get 9464 /metrics | awk -v signal="signal=\"$1\"" '/^aries_telemetry_dropped_total/ && index($0, signal) { n = $NF } END { print n + 0 }'
}

# shellcheck disable=SC2317,SC2329
dropped_more() {
  local now
  now=$(dropped "$1")
  last="aries_telemetry_dropped_total{signal=\"$1\"} is $now, and was $2 before"
  ((now > $2))
}

# logged PATTERN prints how many lines of ariesd's log, since it last
# started, match PATTERN.
logged() { ariesd_log | grep -c -- "$1" || true; }

# shellcheck disable=SC2317,SC2329
logged_once_more() {
  local now
  now=$(logged "$1")
  last="ariesd logged /$1/ $now times since it started, and $2 times before"
  ((now == $2 + 1))
}

# readyz_says PATTERN passes once /readyz's answer has a line matching.
# shellcheck disable=SC2317,SC2329
readyz_says() {
  last=$(http_get 9464 /readyz | tr -d '\r') || true
  grep -Eq -- "$1" <<<"$last"
}

# rules_healthy COUNT passes once Prometheus has evaluated COUNT rules
# and found each one sound.
# shellcheck disable=SC2317,SC2329
rules_healthy() {
  local body ok
  body=$(http_get 9090 /api/v1/rules) || true
  ok=$(grep -o '"health":"ok"' <<<"$body" | grep -c '' || true)
  last="Prometheus reports $ok of $1 rules healthy: $(grep -o '"name":"[A-Za-z]*","query":"[^}]*"health":"[a-z]*","lastError":"[^"]*"' <<<"$body" | grep -v '"health":"ok"' || true)"
  ((ok == $1)) && ! grep -q '"health":"err"' <<<"$body"
}

# alert_is NAME STATE passes once Prometheus gives the alert that state.
# shellcheck disable=SC2317,SC2329
alert_is() {
  last=$(http_get 9090 /api/v1/rules | grep -o "\"state\":\"[a-z]*\",\"name\":\"$1\"") || true
  [[ $last == "\"state\":\"$2\",\"name\":\"$1\"" ]]
}

# batch TAG sends the same requests every time it is called, reads and
# writes, and prints what the reads answered.
batch() {
  local i
  for i in 1 2 3 4 5 6; do
    aries_as alice show "$a" || fail "batch $1: aries show exited $?"
    aries_as alice ready || fail "batch $1: aries ready exited $?"
    aries_as alice comment "$b" "telemetry batch $1 $i" >/dev/null || fail "batch $1: aries comment exited $?"
  done
}

# new_traceparent sets trace_id and parent_id to random ids, and
# traceparent to the W3C header that carries them, sampled.
new_traceparent() {
  trace_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  parent_id=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  traceparent=00-$trace_id-$parent_id-01
}

# telemetry_checks checks the harness of deploy/observability against
# the server the checks above have used, which has run since the
# restart with its telemetry on.
telemetry_checks() {
  local pushed=(aries.request.duration aries.project.requests aries.connections aries.handshakes
    aries.store.write.duration aries.stores.open aries.store.open.duration
    aries.claims aries.control.writes aries.keys_file.writes aries.keys_file.last_write.time
    aries.reaper.duration aries.reaper.last_run.time aries.clock.behind aries.db.pool.conns aries.db.pool.waits)
  local scraped=(aries_request_duration_seconds aries_project_requests_total aries_connections aries_handshakes_total
    aries_store_write_duration_seconds aries_stores_open aries_store_open_duration_seconds
    aries_claims_total aries_control_writes_total aries_keys_file_writes_total aries_keys_file_last_write_time_seconds
    aries_reaper_duration_seconds aries_reaper_last_run_time_seconds aries_clock_behind_seconds aries_db_pool_conns aries_db_pool_waits_total)
  local name='     -> Name: ' traced server_span attempt_span stand_in rules log all bad t0 alive_took alive dead
  local metrics0 traces0 logs0 warned_metrics warned_traces warned_logs recovered queries query refused_took frozen_took frozen_restart
  local req seq subject

  echo "smoke: telemetry: a trace from the test's own TRACEPARENT"
  expect "status says people are named by stand-ins" '^traces: people are named by stand-ins' alice admin status
  new_traceparent
  TRACEPARENT=$traceparent expect "a request with a trace context" '^ar-[a-z0-9]+$' alice create "Traced by the smoke test" -p 3 -t task
  traced=$last
  # The server's span is a child of the test's, in the test's trace; the
  # attempt of the write is beneath it, and the wait for the writer
  # beneath that.
  await 30 "the request's span arrives, under the test's span" span_arrived "^span trace=$trace_id parent=$parent_id id=[0-9a-f]{16} name=create op=create result=ok "
  server_span=$(sed -n 's/.* id=\([0-9a-f]*\) name=create .*/\1/p' <<<"$last")
  stand_in=$(sed -n 's/.* principal=\(p_[0-9a-f]*\) .*/\1/p' <<<"$last")
  grep -Eq -- " project=$project .* principal=p_[0-9a-f]{16} session=s_[0-9a-f]{16} " <<<"$last" ||
    fail "the request's span does not carry the project and stand-ins for the principal and the session" "$last"
  passed=$((passed + 1))
  await 10 "the write's attempt is beneath the request's span" span_arrived "^span trace=$trace_id parent=$server_span id=[0-9a-f]{16} name=store.write.attempt .* outcome=ok "
  attempt_span=$(sed -n 's/.* id=\([0-9a-f]*\) name=store.write.attempt .*/\1/p' <<<"$last")
  await 10 "the wait for the writer is beneath the attempt" span_arrived "^span trace=$trace_id parent=$attempt_span id=[0-9a-f]{16} name=store.write.wait "
  # A request that brings no trace context starts a trace of its own.
  expect "a claim, for the claims counter" "^$traced .*in_progress" alice start "$traced"
  await 30 "a request with no trace context is the root of its own trace" span_arrived "^span trace=[0-9a-f]{32} parent= id=[0-9a-f]{16} name=start "

  echo "smoke: telemetry: stand-ins, not names"
  expect "a server admin resolves the stand-in" "^$stand_in is alice\$" alice admin principal whois "$stand_in"
  refuse "nobody else does" '"code":"forbidden"' bob --json admin principal whois "$stand_in"
  log=$(collector_log)
  all=$(spans <<<"$log")
  grep -Eq ' principal=p_[0-9a-f]{16} session=s_[0-9a-f]{16} ' <<<"$all" || fail "no span says who, by a stand-in"
  bad=$(grep -Eo ' (principal|session)=[^ ]*' <<<"$all" | grep -Ev '^ (principal=p_|session=s_)[0-9a-f]{16}$' || true)
  [[ -z $bad ]] || fail "a span names a principal or a session by something other than a stand-in" "$(sort -u <<<"$bad" | head -n 5)"
  passed=$((passed + 1))

  # ariesd's log, exported (docs/server.md, Logs over OTLP). A
  # record carries the ids of the span its line was logged under: a
  # request the test traces and the server refuses has a line at info,
  # so a record, in the test's trace and with the id of the server's
  # own span for the request.
  echo "smoke: telemetry: log records, with their trace, and the audit stream"
  new_traceparent
  TRACEPARENT=$traceparent refuse "a traced request that is refused" 'not found' alice show ar-nosuchid
  await 30 "the refused request's span arrives" span_arrived "^span trace=$trace_id parent=$parent_id id=[0-9a-f]{16} name=show op=show result=not_found "
  server_span=$(sed -n 's/.* id=\([0-9a-f]*\) name=show .*/\1/p' <<<"$last")
  await 30 "its log record arrives, in the test's trace and under the server's span" record_arrived \
    "^record event=request.refused trace=$trace_id span=$server_span .* principal=$stand_in session=s_[0-9a-f]{16} .* op=show .* code=not_found req=[0-9a-f]{16} "
  req=$(sed -n 's/.* req=\([0-9a-f]*\) .*/\1/p' <<<"$last")
  # The journal has the same line, found by the request's id, with the
  # name the record leaves out.
  last=$("${compose[@]}" logs --no-log-prefix aries 2>&1 | grep -F " req=$req " || true)
  grep -Eq " principal=alice .* event=request.refused op=show .* code=not_found req=$req trace_id=$trace_id span_id=$server_span\$" <<<"$last" ||
    fail "ariesd's own log has no line for the refused request, by its id $req, naming alice and the trace" "$last"
  passed=$((passed + 1))
  # An access change is a record of the audit stream at the collector:
  # who made it and whom it changed are stand-ins, which a server admin
  # resolves, and the line in the server's log, by the event's number,
  # has the names.
  expect "an access change" '^principal erin added, a member of project first$' alice admin principal add erin
  last=$("${compose[@]}" logs --no-log-prefix aries 2>&1 | grep -E ' event=audit[.]principal[.]add audit=true seq=[0-9]+ at=[^ ]+ principal=alice session=[^ ]+ op=principal[.]add target=erin subject=erin$' || true)
  seq=$(sed -n 's/.* seq=\([0-9]*\) .*/\1/p' <<<"$last")
  [[ $seq =~ ^[0-9]+$ ]] || fail "ariesd's own log has no one audit line for the principal alice added, naming alice and erin" "$last"
  passed=$((passed + 1))
  await 30 "its audit record arrives, with the event's number" record_arrived \
    "^record event=audit.principal.add trace= span= event=audit.principal.add audit=true seq=$seq at=[^ ]+ principal=$stand_in session=s_[0-9a-f]{16} op=principal.add subject=p_[0-9a-f]{16} "
  subject=$(sed -n 's/.* subject=\(p_[0-9a-f]*\) .*/\1/p' <<<"$last")
  expect "a server admin resolves whom it changed" "^$subject is erin\$" alice admin principal whois "$subject"
  await 10 "the membership it gave is a record too" record_arrived \
    "^record event=audit.member.set .* audit=true seq=$((seq + 1)) .* principal=$stand_in .* op=member.set project=$project subject=$subject role=member "
  # Every field of a record that names a person holds a stand-in, and
  # no record has a field that holds text: an error, a target, a
  # machine's name, a path.
  log=$(collector_log)
  all=$(records <<<"$log")
  bad=$(grep -Eo ' (principal|session|by|subject|admin|created_by|added_by)=[^ ]*' <<<"$all" |
    grep -Ev '^ ((principal|by|subject|admin|created_by|added_by)=p_|session=s_)[0-9a-f]{16}$' || true)
  [[ -z $bad ]] || fail "a log record names a person by something other than a stand-in" "$(sort -u <<<"$bad" | head -n 5)"
  bad=$(grep -E ' (err|detail|cause|fix|effect|target|machine|harness|name|database|path|url)=' <<<"$all" || true)
  [[ -z $bad ]] || fail "a log record carries a field that holds text" "$(head -n 3 <<<"$bad")"
  passed=$((passed + 2))
  # Nothing the collector received, in a span, a metric or a log
  # record, names a person, a session or an issue's title.
  if last=$(grep -Ew -m 3 -- 'alice|bob|carol|dave|erin|mallory' <<<"$log") || last=$(grep -E -m 3 -- 'example[.]com|smoke-|Traced by' <<<"$log"); then
    fail "what the collector received names a person, a session or a title" "$last"
  fi
  passed=$((passed + 1))

  echo "smoke: telemetry: metrics from ariesd, pushed and scraped"
  await 90 "ariesd's metrics arrive over OTLP" arrived "$name" "${pushed[@]}"
  await 60 "ariesd's metrics arrive by a scrape of /metrics" arrived "$name" "${scraped[@]}"
  log=$(collector_log)
  grep -Fq -- "     -> service.name: Str(ariesd)" <<<"$log" || fail "the collector received nothing whose resource is the service ariesd"
  passed=$((passed + 1))

  echo "smoke: telemetry: the alert rules in Prometheus, and /readyz"
  rules=$(grep -c '^ *- alert: ' "$root/deploy/observability/alerts.yaml")
  await 90 "Prometheus evaluates every alert rule" rules_healthy "$rules"
  await 30 "no span has been dropped, so its alert is quiet" alert_is AriesTelemetrySpansDropped inactive
  # Every query of the dashboard is one that Prometheus can run.
  queries=0
  while IFS= read -r query; do
    last=$("${compose[@]}" exec -T prometheus promtool query instant http://127.0.0.1:9090 "$query" 2>&1 </dev/null) ||
      fail "Prometheus cannot run a query of the dashboard: $query" "$last"
    queries=$((queries + 1))
  done < <(sed -n 's/^ *"expr": "\(.*\)",$/\1/p' "$root/deploy/observability/dashboard.json" | sed 's/\\"/"/g')
  ((queries >= 20)) || fail "found only $queries queries in the dashboard"
  passed=$((passed + 1))
  await 10 "/readyz says ready" readyz_says '^ready$'
  # Postgres frozen whole, as a hang would leave it: it holds its port
  # and answers nothing. The status report answers all the same, from
  # what ariesd holds in memory, and says why.
  "${compose[@]}" pause postgres >/dev/null 2>&1 || fail "could not freeze Postgres"
  if ! (await 30 "/readyz says why not, with Postgres frozen" readyz_says '^not ready: the last check of the database failed$'); then
    "${compose[@]}" unpause postgres >/dev/null 2>&1 || true
    exit 1
  fi
  passed=$((passed + 1))
  if ! (expect "status answers with Postgres frozen" '^postgres: unknown \(' alice admin status); then
    "${compose[@]}" unpause postgres >/dev/null 2>&1 || true
    exit 1
  fi
  passed=$((passed + 1))
  "${compose[@]}" unpause postgres >/dev/null 2>&1 || fail "could not thaw Postgres"
  await 30 "/readyz says ready again" readyz_says '^ready$'
  grep -q '^HTTP/1.0 200' <<<"$(http_get 9464 /healthz)" || fail "/healthz does not answer 200"
  passed=$((passed + 1))

  # design §16.6: a collector that is down, slow or refusing changes
  # nothing but the drop counter and one warning. The same requests are
  # sent three times: with the collector running, with it stopped, so
  # that its port refuses, and with it frozen, so that its port accepts
  # and never answers. An export that a request waited for would hold
  # the frozen run for the exporter's 10 seconds, at each request.
  echo "smoke: telemetry: a collector that is down changes nothing"
  t0=$SECONDS
  alive=$(batch alive)
  alive_took=$((SECONDS - t0))
  metrics0=$(dropped metrics)
  traces0=$(dropped traces)
  warned_metrics=$(logged 'a metrics export failed')
  warned_traces=$(logged 'a trace export failed')
  warned_logs=$(logged 'a log export failed')
  logs0=$(dropped logs)
  recovered=$(logged 'event=telemetry.export_recovered signal=metrics')
  "${compose[@]}" stop collector >/dev/null 2>&1 || fail "could not stop the collector"
  t0=$SECONDS
  dead=$(batch refused)
  refused_took=$((SECONDS - t0))
  # A line at info, so that there is a record to lose.
  refuse "a refused request, for its log line" 'not found' alice show ar-nosuchid
  ((refused_took <= alive_took + 5)) ||
    fail "with the collector stopped, the requests took ${refused_took}s, and ${alive_took}s with it running"
  [[ $dead == "$alive" ]] || fail "with the collector stopped, the requests answered something else" "$(diff <(printf '%s\n' "$alive") <(printf '%s\n' "$dead"))"
  passed=$((passed + 2))
  await 30 "a failed export of metrics is counted" dropped_more metrics "$metrics0"
  await 30 "the spans lost are counted" dropped_more traces "$traces0"
  await 10 "one warning for metrics" logged_once_more 'a metrics export failed' "$warned_metrics"
  await 10 "one warning for traces" logged_once_more 'a trace export failed' "$warned_traces"
  await 30 "the log records lost are counted" dropped_more logs "$logs0"
  await 10 "one warning for logs" logged_once_more 'a log export failed' "$warned_logs"
  await 90 "the alert for dropped spans fires" alert_is AriesTelemetrySpansDropped firing
  await 60 "the alert for dropped log records fires" alert_is AriesTelemetryLogRecordsDropped firing
  "${compose[@]}" start collector >/dev/null 2>&1 || fail "could not start the collector again"
  new_traceparent
  TRACEPARENT=$traceparent expect "a request once the collector is back" "^$traced " alice show "$traced"
  await 60 "its span arrives" span_arrived "^span trace=$trace_id parent=$parent_id id=[0-9a-f]{16} name=show "
  await 30 "ariesd says its exports of metrics work again" logged_once_more 'event=telemetry.export_recovered signal=metrics' "$recovered"
  [[ $(logged 'a metrics export failed') == $((warned_metrics + 1)) && $(logged 'a trace export failed') == $((warned_traces + 1)) &&
    $(logged 'a log export failed') == $((warned_logs + 1)) ]] ||
    fail "ariesd warned of failed exports more than once for each signal while the collector was stopped" "$(ariesd_log | grep 'export')"
  passed=$((passed + 1))

  echo "smoke: telemetry: a collector that never answers changes nothing, and does not hold a stop"
  "${compose[@]}" pause collector >/dev/null 2>&1 || fail "could not freeze the collector"
  t0=$SECONDS
  dead=$(batch frozen)
  frozen_took=$((SECONDS - t0))
  ((frozen_took <= alive_took + 5)) ||
    fail "with the collector frozen, the requests took ${frozen_took}s, and ${alive_took}s with it running"
  [[ $dead == "$alive" ]] || fail "with the collector frozen, the requests answered something else" "$(diff <(printf '%s\n' "$alive") <(printf '%s\n' "$dead"))"
  passed=$((passed + 2))
  # ariesd gives its last exports 5 seconds when it stops. Docker is
  # told to wait for 30, so that the time measured is the server's own;
  # 3 more are for a clock read in whole seconds.
  t0=$SECONDS
  "${compose[@]}" restart -t 30 aries >/dev/null 2>&1 || fail "could not restart the server with the collector frozen"
  frozen_restart=$((SECONDS - t0))
  ((frozen_restart <= restart_took + 5 + 3)) ||
    fail "with the collector frozen, restarting the server took ${frozen_restart}s, and ${restart_took}s with it running: more than the 5s its last exports may take"
  passed=$((passed + 1))
  echo "smoke: telemetry: 18 requests took ${alive_took}s with the collector running, ${refused_took}s with it stopped and ${frozen_took}s with it frozen; a restart took ${restart_took}s and, with it frozen, ${frozen_restart}s"
  "${compose[@]}" unpause collector >/dev/null 2>&1 || fail "could not thaw the collector"
  "${compose[@]}" restart "${sidecars[@]}" >/dev/null 2>&1 || fail "could not restart ${sidecars[*]} after the server"
  up
  new_traceparent
  TRACEPARENT=$traceparent expect "a request after the restart" "^$traced " alice show "$traced"
  await 60 "its span arrives at the collector" span_arrived "^span trace=$trace_id parent=$parent_id id=[0-9a-f]{16} name=show .* principal=$stand_in "
  new_traceparent
  TRACEPARENT=$traceparent refuse "a refused request after the restart" 'not found' alice show ar-nosuchid
  await 30 "its log record arrives at the collector" record_arrived "^record event=request.refused trace=$trace_id "
}

# What follows, to identity_checks, runs only with ARIES_SMOKE_IDENTITY=1.
# None of it puts a password, a secret, a code or a token in an
# argument or in what it prints: each goes from Keycloak's volume, or
# from one answer to the next request, through a pipe or a variable.

# kc_curl ARGS... is curl to Keycloak from this machine, which reaches
# keycloak.localhost on loopback. --resolve says so for a curl that
# does not resolve *.localhost itself.
kc_curl() {
  curl -sS --max-time 20 --resolve "keycloak.localhost:$ARIES_IDENTITY_PORT:127.0.0.1" "$@"
}

# kc_secret NAME writes the generated secret in NAME's file on
# Keycloak's volume to stdout, for a pipe.
kc_secret() { "${compose[@]}" exec -T keycloak cat "/opt/keycloak/data/aries/$1"; }

# kc_get_in_server PATH prints the body of what Keycloak answers to GET
# PATH, asked from the server's container by the issuer's name. The
# image has no curl, so bash asks.
kc_get_in_server() {
  # The script is the container's bash's, which expands $1 and $2.
  # shellcheck disable=SC2016
  in_server bash -c 'exec 3<>"/dev/tcp/keycloak.localhost/$1" &&
    printf "GET %s HTTP/1.0\r\nHost: keycloak.localhost:%s\r\n\r\n" "$2" "$1" >&3 &&
    sed "1,/^\r$/d" <&3' - "$ARIES_IDENTITY_PORT" "$1" 2>&1
}

# jwt_part TOKEN N prints the Nth part of a compact JWT, decoded: 1 is
# its header, 2 its claims. Never the signature, nor the token.
jwt_part() {
  local p
  p=$(cut -d. -f"$2" <<<"$1" | tr '_-' '/+')
  while ((${#p} % 4)); do p+='='; done
  openssl base64 -d -A <<<"$p"
}

# json_string NAME prints the string NAME holds in the JSON on stdin.
json_string() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }

# has NAME TEXT PATTERN fails unless TEXT matches the extended regexp.
has() {
  grep -Eq -- "$3" <<<"$2" || fail "$1: nothing matches /$3/" "$2"
  passed=$((passed + 1))
}

# pkce_challenge VERIFIER prints S256 of VERIFIER, base64url.
pkce_challenge() {
  printf '%s' "$1" | openssl dgst -sha256 -binary | openssl base64 -A | tr '+/' '-_' | tr -d '='
}

random_b64url() { openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n'; }

# browser_sign_in WHO VERIFIER signs WHO in to aries-cli as a browser
# would, by the code flow with PKCE (S256 of a verifier of its own), a
# nonce and prompt=login, and exchanges the code with VERIFIER, which
# is the right one or not. WHO's password goes from Keycloak's volume
# to the login form through a pipe. It sets token_answer to Keycloak's
# answer to the exchange, and nonce to the nonce sent.
redirect_uri=http://127.0.0.1:53682/callback
token_answer=
nonce=
browser_sign_in() {
  local who=$1 verifier state page action location code jar
  # Keycloak marks its cookies Secure, which a browser sends to
  # http://*.localhost and curl does not: they go back to the form in a
  # Cookie header made from the page's, read from a file.
  jar=$(mktemp "$work/kc-jar.XXXXXX")
  state=$(random_b64url)
  nonce=$(random_b64url)
  verifier=$(random_b64url)$(random_b64url)
  page=$(kc_curl -D "$jar" -G "$issuer/protocol/openid-connect/auth" \
    --data-urlencode client_id=aries-cli --data-urlencode response_type=code \
    --data-urlencode 'scope=openid email' --data-urlencode "redirect_uri=$redirect_uri" \
    --data-urlencode "state=$state" --data-urlencode "nonce=$nonce" \
    --data-urlencode "code_challenge=$(pkce_challenge "$verifier")" --data-urlencode code_challenge_method=S256 \
    --data-urlencode prompt=login) || fail "the sign-in page did not load" "$page"
  printf 'Cookie: %s\n' "$(sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\);.*/\1/p' "$jar" | paste -sd ';' -)" >"$jar.cookie"
  action=$(grep -o 'action="[^"]*"' <<<"$page" | head -n 1 | sed 's/^action="//; s/"$//; s/&amp;/\&/g')
  [[ $action == "$issuer/login-actions/authenticate?"* ]] || fail "the sign-in page has no login form" "$(head -c 400 <<<"$page")"
  location=$(kc_secret "$who.pass" | kc_curl -H @"$jar.cookie" -o /dev/null -w '%{redirect_url}' \
    --data-urlencode "username=$who" --data-urlencode 'password@-' "$action") ||
    fail "the login form was not accepted"
  rm -f "$jar" "$jar.cookie"
  [[ $location == "$redirect_uri?"* ]] || fail "signing in as $who did not return to the loopback redirect" "${location:-(no redirect)}"
  [[ $location == *"state=$state"* ]] || fail "the redirect does not carry the state sent"
  code=$(sed -n 's/.*[?&]code=\([^&]*\).*/\1/p' <<<"$location")
  [[ -n $code ]] || fail "the redirect carries no code"
  [[ $2 == right ]] || verifier=$(random_b64url)$(random_b64url)
  token_answer=$(printf 'grant_type=authorization_code&client_id=aries-cli&code=%s&redirect_uri=%s&code_verifier=%s' \
    "$code" "$redirect_uri" "$verifier" | kc_curl --data-binary @- "$issuer/protocol/openid-connect/token") || true
}

# identity_checks checks the `identity` profile: Keycloak and the realm
# identity.sh makes (docs/deploy/docker.md, "Sign-in with a local
# identity provider").
identity_checks() {
  local issuer=http://keycloak.localhost:$ARIES_IDENTITY_PORT/realms/aries
  local body token claims header answer

  echo "smoke: identity: the discovery document and keys, from here and from the server's container"
  body=$(kc_curl "$issuer/.well-known/openid-configuration") || fail "no discovery document at $issuer" "$body"
  has "the issuer is the one URL" "$body" "\"issuer\":\"$issuer\""
  has "the device grant is offered" "$body" "\"device_authorization_endpoint\":\"$issuer/protocol/openid-connect/auth/device\""
  has "PKCE with S256 is offered" "$body" '"code_challenge_methods_supported":\[[^]]*"S256"'
  has "the claims sign-in needs are offered" "$body" '"claims_supported":\[[^]]*"auth_time"[^]]*"email"[^]]*"nonce"'
  body=$(kc_curl "$issuer/protocol/openid-connect/certs") || fail "no JWKS" "$body"
  has "an ES256 signing key" "$body" '"kty":"EC"[^}]*"alg":"ES256"|"alg":"ES256"[^}]*"kty":"EC"'
  has "an RS256 signing key" "$body" '"alg":"RS256"'
  body=$(kc_get_in_server "/realms/aries/.well-known/openid-configuration")
  has "the server's container reaches it by the same name, with the same issuer" "$body" "\"issuer\":\"$issuer\""
  body=$(kc_get_in_server "/realms/aries/protocol/openid-connect/certs")
  has "and its keys" "$body" '"alg":"ES256"'

  echo "smoke: identity: the realm, its clients and its people"
  body=$("${compose[@]}" exec -T keycloak /opt/aries/identity.sh show 2>&1) || fail "identity.sh show failed" "$body"
  printf '%s\n' "$body" | sed 's/^/  | /'
  has "the realm" "$body" '^realm aries enabled=true$'
  has "aries login's client: public, PKCE, the device grant, ES256" "$body" \
    '^client aries-cli public=true code=true service=false password=false pkce=S256 device=true id_token=ES256$'
  has "the server's client: confidential" "$body" '^client aries-server public=false code=true service=false password=false pkce=S256 '
  has "the service principal's client: client credentials only" "$body" '^client ci-example public=false code=false service=true password=false '
  has "alice, verified" "$body" '^user alice email=alice@example.com email_verified=true enabled=true$'
  has "bob, verified" "$body" '^user bob email=bob@example.com email_verified=true enabled=true$'
  has "every secret's file is 0600" "$body" '^file aries-server.secret 600$'
  if grep -E '^file ' <<<"$body" | grep -Ev ' 600$' >/dev/null; then
    fail "a secret's file is not 0600" "$body"
  fi
  passed=$((passed + 1))
  has "in a 0700 directory" "$body" '^dir 700$'
  if "${compose[@]}" logs --no-log-prefix keycloak 2>&1 | grep -Fq -f <(
    for f in admin.pass alice.pass bob.pass aries-server.secret ci-example.secret; do
      kc_secret "$f"
      echo
    done
  ); then
    fail "Keycloak's log holds a generated password or secret"
  fi
  passed=$((passed + 1))

  echo "smoke: identity: alice signs in with PKCE and a nonce"
  browser_sign_in alice right
  token=$(json_string id_token <<<"$token_answer")
  [[ -n $token ]] || fail "the code exchange returned no ID token" \
    "$(json_string error <<<"$token_answer"): $(json_string error_description <<<"$token_answer")"
  header=$(jwt_part "$token" 1)
  claims=$(jwt_part "$token" 2)
  token=
  printf '  claims: %s\n' "$claims"
  has "signed ES256" "$header" '"alg":"ES256"'
  has "the issuer" "$claims" "\"iss\":\"$issuer\""
  has "for aries login" "$claims" '"aud":"aries-cli"'
  has "the nonce sent" "$claims" "\"nonce\":\"$nonce\""
  has "auth_time" "$claims" '"auth_time":[0-9]+[,}]'
  has "her address" "$claims" '"email":"alice@example.com"'
  has "verified" "$claims" '"email_verified":true'
  if grep -q '"refresh_token"' <<<"$token_answer"; then
    fail "the exchange returned a refresh token"
  fi
  passed=$((passed + 1))
  token_answer=
  browser_sign_in alice wrong
  has "a code exchanged with the wrong PKCE verifier is refused" "$token_answer" '"error":"invalid_grant"'
  token_answer=

  echo "smoke: identity: the device grant"
  answer=$(kc_curl "$issuer/protocol/openid-connect/auth/device" -d client_id=aries-cli -d 'scope=openid email' \
    --data-urlencode "code_challenge=$(pkce_challenge "$(random_b64url)")" -d code_challenge_method=S256) ||
    fail "the device grant did not answer" "$answer"
  has "a user code" "$answer" '"user_code":"[A-Z]{4}-[A-Z]{4}"'
  has "and a page to enter it on" "$answer" "\"verification_uri\":\"$issuer/device\""
  answer=

  echo "smoke: identity: a token for the service principal"
  answer=$({
    printf 'user = "ci-example:'
    kc_secret ci-example.secret
    printf '"\n'
  } | kc_curl -K - -d grant_type=client_credentials "$issuer/protocol/openid-connect/token") || true
  token=$(json_string access_token <<<"$answer")
  answer=
  [[ -n $token ]] || fail "the client-credentials grant returned no token"
  claims=$(jwt_part "$token" 2)
  token=
  printf '  claims: %s\n' "$claims"
  has "the issuer" "$claims" "\"iss\":\"$issuer\""
  has "for the workload audience" "$claims" '"aud":(\["aries.example.com"|"aries.example.com"|\[[^]]*"aries.example.com")'
  has "from ci-example" "$claims" '"azp":"ci-example"'
  has "with the claims a trust rule matches" "$claims" '"repository":"example/app"'
  answer=$(printf 'user = "ci-example:not-the-secret"\n' |
    kc_curl -K - -d grant_type=client_credentials "$issuer/protocol/openid-connect/token") || true
  has "a wrong secret is refused" "$answer" '"error":"(invalid_client|unauthorized_client)"'
  answer=
}

# login_by_url WHO PERSON runs `aries login --url` as WHO, signs PERSON
# in at Keycloak with curl as the browser on another machine, which
# stops at the loopback address it cannot open, and pastes that address
# back, as a person would. It sets last to what aries printed, and
# returns aries's status. PERSON's password goes from Keycloak's volume
# to the login form through a pipe; the URLs carry no secret.
login_by_url() {
  local who=$1 person=$2 fifo=$work/login-$1.fifo errf=$work/login-$1.err outf=$work/login-$1.out
  local authorize='' location page action jar pid status=0 i
  rm -f "$fifo"
  mkfifo "$fifo"
  aries_as "$who" login --url <"$fifo" >"$outf" 2>"$errf" &
  pid=$!
  # Held open so that aries waits for the address on it.
  exec 4>"$fifo"
  for ((i = 0; i < 150; i++)); do
    authorize=$(grep -Eo "https://localhost:$https_port/oauth/authorize[?][^ ]+" "$errf" | head -n 1 || true)
    [[ -n $authorize ]] && break
    sleep 0.2
  done
  [[ -n $authorize ]] || fail "aries login --url printed no URL" "$(cat "$errf")"
  location=$(curl -sS --max-time 20 --cacert "$work/ca.pem" -o /dev/null -w '%{redirect_url}' "$authorize") ||
    fail "the server's authorization endpoint did not answer"
  [[ $location == "$issuer/protocol/openid-connect/auth?"* ]] || fail "the server did not send the browser to Keycloak" "${location:-(no redirect)}"
  [[ $location == *"prompt=login"* && $location == *"code_challenge_method=S256"* && $location == *"nonce="* ]] ||
    fail "the server's request to Keycloak has no prompt=login, PKCE or nonce" "$location"
  jar=$(mktemp "$work/kc-jar.XXXXXX")
  page=$(kc_curl -D "$jar" "$location") || fail "Keycloak's sign-in page did not load"
  printf 'Cookie: %s\n' "$(sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\);.*/\1/p' "$jar" | paste -sd ';' -)" >"$jar.cookie"
  action=$(grep -o 'action="[^"]*"' <<<"$page" | head -n 1 | sed 's/^action="//; s/"$//; s/&amp;/\&/g')
  [[ $action == "$issuer/login-actions/authenticate?"* ]] || fail "Keycloak's sign-in page has no login form" "$(head -c 400 <<<"$page")"
  location=$(kc_secret "$person.pass" | kc_curl -H @"$jar.cookie" -o /dev/null -w '%{redirect_url}' \
    --data-urlencode "username=$person" --data-urlencode 'password@-' "$action") || fail "Keycloak did not take the login form"
  rm -f "$jar" "$jar.cookie"
  [[ $location == "https://localhost:$https_port/auth/callback?"* ]] || fail "Keycloak did not send the browser back to the server" "${location:-(no redirect)}"
  location=$(curl -sS --max-time 20 --cacert "$work/ca.pem" -o /dev/null -w '%{redirect_url}' "$location") ||
    fail "the server's callback did not answer"
  [[ $location =~ ^http://127[.]0[.]0[.]1:[0-9]+/callback[?] ]] || fail "the server did not send the browser back to aries" "${location:-(no redirect)}"
  printf 'not an address\n%s\n' "$location" >&4
  exec 4>&-
  wait "$pid" || status=$?
  last=$(cat "$outf" "$errf")
  rm -f "$fifo" "$outf" "$errf"
  return "$status"
}

# signin_checks signs bob in end to end through the server at Keycloak
# (docs/server.md, "Sign-in"), with ARIES_SMOKE_IDENTITY=1: the server
# signs people in at Keycloak offline, its keys from a file; bob, invited
# there, signs in by URL; his commands then connect with the sign-in; and
# aries logout ends it. alice, whom nobody invited, is refused.
signin_checks() {
  local issuer=http://keycloak.localhost:$ARIES_IDENTITY_PORT/realms/aries token tokenfile
  echo "smoke: sign-in: bob, invited at the server's provider, signs in by URL through the server"
  last=$(curl -sS --max-time 20 --cacert "$work/ca.pem" "https://localhost:$https_port/.well-known/aries") || fail "no /.well-known/aries"
  grep -q '"auth":\["key","signin"\],"providers":\[{"name":"keycloak","kind":"oidc"}\]' <<<"$last" ||
    fail "/.well-known/aries does not name the sign-in and its provider" "$last"
  passed=$((passed + 1))
  refuse "an invitation at a provider the server does not have" 'this server signs no one in at google' alice admin invite add bob bob@example.com --provider google
  expect "an invitation at the server's provider" '^invitation [0-9]+ for bob: bob@example.com at keycloak' alice admin invite add bob bob@example.com --provider keycloak
  # bob's sign-in is kept in a data directory of his own: the users here
  # share a home, and a sign-in is the machine's user's for the server.
  install -d -m 0700 "$work/data-bob" "$work/data-alice"
  smoke_data=$work/data-bob login_by_url bob bob || fail "aries login --url as bob failed" "$last"
  grep -Eq '^signed in as bob at keycloak, until [0-9-]+ [0-9:]+ UTC [(]sign-in g-[a-z2-7]{16}[)]$' <<<"$last" || fail "aries login printed no sign-in" "$last"
  passed=$((passed + 1))
  smoke_data=$work/data-bob expect "whoami shows the sign-in" '^signed in +at keycloak, until .* scopes cli mcp' bob whoami
  smoke_data=$work/data-bob expect "bob works, signed in" '^ar-[a-z0-9]+$' bob create "Signed in at Keycloak" -p 3 -t task
  expect "his identity is bound" '^[0-9]+ +keycloak +bob@example[.]com +bound ' alice admin identity list bob
  expect "an admin sees his sign-in" 'live' alice admin signin list bob
  expect "the status counts it, and the provider's keys are a file's" '^sign-in: 1 live$' alice admin status
  grep -Eq '^  provider keycloak [(]oidc[)] http://keycloak[.]localhost:[0-9]+/realms/aries: endpoints written out; keys from a file, nothing fetched, [0-9]+ keys?, read ' <<<"$last" ||
    fail "the status does not show Keycloak's keys from a file" "$last"
  passed=$((passed + 1))
  # The token is bob's alone, and in no output and no log line.
  tokenfile=$(find "$work/data-bob" -path '*/signin/*' -name token.json | head -n 1)
  [[ -n $tokenfile ]] || fail "no sign-in kept in bob's data directory"
  [[ $(stat -f '%Lp' "$tokenfile" 2>/dev/null || stat -c '%a' "$tokenfile") == 600 ]] || fail "the token's file is not 0600"
  token=$(sed -n 's/.*"token": "\([^"]*\)".*/\1/p' "$tokenfile")
  [[ -n $token ]] || fail "the token's file holds no token"
  if "${compose[@]}" logs --no-log-prefix aries 2>&1 | grep -Fq -- "$token"; then
    fail "ariesd's log holds a token"
  fi
  "${compose[@]}" logs --no-log-prefix aries 2>&1 | grep -Eq ' event=signin[.]granted principal=bob provider=keycloak grant=g-' ||
    fail "ariesd logged no signin.granted for bob" "$("${compose[@]}" logs --no-log-prefix aries 2>&1 | grep signin | tail -n 5)"
  token=
  passed=$((passed + 2))
  smoke_data=$work/data-bob expect "logout ends it" '^signed out of https://localhost:[0-9]+ [(]sign-in g-' bob logout
  smoke_data=$work/data-bob expect "and bob is back on his key" '^key +SHA256:' bob whoami
  expect "and the server ended it" 'ended [(]logout[)]' alice admin signin list bob

  echo "smoke: sign-in: alice, whom nobody invited at Keycloak, is refused"
  if smoke_data=$work/data-alice login_by_url alice alice; then
    fail "aries login as alice, uninvited, succeeded" "$last"
  fi
  grep -q 'no invitation for alice@example.com at keycloak' <<<"$last" || fail "alice's refusal does not say she is not invited" "$last"
  grep -q 'aries admin invite add' <<<"$last" || fail "alice's refusal names no fix" "$last"
  passed=$((passed + 2))
}

image=${ARIES_IMAGE:-aries-dev:local}
if [[ -n ${ARIES_IMAGE:-} ]]; then
  echo "smoke: testing the image $image, and building aries"
else
  echo "smoke: building the image and aries"
  docker build -q -f "$root/deploy/docker/Dockerfile" -t "$image" "$@" "$root" >/dev/null
fi
if [[ -n ${ARIES:-} ]]; then
  aries=$ARIES
else
  aries=$work/aries
  (cd "$root" && CGO_ENABLED=0 go build -trimpath -o "$aries" ./cmd/aries)
fi

mkdir -p "$work/keys" "$work/home" "$work/alice" "$work/bob" "$work/carol" "$work/alice-web" "$work/bob-web" "$work/transcripts" \
  "$work/bob-key" "$work/bob-out"
keys=
for who in alice bob; do
  ssh-keygen -q -t ed25519 -N '' -C "$who@example.com" -f "$work/keys/$who"
  keys+="$who $(cat "$work/keys/$who.pub")"$'\n'
done

if [[ -n $ssh ]]; then
  echo "smoke: starting the server, HTTPS on 127.0.0.1:$https_port and SSH on 127.0.0.1:$port; aries over SSH"
else
  echo "smoke: starting the server, HTTPS alone on 127.0.0.1:$https_port; aries over HTTPS"
fi
"${compose[@]}" down -v >/dev/null 2>&1 || true
up
project=$(server_setting project)
url=$(server_setting url)
host_key=$(server_setting host_key)
[[ $project =~ ^[0-9a-f-]{36}$ && $url == "https://localhost:$https_port" && (-z $ssh || $host_key == SHA256:*) ]] ||
  fail "the server printed no .aries.yaml" "$("${compose[@]}" logs --no-log-prefix 2>&1 | tail -n 20)"
in_server cat /etc/aries/tls/ca.pem >"$work/ca.pem" || fail "the server has no test CA at /etc/aries/tls/ca.pem"
for who in alice bob; do
  yaml_for "$project" >"$work/$who/.aries.yaml"
done
no_ssh_path "at the start"

# Which key aries offers. alice is the server admin and bob a member; an
# agent that holds alice's key behind six the server does not know is in
# the environment throughout.
echo "smoke: a configured key is the only key offered"
start_agent
for i in 1 2 3 4 5 6; do
  ssh-keygen -q -t ed25519 -N '' -C "stray-$i@example.com" -f "$work/keys/stray-$i"
  agent_add "$work/keys/stray-$i"
done
agent_add "$work/keys/alice"
bob_key_fp=$(ssh-keygen -lf "$work/keys/bob.pub" | cut -d' ' -f2 | sed 's/[+/.]/[&]/g')
smoke_agent=$agent_sock expect "ARIES_KEY_FILE wins over the agent's keys" '^principal +bob$' bob whoami
grep -Eq -- "^key +$bob_key_fp, from the file $work/keys/bob, which ARIES_KEY_FILE names\$" <<<"$last" || fail "whoami does not name bob's key, its file and what named it" "$last"
passed=$((passed + 1))
# The same with the key in the checkout, named by key: in .aries.yaml.
cp "$work/keys/bob" "$work/bob-key/id" && cp "$work/bob/.aries.yaml" "$work/bob-key/.aries.yaml"
printf 'key: id\n' >>"$work/bob-key/.aries.yaml"
smoke_agent=$agent_sock smoke_key='' expect "key: in .aries.yaml wins over the agent's keys" '^principal +bob$' bob-key whoami
# The variable comes before key:, which names bob's key here.
smoke_agent=$agent_sock smoke_key="$work/keys/alice" expect "ARIES_KEY_FILE wins over key:" '^principal +alice$' bob-key whoami
# A key: that leads out of the checkout is not followed.
sed "s|^key: .*|key: $work/keys/alice|" "$work/bob-key/.aries.yaml" >"$work/bob-out/.aries.yaml"
smoke_agent=$agent_sock smoke_key='' refuse "key: outside the repository" 'outside the repository' bob-out whoami
# The .pub alone: the agent signs with that one key, and offers no other.
smoke_agent=$agent_sock smoke_key="$work/keys/alice.pub" expect "a .pub names one of the agent's keys" '^principal +alice$' alice whoami
grep -Eq -- '^key +SHA256:.*, signed by ssh-agent for ' <<<"$last" || fail "whoami does not say the agent signed" "$last"
passed=$((passed + 1))

echo "smoke: with no key configured, the default identities and then ssh-agent"
# A default identity file, in a home of its own, comes before the
# agent's keys: bob's file wins though the agent holds alice's key.
mkdir -p "$work/home-bob/.ssh" "$work/home-locked/.ssh"
chmod 700 "$work/home-bob/.ssh" "$work/home-locked/.ssh"
cp "$work/keys/bob" "$work/home-bob/.ssh/id_ed25519"
smoke_home="$work/home-bob" smoke_agent=$agent_sock smoke_key='' expect "a default identity comes before the agent's keys" '^principal +bob$' bob whoami
grep -Eq -- "^key +$bob_key_fp, from the file $work/home-bob/[.]ssh/id_ed25519, a default identity\$" <<<"$last" || fail "whoami does not say bob's key is a default identity" "$last"
passed=$((passed + 1))
# The agent holds seven keys, alice's last. aries offers five, one fewer
# than sshd tries, so the server answers instead of hanging up, and the
# refusal says what was left out and what to do.
smoke_agent=$agent_sock smoke_key='' refuse "no more keys are offered than sshd tries" 'accepted none of the 5 keys aries offered' alice whoami
grep -q '2 more keys that ssh-agent holds were not offered' <<<"$last" || fail "the refusal does not say how many keys were left out" "$last"
grep -q 'ARIES_KEY_FILE' <<<"$last" || fail "the refusal does not name ARIES_KEY_FILE" "$last"
absent "sshd did not hang up" 'too many refused keys'
passed=$((passed + 2))
# A default identity that needs a passphrase is skipped, and said to be.
ssh-keygen -q -t ed25519 -N 'smoke passphrase' -C locked@example.com -f "$work/home-locked/.ssh/id_ed25519"
smoke_home="$work/home-locked" smoke_key='' refuse "a passphrase-protected default identity is skipped" 'no SSH key available; skipped .*/id_ed25519: it is passphrase-protected' alice whoami
# The agent alone, holding only alice's key.
SSH_AUTH_SOCK=$agent_sock ssh-add -q -D 2>/dev/null || fail "could not empty the test's ssh-agent"
agent_add "$work/keys/alice"
smoke_agent=$agent_sock smoke_key='' expect "the agent's key, with nothing configured" '^principal +alice$' alice whoami
grep -Eq -- '^key +SHA256:.*, from ssh-agent; no key is configured' <<<"$last" || fail "whoami does not say the key came from ssh-agent" "$last"
passed=$((passed + 1))
# ARIES_KEY is never read: holding a private key, as an earlier CI
# recipe had it, it signs nobody in, and no part of it is printed.
old_value=$(cat "$work/keys/alice")
smoke_old=$old_value smoke_key='' refuse "ARIES_KEY is not read" 'ARIES_KEY is set, and aries does not read it' alice whoami
if grep -qF -- "$(sed -n 2p "$work/keys/alice")" <<<"$last"; then
  fail "the refusal repeats what ARIES_KEY holds"
fi
passed=$((passed + 1))
smoke_old=$old_value expect "ARIES_KEY beside ARIES_KEY_FILE changes nothing" '^principal +bob$' bob whoami
absent "a set ARIES_KEY is not mentioned when sign-in works" 'ARIES_KEY is set'

echo "smoke: driving aries"
expect "create" '^ar-[a-z0-9]+$' alice create "Fix the login redirect" -p 1 -t bug
a=$last
expect "create" '^ar-[a-z0-9]+$' alice create "Add a sign-out button" -p 2 -t feature
b=$last
expect "dep add" '' alice dep add "$b" "$a"
expect "blocked" "^$b .*blocked by $a" alice blocked
expect "ready" "^$a " alice ready
absent "ready leaves out the blocked issue" "^$b "
expect "start" "^$a .*in_progress" alice start "$a"
expect "comment" '^[a-z0-9]+$' alice comment "$a" "The redirect drops ?next="
expect "log hours" "^logged 1h on $a" alice log 1h "$a" --note pairing
expect "list hours" "alice +$a +1h " alice log
expect "remember" '^smoke-note \(project\) rev 1$' alice remember smoke-note "The redirect honors ?next=" --issue "$a"
expect "recall" '^smoke-note ' alice recall redirect
expect "forget" '^forgot smoke-note' alice forget smoke-note
expect "recall after forget" '^no memories$' alice recall --key smoke-note
expect "finish" '^created ar-[a-z0-9]+$' alice finish "$a" --reason "Fixed the redirect" \
  --handoff "The redirect now honors ?next=" --discovered "Add a test for ?next= redirects"
d=$(sed -n 's/^created //p' <<<"$last")
expect "ready after finish" "^$d " alice ready
expect "ready after finish" "^$b " alice ready
expect "blocked after finish" '' alice blocked
absent "blocked after finish" "^$b "
expect "show" "^$a .*closed" alice show "$a"
expect "show hours" '^logged 1h: alice 1h' alice show "$a"
expect "discovered from" "$d \(discovered-from\)" alice show "$a"

# Prices, tokens and plans. The invented request costs 100,000 input
# tokens at $4 and 10,000 output tokens at $20 a million: $0.60.
price=(example-large --from 2026-01-01 --input 4 --output 20 --cache-write 5 --cache-write-1h 8 --cache-read 0.4)
expect "prices set" ': added$' alice admin price set "${price[@]}"
expect "prices set again" ': unchanged$' alice admin price set "${price[@]}"
session=5f0c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f
transcript=$work/transcripts/$session.jsonl
printf '{"type":"assistant","sessionId":"%s","version":"2.1.300","timestamp":"%s","requestId":"req_smoke","uuid":"u1","message":{"id":"msg_smoke","type":"message","role":"assistant","model":"example-large","content":[],"stop_reason":"end_turn","usage":{"input_tokens":100000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10000}}}\n' \
  "$session" "$(date -u +%Y-%m-%dT%H:%M:%S.000Z)" >"$transcript"
hook=$(printf '{"session_id":"%s","transcript_path":"%s","hook_event_name":"SessionEnd"}' "$session" "$transcript")
expect "usage hook" '' alice usage --hook <<<"$hook"
[[ -z $last ]] || fail "usage hook: it printed a note" "$last"
expect "cost" '^total [$]0\.60, 110k tokens, 1h logged$' alice cost --since 1d
expect "cost by model" '^ +example-large +[$]0\.60' alice cost --since 1d --group-by model
expect "plans set" ': added$' alice admin plan set team --from "$(date -u +%Y-%m)" --fee 25 --seats 2 --principal alice --principal bob
expect "plans" '^team .* alice, bob ' alice admin plan list
# The plan's share accrues with the month, so its amount depends on the date.
expect "cost amortized" '^total [$]0\.60, [$][0-9]+\.[0-9]{2} amortized, 110k tokens' alice cost --since 1d
expect "digest" '^closed 1$' alice digest
expect "digest discovered" "^ +$d " alice digest

# A second principal reads, but is refused what only admins may do.
expect "ready as bob" "^$b " bob ready
refuse "prices set as bob" '"code":"forbidden"' bob --json admin price set example-large --from 2026-02-01 \
  --input 1 --output 1 --cache-write 1 --cache-write-1h 1 --cache-read 1
refuse "plans set as bob" '"code":"forbidden"' bob --json admin plan set team --from "$(date -u +%Y-%m)" \
  --fee 1 --seats 1 --principal bob

# Many projects, and keys from the control database. alice is the server
# admin; `ariesd control init` made bob a member of the first project.
echo "smoke: a second project, and a key the control database holds"
expect "project list" '^first +[0-9a-f-]{36} +ar +internal +active +public$' alice admin project list
expect "project add" '^project web \([0-9a-f-]{36}\) created in schema project_web: active$' alice admin project add web
web=$(sed -n 's/^project: //p' <<<"$last")
[[ $web =~ ^[0-9a-f-]{36}$ && $web != "$project" ]] || fail "project add printed no .aries.yaml for the new project" "$last"
for who in alice bob; do
  yaml_for "$web" >"$work/$who-web/.aries.yaml"
done
expect "create in the second project" '^web-[a-z0-9]+$' alice-web create "First in web" -p 2 -t task
w=$last
expect "the second project holds it" "^$w " alice-web ready
expect "the first project does not" '' alice ready
absent "the first project does not" "^$w "
# bob is a member of the first project only.
refuse "a non-member of the second project" 'bob is not a member of project' bob-web ready
# bob has a key in the control database, which would sign in to web too:
# it is shown, and he is added only once it is accepted (decision 36).
refuse "member add shows the keys first" '^aries: bob was not added to project web: it has 1 key' alice-web admin member add bob
grep -Eq -- '^SHA256:[A-Za-z0-9+/]{43} +ssh-ed25519 +bob +import +.* +yes$' <<<"$last" ||
  fail "member add did not list bob's key, who added it and that the operator did" "$last"
grep -q -- '--accept-keys' <<<"$last" || fail "the refusal does not name --accept-keys" "$last"
passed=$((passed + 2))
refuse "and bob is not a member yet" 'bob is not a member of project' bob-web ready
expect "member add --accept-keys" '^bob added to project web as member$' alice-web admin member add bob --accept-keys
expect "a member of the second project" "^$w " bob-web ready

# The keys file is written before the server listens, with the keys
# `control init` imported. Those are lines of authorized_keys too. sshd
# reads the keys file first, so alice's own connection came in by its
# line, which names her key: ariesd counts no connection of hers as let
# in by authorized_keys, which it reads, and marks her key as in both.
# With bob's key out of the store, sshd falls back to its line of
# authorized_keys, and his own connection is then counted as by hand.
check_keys_file "the keys file at the start"
in_keys_file "an imported key" '^restrict,command="/usr/local/bin/ariesd stdio --principal alice --key SHA256:[A-Za-z0-9+/]{43}" ssh-ed25519 [A-Za-z0-9+/=]+$'
in_keys_file "an imported key" 'stdio --principal bob --key SHA256:'
expect "a key in both files is marked" ' +import +.* +authorized_keys$' alice admin key list alice
absent "sshd took the line of the keys file, not of authorized_keys" 'came in by a line of the server.s hand-maintained authorized_keys file'
expect "bob lists his own key, in both files" ' +import +.* +authorized_keys$' bob admin key list bob
absent "sshd took the line of the keys file for bob too" 'came in by a line of the server.s hand-maintained authorized_keys file'
bob_fp=$(ssh-keygen -lf "$work/keys/bob.pub" | cut -d' ' -f2)
expect "removing a key that authorized_keys still holds warns" '^aries: warning: sshd will still accept key SHA256:.*: the connections it had signed in were closed, but it is also a line of the server.s authorized_keys file' \
  alice admin key remove "$bob_fp"
not_in_keys_file "the removed key" 'stdio --principal bob '
expect "and the key still signs in, by authorized_keys" "^$b " bob ready
expect "sshd then takes the line of authorized_keys" "^1 open connection of bob came in by a line of the server's hand-maintained authorized_keys file" bob admin key list bob
expect "key add, back in both files" ' added to bob$' alice admin key add bob "$work/keys/bob.pub"

# carol's key is in no authorized_keys file: sshd reads it from the keys
# file ariesd writes from the control database, with no restart.
ssh-keygen -q -t ed25519 -N '' -C carol@example.com -f "$work/keys/carol"
carol_fp=$(ssh-keygen -lf "$work/keys/carol.pub" | cut -d' ' -f2)
# A fingerprint is base64, whose + and / mean something in a pattern.
carol_re=$(sed 's/[+/.]/[&]/g' <<<"$carol_fp")
yaml_for "$project" >"$work/carol/.aries.yaml"
refuse "a key nobody added" 'accepted no key: aries offered SHA256:' carol ready
expect "principal add --key" "^key $carol_re added to carol\$" alice admin principal add carol --key "$work/keys/carol.pub"
if "${compose[@]}" exec -T aries grep -q carol /state/ssh/authorized_keys; then
  fail "carol has a line in authorized_keys; her key was to be in the control database alone"
fi
passed=$((passed + 1))
in_keys_file "a key added with aries admin" "stdio --principal carol --key $carol_re\" ssh-ed25519 "
check_keys_file "the keys file after a key was added"
expect "a key from the control database" "^$b " carol ready
expect "it signs in as its principal" '^[a-z0-9]+$' carol comment "$b" "carol was here"
expect "key list" "^$carol_re +ssh-ed25519 +carol@example.com +alice " alice admin key list carol
refuse "a private key file is not sent" 'holds a private key, which never leaves this machine' alice admin key add carol "$work/keys/bob"
refuse "nobody adds a key to their own account" 'nobody adds a key to their own account' alice admin key add alice "$work/keys/carol.pub"
# Anyone may remove a key of their own (decision 38), but not the one
# the connection signed in with: sshd took carol's line from the keys
# file, so ariesd knows which key that is.
refuse "the key in use is not removed by its own connection" "^aries: key $carol_re is the key this connection signed in with" carol admin key remove "$carol_fp"
expect "key remove" "^key $carol_re removed from carol\$" alice admin key remove "$carol_fp"
absent "removing a key only the store holds warns of nothing" 'warning'
not_in_keys_file "a removed key" 'stdio --principal carol '
refuse "a removed key" 'accepted no key' carol ready
expect "key add" "^key $carol_re .* added to carol\$" alice admin key add carol "$work/keys/carol.pub"
expect "the key added again" "^$b " carol ready
expect "principal disable" '^carol: disabled$' alice admin principal disable carol
not_in_keys_file "a disabled principal's key" 'stdio --principal carol '
refuse "a disabled principal's key" 'accepted no key' carol ready
if [[ -n $ssh ]]; then
  expect "status shows the transports" "^transports: https on [^ ]*:8443 for https://localhost:$https_port \(files, certificate ends [0-9-]+\), [0-9]+ connections?; ssh on, [0-9]+ connections?$" alice admin status
  expect "status shows the keys file" "^keys file $keys_file: 2 keys, written " alice admin status
else
  expect "status shows the transports" "^transports: https on [^ ]*:8443 for https://localhost:$https_port \(files, certificate ends [0-9-]+\), [0-9]+ connections?; ssh off$" alice admin status
  absent "status shows no keys file with SSH off" '^keys file '
fi
# A CI job reports a check, as the workflows of docs/ci.md run it: its
# own principal, CI=true and the branch in GITHUB_HEAD_REF, with no
# session named, so aries takes the session ci.
echo "smoke: a CI job reports a check on the issue its branch names"
mkdir -p "$work/ci-example"
ssh-keygen -q -t ed25519 -N '' -C ci-example@example.com -f "$work/keys/ci-example"
yaml_for "$project" >"$work/ci-example/.aries.yaml"
expect "a principal for CI" ' added to ci-example$' alice admin principal add ci-example --key "$work/keys/ci-example.pub"
ci_subject=$(aries_as alice create "Parse the config" 2>/dev/null) || fail "aries create of the CI subject exited $?"
ci_next=$(aries_as alice create "Use the parsed config" 2>/dev/null) || fail "aries create of the dependent exited $?"
expect "a ci gate" '^[a-z0-9]+-[a-z0-9]+$' alice gate add --kind ci --check build --issue "$ci_subject" --approver ci-example --blocks "$ci_next"
ci_branch="feature/$ci_subject-parse-the-config"
CI=true GITHUB_HEAD_REF=$ci_branch smoke_session='' expect "a CI job's failure" ' failed$' \
  ci-example gate fail --check build --from-branch --optional --sha 0123abc --url https://ci.example.com/runs/1 --reason "tests failed"
CI=true GITHUB_HEAD_REF=$ci_branch refuse "a report from another session" "an agent's session" ci-example gate pass --check build --from-branch
CI=true GITHUB_HEAD_REF=$ci_branch smoke_session='' refuse "a principal the gate does not name" 'only ci-example may pass' \
  bob gate pass --check build --from-branch
CI=true GITHUB_HEAD_REF=$ci_branch smoke_session='' expect "a CI job's pass" ' passed$' \
  ci-example gate pass --check build --from-branch --optional --sha 4567def --url https://ci.example.com/runs/2
expect "the dependent is ready" "^$ci_next " alice ready
expect "the subject shows its check" '^check build: passed at 4567def' alice show "$ci_subject"
CI=true GITHUB_HEAD_REF=dependabot/go_modules/lib-1.2.3 smoke_session='' expect "a branch that names no issue" 'nothing reported' \
  ci-example gate pass --check build --from-branch --optional

# Locks (docs/cli.md, Locks): a run holds the lock around its command,
# with the epoch in its environment, passes the command's exit code on
# and releases the lock; a held lock refuses a second acquire, naming its
# holder; the list shows it; a release frees it.
echo "smoke: a lock held around a command"
# shellcheck disable=SC2016 # the command's own shell expands the epoch
expect "lock run" '^ran under epoch [0-9]+$' alice lock run deploy/smoke -- sh -c 'echo "ran under epoch $ARIES_LOCK_EPOCH"'
lock_code=0
aries_as alice lock run deploy/smoke -- sh -c 'exit 7' >/dev/null 2>&1 || lock_code=$?
((lock_code == 7)) || fail "lock run: the command exited 7, and aries exited $lock_code"
passed=$((passed + 1))
lock_epoch=$(aries_as alice lock acquire deploy/smoke 2>/dev/null) || fail "lock acquire exited $?"
[[ $lock_epoch =~ ^[0-9]+$ ]] || fail "lock acquire printed '$lock_epoch', want the epoch"
refuse "a held lock" 'held by alice' bob lock acquire deploy/smoke
expect "lock list" '^deploy/smoke +1/1 +alice/smoke-alice epoch ' bob lock list
expect "lock release" "^released deploy/smoke epoch $lock_epoch$" alice lock release deploy/smoke "$lock_epoch"
expect "no lock held" '^no locks held$' bob lock list

no_failed_write "before the restart"

# Postgres's own figures (docs/server.md, Watching a server), read with
# ariesd's role: the server's version and connections, ariesd's pool,
# the database's transactions and size, and each project's schema's
# size. They are the server's, so a project admin is shown none of them.
echo "smoke: Postgres's figures in the status"
expect "status shows Postgres's figures" "^postgres 18[.][0-9]+[^:]*: connections [0-9]+ of [0-9]+, [0-9]+ in this database; ariesd's pool [0-9]+ in use of [0-9]+ held, at most 32, waited for [0-9]+ times?$" alice admin status
grep -Eq "^postgres's database [0-9.]+ [KMG]iB; since its statistics were reset, [0-9]+ commits, [0-9]+ rollbacks?, 0 deadlocks, 0 conflicts, [0-9.]+ [KMG]iB in temporary files$" <<<"$last" ||
  fail "status does not show the database's transactions and size" "$last"
grep -Eq '^web +active +open +[0-9.]+ [KM]iB +[0-9]+ ' <<<"$last" || fail "status does not show the size of web's schema" "$last"
passed=$((passed + 2))
expect "status --json carries them" '"postgres":\{"version":"18[.][0-9]+[^"]*","conns":[0-9]+,"conns_max":[0-9]+,"database_conns":[0-9]+,"pool_conns":[0-9]+' alice --json admin status
grep -Eq '"schema_bytes":[0-9]+' <<<"$last" || fail "status --json gives no project's schema_bytes" "$last"
passed=$((passed + 1))
# bob is no server admin: Postgres is the server's, and he is shown none of it.
refuse "Postgres's figures are for server admins" 'may not see the server.s status' bob admin status
# Postgres answers on the compose network alone: the server's container
# reaches it, and no port of this machine does.
in_server bash -c "exec 3<>/dev/tcp/postgres/5432" 2>/dev/null || fail "the server's container does not reach Postgres at postgres:5432"
last=$(docker port "$("${compose[@]}" ps -q postgres)" 2>&1) || fail "could not list the ports of Postgres's container" "$last"
[[ -z $last ]] || fail "Postgres's container publishes a port on this machine" "$last"
passed=$((passed + 1))
# The config file holds no password, and the password file the DSN
# names is the aries user's alone.
last=$(in_server stat -L -c '%a %U:%G' /etc/aries/postgres/pgpass 2>&1) || fail "no password file at /etc/aries/postgres/pgpass" "$last"
[[ $last == "600 aries:aries" ]] || fail "the password file is '$last', want 600 aries:aries"
last=$(in_server sed -n 's/^dsn: //p' /etc/aries/ariesd.yaml)
[[ $last == postgres://aries@postgres:5432/aries?*passfile=/etc/aries/postgres/pgpass* && $last != *password=* && $last != *:*:*@* ]] ||
  fail "the config's dsn is not the password-free URL that names the password file"
passed=$((passed + 2))

# What a restart must not undo. The container runs `ariesd control
# init` at every start, and bob's line is still in ARIES_KEYS.
expect "member remove before the restart" '^bob removed from project first$' alice admin member remove bob
refuse "a removed member" 'bob is not a member of project' bob ready

echo "smoke: restarting the server"
restart_server
[[ $(server_setting project) == "$project" ]] || fail "restart: the project changed" "$(server_setting project)"
[[ $(server_setting host_key) == "$host_key" ]] || fail "restart: the host key changed" "$(server_setting host_key)"
in_server cat /etc/aries/tls/ca.pem | cmp -s - "$work/ca.pem" || fail "restart: the test CA changed"
no_ssh_path "after the restart"
passed=$((passed + 2))
expect "show after restart" "^$a .*closed" alice show "$a"
expect "admins after restart" ': unchanged$' alice admin price set "${price[@]}"
expect "the second project after restart" "^$w " bob-web ready
check_keys_file "the keys file after the restart"
expect "principal enable after restart" '^carol: active$' alice admin principal enable carol
in_keys_file "an enabled principal's key" "stdio --principal carol --key $carol_re\" "
expect "the control database's key after restart" "^$b " carol ready
no_failed_write "after the restart"

# The start's `control init` left bob as the admin left him, and said so.
refuse "a restart does not put a removed member back" 'bob is not a member of project' bob ready
# The log is read whole first: under pipefail, a grep that stops at its
# first match would fail the pipeline that feeds it.
last=$("${compose[@]}" logs --no-log-prefix aries 2>&1)
grep -Eq 'authorized_keys line [0-9]+: bob was removed from the project by alice on .*; kept removed' <<<"$last" ||
  fail "the start did not report bob's membership as kept removed" "$(grep -E 'authorized_keys|left as admins' <<<"$last" | tail -n 10)"
passed=$((passed + 1))
# The operator puts him back on purpose, as docs/deploy/docker.md says
# to; the running server reads the control database again within half a
# minute.
last=$("${compose[@]}" exec -T -u aries -w /var/lib/aries aries ariesd control init --name first --restore-removed-access 2>&1) ||
  fail "control init --restore-removed-access failed" "$last"
grep -Eq '^restored from this file, over what admins had set: 0 keys, 1 membership, 0 principals enabled$' <<<"$last" ||
  fail "control init --restore-removed-access did not say it restored bob's membership" "$last"
passed=$((passed + 1))
for ((i = 0; i < 90; i++)); do
  if aries_as bob ready >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
expect "the restored member" "^$b " bob ready

# A key of authorized_keys keeps the principal its line names. sshd reads
# the keys file first, so a break-glass key that the control database
# took for someone else would sign its owner in as that someone. dave's
# key is a second key of alice, the server admin, added by hand on the
# server and never imported.
echo "smoke: a key of authorized_keys keeps its principal"
mkdir -p "$work/dave"
ssh-keygen -q -t ed25519 -N '' -C dave@example.com -f "$work/keys/dave"
yaml_for "$project" >"$work/dave/.aries.yaml"
by_hand() {
  printf 'restrict,command="/usr/local/bin/ariesd stdio --principal %s" %s\n' "$1" "$(cat "$2")" |
    in_server sh -c 'cat >> /state/ssh/authorized_keys'
}
by_hand alice "$work/keys/dave.pub"
# Listing the principals is for server admins: whoever can, is alice.
expect "a key added by hand signs in as the admin its line names" '^alice +active' dave admin principal list
refuse "it cannot be given to another principal" 'is a line of the server.s authorized_keys file, for another principal' \
  alice admin key add bob "$work/keys/dave.pub"
refuse "nor to a new principal" 'is a line of the server.s authorized_keys file, for another principal' \
  alice admin principal add mallory --key "$work/keys/dave.pub"
dave_re=$(ssh-keygen -lf "$work/keys/dave.pub" | cut -d' ' -f2 | sed 's/[+/.]/[&]/g')
not_in_keys_file "the refused key" "--key $dave_re\""
expect "and it still signs its owner in as themselves" '^alice +active' dave admin principal list

# The other order: the control database holds carol's key, for carol, and
# then the operator gives the same key to alice by hand. ariesd holds
# carol's line back from its file, so sshd reaches the operator's; over
# HTTPS ariesd takes the operator's line itself, as sshd would.
refuse "carol's key signs in as carol, who is no server admin" 'for server admins' carol admin principal list
by_hand alice "$work/keys/carol.pub"
if [[ -n $ssh ]]; then
  for _ in $(seq 1 45); do
    in_server grep -Eq 'stdio --principal carol ' "$keys_file" || break
    sleep 1
  done
fi
not_in_keys_file "a key the operator's file gives to another principal" 'stdio --principal carol '
expect "that key signs in as the principal the operator's line names" '^alice +active' carol admin principal list
if [[ -n $ssh ]]; then
  expect "status counts the key held back" '^keys held back from it: 1,' alice admin status
fi
no_failed_write "at the end"

# A key that ends (docs/server.md, Keys that end). The server runs at
# UTC+14: sshd reads expiry-time digits without a Z in its own zone,
# which would end the key fourteen hours early there, so its signing in
# over SSH before its end shows that ariesd writes the Z. At the end
# ariesd takes the key's line out, closes the connection it signed in,
# and refuses it, saying when it ended.
echo "smoke: a key that ends"
last=$(in_server env TZ="$tz" date +%z)
[[ $last == +1400 ]] || fail "the zone $tz is $last, want +1400"
# The daemon runs in it: its environment is read as its own user, since
# the container's root may not read another's.
# shellcheck disable=SC2016 # the server's shell expands it
daemon=$(in_server sh -c 'for p in $(pidof ariesd); do tr "\0" " " </proc/$p/cmdline | grep -q "^ariesd serve" && echo $p; done') || true
last=$("${compose[@]}" exec -T -u aries aries sh -c "tr '\\0' '\\n' </proc/$daemon/environ" 2>&1) || true
grep -qx "TZ=$tz" <<<"$last" || fail "ariesd serve (process $daemon) does not run in the zone $tz" "$last"
passed=$((passed + 1))
# So does sshd, whose title hides its environment: digits without the Z,
# five minutes ahead in UTC, are fourteen hours past at UTC+14, and it
# refuses the key of a line that has them.
if [[ -n $ssh ]]; then
  mkdir -p "$work/probe"
  ssh-keygen -q -t ed25519 -N '' -C probe@example.com -f "$work/keys/probe"
  yaml_for "$project" >"$work/probe/.aries.yaml"
  ahead=$(date -u -v+5M +%Y%m%d%H%M%S 2>/dev/null || date -u -d '+5 minutes' +%Y%m%d%H%M%S)
  printf 'restrict,expiry-time="%s",command="/usr/local/bin/ariesd stdio --principal alice" %s\n' "$ahead" "$(cat "$work/keys/probe.pub")" |
    in_server sh -c 'cat >> /state/ssh/authorized_keys'
  refuse "sshd reads digits without the Z in its own zone" 'accepted no key' probe whoami
fi
mkdir -p "$work/erin"
ssh-keygen -q -t ed25519 -N '' -C erin@example.com -f "$work/keys/erin"
yaml_for "$project" >"$work/erin/.aries.yaml"
erin_fp=$(ssh-keygen -lf "$work/keys/erin.pub" | cut -d' ' -f2)
erin_re=$(sed 's/[+/.]/[&]/g' <<<"$erin_fp")
expect "a principal for the key" '^principal erin added' alice admin principal add erin
expect "key add --expires" "^key $erin_re \(ssh-ed25519, erin@example.com\) added to erin, ending [0-9]{4}-[0-9]{2}-[0-9]{2} [0-9:]{8} UTC\$" \
  alice admin key add erin "$work/keys/erin.pub" --expires 60s
erin_end=$(sed -n 's/.*, ending \(.*\) UTC$/\1/p' <<<"$last")
in_keys_file "the key's end, in UTC with its Z" \
  "^restrict,expiry-time=\"$(tr -d ' :-' <<<"$erin_end")Z\",command=\"/usr/local/bin/ariesd stdio --principal erin --key $erin_re\" ssh-ed25519 "
expect "the list gives its end" "^$erin_re .* $erin_end UTC\$" alice admin key list erin
expect "the key signs in before its end" '^principal +erin$' erin whoami
aries_as erin watch >"$work/erin-watch.out" 2>&1 &
key_watch=$!
for ((i = 0; i < 60; i++)); do
  grep -q 'watching your inbox' "$work/erin-watch.out" && break
  sleep 0.5
done
grep -q 'watching your inbox' "$work/erin-watch.out" || fail "erin's watch did not start" "$(cat "$work/erin-watch.out")"
for ((i = 0; i < 120; i++)); do
  last=$("${compose[@]}" logs --no-log-prefix aries 2>&1)
  grep -q " event=key.expired fingerprint=$erin_fp " <<<"$last" && break
  sleep 1
done
grep -Eq " event=key[.]expired fingerprint=$erin_re subject=erin at=[^ ]+ connections=1\$" <<<"$last" ||
  fail "ariesd did not log the key's end, with the one connection it closed" "$(grep -E 'key[.]expired|conn[.]dropped' <<<"$last" | tail -n 5)"
grep -Eq ' event=conn[.]dropped reason="key expired" by=ariesd connections=1 ' <<<"$last" ||
  fail "ariesd did not log the connection it closed at the key's end" "$(grep -E 'conn[.]dropped' <<<"$last" | tail -n 5)"
passed=$((passed + 2))
for ((i = 0; i < 30; i++)); do
  kill -0 "$key_watch" 2>/dev/null || break
  sleep 0.5
done
watch_code=0
wait "$key_watch" || watch_code=$?
key_watch=
((watch_code == 1)) || fail "erin's watch exited $watch_code at the key's end, want 1: the server closes the connection" "$(cat "$work/erin-watch.out")"
passed=$((passed + 1))
not_in_keys_file "an ended key" 'stdio --principal erin '
if [[ -n $ssh ]]; then
  refuse "an ended key, at sshd" 'accepted no key' erin whoami
else
  refuse "an ended key, in the proof over HTTPS" "the key that signed in ended at $(tr ' ' T <<<"$erin_end")Z" erin whoami
fi
expect "an ended key is listed until it is pruned" "^$erin_re .* $erin_end UTC\$" alice admin key list erin

# ariesd's log follows its schema (docs/server.md, Logs): every line it
# wrote names an event, and an access change made with `aries admin` is a
# line of the audit stream, read back from the control database's log.
# Only ariesd writes lines that start with time= and have level=;
# sshd's are its own.
echo "smoke: the log's schema and the audit stream"
last=$("${compose[@]}" logs --no-log-prefix aries 2>&1 | grep -E '^time=[^ ]+ level=[A-Z]+ msg=' || true)
[[ -n $last ]] || fail "ariesd logged no line"
if unnamed=$(grep -Ev ' event=[a-z_]+([.][a-z_]+)+( |$)' <<<"$last"); then
  fail "a line of ariesd's log names no event" "$(head -n 5 <<<"$unnamed")"
fi
passed=$((passed + 1))
grep -Eq ' event=audit[.]key[.]add audit=true seq=[0-9]+ at=[^ ]+ principal=alice session=[^ ]+ op=key[.]add target=SHA256:[^ ]+ fingerprint=SHA256:[^ ]+ subject=carol$' <<<"$last" ||
  fail "the audit stream has no line for the key alice added to carol" "$(grep -F ' audit=true ' <<<"$last" | tail -n 10)"
passed=$((passed + 1))
if grep -Eq 'ssh-ed25519 AAAA' <<<"$last"; then
  fail "ariesd's log holds a key's text" "$(grep -E 'ssh-ed25519 AAAA' <<<"$last" | cut -c1-200 | head -n 3)"
fi
passed=$((passed + 1))

if [[ -n $telemetry ]]; then
  telemetry_checks
  no_failed_write "after the telemetry checks"
fi

if [[ -n $identity ]]; then
  identity_checks
  signin_checks
fi

echo "smoke: ok, $passed checks passed"
