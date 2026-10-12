#!/usr/bin/env bash
# Smoke test of the Docker test server (docs/deploy/docker.md). It builds
# the image and sfx from this checkout, starts a throwaway server with two
# principals, drives sfx through the main surfaces end to end, then
# through a server of many projects (a second project, and a person
# whose key only the control database holds, served to sshd by `starfixd
# keys`), restarts the server to check that its state persists, then
# removes the server and its volume. It exits non-zero on the first
# failure.
#
#   deploy/docker/smoke.sh [DOCKER BUILD FLAGS...]
#
# Flags go to `docker build`, for example --secret id=ca,src=FILE behind a
# TLS-intercepting proxy. SFX names an sfx binary to use instead of
# building one. STARFIX_PORT is the port on 127.0.0.1, 2223 by default, so
# a dev server on 2222 is left alone.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
port=${STARFIX_PORT:-2223}
work=$(mktemp -d)
compose=(docker compose -p starfix-smoke -f "$root/deploy/docker/compose.yaml")
passed=0
last=

cleanup() {
  "${compose[@]}" down -v >/dev/null 2>&1 || true
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
# session and a home inside the work directory.
sfx_as() {
  local who=$1
  shift
  (cd "$work/$who" && HOME="$work/home" XDG_CACHE_HOME="$work/home/.cache" STARFIX_SESSION="smoke-$who" "$sfx" "$@")
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

up() {
  STARFIX_KEYS=$keys STARFIX_ADMINS=alice STARFIX_PORT=$port \
    "${compose[@]}" up -d --no-build --wait --wait-timeout 120 >/dev/null 2>&1 ||
    fail "the server did not become healthy" "$("${compose[@]}" logs --no-log-prefix 2>&1 | tail -n 40)"
}

echo "smoke: building the image and sfx"
docker build -q -f "$root/deploy/docker/Dockerfile" -t starfix-dev:local "$@" "$root" >/dev/null
if [[ -n ${SFX:-} ]]; then
  sfx=$SFX
else
  sfx=$work/sfx
  (cd "$root" && CGO_ENABLED=0 go build -trimpath -o "$sfx" ./cmd/sfx)
fi

mkdir -p "$work/keys" "$work/home" "$work/alice" "$work/bob" "$work/carol" "$work/alice-web" "$work/bob-web" "$work/transcripts"
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
  printf 'project: %s\nserver:\n  host: localhost\n  port: %s\n  host_key: %s\nkey: %s\n' \
    "$project" "$port" "$host_key" "$work/keys/$who" >"$work/$who/.starfix.yaml"
done

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
  printf 'project: %s\nserver:\n  host: localhost\n  port: %s\n  host_key: %s\nkey: %s\n' \
    "$web" "$port" "$host_key" "$work/keys/$who" >"$work/$who-web/.starfix.yaml"
done
expect "create in the second project" '^web-[a-z0-9]+$' alice-web create "First in web" -p 2 -t task
w=$last
expect "the second project holds it" "^$w " alice-web ready
expect "the first project does not" '' alice ready
absent "the first project does not" "^$w "
# bob is a member of the first project only.
refuse "a non-member of the second project" 'bob is not a member of project' bob-web ready
expect "member add" '^bob added to project web as member$' alice-web admin member add bob
expect "a member of the second project" "^$w " bob-web ready

# carol's key is in no authorized_keys file: sshd learns of it from
# starfixd keys, which reads the control database.
ssh-keygen -q -t ed25519 -N '' -C carol@example.com -f "$work/keys/carol"
carol_fp=$(ssh-keygen -lf "$work/keys/carol.pub" | cut -d' ' -f2)
# A fingerprint is base64, whose + and / mean something in a pattern.
carol_re=$(sed 's/[+/.]/[&]/g' <<<"$carol_fp")
printf 'project: %s\nserver:\n  host: localhost\n  port: %s\n  host_key: %s\nkey: %s\n' \
  "$project" "$port" "$host_key" "$work/keys/carol" >"$work/carol/.starfix.yaml"
refuse "a key nobody added" 'send your public key' carol ready
expect "principal add --key" "^key $carol_re added to carol\$" alice admin principal add carol --key "$work/keys/carol.pub"
if "${compose[@]}" exec -T starfix grep -q carol /state/ssh/authorized_keys; then
  fail "carol has a line in authorized_keys; her key was to be in the control database alone"
fi
passed=$((passed + 1))
expect "a key from the control database" "^$b " carol ready
expect "it signs in as its principal" '^[a-z0-9]+$' carol comment "$b" "carol was here"
expect "key list" "^$carol_re +ssh-ed25519 +carol@example.com +alice " alice admin key list carol
refuse "a private key file is not sent" 'holds a private key, which never leaves this machine' alice admin key add carol "$work/keys/bob"
refuse "nobody changes their own keys" 'nobody changes the keys of their own account' alice admin key add alice "$work/keys/carol.pub"
expect "key remove" "^key $carol_re removed from carol\$" alice admin key remove "$carol_fp"
refuse "a removed key" 'send your public key' carol ready
expect "key add" "^key $carol_re .* added to carol\$" alice admin key add carol "$work/keys/carol.pub"
expect "the key added again" "^$b " carol ready
expect "principal disable" '^carol: disabled$' alice admin principal disable carol
refuse "a disabled principal's key" 'send your public key' carol ready

echo "smoke: restarting the server"
"${compose[@]}" restart >/dev/null 2>&1
up
[[ $(server_setting project) == "$project" ]] || fail "restart: the project changed" "$(server_setting project)"
[[ $(server_setting host_key) == "$host_key" ]] || fail "restart: the host key changed" "$(server_setting host_key)"
passed=$((passed + 2))
expect "show after restart" "^$a .*closed" alice show "$a"
expect "admins after restart" ': unchanged$' alice admin prices set "${price[@]}"
expect "the second project after restart" "^$w " bob-web ready
expect "principal enable after restart" '^carol: active$' alice admin principal enable carol
expect "the control database's key after restart" "^$b " carol ready

echo "smoke: ok, $passed checks passed"
