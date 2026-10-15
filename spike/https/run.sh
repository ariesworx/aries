#!/bin/sh
# Runs the HTTPS transport spike (RESULTS.md) on a Docker network with
# no route out: Keycloak with the spike's internal CA, the stand-in
# ariesd, nginx in front of it, an sshd for today's path, and the
# client, each in a throwaway container. Images are pinned by digest.
# Everything it creates is removed when it ends, images it pulled
# included; images that were already there are kept.
#
#   spike/https/run.sh STATE_DIR
#
# STATE_DIR must not exist. It is made mode 0700 and holds the CA and
# its keys, the generated passwords, the client secrets and the
# signed-in token (each 0600 but Keycloak's own certificate key, which
# its container must read); nothing of them is printed. Delete it
# afterward. Needs docker and go on PATH. Building the sshd image is the
# one step that reaches the internet (Ubuntu's archive), before the
# offline network exists.
set -eu

if [ $# -ne 1 ]; then
    echo "usage: $0 STATE_DIR" >&2
    exit 2
fi
state=$1
if [ -e "$state" ]; then
    echo "$state exists; name a new directory" >&2
    exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
umask 077
mkdir -p "$state/ca" "$state/kc"
state=$(cd "$state" && pwd)

keycloak=quay.io/keycloak/keycloak@sha256:79c623a22abacf3fe6c00a5fb30f5b7843d717b234033b80995891c06116d244 # 26.8.1
nginx=nginx@sha256:970364228b1e4f932abc22d321932816782cca8a20b85a77552a7cfe9acfb3b5 # 1.30.6, stable-alpine
ubuntu=ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55 # 24.04
net=aries-https-spike
sshd_image=aries-https-spike-sshd:local
containers="ahs-keycloak ahs-aries ahs-nginx ahs-sshd ahs-client"
pulled=""

have() { docker image inspect "$1" >/dev/null 2>&1; }

cleanup() {
    for c in $containers; do docker rm -f -v "$c" >/dev/null 2>&1 || true; done
    docker network rm "$net" >/dev/null 2>&1 || true
    docker rmi "$sshd_image" >/dev/null 2>&1 || true
    for img in $pulled; do docker rmi "$img" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for img in "$keycloak" "$nginx" "$ubuntu"; do
    if ! have "$img"; then
        pulled="$pulled $img"
        docker pull -q "$img" >/dev/null
    fi
done

echo "== build"
arch=$(docker version --format '{{.Server.Arch}}')
(cd "$here" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -o "$state/spike" ./cmd/spike)
(cd "$here" && go build -o "$state/spike-host" ./cmd/spike)
"$state/spike-host" ca -dir "$state/ca"
cp "$state/ca/keycloak.internal.pem" "$state/kc/tls.pem"
cp "$state/ca/keycloak.internal.key" "$state/kc/tls.key"
chmod 0755 "$state/kc"
chmod 0644 "$state/kc/tls.pem" "$state/kc/tls.key" # Keycloak's container user must read it
chmod 0755 "$state"
docker build -q --build-arg "BASE=$ubuntu" -t "$sshd_image" "$here/../identity/sshd" >/dev/null

echo "== an internal network, with no route out"
docker network create --internal "$net" >/dev/null
docker network inspect "$net" --format 'network {{.Name}}: internal={{.Internal}}'

run_on_net() { # NAME ALIAS IMAGE [docker args…] -- [command…]
    name=$1 alias=$2
    shift 2
    docker run -d --name "$name" --network "$net" --network-alias "$alias" "$@" >/dev/null
}

echo "== Keycloak, on HTTPS with the internal CA's certificate"
head -c 24 /dev/urandom | base64 >"$state/admin.pass"
KC_BOOTSTRAP_ADMIN_USERNAME="admin"
KC_BOOTSTRAP_ADMIN_PASSWORD=$(cat "$state/admin.pass")
export KC_BOOTSTRAP_ADMIN_USERNAME KC_BOOTSTRAP_ADMIN_PASSWORD
run_on_net ahs-keycloak keycloak.internal -e KC_BOOTSTRAP_ADMIN_USERNAME -e KC_BOOTSTRAP_ADMIN_PASSWORD \
    -v "$state/kc:/certs:ro" "$keycloak" start-dev --http-enabled=false --https-port=8443 \
    --https-certificate-file=/certs/tls.pem --https-certificate-key-file=/certs/tls.key \
    --hostname=https://keycloak.internal:8443
unset KC_BOOTSTRAP_ADMIN_PASSWORD

client() { docker exec ahs-client /state/spike "$@"; }
run_on_net ahs-client client.internal -v "$state:/state" "$ubuntu" sleep infinity

i=0
until docker exec ahs-keycloak bash -c 'exec 3<>/dev/tcp/127.0.0.1/8443' 2>/dev/null; do
    i=$((i + 1))
    if [ "$i" -gt 90 ]; then
        echo "Keycloak did not start" >&2
        docker logs ahs-keycloak 2>&1 | tail -n 20 >&2
        exit 1
    fi
    sleep 2
done
sleep 5
docker exec ahs-keycloak /opt/keycloak/bin/kc.sh --version 2>/dev/null | grep -i keycloak || true

echo "== offline: the client and Keycloak cannot reach the internet"
client egress
docker exec ahs-keycloak timeout 5 bash -c 'exec 3<>/dev/tcp/1.1.1.1/443' 2>/dev/null && echo "Keycloak REACHED 1.1.1.1:443" || echo "Keycloak: 1.1.1.1:443 unreachable"

echo "== setup, and the key set exported as an operator would"
client setup -kc https://keycloak.internal:8443 -ca /state/ca/ca.pem -admin-pass-file /state/admin.pass -state /state \
    -aries https://aries.internal:8443,https://aries.internal:8444
client jwks -ca /state/ca/ca.pem -out /state/kc-jwks.json

echo "== the stand-in ariesd: 8443 discovers the provider's keys at its internal URL; 8444 reads them from a file and its sign-ins last 15 s"
run_on_net ahs-aries aries.internal -v "$state:/state:ro" "$ubuntu" /bin/sh -c '
    /state/spike serve -addr :8443 -base https://aries.internal:8443 -cert /state/ca/aries.internal.pem -key /state/ca/aries.internal.key \
        -ca /state/ca/ca.pem -crl /state/ca/ca.crl -client-secret-file /state/aries-server.secret -also https://proxy.internal &
    /state/spike serve -addr :8444 -base https://aries.internal:8444 -cert /state/ca/aries.internal.pem -key /state/ca/aries.internal.key \
        -ca /state/ca/ca.pem -crl /state/ca/ca.crl -client-secret-file /state/aries-server.secret \
        -jwks-file /state/kc-jwks.json -day 15s &
    wait'
run_on_net ahs-nginx proxy.internal -v "$here/nginx/default.conf:/etc/nginx/conf.d/default.conf:ro" -v "$state/ca:/certs:ro" "$nginx"
run_on_net ahs-sshd sshd.internal "$sshd_image"
sleep 3
docker exec -i -u aries ahs-sshd sh -c 'umask 077; cat > /var/lib/aries/.ssh/aries_keys' <"$state/ca/aries_keys"
docker exec ahs-aries /state/spike egress || true

echo "== aries login: alice, and what the server must refuse"
client login -aries https://aries.internal:8443 -ca /state/ca/ca.pem -state /state || echo "login: FAILED"
echo "== aries login: bob, who has no invitation"
client login -aries https://aries.internal:8443 -ca /state/ca/ca.pem -state /state -user bob || echo "login bob: FAILED"
echo "== a workload's token exchanged"
client workload -ca /state/ca/ca.pem -state /state || echo "workload: FAILED"
echo "== a remote MCP client through the MCP authorization flow"
client mcp -ca /state/ca/ca.pem -state /state || echo "mcp: FAILED"
echo "== the measurements"
client login -aries https://aries.internal:8443 -ca /state/ca/ca.pem -state /state -only || echo "login: FAILED"
client bench -ca /state/ca/ca.pem -state /state -ssh sshd.internal:22 -ssh-key /state/ca/ssh.key -idle 75s || echo "bench: FAILED"
echo "== keys from a file, and a sign-in that lasts 15 s: the connection is closed at its end"
client login -aries https://aries.internal:8444 -ca /state/ca/ca.pem -state /state -expiry || echo "expiry: FAILED"
echo "== the servers' log"
docker logs ahs-aries 2>&1 | sed 's/^[0-9/]* [0-9:]* //' | sort | uniq -c | sort -rn | head -n 40
echo "== done"
