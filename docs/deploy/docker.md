# Docker test server

**For** trying Aries, or testing a branch, against a real `ariesd`
from a Mac or any machine with Docker, without touching a real server.

**Not for production.** It keeps no backups, and the
[deployment examples](README.md) are how to run Aries for real.

```text
your machine
  aries ──HTTPS──▶ 127.0.0.1:8443 ──▶ container aries: ariesd ──▶ container postgres
  aries ──SSH────▶ 127.0.0.1:2222 ──▶   sshd ─┘ (ARIES_SSH=1)    └── volume: the database
                                       └── volume: the config, the test CA, the host key
```

Two containers. One runs ariesd, set up as in
[Running a server](../server.md#set-up-a-server): ariesd as `aries`,
serving HTTPS as a fresh server does ([The HTTPS
listener](../server.md#the-https-listener)), with a certificate for
`localhost` and `127.0.0.1` from a test CA the container makes on its
first start; and, with `ARIES_SSH=1`, sshd with a forced command per
key ([SSH on and off](../server.md#ssh-on-and-off)). The other runs Postgres, from
Docker's official `postgres:18` image, which ariesd reaches over the
compose network as a Postgres on another machine: its role and database
are made with step 2's statements, the role with a generated password
that only 0600 files hold, as for
[a managed service](../server.md#managed-services). ariesd runs
without `--dev`, so it makes every check it makes on a real server,
its database role's included. The files are in
[deploy/docker](../../deploy/docker).

It is a server of [many projects](../server.md#serving-many-projects):
it has a control store, ariesd makes each new project's schema, the
project it starts with is adopted under the name `first`, and, with
SSH on, sshd reads the keys file ariesd writes
([Keys from the store](../server.md#keys-from-the-store)). The keys of
the control store sign in over HTTPS too
([How aries signs in over HTTPS](../server.md#how-aries-signs-in-over-https)). So you can
try `aries admin project add`, and add a person with `aries admin principal
add NAME --key FILE` and no restart.

[Troubleshooting](#troubleshooting) has what to do when a build fails
or a key is refused.

## What you need

- **Docker Desktop or OrbStack** on a Mac, Apple silicon or Intel, or
  Docker Engine 25 or later with Compose v2 on Linux. On Apple silicon the
  image is built for arm64, so nothing is emulated. With Docker Desktop,
  `~/.docker/bin` must be on your `PATH`
  ([Troubleshooting](#troubleshooting)).
- **Go**, at the `go` line in [go.mod](../../go.mod), to build `aries` from
  the same checkout, so client and server speak the same protocol.
- **An SSH key**, such as `~/.ssh/id_ed25519`. If it has a passphrase,
  load it into ssh-agent (`ssh-add -l` lists it).

## Start it

Run everything from the repository root, on the branch you want to test.
`COMPOSE_FILE` saves typing the file in every command:

```sh
export COMPOSE_FILE=deploy/docker/compose.yaml
export ARIES_KEYS="alice $(cat ~/.ssh/id_ed25519.pub)"
docker compose up -d --build --wait
docker compose logs --no-log-prefix aries | awk '/test server ready/ { out = "" } { out = out $0 "\n" } END { printf "%s", out }'
```

The build compiles `ariesd` and `aries` from your checkout, and the
first start pulls Postgres's image, pinned by digest. On that start the
Postgres container makes ariesd's role, with a generated password, and
its database, with collation `C`; the server's container makes the host
key and the project, and writes the password into the file its DSN
names, `/etc/aries/postgres/pgpass` (mode 0600), so the config file
holds none and nothing prints it. Every start then runs `ariesd control
init`, which makes the control store and records the project and the
principals and keys you gave. The log ends with what a
client needs (these values are invented):

```text
Aries test server ready (not for production)
  transports  https
  url         https://localhost:8443
  host key    SHA256:…
  project     6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
  principals  alice
  admins      alice
  more        aries admin project add NAME; aries admin principal add NAME --pubkey FILE

Save this as .aries.yaml in a scratch repository:

project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  url: https://localhost:8443
  ca: |
    -----BEGIN CERTIFICATE-----
    …
    -----END CERTIFICATE-----
```

With `ARIES_SSH=1` the block also has `host: localhost`, `port: 2222`
and `host_key: SHA256:…`, and `aries` uses SSH when `ARIES_TRANSPORT=ssh`.
Later starts reuse the volume, so the project, the test CA, the host
key and the data stay the same, and print the block again; the command
above shows the newest. The container made the CA and the host key on
your own machine, so there is nothing to verify out of band. The CA is
a test CA, trusted by this checkout's `server.ca` alone; its key stays
on the volume.

| Variable | Default | Sets |
|---|---|---|
| `ARIES_KEYS` | none | The principals: one `NAME TYPE KEY` line per key, such as `alice ssh-ed25519 AAAA…`. A file mounted at `/etc/aries-keys` adds lines. A start that gives keys rewrites `authorized_keys` with them, and one that gives none keeps the file. Each start also copies them into the control database (`ariesd control init`), where they stay: dropping a name here does not remove it, so remove it with `aries admin principal disable NAME` or `aries admin key remove FINGERPRINT`. Everyone new, but the admins, becomes a member of the first project. A start does not undo what an admin did with `aries admin`: someone removed from the first project stays removed, a disabled principal stays disabled, and a key removed from the control database stays out of it, each reported in the container's log. One thing it cannot do for you: a key still listed here is still a line of `authorized_keys`, where sshd accepts it whatever the control database holds, so to revoke a key, take it out of `ARIES_KEYS` as well, or disable its principal ([What a restart keeps](#what-a-restart-keeps)) |
| `ARIES_ADMINS` | the first name | The server [admins](../server.md#admins), comma-separated. They reach every project and need no membership |
| `ARIES_PROJECT` | random | The project UUID, used on the first start only |
| `ARIES_HTTPS_PORT` | `8443` | The HTTPS listener's port on 127.0.0.1, which the server's URL names |
| `ARIES_SSH` | unset | `1` serves SSH beside HTTPS: sshd runs, and ariesd writes the keys file it reads. Unset, sshd does not run, ariesd refuses the bridge and writes no keys file, as on a fresh server. With `1`, also add [compose.ssh.yaml](../../deploy/docker/compose.ssh.yaml) to the files, which publishes the port; without it sshd runs unreachable from the host |
| `ARIES_PORT` | `2222` | sshd's port on 127.0.0.1, published by [compose.ssh.yaml](../../deploy/docker/compose.ssh.yaml) with SSH on, and printed in `.aries.yaml`. Nothing is published on it otherwise, so a second stack on the machine does not collide with this one on it |
| `ARIES_TELEMETRY` | unset | `1` turns ariesd's telemetry on ([Telemetry](#telemetry)). Unset, ariesd exports nothing and opens no port, as on a server where nothing is configured |
| `ARIES_TZ` | unset, UTC | The zone sshd and ariesd run in, as `TZ`: a POSIX zone such as `ARIES-14`, fourteen hours east of UTC, since the image holds no zone files. sshd reads a key's `expiry-time` without its `Z` in this zone; the smoke test runs the server at UTC+14 to show the `Z` is written ([Keys that end](../server.md#keys-that-end)) |
| `ARIES_GRAFANA_PORT` | `3000` | Grafana's port on 127.0.0.1, with the `observe` profile ([Observe](#observe)) |
| `ARIES_PROXY` | unset | `1` makes ariesd serve plain HTTP on loopback, for the `proxy` profile's nginx to pass requests to ([Behind a proxy](#behind-a-proxy)). Unset, ariesd serves TLS itself, as before |

Both ports are published on 127.0.0.1 alone, so nothing beyond your
machine can reach them.

With SSH, set `ARIES_SSH=1` and add the override that publishes its
port, so that the default stack publishes no port 22 at all:

```sh
export COMPOSE_FILE=deploy/docker/compose.yaml:deploy/docker/compose.ssh.yaml
ARIES_SSH=1 docker compose up -d --build --wait
```

## Connect a scratch repository

Build `aries` into a scratch repository, and save the printed block as
its `.aries.yaml`; the third command copies the newest one from the
log:

```sh
mkdir -p ~/aries-scratch
go build -o ~/aries-scratch/aries ./cmd/aries
docker compose logs --no-log-prefix aries | awk '/^project:/ { yaml = "" } /^project:/, /^$/ { yaml = yaml $0 "\n" } END { printf "%s", yaml }' > ~/aries-scratch/.aries.yaml
cd ~/aries-scratch && git init -q
./aries ready
./aries create "Try the test server" -p 2 -t task
./aries ready
```

`aries` signs in, over HTTPS as over SSH, with the key `ssh` would use:
a default file in `~/.ssh`, then the keys in ssh-agent. If yours is neither, name its file by its
path: `export ARIES_KEY_FILE=~/keys/mine`. The rest of
[Using `aries`](../cli.md) and the [agent guide](../agents.md) work as on any
server.

**To act as a second principal**, such as a non-admin, make it a key
and add it as alice, the admin. Nothing restarts:

```sh
ssh-keygen -q -t ed25519 -N '' -C bob@example.com -f ~/aries-scratch/bob
cd ~/aries-scratch && ./aries admin principal add bob --pubkey ~/aries-scratch/bob.pub
```

Then run `aries` as bob by naming his key:

```sh
ARIES_KEY_FILE=~/aries-scratch/bob ./aries whoami
ARIES_KEY_FILE=~/aries-scratch/bob ./aries ready
```

A key named this way is the only key `aries` offers, so you are bob even
with your own key, alice's, in `~/.ssh` and in ssh-agent
([Your key is your identity](../cli.md#your-key-is-your-identity)).
`aries whoami` says which principal the server took you for and which key
signed. bob is a member of the first project and no admin: `aries admin
project list` shows bob his `ROLE` in each project, and alice each
project's `SCHEMA`. `./aries admin key remove FINGERPRINT` takes his key
away again, and `./aries admin key list bob` shows the fingerprint.

**To try a second project**, run `./aries admin project add web` as alice.
It prints the `.aries.yaml` for the new project: save it in another
scratch repository, and add bob to it there with `~/aries-scratch/aries
admin member add bob`. bob has a key, which would sign in to the new
project too, so `aries` lists it and asks before it adds him
([Managing access](../server.md#managing-access)). `aries admin status`
then shows the server and a row for each project.

**Postgres's own figures** are in `aries admin status`, as on any
server: its version, its connections and ariesd's pool, the database's
transactions and size, and each project's schema's size
([Watching a server](../server.md#watching-a-server)). Postgres
publishes no port: only the server's container reaches it.

Giving a second key in `ARIES_KEYS` and starting again still works,
as before, and puts the key in `authorized_keys` as well.

### What a restart keeps

The container runs `ariesd control init` at every start, to import
what `ARIES_KEYS` gained. It adds what is new and leaves alone what an
admin did since ([Opt in](../server.md#opt-in)):

| An admin ran | After a restart, with the name still in `ARIES_KEYS` |
|---|---|
| `aries admin member remove NAME` | NAME is still not a member of the first project |
| `aries admin principal disable NAME` | NAME is still disabled, and refused |
| `aries admin key remove FINGERPRINT` | The key is still out of the control database. sshd still accepts it, because the start wrote it to `authorized_keys` again: the removal warned of this. Take the key out of `ARIES_KEYS` too, or disable NAME |

The container's log names each line it left alone, and why. To put back
from `ARIES_KEYS` what admins removed, which overrides them, run it by
hand with the flag for that; the running server takes it up within half
a minute:

```sh
docker compose exec -u aries -w /var/lib/aries aries \
  ariesd control init --name first --restore-removed-access
```

## Telemetry

Off unless you ask. To see what ariesd reports about itself
([Observability](../server.md#observability)), start the server with
its telemetry on and a collector beside it:

```sh
export ARIES_TELEMETRY=1
docker compose --profile telemetry up -d --build --wait
docker compose --profile telemetry logs -f collector
```

| You set | It does |
|---|---|
| `ARIES_TELEMETRY=1` | ariesd serves `/metrics`, `/healthz` and `/readyz` on `127.0.0.1:9464` inside the container, pushes metrics, traces and the records of its log lines ([Logs over OTLP](../server.md#logs-over-otlp)) to `http://127.0.0.1:4318`, and keeps the key for people's stand-ins on the volume (`/state/telemetry/principals.key`), so a rebuild keeps them |
| `--profile telemetry` | Runs an OpenTelemetry Collector with [deploy/observability/collector.yaml](../../deploy/observability/collector.yaml): it receives what ariesd pushes, scrapes ariesd's `/metrics`, and prints every metric, span and log record in full |
| `--profile prometheus` | Runs a Prometheus with [prometheus.yaml](../../deploy/observability/prometheus.yaml) and the alert rules of [alerts.yaml](../../deploy/observability/alerts.yaml) and [alerts-sextant.yaml](../../deploy/observability/alerts-sextant.yaml), scraping ariesd |

- **Both run in the server's network namespace** (`network_mode:
  service:aries`). ariesd's listener answers on loopback alone and has
  no authentication, and ariesd pushes to loopback; a collector in a
  container of its own could reach neither.
  On a real server the collector runs on the machine, for the same
  reason.
- **Neither publishes a port or keeps a volume.** Prometheus keeps its
  data in memory, and has no page you can open from your machine. Ask
  it from inside:

  ```sh
  docker compose --profile prometheus exec prometheus promtool query instant http://127.0.0.1:9090 'aries_stores_open'
  docker compose --profile prometheus exec prometheus promtool query instant http://127.0.0.1:9090 'ALERTS'
  ```

- **The images are pinned by tag and digest** in the compose file
  (`name:tag@sha256:…`; the digest is what Docker pulls): the
  Collector's core distribution at 0.162.0, and Prometheus at 3.15.0.
  Dependabot proposes a new digest when a tag is rebuilt and a new tag
  when one is released, after a cooldown.
- **A trace of your own.** Give `aries` a W3C trace context and look for
  its trace id in the collector's output: the server's span is under
  yours, with the write beneath it and you named by a stand-in, which
  `aries admin principal whois` resolves for a server admin.

  ```sh
  TRACEPARENT=00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01 ./aries create "Traced" -p 3 -t task
  docker compose --profile telemetry logs collector | grep -A 20 0af7651916cd43dd8448eb211c80319c
  ```

- **The audit stream, at the collector.** Change who has access, and
  the change is a log record with `audit: Bool(true)`, naming who made
  it and whom it changed by stand-ins; the same line in the server's own
  log has the names, under the same `seq`.

  ```sh
  ./aries admin principal add erin
  docker compose --profile telemetry logs collector | grep -B 9 -A 9 'audit: Bool(true)'
  docker compose logs aries | grep 'audit=true'
  ```

- **A test server exports sooner than a real one,** and exports its
  log: metrics every 5 seconds, not 60, and a finished span within 1
  second, not 5, so that what you just did is there to look at; and its
  log records, which a real server sends only when asked
  (`OTEL_LOGS_EXPORTER=otlp`).
- **After restarting the server, restart the two beside it.** A restart
  gives the server's container a new network namespace, and the others
  are left in the old one: `docker compose restart aries`, then
  `docker compose --profile telemetry --profile prometheus restart
  collector prometheus`. `docker compose restart` of all three at once
  fails for those two.
- **`ARIES_TELEMETRY=1` without the collector** is a server whose
  collector is down: ariesd warns once for each signal, counts what it
  drops, and serves as before
  ([When the collector is down](../server.md#when-the-collector-is-down)).
- **To turn it off again,** unset the variable and run `docker compose
  up -d`. The key for stand-ins stays on the volume.

ariesd's log still goes to the container's output, whole, as before:
`docker compose logs aries`. What the collector receives of it is
less, by design: ids, codes and numbers, and no text.

### Observe

To look at what ariesd reports over time, in Grafana, run the `observe`
profile instead of the two above:

```sh
export ARIES_TELEMETRY=1
docker compose --profile observe up -d --build --wait
```

Then open <http://127.0.0.1:3000>.

It runs Grafana's all-in-one OpenTelemetry backend, `grafana/otel-lgtm`
(pinned by tag and digest): a Collector that receives what ariesd
pushes, Prometheus for the metrics, Tempo for the traces, Loki for the
log records, and Grafana, with the dashboards of
[deploy/observability](../../deploy/observability): it opens on
**Aries server**, headline numbers above the server's health, and links
to **Aries requests**, each op's traffic, latency and refusals, and
**Aries traces and logs**, recent and slow traces, log events, warnings
and errors, and the audit stream. Explore has each signal: a trace by its id in Tempo
(the `TRACEPARENT` example above works here too), the log records in
Loki by `service_name`, and any `aries_*` series in Prometheus.

- **For developing Aries as much as for running it.** A change to an
  instrument, a span or a log line can be looked at, not only tested:
  start the profile from your branch, use `aries`, and find it.
- **It keeps its data on the volume `observe`**, so a restart or a
  rebuild keeps the history; `docker compose down -v` wipes it with the
  rest.
- **It takes the ports the `telemetry` and `prometheus` profiles take**
  in the server's namespace, so run one or the other, not both.
- **Anyone who reaches Grafana is an admin**, as the image has it. It is
  published on 127.0.0.1 alone (`ARIES_GRAFANA_PORT`, 3000 by default),
  so that is anyone on your machine. Grafana downloads no plugin at
  start, so it reaches nothing outside the machine.
- **It also shows the agents**: the dashboard "Aries agents" reads what
  Claude Code exports under the service name `aries-agents`, with no
  content, once Desktop sessions and `aries dispatch run` are pointed at
  the Collector's `http://127.0.0.1:4318`
  ([Agent metrics](../agents.md#agent-metrics-claude-code), which says
  what is never exported).
- **The alert rules are not loaded here.** They stay with the
  `prometheus` profile and the smoke test.
- **After restarting the server, restart it too**, as for the
  collector: `docker compose --profile observe restart observe`.
- A counter appears in Prometheus once it has counted: a series of the
  dashboard that nothing has caused yet, such as refused upgrades, shows
  no data rather than 0.

## Sign-in with a local identity provider

**For** developing and testing sign-in
([docs/design/identity.md](../design/identity.md)) on your own machine,
with no account at Google, Entra ID, Okta or GitHub and no internet:
Keycloak stands in for them until a final check against the real
providers. With `ARIES_IDENTITY=1` the server signs people in at it
([Sign in at it](#sign-in-at-it), below).

**A test fixture, never production.** Keycloak runs in development mode
over plain HTTP, its database is a file on a volume, and its realm is
made by a script with generated passwords.

```sh
docker compose --profile identity up -d --wait
docker compose --profile identity logs --no-log-prefix keycloak | grep -A 9 'Keycloak ready'
```

The first start pulls Keycloak 26.8.1, the release
[spike/identity](../../spike/identity/RESULTS.md) ran against, pinned
by digest. Once the image is pulled, a start, the first included,
took about half a minute on an Apple silicon Mac. `ARIES_IDENTITY_PORT` sets the port, 8180 by default.

| | |
|---|---|
| Issuer | `http://keycloak.localhost:8180/realms/aries`, the same from a browser on your machine, where `*.localhost` is loopback, and from the server's container, where it is the name of Keycloak's container on the compose network. Keycloak listens on the same port inside and out, so one URL works from both |
| Discovery | `http://keycloak.localhost:8180/realms/aries/.well-known/openid-configuration` |
| Admin console | `http://keycloak.localhost:8180/admin/`, user `admin` |
| People | `alice@example.com` and `bob@example.com` (usernames `alice` and `bob`), addresses verified |
| `aries-cli` | Public, for `aries login`: the code flow with PKCE (S256 required), redirects to `http://127.0.0.1/*` on any port and to Keycloak's own code page (`urn:ietf:wg:oauth:2.0:oob`), the device grant on for testing, ID tokens signed ES256, no refresh tokens |
| `aries-server` | Confidential, standing for the server: the code flow with PKCE, for a code the server exchanges itself, with the server's callback, `https://localhost:ARIES_HTTPS_PORT/auth/callback`, among its redirects; its secret in `aries-server.secret` |
| `ci-example` | A service principal: the client-credentials grant only, its tokens for the audience `aries.example.com`, with made-up claims in the shape of a GitHub Actions token (`repository_id`, `repository`, `ref`, `workflow_ref`) for trust rules to match; its secret in `ci-example.secret` |
| Claims | ID tokens carry `email`, `email_verified`, `nonce` (the one sent) and `auth_time`; the realm has an ES256 key beside the default RS256 one; tokens live five minutes |

**The passwords and secrets.** The first start generates the admin's
password, alice's and bob's, and the two client secrets, and writes
each to a file of its own, mode 0600, in `/opt/keycloak/data/aries`
(mode 0700) on the `identity` volume. None is printed, logged or
passed as an argument. Print one to your own terminal when you need it:

```sh
docker compose --profile identity exec keycloak cat /opt/keycloak/data/aries/alice.pass; echo
```

The files are `admin.pass`, `alice.pass`, `bob.pass`,
`aries-server.secret` and `ci-example.secret`. To see the realm, its
clients and its people, with no secret:

```sh
docker compose --profile identity exec keycloak /opt/aries/identity.sh show
```

- **Made by a script, with no clicks.**
  [identity/identity.sh](../../deploy/docker/identity/identity.sh) is
  the container's entrypoint: it starts Keycloak and, with Keycloak's
  own `kcadm.sh`, makes whatever the realm lacks, on every start. What
  is there is kept as it is, edits in the admin console included. To
  start over, or to pick up a change to the script's realm, remove the
  volume: `docker compose --profile identity down -v`, which removes
  the server's volume too, or `docker compose --profile identity rm -sf
  keycloak` and `docker volume rm aries-dev_identity` for Keycloak's
  alone.
- **A browser on your machine** opens the issuer's pages directly. It
  must resolve `keycloak.localhost` to loopback, and send Keycloak's
  cookies, which are marked `Secure`, to it over HTTP, as browsers do
  for a `localhost` name they treat as secure. A Chromium-based browser
  on macOS did both; others were not tried. macOS's resolver, Go's and
  curl resolve the name too. A tool that does not can be given
  `keycloak.localhost:8180:127.0.0.1` (curl's `--resolve`).
- **The server's container** reaches Keycloak by the same name, and
  with `ARIES_IDENTITY=1` signs people in at it. The server's image is
  unchanged by the profile.
- **Air-gapped.** Nothing Keycloak does needs the internet once its
  image is pulled. To show that, put the compose network out of reach
  with an override file:

  ```yaml
  # airgap.yaml
  networks:
    default:
      internal: true
  ```

  ```sh
  docker compose -f deploy/docker/compose.yaml -f airgap.yaml --profile identity up -d --wait
  ```

  On an internal network neither container can reach anything outside
  it and Docker publishes no port, so you cannot reach either from your
  machine; the server's container still reaches Keycloak by name and
  gets its discovery document, its keys and a client-credentials token
  ([Run the smoke test](#with-the-identity-provider) has what was run).
  On a machine with no network at all, the default network and the
  loopback ports work as they do online.

### Sign in at it

```sh
ARIES_KEYS="alice $(cat ~/.ssh/id_ed25519.pub)" ARIES_IDENTITY=1 docker compose --profile identity up -d --wait
aries admin invite add bob bob@example.com --provider keycloak   # as alice, the server admin
```

With `ARIES_IDENTITY=1` the server's entrypoint waits for Keycloak's
realm, copies the server's client secret from Keycloak's volume, which
the server's container mounts read-only, to `/etc/aries/login`, mode
0600, exports the realm's key set beside it, and writes the provider
`keycloak` into the config ([Sign-in](../server.md#sign-in)), offline:
its endpoints written out and its keys from the file, so that ariesd
fetches nothing to check a token. Then, in a repository whose
`.aries.yaml` names the server, `aries login` signs bob in at Keycloak
through the server: his browser opens Keycloak's page, he signs in with
`bob.pass`, and `aries whoami` says he is signed in. A volume of
Keycloak's from before this release has the server's client without the
server's callback: remove it (`docker compose --profile identity down
-v`) to have it made again.

## Behind a proxy

The `proxy` profile puts nginx in front of ariesd, as a load balancer
that terminates TLS would be, to test what docs/server.md's
[Behind a proxy](../server.md#behind-a-proxy) asks of one: the WebSocket
upgrade on `/v1/connect`, and an idle connection kept for at least 60
seconds, and that the limits per client address are the proxy's own
(`limit_conn` and `limit_req`; `smoke.sh` checks a flood of requests is
answered 429). Start it with `ARIES_PROXY=1` as well, since a container cannot
see its compose profiles:

```bash
ARIES_PROXY=1 docker compose --profile proxy up -d --build --wait
```

nginx ([nginx.conf](../../deploy/docker/nginx.conf)) serves HTTPS on the
port `ARIES_HTTPS_PORT` publishes, with the test CA's certificate, and
passes each request to ariesd's listener on 127.0.0.1:8080, which trusts
127.0.0.1 alone. nginx shares the server's network namespace, as the
telemetry profiles do, so it publishes no port of its own and the
server's URL and `.aries.yaml` do not change. Without `ARIES_PROXY=1`,
nginx cannot bind the port, since ariesd keeps it: start the profile
with the variable, or not at all.

Like the rest of this directory, the profile is a test fixture, never
production: the certificate is the test CA's, and nginx is configured
for the checks above and nothing more. `ARIES_SMOKE_PROXY=1
deploy/docker/smoke.sh` runs the whole smoke test through it (see
[Behind the proxy](#behind-the-proxy) under the smoke test).

## Test a change

After you change the code, rebuild the image and `aries`:

```sh
docker compose up -d --build --wait
go build -o ~/aries-scratch/aries ./cmd/aries
```

The volumes keep their data, and a new migration runs on it when ariesd
starts. An image older than the database's schema refuses to open it
(migrations only go forward): wipe the volumes, below, to go back. A
volume made before the move to Postgres holds data this server cannot
read: its first start says so, and `docker compose down -v` clears it.

### Rebuild a server you work on

A stack can track real work, as this repository's own backlog is kept
on one (AGENTS.md, Issue tracking). Its volumes are then the only copy
of that work, so a rebuild follows a few more steps than a test does.
Give such a stack a project name of its own (`-p NAME`) and build it
from a checkout used for nothing else, such as a worktree detached at
`origin/main`, so that switching branches in your working checkout
never changes the files it runs from.

1. **Dump the database and keep the dump off the volume**, as
   [Back up and restore](#back-up-and-restore) does, then
   `docker compose -p NAME cp postgres:/var/lib/postgresql/aries.dump .`
   and `chmod 600` it: it holds every issue, comment and memory.
2. **Bring the checkout up to date**: `git fetch` and
   `git checkout --detach origin/main` in that worktree.
3. **Rebuild with the variables of the first start.** `ARIES_KEYS`,
   `ARIES_TELEMETRY`, the profiles and the ports must be the same, or the
   stack changes with them:

   ```sh
   COMPOSE_PROJECT_NAME=NAME ARIES_KEYS="alice $(cat ~/.ssh/id_ed25519.pub)" ARIES_TELEMETRY=1 \
     docker compose --profile observe up -d --build --wait
   ```

4. **Rebuild `aries` from the same commit**, and put it where the
   repository's agents find it on `PATH`.
5. **Check the client:** `aries whoami` names the principal and the
   project over HTTPS. If it says the certificate does not verify, the
   CA changed; put the new one in `.aries.yaml`'s `server.ca`, from
   `docker compose -p NAME exec -T aries cat /etc/aries/tls/ca.pem`.
6. **With telemetry, check it reaches the backend.** The image has no
   `curl`; ask the observe container's Prometheus instead, for example
   `docker compose -p NAME exec -T observe curl -s --get http://127.0.0.1:9090/api/v1/query --data-urlencode 'query=count({__name__=~"aries_.*"})'`.

**The CA changes once for a stack made before 17 October 2026.** The
test CA has always been on the `state` volume, where `/etc/aries`
points. Since the proxy profile, the CA and the certificate live on a
volume of their own, `tls`, mounted at `/etc/aries/tls`, which hides
the old directory. So the first start after that change makes a new CA,
and every checkout that names the old one in `server.ca` must take the
new one (step 5). It stays the same from then on.

**What agents see meanwhile.** A restart drops every connection for as
long as ariesd takes to start, under a minute here. `aries mcp`
reconnects by itself; a write made while the server is away is queued
and sent once it answers, and a read that needs the server is refused,
saying so ([When the server cannot be reached](../cli.md#when-the-server-cannot-be-reached)).
Claims keep their leases across a short restart. So a rebuild need not
wait for agents to stop, though one between their tasks is tidier.

## Back up and restore

The [guide's dump and restore](../server.md#backups-and-restore) work
here, with the Postgres container standing for the server's Postgres
and `docker compose stop aries` for `systemctl stop ariesd`:

```sh
docker compose exec -u postgres postgres pg_dump --format=custom --file=/var/lib/postgresql/aries.dump aries
docker compose stop aries
docker compose exec -u postgres postgres dropdb aries
docker compose exec -u postgres postgres pg_restore --create --dbname postgres /var/lib/postgresql/aries.dump
docker compose start aries
```

The dump stays on Postgres's volume, which `down -v` removes; copy it
out with `docker compose cp postgres:/var/lib/postgresql/aries.dump .`
to keep it. Run on 15 October 2026: a dump of the whole database and
one of `project_web`, a restore of each after a wipe, with what was
written after the dump gone, the rest there, and `aries admin status`
reporting no fault.

## Run the smoke test

```sh
deploy/docker/smoke.sh
```

CI runs it too, in every mode below, every night on `main` and whenever
a person starts it from the Actions tab (`.github/workflows/smoke.yml`):
one job per mode, none of them a required check and none on a pull
request, so a pull request's checks do not cover it. A mode broken by two
merges that each passed then fails within a day. A failed night is in
the Actions tab, with the last lines of `smoke.sh` on the job's summary
naming the check that failed; file it in Aries with the run's link.

It builds the image and `aries` from the checkout and starts its own
server, serving HTTPS alone on port 8444 with two invented principals,
so a server on 8443 is left alone, and drives `aries` over HTTPS. It
first checks that the server exposes no SSH path: sshd does not run,
ariesd wrote no keys file, and the bridge sshd would run is refused,
`SSH is off`. With `ARIES_SMOKE_SSH=1` it serves SSH too, on port 2223,
and drives `aries` over SSH (`ARIES_TRANSPORT=ssh`); the checks of the
keys file below run only then. It drives `aries` through create, ready, start, comment,
hours, memory, finish with discovered work, dependencies, show, prices,
token usage and cost, plans and the digest, and checks that a non-admin
is refused admin commands. Then it adds a second project with `aries admin
project add` and uses it, checks that someone who is no member of it is
refused, adds a third principal whose key is in no `authorized_keys`
file and connects as it with no restart, removes that key and checks
that it is refused, holds a lock around a command with `aries lock run`
(the epoch in its environment, its exit code passed on, a second
acquire refused while the lock is held), and restarts the server to
check that nothing changed. Along the way it checks the keys file inside the container: a
regular file of mode 0600 owned by `aries`, with a line for each key
added and none for a key removed or a disabled principal's; that a key
in both files is marked, comes in by ariesd's file, which sshd reads
first, warns when it is removed from the control database, and then
comes in by `authorized_keys`; that a key added to `authorized_keys`
by hand keeps its principal, whoever tries to add it to the control
database and whichever was there first; and that ariesd's log has no
failed write, names an event on every line, has the audit stream's line
for a key an admin added, and holds no key's text. It checks that
`aries admin status` shows Postgres's figures and each project's
schema's size, as text and as JSON, to a server admin and not to anyone
else; that the server's container reaches Postgres and this machine
does not; and that the config file holds no password, and the password
file its DSN names is mode 0600 and the `aries` user's; and that the
status report names the transports, and with SSH off shows no keys
file. It runs the server fourteen hours east of UTC (`ARIES_TZ`), and
gives a key an end a minute away ([Keys that end](../server.md#keys-that-end)):
the keys file's line carries it in UTC with its `Z`, which sshd honors
there where it refuses a line whose digits lack the `Z`; at the end the
line goes, the connection the key signed in is closed and logged, and
the key is refused, by sshd with SSH on and saying when it ended over
HTTPS. It removes its server and volumes, prints one line (on 16
October 2026: 156 checks over HTTPS, 169 with SSH on), and exits
non-zero on the first failure. CI
does not run it; `scripts/gate.sh --smoke` runs it after the gate. It
hides ssh-agent from `aries`, so the keys you hold play no part. Arguments
go to `docker build`; behind a proxy that
intercepts TLS, pass its CA with `--secret id=ca,src=FILE`, which never
reaches the image.

`ARIES_IMAGE=NAME` tests an image you built instead of building
`aries-dev:local`, and `ARIES_SMOKE_LOG=FILE` saves the server's log
before the server is removed. `ARIES_SMOKE_PROJECT=NAME` names the
Compose project, `aries-smoke` by default: give each of two runs on
one machine its own, and its own `ARIES_HTTPS_PORT` and `ARIES_PORT`.

### Behind the proxy

```sh
ARIES_SMOKE_PROXY=1 deploy/docker/smoke.sh
```

This runs the same checks through the `proxy` profile
([Behind a proxy](#behind-a-proxy)): ariesd serves plain HTTP on
loopback, nginx serves TLS on the HTTPS port, and everything the test
does, `aries` and its WebSocket, the CI commands, the restart and the
keys that end, reaches the server through nginx (156 checks on 16
October 2026, as direct). The checks of what `aries admin status` says
of the listener expect the proxy mode. It pulls one more image, and
combines with `ARIES_SMOKE_IDENTITY=1`, where bob's sign-in through
Keycloak also passes through nginx (207 checks). The server's
container is restarted by the test, so nginx, which shares its network
namespace, is restarted after it. The smoke test publishes Grafana's
port 3000 as the server does: set `ARIES_GRAFANA_PORT` if something else
holds it.

### With the telemetry harness

```sh
ARIES_SMOKE_TELEMETRY=1 deploy/docker/smoke.sh
```

This runs the same checks with ariesd's telemetry on and both
profiles of [Telemetry](#telemetry), which pulls two more images, and
then checks the harness of
[deploy/observability](../../deploy/observability) against the server
it has just used (212 checks in all on 15 October 2026, in about two
minutes):

- **A trace.** A request sent with a `TRACEPARENT` of the test's own
  arrives at the collector as a span in that trace, under the test's
  span, with the attempt of its write beneath it and the wait for the
  writer beneath that; a request that brings none is the root of its
  own trace.
- **Stand-ins, not names.** Every span that says who says so with a
  stand-in, `aries admin principal whois` resolves it for a server admin
  and for nobody else, and nothing the collector received, in a span, a
  metric or a log record, holds a principal's name, a session id or an
  issue's title.
- **A log record, with its trace.** A request the test traces and the
  server refuses arrives as a log record in the test's trace, with the
  id of the server's own span for it, its `code` and its `req`; and the
  server's own log has that line, found by the `req`, with the name the
  record leaves out.
- **The audit stream.** A principal added with `aries admin` is a line of
  the audit stream in the server's log, naming who added whom, and a
  record at the collector with the same `seq`, where both are stand-ins
  that `whois` resolves; the membership it gave is the next record.
- **No text in a record.** Every field of a record that names a person
  holds a stand-in, and no record has a field that holds text: an
  error, a target, a machine's name, a path.
- **Metrics.** Sixteen of ariesd's instruments arrive pushed over
  OTLP, and the same sixteen by a scrape of `/metrics`, its pool of
  connections to Postgres among them: every one a rule or the dashboard
  reads.
- **The alert rules and the dashboard.** Prometheus loads every rule and
  evaluates each without error, and runs every query of the dashboard.
- **`/readyz`** says `ready`; with Postgres's container frozen
  (`docker compose pause postgres`), `not ready: the last check of the
  database failed`, while `aries admin status` still answers, with
  `postgres: unknown`; and `ready` again once it runs.
- **A collector that is down.** The same 18 requests are sent with the
  collector running, with it stopped, so that its port refuses, and with
  it frozen, so that its port accepts and never answers. They answer
  the same, and take no more than 5 seconds longer in all. ariesd
  counts the failed exports of metrics, the lost spans and the lost log
  records, warns once for each signal, the alerts for dropped spans and
  dropped log records fire, and exports work again when the collector
  is back.
- **A stop is not held.** With the collector frozen, restarting the
  server takes at most 8 seconds longer than with it running: the 5
  seconds ariesd gives its last exports, and 3 for a clock read in
  whole seconds.

### With the identity provider

```sh
ARIES_SMOKE_IDENTITY=1 deploy/docker/smoke.sh
```

This runs the same checks with the `identity` profile of
[Sign-in with a local identity provider](#sign-in-with-a-local-identity-provider),
which pulls Keycloak, on `127.0.0.1:8181` (`ARIES_IDENTITY_PORT`) so
that a Keycloak of your own on 8180 is left alone, and then checks it.
It needs `curl` and `openssl` on your machine. It prints the claims of
the tokens it gets and never a password, a secret, a code or a token,
which go from Keycloak's volume to each request through pipes. 197
checks passed on 16 October 2026, on Postgres, over HTTPS, 34 of them
Keycloak's and 17 the sign-in's through the server:

- **One issuer.** The discovery document from your machine and from
  the server's container, by name, names the same issuer, offers PKCE
  with S256 and the device grant, and lists `auth_time`, `email` and
  `nonce`; the key set has an ES256 and an RS256 signing key.
- **The realm.** `identity.sh show` lists the three clients as above,
  alice and bob with verified addresses, and every secret's file 0600
  in a 0700 directory; and none of the generated passwords and secrets
  is in Keycloak's log.
- **A sign-in, as a browser makes one.** alice signs in to `aries-cli`
  by the code flow with PKCE, a nonce and `prompt=login`, to a loopback
  redirect with the state sent; the ID token for the code is signed
  ES256 and carries the issuer, `aud` `aries-cli`, the nonce sent,
  `auth_time`, `email` `alice@example.com` and `email_verified` true,
  and no refresh token comes with it. A second code, exchanged with the
  wrong PKCE verifier, is refused (`invalid_grant`).
- **The device grant** answers with a user code and Keycloak's page to
  enter it on.
- **A service principal.** `ci-example`'s secret gets a token by the
  client-credentials grant, for `aries.example.com`, with `azp`
  `ci-example` and the made-up `repository` claim; a wrong secret is
  refused.
- **A sign-in through the server, end to end** (`ARIES_IDENTITY=1`,
  slice T2). `/.well-known/aries` names the provider `keycloak`; an
  invitation at a provider the server does not have is refused, and
  alice invites bob's address at `keycloak`. bob runs `aries login
  --url`, and curl, as a browser on another machine, opens the URL it
  prints: the server sends it to Keycloak with `prompt=login`, PKCE and
  a nonce; it signs bob in with his password; Keycloak sends it to the
  server's callback, which exchanges the code and sends it on to `aries`'s
  loopback address, which curl does not open; the address is pasted
  back, after a line that is not one. `aries login` says bob is signed
  in, for the day; `whoami` shows the sign-in; bob creates an issue
  signed in; his identity is bound; alice sees his sign-in live; the
  status shows one live, and Keycloak's keys from a file, nothing
  fetched; the token's file is 0600, and the token is in no line of
  ariesd's log, which has `signin.granted`; `aries logout` ends it, bob
  is back on his key, and the server lists it ended by logout. alice,
  whom nobody invited at Keycloak, is refused with the fix to ask for an
  invitation.

Without `ARIES_SMOKE_IDENTITY` the smoke test is as before: 155 checks
on 15 October 2026, and no Keycloak.

**On an internal network** (`internal: true`, the override above), run
by hand on 15 October 2026 since the smoke test reaches the server
from your machine: both containers became healthy; neither could open
a connection to an address or a name outside (`1.1.1.1:443`,
`example.com:443`); no port was published; `identity.sh show` listed
the realm as above; and from the server's container, by name, the
discovery document named the issuer, the key set had the ES256 and
RS256 keys, and a client-credentials token for `ci-example` came back
with the issuer, `aries.example.com` and the made-up claims.

### What was checked with variants of the image

The keys file replaced an sshd `AuthorizedKeysCommand` that asked
ariesd about each key. Three things that mechanism could not do were
checked on 12 October 2026, each in a throwaway image built `FROM
aries-dev:local`. The images are not in the repository; each is a few
lines, described here so that you can build it again.

| Variant | How | Result |
|---|---|---|
| **The guide's install.** ariesd owned by `aries` in `/opt/aries/bin`, linked from `/usr/local/bin`, with no file of that name owned by root | `mv` the binary, `chown aries:aries`, `ln -sfn`; then `ARIES_IMAGE=… deploy/docker/smoke.sh` | All 84 checks pass. With the old mechanism sshd refused this layout (`Unsafe AuthorizedKeysCommand … bad ownership or modes`) |
| **A provider-style global command.** `/etc/ssh/sshd_config.d/00-provider.conf`, read before Aries's file and outside any `Match` block, with `AuthorizedKeysCommand /usr/local/sbin/provider-keys %u` and `AuthorizedKeysCommandUser nobody`; the script, owned by root, notes each call in a file and prints nothing | The smoke test, then one sign-in at a time with the note file counted | All 84 checks pass, with no override. `sshd -T -C user=aries` shows the global command and `authorizedkeysfile .ssh/authorized_keys .ssh/aries_keys` together. sshd ran the script twice for each sign-in by `aries`, whichever file held the key, and three times for a refused one. With `AuthorizedKeysCommand none` added to the `Match` block, it ran it no times, and keys from both files still signed in |
| **ariesd stopped.** An entrypoint that starts sshd again, alone, after the image's entrypoint ends | A principal added with a key only the control database holds; `kill` ariesd in the container; `aries ready` as that principal | sshd logs `Accepted publickey for aries`, the keys file is still there (0600, `aries`), and `aries` prints `ariesd is not running on the server`, with the fix `ask the server admin to start it`, and exits 1: with `--json`, code `unavailable`. A key from `authorized_keys` gets the same answer, and a key in neither file is refused by sshd as before |

**What the stand-in for a provider does not show.** It is a command
that is asked and answers with no key. Google Cloud's OS Login, AWS's
EC2 Instance Connect and Azure's Entra ID extension were not run: not
what else they set in sshd's configuration or in PAM, not what their
commands print for an account named `aries`, and not how long they
take. sshd accepts a key that such a command prints, so
`AuthorizedKeysCommand none` in the `Match` block, which this image and
the server guide both set, is what limits the account to the two files.
With it, sshd did not run the stand-in at all and both files still
worked.

**Not checked here at all:** systemd, which a container does not have,
so the unit's `ReadWritePaths`
([Keys from the store](../server.md#keys-from-the-store), step 1); and
mandatory access control. Docker Desktop's kernel loads neither
AppArmor nor SELinux, so nothing here says anything about either
([What a server needs](../server.md#what-a-server-needs)). The units
and the AppArmor profiles were run on an Ubuntu 24.04 virtual machine
instead ([What was run, and where](../server.md#what-was-run-and-where)).

## Troubleshooting

[Troubleshooting](../server.md#troubleshooting) in the server guide
covers what `aries` and ariesd say. These are the test server's own:

| What you see | Cause | Fix |
|---|---|---|
| `docker build` or `docker compose up --build` fails with `exec: "docker-credential-desktop": executable file not found in $PATH` | Docker Desktop's configuration (`~/.docker/config.json`, `"credsStore": "desktop"`) names a helper program that Docker Desktop keeps in `~/.docker/bin`, and that directory is not on your `PATH`: usually because another `docker`, such as a package manager's, comes first. Docker runs the helper whenever it asks a registry for a base image, public ones included, so a build whose base images are already cached does not show it | `export PATH="$HOME/.docker/bin:$PATH"`, in your shell's profile to keep it |
| You are not who you meant to be | With no key named, `aries` offers your default identities and then ssh-agent's keys, and the server takes the first it knows | `aries whoami` says who you are and by which key. Name the key you mean with `ARIES_KEY_FILE=PATH`: it is then the only one offered |
| `aries: localhost:2222 accepted none of the 5 keys aries offered: … more keys that ssh-agent holds were not offered` | You have more keys than `aries` offers, five, which is one fewer than sshd tries, and yours is not among them | Name your key with `ARIES_KEY_FILE=PATH`, the private key or its `.pub` |
| `aries: key: in .aries.yaml names …, outside the repository` | `.aries.yaml` is committed, so its `key:` may name only a file inside the repository | Remove the line and set `ARIES_KEY_FILE` to the file's path instead |
| `aries: localhost:2222 accepted no key` for a key added with `aries admin` | sshd did not find the key in either file | `docker compose logs` shows what sshd and ariesd said, and [Look inside](#look-inside) shows the keys file |
| `aries: host key mismatch for localhost` after `docker compose down -v` | The volume held the host key and the project, and the next start made new ones | Save the newly printed block as `.aries.yaml` |

## Look inside

| To | Run |
|---|---|
| Follow the logs of sshd, ariesd and Postgres | `docker compose logs -f` |
| Open a shell | `docker compose exec aries bash` |
| Query the database as Postgres's superuser | `docker compose exec -u postgres postgres psql aries` |
| See ariesd's role and database as Postgres has them | `docker compose exec -u postgres postgres psql -c '\du aries' -c '\l aries'` |
| Read ariesd's own metrics, with [telemetry](#telemetry) on | `docker compose exec aries bash -c 'exec 3<>/dev/tcp/127.0.0.1/9464; printf "GET /metrics HTTP/1.0\r\n\r\n" >&3; cat <&3'`; `/readyz` and `/healthz` the same way |
| See what the collector received | `docker compose --profile telemetry logs collector` |
| Read the keys file ariesd writes for sshd | `docker compose exec aries cat /var/lib/aries/.ssh/aries_keys` (public keys only) |
| See which files sshd reads for the account | `docker compose exec aries sshd -T -C user=aries \| grep -i authorizedkeys` |
| Read the config | `docker compose exec aries cat /etc/aries/ariesd.yaml` (it holds no password: that is in `/etc/aries/postgres/pgpass`, which this does not print) |
| See Keycloak's realm, clients and people, with the [identity profile](#sign-in-with-a-local-identity-provider) | `docker compose --profile identity exec keycloak /opt/aries/identity.sh show` (no secret) |

## Stop and wipe

```sh
docker compose stop              # stop; docker compose start resumes
docker compose down              # remove the container, keep the volume
docker compose down -v           # remove the volumes too: the database, its password, the host key and the project
docker image rm aries-dev:local
```

With the identity profile, give `--profile identity` to `down`, so
that Keycloak's container goes too; `down -v` then also removes its
volume, with its realm and generated passwords.

After `down -v`, the next start makes a new project and host key, so
update `.aries.yaml`.

## How it differs from a real server

| Running a server | Here | Why |
|---|---|---|
| Ubuntu 24.04 or 26.04 with systemd units for Postgres and ariesd | Ubuntu 24.04 for sshd and ariesd; the entrypoint starts the two and stops both when one exits. Postgres in a container of its own, Docker's official image | A container has no systemd. Each program still runs as its own user with a bare environment, `umask 077` and no way to gain privileges. The image stays on 24.04: 26.04 was run with the [installer](../server.md#with-the-installer) instead, and its OpenSSH turns away for a while an address that fails to log in, which here is Docker's one address for every client, so the smoke test's refused keys would lock out its next steps ([step 6](../server.md#6-let-the-first-principal-in)) |
| ariesd installed from a signed release into `/opt/aries/bin` | Built from the checkout into `/usr/local/bin`; no `systemd_unit` and no sudo rule | It tests what is checked out. Rebuild the image instead of running `ariesd upgrade` |
| ariesd may write to `~aries/.ssh` by a `ReadWritePaths` line in its unit | Nothing confines it | A container has no systemd. sshd's `AuthorizedKeysFile` line is the guide's |
| You opt in to many projects and run `control init` once | Opted in from the first start; `control init` runs at every start | So there is nothing to set up |
| HTTPS on port 443, with a certificate from ACME or the company's CA ([Certificates](../server.md#certificates)) | Port 8443 inside the container, published on 127.0.0.1; a certificate from a test CA the container made, which clients trust by `server.ca` | No DNS name and no port below 1024 to bind; the files mode, which reads the certificate again when it changes, is the guide's |
| Postgres on the machine, reached by its unix socket with no password | Postgres in the other container, reached over the compose network with a password in a 0600 file, without TLS on that private network | It tests the path a Postgres on another machine, or a managed service, takes |
| The host key in `/etc/ssh`; `/var/lib/postgresql`, `/etc/aries` and `~aries/.ssh` on the disk | The server's paths linked into the `/state` volume, with the host key in `/state/hostkeys`; the database on Postgres's own volume | Everything survives a rebuild, and `down -v` wipes it |
| sshd serves every account | `AllowUsers aries`, with passwords off | Nobody else signs in |
| A daily dump | No backups, but the guide's dump and restore work ([Back up and restore](#back-up-and-restore)) | It is for testing |
| Telemetry is set in ariesd's unit and config, and a collector runs on the machine | `ARIES_TELEMETRY=1` sets both, and the collector runs in a container that shares the server's network namespace; metrics are exported every 5 seconds and spans within 1, and the log is exported too, which a real server does only when asked | One variable to try it, and something to look at soon after |
| Sign-in, once built, is at the organization's identity provider, over HTTPS | Keycloak in development mode, over HTTP on `keycloak.localhost`, with a realm made by a script and generated passwords, with the `identity` profile | To develop and test sign-in with no account and no internet; never for production |
