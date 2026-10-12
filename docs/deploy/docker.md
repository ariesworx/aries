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
- **An SSH key**, such as `~/.ssh/id_ed25519`, loaded into ssh-agent
  (`ssh-add -l` lists it).

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

`sfx` signs in through ssh-agent. If your key is not in it, name its
file: `export STARFIX_KEY=~/.ssh/id_ed25519`. The rest of
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
STARFIX_KEY=~/sfx-scratch/bob ./sfx whoami
STARFIX_KEY=~/sfx-scratch/bob ./sfx ready
```

A key named this way is the only key `sfx` offers, so you are bob even
with your own key, alice's, in ssh-agent
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
that it is refused, and restarts the server to check that nothing
changed. Along the way it checks the keys file inside the container: a
regular file of mode 0600 owned by `starfix`, with a line for each key
added and none for a key removed or a disabled principal's; that a key
in both files is marked, comes in by starfixd's file, which sshd reads
first, warns when it is removed from the control database, and then
comes in by `authorized_keys`; that a key added to `authorized_keys`
by hand keeps its principal, whoever tries to add it to the control
database and whichever was there first; and that starfixd's log has no
failed write. It removes its server and volume, prints one line (97
checks on 12 October 2026), and exits non-zero on the first failure. CI
does not run it; `scripts/gate.sh --smoke` runs it after the gate. It
hides ssh-agent from `sfx`, so the keys you hold play no part. Arguments
go to `docker build`; behind a proxy that
intercepts TLS, pass its CA with `--secret id=ca,src=FILE`, which never
reaches the image.

`STARFIX_IMAGE=NAME` tests an image you built instead of building
`starfix-dev:local`, and `STARFIX_SMOKE_LOG=FILE` saves the server's log
before the server is removed.

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
| You are not who you meant to be | With no key named, `sfx` offers every key in ssh-agent and the server takes the first it knows | `sfx whoami` says who you are and by which key. Name the key you mean with `STARFIX_KEY=FILE`: it is then the only one offered |
| `sfx: localhost:2222 closed the connection after too many refused keys` | ssh-agent holds more keys than sshd tries, six by default, and yours is not among the first | Name your key with `STARFIX_KEY=FILE`, the private key or its `.pub` |
| `sfx: key: in .starfix.yaml names …, outside the repository` | `.starfix.yaml` is committed, so its `key:` may name only a file inside the repository | Remove the line and set `STARFIX_KEY` to the file instead |
| `sfx: localhost:2222 accepted no key` for a key added with `sfx admin` | sshd did not find the key in either file | `docker compose logs` shows what sshd and starfixd said, and [Look inside](#look-inside) shows the keys file |
| `sfx: host key mismatch for localhost` after `docker compose down -v` | The volume held the host key and the project, and the next start made new ones | Save the newly printed block as `.starfix.yaml` |

## Look inside

| To | Run |
|---|---|
| Follow the logs of all three programs | `docker compose logs -f` |
| Open a shell | `docker compose exec starfix bash` |
| Query Dolt as its own user | `docker compose exec -u dolt -w /var/lib/dolt/data/starfix starfix dolt sql` |
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
| Ubuntu 24.04 with systemd units for Dolt and starfixd | Ubuntu 24.04; the entrypoint starts the three programs and stops all of them when one exits | A container has no systemd. Each program still runs as its own user with a bare environment, `umask 077` and no way to gain privileges |
| starfixd installed from a signed release into `/opt/starfix/bin` | Built from the checkout into `/usr/local/bin`; no `systemd_unit` and no sudo rule | It tests what is checked out. Rebuild the image instead of running `starfixd upgrade` |
| starfixd may write to `~starfix/.ssh` by a `ReadWritePaths` line in its unit | Nothing confines it | A container has no systemd. sshd's `AuthorizedKeysFile` line is the guide's |
| You opt in to many projects, grant 100 database names and run `control init` once | Opted in from the first start, with 8 names; `control init` runs at every start | So there is nothing to set up. A volume made by an older image is moved at its next start |
| The host key in `/etc/ssh`; `/var/lib/dolt`, `/etc/starfix` and `~starfix/.ssh` on the disk | The same paths, linked into the `/state` volume, with the host key in `/state/hostkeys` | Everything survives a rebuild, and `down -v` wipes it |
| sshd serves every account | `AllowUsers starfix`, with passwords off | Nobody else signs in |
| A daily dump | No backups | It is for testing |
