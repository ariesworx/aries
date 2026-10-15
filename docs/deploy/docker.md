# Docker test server

**For** trying starfix, or testing a branch, against a real `starfixd`
from a Mac or any machine with Docker, without touching a real server.

**Not for production.** It keeps no backups, and the
[deployment examples](README.md) are how to run starfix for real.

```text
your machine
  sfx ──SSH──▶ 127.0.0.1:2222 ──▶ container: sshd ──▶ starfixd ──▶ Dolt
                                     └── volume: Dolt's data, the host key, the config
```

One container runs the three programs, set up as in
[Running a server](../server.md#set-up-a-server): Dolt under its own
account with the same locked-down grants, starfixd as `starfix`, and
sshd with a forced command per key. starfixd runs without `--dev`, so it
makes every check it makes on a real server. The files are in
[deploy/docker](../../deploy/docker).

It is a server of [many projects](../server.md#serving-many-projects):
it has a control database and eight database names granted for new
projects, the project it starts with is adopted under the name `first`,
and sshd reads the keys file starfixd writes
([Keys from the store](../server.md#keys-from-the-store)). So you can
try `sfx admin project add`, and add a person with `sfx admin principal
add NAME --key FILE` and no restart.

[Troubleshooting](#troubleshooting) has what to do when a build fails
or a key is refused.

## What you need

- **Docker Desktop or OrbStack** on a Mac, Apple silicon or Intel, or
  Docker Engine 25 or later with Compose v2 on Linux. On Apple silicon the
  image is built for arm64, so nothing is emulated. With Docker Desktop,
  `~/.docker/bin` must be on your `PATH`
  ([Troubleshooting](#troubleshooting)).
- **Go**, at the `go` line in [go.mod](../../go.mod), to build `sfx` from
  the same checkout, so client and server speak the same protocol.
- **An SSH key**, such as `~/.ssh/id_ed25519`. If it has a passphrase,
  load it into ssh-agent (`ssh-add -l` lists it).

## Start it

Run everything from the repository root, on the branch you want to test.
`COMPOSE_FILE` saves typing the file in every command:

```sh
export COMPOSE_FILE=deploy/docker/compose.yaml
export STARFIX_KEYS="alice $(cat ~/.ssh/id_ed25519.pub)"
docker compose up -d --build --wait
docker compose logs --no-log-prefix starfix | grep -A 13 'test server ready' | tail -n 14
```

The build compiles `starfixd` and `sfx` from your checkout and downloads
Dolt at the release starfix pins, checking its SHA-256. The first start
makes the host key, the project, the control database and Dolt's
password, which only the config file holds (mode 0600) and nothing
prints. Every start then runs `starfixd control init`, which records the
project and the principals and keys you gave. The log ends with what a
client needs (these values are invented):

```text
starfix test server ready (not for production)
  host key    SHA256:…
  project     6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
  principals  alice
  admins      alice
  more        sfx admin project add NAME (8 database names granted); sfx admin principal add NAME --key FILE

Save this as .starfix.yaml in a scratch repository:

project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: localhost
  port: 2222
  host_key: SHA256:…
```

Later starts reuse the volume, so the project, the host key and the data
stay the same, and print the block again; the command above shows the
newest. The container generated the host key on your own machine,
so there is nothing to verify out of band.

| Variable | Default | Sets |
|---|---|---|
| `STARFIX_KEYS` | none | The principals: one `NAME TYPE KEY` line per key, such as `alice ssh-ed25519 AAAA…`. A file mounted at `/etc/starfix-keys` adds lines. A start that gives keys rewrites `authorized_keys` with them, and one that gives none keeps the file. Each start also copies them into the control database (`starfixd control init`), where they stay: dropping a name here does not remove it, so remove it with `sfx admin principal disable NAME` or `sfx admin key remove FINGERPRINT`. Everyone new, but the admins, becomes a member of the first project. A start does not undo what an admin did with `sfx admin`: someone removed from the first project stays removed, a disabled principal stays disabled, and a key removed from the control database stays out of it, each reported in the container's log. One thing it cannot do for you: a key still listed here is still a line of `authorized_keys`, where sshd accepts it whatever the control database holds, so to revoke a key, take it out of `STARFIX_KEYS` as well, or disable its principal ([What a restart keeps](#what-a-restart-keeps)) |
| `STARFIX_ADMINS` | the first name | The server [admins](../server.md#admins), comma-separated. They reach every project and need no membership |
| `STARFIX_PROJECT` | random | The project UUID, used on the first start only |
| `STARFIX_PORT` | `2222` | The port on 127.0.0.1, also printed in `.starfix.yaml` |
| `STARFIX_TELEMETRY` | unset | `1` turns starfixd's telemetry on ([Telemetry](#telemetry)). Unset, starfixd exports nothing and opens no port, as on a server where nothing is configured |

sshd listens on 127.0.0.1 alone, so nothing beyond your machine can reach
it.

## Connect a scratch repository

Build `sfx` into a scratch repository, and save the printed block as
its `.starfix.yaml`; the third command copies the newest one from the
log:

```sh
mkdir -p ~/sfx-scratch
go build -o ~/sfx-scratch/sfx ./cmd/sfx
docker compose logs --no-log-prefix starfix | grep -A 4 '^project:' | tail -n 5 > ~/sfx-scratch/.starfix.yaml
cd ~/sfx-scratch && git init -q
./sfx ready
./sfx create "Try the test server" -p 2 -t task
./sfx ready
```

`sfx` signs in with the key `ssh` would use: a default file in `~/.ssh`,
then the keys in ssh-agent. If yours is neither, name its file by its
path: `export STARFIX_KEY_FILE=~/keys/mine`. The rest of
[Using sfx](../cli.md) and the [agent guide](../agents.md) work as on any
server.

**To act as a second principal**, such as a non-admin, make it a key
and add it as alice, the admin. Nothing restarts:

```sh
ssh-keygen -q -t ed25519 -N '' -C bob@example.com -f ~/sfx-scratch/bob
cd ~/sfx-scratch && ./sfx admin principal add bob --key ~/sfx-scratch/bob.pub
```

Then run `sfx` as bob by naming his key:

```sh
STARFIX_KEY_FILE=~/sfx-scratch/bob ./sfx whoami
STARFIX_KEY_FILE=~/sfx-scratch/bob ./sfx ready
```

A key named this way is the only key `sfx` offers, so you are bob even
with your own key, alice's, in `~/.ssh` and in ssh-agent
([Your key is your identity](../cli.md#your-key-is-your-identity)).
`sfx whoami` says which principal the server took you for and which key
signed. bob is a member of the first project and no admin: `sfx admin
project list` shows bob his `ROLE` in each project, and alice each
project's `DATABASE`. `./sfx admin key remove FINGERPRINT` takes his key
away again, and `./sfx admin key list bob` shows the fingerprint.

**To try a second project**, run `./sfx admin project add web` as alice.
It prints the `.starfix.yaml` for the new project: save it in another
scratch repository, and add bob to it there with `~/sfx-scratch/sfx
admin member add bob`. bob has a key, which would sign in to the new
project too, so `sfx` lists it and asks before it adds him
([Managing access](../server.md#managing-access)). `sfx admin status`
then shows the server and a row for each project.

**Dolt's own metrics are on**, as the guide's setup has them: Dolt
serves them at `http://127.0.0.1:11228/metrics` inside the container,
on loopback only, since the endpoint has no authentication, and the
compose file publishes no port but sshd's. starfixd reads them every 15
seconds, so `sfx admin status` has a `Dolt` line with its memory,
connections and uptime, and a `Dolt's collector:` line
([Dolt's figures](../server.md#dolts-figures)). A volume made by an
older image gains the setting at its next start.

Giving a second key in `STARFIX_KEYS` and starting again still works,
as before, and puts the key in `authorized_keys` as well.

### What a restart keeps

The container runs `starfixd control init` at every start, to import
what `STARFIX_KEYS` gained. It adds what is new and leaves alone what an
admin did since ([Opt in](../server.md#opt-in)):

| An admin ran | After a restart, with the name still in `STARFIX_KEYS` |
|---|---|
| `sfx admin member remove NAME` | NAME is still not a member of the first project |
| `sfx admin principal disable NAME` | NAME is still disabled, and refused |
| `sfx admin key remove FINGERPRINT` | The key is still out of the control database. sshd still accepts it, because the start wrote it to `authorized_keys` again: the removal warned of this. Take the key out of `STARFIX_KEYS` too, or disable NAME |

The container's log names each line it left alone, and why. To put back
from `STARFIX_KEYS` what admins removed, which overrides them, run it by
hand with the flag for that; the running server takes it up within half
a minute:

```sh
docker compose exec -u starfix -w /var/lib/starfix starfix \
  starfixd control init --name first --restore-removed-access
```

## Telemetry

Off unless you ask. To see what starfixd reports about itself
([Observability](../server.md#observability)), start the server with
its telemetry on and a collector beside it:

```sh
export STARFIX_TELEMETRY=1
docker compose --profile telemetry up -d --build --wait
docker compose --profile telemetry logs -f collector
```

| You set | It does |
|---|---|
| `STARFIX_TELEMETRY=1` | starfixd serves `/metrics`, `/healthz` and `/readyz` on `127.0.0.1:9464` inside the container, pushes metrics, traces and the records of its log lines ([Logs over OTLP](../server.md#logs-over-otlp)) to `http://127.0.0.1:4318`, and keeps the key for people's stand-ins on the volume (`/state/telemetry/principals.key`), so a rebuild keeps them |
| `--profile telemetry` | Runs an OpenTelemetry Collector with [deploy/observability/collector.yaml](../../deploy/observability/collector.yaml): it receives what starfixd pushes, scrapes starfixd's `/metrics` and Dolt's, and prints every metric, span and log record in full |
| `--profile prometheus` | Runs a Prometheus with [prometheus.yaml](../../deploy/observability/prometheus.yaml) and the alert rules of [alerts.yaml](../../deploy/observability/alerts.yaml), scraping the same two endpoints |

- **Both run in the server's network namespace** (`network_mode:
  service:starfix`). starfixd's listener and Dolt's metrics answer on
  loopback alone and have no authentication, and starfixd pushes to
  loopback; a collector in a container of its own could reach neither.
  On a real server the collector runs on the machine, for the same
  reason.
- **Neither publishes a port or keeps a volume.** Prometheus keeps its
  data in memory, and has no page you can open from your machine. Ask
  it from inside:

  ```sh
  docker compose --profile prometheus exec prometheus promtool query instant http://127.0.0.1:9090 'starfix_stores_open'
  docker compose --profile prometheus exec prometheus promtool query instant http://127.0.0.1:9090 'ALERTS'
  ```

- **The images are pinned by digest** in the compose file: the
  Collector's core distribution at 0.162.0, and Prometheus at 3.15.0.
- **A trace of your own.** Give `sfx` a W3C trace context and look for
  its trace id in the collector's output: the server's span is under
  yours, with the write beneath it and you named by a stand-in, which
  `sfx admin principal whois` resolves for a server admin.

  ```sh
  TRACEPARENT=00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01 ./sfx create "Traced" -p 3 -t task
  docker compose --profile telemetry logs collector | grep -A 20 0af7651916cd43dd8448eb211c80319c
  ```

- **The audit stream, at the collector.** Change who has access, and
  the change is a log record with `audit: Bool(true)`, naming who made
  it and whom it changed by stand-ins; the same line in the server's own
  log has the names, under the same `seq`.

  ```sh
  ./sfx admin principal add erin
  docker compose --profile telemetry logs collector | grep -B 9 -A 9 'audit: Bool(true)'
  docker compose logs starfix | grep 'audit=true'
  ```

- **A test server exports sooner than a real one,** and exports its
  log: metrics every 5 seconds, not 60, and a finished span within 1
  second, not 5, so that what you just did is there to look at; and its
  log records, which a real server sends only when asked
  (`OTEL_LOGS_EXPORTER=otlp`).
- **After restarting the server, restart the two beside it.** A restart
  gives the server's container a new network namespace, and the others
  are left in the old one: `docker compose restart starfix`, then
  `docker compose --profile telemetry --profile prometheus restart
  collector prometheus`. `docker compose restart` of all three at once
  fails for those two.
- **`STARFIX_TELEMETRY=1` without the collector** is a server whose
  collector is down: starfixd warns once for each signal, counts what it
  drops, and serves as before
  ([When the collector is down](../server.md#when-the-collector-is-down)).
- **To turn it off again,** unset the variable and run `docker compose
  up -d`. The key for stand-ins stays on the volume.

starfixd's log still goes to the container's output, whole, as before:
`docker compose logs starfix`. What the collector receives of it is
less, by design: ids, codes and numbers, and no text.

## Test a change

After you change the code, rebuild the image and `sfx`:

```sh
docker compose up -d --build --wait
go build -o ~/sfx-scratch/sfx ./cmd/sfx
```

The volume keeps its data, and a new migration runs on it when starfixd
starts. An image older than the volume's schema refuses to open it
(migrations only go forward): wipe the volume, below, to go back.

## Run the smoke test

```sh
deploy/docker/smoke.sh
```

It builds the image and `sfx` from the checkout and starts its own
server, on port 2223 with two invented principals, so a server on 2222
is left alone. It drives `sfx` through create, ready, start, comment,
hours, memory, finish with discovered work, dependencies, show, prices,
token usage and cost, plans and the digest, and checks that a non-admin
is refused admin commands. Then it adds a second project with `sfx admin
project add` and uses it, checks that someone who is no member of it is
refused, adds a third principal whose key is in no `authorized_keys`
file and connects as it with no restart, removes that key and checks
that it is refused, holds a lock around a command with `sfx lock run`
(the epoch in its environment, its exit code passed on, a second
acquire refused while the lock is held), and restarts the server to
check that nothing changed. Along the way it checks the keys file inside the container: a
regular file of mode 0600 owned by `starfix`, with a line for each key
added and none for a key removed or a disabled principal's; that a key
in both files is marked, comes in by starfixd's file, which sshd reads
first, warns when it is removed from the control database, and then
comes in by `authorized_keys`; that a key added to `authorized_keys`
by hand keeps its principal, whoever tries to add it to the control
database and whichever was there first; and that starfixd's log has no
failed write, names an event on every line, has the audit stream's line
for a key an admin added, and holds no key's text. It checks that Dolt's metrics endpoint answers on
loopback in the container and not on the container's own address, and
that `sfx admin status` shows Dolt's figures and its collector's cycles
against the writes, as text and as JSON, to a server admin and not to
anyone else. It removes its server and volume, prints one line (137
checks on 12 October 2026), and exits non-zero on the first failure. CI
does not run it; `scripts/gate.sh --smoke` runs it after the gate. It
hides ssh-agent from `sfx`, so the keys you hold play no part. Arguments
go to `docker build`; behind a proxy that
intercepts TLS, pass its CA with `--secret id=ca,src=FILE`, which never
reaches the image.

`STARFIX_IMAGE=NAME` tests an image you built instead of building
`starfix-dev:local`, and `STARFIX_SMOKE_LOG=FILE` saves the server's log
before the server is removed. `STARFIX_SMOKE_PROJECT=NAME` names the
Compose project, `starfix-smoke` by default: give each of two runs on
one machine its own, and its own `STARFIX_PORT`.

### With the telemetry harness

```sh
STARFIX_SMOKE_TELEMETRY=1 deploy/docker/smoke.sh
```

This runs the same checks with starfixd's telemetry on and both
profiles of [Telemetry](#telemetry), which pulls two more images, and
then checks the harness of
[deploy/observability](../../deploy/observability) against the server
it has just used (194 checks in all on 12 October 2026, in about two
and a half minutes):

- **A trace.** A request sent with a `TRACEPARENT` of the test's own
  arrives at the collector as a span in that trace, under the test's
  span, with the attempt of its write beneath it and the wait for the
  writer beneath that; a request that brings none is the root of its
  own trace.
- **Stand-ins, not names.** Every span that says who says so with a
  stand-in, `sfx admin principal whois` resolves it for a server admin
  and for nobody else, and nothing the collector received, in a span, a
  metric or a log record, holds a principal's name, a session id or an
  issue's title.
- **A log record, with its trace.** A request the test traces and the
  server refuses arrives as a log record in the test's trace, with the
  id of the server's own span for it, its `code` and its `req`; and the
  server's own log has that line, found by the `req`, with the name the
  record leaves out.
- **The audit stream.** A principal added with `sfx admin` is a line of
  the audit stream in the server's log, naming who added whom, and a
  record at the collector with the same `seq`, where both are stand-ins
  that `whois` resolves; the membership it gave is the next record.
- **No text in a record.** Every field of a record that names a person
  holds a stand-in, and no record has a field that holds text: an
  error, a target, a machine's name, a path.
- **Metrics.** Fifteen of starfixd's instruments arrive pushed over
  OTLP, the same fifteen by a scrape of `/metrics`, and ten of Dolt's
  series by a scrape of its endpoint: every one a rule or the dashboard
  reads.
- **The alert rules and the dashboard.** Prometheus loads every rule and
  evaluates each without error, and runs every query of the dashboard.
- **`/readyz`** says `ready`; with Dolt frozen (`SIGSTOP`), `not ready:
  the last check of Dolt failed`; and `ready` again once it runs.
- **A collector that is down.** The same 18 requests are sent with the
  collector running, with it stopped, so that its port refuses, and with
  it frozen, so that its port accepts and never answers. They answer
  the same, and take no more than 5 seconds longer in all. starfixd
  counts the failed exports of metrics, the lost spans and the lost log
  records, warns once for each signal, the alerts for dropped spans and
  dropped log records fire, and exports work again when the collector
  is back.
- **A stop is not held.** With the collector frozen, restarting the
  server takes at most 8 seconds longer than with it running: the 5
  seconds starfixd gives its last exports, and 3 for a clock read in
  whole seconds.

### What was checked with variants of the image

The keys file replaced an sshd `AuthorizedKeysCommand` that asked
starfixd about each key. Three things that mechanism could not do were
checked on 12 October 2026, each in a throwaway image built `FROM
starfix-dev:local`. The images are not in the repository; each is a few
lines, described here so that you can build it again.

| Variant | How | Result |
|---|---|---|
| **The guide's install.** starfixd owned by `starfix` in `/opt/starfix/bin`, linked from `/usr/local/bin`, with no file of that name owned by root | `mv` the binary, `chown starfix:starfix`, `ln -sfn`; then `STARFIX_IMAGE=… deploy/docker/smoke.sh` | All 84 checks pass. With the old mechanism sshd refused this layout (`Unsafe AuthorizedKeysCommand … bad ownership or modes`) |
| **A provider-style global command.** `/etc/ssh/sshd_config.d/00-provider.conf`, read before starfix's file and outside any `Match` block, with `AuthorizedKeysCommand /usr/local/sbin/provider-keys %u` and `AuthorizedKeysCommandUser nobody`; the script, owned by root, notes each call in a file and prints nothing | The smoke test, then one sign-in at a time with the note file counted | All 84 checks pass, with no override. `sshd -T -C user=starfix` shows the global command and `authorizedkeysfile .ssh/authorized_keys .ssh/starfix_keys` together. sshd ran the script twice for each sign-in by `sfx`, whichever file held the key, and three times for a refused one. With `AuthorizedKeysCommand none` added to the `Match` block, it ran it no times, and keys from both files still signed in |
| **starfixd stopped.** An entrypoint that starts sshd again, alone, after the image's entrypoint ends | A principal added with a key only the control database holds; `kill` starfixd in the container; `sfx ready` as that principal | sshd logs `Accepted publickey for starfix`, the keys file is still there (0600, `starfix`), and `sfx` prints `starfixd is not running on the server`, with the fix `ask the server admin to start it`, and exits 1: with `--json`, code `unavailable`. A key from `authorized_keys` gets the same answer, and a key in neither file is refused by sshd as before |

**What the stand-in for a provider does not show.** It is a command
that is asked and answers with no key. Google Cloud's OS Login, AWS's
EC2 Instance Connect and Azure's Entra ID extension were not run: not
what else they set in sshd's configuration or in PAM, not what their
commands print for an account named `starfix`, and not how long they
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
covers what `sfx` and starfixd say. These are the test server's own:

| What you see | Cause | Fix |
|---|---|---|
| `docker build` or `docker compose up --build` fails with `exec: "docker-credential-desktop": executable file not found in $PATH` | Docker Desktop's configuration (`~/.docker/config.json`, `"credsStore": "desktop"`) names a helper program that Docker Desktop keeps in `~/.docker/bin`, and that directory is not on your `PATH`: usually because another `docker`, such as a package manager's, comes first. Docker runs the helper whenever it asks a registry for a base image, public ones included, so a build whose base images are already cached does not show it | `export PATH="$HOME/.docker/bin:$PATH"`, in your shell's profile to keep it |
| You are not who you meant to be | With no key named, `sfx` offers your default identities and then ssh-agent's keys, and the server takes the first it knows | `sfx whoami` says who you are and by which key. Name the key you mean with `STARFIX_KEY_FILE=PATH`: it is then the only one offered |
| `sfx: localhost:2222 accepted none of the 5 keys sfx offered: … more keys that ssh-agent holds were not offered` | You have more keys than `sfx` offers, five, which is one fewer than sshd tries, and yours is not among them | Name your key with `STARFIX_KEY_FILE=PATH`, the private key or its `.pub` |
| `sfx: key: in .starfix.yaml names …, outside the repository` | `.starfix.yaml` is committed, so its `key:` may name only a file inside the repository | Remove the line and set `STARFIX_KEY_FILE` to the file's path instead |
| `sfx: localhost:2222 accepted no key` for a key added with `sfx admin` | sshd did not find the key in either file | `docker compose logs` shows what sshd and starfixd said, and [Look inside](#look-inside) shows the keys file |
| `sfx: host key mismatch for localhost` after `docker compose down -v` | The volume held the host key and the project, and the next start made new ones | Save the newly printed block as `.starfix.yaml` |

## Look inside

| To | Run |
|---|---|
| Follow the logs of all three programs | `docker compose logs -f` |
| Open a shell | `docker compose exec starfix bash` |
| Query Dolt as its own user | `docker compose exec -u dolt -w /var/lib/dolt/data/starfix starfix dolt sql` |
| Read Dolt's own metrics | `docker compose exec starfix bash -c 'exec 3<>/dev/tcp/127.0.0.1/11228; printf "GET /metrics HTTP/1.0\r\n\r\n" >&3; cat <&3'` (the image has no `curl`) |
| Read starfixd's own metrics, with [telemetry](#telemetry) on | `docker compose exec starfix bash -c 'exec 3<>/dev/tcp/127.0.0.1/9464; printf "GET /metrics HTTP/1.0\r\n\r\n" >&3; cat <&3'`; `/readyz` and `/healthz` the same way |
| See what the collector received | `docker compose --profile telemetry logs collector` |
| Read the keys file starfixd writes for sshd | `docker compose exec starfix cat /var/lib/starfix/.ssh/starfix_keys` (public keys only) |
| See which files sshd reads for the account | `docker compose exec starfix sshd -T -C user=starfix \| grep -i authorizedkeys` |
| Read the config | `docker compose exec starfix cat /etc/starfix/starfixd.yaml` (it holds the password) |

## Stop and wipe

```sh
docker compose stop              # stop; docker compose start resumes
docker compose down              # remove the container, keep the volume
docker compose down -v           # remove the volume too: data, host key and project
docker image rm starfix-dev:local
```

After `down -v`, the next start makes a new project and host key, so
update `.starfix.yaml`.

## How it differs from a real server

| Running a server | Here | Why |
|---|---|---|
| Ubuntu 24.04 or 26.04 with systemd units for Dolt and starfixd | Ubuntu 24.04; the entrypoint starts the three programs and stops all of them when one exits | A container has no systemd. Each program still runs as its own user with a bare environment, `umask 077` and no way to gain privileges. The image stays on 24.04: 26.04 was run with the [installer](../server.md#with-the-installer) instead, and its OpenSSH turns away for a while an address that fails to log in, which here is Docker's one address for every client, so the smoke test's refused keys would lock out its next steps ([step 6](../server.md#6-let-the-first-principal-in)) |
| starfixd installed from a signed release into `/opt/starfix/bin` | Built from the checkout into `/usr/local/bin`; no `systemd_unit` and no sudo rule | It tests what is checked out. Rebuild the image instead of running `starfixd upgrade` |
| starfixd may write to `~starfix/.ssh` by a `ReadWritePaths` line in its unit | Nothing confines it | A container has no systemd. sshd's `AuthorizedKeysFile` line is the guide's |
| You opt in to many projects, grant 100 database names and run `control init` once | Opted in from the first start, with 8 names; `control init` runs at every start | So there is nothing to set up. A volume made by an older image is moved at its next start |
| The host key in `/etc/ssh`; `/var/lib/dolt`, `/etc/starfix` and `~starfix/.ssh` on the disk | The same paths, linked into the `/state` volume, with the host key in `/state/hostkeys` | Everything survives a rebuild, and `down -v` wipes it |
| sshd serves every account | `AllowUsers starfix`, with passwords off | Nobody else signs in |
| A daily dump | No backups | It is for testing |
| Telemetry is set in starfixd's unit and config, and a collector runs on the machine | `STARFIX_TELEMETRY=1` sets both, and the collector runs in a container that shares the server's network namespace; metrics are exported every 5 seconds and spans within 1, and the log is exported too, which a real server does only when asked | One variable to try it, and something to look at soon after |
