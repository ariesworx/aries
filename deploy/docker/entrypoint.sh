#!/usr/bin/env bash
# The Aries test server's entrypoint (deploy/docker/Dockerfile), doing
# what docs/server.md's steps 3 to 6 do on a machine, and then what its
# "Serving many projects" does: the server holds many projects, and sshd
# reads the keys of its control store from a file ariesd writes,
# /state/ssh/aries_keys, beside authorized_keys.
#
# The database is the compose file's `postgres` container, which made
# ariesd's role and database on its first start, as an administrator
# makes them on a Postgres ariesd reaches over the network
# (docs/server.md, "Managed services"), and left the role's generated
# password in a 0600 file on the dbsecret volume. On every start this
# copies the password into the password file ariesd's DSN names, 0600
# and the aries user's, so that the config file holds no password.
#
# On the first start it makes the sshd host key and writes the config,
# a server of many projects from the start. On every start it installs
# the principals given in ARIES_KEYS or /etc/aries-keys, runs `ariesd
# control init`, which makes the control schema, adopts the project and
# imports those principals and keys, runs ariesd and sshd, and prints
# the .aries.yaml a client needs. When one of the two exits, it stops
# the other and exits 1.
#
# It serves HTTPS, as a fresh server does (docs/server.md, The HTTPS
# listener), on port 8443 inside the container, with a certificate for
# localhost and 127.0.0.1 from a CA of its own that it makes on the
# first start and keeps on the volume. SSH is off unless ARIES_SSH=1:
# then sshd runs and ariesd writes the keys file it reads, as with
# install-server.sh --enable-ssh.
#
# With ARIES_TELEMETRY=1 ariesd also reports on itself, set up as
# docs/server.md's Observability sets a server up: it serves /metrics,
# /healthz and /readyz on loopback, and pushes metrics, traces and logs over
# OTLP to a collector on loopback, which the compose file's `telemetry`
# profile runs in this container's network namespace. Without it none
# of that is on.
set -euo pipefail
umask 077

state=/state
config=/etc/aries/ariesd.yaml
authorized=$state/ssh/authorized_keys
hostkey=$state/hostkeys/ssh_host_ed25519_key
keys_file=/etc/aries-keys
ready=/run/aries-ready
port=${ARIES_PORT:-2222}
# HTTPS: the listener inside the container, the port of this machine it
# is published on, which the URL names, and the certificate's files,
# with the CA that signed it beside them for clients' server.ca.
https_port=${ARIES_HTTPS_PORT:-8443}
tls_dir=/etc/aries/tls
tls_ca=$tls_dir/ca.pem
# The server of many projects: its control schema, and the name the
# project it starts with is adopted under.
control_schema=aries_control
first_project=first
# The database: the postgres container, by its name on the compose
# network, with the password file this entrypoint writes. The network
# is the containers' own, so the DSN asks for no TLS; a real Postgres on
# another machine gets sslmode=verify-full (docs/server.md, "Managed
# services").
db_host=postgres
db_secret=/aries-db/password
db_files=/etc/aries/postgres
pgpass=$db_files/pgpass
dsn="postgres://aries@$db_host:5432/aries?sslmode=disable&passfile=$pgpass"
# ariesd's telemetry, with ARIES_TELEMETRY=1: its loopback listener,
# the collector it pushes to, and the key its stand-ins for people are
# made with, which is on the volume so that a person keeps one stand-in
# across a rebuild. Its log records are exported too, which a server
# has to ask for (OTEL_LOGS_EXPORTER=otlp), so that the harness has all
# three signals. A test server exports metrics every 5 seconds and
# holds a finished span or log record for at most one, where a real server's defaults
# are 60 and 5, so that what was just done can be looked at. None of
# these values is a secret, so they may be arguments of env.
telemetry_listen=127.0.0.1:9464
telemetry_endpoint=http://127.0.0.1:4318
telemetry_key=$state/telemetry/principals.key
telemetry_env=("OTEL_EXPORTER_OTLP_ENDPOINT=$telemetry_endpoint" OTEL_LOGS_EXPORTER=otlp OTEL_METRIC_EXPORT_INTERVAL=5000
  OTEL_BSP_SCHEDULE_DELAY=1000 OTEL_BLRP_SCHEDULE_DELAY=1000)

name_re='^[a-z][a-z0-9._-]{0,63}$'
uuid_re='^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
type_re='^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com|sk-ecdsa-sha2-nistp256@openssh\.com)$'
blob_re='^[A-Za-z0-9+/]+={0,3}$'

pids=()   # children, in the order they started
names=()  # their names, for the log

log() { printf 'aries-docker: %s\n' "$*" >&2; }

# stop ends the children, last started first, so sshd stops taking
# connections before ariesd closes its stores.
stop() {
  trap - TERM INT
  local i
  for ((i = ${#pids[@]} - 1; i >= 0; i--)); do
    kill -TERM "${pids[i]}" 2>/dev/null || true
    wait "${pids[i]}" 2>/dev/null || true
  done
  pids=()
}

die() {
  log "$1"
  log "fix: $2"
  stop
  exit 1
}

# run_as USER DIR CMD... replaces the shell with CMD, run as USER in DIR
# with a bare environment and no way to gain privileges, as the systemd
# unit in docs/server.md does. Call it in a subshell or in the background,
# so the child's pid is the program's own.
run_as() {
  local user=$1 dir=$2
  shift 2
  cd "$dir" || exit 1
  exec env -i PATH=/usr/local/bin:/usr/bin:/bin HOME="$(getent passwd "$user" | cut -d: -f6)" \
    setpriv --reuid="$user" --regid="$user" --init-groups --no-new-privs "$@"
}

# start NAME CMD... runs CMD in the background as a supervised child.
start() {
  local name=$1
  shift
  "$@" &
  pids+=("$!")
  names+=("$name")
}

# wait_for NAME TEST... waits up to 30s for TEST to pass while NAME, the
# newest child, still runs.
wait_for() {
  local name=$1 pid=${pids[-1]} i
  shift
  for ((i = 0; i < 300; i++)); do
    if "$@"; then
      return 0
    fi
    kill -0 "$pid" 2>/dev/null || die "$name exited while starting" "read its log above"
    sleep 0.1
  done
  die "$name did not start within 30s" "read its log above"
}

# Called through wait_for or wait_db, which shellcheck cannot see: 0.11
# reports that as SC2329, and the 0.9 on CI's runner as SC2317.
# shellcheck disable=SC2317,SC2329
port_open() { (: <"/dev/tcp/$1/$2") 2>/dev/null; }

# wait_db waits up to 60s for Postgres to take connections. The compose
# file starts this container once Postgres is healthy; a restart of this
# container alone, or of Postgres's, may still find it on its way up.
wait_db() {
  local i
  for ((i = 0; i < 600; i++)); do
    if port_open "$db_host" 5432; then
      return 0
    fi
    sleep 0.1
  done
  die "Postgres does not answer at $db_host:5432" "docker compose logs postgres; and docker compose up -d starts it"
}

setting() { sed -n "s/^$1: //p" "$config"; }

# write_config PROJECT ADMINS writes the config file, mode 0600: the DSN,
# which holds no password, the project, the admins and the control
# schema; and, with ARIES_TELEMETRY=1, ariesd's own listener and the
# stand-ins' key.
write_config() {
  {
    printf 'dsn: %s\nproject: %s\nadmins: [%s]\ncontrol_schema: %s\n' "$dsn" "$1" "$2" "$control_schema"
    if ssh_on; then
      printf 'transports: [https, ssh]\n'
    else
      printf 'transports: [https]\n'
    fi
    printf 'https:\n  listen: ":8443"\n  public_urls: [https://localhost:%s]\n  tls:\n    mode: files\n    files:\n      cert: %s/cert.pem\n      key: %s/key.pem\n' \
      "$https_port" "$tls_dir" "$tls_dir"
    if telemetry_on; then
      printf 'telemetry:\n  listen: %s\n  principals_key: %s\n' "$telemetry_listen" "$telemetry_key"
    fi
    if identity_on; then
      local iss
      iss=$(kc_issuer)
      printf 'login:\n  providers:\n    - name: keycloak\n      kind: oidc\n      issuer: %s\n      client_id: aries-server\n' "$iss"
      printf '      client_secret_file: %s/keycloak.secret\n      discovery: static\n' "$login_dir"
      printf '      authorization_endpoint: %s/protocol/openid-connect/auth\n      token_endpoint: %s/protocol/openid-connect/token\n' "$iss" "$iss"
      printf '      jwks_file: %s/keycloak-jwks.json\n' "$login_dir"
    fi
  } | install -o aries -g aries -m 0600 /dev/stdin "$config"
}

telemetry_on() { [[ ${ARIES_TELEMETRY:-} == 1 ]]; }
ssh_on() { [[ ${ARIES_SSH:-} == 1 ]]; }
identity_on() { [[ ${ARIES_IDENTITY:-} == 1 ]]; }

# With ARIES_IDENTITY=1, ariesd signs people in at the identity
# profile's Keycloak, as the provider keycloak (docs/server.md,
# "Sign-in"), offline: its endpoints written out, and its keys from a
# file, so that ariesd fetches nothing to check a token. The client
# secret Keycloak generated is on its volume, read-only here; ariesd
# reads a 0600 copy.
identity_port=${ARIES_IDENTITY_PORT:-8180}
identity_secrets=/run/aries-identity/aries
login_dir=/etc/aries/login
kc_issuer() { printf 'http://keycloak.localhost:%s/realms/aries' "$identity_port"; }

# kc_get PATH prints the body of what Keycloak answers to GET PATH, by
# the issuer's name. The image has no curl, so bash asks.
kc_get() {
  (
    exec 3<>"/dev/tcp/keycloak.localhost/$identity_port" &&
      printf 'GET %s HTTP/1.0\r\nHost: keycloak.localhost:%s\r\n\r\n' "$1" "$identity_port" >&3 &&
      sed '1,/^\r$/d' <&3
  ) 2>/dev/null
}

# setup_login waits for Keycloak's realm and its client secret, and
# keeps for ariesd the secret, mode 0600, and the realm's key set.
setup_login() {
  identity_on || return 0
  [[ $identity_port =~ ^[1-9][0-9]{0,4}$ ]] || die "ARIES_IDENTITY_PORT is '$identity_port'" "set it to Keycloak's port, as the identity profile publishes it"
  install -d -o root -g aries -m 0750 "$login_dir"
  local i jwks
  for ((i = 0; i < 240; i++)); do
    if [[ -s $identity_secrets/aries-server.secret ]] && jwks=$(kc_get /realms/aries/protocol/openid-connect/certs) && [[ $jwks == *'"keys"'*'"ES256"'* ]]; then
      install -o aries -g aries -m 0600 "$identity_secrets/aries-server.secret" "$login_dir/keycloak.secret"
      printf '%s\n' "$jwks" | install -o aries -g aries -m 0644 /dev/stdin "$login_dir/keycloak-jwks.json"
      log "sign-in at Keycloak ($(kc_issuer)): its client secret and key set are in $login_dir"
      return 0
    fi
    sleep 1
  done
  die "Keycloak's realm and client secret did not appear within 240s" "start the identity profile beside the server: docker compose --profile identity up -d"
}

# make_tls makes, on the first start, a CA and a certificate it signs
# for localhost and 127.0.0.1, which the listener serves: a test CA,
# never a real one, whose key stays on the volume, mode 0600. Clients
# trust it by server.ca, which report prints.
make_tls() {
  install -d -o root -g aries -m 0750 "$tls_dir"
  if [[ -s $tls_dir/cert.pem && -s $tls_dir/key.pem && -s $tls_ca ]]; then
    return 0
  fi
  local tmp
  tmp=$(mktemp -d)
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 -subj '/CN=Aries test CA' \
    -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign' \
    -keyout "$tmp/ca.key" -out "$tmp/ca.pem" 2>/dev/null || die "could not make the test CA" "read openssl's error: run the entrypoint by hand"
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj '/CN=localhost' \
    -keyout "$tmp/key.pem" -out "$tmp/req.pem" 2>/dev/null || die "could not make the certificate's key" "read openssl's error"
  printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' >"$tmp/ext"
  openssl x509 -req -in "$tmp/req.pem" -CA "$tmp/ca.pem" -CAkey "$tmp/ca.key" -CAcreateserial -days 825 \
    -extfile "$tmp/ext" -out "$tmp/cert.pem" 2>/dev/null || die "could not sign the certificate" "read openssl's error"
  install -o root -g aries -m 0644 "$tmp/ca.pem" "$tls_ca"
  install -o root -g aries -m 0644 "$tmp/cert.pem" "$tls_dir/cert.pem"
  install -o aries -g aries -m 0600 "$tmp/key.pem" "$tls_dir/key.pem"
  install -o root -g root -m 0600 "$tmp/ca.key" "$state/tls-ca.key"
  rm -rf "$tmp"
  log "made a test CA and a certificate for localhost and 127.0.0.1 in $tls_dir"
}

# watch_self brings the config in step with ARIES_TELEMETRY, which may
# differ from the start that wrote it: on, ariesd's listener and the
# stand-ins' key are named and the key's directory is made; off, they
# are not named. The key is never removed here: deleting it makes every
# earlier stand-in unlinkable, which is for whoever runs the server to
# choose.
watch_self() {
  local has=0
  if grep -q '^  listen: ' "$config"; then
    has=1
  fi
  if telemetry_on; then
    install -d -o aries -g aries -m 0700 "$state/telemetry"
    if ((has)); then
      return 0
    fi
  elif ((!has)); then
    return 0
  fi
  write_config "$(setting project)" "$(setting admins | tr -d '[]')"
  if telemetry_on; then
    log "ariesd serves /metrics, /healthz and /readyz on $telemetry_listen, and pushes metrics, traces and log records to $telemetry_endpoint"
  else
    log "ariesd's telemetry is off: ARIES_TELEMETRY is not set"
  fi
}

prepare() {
  rm -f "$ready" /run/sshd.pid
  # A volume an image from before Postgres made holds that database's
  # files, and a config whose dsn this ariesd refuses.
  if [[ -f $config && $(setting dsn) != postgres://* ]]; then
    die "this volume was made by an image from before Postgres, whose data this server cannot read" \
      "remove the volume (docker compose down -v) and start again"
  fi
  install -d -o root -g aries -m 0750 "$state/aries" "$db_files"
  install -d -o aries -g aries -m 0700 "$state/ssh" /run/aries
  install -d -o root -g root -m 0700 "$state/hostkeys"
  install -d -m 0755 /run/sshd
  if [[ ! -f $hostkey ]]; then
    ssh-keygen -q -t ed25519 -N '' -C aries-docker -f "$hostkey"
  fi
}

# install_pgpass writes the password file ariesd's DSN names, from the
# password the postgres container generated, as Postgres reads one: for
# any host, port, database and role, with \ and : escaped. The password
# goes from file to file and is never an argument or printed.
install_pgpass() {
  [[ -s $db_secret ]] || die "the postgres container left no password for ariesd's role in $db_secret" \
    "docker compose logs postgres; a postgres volume made by an earlier failed start is removed with docker compose down -v"
  {
    printf '*:*:*:*:'
    sed -n '1{s/\\/\\\\/g;s/:/\\:/g;p;}' "$db_secret"
  } | install -o aries -g aries -m 0600 /dev/stdin "$pgpass"
}

# adopt is "Opt in" step 2, run on every start because it is safe to run
# again: it makes the control schema, records the DSN's schema as the
# first project and imports the principals and keys of authorized_keys,
# adding what is new. It removes nothing: a principal dropped from
# ARIES_KEYS keeps its row and keys in the control store, and is removed
# with aries admin. And it undoes nothing an admin did with aries admin:
# a member removed from the first project, a key removed and a principal
# disabled stay so across a start, each named in this log, though their
# lines are still in ARIES_KEYS. It is never run here with
# --restore-removed-access, which overrides those admins;
# docs/deploy/docker.md says how to.
adopt() {
  if [[ ! -f $authorized ]]; then
    install -o aries -g aries -m 0600 /dev/null "$authorized"
  fi
  (run_as aries /var/lib/aries ariesd control init --name "$first_project" >&2) ||
    die "ariesd control init failed" "read the error above"
}

# parse_principals reads ARIES_KEYS and /etc/aries-keys, one
# "NAME TYPE KEY [COMMENT]" per line, into key_lines and admins
# (ARIES_ADMINS, or else the first name). It runs before anything is
# written, so a bad line leaves the volume as it was.
key_lines=()
admins=()
parse_principals() {
  local name type blob rest principals=() a
  while read -r name type blob rest; do
    [[ -z $name || $name == \#* ]] && continue
    [[ $name =~ $name_re ]] ||
      die "principal name '$name' is not valid" "use lowercase letters, digits, '.', '_' or '-', starting with a letter"
    case $name in ariesd | import | ariesd-upgrade)
      die "principal name '$name' is reserved" "choose another name" ;;
    esac
    if ! [[ $type =~ $type_re && $blob =~ $blob_re ]] ||
      ! printf '%s %s\n' "$type" "$blob" | ssh-keygen -lf /dev/stdin >/dev/null 2>&1; then
      die "the key for '$name' is not an SSH public key" "give lines like: alice ssh-ed25519 AAAA..."
    fi
    key_lines+=("restrict,command=\"/usr/local/bin/ariesd stdio --principal $name\" $type $blob $name")
    [[ " ${principals[*]} " == *" $name "* ]] || principals+=("$name")
  done < <(
    printf '%s\n' "${ARIES_KEYS:-}"
    if [[ -r $keys_file ]]; then cat "$keys_file"; fi
  )
  if ((${#key_lines[@]} == 0)); then
    return 0
  fi
  IFS=', ' read -r -a admins <<<"${ARIES_ADMINS:-${principals[0]}}"
  for a in "${admins[@]}"; do
    [[ $a =~ $name_re ]] || die "admin name '$a' is not valid" "list principal names in ARIES_ADMINS, comma-separated"
  done
}

# install_principals writes authorized_keys and the config's admins from
# what parse_principals read. Given no keys, it keeps what an earlier
# start installed.
install_principals() {
  if ((${#key_lines[@]} == 0)); then
    return 0
  fi
  printf '%s\n' "${key_lines[@]}" | install -o aries -g aries -m 0600 /dev/stdin "$authorized"
  local quoted
  quoted=$(printf '"%s", ' "${admins[@]}")
  write_config "$(setting project)" "${quoted%, }"
}

report() {
  local fpr project admins principals
  fpr=$(ssh-keygen -lf "$hostkey.pub" | cut -d' ' -f2)
  project=$(setting project)
  admins=$(setting admins | tr -d '[]"')
  principals=$( (sed -n 's/.*--principal \([^"]*\)".*/\1/p' "$authorized" 2>/dev/null || true) | sort -u | paste -sd, - | sed 's/,/, /g')
  if [[ -z $principals ]]; then
    log "no principals in authorized_keys, so only those an admin added with aries admin can connect"
    log "fix: set ARIES_KEYS to \"alice \$(cat ~/.ssh/id_ed25519.pub)\" and start again"
  fi
  local transports=https ssh_lines=''
  if ssh_on; then
    transports='https, ssh'
    ssh_lines=$(printf '  host: localhost\n  port: %s\n  host_key: %s' "$port" "$fpr")
  fi
  cat <<EOF

Aries test server ready (not for production)
  transports  $transports
  url         https://localhost:$https_port
  host key    $fpr
  project     $project
  principals  ${principals:-none}
  admins      ${admins}
  more        aries admin project add NAME; aries admin principal add NAME --key FILE

Save this as .aries.yaml in a scratch repository:

project: $project
server:
  url: https://localhost:$https_port
  ca: |
$(sed 's/^/    /' "$tls_ca")
${ssh_lines}

EOF
}

main() {
  trap 'stop; exit 0' TERM INT
  parse_principals
  if [[ -n ${ARIES_PROJECT:-} && ! ${ARIES_PROJECT} =~ $uuid_re ]]; then
    die "ARIES_PROJECT is not a lowercase UUID" "unset it for a random one, or give one like 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
  fi
  if [[ -n ${ARIES_TELEMETRY:-} ]] && ! telemetry_on; then
    die "ARIES_TELEMETRY is '${ARIES_TELEMETRY}'" "set it to 1 for ariesd's telemetry, or leave it unset for none"
  fi
  if [[ -n ${ARIES_IDENTITY:-} ]] && ! identity_on; then
    die "ARIES_IDENTITY is '${ARIES_IDENTITY}'" "set it to 1 to sign people in at the identity profile's Keycloak, or leave it unset"
  fi
  if [[ -n ${ARIES_SSH:-} ]] && ! ssh_on; then
    die "ARIES_SSH is '${ARIES_SSH}'" "set it to 1 to serve SSH beside HTTPS, or leave it unset for HTTPS alone"
  fi
  [[ $https_port =~ ^[1-9][0-9]{0,4}$ ]] || die "ARIES_HTTPS_PORT is '$https_port'" "set it to the port of this machine the HTTPS listener is published on"
  prepare
  install_pgpass
  wait_db

  if [[ ! -f $config ]]; then
    local project=${ARIES_PROJECT:-$(cat /proc/sys/kernel/random/uuid)}
    write_config "$project" ""
    log "created project $project, served from schema public of the database aries, with the control schema $control_schema"
  elif [[ -n ${ARIES_PROJECT:-} && ${ARIES_PROJECT} != "$(setting project)" ]]; then
    die "ARIES_PROJECT is not the project this volume holds, $(setting project)" \
      "unset ARIES_PROJECT, or remove the volumes (docker compose down -v) for a new project"
  fi
  watch_self
  install_principals
  # The transports, the URL and the sign-in follow ARIES_SSH,
  # ARIES_HTTPS_PORT and ARIES_IDENTITY on every start, which may differ
  # from the start that wrote the file.
  setup_login
  write_config "$(setting project)" "$(setting admins | tr -d '[]')"
  make_tls
  adopt

  if telemetry_on; then
    start ariesd run_as aries /var/lib/aries env "${telemetry_env[@]}" ariesd serve
  else
    start ariesd run_as aries /var/lib/aries ariesd serve
  fi
  wait_for ariesd test -S /run/aries/ariesd.sock
  wait_for ariesd port_open 127.0.0.1 8443

  if ssh_on; then
    /usr/sbin/sshd -t || die "sshd refuses its config" "read the error above"
    start sshd env -i PATH=/usr/sbin:/usr/bin:/bin /usr/sbin/sshd -D -e
    # sshd writes its pid file once it listens; probing the port would
    # log a dropped connection.
    wait_for sshd test -s /run/sshd.pid
  else
    log "SSH is off: sshd does not run; ARIES_SSH=1 turns it on"
  fi

  report
  touch "$ready"

  local done_pid status=0 i
  wait -n -p done_pid "${pids[@]}" || status=$?
  for i in "${!pids[@]}"; do
    if [[ ${pids[i]} == "$done_pid" ]]; then
      log "${names[i]} exited with status $status; stopping"
    fi
  done
  stop
  exit 1
}

main "$@"
