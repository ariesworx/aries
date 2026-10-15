#!/usr/bin/env bash
# The entrypoint of the compose file's `identity` profile: Keycloak, a
# local OpenID Connect provider for developing and testing sign-in with
# no account anywhere and no internet (docs/deploy/docker.md, "Sign-in
# with a local identity provider"). A test fixture, never production.
#
# It runs in Keycloak's own image, as Keycloak's user. On the first
# start it generates the bootstrap admin's password, two people's
# passwords and two client secrets, each in a 0600 file on the volume
# (secrets, below), and never prints, logs or passes one as an
# argument: they reach Keycloak in the environment of the process that
# needs them and in request bodies read from stdin. Then, on every
# start, it starts Keycloak and makes whatever the realm lacks with
# kcadm.sh: the realm, an ES256 key, the people and the three clients.
# What is already there is kept as it is, so a start changes nothing a
# person did in the admin console; to start over, remove the volume.
# Once all of it is there it touches the file the health check reads.
#
#   identity.sh          start Keycloak and set the realm up
#   identity.sh show     print the realm, its clients and its people,
#                        with no secret, for smoke.sh and for a person
set -euo pipefail
umask 077

port=${ARIES_IDENTITY_PORT:-8180}
# One issuer for everyone: a browser on the host reaches it on loopback,
# where *.localhost resolves, and the server's container by the
# compose network's alias of the same name.
host=keycloak.localhost
url=http://$host:$port
realm=aries
secrets=/opt/keycloak/data/aries
ready=/tmp/aries-identity-ready
kcadm=/opt/keycloak/bin/kcadm.sh
# What a workload asks its platform for, as identity.md's settings show
# it, and so the audience of the service principal's tokens.
workload_audience=aries.example.com

log() { printf 'aries-identity: %s\n' "$*" >&2; }

die() {
  log "$1"
  log "fix: $2"
  exit 1
}

random_hex() { od -An -N24 -tx1 /dev/urandom | tr -d ' \n'; }

# secret NAME prints the secret in NAME's file, making it first if it is
# not there.
secret() {
  local f=$secrets/$1
  if [[ ! -s $f ]]; then
    random_hex >"$f.new"
    chmod 0600 "$f.new"
    mv "$f.new" "$f"
  fi
  cat "$f"
}

# kc ARGS... runs kcadm.sh against this Keycloak with its own config
# file, which holds the admin's access token for the length of this
# script and is removed with it.
kc_config=
kc() { "$kcadm" "$@" --config "$kc_config"; }

sign_in() {
  kc_config=$(mktemp -d)/kcadm.config
  local i
  for ((i = 0; i < 180; i++)); do
    if KC_CLI_PASSWORD=$(cat "$secrets/admin.pass") \
      "$kcadm" config credentials --config "$kc_config" --server "http://127.0.0.1:$port" \
      --realm master --user admin >/dev/null 2>&1; then
      return 0
    fi
    if [[ -n ${kc_pid:-} ]] && ! kill -0 "$kc_pid" 2>/dev/null; then
      die "Keycloak exited while starting" "read its log above"
    fi
    sleep 1
  done
  die "could not sign in to Keycloak's admin API within 180s" "read its log above"
}

# id_of PATH QUERY... prints the id of the one object at PATH matching
# QUERY, or nothing.
id_of() {
  local path=$1
  shift
  kc get "$path" -r "$realm" "$@" --fields id --format csv --noquotes 2>/dev/null | head -n 1
}

ensure_realm() {
  if kc get "realms/$realm" --fields realm >/dev/null 2>&1; then
    return 0
  fi
  # Five minutes for a token: it is spent at once. The device grant's
  # code lives ten minutes and is polled every two seconds.
  kc create realms -f - >/dev/null <<EOF
{"realm": "$realm", "enabled": true, "displayName": "Aries (test)",
 "registrationAllowed": false, "loginWithEmailAllowed": true, "duplicateEmailsAllowed": false,
 "accessTokenLifespan": 300, "oauth2DeviceCodeLifespan": 600, "oauth2DevicePollingInterval": 2}
EOF
  log "made the realm $realm"
}

# ensure_es256 adds an ES256 signing key beside the default RS256 one,
# as the spike did, so that ID tokens can be signed with either.
ensure_es256() {
  if [[ -n $(id_of components -q name=ecdsa-p256) ]]; then
    return 0
  fi
  local parent
  parent=$(kc get "realms/$realm" --fields id --format csv --noquotes)
  kc create components -r "$realm" -f - >/dev/null <<EOF
{"name": "ecdsa-p256", "providerId": "ecdsa-generated", "providerType": "org.keycloak.keys.KeyProvider",
 "parentId": "$parent", "config": {"priority": ["100"], "ecdsaEllipticCurveKey": ["P-256"]}}
EOF
  log "made an ES256 signing key"
}

# ensure_person NAME FIRST makes NAME@example.com, with a verified
# address and the password in NAME.pass.
ensure_person() {
  local name=$1 first=$2
  if [[ -n $(id_of users -q "username=$name" -q exact=true) ]]; then
    return 0
  fi
  kc create users -r "$realm" -f - >/dev/null <<EOF
{"username": "$name", "email": "$name@example.com", "emailVerified": true, "enabled": true,
 "firstName": "$first", "lastName": "Example",
 "credentials": [{"type": "password", "value": "$(secret "$name.pass")", "temporary": false}]}
EOF
  log "made $name@example.com; the password is in $secrets/$name.pass"
}

# ensure_client ID JSON makes the client ID from JSON on stdin.
ensure_client() {
  local id=$1 body
  body=$(cat)
  if [[ -n $(id_of clients -q "clientId=$id") ]]; then
    return 0
  fi
  kc create clients -r "$realm" -f - >/dev/null <<<"$body"
  log "made the client $id"
}

# The redirects of a native client: loopback on any port (RFC 8252
# §7.3), and Keycloak's own page that shows the code, for a sign-in
# finished by pasting it.
redirects='["http://127.0.0.1/*", "urn:ietf:wg:oauth:2.0:oob"]'

ensure_clients() {
  # aries login: public, the code flow with PKCE, and the device grant,
  # which is on here for testing and off by default on a server.
  ensure_client aries-cli <<EOF
{"clientId": "aries-cli", "name": "aries login (test)", "protocol": "openid-connect", "publicClient": true,
 "standardFlowEnabled": true, "implicitFlowEnabled": false, "directAccessGrantsEnabled": false,
 "serviceAccountsEnabled": false, "consentRequired": false, "redirectUris": $redirects,
 "attributes": {"pkce.code.challenge.method": "S256", "oauth2.device.authorization.grant.enabled": "true",
   "id.token.signed.response.alg": "ES256", "use.refresh.tokens": "false"}}
EOF
  # The server: confidential, for a code exchanged by the server rather
  # than by aries. Its secret is in aries-server.secret.
  ensure_client aries-server <<EOF
{"clientId": "aries-server", "name": "The Aries server (test)", "protocol": "openid-connect", "publicClient": false,
 "clientAuthenticatorType": "client-secret", "secret": "$(secret aries-server.secret)",
 "standardFlowEnabled": true, "implicitFlowEnabled": false, "directAccessGrantsEnabled": false,
 "serviceAccountsEnabled": false, "consentRequired": false, "redirectUris": $redirects,
 "attributes": {"pkce.code.challenge.method": "S256", "id.token.signed.response.alg": "ES256",
   "use.refresh.tokens": "false"}}
EOF
  # A service principal: the client-credentials grant only, its tokens
  # for the workload audience, with made-up claims in the shape of a
  # GitHub Actions token for trust rules to match. Its secret is in
  # ci-example.secret.
  ensure_client ci-example <<EOF
{"clientId": "ci-example", "name": "A CI workload (test)", "protocol": "openid-connect", "publicClient": false,
 "clientAuthenticatorType": "client-secret", "secret": "$(secret ci-example.secret)",
 "standardFlowEnabled": false, "implicitFlowEnabled": false, "directAccessGrantsEnabled": false,
 "serviceAccountsEnabled": true, "consentRequired": false,
 "attributes": {"use.refresh.tokens": "false"},
 "protocolMappers": [
  {"name": "aud", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
   "config": {"included.custom.audience": "$workload_audience", "access.token.claim": "true", "id.token.claim": "false"}},
  $(claim repository_id 123456789), $(claim repository example/app), $(claim ref refs/heads/main),
  $(claim workflow_ref example/app/.github/workflows/ci.yml@refs/heads/main)]}
EOF
}

# claim NAME VALUE prints a mapper that puts NAME: VALUE in access tokens.
claim() {
  printf '{"name": "%s", "protocol": "openid-connect", "protocolMapper": "oidc-hardcoded-claim-mapper", "config": {"claim.name": "%s", "claim.value": "%s", "jsonType.label": "String", "access.token.claim": "true", "id.token.claim": "false", "introspection.token.claim": "true"}}' "$1" "$1" "$2"
}

setup() {
  ensure_realm
  ensure_es256
  ensure_person alice Alice
  ensure_person bob Bob
  ensure_clients
}

# show prints one line for the realm, each client and each person, and
# the mode of each secret's file: what smoke.sh checks.
show() {
  sign_in
  local c id json
  kc get "realms/$realm" --fields realm,enabled --format csv --noquotes 2>/dev/null |
    sed -n 's/^\([^,]*\),\(.*\)$/realm \1 enabled=\2/p'
  for c in aries-cli aries-server ci-example; do
    id=$(id_of clients -q "clientId=$c")
    if [[ -z $id ]]; then
      echo "client $c missing"
      continue
    fi
    # The client's representation holds its secret: only the fields
    # named here are read from it, and it is never printed.
    json=$(kc get "clients/$id" -r "$realm" 2>/dev/null | tr -d ' \n')
    echo "client $c public=$(field publicClient) code=$(field standardFlowEnabled)" \
      "service=$(field serviceAccountsEnabled) password=$(field directAccessGrantsEnabled)" \
      "pkce=$(field pkce.code.challenge.method) device=$(field oauth2.device.authorization.grant.enabled)" \
      "id_token=$(field id.token.signed.response.alg)"
  done
  kc get users -r "$realm" --fields username,email,emailVerified,enabled --format csv --noquotes 2>/dev/null |
    sed -n 's/^\([^,]*\),\([^,]*\),\([^,]*\),\([^,]*\)$/user \1 email=\2 email_verified=\3 enabled=\4/p'
  (cd "$secrets" && stat -c 'file %n %a' admin.pass alice.pass bob.pass aries-server.secret ci-example.secret)
  stat -c 'dir %a' "$secrets"
}

# field NAME prints the value of NAME, a boolean or a string, from json,
# or "-".
field() {
  local v
  v=$(sed -n "s/.*\"$1\":\"\{0,1\}\([A-Za-z0-9_-]*\).*/\1/p" <<<"$json" | head -n 1)
  echo "${v:--}"
}

cleanup() {
  if [[ -n $kc_config ]]; then
    rm -rf "$(dirname "$kc_config")"
  fi
}
trap cleanup EXIT

run() {
  rm -f "$ready"
  install -d -m 0700 "$secrets"
  local admin_pass
  admin_pass=$(secret admin.pass)
  # The bootstrap admin is made on the first start alone, when the
  # master realm has no admin; later starts ignore these two.
  KC_BOOTSTRAP_ADMIN_USERNAME=admin KC_BOOTSTRAP_ADMIN_PASSWORD=$admin_pass \
    KC_HTTP_PORT=$port KC_HOSTNAME=$url \
    /opt/keycloak/bin/kc.sh start-dev &
  kc_pid=$!
  unset admin_pass
  trap 'kill -TERM "$kc_pid" 2>/dev/null || true; wait "$kc_pid" || true; exit 0' TERM INT
  sign_in
  setup
  rm -rf "$(dirname "$kc_config")"
  kc_config=
  touch "$ready"
  cat >&2 <<EOF

Keycloak ready for sign-in tests (a test fixture, not for production)
  issuer      $url/realms/$realm
  discovery   $url/realms/$realm/.well-known/openid-configuration
  admin       $url/admin/ (user admin)
  clients     aries-cli (public), aries-server (confidential), ci-example (client credentials)
  people      alice@example.com, bob@example.com
  secrets     $secrets, one 0600 file each; print one with
              docker compose --profile identity exec keycloak cat $secrets/alice.pass

EOF
  local status=0
  wait "$kc_pid" || status=$?
  log "Keycloak exited with status $status"
  exit 1
}

case ${1:-} in
'') run ;;
show) show ;;
*) die "unknown argument '$1'" "run it with no argument, or with show" ;;
esac
