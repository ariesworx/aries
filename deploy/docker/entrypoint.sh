#!/usr/bin/env bash
# The Aries test server's entrypoint (deploy/docker/Dockerfile), doing
# what docs/server.md's steps 2 to 6 do on a machine, and then what its
# "Serving many projects" does: the server holds many projects, and sshd
# reads the keys of its control database from a file ariesd writes,
# /state/ssh/aries_keys, beside authorized_keys.
#
# On the first start it makes the sshd host key, gives Dolt's aries
# account a random password, which only the 0600 config file holds, and
# writes the config. Then, once, it creates the control database, grants
# a block of database names for new projects, and names both in the
# config. On every start it installs the principals given in
# ARIES_KEYS or /etc/aries-keys, runs `ariesd control init`, which
# adopts the project and imports those principals and keys, runs dolt,
# ariesd and sshd, and prints the .aries.yaml a client needs. When
# one of the three exits, it stops the other two and exits 1.
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
# The server of many projects: its control database, the name the
# project it starts with is adopted under, and the block of database
# names `aries admin project add` takes from.
control_db=aries_control
first_project=first
project_dbs_prefix=aries_p
project_dbs_count=8
# Dolt's metrics endpoint, as /etc/dolt/config.yaml sets it: loopback
# only, since it has no authentication.
dolt_metrics_url=http://127.0.0.1:11228/metrics
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

# stop ends the children, last started first, so ariesd closes the store
# before Dolt goes away.
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
# units in docs/server.md do. Call it in a subshell or in the background,
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

# Called through wait_for, which shellcheck cannot see: 0.11 reports
# that as SC2329, and the 0.9 on CI's runner as SC2317.
# shellcheck disable=SC2317,SC2329
port_open() { (: <"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }

random_hex() { od -An -N24 -tx1 /dev/urandom | tr -d ' \n'; }

setting() { sed -n "s/^$1: //p" "$config"; }

# write_config DSN PROJECT ADMINS [many] writes the config file, mode
# 0600, from stdin so the password is never an argument. With many, or
# when the config it replaces names a control database, it also names
# the control database and the block of project databases (opt_in). It
# always names Dolt's metrics endpoint, which the image's Dolt config
# turns on, so that `aries admin status` shows Dolt's figures; and, with
# ARIES_TELEMETRY=1, ariesd's own listener and the stand-ins' key.
write_config() {
  local many=${4:-}
  if [[ -f $config && -n $(setting control_db) ]]; then
    many=many
  fi
  {
    printf 'dsn: %s\nproject: %s\nadmins: [%s]\n' "$1" "$2" "$3"
    if [[ -n $many ]]; then
      printf 'control_db: %s\nproject_dbs:\n  prefix: %s\n  count: %s\n' "$control_db" "$project_dbs_prefix" "$project_dbs_count"
    fi
    printf 'telemetry:\n  dolt_metrics_url: %s\n' "$dolt_metrics_url"
    if telemetry_on; then
      printf '  listen: %s\n  principals_key: %s\n' "$telemetry_listen" "$telemetry_key"
    fi
  } | install -o aries -g aries -m 0600 /dev/stdin "$config"
}

# watch_dolt moves a volume that an image from before Dolt's metrics
# made: its config does not say where ariesd reads them. Dolt's own
# config is the image's, so Dolt serves them from this start on.
watch_dolt() {
  if grep -q '^  dolt_metrics_url: ' "$config"; then
    return 0
  fi
  write_config "$(setting dsn)" "$(setting project)" "$(setting admins | tr -d '[]')"
  log "ariesd now reads Dolt's metrics at $dolt_metrics_url, for aries admin status"
}

telemetry_on() { [[ ${ARIES_TELEMETRY:-} == 1 ]]; }

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
  write_config "$(setting dsn)" "$(setting project)" "$(setting admins | tr -d '[]')"
  if telemetry_on; then
    log "ariesd serves /metrics, /healthz and /readyz on $telemetry_listen, and pushes metrics, traces and log records to $telemetry_endpoint"
  else
    log "ariesd's telemetry is off: ARIES_TELEMETRY is not set"
  fi
}

prepare() {
  rm -f "$ready" /run/sshd.pid
  install -d -o dolt -g dolt -m 0700 "$state/dolt" "$state/dolt/data" "$state/dolt/cfg"
  install -d -o root -g aries -m 0750 "$state/aries"
  install -d -o aries -g aries -m 0700 "$state/ssh" /run/aries
  install -d -o root -g root -m 0700 "$state/hostkeys"
  install -d -m 0755 /run/sshd
  if [[ ! -f $hostkey ]]; then
    ssh-keygen -q -t ed25519 -N '' -C aries-docker -f "$hostkey"
  fi
}

# many_sql prints the SQL of "Serving many projects, Opt in", steps 1 and
# 4: the control database, and a block of database names for the
# projects `aries admin project add` creates, each granted to ariesd's
# account by its exact name, since Dolt ignores a grant on a pattern.
many_sql() {
  local i
  printf "CREATE DATABASE IF NOT EXISTS %s;\nGRANT ALL ON %s.* TO 'aries'@'localhost';\n" "$control_db" "$control_db"
  for ((i = 1; i <= project_dbs_count; i++)); do
    printf "GRANT ALL ON %s%03d.* TO 'aries'@'localhost';\n" "$project_dbs_prefix" "$i"
  done
}

# init_store is step 4: the database, an account with rights on it alone,
# a random password for Dolt's root that nobody keeps, and the config.
# The control database and the block of project databases are made in
# the same pass, before root's password is set and forgotten.
init_store() {
  if [[ -d $state/dolt/data/aries ]]; then
    die "the volume holds a database but no config, from a first start that failed" \
      "remove the volume (docker compose down -v) and start again"
  fi
  local dbpw rootpw project=${ARIES_PROJECT:-$(cat /proc/sys/kernel/random/uuid)}
  dbpw=$(random_hex)
  rootpw=$(random_hex)
  {
    printf "CREATE DATABASE aries;\nCREATE USER 'aries'@'localhost' IDENTIFIED BY '%s';\nGRANT ALL ON aries.* TO 'aries'@'localhost';\n" "$dbpw"
    many_sql
    printf "ALTER USER 'root'@'localhost' IDENTIFIED BY '%s';\n" "$rootpw"
  } |
    (run_as dolt /var/lib/dolt env DOLT_CLI_PASSWORD= dolt --host 127.0.0.1 --port 3306 --no-tls -u root sql >/dev/null) ||
    die "could not create the databases and the account" "read Dolt's log above"
  write_config "aries:$dbpw@tcp(127.0.0.1:3306)/aries" "$project" "" many
  log "created project $project, the control database $control_db and $project_dbs_count database names for new projects"
}

# opt_in moves a volume that an image from before many projects made: its
# config names no control database. Nobody kept the password of Dolt's
# root, so the SQL is run as the server guide runs it on a machine: by
# Dolt's own user, from the data directory. A new volume never comes
# here: init_store did the same as root.
opt_in() {
  if [[ -n $(setting control_db) ]]; then
    return 0
  fi
  many_sql | (run_as dolt /var/lib/dolt/data/aries dolt sql >/dev/null) ||
    die "could not create the control database on a volume from an older image" \
      "read Dolt's log above, or remove the volume (docker compose down -v) and start again"
  write_config "$(setting dsn)" "$(setting project)" "$(setting admins | tr -d '[]')" many
  log "this volume now serves many projects: control database $control_db, $project_dbs_count database names for new projects"
}

# adopt is "Opt in" step 3, run on every start because it is safe to run
# again: it records the database as the first project and imports the
# principals and keys of authorized_keys, adding what is new. It removes
# nothing: a principal dropped from ARIES_KEYS keeps its row and keys
# in the control database, and is removed with aries admin. And it undoes
# nothing an admin did with aries admin: a member removed from the first
# project, a key removed and a principal disabled stay so across a
# start, each named in this log, though their lines are still in
# ARIES_KEYS. It is never run here with --restore-removed-access,
# which overrides those admins; docs/deploy/docker.md says how to.
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
  write_config "$(setting dsn)" "$(setting project)" "${quoted%, }"
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
  cat <<EOF

Aries test server ready (not for production)
  host key    $fpr
  project     $project
  principals  ${principals:-none}
  admins      ${admins}
  more        aries admin project add NAME ($project_dbs_count database names granted); aries admin principal add NAME --key FILE

Save this as .aries.yaml in a scratch repository:

project: $project
server:
  host: localhost
  port: $port
  host_key: $fpr

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
  prepare

  local first=0
  [[ -f $config ]] || first=1
  if ((first)); then
    (run_as dolt /var/lib/dolt sh -c 'dolt config --global --add user.name "aries server" &&
      dolt config --global --add user.email aries@localhost &&
      dolt config --global --add metrics.disabled true' >/dev/null)
  fi
  start dolt run_as dolt /var/lib/dolt/data dolt sql-server --config /etc/dolt/config.yaml
  wait_for dolt port_open 3306
  if ((first)); then
    init_store
  elif [[ -n ${ARIES_PROJECT:-} && ${ARIES_PROJECT} != "$(setting project)" ]]; then
    die "ARIES_PROJECT is not the project this volume holds, $(setting project)" \
      "unset ARIES_PROJECT, or remove the volume (docker compose down -v) for a new project"
  fi
  opt_in
  watch_dolt
  watch_self
  install_principals
  adopt

  if telemetry_on; then
    start ariesd run_as aries /var/lib/aries env "${telemetry_env[@]}" ariesd serve
  else
    start ariesd run_as aries /var/lib/aries ariesd serve
  fi
  wait_for ariesd test -S /run/aries/ariesd.sock

  /usr/sbin/sshd -t || die "sshd refuses its config" "read the error above"
  start sshd env -i PATH=/usr/sbin:/usr/bin:/bin /usr/sbin/sshd -D -e
  # sshd writes its pid file once it listens; probing the port would log a
  # dropped connection.
  wait_for sshd test -s /run/sshd.pid

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
