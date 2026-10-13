#!/usr/bin/env bash
# Smoke test of the Docker test server (docs/deploy/docker.md). It builds
# the image and sfx from this checkout, starts a throwaway server with two
# principals, drives sfx through the main surfaces end to end, then
# through a server of many projects (a second project, and a person
# whose key only the control database holds, which sshd reads from the
# keys file starfixd writes), restarts the server to check that its state
# persists, then removes the server and its volume. It exits non-zero on
# the first failure.
#
#   deploy/docker/smoke.sh [DOCKER BUILD FLAGS...]
#
# Flags go to `docker build`, for example --secret id=ca,src=FILE behind a
# TLS-intercepting proxy. SFX names an sfx binary to use instead of
# building one. STARFIX_PORT is the port on 127.0.0.1, 2223 by default, so
# a dev server on 2222 is left alone. STARFIX_IMAGE names an image to test
# instead of building starfix-dev:local: a variant of it, built FROM it,
# such as those docs/deploy/docker.md describes. STARFIX_SMOKE_LOG names
# a file to save the server's log to (sshd's, starfixd's and Dolt's)
# before the server is removed. STARFIX_SMOKE_PROJECT names the Compose
# project, starfix-smoke by default, so that two runs on one machine,
# each with its own STARFIX_PORT, do not remove each other's server.
#
# STARFIX_SMOKE_TELEMETRY=1 runs all of it with starfixd's telemetry on
# and the compose file's `telemetry` and `prometheus` profiles, and then
# checks the harness of deploy/observability (telemetry_checks, below):
# what reaches the collector, the alert rules in a real Prometheus,
# /readyz, and that a collector that is down or hangs changes nothing.
# It pulls two more images.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
port=${STARFIX_PORT:-2223}
work=$(mktemp -d)
compose=(docker compose -p "${STARFIX_SMOKE_PROJECT:-starfix-smoke}" -f "$root/deploy/docker/compose.yaml")
telemetry=
sidecars=()
case ${STARFIX_SMOKE_TELEMETRY:-} in
'') ;;
1)
  telemetry=1
  sidecars=(collector prometheus)
  compose+=(--profile telemetry --profile prometheus)
  ;;
*)
  printf 'smoke: STARFIX_SMOKE_TELEMETRY is %s\nfix: set it to 1, or leave it unset\n' "$STARFIX_SMOKE_TELEMETRY" >&2
  exit 2
  ;;
esac
passed=0
restart_took=0
last=

# The test's own ssh-agent (start_agent), never the caller's: its socket,
# on a short path, since a socket's is limited to about 100 bytes, and
# its pid.
agent_sock=/tmp/sfx-smoke-$$.sock
agent_pid=

cleanup() {
  if [[ -n ${STARFIX_SMOKE_LOG:-} ]]; then
    "${compose[@]}" logs --no-log-prefix starfix >"$STARFIX_SMOKE_LOG" 2>&1 || true
  fi
  "${compose[@]}" down -v >/dev/null 2>&1 || true
  if [[ -n $agent_pid ]]; then
    kill "$agent_pid" 2>/dev/null || true
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

# sfx_as WHO ARGS... runs sfx in WHO's scratch repository, with its own
# session. WHO's key is named with STARFIX_KEY_FILE, so it is the only
# key sfx offers (design, decision 35). Nothing of the caller's can sign
# in: the home is a directory of the test's own, with no .ssh, and the
# caller's ssh-agent socket is never passed on. Four variables change
# that for the checks of the rule itself: smoke_key names another key
# file, or none when it is empty; smoke_agent is an ssh-agent socket of
# the test's own; smoke_home is another home directory; and smoke_old is
# a value for STARFIX_KEY, the variable sfx never reads.
sfx_as() {
  local who=$1
  shift
  local key=${smoke_key-$work/keys/${who%-web}}
  (cd "$work/$who" && HOME="${smoke_home-$work/home}" XDG_CACHE_HOME="$work/home/.cache" SSH_AUTH_SOCK="${smoke_agent-}" \
    STARFIX_KEY_FILE="$key" STARFIX_KEY="${smoke_old-}" STARFIX_SESSION="smoke-$who" "$sfx" "$@")
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

# expect NAME PATTERN WHO ARGS... runs sfx as WHO, and fails unless it
# exits 0 and prints a line matching the extended regexp PATTERN.
expect() {
  local name=$1 pattern=$2
  shift 2
  last=$(sfx_as "$@" 2>&1) || fail "$name: sfx ${*:2} exited $?" "$last"
  grep -Eq -- "$pattern" <<<"$last" || fail "$name: sfx ${*:2} printed nothing matching /$pattern/" "$last"
  passed=$((passed + 1))
}

# refuse NAME PATTERN WHO ARGS... is expect for a command that must fail.
refuse() {
  local name=$1 pattern=$2
  shift 2
  if last=$(sfx_as "$@" 2>&1); then
    fail "$name: sfx ${*:2} succeeded, want a refusal" "$last"
  fi
  grep -Eq -- "$pattern" <<<"$last" || fail "$name: sfx ${*:2} printed nothing matching /$pattern/" "$last"
  passed=$((passed + 1))
}

# absent NAME PATTERN fails if the last output has a line matching PATTERN.
absent() {
  if grep -Eq -- "$2" <<<"$last"; then
    fail "$1: output matches /$2/" "$last"
  fi
  passed=$((passed + 1))
}

# server_setting KEY prints KEY's value from the newest .starfix.yaml the
# server printed.
server_setting() {
  "${compose[@]}" logs --no-log-prefix starfix 2>/dev/null | sed -n "s/^ *$1: //p" | tail -n 1
}

# in_server CMD... runs CMD in the server's container, as root.
in_server() { "${compose[@]}" exec -T starfix "$@"; }

# keys_file checks the file starfixd writes for sshd: there, a regular
# file of mode 0600 that starfix owns, starting with the comment that
# says who writes it.
keys_file=/var/lib/starfix/.ssh/starfix_keys
check_keys_file() {
  local got
  got=$(in_server stat -L -c '%F %a %U:%G' "$keys_file" 2>&1) || fail "$1: no keys file at $keys_file" "$got"
  [[ $got == "regular file 600 starfix:starfix" ]] || fail "$1: the keys file is '$got', want a regular file, 600, starfix:starfix"
  got=$(in_server head -n 1 "$keys_file")
  [[ $got == "# Generated by starfixd"* ]] || fail "$1: the keys file does not start with starfixd's comment" "$got"
  passed=$((passed + 1))
}

# in_keys_file NAME PATTERN / not_in_keys_file NAME PATTERN check the
# keys file for a line matching the extended regexp PATTERN.
in_keys_file() {
  in_server grep -Eq -- "$2" "$keys_file" || fail "$1: the keys file has no line matching /$2/" "$(in_server cut -c1-120 "$keys_file")"
  passed=$((passed + 1))
}

not_in_keys_file() {
  if in_server grep -Eq -- "$2" "$keys_file"; then
    fail "$1: the keys file has a line matching /$2/" "$(in_server cut -c1-120 "$keys_file")"
  fi
  passed=$((passed + 1))
}

# no_failed_write fails if starfixd logged that it could not write the
# keys file, or never logged writing it.
no_failed_write() {
  local log
  log=$("${compose[@]}" logs --no-log-prefix starfix 2>&1)
  if grep -q 'keys file is out of date' <<<"$log"; then
    fail "$1: starfixd logged a failed write of the keys file" "$(grep 'keys file' <<<"$log")"
  fi
  grep -q 'msg="keys file written"' <<<"$log" || fail "$1: starfixd never logged writing the keys file" "$(tail -n 20 <<<"$log")"
  passed=$((passed + 1))
}

up() {
  STARFIX_IMAGE=$image STARFIX_KEYS=$keys STARFIX_ADMINS=alice STARFIX_PORT=$port STARFIX_TELEMETRY=$telemetry \
    "${compose[@]}" up -d --no-build --wait --wait-timeout 120 >/dev/null 2>&1 ||
    fail "the server did not become healthy" "$("${compose[@]}" logs --no-log-prefix 2>&1 | tail -n 40)"
}

# restart_server restarts the server's container and waits for it. The
# collector and Prometheus live in its network namespace, which a
# restart replaces, so they are restarted after it: at once, so that
# the collector listens again before starfixd, which starts after Dolt,
# has anything to send.
restart_server() {
  local t0=$SECONDS
  "${compose[@]}" restart starfix >/dev/null 2>&1
  restart_took=$((SECONDS - t0))
  if ((${#sidecars[@]})); then
    "${compose[@]}" restart "${sidecars[@]}" >/dev/null 2>&1 || fail "could not restart ${sidecars[*]} after the server"
  fi
  up
}

# What follows, to telemetry_checks, runs only with
# STARFIX_SMOKE_TELEMETRY=1.

# http_get PORT PATH prints what the server answers on loopback inside
# its container, headers and all. The image has no curl, so bash asks.
http_get() {
  in_server bash -c "exec 3<>/dev/tcp/127.0.0.1/$1 && printf 'GET %s HTTP/1.0\r\n\r\n' '$2' >&3 && cat <&3" 2>&1
}

# collector_log prints all the collector has printed: every metric and
# span it received, in its debug exporter's text.
collector_log() { "${compose[@]}" logs --no-log-prefix collector 2>&1; }

# starfixd_log prints the server's log since starfixd last started.
starfixd_log() {
  "${compose[@]}" logs --no-log-prefix starfix 2>&1 | awk '/msg="telemetry on"/ { n = 0 } { line[n++] = $0 } END { for (i = 0; i < n; i++) print line[i] }'
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
# attributes. The record's body, which is a fixed message of starfixd's,
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
# records, starfixd has counted as dropped since it started.
dropped() {
  http_get 9464 /metrics | awk -v signal="signal=\"$1\"" '/^starfix_telemetry_dropped_total/ && index($0, signal) { n = $NF } END { print n + 0 }'
}

# shellcheck disable=SC2317,SC2329
dropped_more() {
  local now
  now=$(dropped "$1")
  last="starfix_telemetry_dropped_total{signal=\"$1\"} is $now, and was $2 before"
  ((now > $2))
}

# logged PATTERN prints how many lines of starfixd's log, since it last
# started, match PATTERN.
logged() { starfixd_log | grep -c -- "$1" || true; }

# shellcheck disable=SC2317,SC2329
logged_once_more() {
  local now
  now=$(logged "$1")
  last="starfixd logged /$1/ $now times since it started, and $2 times before"
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
    sfx_as alice show "$a" || fail "batch $1: sfx show exited $?"
    sfx_as alice ready || fail "batch $1: sfx ready exited $?"
    sfx_as alice comment "$b" "telemetry batch $1 $i" >/dev/null || fail "batch $1: sfx comment exited $?"
  done
}

# new_traceparent sets trace_id and parent_id to random ids, and
# traceparent to the W3C header that carries them, sampled.
new_traceparent() {
  trace_id=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
  parent_id=$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')
  traceparent=00-$trace_id-$parent_id-01
}

# The series the harness's rules and dashboard read from Dolt, by the
# names its endpoint serves; a test in internal/telemetry holds its own
# list of them to this one.
dolt_series=(dss_concurrent_connections dss_query_duration process_resident_memory_bytes process_cpu_seconds_total go_gc_duration_seconds go_gc_gomemlimit_bytes go_goroutines sys_cpu_usage sys_mem_usage sys_disk_usage)

# telemetry_checks checks the harness of deploy/observability against
# the server the checks above have used, which has run since the
# restart with its telemetry on.
telemetry_checks() {
  local pushed=(starfix.request.duration starfix.project.requests starfix.connections starfix.handshakes
    starfix.store.write.duration starfix.store.commit.duration starfix.stores.open starfix.store.open.duration
    starfix.claims starfix.control.writes starfix.keys_file.writes starfix.keys_file.last_write.time
    starfix.reaper.duration starfix.reaper.last_run.time starfix.clock.behind)
  local scraped=(starfix_request_duration_seconds starfix_project_requests_total starfix_connections starfix_handshakes_total
    starfix_store_write_duration_seconds starfix_store_commit_duration_seconds starfix_stores_open starfix_store_open_duration_seconds
    starfix_claims_total starfix_control_writes_total starfix_keys_file_writes_total starfix_keys_file_last_write_time_seconds
    starfix_reaper_duration_seconds starfix_reaper_last_run_time_seconds starfix_clock_behind_seconds)
  local name='     -> Name: ' traced server_span attempt_span stand_in rules log all bad job t0 alive_took alive dead
  local metrics0 traces0 logs0 warned_metrics warned_traces warned_logs recovered queries query refused_took frozen_took frozen_restart
  local req seq subject

  echo "smoke: telemetry: a trace from the test's own TRACEPARENT"
  expect "status says people are named by stand-ins" '^traces: people are named by stand-ins' alice admin status
  new_traceparent
  TRACEPARENT=$traceparent expect "a request with a trace context" '^sf-[a-z0-9]+$' alice create "Traced by the smoke test" -p 3 -t task
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
  refuse "nobody else does" '"c":"forbidden"' bob --json admin principal whois "$stand_in"
  log=$(collector_log)
  all=$(spans <<<"$log")
  grep -Eq ' principal=p_[0-9a-f]{16} session=s_[0-9a-f]{16} ' <<<"$all" || fail "no span says who, by a stand-in"
  bad=$(grep -Eo ' (principal|session)=[^ ]*' <<<"$all" | grep -Ev '^ (principal=p_|session=s_)[0-9a-f]{16}$' || true)
  [[ -z $bad ]] || fail "a span names a principal or a session by something other than a stand-in" "$(sort -u <<<"$bad" | head -n 5)"
  passed=$((passed + 1))

  # starfixd's log, exported (docs/server.md, Logs over OTLP). A
  # record carries the ids of the span its line was logged under: a
  # request the test traces and the server refuses has a line at info,
  # so a record, in the test's trace and with the id of the server's
  # own span for the request.
  echo "smoke: telemetry: log records, with their trace, and the audit stream"
  new_traceparent
  TRACEPARENT=$traceparent refuse "a traced request that is refused" 'not found' alice show sf-nosuchid
  await 30 "the refused request's span arrives" span_arrived "^span trace=$trace_id parent=$parent_id id=[0-9a-f]{16} name=show op=show result=not_found "
  server_span=$(sed -n 's/.* id=\([0-9a-f]*\) name=show .*/\1/p' <<<"$last")
  await 30 "its log record arrives, in the test's trace and under the server's span" record_arrived \
    "^record event=request.refused trace=$trace_id span=$server_span .* principal=$stand_in session=s_[0-9a-f]{16} .* op=show .* code=not_found req=[0-9a-f]{16} "
  req=$(sed -n 's/.* req=\([0-9a-f]*\) .*/\1/p' <<<"$last")
  # The journal has the same line, found by the request's id, with the
  # name the record leaves out.
  last=$("${compose[@]}" logs --no-log-prefix starfix 2>&1 | grep -F " req=$req " || true)
  grep -Eq " principal=alice .* event=request.refused op=show .* code=not_found req=$req trace_id=$trace_id span_id=$server_span\$" <<<"$last" ||
    fail "starfixd's own log has no line for the refused request, by its id $req, naming alice and the trace" "$last"
  passed=$((passed + 1))
  # An access change is a record of the audit stream at the collector:
  # who made it and whom it changed are stand-ins, which a server admin
  # resolves, and the line in the server's log, by the event's number,
  # has the names.
  expect "an access change" '^principal erin added, a member of project first$' alice admin principal add erin
  last=$("${compose[@]}" logs --no-log-prefix starfix 2>&1 | grep -E ' event=audit[.]principal[.]add audit=true seq=[0-9]+ at=[^ ]+ principal=alice session=[^ ]+ op=principal[.]add target=erin subject=erin$' || true)
  seq=$(sed -n 's/.* seq=\([0-9]*\) .*/\1/p' <<<"$last")
  [[ $seq =~ ^[0-9]+$ ]] || fail "starfixd's own log has no one audit line for the principal alice added, naming alice and erin" "$last"
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

  echo "smoke: telemetry: metrics from starfixd, pushed and scraped, and from Dolt"
  await 90 "starfixd's metrics arrive over OTLP" arrived "$name" "${pushed[@]}"
  await 60 "starfixd's metrics arrive by a scrape of /metrics" arrived "$name" "${scraped[@]}"
  await 60 "Dolt's metrics arrive by a scrape" arrived "$name" "${dolt_series[@]}"
  log=$(collector_log)
  for job in starfixd dolt; do
    grep -Fq -- "     -> service.name: Str($job)" <<<"$log" || fail "the collector received nothing whose resource is the service $job"
  done
  passed=$((passed + 1))

  echo "smoke: telemetry: the alert rules in Prometheus, and /readyz"
  rules=$(grep -c '^ *- alert: ' "$root/deploy/observability/alerts.yaml")
  await 90 "Prometheus evaluates every alert rule" rules_healthy "$rules"
  await 30 "no span has been dropped, so its alert is quiet" alert_is StarfixTelemetrySpansDropped inactive
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
  # Dolt frozen whole, as a hang would leave it: it holds its port and
  # answers nothing.
  in_server pkill -STOP -x dolt || fail "could not freeze Dolt"
  if ! (await 30 "/readyz says why not, with Dolt frozen" readyz_says '^not ready: the last check of Dolt failed$'); then
    in_server pkill -CONT -x dolt || true
    exit 1
  fi
  passed=$((passed + 1))
  in_server pkill -CONT -x dolt || fail "could not thaw Dolt"
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
  refuse "a refused request, for its log line" 'not found' alice show sf-nosuchid
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
  await 90 "the alert for dropped spans fires" alert_is StarfixTelemetrySpansDropped firing
  await 60 "the alert for dropped log records fires" alert_is StarfixTelemetryLogRecordsDropped firing
  "${compose[@]}" start collector >/dev/null 2>&1 || fail "could not start the collector again"
  new_traceparent
  TRACEPARENT=$traceparent expect "a request once the collector is back" "^$traced " alice show "$traced"
  await 60 "its span arrives" span_arrived "^span trace=$trace_id parent=$parent_id id=[0-9a-f]{16} name=show "
  await 30 "starfixd says its exports of metrics work again" logged_once_more 'event=telemetry.export_recovered signal=metrics' "$recovered"
  [[ $(logged 'a metrics export failed') == $((warned_metrics + 1)) && $(logged 'a trace export failed') == $((warned_traces + 1)) &&
    $(logged 'a log export failed') == $((warned_logs + 1)) ]] ||
    fail "starfixd warned of failed exports more than once for each signal while the collector was stopped" "$(starfixd_log | grep 'export')"
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
  # starfixd gives its last exports 5 seconds when it stops. Docker is
  # told to wait for 30, so that the time measured is the server's own;
  # 3 more are for a clock read in whole seconds.
  t0=$SECONDS
  "${compose[@]}" restart -t 30 starfix >/dev/null 2>&1 || fail "could not restart the server with the collector frozen"
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
  TRACEPARENT=$traceparent refuse "a refused request after the restart" 'not found' alice show sf-nosuchid
  await 30 "its log record arrives at the collector" record_arrived "^record event=request.refused trace=$trace_id "
}

image=${STARFIX_IMAGE:-starfix-dev:local}
if [[ -n ${STARFIX_IMAGE:-} ]]; then
  echo "smoke: testing the image $image, and building sfx"
else
  echo "smoke: building the image and sfx"
  docker build -q -f "$root/deploy/docker/Dockerfile" -t "$image" "$@" "$root" >/dev/null
fi
if [[ -n ${SFX:-} ]]; then
  sfx=$SFX
else
  sfx=$work/sfx
  (cd "$root" && CGO_ENABLED=0 go build -trimpath -o "$sfx" ./cmd/sfx)
fi

mkdir -p "$work/keys" "$work/home" "$work/alice" "$work/bob" "$work/carol" "$work/alice-web" "$work/bob-web" "$work/transcripts" \
  "$work/bob-key" "$work/bob-out"
keys=
for who in alice bob; do
  ssh-keygen -q -t ed25519 -N '' -C "$who@example.com" -f "$work/keys/$who"
  keys+="$who $(cat "$work/keys/$who.pub")"$'\n'
done

echo "smoke: starting the server on 127.0.0.1:$port"
"${compose[@]}" down -v >/dev/null 2>&1 || true
up
project=$(server_setting project)
host_key=$(server_setting host_key)
[[ $project =~ ^[0-9a-f-]{36}$ && $host_key == SHA256:* ]] ||
  fail "the server printed no .starfix.yaml" "$("${compose[@]}" logs --no-log-prefix 2>&1 | tail -n 20)"
for who in alice bob; do
  printf 'project: %s\nserver:\n  host: localhost\n  port: %s\n  host_key: %s\n' \
    "$project" "$port" "$host_key" >"$work/$who/.starfix.yaml"
done

# Which key sfx offers. alice is the server admin and bob a member; an
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
smoke_agent=$agent_sock expect "STARFIX_KEY_FILE wins over the agent's keys" '^principal +bob$' bob whoami
grep -Eq -- "^key +$bob_key_fp, from the file $work/keys/bob, which STARFIX_KEY_FILE names\$" <<<"$last" || fail "whoami does not name bob's key, its file and what named it" "$last"
passed=$((passed + 1))
# The same with the key in the checkout, named by key: in .starfix.yaml.
cp "$work/keys/bob" "$work/bob-key/id" && cp "$work/bob/.starfix.yaml" "$work/bob-key/.starfix.yaml"
printf 'key: id\n' >>"$work/bob-key/.starfix.yaml"
smoke_agent=$agent_sock smoke_key='' expect "key: in .starfix.yaml wins over the agent's keys" '^principal +bob$' bob-key whoami
# The variable comes before key:, which names bob's key here.
smoke_agent=$agent_sock smoke_key="$work/keys/alice" expect "STARFIX_KEY_FILE wins over key:" '^principal +alice$' bob-key whoami
# A key: that leads out of the checkout is not followed.
sed "s|^key: .*|key: $work/keys/alice|" "$work/bob-key/.starfix.yaml" >"$work/bob-out/.starfix.yaml"
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
# The agent holds seven keys, alice's last. sfx offers five, one fewer
# than sshd tries, so the server answers instead of hanging up, and the
# refusal says what was left out and what to do.
smoke_agent=$agent_sock smoke_key='' refuse "no more keys are offered than sshd tries" 'accepted none of the 5 keys sfx offered' alice whoami
grep -q '2 more keys that ssh-agent holds were not offered' <<<"$last" || fail "the refusal does not say how many keys were left out" "$last"
grep -q 'STARFIX_KEY_FILE' <<<"$last" || fail "the refusal does not name STARFIX_KEY_FILE" "$last"
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
# STARFIX_KEY is never read: holding a private key, as an earlier CI
# recipe had it, it signs nobody in, and no part of it is printed.
old_value=$(cat "$work/keys/alice")
smoke_old=$old_value smoke_key='' refuse "STARFIX_KEY is not read" 'STARFIX_KEY is set, and sfx does not read it' alice whoami
if grep -qF -- "$(sed -n 2p "$work/keys/alice")" <<<"$last"; then
  fail "the refusal repeats what STARFIX_KEY holds"
fi
passed=$((passed + 1))
smoke_old=$old_value expect "STARFIX_KEY beside STARFIX_KEY_FILE changes nothing" '^principal +bob$' bob whoami
absent "a set STARFIX_KEY is not mentioned when sign-in works" 'STARFIX_KEY is set'

echo "smoke: driving sfx"
expect "create" '^sf-[a-z0-9]+$' alice create "Fix the login redirect" -p 1 -t bug
a=$last
expect "create" '^sf-[a-z0-9]+$' alice create "Add a sign-out button" -p 2 -t feature
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
expect "finish" '^created sf-[a-z0-9]+$' alice finish "$a" --reason "Fixed the redirect" \
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
expect "prices set" ': added$' alice admin prices set "${price[@]}"
expect "prices set again" ': unchanged$' alice admin prices set "${price[@]}"
session=5f0c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f
transcript=$work/transcripts/$session.jsonl
printf '{"type":"assistant","sessionId":"%s","version":"2.1.300","timestamp":"%s","requestId":"req_smoke","uuid":"u1","message":{"id":"msg_smoke","type":"message","role":"assistant","model":"example-large","content":[],"stop_reason":"end_turn","usage":{"input_tokens":100000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10000}}}\n' \
  "$session" "$(date -u +%Y-%m-%dT%H:%M:%S.000Z)" >"$transcript"
hook=$(printf '{"session_id":"%s","transcript_path":"%s","hook_event_name":"SessionEnd"}' "$session" "$transcript")
expect "usage hook" '' alice usage --hook <<<"$hook"
[[ -z $last ]] || fail "usage hook: it printed a note" "$last"
expect "cost" '^total [$]0\.60, 110k tokens, 1h logged$' alice cost --since 1d
expect "cost by model" '^ +example-large +[$]0\.60' alice cost --since 1d --by model
expect "plans set" ': added$' alice admin plans set team --from "$(date -u +%Y-%m)" --fee 25 --seats 2 --principal alice --principal bob
expect "plans" '^team .* alice, bob ' alice admin plans
# The plan's share accrues with the month, so its amount depends on the date.
expect "cost amortized" '^total [$]0\.60, [$][0-9]+\.[0-9]{2} amortized, 110k tokens' alice cost --since 1d
expect "digest" '^closed 1$' alice digest
expect "digest discovered" "^ +$d " alice digest

# A second principal reads, but is refused what only admins may do.
expect "ready as bob" "^$b " bob ready
refuse "prices set as bob" '"c":"forbidden"' bob --json admin prices set example-large --from 2026-02-01 \
  --input 1 --output 1 --cache-write 1 --cache-write-1h 1 --cache-read 1
refuse "plans set as bob" '"c":"forbidden"' bob --json admin plans set team --from "$(date -u +%Y-%m)" \
  --fee 1 --seats 1 --principal bob

# Many projects, and keys from the control database. alice is the server
# admin; `starfixd control init` made bob a member of the first project.
echo "smoke: a second project, and a key the control database holds"
expect "project list" '^first +[0-9a-f-]{36} +sf +internal +active +starfix$' alice admin project list
expect "project add" '^project web \([0-9a-f-]{36}\) created in database starfix_p001: active$' alice admin project add web
web=$(sed -n 's/^project: //p' <<<"$last")
[[ $web =~ ^[0-9a-f-]{36}$ && $web != "$project" ]] || fail "project add printed no .starfix.yaml for the new project" "$last"
for who in alice bob; do
  printf 'project: %s\nserver:\n  host: localhost\n  port: %s\n  host_key: %s\n' \
    "$web" "$port" "$host_key" >"$work/$who-web/.starfix.yaml"
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
refuse "member add shows the keys first" '^sfx: bob was not added to project web: it has 1 key' alice-web admin member add bob
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
# line, which names her key: starfixd counts no connection of hers as let
# in by authorized_keys, which it reads, and marks her key as in both.
# With bob's key out of the store, sshd falls back to its line of
# authorized_keys, and his own connection is then counted as by hand.
check_keys_file "the keys file at the start"
in_keys_file "an imported key" '^restrict,command="/usr/local/bin/starfixd stdio --principal alice --key SHA256:[A-Za-z0-9+/]{43}" ssh-ed25519 [A-Za-z0-9+/=]+$'
in_keys_file "an imported key" 'stdio --principal bob --key SHA256:'
expect "a key in both files is marked" ' +import +.* +authorized_keys$' alice admin key list alice
absent "sshd took the line of the keys file, not of authorized_keys" 'came in by a line of the server.s hand-maintained authorized_keys file'
expect "bob lists his own key, in both files" ' +import +.* +authorized_keys$' bob admin key list bob
absent "sshd took the line of the keys file for bob too" 'came in by a line of the server.s hand-maintained authorized_keys file'
bob_fp=$(ssh-keygen -lf "$work/keys/bob.pub" | cut -d' ' -f2)
expect "removing a key that authorized_keys still holds warns" '^sfx: warning: sshd will still accept key SHA256:.*: the connections it had signed in were closed, but it is also a line of the server.s authorized_keys file' \
  alice admin key remove "$bob_fp"
not_in_keys_file "the removed key" 'stdio --principal bob '
expect "and the key still signs in, by authorized_keys" "^$b " bob ready
expect "sshd then takes the line of authorized_keys" "^1 open connection of bob came in by a line of the server's hand-maintained authorized_keys file" bob admin key list bob
expect "key add, back in both files" ' added to bob$' alice admin key add bob "$work/keys/bob.pub"

# carol's key is in no authorized_keys file: sshd reads it from the keys
# file starfixd writes from the control database, with no restart.
ssh-keygen -q -t ed25519 -N '' -C carol@example.com -f "$work/keys/carol"
carol_fp=$(ssh-keygen -lf "$work/keys/carol.pub" | cut -d' ' -f2)
# A fingerprint is base64, whose + and / mean something in a pattern.
carol_re=$(sed 's/[+/.]/[&]/g' <<<"$carol_fp")
printf 'project: %s\nserver:\n  host: localhost\n  port: %s\n  host_key: %s\n' \
  "$project" "$port" "$host_key" >"$work/carol/.starfix.yaml"
refuse "a key nobody added" 'accepted no key: sfx offered SHA256:' carol ready
expect "principal add --key" "^key $carol_re added to carol\$" alice admin principal add carol --key "$work/keys/carol.pub"
if "${compose[@]}" exec -T starfix grep -q carol /state/ssh/authorized_keys; then
  fail "carol has a line in authorized_keys; her key was to be in the control database alone"
fi
passed=$((passed + 1))
in_keys_file "a key added with sfx admin" "stdio --principal carol --key $carol_re\" ssh-ed25519 "
check_keys_file "the keys file after a key was added"
expect "a key from the control database" "^$b " carol ready
expect "it signs in as its principal" '^[a-z0-9]+$' carol comment "$b" "carol was here"
expect "key list" "^$carol_re +ssh-ed25519 +carol@example.com +alice " alice admin key list carol
refuse "a private key file is not sent" 'holds a private key, which never leaves this machine' alice admin key add carol "$work/keys/bob"
refuse "nobody adds a key to their own account" 'nobody adds a key to their own account' alice admin key add alice "$work/keys/carol.pub"
# Anyone may remove a key of their own (decision 38), but not the one
# the connection signed in with: sshd took carol's line from the keys
# file, so starfixd knows which key that is.
refuse "the key in use is not removed by its own connection" "^sfx: key $carol_re is the key this connection signed in with" carol admin key remove "$carol_fp"
expect "key remove" "^key $carol_re removed from carol\$" alice admin key remove "$carol_fp"
absent "removing a key only the store holds warns of nothing" 'warning'
not_in_keys_file "a removed key" 'stdio --principal carol '
refuse "a removed key" 'accepted no key' carol ready
expect "key add" "^key $carol_re .* added to carol\$" alice admin key add carol "$work/keys/carol.pub"
expect "the key added again" "^$b " carol ready
expect "principal disable" '^carol: disabled$' alice admin principal disable carol
not_in_keys_file "a disabled principal's key" 'stdio --principal carol '
refuse "a disabled principal's key" 'accepted no key' carol ready
expect "status shows the keys file" "^keys file $keys_file: 2 keys, written " alice admin status
no_failed_write "before the restart"

# Dolt's own metrics (docs/server.md, step 2): the endpoint answers on
# loopback in the container and on no other address, and starfixd reads
# it for the status report. The image has no curl, so bash asks.
echo "smoke: Dolt's metrics, and its figures in the status"
last=$(in_server bash -c 'exec 3<>/dev/tcp/127.0.0.1/11228 && printf "GET /metrics HTTP/1.0\r\n\r\n" >&3 && cat <&3' 2>&1) ||
  fail "Dolt's metrics endpoint does not answer on 127.0.0.1:11228 in the server" "$last"
for series in dss_concurrent_connections process_resident_memory_bytes go_gc_duration_seconds_count; do
  grep -q "^$series " <<<"$last" || fail "Dolt's metrics endpoint did not serve $series" "$(head -n 20 <<<"$last")"
done
passed=$((passed + 1))
# sshd listens on every address, so its port shows that the probe works.
addr=$(in_server hostname -i | cut -d' ' -f1)
[[ $addr =~ ^[0-9.]+$ && $addr != 127.* ]] || fail "could not find the container's own address" "$addr"
in_server bash -c "exec 3<>/dev/tcp/$addr/22" 2>/dev/null || fail "sshd does not answer on the container's own address, $addr"
if in_server bash -c "exec 3<>/dev/tcp/$addr/11228" 2>/dev/null; then
  fail "Dolt's metrics endpoint answers on the container's own address, $addr, not on loopback alone"
fi
passed=$((passed + 1))
expect "status shows Dolt's figures" '^Dolt 2\.[0-9]+\.[0-9]+, up [0-9a-z ]+, [0-9]+ connections?, [0-9]+ MiB resident, no memory limit$' alice admin status
expect "status --json carries them" '"dolt":\{"read_at":"[^"]+","version":"2\.[0-9.]+","started_at":"[^"]+","uptime_seconds":[0-9]+,"resident_bytes":[0-9]+,"conns":[0-9]+,"mem_limit_bytes":0' alice --json admin status
# The collector's cycles are set against starfixd's writes once it has
# read twice, 15 seconds apart.
for ((i = 0; i < 60; i++)); do
  if sfx_as alice admin status 2>/dev/null | grep -q "^Dolt's collector: "; then
    break
  fi
  sleep 0.5
done
expect "status shows Dolt's collector against the writes" "^Dolt's collector: [0-9]+ cycles? for [0-9]+ writes? in [0-9a-z ]+" alice admin status
# bob is no server admin: Dolt is the server's, and he is shown none of it.
refuse "Dolt's figures are for server admins" 'may not see the server.s status' bob admin status

# What a restart must not undo. The container runs `starfixd control
# init` at every start, and bob's line is still in STARFIX_KEYS.
expect "member remove before the restart" '^bob removed from project first$' alice admin member remove bob
refuse "a removed member" 'bob is not a member of project' bob ready

echo "smoke: restarting the server"
restart_server
[[ $(server_setting project) == "$project" ]] || fail "restart: the project changed" "$(server_setting project)"
[[ $(server_setting host_key) == "$host_key" ]] || fail "restart: the host key changed" "$(server_setting host_key)"
passed=$((passed + 2))
expect "show after restart" "^$a .*closed" alice show "$a"
expect "admins after restart" ': unchanged$' alice admin prices set "${price[@]}"
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
last=$("${compose[@]}" logs --no-log-prefix starfix 2>&1)
grep -Eq 'authorized_keys line [0-9]+: bob was removed from the project by alice on .*; kept removed' <<<"$last" ||
  fail "the start did not report bob's membership as kept removed" "$(grep -E 'authorized_keys|left as admins' <<<"$last" | tail -n 10)"
passed=$((passed + 1))
# The operator puts him back on purpose, as docs/deploy/docker.md says
# to; the running server reads the control database again within half a
# minute.
last=$("${compose[@]}" exec -T -u starfix -w /var/lib/starfix starfix starfixd control init --name first --restore-removed-access 2>&1) ||
  fail "control init --restore-removed-access failed" "$last"
grep -Eq '^restored from this file, over what admins had set: 0 keys, 1 membership, 0 principals enabled$' <<<"$last" ||
  fail "control init --restore-removed-access did not say it restored bob's membership" "$last"
passed=$((passed + 1))
for ((i = 0; i < 90; i++)); do
  if sfx_as bob ready >/dev/null 2>&1; then
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
printf 'project: %s\nserver:\n  host: localhost\n  port: %s\n  host_key: %s\n' \
  "$project" "$port" "$host_key" >"$work/dave/.starfix.yaml"
by_hand() {
  printf 'restrict,command="/usr/local/bin/starfixd stdio --principal %s" %s\n' "$1" "$(cat "$2")" |
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
# then the operator gives the same key to alice by hand. starfixd holds
# carol's line back from its file, so sshd reaches the operator's.
refuse "carol's key signs in as carol, who is no server admin" 'for server admins' carol admin principal list
by_hand alice "$work/keys/carol.pub"
for _ in $(seq 1 45); do
  in_server grep -Eq 'stdio --principal carol ' "$keys_file" || break
  sleep 1
done
not_in_keys_file "a key the operator's file gives to another principal" 'stdio --principal carol '
expect "that key signs in as the principal the operator's line names" '^alice +active' carol admin principal list
expect "status counts the key held back" '^keys held back from it: 1,' alice admin status
no_failed_write "at the end"

# starfixd's log follows its schema (docs/server.md, Logs): every line it
# wrote names an event, and an access change made with `sfx admin` is a
# line of the audit stream, read back from the control database's log.
# Only starfixd writes lines that start with time= and have level=;
# sshd's and Dolt's are theirs.
echo "smoke: the log's schema and the audit stream"
last=$("${compose[@]}" logs --no-log-prefix starfix 2>&1 | grep -E '^time=[^ ]+ level=[A-Z]+ msg=' || true)
[[ -n $last ]] || fail "starfixd logged no line"
if unnamed=$(grep -Ev ' event=[a-z_]+([.][a-z_]+)+( |$)' <<<"$last"); then
  fail "a line of starfixd's log names no event" "$(head -n 5 <<<"$unnamed")"
fi
passed=$((passed + 1))
grep -Eq ' event=audit[.]key[.]add audit=true seq=[0-9]+ at=[^ ]+ principal=alice session=[^ ]+ op=key[.]add target=SHA256:[^ ]+ fingerprint=SHA256:[^ ]+ subject=carol$' <<<"$last" ||
  fail "the audit stream has no line for the key alice added to carol" "$(grep -F ' audit=true ' <<<"$last" | tail -n 10)"
passed=$((passed + 1))
if grep -Eq 'ssh-ed25519 AAAA' <<<"$last"; then
  fail "starfixd's log holds a key's text" "$(grep -E 'ssh-ed25519 AAAA' <<<"$last" | cut -c1-200 | head -n 3)"
fi
passed=$((passed + 1))

if [[ -n $telemetry ]]; then
  telemetry_checks
  no_failed_write "after the telemetry checks"
fi

echo "smoke: ok, $passed checks passed"
