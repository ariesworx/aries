#!/bin/sh
# Runs the identity spike end to end (RESULTS.md): Keycloak and sshd in
# throwaway containers, images pinned by digest, and the spike's Go
# client against them. Everything it creates is removed when it ends,
# images it pulled included; images that were already there are kept.
#
#   spike/identity/run.sh STATE_DIR
#
# STATE_DIR must not exist. It is made mode 0700 and holds the generated
# passwords, the client secret and the keys, each 0600; nothing of them
# is printed. Delete it afterward. Needs docker and go on PATH.
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
mkdir -p "$state"
state=$(cd "$state" && pwd)

keycloak=quay.io/keycloak/keycloak@sha256:79c623a22abacf3fe6c00a5fb30f5b7843d717b234033b80995891c06116d244 # 26.8.1
ubuntu_2404=ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
ubuntu_2604=ubuntu@sha256:f144425ff09be612d6d9ad965196e9cdc23dae1f42110a8a11a3e9a8198759f7
kc=aries-spike-kc
sshd=aries-spike-sshd
kcport=18080
sshport=12222
pulled=""

have() { docker image inspect "$1" >/dev/null 2>&1; }

cleanup() {
    docker rm -f -v "$kc" "$sshd" >/dev/null 2>&1 || true
    docker rmi aries-spike-sshd:2404 aries-spike-sshd:2604 >/dev/null 2>&1 || true
    for img in $pulled; do
        docker rmi "$img" >/dev/null 2>&1 || true
    done
}
trap cleanup EXIT
trap 'exit 130' INT TERM

for img in "$keycloak" "$ubuntu_2404" "$ubuntu_2604"; do
    if ! have "$img"; then
        pulled="$pulled $img"
        docker pull -q "$img" >/dev/null
    fi
done

echo "== build the spike"
(cd "$here" && go build -o "$state/spike" ./cmd/spike)
spike=$state/spike

echo "== Keycloak"
head -c 24 /dev/urandom | base64 >"$state/admin.pass"
KC_BOOTSTRAP_ADMIN_USERNAME="admin"
KC_BOOTSTRAP_ADMIN_PASSWORD=$(cat "$state/admin.pass")
export KC_BOOTSTRAP_ADMIN_USERNAME KC_BOOTSTRAP_ADMIN_PASSWORD
docker run -d --name "$kc" -p "127.0.0.1:$kcport:8080" \
    -e KC_BOOTSTRAP_ADMIN_USERNAME -e KC_BOOTSTRAP_ADMIN_PASSWORD \
    "$keycloak" start-dev >/dev/null
unset KC_BOOTSTRAP_ADMIN_PASSWORD
i=0
until curl -fsS -o /dev/null "http://127.0.0.1:$kcport/realms/master/.well-known/openid-configuration"; do
    i=$((i + 1))
    if [ "$i" -gt 90 ]; then
        echo "Keycloak did not start" >&2
        exit 1
    fi
    sleep 2
done
docker exec "$kc" /opt/keycloak/bin/kc.sh --version 2>/dev/null | grep -i keycloak || true
"$spike" setup -kc "http://127.0.0.1:$kcport" -admin-pass-file "$state/admin.pass" -state "$state"

echo "== people: the four ways to finish a sign-in"
for mode in loopback paste oob device; do
    "$spike" login -mode "$mode" -state "$state" || echo "login $mode: FAILED"
done
echo "== people: what must be refused"
if "$spike" login -mode loopback -user bob -state "$state"; then
    echo "login bob: ADMITTED, with no invitation"
fi
if "$spike" login -mode loopback -wrong-verifier -state "$state" 2>&1; then
    echo "login with the wrong PKCE verifier: ADMITTED"
fi

echo "== a service principal"
"$spike" workload -state "$state"

for release in 2404 2604; do
    if [ "$release" = 2404 ]; then base=$ubuntu_2404; else base=$ubuntu_2604; fi
    echo "== sshd on Ubuntu $release"
    docker build -q --build-arg "BASE=$base" -t "aries-spike-sshd:$release" "$here/sshd" >/dev/null
    for zone in UTC Pacific/Kiritimati Pacific/Pago_Pago; do
        docker run -d --name "$sshd" -e "ZONE=$zone" -p "127.0.0.1:$sshport:22" "aries-spike-sshd:$release" >/dev/null
        i=0
        until docker exec "$sshd" test -e /run/sshd.pid; do
            i=$((i + 1))
            if [ "$i" -gt 30 ]; then
                echo "sshd did not start" >&2
                exit 1
            fi
            sleep 1
        done
        dir="$state/ssh-$release-$(echo "$zone" | tr / -)"
        mkdir "$dir"
        extra=""
        if [ "$zone" = UTC ]; then extra="$state/login-loopback.line"; fi
        "$spike" sshkeys -dir "$dir" -zone "$zone" ${extra:+-extra "$extra"}
        docker exec -i -u aries "$sshd" sh -c 'umask 077; cat > /var/lib/aries/.ssh/aries_keys' <"$dir/aries_keys"
        "$spike" probe -dir "$dir" -addr "127.0.0.1:$sshport" -login-key "$state/login-loopback.key" || echo "probe: FAILED"
        if [ "$zone" = UTC ]; then
            "$spike" hold -dir "$dir" -addr "127.0.0.1:$sshport" || echo "hold: FAILED"
        fi
        echo "-- sshd's log for refused keys:"
        docker logs "$sshd" 2>&1 | grep -iE 'expired|expiry|refused|not allowed' | sed 's/SHA256:[A-Za-z0-9+\/]*/SHA256:…/g' | sort | uniq -c | head -n 8
        docker rm -f -v "$sshd" >/dev/null
    done
done
echo "== done"
