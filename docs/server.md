# Running a server

> **This guide still describes Dolt.** Aries now stores its data in
> Postgres, and this guide is rewritten for it when the port's
> operations slice lands; until then its setup, backups and tuning are
> Dolt's ([design, decision 79](design/aries.md)).

An Aries server is one Linux machine running three programs:

| Program | Role |
|---|---|
| sshd | The front door. It checks each person's or agent's key and runs `ariesd stdio` for them |
| `ariesd serve` | The daemon. It owns the data and applies every change in order |
| Dolt | The database, listening on loopback for ariesd alone |

A server holds one project, as the steps below set it up. It can be
switched to hold many, each in its own database ([Serving many
projects](#serving-many-projects)). This page sets a server up step by
step, then covers principals, settings, limits, logs, metrics and health
([Observability](#observability)), upgrades, backups and troubleshooting.
For complete deployments with costs, see the
[deployment examples](deploy/README.md).

## What a server needs

- **Linux**, amd64 or arm64, with systemd and OpenSSH. `ariesd serve`
  refuses to run on other systems, because only Linux lets it check which
  user connects to its socket.
- **Ubuntu 24.04 LTS and 26.04 LTS are the supported platforms.** The
  steps below are written for both and were run on both, under systemd,
  in virtual machines on arm64 ([What was run, and
  where](#what-was-run-and-where)). amd64 and cloud providers' images
  were not. Other distributions, and Ubuntu's releases between the two,
  may work and are untested. One text serves both; where 26.04 behaves
  differently the guide says so: its `sudo` is sudo-rs, its sshd turns
  away for a while an address that fails to log in
  ([step 6](#6-let-the-first-principal-in)), its clock is kept by chrony
  ([Keep the clock synchronized](#keep-the-clock-synchronized)), and its
  AppArmor stacks a profile a service changes to
  ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)).
- **Systems that run SELinux are not supported yet**: RHEL, Rocky Linux,
  AlmaLinux, Fedora. Nothing here has been run on one, and this guide
  sets no SELinux label and ships no policy module. That support will
  follow once Aries has a test harness that boots such a machine.
  Ubuntu's mandatory access control is AppArmor. Stock Ubuntu, 24.04 or
  26.04, confines neither sshd nor ariesd with it; profiles for ariesd ship
  with Aries, as an optional step
  ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)).
- **A synchronized clock.** Leases and the order of history depend on
  it; ariesd detects a clock that jumps or goes back and, by default,
  refuses to start when the clock is behind its own history
  ([The server's clock](#the-servers-clock)).
- **Dolt**, at the release Aries is tested with: `Dolt` in
  [internal/version](../internal/version/version.go), 2.4.2 for Aries
  v0.2.0. Each release's notes name it.
- **Little hardware.** Idle, Dolt used about 90 MB of memory and ariesd
  about 20 MB, or 22 MB with metrics pushed and scraped (resident
  memory on arm64, a minute after its start; about 14 MB of either is
  the program itself, mapped from its file). Each connection adds about
  5 MiB more: 2.5 MiB for its bridge (`ariesd stdio`), 2 MiB for
  sshd's two processes, half a MiB in the kernel and under 0.1 MiB in
  ariesd, so 64 connections take about 320 MiB and 1,024 about
  5 GiB. That is with transparent huge pages in `madvise` mode, Ubuntu's
  default; with them `always` (`cat
  /sys/kernel/mm/transparent_hugepage/enabled`) the bridge takes about
  6 MiB, and a connection about 9 (measured on Linux, arm64, OpenSSH 9.6,
  14 October 2026; [the profile](design/profile-ariesd.md#2-a-bridge-that-is-not-the-whole-of-ariesd)
  has how). Under the stage 0 load test Dolt reached 1.4 GB and about
  250 writes a second on 4 vCPUs
  ([results](https://github.com/ariesworx/aries/blob/fbbbf4655665845db81a72d2bbd1845472fd1c5a/spike/dolt/RESULTS.md)), roughly ten times what a busy
  team of agents needs. 2 GB of memory suits a team with one project of
  a few thousand issues; [Dolt's memory](#dolts-memory) says how it grows
  and how to get it back. The data starts at under 1 MB.

## Set up a server

Run these steps as root (`sudo -i`). Each step ends with a command that
shows it worked, and none puts a password on a command line. On a fresh
Ubuntu 24.04 LTS or 26.04 LTS machine, the installer does them for you
([below](#with-the-installer)); the steps stay the reference for what
it does and why.

### With the installer

`install-server.sh`, beside `install.sh` at the top of the repository,
does the steps of this section, and those of [Opt in](#opt-in) and
[Keys from the store](#keys-from-the-store), on a fresh Ubuntu 24.04 LTS
or 26.04 LTS machine. It sets up a server of [many projects](#serving-many-projects)
from the start: admins add people and projects with `aries admin`, from
anywhere. Copy the first admin's public key to the machine, read the
script, and run it as root, first with `--dry-run`:

```sh
curl -fsSLO https://raw.githubusercontent.com/ariesworx/aries/main/install-server.sh
sudo sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com --dry-run
sudo sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com
```

It prints each step as it goes, each thing it finds already done, and
each command it runs; it ends with the `.aries.yaml` to commit
([step 7](#7-connect-a-repository)), which pins the host key sshd
presents now. [Rotating the host key](#rotating-the-host-key) says how
to change that key later without refusing every client.

| Option | Does |
|---|---|
| `--admin NAME` | The first server admin: named under `admins:`, and given a line of `authorized_keys` |
| `--admin-key FILE` | That admin's public key, a `.pub` file |
| `--host HOST`, `--port PORT` | The address clients reach sshd at, for `.aries.yaml`. The host defaults to `hostname -f`, the port to 22 |
| `--project NAME` | The name the first project is adopted under (`control init --name`). Default `first` |
| `--version vX.Y.Z` | The release of `ariesd` and `aries` to install. Default the latest |
| `--releases SOURCE` | Where releases are read from ([Install](install.md#where-releases-are-read-from)), also written to the config file as `releases:` for `ariesd upgrade`. Default `$ARIES_RELEASES`, or GitHub |
| `--apparmor` | Also confine ariesd ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)). Off unless given |
| `--dry-run` | Print every action, and change nothing |

Its steps, and the sections of this guide each one follows:

| Step | | Follows |
|---|---|---|
| 1 | Check the machine | [What a server needs](#what-a-server-needs): Ubuntu 24.04 or 26.04, amd64 or arm64, systemd, OpenSSH |
| 2 | Install Dolt | [1. Install Dolt](#1-install-dolt), with the checksum compared for you |
| 3 | Run Dolt under its own account | [2. Run Dolt under its own account](#2-run-dolt-under-its-own-account) |
| 4 | Install ariesd | [3. Install ariesd](#3-install-ariesd), by `install.sh`, which checks the signature and the checksum; and `aries` in `/usr/local/bin` |
| 5 | Create the databases and the config file | [4. Create the database and the config file](#4-create-the-database-and-the-config-file), and [Opt in](#opt-in) steps 1, 2 and 4: the control database and 100 database names for new projects |
| 6 | Run ariesd under systemd | [5. Run ariesd under systemd](#5-run-ariesd-under-systemd), and [Keys from the store](#keys-from-the-store) step 1 |
| 7 | Let the first admin in, and have sshd read the keys file | [6. Let the first principal in](#6-let-the-first-principal-in), and [Keys from the store](#keys-from-the-store) step 2 |
| 8 | Confine ariesd with AppArmor | [Confine ariesd with AppArmor](#confine-ariesd-with-apparmor), with `--apparmor` |
| 9 | Adopt the database as the first project | [Opt in](#opt-in) step 3 |
| 10 | Start ariesd | [5. Run ariesd under systemd](#5-run-ariesd-under-systemd), and [Keys from the store](#keys-from-the-store) step 3 |
| 11 | Connect a repository | [7. Connect a repository](#7-connect-a-repository) |

- **Run it again at any time.** Each step checks before it acts, so a
  second run changes nothing and ends `Nothing to change`, and a run
  that was interrupted is finished by running it again. `control init`
  runs each time, as [Opt in](#opt-in) allows, and says that it added
  nothing.
- **It changes no file it did not write.** Where a file it writes
  already exists with other text, it shows the difference and stops,
  and you choose: make it the guide's, or move it aside and run again.
  Of the files it did not create, it only appends the admin's line to
  `authorized_keys`, and prints the line.
- **sshd is checked before it is reloaded.** The drop-in is checked with
  `sshd -t`; if that fails, the new file is removed again and sshd is
  not reloaded.
- **No secret is on a command line or in its output.** The database
  password is generated into the config file, mode 0600, before Dolt is
  given it, so a run interrupted between the two reads it back. Dolt's
  `root` gets a generated password nobody keeps. The SQL travels on
  stdin, run as Dolt's own user from its data directory, and is printed
  with `<generated>` in place of each password. `--dry-run` prints the
  config file the same way.
- **The files it writes are this guide's,** byte for byte: Dolt's config
  and unit, ariesd's unit and its two drop-ins, the sudo rule and
  sshd's drop-in. A test (`scripts/installserver_test.go`) fails when
  they differ from the blocks above, or when its steps and this table
  do.
- **Where its parts come from.** `install.sh` and the AppArmor profiles
  are read from beside it when it runs from a checkout of Aries, and
  otherwise from `raw.githubusercontent.com`, as the steps download
  them. It forwards `ARIES_RELEASES` (or `--releases`) to `install.sh`.
  Dolt comes from Dolt's GitHub releases, or from
  `ARIES_DOLT_BASE_URL`, and is checked against the pinned checksum
  either way.
- **What it leaves to you:** the firewall (`ufw limit 22/tcp`, step 6),
  `PasswordAuthentication no` and, on 26.04, `PerSourcePenaltyExemptList`,
  [telemetry](#observability), [backups](#backups-and-restore), and
  upgrades (`ariesd upgrade`). Keep the admin's line in
  `authorized_keys`: it is the way in when nothing else is
  ([Keys from the store](#keys-from-the-store), step 5).
- **It refuses** another system, a machine without systemd, a user other
  than root (`--dry-run` included, since it reads root's files), and a
  machine set up some other way: a `dolt` that is not the pinned
  release, accounts with other homes or shells, a config file it did not
  write. Each refusal says what to do.

What was run, on 14 October 2026, with `ariesd` and `aries` built from
the source tree for linux/arm64 and installed by `install.sh` from a
release tree in a directory, signed with a test key, since the
installer is newer than any release. That run set the key with
`ARIES_PUBKEY_PEM`, which `install.sh` no longer reads (decision 88);
to repeat it, run the installer from a checkout whose `install.sh`
holds the test key in place of the release key:

- **In Docker** (Docker Desktop on an Apple silicon Mac), Ubuntu 24.04.5
  with systemd 255 as its first process: a dry run that changed
  nothing; a run; a second run and a dry run, which found nothing to
  change and modified no file; runs stopped by a signal in step 3,
  between the config file and the SQL of step 5, and in step 6, each
  finished by the next run; a drop-in that differed, shown and left; an
  `sshd -t` that failed, with the new drop-in removed and sshd not
  reloaded; `--apparmor` refused where AppArmor does not run. Then, from
  the host, through the container's sshd: the first admin connected,
  added a project and a member with a key, the member connected and was
  refused the first project, and once the key was removed it was
  refused at sshd. During one run every process's arguments were
  sampled about 100,000 times, and neither they nor the output held the
  generated passwords.
- **In a virtual machine** (Lima 2.2.1, the cloud image of
  [What was run, and where](#what-was-run-and-where), freshly made):
  a run with `--apparmor`, after which `ariesd serve` ran as
  `ariesd-serve (enforce)`, `systemd-analyze verify` accepted both
  units, and the kernel's log held no denial; the same admin and member
  checks with `aries` from the host; a second run, with and without
  `--apparmor`, that changed nothing; a reboot, after which both units
  were active, with no failed start and no denial, and a run changed
  nothing.
- **On Ubuntu 26.04**, the same day, the same way: in Docker, Ubuntu
  26.04.1 with systemd 259, sudo-rs and Rust coreutils, as the release
  ships them: a dry run, a run (34 changes), a second run that changed
  nothing, runs stopped by a signal in step 3, between the config file
  and the SQL of step 5, and in step 6, each finished by the next, and
  the three refusals above; then the same `aries` checks from the host,
  against the server of the full run and that of the stopped ones. In a
  fresh virtual machine (Lima 2.2.1, Ubuntu's cloud image of 26.04.1,
  kernel 7.0, OpenSSH 10.2p1, AppArmor 5.0.2, enforced): a run with
  `--apparmor`, the `aries` checks from the host, reruns with and without
  `--apparmor` that changed nothing, and cold boots, after two of which
  both units were checked and active, with no failed start and nothing
  of ariesd's denied, and a run changed nothing.
  `systemd-analyze verify` accepted both units, and `systemd-analyze
  security` scores them as on 24.04. What differed is in the steps it
  concerns: the profiles needed two changes
  ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)),
  sshd's penalties ([step 6](#6-let-the-first-principal-in)), and the
  time service ([Keep the clock synchronized](#keep-the-clock-synchronized)).
  The installer itself changed only to accept 26.04, and to keep
  sudo-rs's `visudo` from printing `parsed OK`. Then on 24.04 again, in
  Docker and in a fresh virtual machine with `--apparmor`, a run, a
  rerun that changed nothing and the `aries` checks, with the changed
  profiles, and a cold boot with no denial.

Not run: amd64; a real release downloaded from GitHub, and
`raw.githubusercontent.com` as the source of `install.sh` and the
profiles (both runs read them from a checkout); a cloud provider's image;
a machine set up by hand beforehand.

### 1. Install Dolt

Use `arm64` in place of `amd64` on an Arm machine:

```sh
curl -fsSLO https://github.com/dolthub/dolt/releases/download/v2.4.2/dolt-linux-amd64.tar.gz
sha256sum dolt-linux-amd64.tar.gz
tar -xzf dolt-linux-amd64.tar.gz
install -m 0755 dolt-linux-amd64/bin/dolt /usr/local/bin/dolt
dolt version
```

Before you unpack it, compare the checksum with the one Aries's own
tests download Dolt 2.4.2 by
([deploy/docker/Dockerfile](../deploy/docker/Dockerfile)):

| File | SHA-256 |
|---|---|
| `dolt-linux-amd64.tar.gz` | `5d95a271149dc6ea97f47157af7ba9a0df5e8d4e9bbac4d79c169f2faa3cfb9e` |
| `dolt-linux-arm64.tar.gz` | `c1c610e22c6bacd4c5b2fbf5697a0f453d759d075c4abff380c6e646553e22d8` |

### 2. Run Dolt under its own account

Dolt gets its own Unix user and listens on loopback only:

```sh
useradd --system --home-dir /var/lib/dolt --shell /usr/sbin/nologin dolt
install -d -o dolt -g dolt -m 0700 /var/lib/dolt /var/lib/dolt/data /var/lib/dolt/cfg
install -d -m 0755 /etc/dolt
cat > /etc/dolt/config.yaml <<'EOF'
log_level: info
listener:
  host: 127.0.0.1
  port: 3306
metrics:
  host: 127.0.0.1
  port: 11228
  labels: {}
data_dir: /var/lib/dolt/data
cfg_dir: /var/lib/dolt/cfg
system_variables:
  secure_file_priv: /var/lib/dolt/no-files
EOF
cd /var/lib/dolt
sudo -u dolt -H dolt config --global --add user.name "aries server"
sudo -u dolt -H dolt config --global --add user.email aries@localhost
sudo -u dolt -H dolt config --global --add metrics.disabled true
```

- `secure_file_priv` names a directory that does not exist, so no account
  can read or write files through Dolt, whatever its grants. ariesd
  refuses to start when it is empty ([why](security-model.md#the-dolt-account)).
- `cfg_dir` keeps Dolt's accounts outside the data directory.
- **`metrics:` turns on Dolt's own metrics**, in Prometheus's text
  format, at `http://127.0.0.1:11228/metrics`. ariesd reads them for
  [the status report](#watching-a-server), and a collector on the
  machine can scrape them. **Set up this way the endpoint has no
  authentication, so `host` must stay a loopback address:** any user on
  the machine can read it, and with any other address so could the
  network. It says how
  much memory and CPU Dolt uses, how many connections and queries it
  has, how long queries take, its version and the Go runtime's figures,
  and the machine's CPU, memory and disk in use; no database's name, no
  query's text, no account and no row
  ([security model](security-model.md#dolts-metrics-endpoint)). The port
  is one this guide chose: Dolt has no default, 11228 is free on stock
  Ubuntu, and it is neither Dolt's SQL port nor the 9464 the guide gives
  [ariesd's own listener](#the-listener-metrics-to-scrape-and-health).
  Leave the block out to run without it; the status report then says
  nothing of Dolt.
- Run `dolt` commands from a directory the `dolt` user can read, such as
  `/var/lib/dolt`, or it fails to load its config.

Then run it under systemd. Save this as `/etc/systemd/system/dolt.service`:

```ini
[Unit]
Description=Dolt SQL server for aries
After=network.target

[Service]
User=dolt
Group=dolt
Environment=HOME=/var/lib/dolt
WorkingDirectory=/var/lib/dolt/data
ExecStart=/usr/local/bin/dolt sql-server --config /etc/dolt/config.yaml
ExecStartPost=/bin/bash -c 'for _ in {1..300}; do (exec 3<>/dev/tcp/127.0.0.1/3306) 2>/dev/null && exit 0; sleep 0.2; done; echo "Dolt is not listening on 127.0.0.1:3306" >&2; exit 1'
Restart=on-failure
RestartSec=5
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/dolt
PrivateTmp=yes
PrivateDevices=yes
IPAddressDeny=any
IPAddressAllow=localhost
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
```

```sh
systemctl daemon-reload
systemctl enable --now dolt
journalctl -u dolt -n 3
curl -s --retry 5 --retry-connrefused http://127.0.0.1:11228/metrics | grep -c '^dss_'
```

**`ExecStartPost` makes the unit started only once Dolt listens.** Dolt
tells systemd nothing, so without the line systemd counts the unit as
started the moment its process exists, before Dolt accepts a
connection, and whatever is ordered after it, ariesd first, starts
against a port that refuses. The line tries the port five times a
second and returns at the first connection; until then the unit is
`activating` and nothing ordered after it starts. If Dolt does not
listen within a minute the start fails with `Dolt is not listening on
127.0.0.1:3306`, and `Restart=on-failure` tries again. It needs only
bash, and runs as the `dolt` user under the unit's own restrictions. If
you change `listener.port` in Dolt's config, change it here too.

The log ends with `Server ready. Accepting connections.`, and the last
command counts the lines of Dolt's metrics about its SQL server: 14 at
Dolt 2.4.2. A 0 means the endpoint does not answer: check the `metrics:`
block and `journalctl -u dolt`.

### 3. Install ariesd

ariesd runs as its own user, `aries`. sshd runs every key's forced
command as this user:

```sh
useradd --system --create-home --home-dir /var/lib/aries --shell /bin/sh aries
usermod -p '*' aries
install -d -o aries -g aries -m 0755 /opt/aries/bin
cd /var/lib/aries
curl -fsSL https://raw.githubusercontent.com/ariesworx/aries/main/install.sh | sudo -u aries -H sh -s -- --server --dir /opt/aries/bin --require-signature
ln -sfn /opt/aries/bin/ariesd /usr/local/bin/ariesd
ariesd version
```

- **The shell must be real.** sshd runs a forced command through the
  account's shell, so `/usr/sbin/nologin` would refuse every connection.
- **The password is `*`, not locked.** No password matches `*`. A locked
  account (`passwd -l`, a `!` in `/etc/shadow`) makes sshd refuse even
  keys when PAM is off.
- **Aries owns the binary's directory**, so `ariesd upgrade` can
  replace it without root. `/usr/local/bin/ariesd` gives the forced
  command a path that never changes.
- The installer checks the release's signature and checksum
  ([Install](install.md)).

### 4. Create the database and the config file

This creates Aries's database and an account with rights on it alone,
gives Dolt's `root` a random password, and writes the config file. Both
passwords travel on stdin:

```sh
cd /var/lib/dolt
(
  set -e
  umask 077
  dbpw=$(openssl rand -hex 24)
  rootpw=$(openssl rand -hex 24)
  printf "CREATE DATABASE aries;\nCREATE USER 'aries'@'localhost' IDENTIFIED BY '%s';\nGRANT ALL ON aries.* TO 'aries'@'localhost';\nALTER USER 'root'@'localhost' IDENTIFIED BY '%s';\n" "$dbpw" "$rootpw" |
    sudo -u dolt -H env DOLT_CLI_PASSWORD= dolt --host 127.0.0.1 --port 3306 --no-tls -u root sql
  install -d -o root -g aries -m 0750 /etc/aries
  printf 'dsn: aries:%s@tcp(127.0.0.1:3306)/aries\nproject: %s\nsystemd_unit: ariesd.service\nadmins: [alice]\ntelemetry:\n  dolt_metrics_url: http://127.0.0.1:11228/metrics\n' "$dbpw" "$(cat /proc/sys/kernel/random/uuid)" |
    install -o aries -g aries -m 0600 /dev/stdin /etc/aries/ariesd.yaml
)
grep project /etc/aries/ariesd.yaml
```

- **The account can do anything to its one database and nothing else**:
  no global privilege, no `FILE`, no `GRANT OPTION` (`ALL` does not include
  it). ariesd checks this every time it opens the store.
- **Nobody needs `root`'s password**, so the step forgets it. Dolt's own
  user can still administer the server from the data directory:
  `cd /var/lib/dolt/data/aries && sudo -u dolt -H dolt sql`. You may
  drop `root` instead (`DROP USER 'root'@'localhost';`); Dolt does not
  bring it back on restart.
- **`project`** is the project's UUID. Every repository's `.aries.yaml`
  names it.
- **`admins`** lists the first principal, `alice`, who you let in at step
  6. [Settings](#settings) lists every other key.
- **`telemetry: dolt_metrics_url:`** is where ariesd reads Dolt's own
  metrics, which step 2 turned on, for
  [the status report](#watching-a-server). Leave the two lines out if
  you left `metrics:` out of Dolt's config.

The step runs once: a second run finds `root` protected and stops.

### 5. Run ariesd under systemd

Save this as `/etc/systemd/system/ariesd.service`:

```ini
[Unit]
Description=Aries server
After=network.target dolt.service time-sync.target
Wants=time-sync.target
Requires=dolt.service

[Service]
Type=notify
NotifyAccess=main
TimeoutStartSec=600
WatchdogSec=120
User=aries
Group=aries
ExecStart=/opt/aries/bin/ariesd serve
Restart=on-failure
RestartSec=5
RestartPreventExitStatus=75
RuntimeDirectory=aries
RuntimeDirectoryMode=0700
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX

[Install]
WantedBy=multi-user.target
```

`RuntimeDirectory` makes `/run/aries` with mode 0700, so only the
`aries` user reaches the daemon's socket.

| Line | Does |
|---|---|
| `Type=notify` | systemd counts the unit as started when ariesd says it is ready, which it does once it listens on its socket: after the database is open and, on a server of many projects, every project migrated. Until then `systemctl start` waits, and units ordered after this one do not start |
| `NotifyAccess=main` | Only ariesd's own process may send those notifications |
| `TimeoutStartSec=600` | How long systemd waits for that. Its default is 90 seconds, after which it fails the start and `Restart=on-failure` tries again; an upgrade that migrates many projects can take longer than that, so the unit gives it 10 minutes |
| `WatchdogSec=120` | ariesd must say it is alive at least every 120 seconds, or systemd kills it and `Restart=on-failure` starts it again. ariesd says so every 60, half the interval, from the goroutine that runs its 30-second reaper, so a daemon whose reaper hangs is restarted. One tick of the reaper that takes longer than `WatchdogSec` is a hang too, and a Dolt that has stopped answering can cause one: each of the reaper's writes waits up to a minute |
| `Wants=time-sync.target` and `time-sync.target` in `After=` | Order ariesd after the clock is synchronized, on a machine where something provides that target. Stock Ubuntu, 24.04 or 26.04, reaches it at once, so there the lines wait for nothing ([Start ariesd after the clock is set](#start-ariesd-after-the-clock-is-set)) |
| `RestartPreventExitStatus=75` | Leaves ariesd stopped when it refuses to start because the clock is behind its own history, which no restart can correct ([The server's clock](#the-servers-clock)). It exits with that status 0.13 seconds after systemd starts it, before it would tell systemd it is ready, so the start fails then and not when `TimeoutStartSec` runs out. Every other failure is still restarted |

Without the first four lines ariesd runs as before: it sends a
notification only when systemd asks for one.

The lines were run on the machine of
[What was run, and where](#what-was-run-and-where); what the two for
the clock showed is in [The server's clock](#the-servers-clock):

- **The unit is active only once ariesd serves.** systemd logs
  `Started ariesd.service` within a millisecond of ariesd's own
  `ariesd serving`, and until then the unit is `activating`.
- **A daemon that hangs is restarted.** With the daemon frozen
  (`SIGSTOP`), systemd logged `Watchdog timeout (limit 2min)!` 120
  seconds after the last sign of life, killed it with `SIGABRT` and
  started it again five seconds later. Go answers `SIGABRT` by printing
  every goroutine's stack, so the journal then holds what the daemon was
  doing. A healthy daemon was left alone.
- **`systemctl stop` is clean**: ariesd tells systemd it is stopping,
  closes its stores and exits with status 0.
- **If you use the [AppArmor profiles](#confine-ariesd-with-apparmor),
  install this release's with the unit.** The daemon's profile from
  before these lines existed does not let it write to systemd's socket:
  the daemon then serves, logs `could not tell systemd the daemon is
  ready`, and the unit stays `activating` until `TimeoutStartSec` runs
  out, when systemd stops it and starts it again
  ([Troubleshooting](#troubleshooting)).

Then let `ariesd upgrade` restart the unit, and start it:

```sh
echo 'aries ALL=(root) NOPASSWD: /usr/bin/systemctl restart ariesd.service' > /etc/sudoers.d/aries
chmod 0440 /etc/sudoers.d/aries
visudo -cf /etc/sudoers.d/aries
systemctl daemon-reload
systemctl enable --now ariesd
journalctl -u ariesd -n 3
```

The log shows `ariesd serving` with the socket, the project and the
admins. If it shows a refusal instead, its `fix:` says what to change.

**ariesd starts once Dolt listens,** at boot and whenever Dolt's unit
restarts. `After=dolt.service` orders it after Dolt's unit, and that
unit is started only when Dolt accepts connections (its `ExecStartPost`,
[step 2](#2-run-dolt-under-its-own-account)). `Requires=dolt.service`
has systemd stop ariesd with Dolt, on `systemctl restart dolt` or
after a crash, and start both again in that order.

**A server set up from this guide before 13 October 2026 has no such
line,** and there ariesd's first start fails at every boot and at
every restart of Dolt: ariesd finds no database (`dial tcp
127.0.0.1:3306: connect: connection refused`) and exits, and
`Restart=on-failure` starts it again five seconds later, which works.
The journal holds one failed start each time (`Failed to start
ariesd.service`), and `aries` answers `ariesd is not running on the
server` for those seconds. `Type=notify` does not remove that race,
since it is Dolt's unit that is counted as started too early. To remove
it, add the `ExecStartPost` line of step 2 to `dolt.service` and run
`systemctl daemon-reload`; it takes effect when Dolt next starts.

The line is in Dolt's unit, and not an `ExecStartPre` in ariesd's,
because the [AppArmor profile](#confine-ariesd-with-apparmor) the
daemon's unit may ask for confines every command of that unit, and it
allows the daemon to run nothing.

### 6. Let the first principal in

Each key gets one line in `~aries/.ssh/authorized_keys`. The line's
forced command names the principal, so sshd, not the client, decides who
connects:

```sh
install -d -o aries -g aries -m 0700 /var/lib/aries/.ssh
echo 'restrict,command="/usr/local/bin/ariesd stdio --principal alice" ssh-ed25519 AAAA… alice@example.com' >> /var/lib/aries/.ssh/authorized_keys
chown aries:aries /var/lib/aries/.ssh/authorized_keys
chmod 0600 /var/lib/aries/.ssh/authorized_keys
```

`restrict` turns off forwarding, terminals and the rest. Never add a
`ForceCommand` for the account: it would override each key's `command=`,
and so its principal.

Then harden sshd for the account. Save this as
`/etc/ssh/sshd_config.d/00-aries.conf`:

```text
MaxStartups 10:30:60
ClientAliveInterval 60
Match User aries
    AuthenticationMethods publickey
    DisableForwarding yes
    PermitTTY no
    AuthorizedKeysCommand none
    TrustedUserCAKeys none
    AuthorizedPrincipalsFile none
```

The last three lines make the account's keys come from its own key
files and nowhere else. Without them, whatever the machine's sshd trusts
for every account applies to `aries` too: a cloud image's own key
command (Google Cloud's OS Login, AWS's EC2 Instance Connect, Azure's
Entra ID login), or a certificate authority your company's images trust.
A key such a command prints, or a certificate from such an authority
that names `aries`, would be let in with no forced command, which is a
shell as the account that can read ariesd's config file. Checked on 12
October 2026 with OpenSSH 9.6 on Ubuntu 24.04: with a certificate
authority trusted outside the `Match` block, a certificate for the
principal `aries` ran a command as the account; with these lines it is
refused. Checked again on 14 October 2026 with OpenSSH 10.2 on Ubuntu
26.04, with the drop-in as the installer writes it: with an authority
trusted for every account, one certificate for two principals let in
the machine's own user and was refused for `aries`.

```sh
install -d -m 0755 /run/sshd
sshd -t
systemctl try-reload-or-restart ssh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

- `sshd -t` checks the config. It needs `/run/sshd`, which systems that
  start sshd on the first connection have not made yet.
- The unit is `sshd` rather than `ssh` on some distributions; Ubuntu
  26.04 answers to both.
- `DisableForwarding` means no tunnel to Dolt, even from a key line that
  lacks `restrict`.
- `MaxStartups` drops floods of unauthenticated connections early.
- `ClientAliveInterval` keeps an agent's long session alive through
  tunnels that drop idle connections.
- A `Match` block in a drop-in file applies to that file only (OpenSSH 9.6
  and 10.2 checked).
- On a machine whose admins all sign in with keys, also add
  `PasswordAuthentication no` above the `Match` line.

The last command prints the host key fingerprint, `SHA256:…`. Give it to
everyone who connects. To change the key later without refusing every
client at once, see [Rotating the host key](#rotating-the-host-key).

**On a public port**, also rate-limit new connections at the firewall
(`ufw limit 22/tcp`). Skip that when everyone arrives through a few shared
addresses (Google Cloud IAP, a VPN or a NAT), because it would throttle
the whole team. To keep sshd off the internet altogether, put it behind a
VPN such as WireGuard; Aries works the same either way.

OpenSSH 9.8 and later also turn away, for a while, an address that fails
to log in or crashes a session (`PerSourcePenalties`, on by default).
Behind shared addresses, one bad client could then lock out the team, so
list those ranges in `PerSourcePenaltyExemptList`, above the `Match` line
(it is a global setting). **Ubuntu 26.04 has OpenSSH 10.2, so there this
is on;** 24.04's OpenSSH 9.6 has no such setting, and a line naming it
fails `sshd -t` there. What 26.04 showed, with its defaults (`sshd -T |
grep persourcepenalties`):

- A principal whose key was removed tried four times; sshd then dropped
  every connection from that address for about 25 seconds, the admin's
  among them (`drop connection … penalty: failed authentication` in
  `journalctl -u ssh`). `aries` says `HOST:PORT closed the connection
  before the ssh handshake` (before 15 Oct 2026, only `ssh handshake
  with HOST:PORT failed: ssh: handshake failed: EOF`).
- A connection that ends before it tries to log in counts too (`penalty:
  connections without attempting authentication`): 16 from `aries`
  refusing a host key that no longer matched its pin turned the address
  away, and each refused retry lengthened it. A client that keeps
  retrying with an old pin, or a port check, can keep its address out.
  Before you [rotate the host key](#rotating-the-host-key), have clients
  pin the new key.
- Penalties last at most 10 minutes (`max:600`). An address in
  `PerSourcePenaltyExemptList` is not turned away by them, as
  `sshd_config(5)` says (not run here).

### 7. Connect a repository

Commit a `.aries.yaml` to the repository, with the project UUID from step
4 and the fingerprint from step 6:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: aries.example.com
  host_key: SHA256:…
```

Then, on alice's machine, inside the repository:

```console
$ aries create "Fix the login redirect" -p 1 -t bug
sf-aukz3rip
$ aries ready
sf-aukz3rip  P1  open  Fix the login redirect
```

[Using `aries`](cli.md#connect-a-repository) explains every field of the file,
and the [agent guide](agents.md) sets agents up in the repository.

### What was run, and where

Steps 1 to 7, then [Serving many projects](#serving-many-projects) with
its keys file and `aries admin`, were run in order as written on 12
October 2026, on one machine:

| | |
|---|---|
| Machine | A virtual machine on an Apple silicon Mac (Lima 2.2.1), arm64, 2 CPUs and 4 GiB, from Ubuntu's cloud image for 24.04 |
| System | Ubuntu 24.04.5, kernel 6.8.0-142, systemd 255, OpenSSH 9.6p1, sudo 1.9.15p5, AppArmor 4.0.1, Dolt 2.4.2 |
| ariesd | Built from the source tree for linux/arm64 with the release's flags, and put where step 3's installer puts a release: `/opt/aries/bin/ariesd`, owned by `aries`, mode 0755. The installer itself was not run |
| Clients | `aries` inside the same machine, against `localhost`, as three Unix users with a key each |

What that showed:

- **The units work as written.** `systemd-analyze verify` accepts both.
  `systemd-analyze security` scores ariesd's 7.2 and Dolt's 7.1
  ("medium"; lower is tighter): they run as their own users with a
  read-only file system, and nothing limits their system calls,
  capabilities or namespaces.
- **sshd's `Match` block works on stock Ubuntu.** The forced command
  runs in place of whatever command the client asks for, a terminal is
  refused
  (`PTY allocation request failed`), and a tunnel to Dolt is refused
  (`open failed: administratively prohibited`).
- **The first start after a boot fails**, and the next one works
  ([step 5](#5-run-ariesd-under-systemd)). That was with Dolt's unit
  as it then was, without its `ExecStartPost`.
- **Dolt's unit with `ExecStartPost` is started only once Dolt
  listens.** This was run on 13 October 2026, and not on the machine
  above: in a container (Docker Desktop on an Apple silicon Mac, arm64)
  of Ubuntu 24.04 with systemd 255.4 as its first process and Dolt
  2.4.2 set up by steps 1 and 2, with a stand-in for ariesd: a unit
  with ariesd's `After=dolt.service` and `Requires=dolt.service` that
  tries the port once. Started from stopped five times each way,
  without the line every attempt was refused, and with it every attempt
  connected; the start took 0.28 seconds in place of 0.03. A connection
  made at once after `systemctl restart dolt` succeeded three times of
  three with the line. With a process in Dolt's place that never
  listens, the start failed after 63 seconds and systemd tried again.
  `systemd-analyze verify` accepts the unit. Not run: ariesd itself
  ordered after the unit, a real boot, and the machine above.
- **The unit's `Type=notify`, start timeout and watchdog work**
  ([step 5](#5-run-ariesd-under-systemd)), on the same machine with
  the AppArmor profiles enforced, across a cold boot. So do the
  loopback listener, `/readyz` when Dolt stops answering, metrics
  pushed to a receiver on the machine, and `ariesd status`
  ([Observability](#observability)).
- **Traces work as written** ([Traces](#traces)), on the same machine
  on the same day with the profiles enforced. With the guide's one
  variable and nothing else ariesd starts and serves, posts spans to
  a receiver on the machine that hold no stand-in and no name, and says
  why at error level, in its status report and in `whois`: `it cannot
  be made: no such file or directory`. With the directory made and no
  restart, half a minute later: `read-only file system`. With the
  drop-in and a restart, under a profile without the rule for the file:
  `permission denied`, with the denial in the kernel's log. With the
  rule loaded and no restart, half a minute later: `made the key`,
  mode 0600, `spans name people by stand-ins from now on`, and spans
  with a stand-in. `aries admin principal whois` then resolves it for a
  server admin. With the file made readable by others and a restart:
  no stand-in again, `CANNOT BE TRUSTED` in the status; after `chmod
  600` and no restart, the same stand-in as before. An earlier build of
  the same day, which still refused to start without the key, showed
  that a restart keeps the key, that deleting the file gives the same
  person another stand-in and leaves the old one resolving to nobody,
  that `principals: name` and `OTEL_TRACES_EXPORTER=none` need no key,
  and that neither another user, nor the `aries` user under the
  bridge's or the operator's profile, can read the key. The 76 checks
  of the earlier runs passed with traces on, with no denial.
- **The clock's unit lines and settings work** ([The server's
  clock](#the-servers-clock)), on the same machine on the same day with
  the profiles enforced and no denial in the kernel's log: the refusal
  to start and exit status 75 under `Type=notify`, with and without
  `RestartPreventExitStatus=75`; each way on from it; the clock set
  back and forward under a running daemon with claims held; boots with
  and without `systemd-time-wait-sync`, and with no time server
  answering; `upgrade --check`'s `clock:` line and the query of
  [Upgrades](#upgrades). The 76 checks of the earlier runs passed
  afterwards.
- **Dolt's metrics and the status report's lines about Dolt work**
  ([Dolt's figures](#dolts-figures)), on the same machine on the same
  day, set up by the block for a server set up before, with the
  profiles enforced and no denial in the kernel's log. The endpoint
  answered on 127.0.0.1 and not on the machine's other address. `MiB
  resident` was within a mebibyte of `VmRSS` in `/proc`, the start time was
  `ps`'s, and the connections were ariesd's own, counted with `ss`.
  The collector's line gave the same cycles and writes as Dolt's counter
  and ariesd's own metric did over a burst. With the port refused by
  a firewall rule for 50 seconds the report said `Dolt: unknown` with
  the last read, `/readyz` stayed ready and `aries` worked, with one
  warning in the log and one line when the reads came back; a URL with
  the wrong port, one that is not loopback and one with `https://` did
  what [Dolt's figures](#dolts-figures) says. [Dolt's
  memory](#dolts-memory) has what the line showed under a memory limit
  too low and one with room. The 76 checks passed afterwards, once
  their script was brought up to date with this release in three
  places: each user's
  key is named by `ARIES_KEY_FILE` in place of a `key:` line outside
  the repository, `member add` for someone with a key takes
  `--accept-keys`, and the refusal to add a key to one's own account
  has new words.
- **`ariesd upgrade --rollback` works for the `aries` user**: it
  renames the binaries in `/opt/aries/bin`, restarts the unit through
  the sudo rule and finds the daemon healthy. `upgrade --check` reaches
  GitHub and compares versions.
- **The daily dump** of [Backups and restore](#backups-and-restore)
  writes its file.

Not run: amd64; a cloud provider's image or network; a real
OpenTelemetry Collector on this machine, or a hosted backend, in place
of which a few lines of Python received the pushes (an OpenTelemetry
Collector, 0.162.0, did receive and decode the spans of an ariesd run
by hand on a developer's machine); `install.sh` on the server; an upgrade to a newer release, which a build from source refuses
(`a development build … never upgrades itself`), so the download, the
signature check and the swap to a new binary were not run here; a
restore from a dump. For the clock: a machine with a real-time clock of
its own, working or dead (this guest's comes from the hypervisor at
every boot and was always right); a boot with the clock wrong; a
virtual machine paused and resumed, or restored from a snapshot; a
provider's time service; `chrony`.

**On Ubuntu 26.04**, on 14 October 2026, the installer did steps 1 to 7
and the many-projects sections
([With the installer](#with-the-installer)) on a second machine:

| | |
|---|---|
| Machine | A virtual machine on an Apple silicon Mac (Lima 2.2.1), arm64, 2 CPUs and 4 GiB, from Ubuntu's cloud image for 26.04 |
| System | Ubuntu 26.04.1, kernel 7.0.0-34, systemd 259, OpenSSH 10.2p1, sudo-rs 0.2.13 as `sudo` (the original, 1.9.17p2, installed as `sudo.ws`), Rust coreutils 0.10, chrony with NTS, AppArmor 5.0.2, Dolt 2.4.2 |
| ariesd | Built from the source tree for linux/arm64 and installed by the installer, which ran `install.sh` against a release tree signed with a test key |
| Clients | `aries` on the Mac, through the port Lima forwards to sshd; and `aries` inside the machine, against `127.0.0.1` |

What that showed, beyond what the installer's run says:

- **Every command the steps and the installer use works under sudo-rs
  and Rust coreutils as under sudo and GNU coreutils**: `sudo -u USER -H`
  with `env` and standard input, `visudo -c -f` (sudo-rs's prints
  `parsed OK` even with `-q`), the sudo rule for `systemctl restart`
  from the `aries` user, and refusing anything else; `install` with
  `-o`, `-g`, `-m` and `-d`, and from `/dev/stdin` and `/dev/null`;
  `stat -c`, `mktemp -d`, `cut -s`, `tail -c`, `sha256sum`, `readlink`,
  `ln -sfn`. Ubuntu keeps GNU's `mv` (and `cp` and `rm`).
- **The units, sshd's `Match` block** and its refusals of a terminal, a
  tunnel and a certificate, as on 24.04
  ([step 6](#6-let-the-first-principal-in)). sshd listens through
  `ssh.socket`, as on 24.04, and systemd 259 also makes it answer on
  `AF_VSOCK` in a virtual machine (`sshd-vsock.socket`), with the same
  configuration.
- **`ariesd upgrade --rollback --restart`** under the operator's
  profile, with sudo-rs and with `sudo.ws` as `sudo`, once the profile
  names both ([Confine ariesd with
  AppArmor](#confine-ariesd-with-apparmor)).
- **The time service**: `time-sync.target` and `chrony-wait.service`
  ([Start ariesd after the clock is set](#start-ariesd-after-the-clock-is-set)).

## Operate a server with Claude Code

`aries-ops` is a [Claude Code](https://code.claude.com/docs) plugin in
this repository: a skill that lets an operations team deploy and run a
server by talking to Claude, in place of running this guide's commands
themselves. It drives the installer, `aries admin` and `ariesd`, follows
this guide's sections and names the one each step comes from; it does
not replace them.

Add the repository as a marketplace, at the release your server runs,
and install the plugin, in Claude Code on your own workstation:

```text
/plugin marketplace add ariesworx/aries@v0.6.0
/plugin install aries-ops@aries
```

From a checkout, `claude plugin marketplace add ./aries` does the
same with the files as they are. The plugin has no version of its own:
it is the one at the tag you add, so add the tag your server runs, and
add the new one when you upgrade. Then ask, for example: "install an
Aries server on the VM `aries-1`", "offboard bob", "set up the daily
backups" or "why does `aries` say ariesd is not running?". `/aries-ops:aries-ops`
calls it by name.

What it does:

| Area | Covers |
|---|---|
| Provisioning | The VM, its firewall and the private path (IAP or a VPN), vendor-neutral, with [the deployment examples](deploy/README.md) as worked cases |
| First install | Preflight, [the installer](#with-the-installer)'s dry run and run, what it leaves to you, AppArmor, the first project and admin, and checking the result |
| Day 2 | [Managing access](#managing-access) and offboarding, [upgrades](#upgrades), [backups and a rehearsed restore](#backups-and-restore), [telemetry](#observability), and [troubleshooting](#troubleshooting) from the status report and the logs |
| Onboarding | A developer's or an agent's key, `.aries.yaml`, and `aries setup AGENT` ([agent guide](agents.md)) |

How it works, and what it will not do:

- **It runs on your workstation, never on the server.** It reaches the
  server with your own `ssh` and its configuration, runs root steps
  through `sudo -n`, and hands you any step that needs a password to
  run in a terminal of your own.
- **Every change is shown first, and runs only once you approve it**:
  the command, the host, the user, and what it changes, after a dry run
  where one exists. Read-only checks run freely. Offboarding, a restore,
  archiving a project, removing a key or a member, and an upgrade need
  you to name the target in your reply, and the backup or tag this guide
  has for them is taken first. It stops and asks when what it finds
  differs from what it expected.
- **No secret passes through it.** Passwords are generated on the
  server into 0600 files and never printed or put on a command line; it
  never prints the config file's `dsn:` line, never reads a private key,
  never runs `ssh-add` or forwards your agent, and never asks you to
  paste a secret. A token for a hosted backend is yours to write.
- **Admin is CLI only**: `aries admin` and `ariesd`, never an MCP tool.
- **It leaves a record**: a short summary of what was done, where, the
  versions and what is left, in a file you choose on your workstation.
- **Not in scope**: the release signing keys, and any machine you have
  not named.

Its files are under
[plugins/aries-ops](../plugins/aries-ops/skills/aries-ops/SKILL.md).
`scripts/opsskill_test.go` fails when a command or flag it names is not
one `aries` or `ariesd` has, when a section it cites is no heading of
these guides, or when its manifests or frontmatter are not what Claude
Code reads. It has no database steps of its own: Aries's backend is
moving to Postgres, and until that lands its `references/database.md`
sends Claude to this guide's current sections and has it stop and ask
before any database command. What was run: its read-only and admin commands against the
[Docker test server](deploy/docker.md), on 15 October 2026; not on a
virtual machine or a cloud provider's, and not a restore.

## Principals

A principal is a name for a person or an agent identity, such as `alice`.
It comes from the key that authenticated, never from the client: sshd
runs a forced command that names it, and nothing the client sends can
change that.

- **Name rules:** lowercase, starting with a letter, then letters, digits,
  `.`, `_` or `-`, up to 64 characters. `ariesd` (the claim reaper and
  the daemon's own changes), `import` (the bd importer and `control init`),
  `ariesd-upgrade` (the health check of `ariesd upgrade`) and
  `ariesd-status` ([`ariesd status`](#ariesd-status)) are reserved.
- **Agents on someone's machine** use that person's key, so their work is
  theirs. Give an agent that runs on its own, in CI or in a cloud sandbox,
  its own principal and key (for example `ci-bot`). Its work then shows
  under its own name, and its key can be revoked alone. A key belongs to
  one principal.

Where the key and its principal are recorded depends on the server:

| | A server of one project | A server of [many projects](#serving-many-projects) |
|---|---|---|
| A key is | A line of `~aries/.ssh/authorized_keys` whose forced command names the principal ([step 6](#6-let-the-first-principal-in)) | A row of the control database, which ariesd writes to a file sshd reads ([Keys from the store](#keys-from-the-store)). A line of `authorized_keys` still works too |
| Add someone | Append a line, on the server. A principal may have several, one per key | `aries admin principal add NAME --key FILE`, from anywhere |
| Add a key | Append a line | `aries admin key add NAME FILE` |
| Remove a key | Delete its line. That stops new connections; to end open sessions too, stop the bridges: `pkill -f -- 'stdio --principal bob$'` | `aries admin key remove FINGERPRINT`, which also closes the connections that key signed in |
| Remove someone | Delete their lines, then `pkill` as above | `aries admin principal disable NAME`, or `member remove NAME` for one project: their connections close and their claims end at once |

On a server of many projects what a principal may reach is its
memberships, managed with `aries admin` ([Managing access](#managing-access)).
A key says who someone is and nothing else.

## Admins

Admins are principals named under `admins:` in the config file, or in
`ARIESD_ADMINS` (comma-separated). `serve` reads the list when it starts.
Only an admin may change an issue another principal holds, or close one
with acceptance items still open (`close --force`). A change to an issue
another principal holds is recorded first as an `admin.override` event
that names the holder; a forced close records the open items in its close
event. Only an admin may set a model's price ([Prices](#prices)) or a
subscription plan ([Plans](#plans)), or undo another person's hours.
Nothing over the protocol or MCP reads or changes the list, and a
reserved name cannot be an admin.

On a server of [many projects](#serving-many-projects) these are the
server admins: admins in every project, and let into every project. One
whose principal is disabled there is refused like anyone disabled, for
as long as it is, though the config file still names it
([Managing access](#managing-access)).

## Serving many projects

A server can hold many projects, each in a Dolt database of its own with
its own writer, so a busy project does not queue another's writes. They
share the one Dolt, the one ariesd and the machine. A second database,
the control database, records the projects, the principals and their
keys, and which principal is a member of which project. You opt in: a
server with no `control_db` setting is the server of one project it was.

What works today:

- **Opting in, and adopting the project the server holds**, below.
- **Membership is enforced at connection.** A principal reaches a project
  only as a member of it, or as a server admin (`admins:`). Anyone else is
  refused with `forbidden`, and the fix names the command an admin runs.
  A project that is unknown or archived is refused as not served.
- **Project admins.** A member whose role in a project is `admin` may
  override a held issue and force a close in that project, and set its
  prices and plans, as a server admin may in every project. A role that
  changes counts on the next request.
- **Projects, principals, members and keys are managed from anywhere**
  with `aries admin`, over the same SSH connection as every other command
  ([Managing access](#managing-access)). A new project needs no shell on
  the server: its database comes from a block of names you grant once.
  Nor does a new person: an admin adds them and their public key, and
  ariesd writes the key to a file sshd reads
  ([Keys from the store](#keys-from-the-store)).
- **Access that ends, ends at once.** When a membership is removed, a
  principal is disabled, a project is archived or a key is removed,
  ariesd closes the connections it had let in, once the change is
  committed. The first three also end the claims the lost access held,
  so the issues are ready for someone else.
- **`aries admin status`** shows an admin the server and each project as
  they are now: open stores, connections, sessions, writes and their
  latency, and ariesd's own memory ([Watching a server](#watching-a-server)).
- **`import-bd` and `export-bd`** act on the config file's `project`, or
  on another with `--project UUID`.

Everything above is built and tested against a real Dolt and Aries's
own stand-in for sshd, including a soak test across two projects
([CONTRIBUTING.md](../CONTRIBUTING.md#the-soak-test)). The sshd setup of
[Keys from the store](#keys-from-the-store) was also run with a real
sshd, OpenSSH 9.6 on Ubuntu 24.04, in the
[Docker test server](deploy/docker.md), and then under systemd on an
Ubuntu 24.04 virtual machine
([What was run, and where](#what-was-run-and-where)). It has not been
run on a cloud provider's machine; that section says what was checked
and what was not.

### Opt in

1. Create the control database and grant it to ariesd's account, as
   Dolt's own user, the way step 4 says to administer Dolt:

   ```sh
   cd /var/lib/dolt/data/aries
   sudo -u dolt -H dolt sql -q "CREATE DATABASE IF NOT EXISTS aries_control; GRANT ALL ON aries_control.* TO 'aries'@'localhost';"
   ```

   The grant names the database exactly. Dolt ignores a grant on a name
   pattern.

2. Name it in `/etc/aries/ariesd.yaml`, leaving `dsn`, `project`,
   `prefix` and `account` as they are. They now describe the project to
   adopt:

   ```yaml
   control_db: aries_control
   ```

3. Adopt the existing database as the first project, and import the
   principals and keys of `authorized_keys`:

   ```sh
   sudo -u aries -H ariesd control init --name acme
   ```

   ```text
   project acme (6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f) adopted from database aries: active
   principals: 2 added, 0 already there; keys: 3 added, 0 already there; members: 1 added, 0 already there; 0 lines skipped
   3 keys in the store are also lines of /var/lib/aries/.ssh/authorized_keys: sshd reads that file too, so `aries admin key remove` does not keep such a key out until its line is deleted too
   ```

   The last line counts the keys that are now in both places. It is a
   reminder, and `control init` never edits `authorized_keys`: see
   [Keys from the store](#keys-from-the-store), step 5.

4. To let admins create further projects, grant ariesd's account a
   block of database names now, and name the block in the config file.
   Dolt honors a grant on a database that does not exist yet, and only on
   an exact name, so the names are listed one by one:

   ```sh
   cd /var/lib/dolt/data/aries
   for i in $(seq 1 100); do
     printf "GRANT ALL ON aries_p%03d.* TO 'aries'@'localhost';\n" "$i"
   done | sudo -u dolt -H dolt sql
   ```

   ```yaml
   project_dbs:
     prefix: aries_p
     count: 100
   ```

   The names are the prefix and a number of three digits, here
   `aries_p001` to `aries_p100`; `count` is at most 999, and the
   prefix a lowercase letter, then lowercase letters, digits and
   underscores. `aries admin project add` takes the lowest name no project
   has. To add more later, grant them the same way (`seq 101 200`)
   and raise `count`. Without `project_dbs` the server serves the
   projects it has and refuses to create one.

5. Let ariesd write its keys file and have sshd read it, which is
   also what lets you add people from anywhere, with no line in
   `authorized_keys`: [Keys from the store](#keys-from-the-store), steps
   1 and 2. ariesd does not start on a server of many projects until
   it can write that file.

6. Restart ariesd: `systemctl restart ariesd`.

`control init` records the database in `dsn` as a project, with the
config file's `project` UUID, `prefix` and `account` and the name you
give, and changes nothing in that database. Then, for each line of
`/var/lib/aries/.ssh/authorized_keys` (or `--authorized-keys FILE`)
whose forced command is `ariesd stdio --principal NAME`, it adds the
principal if it is new, adds the key, and makes the principal a member
of the project. A principal named under `admins:` gets its row and keys
and no membership, since a server admin needs none. A line it cannot use
is skipped and reported on stderr by number and reason, never by its
text.

- **Run it again whenever `authorized_keys` gains a line.** It adds what
  is new and leaves the rest: it does not demote an admin, re-activate an
  archived project, or remove anyone. A running ariesd reads the
  control database again every 30 seconds, so the new member can connect
  within the minute.
- **Run again, it does not undo what an admin did.** The file is not
  edited when an admin removes someone with `aries admin`, so its lines
  still name them. `control init` reads the control database's event log
  before it adds anything: a key whose newest event is its removal stays
  out of the control database, a principal whose newest event on the
  adopted project is its removal stays removed, and a disabled principal
  stays disabled, with nothing of its lines imported. Each is reported on
  stderr with its line number, who did it and when, and counted in a
  line of the summary:

  ```text
  authorized_keys line 2: bob was removed from the project by dana on 2026-10-12; kept removed
  left as admins set them: 0 keys removed, 1 membership removed, 0 principals disabled; to put them back from this file, run it again with --restore-removed-access
  ```

  What an admin added again after removing it is in the control
  database, and is simply already there. A key left out of the control
  database is still a line of the file, where sshd accepts it: delete the
  line to keep the key out.
- **`--restore-removed-access` overrides those admins.** With it,
  `control init` puts back, for every principal the file names, the keys
  removed with `aries admin key remove`, the memberships of the adopted
  project removed with `aries admin member remove`, and the principals
  disabled with `aries admin principal disable`. Each restoration is an
  event under `import`, and is reported. Use it when the file is what
  you mean, for example after restoring a control database from an old
  backup. It is an operator's command like `control init` itself: where
  the AppArmor profiles are installed, run it through `aa-exec -p
  ariesd-admin` ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)). It reads the control
  database and `authorized_keys`, and writes no file.
- **Each line is read as `ariesd stdio` would run it.** The principal
  is the one sshd would sign the key in as: the last `--principal` of
  the forced command. A command that `stdio` would refuse, such as one
  with a stray argument, is skipped.
- **It does not remove.** Deleting a line from `authorized_keys` leaves
  the principal's row, its membership and the key in the control
  database. Where sshd reads ariesd's keys file
  ([Keys from the store](#keys-from-the-store)) the key therefore still
  signs in, until `aries admin key remove` takes it out; where sshd does
  not, deleting the line stops the key, as before.
- **Its changes are on the record** in the control database's event log,
  under the reserved principal `import`.
- **`serve` refuses to start** while the control database holds no active
  project, and says to run `control init`.
- **To go back,** remove `control_db` and restart. The project's database
  was never changed, and the control database is left unused.

### Keys from the store

On a server of many projects ariesd writes the keys its control
database holds to a file, and sshd reads that file before
`authorized_keys`. An admin adds and removes keys with `aries admin`, from
anywhere, and nobody edits a file on the server. The file is
`~aries/.ssh/aries_keys`, in the format of `authorized_keys`: a
first line that says ariesd generates it, then one line for each key
of each active principal, the line you would have written by hand with
the key's fingerprint added:

```text
restrict,command="/usr/local/bin/ariesd stdio --principal alice --key SHA256:…" ssh-ed25519 AAAA…
```

sshd asks ariesd nothing. It reads two files, ariesd's first, and
nothing of Aries runs before sshd has authenticated a key.
`authorized_keys`, which you maintain by hand, stays the way in for a
key the control database does not hold: the first admin's before
`control init`, and a server admin's own new key.

- **When it is written.** At start, before ariesd listens; after every
  `aries admin` change that alters which keys sign in (a key added or
  removed, a principal added with keys, disabled or enabled), before
  the admin is answered; and every 30 seconds if it no longer holds
  what the control database does, which is how an `ariesd control
  init` run beside the daemon reaches it, and how an edit to the file
  is undone. A key added with `aries admin` signs in at once, with no
  restart of anything. When nothing changed, nothing is written.
- **How it is written.** A new file beside it, mode 0600, synced, then
  renamed over it, so sshd never reads half a file and a crash leaves
  the old one or the new one. The lines are in a fixed order, so the
  same keys give the same bytes.
- **The file is a cache, and the control database decides.** Each line
  names its key's fingerprint, and ariesd refuses a connection whose
  key the database no longer holds for that principal, and one whose
  principal is disabled. So a key that was removed, in a file that
  could not be rewritten or was restored from a backup, gets past sshd
  and is refused by ariesd. sshd reads this file first, so that
  refusal stands even when the key is also a line of `authorized_keys`:
  an out-of-date file can refuse a key the other file would have let
  in, and never lets in one the database has dropped.
- **A server of one project writes no file.**

1. **Let ariesd write to the account's `.ssh`.** The unit of
   [step 5](#5-run-ariesd-under-systemd) has `ProtectSystem=strict`,
   which makes the file system read-only for ariesd, apart from
   `/run/aries` and its private `/tmp`. Its `ProtectHome=yes` does not matter here: it
   covers `/home`, `/root` and `/run/user`, and the account's home is
   `/var/lib/aries`. Add the one directory:

   ```sh
   mkdir -p /etc/systemd/system/ariesd.service.d
   printf '[Service]\nReadWritePaths=/var/lib/aries/.ssh\n' > /etc/systemd/system/ariesd.service.d/keys.conf
   systemctl daemon-reload
   ```

   The directory is the one [step 6](#6-let-the-first-principal-in)
   made: owned by `aries`, mode 0700. ariesd refuses to write into
   a directory that group or others may write to, as sshd refuses to
   read keys from one. It also refuses a path that is a symbolic link
   or anything but a regular file, and a file it did not generate, so
   pointing `keys_file` at `authorized_keys` by mistake cannot replace
   it.

2. **Tell sshd to read the file,** for the `aries` account only. Add
   one line to the `Match` block of
   `/etc/ssh/sshd_config.d/00-aries.conf`
   ([step 6](#6-let-the-first-principal-in)):

   ```text
   Match User aries
       AuthenticationMethods publickey
       DisableForwarding yes
       PermitTTY no
       AuthorizedKeysFile .ssh/aries_keys .ssh/authorized_keys
       AuthorizedKeysCommand none
       TrustedUserCAKeys none
       AuthorizedPrincipalsFile none
   ```

   ```sh
   sshd -t
   systemctl try-reload-or-restart ssh
   ```

   sshd takes the first line that matches the key, in the order the
   files are named, so the order matters: ariesd's file comes first.
   A key in both files is then let in by ariesd's line, which names
   its fingerprint and the principal the control database gives it, so
   `aries admin key remove` closes its connections and ariesd can
   refuse it. With the files the other way round, such a key would come
   in by its line of `authorized_keys`, which names no fingerprint, and
   removing it would neither close its connections nor show.

   With `AuthorizedKeysCommand none` in the `Match` block, sshd on
   stock Ubuntu 24.04 (OpenSSH 9.6, no `AuthorizedKeysCommand` of its
   own) logs `error: No user for AuthorizedKeysCommand specified,
   skipping` each time it is offered a key that is in neither file. It
   is logged as an error and is none: sshd is saying that it ran no
   command, which is what the line is for.

3. **Restart ariesd and check the file:**

   ```sh
   systemctl restart ariesd
   ls -l /var/lib/aries/.ssh/aries_keys
   journalctl -u ariesd -n 5
   ```

   The file is mode 0600 and owned by `aries`, and the log has
   `keys file written`. If ariesd cannot write it at start, it does
   not start, and the error's `fix:` says what to change.

4. **Add someone**, from any repository connected to the server, and
   have them run a command: `aries admin principal add bob --key
   bob.pub`, then `aries ready` as bob. `aries admin status` shows the file,
   how many keys it holds and when it was written.

5. **Move the keys.** `control init` has already copied every key of
   `authorized_keys` into the control database
   ([Opt in](#opt-in)), so each is now in both files. Remove the lines
   from `authorized_keys` that you want `aries admin` to govern, and keep
   the first admin's:

   - **A key whose line is still in `authorized_keys` is not kept out
     by `aries admin key remove`.** While the control database holds the
     key, sshd lets it in by ariesd's line, so the removal closes the
     connections it had signed in. But the removal also takes that line
     out of ariesd's file, and sshd then finds the key's line of
     `authorized_keys` and lets it in again, by a forced command that
     names no fingerprint: ariesd cannot refuse that key, or close a
     connection it makes from then on. The key is revoked when its line
     of `authorized_keys` is deleted too. Meanwhile its principal can
     be disabled, and its memberships removed, which ariesd enforces
     whatever let the key in.
   - **ariesd tells you which keys those are.** It runs as the
     account whose `authorized_keys` it is, so it reads the file: it
     never writes to it, and never prints or logs a line of it. `aries
     admin key list NAME` marks each key that is also in
     `authorized_keys`, and `aries admin key remove` of such a key warns
     that its connections were closed, that sshd will still accept it
     by that line, and that the line must be deleted on the server. A
     line ariesd cannot read as a key is skipped, and only the first
     mebibyte of the file is read.
   - **It also counts who comes in that way.** A connection whose
     forced command named no fingerprint came in by `authorized_keys`:
     its key is not in the control database, or was removed from it.
     `aries admin status` counts those for each project (BY HAND), and
     `aries admin key list NAME` for NAME. `aries admin key remove` does not
     close them.
   - **The control database's line wins for a key in both.** The key
     signs in as the principal the control database gives it, whatever
     principal its line of `authorized_keys` names, and a disabled
     principal's key has no line in ariesd's file, so it reaches its
     line of `authorized_keys` and is refused by ariesd as disabled.
     `authorized_keys` cannot override the control database; it only
     lets in a key the database does not hold.
   - **Keep one line for a server admin.** It is how a server admin's
     own key is replaced, since nobody adds a key to their own account
     with `aries admin`. A key in `authorized_keys` keeps the
     principal its line names: `aries admin key add` and `principal add
     --key` refuse it for anyone else, as they refuse a key another
     principal has, so nobody can take a server admin's key by adding
     it to the control database under another name. And a line you add
     for a key the control database already holds under another name
     wins: ariesd leaves that key out of its file, so that sshd
     reaches your line, says so in its log, and counts it in `aries admin
     status` as a key held back. It takes effect at the next key change
     or within 30 seconds; until then that key is refused. ariesd
     reads the first mebibyte of the file, and takes the first line for
     a key, as sshd does.

**Settings.** `keys_file` is where ariesd writes, and
`authorized_keys` the hand-maintained file it reads; a relative path is
under the home directory of the user ariesd runs as, taken from the
account database and not from `$HOME`. `bridge` is the path of ariesd
in each line's forced command, `/usr/local/bin/ariesd` by default: an
absolute path of letters, digits and `._+-`, since it is written into a
command sshd runs. If you move `keys_file`, change sshd's
`AuthorizedKeysFile` and the unit's `ReadWritePaths` with it
([Settings](#settings)).

**When ariesd is stopped,** sshd still reads both files and accepts
their keys, and every command then fails with `ariesd is not running
on the server` until it is started. Neither file is a way to work
without the daemon, so the order of the two changes nothing then.
ariesd writes its file before it listens, and does not start if it
cannot, so a file left out of date by a stopped daemon is replaced
before anyone is answered.

**When a write fails after the start,** the change an admin made is
already recorded, and stays. The admin's command succeeds and prints a
warning that sshd may still be reading the old file; ariesd logs
`the keys file is out of date` at `error` with the cause and a fix, and
tries again every 30 seconds; and `aries admin status` says `THE LAST
WRITE FAILED` on its `keys file` line until a write succeeds. Meanwhile
a key just added does not sign in, and a key just removed is refused by
ariesd rather than by sshd. That holds for a removed key that is also
a line of `authorized_keys`: sshd finds the old line of ariesd's file
first, so the key is refused until the file is written, and let in by
`authorized_keys` after. If the file cannot be written for long and
someone is shut out who should not be, fix what the log names, or move
the file aside on the server: sshd then reads `authorized_keys` alone,
and ariesd writes a new file when it can.

**On an image whose sshd already has a global `AuthorizedKeysCommand`**
for its provider's login service (Google Cloud's OS Login, AWS's EC2
Instance Connect, Azure's Entra ID login), nothing above needs to
change: `AuthorizedKeysFile` in the `Match` block is read whatever that
command is. Left alone, sshd would still run the provider's command for
the `aries` account, on every sign-in, and accept a key it printed,
with no forced command. `AuthorizedKeysCommand none` in the `Match`
block is what stops that, and `TrustedUserCAKeys none` does the same for
a certificate authority the machine trusts for every account
([step 6](#6-let-the-first-principal-in)): the account's keys come from
the two files and nowhere else.

What was checked, on 12 October 2026, with OpenSSH 9.6 on Ubuntu 24.04
in the [Docker test server](deploy/docker.md), which has no systemd:

| Checked | Result |
|---|---|
| ariesd installed as [step 3](#3-install-ariesd) installs it: owned by `aries` in `/opt/aries/bin`, linked from `/usr/local/bin`, and no copy that root owns | Keys from the control database sign in. sshd runs no program of Aries's before authentication, so who owns the binary does not matter to it |
| A global `AuthorizedKeysCommand`, outside any `Match` block, set to a root-owned script that prints nothing, with `AuthorizedKeysCommandUser nobody` | Keys from both files sign in, with no override. sshd ran the script twice for each sign-in by `aries`. With `AuthorizedKeysCommand none` in the `Match` block it did not run it |
| ariesd killed, sshd left running | sshd accepts a key from either file, and the client gets `ariesd is not running on the server` |
| A key in both files, with `authorized_keys` named first, as this guide had it until that day | sshd let it in by its line of `authorized_keys`, which names no fingerprint |
| A key in both files, with ariesd's file named first, as above | sshd lets it in by ariesd's line: ariesd counts no connection of that key as let in by `authorized_keys`. With the key removed from the control database, the same key signs in by `authorized_keys`, and its connection is counted so |

Checked under systemd 255, on 12 October 2026, on the Ubuntu 24.04
virtual machine of [What was run, and where](#what-was-run-and-where):

| Checked | Result |
|---|---|
| The unit of step 5 as written, on a server of many projects, without step 1's `ReadWritePaths` | ariesd does not start: `write the keys file …: read-only file system`, and its `fix:` names `ReadWritePaths=/var/lib/aries/.ssh` |
| The same with step 1's drop-in | ariesd starts and writes the file, mode 0600, owned by `aries`. `ProtectHome=yes` does not get in the way |
| Step 2 on stock Ubuntu's sshd, which listens through `ssh.socket` | `sshd -t` passes and `systemctl try-reload-or-restart ssh` applies it. A key the control database alone holds signs in with no restart of anything; removing it, or disabling its principal, locks it out |
| A global setting in a later drop-in file (`PasswordAuthentication no` in Ubuntu's `60-cloudimg-settings.conf`) | Still applies to every account: the `Match` block of `00-aries.conf` ends with its file |

Not checked:

- **A provider's own machine.** The stand-in above shows that a global
  command that prints nothing does not get in the way. It does not show
  what OS Login, EC2 Instance Connect or the Entra ID extension change
  besides that one setting, or what their commands print for an account
  named `aries`.
- **SELinux**, which is not supported yet
  ([What a server needs](#what-a-server-needs)). From general knowledge
  of it, unverified: sshd reads keys only from files labeled for it,
  and a home directory outside `/home`, such as `/var/lib/aries`,
  does not get that label on its `.ssh` by default. That is as true of
  `authorized_keys` as of the keys file.

### Managing access

Admins manage a server of many projects with `aries admin`, from any
repository connected to it ([commands](cli.md#agents-and-maintenance)).
A command acts on that repository's project unless `--project` names
another, by UUID or by name. Nothing here is an MCP tool: an agent
cannot run it, and one whose principal is refused is told to ask a
person.

| Command | Who may |
|---|---|
| `aries admin project add NAME`, `archive`, `activate` | Server admins |
| `aries admin project list` | Anyone the server lets in: a server admin sees every project and its database, anyone else the active projects they belong to |
| `aries admin principal add NAME` | Server admins; a project admin, into a project they administer |
| `aries admin principal add NAME --no-project`, `principal list` | Server admins |
| `aries admin key list --added-by NAME`, `principal list --created-by NAME`, `member list --added-by NAME` | Server admins, for all of it; a project admin, for the keys and principals of their projects' members and for their projects' memberships |
| `aries admin principal disable NAME`, `enable NAME` | Server admins; a project admin, for a principal within their reach |
| `aries admin member add NAME`, `remove NAME` | Server admins; the project's admins |
| `aries admin member list` | Server admins; the project's admins |
| `aries admin principal add NAME --key FILE`, `key add NAME FILE`, `key remove FINGERPRINT` | Server admins; a project admin, for a principal within their reach. Never for one's own account |
| `aries admin key list NAME` | Server admins; an admin of an active project NAME belongs to, with or without reach over NAME; NAME themselves |
| `aries admin status` | Server admins, for the server and every project; a project admin, for the active projects they administer ([Watching a server](#watching-a-server)) |

- **A server admin** is a principal named under `admins:`. They are let
  into every active project and may do everything above. Nothing over
  the protocol makes one.
- **Disabled wins.** A principal that is disabled is refused everywhere
  and has no admin rights, in the control database or in any project,
  even when `admins:` names it. An admin named there who has no row in
  the control database is not disabled. Only a server admin disables or
  enables a server admin, and the last server admin who is not disabled
  cannot be disabled: nobody could then enable anyone without a shell
  on the server. That rule counts every name under `admins:`, whether or
  not it has a key, since a key may be a line of `authorized_keys` that
  ariesd does not judge. So name only admins who can sign in: with
  `admins: [dana, spare]` and no key for `spare`, dana can be disabled
  and nobody is then left to sign in as a server admin.
- **Naming an admin trusts whoever set that name up.** A project admin
  may create a principal of any free name, with a key, and may add keys
  to the members of their own projects. If you later put such a name
  under `admins:`, those keys are a server admin's. ariesd warns at
  start, in its log, of each configured admin whose row, or one of whose
  keys, was put there by someone who is neither a server admin nor
  `control init`, and names the key. Check it with its owner, or remove
  it, before you rely on the name.
- **Disabling an account does not undo what it did.** The keys it added
  to others, the principals it created and the memberships it added all
  keep working. `principal disable` says how many of each there are and
  names the three commands that list them: `aries admin key list
  --added-by NAME`, `aries admin principal list --created-by NAME` and
  `aries admin member list --added-by NAME`. A server admin sees all of
  it, and a project admin what is within the projects they administer.
  Review the lists when an account may have been misused, and remove
  what should not be there; nothing is removed for you. A key planted
  on your own account is yours to remove, below.
- **Nobody adds a key to their own account**, a server admin included:
  an agent holding a person's key could otherwise add one of its own.
  Another admin with reach does it. A server admin's keys are added by
  another server admin, or by a line in `authorized_keys` on the
  server.
- **Anyone may remove a key of their own account.** Removing only takes
  access away, and it is how you clean your own account when someone
  with rights over it added a key that is not yours: `aries whoami` shows
  the key you are signed in with, `aries admin key list YOU` shows every
  key of yours with who added it and when, and `aries admin key remove
  FINGERPRINT` removes one. Two limits keep you from locking yourself
  out by accident. The key your connection signed in with is refused,
  by fingerprint: sign in with another of your keys to remove it, or
  ask an admin. And when your connection came in by a line of
  `authorized_keys`, which names no key, ariesd cannot tell which key
  you used: it removes the key and warns that it could not check. That
  connection's key is not one the control database can take away in any
  case. Removing the last key the control database holds for you is
  allowed, a server admin's included: you keep the way in you are
  using. Someone else's key still needs reach.
- **Keys.** `key add NAME FILE` takes a public key file, such as
  `~/.ssh/id_ed25519.pub`, or `-` for standard input. `aries` reads the
  file itself and refuses a private key before it sends anything. A key
  belongs to one principal; one that another principal has is refused,
  without saying whose. `key remove` takes the fingerprint `key list`
  prints, and closes the connections that key had signed in. It warns
  when the key is also a line of `authorized_keys` on the server, where
  sshd still accepts it
  ([Keys from the store](#keys-from-the-store), step 5).
- **Losing access releases claims.** When a membership is removed, the
  principal's claims in that project end; when a principal is disabled,
  its claims in every project; when a project is archived, every claim
  in it. Each ends with a `claim.release` event that names the admin and
  the reason, the session that held it is told in its inbox, and the
  issue goes back to open and ready. The command says how many it
  released. Taking the admin role away releases nothing. When a member
  is removed or a principal disabled, a project whose store is closed at
  that moment has nothing running to release them: its claims end when
  the store next opens, before its first request, recorded under
  `ariesd` with the same reason. An archive does not wait: it opens a
  closed store to end the claims, which are then recorded under
  `ariesd` and not counted in what the command says it released.
- **A project admin** is a member whose role in a project is `admin`
  (`aries admin member add NAME --role admin`).
- **Reach.** A project admin may disable or enable a principal only
  while every project that principal belongs to is one they administer.
  A principal in no project, and a server admin, are out of every
  project admin's reach. So when `member remove` leaves someone only in
  projects another admin runs, that admin gains reach over them, and the
  command says so.
- **Adding someone.** A project admin may add to their project a
  principal that already belongs to some project, or one they created
  themselves. A principal in no project that someone else created is
  placed by a server admin. Principal names are one namespace for the
  server, so `principal add` of a name in use says that it is taken.
- **A new member's keys are shown first.** A key signs in to every
  project its principal is in, so whoever adds a member also lets in
  every key that member has, whoever put it there. `member add NAME` of
  a principal with any key therefore adds nothing at first: it lists
  each key with its fingerprint, type and comment, who added it and
  when, and whether that was an admin of this project (`ADMIN HERE`).
  On a terminal `aries` then asks, and adds the member on `y`. Without a
  terminal, or with `--json`, it exits 1 and names `--accept-keys`,
  which adds without asking: a script never adds by accident, and can
  add on purpose. This is the same for a server admin. A principal with
  no key is added at once, and a change of role asks nothing. Check a
  key marked `no` with its owner before you accept: an admin of another
  project may add keys to someone who belongs to that project alone, and
  such a key would otherwise follow the person into yours unseen.
- **An admin sees the keys of their project's members.** `key list NAME`
  works for an admin of any active project NAME belongs to, for the same
  reason, even where they may not add or remove NAME's keys.
- **A new person** takes one command once sshd reads ariesd's keys file
  ([Keys from the store](#keys-from-the-store)): `aries admin principal add
  NAME --key FILE`, with the `.pub` file they sent you. `--key` may be
  given more than once. Someone the server already knows joins another
  project with `member add NAME`.
- **A new project.** `aries admin project add NAME` records the project,
  creates its database under the next free name of `project_dbs`, applies
  the schema and marks it active, then prints the `.aries.yaml` for its
  repository. If a step fails, run the same command again: it finishes
  the same project. If the name was never granted, the refusal gives the
  `GRANT` statement to run as Dolt's root, and the command then resumes.
  The project is served at once to server admins; add its members with
  `aries admin member add NAME --project NAME`.
- **Archiving** keeps the project's database, members and name, and
  serves it to no one: its connections are closed, its claims end, and
  its store closes with them, after one more sweep for a claim that a
  request in flight took meanwhile. An ariesd that stopped before it
  had released them ends them when it starts, before it listens. The
  server's last active project cannot be archived, since
  `aries admin` needs a project to connect to. `activate` opens the
  database, which applies any migration it missed while archived, and
  serves it again.
- **Everything is on the record** in the control database's event log,
  under the principal who ran the command.

One case in the above is slower than "at once". A project whose store is
closed has nothing running in it: when its member is removed or disabled,
the claims stay in its database until the store next opens. Nobody can
use them meanwhile, and they end before the first request is answered.

An archived project's store never opens for a connection, so ariesd
does not leave its claims to that. The archive opens a closed store to
end them, and at every start, before it listens, ariesd asks each
archived project's database whether it holds a claim, and opens the ones
that do, ends every claim, recorded under `ariesd` for `project
archived`, and closes them. A project with nothing to end costs that one
read, a fraction of a millisecond, and nothing is written. It does not
migrate: a database that an older ariesd last migrated is left as it
is, with a line in the log at `info`, and a claim in it ends when the
project is activated, as before. One that cannot be opened is logged at
`error` and does not stop the server or the other projects. Nothing
runs for archived projects on the 30-second tick: a running ariesd
leaves a claim in one only if the release itself failed, which it logs,
and the next start or the activation ends it.

### Watching a server

`aries admin status` reports what ariesd knows of itself and of its
projects now. A server admin sees the server and every project; a project
admin sees the active projects they administer and nothing of the
server; anyone else is refused. It reads memory, plus one count on an
index for each open project listed, opens no store and writes nothing.
It does not wait for a database that does not answer
([When Dolt does not answer](#when-dolt-does-not-answer)).

```text
ariesd v0.5.0, up 3d 4h
stores 2 of 64, connections 14 of 1024
keys file /var/lib/aries/.ssh/aries_keys: 31 keys, written 2026-10-11 09:12 UTC
ariesd's memory 42 MiB in use, 78 MiB from the system; 112 goroutines
Dolt 2.4.2, up 3d 4h, 9 connections, 1234 MiB resident, memory limit 1500 MiB
Dolt's collector: 37 cycles for 2051 writes in 10m, 1.8 for each 100 writes: under 3, as with a memory limit that had room in the measurements (over 14 was one too low)
projects: 2 active, 1 archived; principals: 12 active, 1 disabled

PROJECT  STATE     STORE   CONNS  BY HAND  SESSIONS  WRITES/MIN  P50    P99    LAST USED
acme     active    open    9      2        14        212         4.2ms  31ms   2026-10-11 09:30 UTC
web      active    open    5      0        6         40          3.9ms  18ms   2026-10-11 09:29 UTC
old      archived  closed  0      -        -         -           -      -      -
```

| Figure | What it is |
|---|---|
| stores, connections | How many are open, beside the limit that caps each (`open_stores`, `conns`) |
| keys file | The file ariesd writes for sshd ([Keys from the store](#keys-from-the-store)): its path, how many keys ariesd last put in it, and when it was last written. `THE LAST WRITE FAILED` means sshd may be reading an older list than the control database holds; ariesd's log says why |
| `clock:` | Shown once the clock has been set while ariesd ran: how many times, in which direction, and the last step's size and time. Each is a `clock stepped` line in the log ([The server's clock](#the-servers-clock)) |
| `CLOCK BEHIND HISTORY` | The clock is behind the latest event of the database named, by how much and since when. ariesd's time runs on from the history at half speed until the clock has caught up. With `clock_behind: stop` it adds that ariesd will not start again until the clock is corrected: check `timedatectl` on the server before any restart |
| ariesd's memory | The Go heap in use and all the memory ariesd has from the operating system (`HeapAlloc` and `Sys`). Not Dolt's |
| `Dolt`, `Dolt's collector:` | With `telemetry: dolt_metrics_url:` set: what ariesd last read from Dolt's own metrics, and how often Dolt's garbage collector ran against ariesd's writes ([Dolt's figures](#dolts-figures)). `Dolt: unknown` means the last read failed. Without the setting neither line is shown |
| projects, principals | The control database's rows, by state |
| STORE | Whether the project's store is open. The figures after it exist only while it is, and start from zero each time it opens |
| CONNS, BY HAND | Connections past their handshake, and those among them that named no key of the store, which sshd let in by the hand-maintained `authorized_keys` |
| SESSIONS | Sessions seen in the last five minutes, as `aries who` lists them. `unknown` when the database did not give the count ([below](#when-dolt-does-not-answer)) |
| WRITES/MIN | Transactions that changed something and committed in the last 60 seconds. A refused request is not one; a lease renewal is |
| P50, P99 | The median and 99th percentile of the latest 256 writes' latency, however old they are: from the request reaching the project's store to its commit, the wait for the one writer included |
| LAST USED | When the project last answered a request, or was opened |

`-n` and `--after NAME` page the projects as the admin lists do, 200 at a
time. `--json` gives the same figures, with latency in microseconds.

On the server itself, [`ariesd status`](#ariesd-status) prints the
same report with no admin key, and the same measurements are exported as
metrics ([Observability](#observability)).

#### When Dolt does not answer

The report is there to be read when the database is in trouble, so it
does not wait for the database. Its one read, the count of each open
project's sessions, is made for every project at once and given 2
seconds in all, the bound `/readyz` gives its own check of Dolt: the
count is on an index and takes milliseconds when Dolt answers at all.
What did not come back is `unknown`, never 0, and a line under the
projects says why:

```text
PROJECT  STATE   STORE  CONNS  BY HAND  SESSIONS  WRITES/MIN  P50    P99   LAST USED
acme     active  open   9      2        unknown   0           4.2ms  31ms  2026-10-11 09:30 UTC
web      active  open   5      0        unknown   0           3.9ms  18ms  2026-10-11 09:29 UTC
sessions: unknown for 2 projects (database did not answer in 2s); the rest is what ariesd holds in memory. Check Dolt on the server (docs/server.md, Troubleshooting)
```

- **Everything else is still there**, since ariesd holds it in memory:
  the server's lines, the stores and connections, the clock, the
  writes and their latency as they last were, and
  [Dolt's line](#dolts-figures), which says `unknown` by itself once a
  read of Dolt's metrics has failed.
- **`--json`** leaves `sessions` out for such a project and gives
  `sessions_unknown`, the cause as text: `database did not answer in 2s`,
  or `the read failed; see ariesd's log` for any other failure, which
  the log has as `count the sessions seen, for status`. A project whose
  store is closed has neither.
- **One project's database that does not answer** costs that project's
  figure and no other's.
- **`aries admin status` answers in about 2 seconds too, from a repository
  whose project's store is open.** `aries` connects anew for each command,
  and a new connection is welcomed without waiting on Dolt: its session
  is noted for `aries who` beside the wait for its first request. The
  report counts its asker's session, so it waits for that note, but
  within the same 2 seconds as its own reads: when the note does not
  come, the sessions are `unknown` and the rest is there.
- **From a repository whose project's store is closed, it is refused
  in about 2 seconds:** `the server's database does not answer, so
  project "…" was not opened`, with `ariesd status` on the server as
  what to do. A connection is to a project, opening the project's store
  waits on Dolt for up to a minute, and the report itself opens no
  store; so before a store is opened for a connection, Dolt is given 2
  seconds to answer a ping. Run `aries admin status` from the repository
  of a project in use, or `ariesd status` on the server.
- **Every other command waits for Dolt**, for the request timeout of a
  minute and no longer, and then ends with `the server timed out on …`.
  It waits for its session to be noted first, within that same minute,
  so nothing reads or writes a project for a session `aries who` cannot
  list. A session whose note failed is noted by its next request that
  Dolt answers.
- **`ariesd status` on the server** answers in about 2 seconds
  whichever stores are open, and needs no admin key.

#### Dolt's figures

Dolt is another process under another user, so what the report says of
it is what Dolt says of itself: ariesd reads Dolt's own metrics
endpoint ([step 2](#2-run-dolt-under-its-own-account)) every 15 seconds
and shows the last answer. A server admin sees the two lines; a project
admin does not, since they are the server's and not a project's.

| Figure | From Dolt's | Why it is there |
|---|---|---|
| Version | `dss_dolt_version` | Which Dolt is running, which after an upgrade of Dolt is not always the one on the disk |
| `up` | `process_start_time_seconds` | When Dolt last restarted. Its memory grows from then ([Dolt's memory](#dolts-memory)), and so does what a restart would give back |
| connections | `dss_concurrent_connections` | Clients connected to Dolt now: ariesd's own, a few for each open store, and anyone else's, such as a `dolt sql` session or a dump |
| `MiB resident` | `process_resident_memory_bytes` | The memory of Dolt's process, which is what the machine must hold. It is the figure `ps` gives, not a container's or a unit's total, which adds the page cache of Dolt's files |
| `memory limit` | `go_gc_gomemlimit_bytes` | The Go memory limit Dolt runs under (`GOMEMLIMIT`), or `no memory limit` |
| `Dolt's collector:` | `go_gc_duration_seconds_count`, and ariesd's own count of writes | The cycles of Dolt's garbage collector, the writes ariesd's project stores committed, and the first for each 100 of the second, over the last ten minutes or since either started. It is what tells whether a memory limit has room ([Dolt's memory](#dolts-memory)) |

- **`Dolt: unknown`** means the latest read failed: nothing answers
  there, the
  port or the path is wrong, the answer took more than 5 seconds, was
  more than 1 MiB or was not Dolt's metrics, or it was a redirect, which
  ariesd does not follow. The line gives the last read that worked,
  and ariesd's log has `could not read Dolt's metrics` with the cause:
  once when it starts, then every 10 minutes while it lasts, and
  `reading Dolt's metrics again` when it ends. Nothing else changes:
  ariesd serves as before, and `/readyz` still answers from its own
  query of Dolt, not from this.
- **A figure Dolt did not give is `unknown` by itself**, as a newer Dolt
  that renamed a metric would make it.
- **The collector line needs two reads** of the same Dolt process, so it
  is missing for the first 15 seconds after ariesd or Dolt starts, and
  its window is shorter than ten minutes until both have run that long.
- **The ratio is shown from 500 writes in the window**, and judged
  against the measurements only when a memory limit is set. An idle Dolt
  still collects about every two minutes, so against a few writes the
  ratio would be large and say nothing. With fewer writes the line says
  `too few writes to judge the memory limit by`, unless the cycles are
  past 14 for each 100 even when counted against 500 writes, more than
  70 in the window: a limit far too low has Dolt collecting without
  pause whether or not anything is written, and the line then says so.
- **A Dolt that hangs whole** stops more than this, though not the
  report: each open project's sessions are then `unknown`
  ([When Dolt does not answer](#when-dolt-does-not-answer)), and
  `/readyz` says `the last check of Dolt failed` within a few seconds.
- **The writes are those of the project stores**, as `WRITES/MIN` counts
  them, summed at each read. A store that closes between two reads takes
  with it the writes it made since the earlier one, and writes to the
  control database are not counted.
- **`--json`** has the same under `server.dolt`: `read_at`, `version`,
  `started_at`, `uptime_seconds`, `resident_bytes`, `conns`,
  `mem_limit_bytes` (0 for no limit) and `gc` with `window_seconds`,
  `cycles`, `writes` and `cycles_per_100_writes`; or `unknown: true`
  with `read_at`, when there was a read that worked.
- **Query latency is not shown.** Dolt's histogram of it
  (`dss_query_duration`) has buckets at 10 ms, 100 ms, 1 s and upward,
  which is too coarse to say more than the report's own P50 and P99 of
  each project's writes do.
- **ariesd does not export Dolt's metrics again.** Have your collector
  scrape `http://127.0.0.1:11228/metrics` beside ariesd's
  ([Observability](#observability)).

**On a server set up before this**, turn Dolt's endpoint on and tell
ariesd where it is. Restarting Dolt restarts ariesd with it and
drops every connection for a few seconds, so pick a quiet moment:

```sh
printf 'metrics:\n  host: 127.0.0.1\n  port: 11228\n  labels: {}\n' >> /etc/dolt/config.yaml
systemctl restart dolt
curl -s --retry 5 --retry-connrefused http://127.0.0.1:11228/metrics | grep -c '^dss_'
printf 'telemetry:\n  dolt_metrics_url: http://127.0.0.1:11228/metrics\n' >> /etc/aries/ariesd.yaml
systemctl restart ariesd
sleep 20
sudo -u aries -H ariesd status | grep '^Dolt'
```

- If `/etc/aries/ariesd.yaml` already has a `telemetry:` block, add
  the `dolt_metrics_url:` line under it with an editor instead of the
  second `printf`: a key given twice stops ariesd at start.
- The `curl` line prints 14 at Dolt 2.4.2, and the last line prints the
  two lines above, the second only once ariesd has read twice.
- A URL that is not `http://` on a loopback address stops ariesd at
  start with a `fix:`. The endpoint has no authentication, and ariesd
  will not read it from another machine.

A database's size on disk is not in the report: Dolt offers its
least-privileged account no table or function
for it, and the files belong to Dolt's user. Read it with `du -sh
/var/lib/dolt/data/aries/*` as root or `dolt`
([Storage growth](#storage-growth)).

Each project's store is opened by its first connection and closed after
`store_idle` without one ([Limits](#limits)). Closing gives back the
store's connections to Dolt. It does not give back Dolt's memory
([Dolt's memory](#dolts-memory)), so size the machine by what all its
projects write, not by how many are open. A store that reopens ends the claims
that expired while it was closed, and those of anyone who lost their
access meanwhile, before it serves its first request.

## Prices

`aries cost`, `show` and `digest` price tokens at each model's list rates,
which an admin records with `aries admin prices set`. Rates are US dollars
per million tokens, from an effective date (midnight UTC) or time; the
model is spelled exactly as the harness reports it, which `aries cost --by
model` shows. For example, with invented rates for an invented model:

```sh
aries admin prices set example-large --from 2026-01-01 \
  --input 4 --output 20 --cache-write 5 --cache-write-1h 8 --cache-read 0.4
aries admin prices
```

`--cache-write` is a five-minute cache write and `--cache-write-1h` a
one-hour one. A record is priced by the newest price for its model whose
effective time is no later than the record's, so a new price applies from
its date on and never rewrites past costs; costs are computed when read,
never stored. A model with no price in effect is reported as unpriced,
never guessed; until the first price is set, `show` and `digest` show no
cost at all. Setting the same model and date again replaces the rates;
setting them to what they are changes nothing. Each change is a
`price.set` event. Anyone may list prices.

## Plans

A flat-rate plan (a subscription paid per seat a month) costs the same
whatever its tokens would cost at list price. `aries cost` shows each
group's share of the plans' fees as its **amortized cost**, beside the
list-price equivalent, once an admin records the plans with `aries admin
plans set`. For example, with an invented plan:

```sh
aries admin plans set team --from 2026-09 --fee 25 --seats 3 \
  --principal alice --principal bob
aries admin plans
```

The fee is US dollars a seat a month, with at most six decimals; the
month's total is fee times seats. The principals are those whose usage
the plan pays for; a principal on no plan adds no amortized cost. Terms
hold from their month until a later `--from` of the same name, so a
price rise or a new seat is a new row from its month on, and past months
keep the terms they had; `--fee 0 --seats 0` ends a plan. Each month's
total is split across the work by the tokens the principals reported
that month ([Cost](concepts.md#cost)), when read, never stored. Setting
the same name and month again replaces the terms; setting them to what
they are changes nothing. Each change is a `plan.set` event. Anyone may
list plans.

## The server's clock

ariesd relies on the server's clock and does not trust it to be right
([design §14, decision 37](design/aries.md#14-decisions)). A lease
runs out by it, every event is dated by it, and windows such as the
daily cap on usage records are counted back from it. A client's own
clock decides nothing here, with one bounded exception: the time a
usage record claims, which may be at most `clock_client_tolerance` (5
minutes) past the server's.

What ariesd does about a clock that is wrong or moves:

| The clock | ariesd |
|---|---|
| Is set back while ariesd runs | Its time does not go back. It runs on from where it was at half speed until the clock has caught up, which takes twice as long as the clock was set back: an hour takes two. Events stay in order and leases still run out, at half speed meanwhile, so a lease lasts longer by at most how far the clock was set back. It logs `clock stepped` and `clock behind history`, and keeps serving |
| Jumps forward while ariesd runs (it was set, or the machine was suspended and resumed) | Every lease that was live is extended by the jump, so no claim ends because the clock moved; each database records one `clock.step` event. It logs `clock stepped` and, for each database, `clock step settled` |
| Is behind the server's own history when ariesd starts | By default ariesd **refuses to start** ([below](#when-the-clock-is-behind-the-servers-history)) |
| Was ahead and has been corrected | That is the same thing, seen later: the events written while it ran ahead are now in its future, so the clock is behind its history |

A database follows a jump forward only once it has settled it: with
its next write, which is the reaper's next tick at the latest, or, for a
project whose store was closed for idleness, when the project is next
used. Until then its time runs on as if the clock had not moved. A lease
that runs out meanwhile has had all of its time and ends as it would
have without the jump; every lease still live is extended. The
`clock.step` event's time is the database's time when it settled, its
`claims` the claims' leases it extended, and its `locks` the holds of
locks and places in their queues, which are leases too.

**No write that begins after a jump is dated before it,** in any
database of the server: the control database's and every project's. A
write looks for a jump when it arrives and again when its turn at its
database's one writer comes, and settles the jump before it goes on, so
a write that was waiting its turn when the clock jumped is dated after
the jump like any other. What an operator or an auditor can rely on:

- In each log, the `clock.step` event stands between the events dated
  before the jump and those dated after it.
- An event that one database records because of an event in another
  (a claim released in a project for a membership removed in the control
  database) is never dated before it, jump or no jump.
- The one event a log may date before a jump and commit after it is
  that of a write whose transaction had already begun when the clock
  jumped: at most one in each database, since each has one writer, and
  late by no more than the time that one write took. It comes before
  its log's `clock.step`.

A database that has written nothing since a jump has no `clock.step`
for it yet, and its next event is dated after the jump. If the clock
jumps again while a jump is being settled, the write is tried again as
one that lost to a concurrent write is; a clock that jumps at every
attempt has the write refused with `conflict` after five attempts, and
nothing is dated before a jump.

A database ariesd opens for the first time since it started (a
project whose database could not be opened at start, or one added or
restored since) follows every jump made since the start in the same
way, with one `clock.step` event for all of them: whatever claims it
holds were made before ariesd started, so each of those jumps took
time from their leases. A database with no history has nothing to
settle and records nothing. If something other than this ariesd wrote
to the database after a jump, as a restore or `ariesd import-bd` may
have, its leases are extended all the same, by no more than the time
since its latest event: a lease may then last that much too long, and
none ends early.

A step is a difference of more than `clock_step_tolerance` (5 seconds)
between how far the wall clock and the monotonic clock moved, between
two readings or added up over several, so that many small steps made
quickly count as one. A second of difference an hour is forgiven, so a
leap second and a time service's small corrections never add up to a
step. A healthy time service slews a running clock and does not
step it, so seconds of difference is never drift. On a loaded
16-core development machine (macOS on arm64, load average 14, 8,500
garbage collections in the minute; not measured on Linux) the two
differed by 10 microseconds at most over 5,000 readings, so the
tolerance is not there for noise: it is there so that a
leap second and a time service's sub-second corrections go unremarked.

**Across a restart there is nothing to compare the clock with but the
history,** so a lease is then measured by the wall clock: a daemon that
comes back after a lease ran out ends the claim, as it always has. The
line is the start of the process: a jump made while ariesd was down is
an outage of that length, and one made at any time after it started is
a step, for every database. The same holds for a virtual machine paused in a way that leaves its
monotonic clock running: ariesd sees time pass, not a step.

### Keep the clock synchronized

- **Run a time service, and check it.** Ubuntu 24.04 ships
  `systemd-timesyncd`, enabled, synchronizing to `ntp.ubuntu.com`.
  Ubuntu 26.04 ships `chrony` in its place, enabled, with NTS to
  Ubuntu's own pools. On either, `timedatectl` must say `System clock
  synchronized: yes`. `timedatectl timesync-status` names the source
  and the offset on 24.04; on 26.04 it needs `systemd-timesyncd`, and
  `chronyc tracking` says the same (`chronyc -n authdata` shows the
  sources answering over NTS).
- **Use several sources, authenticated where they offer it.** Ordinary
  NTP is not authenticated: whoever can answer for the time source can
  set the server's clock. Network Time Security (NTS) authenticates the
  source. `systemd-timesyncd` asks one server at a time and does not
  speak NTS (general knowledge, not checked here). `chrony`, version 4.5
  in Ubuntu 24.04's main archive and built with NTS, combines several
  sources and can use NTS ones; its default configuration there lists
  pools without NTS, and installing it removes `systemd-timesyncd`. On
  26.04 that is the default: four of Ubuntu's NTS pools
  (`/etc/chrony/sources.d/ubuntu-ntp-pools.sources`), seen answering
  over NTS.
  Authentication matters when the network between the server and its
  time source is not yours. On a cloud virtual machine use the
  provider's own time service, which is reached without crossing the
  internet (general practice; no provider's was run for this guide).
- **Do not mix sources that treat leap seconds differently.** Some
  services smear a leap second over hours and others insert it; a
  server that follows both sees them disagree by up to a second and
  may step between them.
- **Alert on the clock being unsynchronized, not only on it being
  wrong.** `timedatectl show -p NTPSynchronized --value` prints `yes`
  or `no`. The commonest causes of a jump are a virtual machine's
  snapshot being restored, or the machine being paused and resumed,
  far more often than an attack.
- **Keep leases much longer than the uncertainty.** An agent's lease is
  15 minutes and `aries mcp` renews it every minute; a person's own
  commands take 8 hours. Steps that stay under `clock_step_tolerance`
  are not compensated, so they can cost a lease at most 5 seconds, and
  a second an hour after that, against a minute between renewals and a
  lease that is rewritten when half of it is left: two orders of
  magnitude of room. Raising the step tolerance
  toward the renewal interval gives that room away.

### Start ariesd after the clock is set

The unit in [step 5](#5-run-ariesd-under-systemd) has
`Wants=time-sync.target` and `After=time-sync.target`. What that waits
for depends on the machine. Each of these was run on the virtual
machine of [What was run, and where](#what-was-run-and-where), with a
cold boot each time:

| Machine | `time-sync.target` reached | ariesd serving | Boot |
|---|---|---|---|
| Stock Ubuntu 24.04 | 1.7 s after the kernel started, 37 microseconds after `time-set.target`, before the network was up or any time server heard from | 9.7 s (its first start loses the race with Dolt, as before) | 4.6 s |
| `systemd-time-wait-sync` enabled, a time server reachable | 33.4 s, as `systemd-timesyncd` logged `Initial clock synchronization` | 33.5 s | 33.7 s |
| The same, no time server answering | 91.5 s, when the wait unit's 90 second timeout failed it (`Failed with result 'timeout'`) | 91.6 s | 1 min 32 s |
| Stock Ubuntu 26.04 (the 26.04 machine of [What was run, and where](#what-was-run-and-where)) | 1.5 s, with `time-set.target`, before `chrony` had started | 3.6 s, with `chrony` not yet synchronized | 3.9 s |
| 26.04 with `chrony-wait.service` enabled, the time servers reachable | 12.3 s, as `chrony-wait` finished, 3.8 s after `chronyd` logged `Selected source` | 12.4 s | 12.7 s |

- **On stock Ubuntu, 24.04 or 26.04, the two lines wait for nothing.**
  `time-sync.target` is reached with `time-set.target`, early in the
  boot and before any time server is heard from. The unit that waits for a synchronized
  clock ships disabled: `systemd-time-wait-sync.service` on 24.04,
  `chrony-wait.service` on 26.04. The lines are in the unit so that it
  waits once something on the machine provides the target.
- **Waiting for synchronization is optional, and needs a timeout.** On
  26.04, `chrony-wait.service` has one, 180 seconds, so enabling it is
  enough: `systemctl enable chrony-wait.service`. 26.04 has no
  `systemd-time-wait-sync.service`, which comes with
  `systemd-timesyncd`. On 24.04:

  ```sh
  mkdir -p /etc/systemd/system/systemd-time-wait-sync.service.d
  printf '[Service]\nTimeoutStartSec=90\n' > /etc/systemd/system/systemd-time-wait-sync.service.d/timeout.conf
  systemctl daemon-reload
  systemctl enable systemd-time-wait-sync.service
  ```

  **Never enable it without the drop-in.** As shipped it has
  `TimeoutStartSec=infinity`. With no time server answering, ariesd's
  start was then still `waiting` after 130 seconds, and would have
  stayed so; so would everything else ordered after the target, which
  on Ubuntu is every timer and `cloud-final.service`. With the drop-in
  the wait fails after 90 seconds, the target is reached all the same,
  and ariesd starts with the clock as it is.
- **What it costs, and when it is worth it.** With a time server
  reachable it added 24 seconds to this machine's boot, since
  `systemd-timesyncd`'s first try comes before the network is up and
  its next is 30 seconds later; with none it adds the 90 seconds. It
  delays the timers too. It is worth that on a machine whose clock at
  boot cannot be trusted: one with no battery-backed real-time clock,
  or one that has come up with a wrong date before. On a virtual
  machine whose hypervisor supplies the time at boot, as this one's
  does (the kernel logged `rtc-efi: setting system clock` 0.05 s into
  every boot, and it was right each time), it buys little. On 26.04,
  with `chrony-wait.service`, it added 9 seconds with the time servers
  reachable; with none answering it would add up to its 180 (not run).

Why the order matters at all: a daemon that starts with the clock
**ahead** writes events in the future, and once the clock is corrected
it is behind its own history, which the next start refuses. A clock that
is behind at start is caught by the check below, whatever the unit
waited for.

What each boot does, with the defaults:

| Boot | Result |
|---|---|
| Normal | The clock is ahead of the last event written before the shutdown. ariesd starts. Run: three cold boots and one after the hypervisor's process died, which is a power loss to the guest |
| The real-time clock is a few seconds slow | The clock is behind the history by less than the time the reboot took, or by seconds: within `clock_behind_tolerance`. ariesd starts; if the time service then steps the clock forward by more than 5 seconds, leases are extended by the step. Not run: this machine's clock at boot was always right |
| The machine has no working real-time clock (a dead battery) | `systemd-timesyncd` advances the clock to the time it last saved, then synchronizes if it can. Imitated, not booted: with the clock set back a day and `systemd-timesyncd` started, it logged `System clock time unset or jumped backwards, restored from recorded timestamp`, the clock read the saved time, and ariesd started. It saves that time every 60 to 70 seconds, synchronized or not (measured, with the poll interval at 4 minutes), and when it stops, so after a power loss the clock comes up at most about 70 seconds behind the history. That is why the tolerance is 2 minutes and not 1 |
| A virtual machine restored from an older snapshot | The disk's history is as old as the snapshot and the clock is the present: not behind. ariesd starts. Not run |
| A database restored from a backup written by a machine whose clock ran ahead | The clock is behind the history. ariesd refuses to start and says how to start once anyway. Run, as the clock set forward under a running daemon and then put back |
| The clock is hours or years behind (a reset with no saved time, a wrong date set by hand) | ariesd refuses to start. Run with the clock set back one and two hours |

### When the clock is behind the server's history

At start, ariesd compares the clock with the latest event of every
database it will write to: the control database's and each active
project's, which it opens once at start anyway, so the check costs no
extra query. A database with no events has nothing to be behind. If the
clock is behind any by more than `clock_behind_tolerance`:

- **`clock_behind: stop`**, the default: ariesd does not start. It
  prints the refusal and exits with status 75, a seventh of a second
  after systemd started it, long before `TimeoutStartSec`:

  ```text
  # systemctl status ariesd
  × ariesd.service - aries server
       Active: failed (Result: exit-code) since Mon 2026-10-12 17:26:10 UTC; 3ms ago
      Process: 5116 ExecStart=/opt/aries/bin/ariesd serve (code=exited, status=75)
     Main PID: 5116 (code=exited, status=75)

  ariesd[5116]: ariesd: not starting: the server's clock is behind its own history (clock_behind: stop)
  ariesd[5116]: ariesd:   the clock reads   2026-10-12T17:26:10Z
  ariesd[5116]: ariesd:   the latest event  2026-10-12T17:35:32Z, 9m21s later
  ariesd[5116]: ariesd:   is in project 5d6b7c84-0ce6-4b09-abcf-e0a10c8bcc78 (database aries), and 1 more database is ahead of the clock too
  ariesd[5116]: ariesd: fix: if the clock is wrong, correct it (`timedatectl` must say synchronized: yes), then `systemctl start ariesd`
  ariesd[5116]: ariesd: fix: if the clock is right and the history is ahead, start once with: ariesd serve --accept-clock-behind 2026-10-12T17:35:34Z
  ariesd[5116]: ariesd: fix: or set clock_behind: warn in the config file, to start with a warning (docs/server.md, The server's clock)
  systemd[1]: ariesd.service: Main process exited, code=exited, status=75/TEMPFAIL
  systemd[1]: ariesd.service: Failed with result 'exit-code'.
  systemd[1]: Failed to start ariesd.service - aries server.
  ```

  The unit's `RestartPreventExitStatus=75` keeps systemd from starting
  it again, since a restart cannot correct a clock: 20 seconds later the
  journal held that one start and the unit was still `failed`, with
  `NRestarts=0`. Without the line the same refusal was retried every
  five seconds, 7 starts in 32 seconds, each adding its lines to the
  journal. systemd names status 75 `TEMPFAIL`, which is what it is:
  nothing is wrong with ariesd or its data. Every other failure to
  start still exits 1 and is retried, as before. `aries` answers
  `ariesd is not running on the server` meanwhile.
- **Correct the clock** and `systemctl start ariesd`.
- **Or, when the history is what is wrong, start once anyway,** with
  the flag the refusal printed, in a drop-in that you then remove:

  ```sh
  mkdir -p /etc/systemd/system/ariesd.service.d
  printf '[Service]\nExecStart=\nExecStart=/opt/aries/bin/ariesd serve --accept-clock-behind 2026-10-12T17:35:34Z\n' > /etc/systemd/system/ariesd.service.d/accept-clock.conf
  systemctl daemon-reload
  systemctl start ariesd
  rm /etc/systemd/system/ariesd.service.d/accept-clock.conf
  systemctl daemon-reload
  ```

  The daemon keeps the unit's confinement, its
  [AppArmor profile](#confine-ariesd-with-apparmor) included, since
  only its command line changes. It logs `clock behind history` and
  serves, with its time running on from the history. The flag answers
  that history and no later one: left in place it did nothing at a
  start with the clock right, and when the clock was set back again
  past newer events the start was refused with the flag still there.
  It also answers for that start only. A restart before the clock has
  caught up is refused again, and prints a later time, because the
  history has moved on.
- **`clock_behind: warn`** starts whatever the clock says, and logs
  `clock behind history` at `error` at start and every ten minutes
  until the clock has caught up, when it logs `clock caught up with
  history`.

Either way a server that starts behind is safe for its history:
ariesd's time runs on from the latest event, not from the clock, so no
event is dated before its predecessor and no window reopens. What it
costs is that timestamps are ahead of the clock until the clock catches
up, and leases run at half speed meanwhile.

**A clock set back under a running daemon never stops it,** under
either setting: a daemon that exits when its clock moves would give
whoever can set the clock, or answer for its time source, a way to take
the server down. It is reported instead, and with `clock_behind: stop`
the report says `THIS SERVER WILL NOT START AGAIN until the clock is
corrected`. `aries admin status`, `ariesd status` and
`ariesd upgrade --check` show the same, so look there before a
planned restart. Run on the same machine with the clock set back seven
minutes: an event written two seconds after another was dated one
second after it, `aries_clock_behind_seconds` fell from 390 to 0 by
30 a minute, the `error` line came again at ten minutes, `clock caught
up with history` at fourteen, and through sixteen minutes, eight of the
watchdog's intervals, the unit stayed `active` with no restart.

**`/readyz` stays ready through it,** for the same reason: the daemon
is serving correctly, and whatever watches readiness may take an
unready daemon out of service, which is the outage a moved clock must
not buy. A monitor sees it in the metrics instead
([Observability](#observability)): alert when
`aries_clock_behind_seconds` is above zero for longer than a boot
takes, and on any increase of `aries_clock_steps_total`. The gauge
is the lag to the second, under the tolerance too; the log and the
status start at the tolerance and go on until the lag is zero.

**A jump forward under a running daemon,** run with four claims held
in two projects, a minute into leases of 4, 5 and 6 minutes and 8 hours,
and the clock set forward ten minutes: the daemon logged `clock step
settled … claims=3` and `claims=1`, every `expires_at` moved on by the
jump, each database's log gained a `clock.step` event, and no claim
ended. The three short leases were then reaped 270, 330 and 390 seconds
after they were taken, at the reaper's first tick past 4, 5 and 6
minutes: a holder who goes silent loses the claim after the time it
was given, no sooner and no later. A restart with the clock still ahead
started as usual.

**Nothing on a virtual machine need put a clock back for you.** With
`timedatectl set-ntp false` the clock of this guest stayed where it was
set for as long as it was watched, twenty minutes at a time. The
hypervisor gave the time once, at boot; `systemd-timesyncd` corrected a
six-minute error with one step, the moment it was turned on again.

**A paused virtual machine was not run.** Lima 2.2.1 has no pause, and
stopping the hypervisor's process to imitate one ended the guest (the
framework reported the machine in error a minute later). How ariesd
fares across a pause depends on whether the guest's monotonic clock
stops with it: if it does, the daemon sees a jump forward and extends
its leases, as above; if it keeps running, the daemon sees time pass
and ends the claims whose leases ran out, as after an outage of that
length.

## Settings

Settings come from flags, then `ARIESD_*` environment variables, then
the config file (`/etc/aries/ariesd.yaml`, or `--config FILE`, or
`ARIESD_CONFIG`), then defaults.

| Key | Holds | Default | Flag and variable |
|---|---|---|---|
| `dsn` | Dolt's address and account, in [go-sql-driver/mysql](https://github.com/go-sql-driver/mysql#dsn-data-source-name) form | required | `--dsn`, `ARIESD_DSN` |
| `project` | The project's UUID, in lowercase. A client naming another project is refused. With `control_db`: the project `control init` adopts, and the one `import-bd`, `export-bd` and the health check of `upgrade` use by default | required | `--project`, `ARIESD_PROJECT` |
| `control_db` | The name of the control database, on the same Dolt server and account as `dsn`. Setting it makes this a server of [many projects](#serving-many-projects). A lowercase letter, then lowercase letters, digits and underscores; not the database in `dsn` | none: one project | `ARIESD_CONTROL_DB` |
| `project_dbs` | With `control_db`: the block of database names granted for new projects, as `prefix` and `count` ([Opt in](#opt-in), step 4). The names are `prefix` and three digits, `001` to `count` (at most 999) | none: no project can be added | |
| `keys_file` | With `control_db`: the file ariesd writes the control database's keys to, for sshd ([Keys from the store](#keys-from-the-store)). A relative path is under the home directory of the user ariesd runs as | `.ssh/aries_keys` | `ARIESD_KEYS_FILE` |
| `authorized_keys` | With `control_db`: the hand-maintained file sshd reads beside it. ariesd only reads it, to say which keys are in both. Never the same file as `keys_file` | `.ssh/authorized_keys` | `ARIESD_AUTHORIZED_KEYS` |
| `bridge` | With `control_db`: the path of ariesd in the forced command of each line of `keys_file`. Absolute, of letters, digits and `._+-` | `/usr/local/bin/ariesd` | `ARIESD_BRIDGE` |
| `socket` | The daemon's unix socket | `/run/aries/ariesd.sock` | `--socket`, `ARIESD_SOCKET` |
| `prefix` | The prefix of new issue ids | `sf` | `--prefix` |
| `admins` | [Admins](#admins) | none | `ARIESD_ADMINS` |
| `systemd_unit` | The unit `ariesd upgrade` restarts and health-checks | none | |
| `releases` | Where `ariesd upgrade` reads releases from: the `https://` URL of a mirror, or the absolute path of a directory ([Upgrades](#upgrades)) | GitHub | `ARIES_RELEASES`, the variable `aries` and `install.sh` read |
| `log_level` | `debug`, `info`, `warn` or `error` ([Logs](#logs)) | `info` | `--log-level`, `ARIESD_LOG_LEVEL` |
| `log_format` | `text` (key=value) or `json`, for a log shipper | `text` | `--log-format`, `ARIESD_LOG_FORMAT` |
| `latest` | The newest Aries release, set by hand. Clients' `prime` notes when this server is older | none | |
| `account` | The project's account: what an issue's time and tokens report against when neither it nor an ancestor sets one ([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)). A code name: lowercase letters, digits and inner hyphens | `internal` | |
| `clock_behind` | What `serve` does when it finds the clock behind the server's own history by more than `clock_behind_tolerance`: `stop` refuses to start, `warn` starts with a warning that repeats ([The server's clock](#the-servers-clock)). Any other value is refused | `stop` | `ARIESD_CLOCK_BEHIND` |
| `clock_behind_tolerance` | How far behind its history the clock may be at start before `clock_behind` applies. 1s to 24h. Raise it only for a machine known to boot with a clock further off than this and no time service to correct it; lower it if ariesd always starts after synchronization | `2m` | |
| `clock_step_tolerance` | How far elapsed wall-clock time and elapsed monotonic time may differ between two readings before ariesd calls it a step of the clock. 1s to 10m. Lower it, to 1s at the least, to be told of smaller corrections; raise it only if a time service on the machine is known to step by more in normal running | `5s` | |
| `clock_client_tolerance` | How far past the server's clock a time a client supplies may be: a usage record's. 1s to 24h. Raise it if people's machines are known to run ahead and their usage records are being dropped | `5m` | |
| `limits` | [Limits](#limits) | defaults | |
| `telemetry.listen` | The loopback address, `HOST:PORT`, of the listener for `/metrics`, `/healthz` and `/readyz` ([Observability](#observability)). An address that is not loopback is refused | none: no listener | `ARIESD_TELEMETRY_LISTEN` |
| `telemetry.per_project` | Whether metrics name the project. `false` leaves it off every instrument | `true` | |
| `telemetry.principals` | How a person appears on a span and on a log record exported over OTLP ([Traces](#traces), People): `pseudonym`, a stable stand-in; `name`; or `omit`, nothing. It does not change the log on stderr, nor the audit stream there. Any other value is refused | `pseudonym` | |
| `telemetry.audit_log` | Whether each access change in the control database's log is also written as a log line with `audit=true` ([The audit stream](#the-audit-stream)). `false` writes none; the event log is the record either way | `true` | |
| `telemetry.principals_key` | The file that holds the key the stand-ins are made with. ariesd makes it, mode 0600, when it exports traces with stand-ins. A relative path is under the home directory of the user ariesd runs as. A file that cannot be read or made does not stop ariesd, whether the path is this default or one you set: its spans then say nothing about who, and its log and status say why ([Traces](#traces), People) | `telemetry/principals.key` | |
| `telemetry.dolt_metrics_url` | The URL of Dolt's own metrics endpoint, which ariesd reads every 15 seconds for the status report ([Dolt's figures](#dolts-figures)): `http://`, a loopback address or a name whose every address is loopback, a port and a path, such as `http://127.0.0.1:11228/metrics`. Anything else is refused at start | none: nothing is read, and the report says nothing of Dolt | `ARIESD_TELEMETRY_DOLT_METRICS_URL` |

- **A clock tolerance is more than zero, and has a ceiling.** Zero, a
  negative duration and a value past the range are refused at start
  with a fix; leaving the key out takes the default. No value turns a
  check off: a tolerance of days would do that without saying so, and
  `clock_behind: warn` is the way to start whatever the clock says.
- **A password never goes on the command line.** `--dsn` with a password is
  refused, because it would show in the process list. A config file that
  holds one must be mode 0600.
- **Where metrics and traces are pushed is not a setting here.** It
  comes from the standard `OTEL_*` environment variables
  ([Observability](#observability)), and a token for a hosted backend
  from the environment only.
- **The Dolt account is checked** by every command that opens the store:
  `serve`, `control init`, `import-bd`, `export-bd` and `upgrade`. [The Dolt
  account](security-model.md#the-dolt-account) lists what they refuse.
- **`--dev`** is for a developer's own machine. It lets `serve` run off
  Linux. With `--dev`, the commands that open the store also accept
  `--allow-unsafe-dolt`, which lets a default Dolt through with a warning.
  No config file or variable can set either.

## Limits

`limits:` in the config file bounds what one request or one principal can
make the daemon do. Leave a field out for its default:

```yaml
limits:
  write_rate: 20
  write_burst: 200
```

| Setting | Default | Bounds |
|---|---|---|
| `labels_per_issue` | 50 | Labels on one issue, and so in one `create` |
| `acceptance_items` | 200 | Items in acceptance text, and item numbers in one `accept` or `finish` |
| `deps_per_issue` | 200 | Edges out of one issue |
| `sessions_per_principal` | 256 | A principal's rows in the `who` registry; a new session past it drops the least recently seen |
| `agent_keep` | `7d` | How long an unseen registry row is kept. A principal's latest row is always kept, so it stays mentionable |
| `inbox_unread` | 1000 | A principal's unread inbox items; past it, the oldest are marked read (still under `inbox --all`) |
| `notices_per_minute` | 10 | Mentions, assignments and handoffs one principal can send another a minute; the rest are not delivered. Lost claims, and locks granted or lost, always are |
| `inbox_keep` | `30d` | How long a read inbox item is kept |
| `idem_window` | `7d` | How long an idempotency key gives back its request's result: a write a client sends again with the same key within it, as an outbox does after an outage, applies once and gets the first answer. A key used longer ago is refused with `stale`, saying its request was applied then, and nothing is written; the same key with other arguments is `conflict`. It is how long a write can wait on a person's machine and still be delivered without them. From `1h` to `365d`. Keys are kept on the events they stamped whatever this is, so it bounds no table: a key is never applied twice, however late it comes back |
| `conns` | 1024 | Connections past the handshake, refused with `busy` beyond it. Twice this caps the sockets the daemon holds in all; at that cap a new connection is let in and the one that has waited longest without a hello is closed |
| `conns_per_principal` | 64 | One principal's connections: one person's sessions on several machines, and a few unattended runs on their key. Agents that run unattended in numbers should have principals of their own, and meet `conns` instead. Each connection costs the server about 5 MiB, most of it the bridge and sshd outside ariesd ([What a server needs](#what-a-server-needs)): 64 connections about 320 MiB, and `conns`' 1,024 about 5 GiB (550 MiB and 8.6 GiB with transparent huge pages `always`). Past it, every new connection of the principal is refused with `busy`, `alice is at its connection limit (64)`, until one of its sessions closes: an `aries` command fails, saying to close some sessions or have this limit raised; an `aries mcp` session that is refused cannot reach the server at all, tells its agent so, and tries again at most every 5 seconds. Its claims lapse if it stays refused for their 15-minute lease, and its writes are not queued |
| `handshakes_per_principal` | 16 | One principal's connections that sshd has let in and that have not yet sent their hello, on the whole server. A connection has 10 seconds to send it. Past the cap the next is refused with `busy` at once, so one key holder cannot use up the sockets everyone else connects through. The upgrade's health probe is not counted |
| `idle_timeout` | `10m` | A connection that sends nothing this long is closed with a note, unless it watches its inbox (`aries mcp`, `aries watch` and `aries tui` do) |
| `write_rate`, `write_burst` | 10, 100 | Each principal's write token bucket: writes a second, and how many at once. Reads are not counted |
| `refusal_logs` | 20 | Refusal log lines per principal a minute |
| `usage_records` | 500 | Token usage records in one `usage` call |
| `usage_per_day` | 50000 | Token usage records one principal may add in 24 hours, counted by when the server stored them. Per principal, because a client can name any number of sessions |
| `paths_per_issue` | 200 | Paths one issue keeps, declared and from commits, and the commit paths one `renew`, `finish` or `handoff` records. Too many declared paths are refused with `invalid`; past the cap, commit paths keep the request's first (most recent) and the issue's most recently recorded, and the request still succeeds |
| `memory_body` | 4096 | Bytes in one memory's body (at most 65535) |
| `memory_key_length` | 128 | Bytes in a memory's key (at most 255) |
| `memory_tags` | 20 | Tags on one memory |
| `memory_tag_length` | 64 | Bytes in one memory tag (at most 255) |
| `memories_per_scope` | 1000 | Memories one principal may author in one scope, forgotten ones included. A new key at the cap first deletes that principal's oldest forgotten memories in the scope; only when live memories fill it is the key refused with `invalid`, and its fix says to forget some or raise the limit; replacing an existing memory is not refused. `import-bd` is exempt |
| `prices` | 1000 | Rows of the prices table (at most 10000, since every cost report, show and digest reads them all): a model's rates from one date are one row. A new price past it is refused with `invalid`, and its fix says to replace an existing one or raise the limit; replacing a price is not refused |
| `plans` | 1000 | Rows of the plans table (at most 10000, since every cost report reads them all): a plan's terms from one month are one row. A new row past it is refused with `invalid`, and its fix says to replace an existing one or raise the limit; replacing a row is not refused |
| `plan_principals` | 100 | Principals one plan row names (at most 1000). `plans` times `plan_principals` is at most 100000, the principals a cost report may read, so raising one past its default means lowering the other; the server refuses to start otherwise |
| `hours_per_day` | 50 | Hours entries one principal may have on one day. Past it a new entry is refused with `invalid`, and its fix says to undo some and log their sum, or raise the limit |
| `hours_note` | 500 | Bytes in an hours entry's note (at most 65535) |
| `conflicts_per_issue` | 20 | Open conflicts on one issue: fields of replayed updates parked against others' changes ([Conflicts](cli.md#conflicts)). An update that would park past it is refused with `invalid`, and nothing of it applies; its fix says to resolve the open ones (`aries conflicts`, `aries resolve`) and `aries outbox retry` it, or to raise the limit. A resolved conflict no longer counts |
| `conflicts_per_principal` | 100 | Open conflicts one principal's replayed updates have parked, on every issue; past it, refused as `conflicts_per_issue` is |
| `conflict_age` | `1d` | How long a conflict is open before `digest` lists it, under old conflicts. It bounds nothing |
| `gate_approvers` | 10 | Principals one [gate](concepts.md#gates) names (at most 100). More are refused with `invalid` |
| `open_gates` | 1000 | Open gates in a project. One more is refused with `busy`, and its fix says to pass or waive an open one (`aries gate list`) or raise the limit. The gates on one issue are its dependencies, so `deps_per_issue` bounds them |
| `gate_checks` | 20 | [CI gates](ci.md) whose subject is one issue, which is its checks, one gate each (at most 100). One more is refused with `invalid` |
| `gate_evidence` | 1024 | Bytes of a CI report's evidence, its commit, link and reason together (at most 4096). More is refused with `invalid`, as is a link with a user or password, or evidence that looks like a secret |
| `locks` | 200 | [Locks](concepts.md#locks) in use in a project: names someone holds or waits for (at most 10000). A lock nobody holds or waits for is gone and does not count. One more is refused with `busy`, and its fix says to see `aries lock list` or raise the limit |
| `lock_capacity` | 64 | The capacity a lock may be given, holders at once (at most 1000). More is refused with `invalid` |
| `lock_waiters` | 100 | Places in one lock's queue (at most 1000). One more is refused with `busy` |
| `lock_waits_per_principal` | 20 | Places one principal has in all of a project's queues, from every session (at most 1000). One more is refused with `busy`, and its fix says to leave a queue (`aries lock leave NAME`) or raise the limit. A principal's holds need no cap of their own: `locks` times `lock_capacity` bounds them |
| `due_soon` | `1d` | How far ahead of its due date `digest` and `prime` list an issue as due soon ([Dates](concepts.md#dates)). Up to a year. It bounds nothing |
| `away_max` | `7d` | The longest `aries away` may extend a person's claims, in every session, ahead of an absence; asking for longer is refused with `invalid`, its fix naming the cap. A claim so extended holds its issue against everyone else until then, so this bounds how long work can be held by someone who is not doing it. From `1h` to `30d` |
| `open_stores` | 64 | With `control_db`: project stores open at once. Each holds one connection to Dolt when idle and up to nine under load. At the cap, a connection to another project closes the store that has been without connections longest; when every open store has one, it is refused with `busy` |
| `store_idle` | `10m` | With `control_db`: how long a project's store stays open with no connection. Closing it gives back its connections to Dolt, not Dolt's memory |
| `projects` | 500 | With `control_db`: projects on the server, archived ones included |
| `principals` | 5000 | With `control_db`: principals in the control database, disabled ones included |
| `principals_per_creator` | 500 | With `control_db`: principals one principal who is no server admin may create, disabled ones included. No command deletes a principal and names are the whole server's, so this keeps one project admin from using up `principals`. Past it `principal add` is refused with `invalid`, and its fix says to ask a server admin, who has no such cap, or to raise the limit |
| `keys_per_principal` | 20 | With `control_db`: public keys of one principal |
| `members_per_project` | 1000 | With `control_db`: principals with a role in one project |
| `telemetry_requests` | 8 | With `telemetry: listen:`: requests the listener serves at once. One more is answered `503`; twice as many connections may be open, and one more is closed |
| `control_write_rate`, `control_write_burst` | 1, 30 | With `control_db`: each principal's token bucket for `aries admin` changes to projects, principals, members and keys: changes a second, and how many at once. It is the principal's on the whole server, apart from its write bucket in each project; the lists are not counted |

On a server of [many projects](#serving-many-projects), every limit above
that is per principal is per principal in each project: `conns_per_principal`,
the write bucket, `refusal_logs` and the limits on rows a principal owns.
`conns` stays the whole server's, and the `aries admin` bucket is each
principal's on the whole server. So is `handshakes_per_principal`, since
a connection has named no project before its hello. Past `principals`,
`keys_per_principal` or `members_per_project`, `control init` skips the
line and says which limit it met, and `aries admin` refuses the row with
`invalid` and names the limit; past `projects` it refuses a new project
the same way. `principals_per_creator` applies to a principal who is no
server admin, and never to `control init`.

Before the hello, a server of many projects also refuses at once a
principal that is disabled, or that the control database does not know
and `admins:` does not name: its key may still be one sshd accepts, and
it would otherwise hold a connection until the hello's deadline.
Membership of the project, and whether the key that signed in is still
the principal's, are decided at the hello, which names the project.

A request past a per-request cap is refused with `invalid`. A write past
the write rate, or an `aries admin` change past its own, is refused with
`busy`, and its fix says how long to wait.
A `usage` call past `usage_per_day` is refused with `busy`, and stores
none of its records. A
connection past a connection cap is also refused with `busy`, and its fix
asks you to close other Aries sessions or wait for them to end; past
`conns_per_principal` it also names the setting to raise.

Fixed caps that no setting changes:

- **Text:** titles 500 bytes, names 255, bodies and comments 64 KiB.
- **Pages:** no reply may pass the 4 MiB frame. `comments` and `history`
  return the newest page (up to 100 entries, 500 with a limit, about 1 MiB
  of text) and a cursor to the page before, which `aries` follows.
- **Lists:** the `aries admin` lists of projects, principals and members
  show 200 rows, or up to 1000 with `-n`, and count the rest, which
  `--after NAME` lists. `key list` shows every key of the principal, at
  most `keys_per_principal`, and one `principal add` carries at most 20
  keys. `who` lists at most 100 sessions and counts the
  rest; `show`
  lists at most 200 edges and `blocked` 50 blockers an issue, each with a
  count of the rest. `show` and `digest` list tokens for at most 20
  models, summing the rest as `(other)`.
- **Usage records:** each count is at most 10¹² tokens, a record's time
  at most `clock_client_tolerance` (5 minutes) past the server's clock
  and no earlier than 2020, and a turn's or session's span
  at most 7 days; a longer session reports in parts. `show` and `digest`
  attribute at most 100,000 records a read, and mark the tokens partial
  past that. `show` reads only the records made while the issue was held,
  and the spans that reach into those stretches.
- **Cost reports:** `cost` covers a window of at most 366 days, prices at
  most 100,000 records and marks the report partial past that, and lists
  at most 500 groups, summing the rest as `(other)`. A rate, and a plan's
  fee a seat a month, is at most 1,000,000 US dollars; a plan has at most
  100,000 seats.
- **Hours:** an entry is from a minute to 24 hours, on a day at most a
  year back and at most one past the server's today (UTC), and one
  principal's entries on one day add up to at most 24 hours. `show`
  names at most 20 people per issue, summing the rest as `(other)`; `aries
  log` lists at most 500 entries. Entries are bounded per day, but the
  events of logging and undoing them are not: a principal who logs and
  undoes over and over grows the event log at the write rate, as any
  write does.
- **Similar issues:** lookups read closed titles from a cache refreshed on
  close and reopen, or after a minute.

## Confine ariesd with AppArmor

This step is optional. It was tested on Ubuntu 24.04, on arm64, in a
virtual machine, on 12 October 2026, and on Ubuntu 26.04 the same way
on 14 October 2026
([What was run, and where](#what-was-run-and-where)); amd64 was not
tested.

Ubuntu runs AppArmor, and out of the box it confines neither sshd
nor a program it has no profile for: on the machines above, `aa-status`
listed no profile for sshd, sshd and its children ran `unconfined`, and
so did ariesd. Aries ships three profiles for ariesd
([deploy/apparmor](../deploy/apparmor/README.md)). They matter most for
the bridge, `ariesd stdio`: sshd starts it for every connection,
outside the systemd unit and so with none of the unit's protections,
as a user that owns its own binary, its config file and its
`authorized_keys`.

| Profile | Runs | May |
|---|---|---|
| `ariesd` | `ariesd stdio`, `ariesd status`, and anything else run from the binary's path with no profile asked for | Read the config file and connect to the daemon's socket. No network, no other file, no program |
| `ariesd-serve` | `ariesd serve`, because the unit asks for it | Read the config file, listen on the socket, reach Dolt over TCP, for SQL and for [its metrics](#dolts-figures), write the keys file and read `authorized_keys`; listen on the loopback port for metrics and health, post metrics and traces to a collector over TCP, make and read the key of the stand-ins its traces name people by, and write to systemd's notify socket ([Observability](#observability)). No program |
| `ariesd-admin` | `control init`, `import-bd`, `export-bd` and `upgrade`, when the operator asks for it | Open the store, read and write files in the top of `/var/lib/aries`, read `authorized_keys`, reach the network, replace the binary in `/opt/aries/bin`, and run `sudo` |

AppArmor picks a profile by the program's path and never by its
arguments, so one path can carry one profile by itself. It carries the
smallest, since sshd has no way to ask for another. None of the three
may write the config file or `authorized_keys`, and only the operator's
may write the binary.

**Install them**, as root:

```sh
cd "$(mktemp -d)"
curl -fsSL -o profile https://raw.githubusercontent.com/ariesworx/aries/main/deploy/apparmor/ariesd
curl -fsSL -o tunables https://raw.githubusercontent.com/ariesworx/aries/main/deploy/apparmor/tunables/ariesd
install -m 0644 profile /etc/apparmor.d/ariesd
install -m 0644 tunables /etc/apparmor.d/tunables/ariesd
apparmor_parser -r /etc/apparmor.d/ariesd
mkdir -p /etc/systemd/system/ariesd.service.d
printf '[Service]\nAppArmorProfile=ariesd-serve\n' > /etc/systemd/system/ariesd.service.d/apparmor.conf
systemctl daemon-reload
systemctl restart ariesd
cat /proc/"$(systemctl show ariesd -p MainPID --value)"/attr/current
```

The last command prints `ariesd-serve (enforce)` on 24.04, and
`ariesd-serve//&unconfined (enforce)` on 26.04. The two files are
short and are not part of a signed release, so read them before you
install them. AppArmor loads everything in `/etc/apparmor.d` at boot.
To watch before you enforce, load them with `apparmor_parser -r -C`
instead: AppArmor then logs what it would have refused and refuses
nothing.

**On Ubuntu 26.04 a profile changed to is stacked.** There
`kernel.apparmor_restrict_unprivileged_unconfined` is 1 (on 24.04 it
is 0), so when a process that is not root changes to a profile, as
systemd does for the unit's `User=aries` and `aa-exec` does for the
`aries` user, the kernel stacks the profile on `unconfined`:
`ariesd-serve//&unconfined`, and `ariesd-admin//&unconfined`. The
profile still decides, since a stack allows only what each of its
profiles allows: under the operator's, `export-bd -o` into `.ssh` was
still refused and logged. The bridge, which its path
attaches, is a plain `ariesd`. What the stack changed, each in this
release's profiles:

- **A process could not signal its own threads.** Go preempts a
  goroutine with `SIGURG`, and the base rules allow signals to the
  profile's own name, which the stacked label is not. The kernel logged
  `operation="signal" … signal=urg peer="ariesd-serve//&unconfined"`.
  The profiles now allow that peer too.
- Not the stack, but the same release: **`sudo` is sudo-rs**, and
  `/usr/bin/sudo` leads to `/usr/lib/cargo/bin/sudo`, or to
  `/usr/bin/sudo.ws` where the machine chose the original. AppArmor
  names the file a link leads to, so the operator's profile, which
  named only `/usr/bin/sudo`, refused it, and `ariesd upgrade` could
  not restart the unit (`fork/exec /usr/bin/sudo: permission denied`).
  The profile now names all three.

A server whose profiles are older than 14 October 2026 needs this
release's on 26.04: install them again as above. The installer shows
how the installed file differs, and leaves it to you.

**Run the operator's commands through `aa-exec`** from then on, as the
`aries` user as before:

```sh
cd /var/lib/aries
sudo -u aries -H aa-exec -p ariesd-admin -- ariesd upgrade --check
sudo -u aries -H aa-exec -p ariesd-admin -- ariesd upgrade
sudo -u aries -H aa-exec -p ariesd-admin -- ariesd control init --name acme
sudo -u aries -H aa-exec -p ariesd-admin -- ariesd import-bd bd.jsonl
sudo -u aries -H aa-exec -p ariesd-admin -- ariesd export-bd -o back.jsonl
```

- Without `aa-exec` such a command runs under the bridge's profile and
  is refused with `permission denied`: `control init` on
  `authorized_keys`, anything that opens the store on `dial tcp
  127.0.0.1:3306: socket`, and `upgrade` on its lookup of the release
  host (`api.github.com`, unless `releases:` names another).
- A `releases:` that is a directory on the server, as the installer's
  `--releases DIR` writes it, is refused under the operator's profile
  too (`open DIR: permission denied`). Allow it in
  `/etc/apparmor.d/local/ariesd-admin` with `DIR/ r,` and `DIR/** r,`,
  then `apparmor_parser -r /etc/apparmor.d/ariesd` (run on 26.04: the
  directory was then read).
- `import-bd` and `export-bd -o` take files in the top of
  `/var/lib/aries`, not below it or elsewhere. A file your shell
  opens is not limited: `ariesd import-bd - < FILE`, `ariesd
  export-bd > FILE`, or a pipe.
- `aa-exec` is in Ubuntu's `apparmor` package, which is installed.
- **`ariesd status` needs no `aa-exec`**: `sudo -u aries -H ariesd
  status` runs under the bridge's profile, which allows the two things
  it does. root cannot run it directly here, because that profile gives
  root no way past the config file's mode; the refusal says to run it
  as `aries`.

**If you moved a path** (the binary, the config file, the socket, the
home directory, `keys_file`, `authorized_keys` or
`telemetry: principals_key`), change it in
`/etc/apparmor.d/tunables/ariesd`, which names each one once, and
load the profiles again with `apparmor_parser -r
/etc/apparmor.d/ariesd`. Rules of your own go in
`/etc/apparmor.d/local/ariesd`, `local/ariesd-serve` and
`local/ariesd-admin`. A `dsn` that names Dolt's host by a name that
needs DNS, where the guide's has an address, needs UDP allowed in
`local/ariesd-serve`; the profile's comments give the two lines.

**Metrics and traces sent straight to a hosted backend need three lines** in
`/etc/apparmor.d/local/ariesd-serve`, then `apparmor_parser -r
/etc/apparmor.d/ariesd` and a restart. The daemon otherwise speaks to
no machine by name and reads no certificate, so the profile as shipped
has neither DNS nor the certificate authorities:

```text
network inet dgram,
network inet6 dgram,
include <abstractions/ssl_certs>
```

A collector on the machine, at an address or at a name `/etc/hosts`
holds, needs none of them, and neither does the listener. Without the
first two, the export fails on `lookup … dial udp …: socket: permission
denied`; without the third, on `x509: failed to load system roots`. Each
is logged once and changes nothing else.

**To see what was refused:**

```sh
journalctl -k -g 'apparmor="DENIED".*profile="ariesd'
```

Each line names the profile, the operation and the path or the kind of
socket. In correct use there are none. Without the profile's name the
command also shows other programs' refusals: Ubuntu 26.04 ships
profiles for many of its own programs, and on the 26.04 machine the
kernel logged some at boot (`who`, `systemd-detect-virt`).

**To remove them:**

```sh
apparmor_parser -R /etc/apparmor.d/ariesd
rm /etc/apparmor.d/ariesd /etc/apparmor.d/tunables/ariesd
rm /etc/systemd/system/ariesd.service.d/apparmor.conf
systemctl daemon-reload
systemctl restart ariesd
```

Unload the profiles and remove the unit's line together. With the line
and no profile, ariesd does not start (`Failed to prepare AppArmor
profile change to ariesd-serve`). With the profiles and no line, the
daemon gets the bridge's profile and cannot reach Dolt.

What they do not do:

- **They do not keep a daemon that has been taken over away from the
  keys file.** `ariesd-serve` must write the file sshd reads keys
  from, and sshd accepts any line in it. The bridge and the operator's
  commands cannot write it.
- **They do not name Dolt's address.** `ariesd-serve` may open TCP
  connections anywhere: rules that name an address or a port were not
  tested, and the profile does not rely on them. The unit can hold the
  daemon to this machine instead: `IPAddressDeny=any` and
  `IPAddressAllow=localhost` in a drop-in, as Dolt's unit has, were run
  on the machine above with the guide's Dolt on 127.0.0.1, and ariesd
  still read Dolt's metrics there. Leave them
  out if `dsn` names another machine.
- **They do not hold the listener to loopback.** ariesd does that
  itself, refusing any other address at start; to AppArmor the listener
  is one more TCP socket.
- **They confine neither sshd nor Dolt.** Ubuntu ships no profile for
  sshd, and both of its keys files worked with the bridge confined.
  [deploy/apparmor](../deploy/apparmor/README.md) says what a profile
  for Dolt would need. Dolt's metrics endpoint is one more socket Dolt
  listens on, kept on loopback by Dolt's own config
  ([security model](security-model.md#dolts-metrics-endpoint)).

What was tested, with the profiles enforced: a sequence taken from the
[Docker smoke test](deploy/docker.md#run-the-smoke-test) (issues, a
second project, keys from the control database, a key removed and a
principal disabled, restarts) and the operator's commands above, 76
checks, before and after a reboot, with no line in the kernel log; then
attempts to do what the profiles forbid, each refused and logged. The
76 checks were run again, and passed with no line in the kernel log,
with the unit's `Type=notify` and watchdog, the listener and metrics
pushed to a receiver on the machine; each of the three lines above was
shown to be needed, and to be enough, against a receiver named through
DNS and one behind TLS on the same machine. They were run a third time,
and passed with no line in the kernel log, with Dolt's metrics on and
ariesd reading them every 15 seconds, which needed no rule added:
to AppArmor it is one more TCP connection. What
was not: an upgrade to a newer release (its `--check` and `--rollback`
were run), and amd64.

On Ubuntu 26.04 (14 October 2026), with this release's profiles
enforced: the installer's run with `--apparmor`, the `aries` checks of
[With the installer](#with-the-installer), 300 requests from three
clients at a time, `ariesd status`, `control init`, `export-bd` and
`upgrade --rollback --restart` under the operator's profile, and cold
boots, two of them checked, with nothing of ariesd's denied; a command run without
`aa-exec`, and a write into `.ssh` under the operator's profile, were
each refused and logged. The two changes above were each seen needed:
with the profiles as they were, the kernel refused `SIGURG` three times
within half a minute of the daemon's first start, and `upgrade --rollback --restart`
restored the binary and could not restart the unit. A throwaway
profile entered as a user other than root could not signal itself
without the new rule and could with it. `SIGURG` was not refused again
under load with either profile, so the daemon's own run shows the rule
needed, not that it suffices. The 24.04 machine ran the installer with
these profiles too: `ariesd-serve (enforce)`, the `aries` checks,
`upgrade --rollback --restart`, and a cold boot, with no denial.

## Logs

ariesd logs to stderr, which systemd sends to the journal
(`journalctl -u ariesd`), as text (`log_format: text`, the default)
or as one JSON object a line (`log_format: json`). Either way a line
has the same fields, and the fields and the event names below are a
contract: one is added, never renamed or removed, so an alert or a
parser written against them keeps working.

```text
time=2026-10-12T09:30:00.123Z level=INFO msg=request project=0190a8a0-0000-7000-8000-00000000000a principal=alice session=s-abcdefgh machine=laptop event=request.refused op=show dur_ms=2 code=not_found req=3f2a9c0e51b7d684 trace_id=4bf92f3577b34da6a3ce929d0e0e4736 span_id=00f067aa0ba902b7
```

```json
{"time":"2026-10-12T09:30:00.123Z","level":"INFO","msg":"request","project":"0190a8a0-0000-7000-8000-00000000000a","principal":"alice","session":"s-abcdefgh","machine":"laptop","event":"request.refused","op":"show","dur_ms":2,"code":"not_found","req":"3f2a9c0e51b7d684","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7"}
```

- **Match `event`, not `msg`.** `event` is a stable name for what the
  line reports; `msg` is a sentence for a person and may be reworded.
  Every line has an `event`.
- **`req` is the request's id:** 16 hex digits, on every line ariesd
  logs for one request, and in the refusal the client is sent. When a
  request fails on the server (`unavailable`), `aries` prints it as a
  third line, `req:`, and `aries --json` and an agent's tool error carry
  it, so a report of the failure finds its lines:
  `journalctl -u ariesd | grep 3f2a9c0e51b7d684`.
- **`trace_id` and `span_id`** are those of the span the line was logged
  under ([Traces](#traces)): a request's, a handshake's, or a part of
  the reaper's tick. With traces off, a request's line has the caller's
  ids when the request brought a valid trace context, and none
  otherwise.
- **The schema is flat:** no field holds an object.
- **Nothing a person or an agent wrote is logged:** no issue's title or
  body, no memory's text, no comment, no key and no DSN. A line does
  carry what a client called itself and asked for, as it came:
  `session`, `machine`, `harness` and `op`. An `err` is the server's own
  error, which may name an issue's id, a principal or a project.

### Fields

| Field | Holds |
|---|---|
| `time`, `level`, `msg` | When, `DEBUG`, `INFO`, `WARN` or `ERROR`, and a sentence for a person |
| `event` | The line's stable name, from [Events](#events) |
| `project` | A project's UUID. With `control_db`, on every line about one project |
| `principal`, `session` | Whose connection or request the line is about; on a claim's line, the holder; on an audit line, who made the change |
| `machine`, `harness` | What the client called its machine and its agent harness |
| `op`, `code`, `dur_ms` | A request's operation, a refusal's code, and how long the request took in milliseconds |
| `req`, `trace_id`, `span_id` | The request's id and its span's ids, above |
| `audit`, `seq`, `at`, `target`, `subject`, `fingerprint`, `role` | The [audit stream](#the-audit-stream)'s. `subject` and `fingerprint` are also on other lines about a principal or a key that is not the line's `principal` |
| `by`, `admin`, `created_by`, `added_by` | A principal other than the line's own: whose change caused it, a configured server admin, and who made that admin's row or added its key |
| `err`, `detail`, `cause`, `fix`, `effect` | An error's text; what the database driver, the SDK or the HTTP server said; why something is wrong, what to do about it, and what follows meanwhile |
| `issue`, `epoch`, `reason` | A claim's issue and number, and a few fixed words for why (`membership removed`, `principal disabled`, `project archived`, `key removed`, `idle`, `to make room`, `archived`, `connection limit`) |
| `name`, `database`, `account`, `problems`, `resumed` | A project's name and database; the Dolt account and what is unsafe about it; whether a `project add` resumed one begun before |
| `after`, `oldest`, `connections`, `own`, `lines`, `per_minute` | Connections: how long one idled or how long before a retry, whether the one closed was the oldest waiting, how many were dropped and whether the admin's own was one, and how many refusal lines the cap left out of how many allowed |
| `agents`, `inbox`, `claims`, `locks`, `extended` | How many registry rows and inbox items a prune removed; how many claims' leases, and locks' holds and places, a clock step extended, and by how much |
| `lock`, `waiting` | A [lock](concepts.md#locks)'s name, and whether what ended was a place in its queue rather than a hold. A lock's name is text a person chose, so it stays in the journal and is never exported ([Logs over OTLP](#logs-over-otlp)) |
| `path`, `keys` | The keys file, or the stand-in key's file, and how many keys were written |
| `log`, `clock`, `history`, `behind`, `since`, `was_behind_since`, `direction`, `step`, `steps`, `from`, `to`, `forward_since_start`, `backward_since_start` | [The server's clock](#the-servers-clock): which history, the two times, how far behind and since when, and a step's direction, size and count |
| `url`, `failures`, `after_failures`, `every` | [Dolt's figures](#dolts-figures): the URL read, how many reads have failed, and how often it is read |
| `version`, `socket`, `control_db`, `projects`, `admins`, `addr` | The start: the build, the socket, the control database, how many active projects, the configured admins, and the listener's address |
| `push`, `push_interval`, `push_headers`, `pull`, `per_project`, `traces`, `trace_headers`, `sampler`, `logs`, `log_headers`, `principals`, `service` | What telemetry is on ([Observability](#observability)). Header counts, never their names or values |
| `signal`, `failed`, `failures_not_logged` | An export that failed or recovered: `metrics`, `traces` or `logs`, and how many failures |

### Events

| Event | Level | Logged when |
|---|---|---|
| `daemon.serving` | info | ariesd listens on its socket |
| `admin.unvouched` | warn | At start: a principal named under `admins:` has a row or a key that someone who is no server admin made. Check the key is theirs |
| `admin.check_failed` | error | At start: that check could not read the control store |
| `systemd.notify_failed` | warn | A notification could not be sent to systemd: ready, or the watchdog's |
| `log.suppressed` | warn | A principal's refusal lines went past `refusal_logs` a minute; `lines` is how many were left out |
| `conn.accept_failed` | warn | Accepting a connection failed in a way that may pass, such as running out of file descriptors; ariesd retries after a pause of up to a second |
| `conn.cap_refused` | warn | A connection that had not said hello was closed at twice `conns` |
| `conn.peer_refused` | warn | A socket peer that is not the user ariesd runs as was refused |
| `conn.opened` | debug | A connection passed its handshake |
| `conn.idle_closed` | debug | A connection idled past `idle_timeout` |
| `conn.read_failed`, `conn.write_failed` | info | A connection ended on a read or a write that failed |
| `conn.access_ended` | info | A connection was closed because its own access ended, during its handshake or by its own request |
| `conn.dropped` | info | An `aries admin` change closed the connections of whoever lost access; `connections` is how many |
| `conn.key_lookup_ignored` | debug | An `ariesd` from before the keys file asked about a key; it is not answered |
| `handshake.failed` | info | A handshake was refused or failed; `err` says why |
| `agent.harness_ignored` | info | A client named a harness ariesd does not know |
| `agent.register_failed` | error | A session could not be noted for `aries who` |
| `request.done` | debug | A request was answered |
| `request.refused` | info | A request was refused, with its `code`. Capped at `refusal_logs` a minute per principal |
| `request.failed` | error | A request failed on the server, and its client was answered `unavailable`. The line with the client's `req` has the cause |
| `request.part_failed` | error | A request was answered, without a part that could not be read: an issue's similar issues, its memories, a new member's keys |
| `project.added` | info | `aries admin project add` made a project |
| `project.opened`, `project.closed` | info | A project's store was opened, or closed (`reason`: `idle`, `to make room` or `archived`) |
| `project.open_failed` | error | A project's database could not be opened: at start, at a handshake, or for an `aries admin` change. Alert on it |
| `project.not_opened` | info | At start: an archived project's database awaits this ariesd's migrations and was left alone |
| `project.close_failed` | error | Closing a project's store failed |
| `store.schema_too_new` | error | A database was migrated by a newer ariesd |
| `store.unsafe_account` | warn | `--allow-unsafe-dolt` let an unsafe Dolt account through |
| `control.reload_failed` | error | The reaper could not reread the control store |
| `control.index_stale` | error | A control write failed and the reread after it failed too: what ariesd holds in memory of keys and members may be stale until a restart |
| `claim.expired` | info | A claim's lease ran out and the reaper ended it |
| `claim.released` | info | A claim ended because its holder lost access; `reason` says how |
| `lock.expired` | info | A lock's hold, or a place in its queue (`waiting=true`), ran out of lease and the reaper ended it, granting the next waiter |
| `lock.released` | info | A lock's hold or place ended because its holder lost access; `reason` says how |
| `reaper.failed`, `reaper.prune_failed` | error | The reaper could not end expired claims, or prune, in a project |
| `reaper.pruned` | info | The reaper removed old registry rows or read inbox items |
| `access.sweep_failed` | error | The claims of holders without access could not be ended in a project |
| `keys_file.written` | info | The keys file was written |
| `keys_file.write_failed` | error | The keys file could not be written: sshd reads the old one. Every 30 seconds until a write succeeds. Alert on it |
| `keys_file.key_held_back` | warn | A key of the store is left out of the keys file because `authorized_keys` gives it to another principal |
| `keys_file.key_left_out` | error | A key's row in the store does not check out, and is left out of the keys file |
| `clock.behind` | error | The clock is behind the latest event of the database named, at start and every ten minutes while it lasts, with the fix ([The server's clock](#the-servers-clock)) |
| `clock.caught_up` | info | A clock that was behind has passed its history |
| `clock.stepped` | warn | The clock was set, or the machine suspended: the direction, the size and what ariesd did about it |
| `clock.step_settled` | warn | For each database, a forward step was added to every live lease |
| `clock.settle_failed` | error | A step could not be settled in the control store |
| `database.check_failed` | error | A check of the database failed: the one for `/readyz`, or the one made before a connection's project store is opened, which is then not opened (`project`, `database`) |
| `telemetry.on`, `telemetry.off` | info | At start: what is exported and where, or that `OTEL_SDK_DISABLED` turned it off |
| `telemetry.listening` | info | The loopback listener serves |
| `telemetry.listener_error` | warn | The listener's HTTP server reported an error |
| `telemetry.listener_stopped` | error | The listener stopped; metrics and health are not served until a restart |
| `telemetry.export_failed` | warn | An export of `signal` failed: once, then every 10 minutes while it lasts |
| `telemetry.export_recovered` | info | Exports of `signal` succeed again |
| `telemetry.sdk_error`, `telemetry.scrape_failed` | warn | The OpenTelemetry SDK reported an error, or a scrape of `/metrics` failed |
| `telemetry.stop_failed` | warn | The last export at the stop did not finish in its 5 seconds |
| `telemetry.key_made`, `telemetry.key_found` | info | The key of the stand-ins was made, or was found on a later look |
| `telemetry.key_unavailable` | error | That key cannot be read or made, so spans and exported log records say nothing about who. A line each for the file, the `cause` and every `fix` |
| `audit.project.add`, `audit.project.archive`, `audit.project.activate`, `audit.principal.add`, `audit.principal.disable`, `audit.principal.enable`, `audit.key.add`, `audit.key.remove`, `audit.member.set`, `audit.member.remove` | info | [The audit stream](#the-audit-stream): one line for each access change |
| `audit.read_failed` | error | The audit stream could not read the control store's log. Its lines follow when a read succeeds; the event log has lost nothing |

Any failure to accept connections that will not pass stops ariesd: it
prints the error and exits 1, and the unit's `Restart=on-failure`
starts it again. An error that stops a start is printed as plain lines,
the cause and then each `fix:`, not as a log line.

### The audit stream

With `control_db`, every change to who has access to what is an event
in the control database's log: a project added, archived or activated;
a principal added, disabled or enabled; a key added or removed; a
membership set or removed. ariesd also writes each as a log line with
`audit=true`, for a SIEM that reads the journal:

```text
level=INFO msg=audit event=audit.key.add audit=true seq=41 at=2026-10-12T09:30:00.123456Z principal=dana session=s-d op=key.add target=SHA256:fw8YS1jLRFPv61bKxSwx+1PRkYi+gXIIutriFmX8Bic fingerprint=SHA256:fw8YS1jLRFPv61bKxSwx+1PRkYi+gXIIutriFmX8Bic subject=carol
level=INFO msg=audit event=audit.member.set audit=true seq=42 at=2026-10-12T09:30:02.200100Z principal=dana session=s-d op=member.set target=0190a8a0-0000-7000-8000-00000000000a/carol project=0190a8a0-0000-7000-8000-00000000000a subject=carol role=member
```

| Field | Holds |
|---|---|
| `seq`, `at` | The event's number in the control database's log, and its time there. `seq` is what to deduplicate on, and what finds the event |
| `principal`, `session` | Who made the change |
| `op` | What it was: `project.add`, `key.remove` and so on |
| `target` | What it changed, as the event log names it: a project's UUID, a principal's name, a key's fingerprint, or `PROJECT/PRINCIPAL` for a membership |
| `project`, `subject`, `fingerprint`, `role` | The target's parts, where it has them: the project, the principal it changed, the key's fingerprint, and the role a membership was set to |

- **Fingerprints, never keys.** No line holds a key's text or its
  comment.
- **The event log is the record; the lines are a copy.** ariesd reads
  the lines from the log, after the change has committed, so a line can
  say nothing the log does not, and there is a line for a change
  whoever made it: `ariesd control init` run beside the daemon shows
  at the reaper's next tick, within 30 seconds.
- **A line appears once, and only while ariesd runs.** The stream
  starts at the newest event there is when ariesd starts. What was
  written while it was stopped (by `ariesd control init`) is in the
  event log and not in the journal, and the journal is not a substitute
  for backing the control database up.
- **A line is no request's.** It carries the event's `seq` and no `req`.
- **`telemetry: audit_log: false`** writes no audit line. The event log
  is unchanged.
- **To send the audit stream somewhere and nothing else**, filter on
  `audit=true` where the journal is read; with [logs over
  OTLP](#logs-over-otlp), on the record's `audit` attribute.

## Observability

ariesd can report on itself in open formats, so that whatever you
already monitor with can read it. **It is off by default:** with nothing
configured, ariesd exports nothing and opens no port.

| What | How it is turned on |
|---|---|
| Metrics pushed over OTLP/HTTP | `OTEL_EXPORTER_OTLP_ENDPOINT` in ariesd's environment |
| Traces pushed over OTLP/HTTP ([Traces](#traces)) | The same variable. They say who did what, by a stand-in, once a directory is made for a key |
| Log lines with a stable schema, and the audit stream of access changes ([Logs](#logs)) | Always there, on stderr. `telemetry: audit_log: false` leaves the audit stream out |
| The same log lines pushed over OTLP/HTTP ([Logs over OTLP](#logs-over-otlp)) | `OTEL_LOGS_EXPORTER=otlp` beside an endpoint. Off unless asked for |
| Metrics for a scraper, in Prometheus's text format | `telemetry: listen:` in the config file |
| `/healthz` and `/readyz` | The same listener |
| Readiness and a watchdog under systemd | `Type=notify` and `WatchdogSec` in the unit ([step 5](#5-run-ariesd-under-systemd)), which the guide's unit has |
| `ariesd status` | Always there, for the user ariesd runs as |
| Dolt's own figures in the status report | `metrics:` in Dolt's config and `telemetry: dolt_metrics_url:` in ariesd's ([Dolt's figures](#dolts-figures)); the guide's setup has both |
| Dolt's own metrics, for a scraper | The same `metrics:` block: `http://127.0.0.1:11228/metrics`. ariesd does not export them again |

Push and pull are independent: turn on either, both or neither. The
instruments are the same either way.

A collector's configuration, alert rules and a dashboard to start from
are in [deploy/observability](../deploy/observability):
[A collector, alert rules and a dashboard](#a-collector-alert-rules-and-a-dashboard).

### Send metrics to a collector on the machine

This is the setup to start with. An
[OpenTelemetry Collector](https://opentelemetry.io/docs/collector/), or
your vendor's or cloud's own agent, runs on the server, receives OTLP on
loopback and forwards to your backend. ariesd then holds no credential
and knows no vendor, and the collector does the retrying.

Add to the unit, then `systemctl daemon-reload` and restart ariesd:

```ini
[Service]
Environment=OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
```

The one variable sends both metrics and [traces](#traces), and that is
all it takes. The log says what is on:

```text
level=INFO msg="telemetry on" event=telemetry.on push=http://127.0.0.1:4318/v1/metrics push_interval=1m0s push_headers=0 pull=false per_project=true traces=http://127.0.0.1:4318/v1/traces logs=off trace_headers=0 sampler=parentbased_always_on principals=pseudonym service=ariesd
```

**Until you make one directory, the traces say nothing about who.**
Traces name people by a stand-in, made with a key that ariesd keeps
in a file, and the guide's unit gives it nowhere to write one. ariesd
starts and serves all the same, sends spans with no principal and no
session on them, exactly as `telemetry: principals: omit` does, and
says so at error level:

```text
level=ERROR msg="telemetry: spans say nothing about who: the key for principals' stand-ins is unavailable" event=telemetry.key_unavailable
level=ERROR msg="the key's file" event=telemetry.key_unavailable path=/var/lib/aries/telemetry/principals.key
level=ERROR msg=cause event=telemetry.key_unavailable cause="it cannot be made: no such file or directory"
level=ERROR msg=fix event=telemetry.key_unavailable fix="make its directory, owned by the user ariesd runs as: install -d -m 0700 -o aries -g aries /var/lib/aries/telemetry"
level=ERROR msg=fix event=telemetry.key_unavailable fix="under systemd, let ariesd write there with a drop-in: ReadWritePaths=/var/lib/aries/telemetry (docs/server.md, Traces)"
```

To have the stand-ins, as root:

```sh
install -d -m 0700 -o aries -g aries /var/lib/aries/telemetry
mkdir -p /etc/systemd/system/ariesd.service.d
printf '[Service]\nReadWritePaths=/var/lib/aries/telemetry\n' > /etc/systemd/system/ariesd.service.d/telemetry.conf
systemctl daemon-reload
systemctl restart ariesd
```

```text
level=INFO msg="telemetry: made the key that principals' stand-ins are derived from" event=telemetry.key_made path=/var/lib/aries/telemetry/principals.key
```

- **The restart is for the drop-in**, which systemd applies when the
  unit starts. Where ariesd may already write to the directory's
  parent, making the directory is enough: it looks for the key again
  every 30 seconds, and logs `spans name people by stand-ins from now
  on` when it has it.
- **`aries admin status` and `ariesd status` say which it is**
  ([Traces](#traces), People), so it is not only in the log.
- **To leave people out on purpose**, set `telemetry: principals: omit`:
  no key is wanted, and nothing is logged as wrong.
- **To send metrics and no traces**, add
  `Environment=OTEL_TRACES_EXPORTER=none`. No key is then made.

### Send metrics straight to a hosted backend

Where running a collector is not wanted, ariesd can post to a backend
that takes OTLP over HTTP: metrics and traces alike, and the traces
say who by a stand-in once the key's directory above is made. That
usually needs a token, which is a secret:

```sh
install -m 0600 -o root -g root /dev/null /etc/aries/telemetry.env
```

```sh
# /etc/aries/telemetry.env
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20REPLACE_WITH_THE_TOKEN
```

```ini
[Service]
EnvironmentFile=/etc/aries/telemetry.env
```

- **With the [AppArmor profiles](#confine-ariesd-with-apparmor)**
  this setup needs three lines added for the daemon, which that section
  gives: DNS, and the machine's certificate authorities. A collector on
  the machine needs none.
- **The token is read from the environment only.** There is no flag and
  no config key for it, so it cannot reach the process list or a config
  file that is not 0600. systemd reads the file as root before it drops
  to the `aries` user.
- **ariesd never logs it.** The start-up line gives the number of
  headers, not their names or values, and the endpoint without any user,
  password or query. An export error is logged with every header's value
  removed, even when the backend's answer repeats it.
- A space in a header's value is written `%20`: the variable is a
  comma-separated list of `name=value` pairs, percent-encoded.

### The variables ariesd reads

| Variable | Does |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector's base URL, `http://` or `https://`. ariesd posts metrics to `/v1/metrics` and traces to `/v1/traces` under it, and logs to `/v1/logs` when `OTEL_LOGS_EXPORTER` asks for them. Unset: nothing is pushed |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | The full URL for metrics, for traces or for logs, used as it is; each wins over the one above for its signal, and any may be set alone |
| `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_METRICS_HEADERS`, `OTEL_EXPORTER_OTLP_TRACES_HEADERS`, `OTEL_EXPORTER_OTLP_LOGS_HEADERS` | Headers sent with each export, as `name=value,name=value`; a signal's own win for a name in both. A secret |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL`, `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL` | Must be unset or `http/protobuf`. ariesd has no gRPC exporter, and refuses `grpc` at start |
| `OTEL_EXPORTER_OTLP_TIMEOUT`, `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT`, `OTEL_EXPORTER_OTLP_TRACES_TIMEOUT`, `OTEL_EXPORTER_OTLP_LOGS_TIMEOUT` | One export request's timeout, in milliseconds. Default 10000 |
| `OTEL_METRIC_EXPORT_INTERVAL` | Milliseconds between exports of metrics. Default 60000 |
| `OTEL_METRIC_EXPORT_TIMEOUT` | Milliseconds one collection and export of metrics may take. Default 30000 |
| `OTEL_METRICS_EXPORTER` | `otlp` (the default) or `none`, which pushes no metrics. Anything else is refused: for Prometheus, set `telemetry: listen:` |
| `OTEL_TRACES_EXPORTER` | `otlp` (the default) or `none`, which exports no traces, starts no span and makes no key. Anything else is refused |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | Which traces are recorded ([Traces](#traces), Sampling). Default `parentbased_always_on` |
| `OTEL_BSP_SCHEDULE_DELAY` | Milliseconds a finished span waits at most before it is exported. Default 5000 |
| `OTEL_BSP_EXPORT_TIMEOUT` | Milliseconds one export of spans may take, its retries included. Default 30000 |
| `OTEL_BSP_MAX_QUEUE_SIZE` | How many finished spans may wait for export; one more is dropped, and counted. 1 to 65536. Default 2048 |
| `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` | How many spans go in one export, at most the queue's size. Default 512 |
| `OTEL_LOGS_EXPORTER` | `otlp` exports the log lines too ([Logs over OTLP](#logs-over-otlp)); unset, the default, or `none` leaves them in the journal alone. `otlp` with no endpoint, and anything else, is refused |
| `OTEL_BLRP_SCHEDULE_DELAY` | Milliseconds a log record waits at most before it is exported. Default 1000 |
| `OTEL_BLRP_EXPORT_TIMEOUT` | Milliseconds one export of log records may take, its retries included. Default 30000 |
| `OTEL_BLRP_MAX_QUEUE_SIZE` | How many log records may wait for export; one more is dropped, and counted. 1 to 65536. Default 2048 |
| `OTEL_BLRP_MAX_EXPORT_BATCH_SIZE` | How many log records go in one export, at most the queue's size. Default 512 |
| `OTEL_SERVICE_NAME` | The resource's `service.name`. Default `ariesd` |
| `OTEL_RESOURCE_ATTRIBUTES` | More resource attributes, as `name=value,name=value`, at most 64. It may set `service.instance.id`; `service.version` is always the build's |
| `OTEL_SDK_DISABLED` | `true` turns all of it off: no push, no span, no exported log record, no key and no `/metrics`. The listener, if set, still answers health, and stderr still has every log line |

A variable ariesd cannot honor stops it at start, with a `fix:`. The
OpenTelemetry SDK itself also reads the variables for TLS files
(`OTEL_EXPORTER_OTLP_CERTIFICATE` and the client key and certificate),
`OTEL_EXPORTER_OTLP_COMPRESSION` and the limits on one span
(`OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT` and the like, which ariesd's spans
are far below) from ariesd's environment; ariesd does not check
those.

Every export, of metrics, of spans and of log records, carries the resource
`service.name`, `service.version` (the ariesd version) and
`service.instance.id` (a random UUID, new at each start, so one run can
be told from the next).

### The listener: metrics to scrape, and health

```yaml
# /etc/aries/ariesd.yaml
telemetry:
  listen: 127.0.0.1:9464
```

| Path | Answers |
|---|---|
| `GET /metrics` | The instruments below, in Prometheus's text format |
| `GET /healthz` | `200 ok` while the process serves |
| `GET /readyz` | `200 ready`, or `503 not ready: ` and one line saying why |

Any other path is 404, and any method but GET and HEAD is 405.

- **Loopback only.** The address must be a loopback one: `127.0.0.1`,
  `[::1]` or a name such as `localhost` whose every address is loopback.
  `0.0.0.0`, `[::]` and any routable address are refused at start. The
  listener has **no authentication**: any user on the machine can read
  it. [What it reveals](security-model.md#the-telemetry-listener) is in
  the security model. To collect from another machine, run a collector
  on this one.
- **It starts before the database is opened**, so a monitor can watch a
  slow start, such as an upgrade that migrates many projects: `/healthz`
  answers at once, and `/readyz` says `ariesd is starting`.
- **A port that cannot be bound stops ariesd at start**, with a `fix:`.
- **It is bounded.** At most `telemetry_requests` requests are served at
  once ([Limits](#limits)); one more is answered `503` with
  `Retry-After: 1`. Twice that many connections may be open, and one more
  is closed. A request's headers must arrive within 5 seconds and its
  answer be read within 30.

`/readyz` is ready when all of these hold, and otherwise names the first
that does not:

| Line | Means |
|---|---|
| `ariesd is starting` | The store is not open yet, or the socket not yet listening |
| `ariesd is not listening on its socket` | The daemon is stopping, or could no longer accept connections |
| `the last check of Dolt failed` | Dolt did not answer a ping within 2 seconds: it hangs, or cannot be reached. The ping is on a pooled connection to the control database, or to the one project's; it is made at most every 5 seconds, however often `/readyz` is asked, and its answer stands until the next. ariesd's log has the error. A Dolt whose unit stops or restarts takes ariesd down with it ([step 5](#5-run-ariesd-under-systemd)), so then nothing answers on the listener at all until both are back |
| `the last write of the keys file failed` | With `control_db`: sshd may be reading an older list of keys ([Keys from the store](#keys-from-the-store)) |

`/healthz` says only that the process answers. A daemon that is healthy
and not ready is usually waiting for Dolt.

To have Prometheus scrape it:

```yaml
scrape_configs:
  - job_name: ariesd
    static_configs:
      - targets: ["127.0.0.1:9464"]
```

### What is measured

Names are OpenTelemetry's, as OTLP carries them. On `/metrics` the
exporter writes dots as underscores, adds `_seconds` for a unit of
seconds and `_total` to a counter, and gives a histogram `_bucket`,
`_sum` and `_count`.

| Instrument | On `/metrics` | Kind | Attributes |
|---|---|---|---|
| `aries.request.duration` | `aries_request_duration_seconds` | histogram, seconds | `op`, `result` |
| `aries.project.requests` | `aries_project_requests_total` | counter | `project`, `result_class` |
| `aries.connections` | `aries_connections` | up-down counter (a gauge on `/metrics`) | `project` |
| `aries.handshakes` | `aries_handshakes_total` | counter | `result` |
| `aries.connections.dropped` | `aries_connections_dropped_total` | counter | `reason` |
| `aries.limit.refusals` | `aries_limit_refusals_total` | counter | `limit` |
| `aries.store.write.duration` | `aries_store_write_duration_seconds` | histogram, seconds | `project` |
| `aries.store.write.retries` | `aries_store_write_retries_total` | counter | `project` |
| `aries.stores.open` | `aries_stores_open` | gauge | none |
| `aries.store.open.duration` | `aries_store_open_duration_seconds` | histogram, seconds | `migrated` |
| `aries.claims` | `aries_claims_total` | counter | `event`, `reason` |
| `aries.locks` | `aries_locks_total` | counter | `event` |
| `aries.lock.wait.duration` | `aries_lock_wait_duration_seconds` | histogram, seconds | none |
| `aries.control.writes` | `aries_control_writes_total` | counter | `op`, `result` |
| `aries.keys_file.writes` | `aries_keys_file_writes_total` | counter | `result` |
| `aries.keys_file.last_write.time` | `aries_keys_file_last_write_time_seconds` | gauge, Unix seconds | none |
| `aries.reaper.duration` | `aries_reaper_duration_seconds` | histogram, seconds | none |
| `aries.reaper.last_run.time` | `aries_reaper_last_run_time_seconds` | gauge, Unix seconds | none |
| `aries.clock.behind` | `aries_clock_behind_seconds` | gauge, seconds | none |
| `aries.clock.steps` | `aries_clock_steps_total` | counter | `direction` |
| `aries.telemetry.dropped` | `aries_telemetry_dropped_total` | counter | `signal` |

What each counts:

| Instrument | Counts |
|---|---|
| `request.duration` | Each request on a connection, from its frame being read to its answer being ready, refusals and rate-limited requests included |
| `project.requests` | The same requests, by project and by class |
| `connections` | Connections past the handshake |
| `handshakes` | Each handshake, by how it ended; those of `ariesd upgrade`'s health check and `ariesd status` among them. A connection refused before its handshake is not one: a socket past twice `conns`, or a peer that is not the daemon's user |
| `connections.dropped` | Connections ariesd closed for a reason of its own, not those a client closed |
| `limit.refusals` | Refusals at a limit that ariesd can name |
| `store.write.duration` | A write that changed a row and committed: the same measurement as P50 and P99 in the status report, the wait for the project's one writer and any retry included |
| `store.write.retries` | Each attempt of a write that lost to a concurrent transaction and was tried again |
| `stores.open` | Project stores open, opening or closing; always 1 on a server of one project |
| `store.open.duration` | With `control_db`: opening a project's store, migrations included: once for each active project before the daemon listens, which is where an upgrade's migrations run, then whenever a connection or an archive opens one that was closed |
| `claims` | Changes of a claim, as the event log records them, and renewals |
| `locks` | Changes of a [lock](concepts.md#locks)'s holds and queue, as the event log records them, and renewals of a hold or a place. A lock's name is no attribute: it is a person's text |
| `lock.wait.duration` | Each grant to a place in a lock's queue: how long the place waited, on the server's time. The buckets end at 10 seconds, so most waits for a deploy or a merge fall past the last |
| `control.writes` | With `control_db`: each `aries admin` change, refused ones included |
| `keys_file.writes` | With `control_db`: each time the keys file was written, or could not be. A file that already held the right keys is not a write |
| `keys_file.last_write.time` | When the keys file took its current content |
| `reaper.duration`, `reaper.last_run.time` | Each 30-second tick that ends expired claims, closes idle stores and rereads the control database, and when the last one ended. A time that stops advancing means the reaper is stuck |
| `clock.behind` | How far the server's clock is behind the history its time is held against, as of the reaper's last tick; 0 when it is not behind. Above 0 the daemon serves, with its time running on from the history at half speed, and with `clock_behind: stop` it will not start again: alert on it ([The server's clock](#the-servers-clock)) |
| `clock.steps` | Each time the daemon found the clock set, or the machine suspended and resumed, by more than `clock_step_tolerance`, counted at the reaper's next tick |
| `telemetry.dropped` | For `signal="metrics"`, each OTLP export that failed; nothing is lost for good, since totals are cumulative and the next export carries them. For `signal="traces"`, each span that was lost: one the full queue had no room for, or one of an export that failed. For `signal="logs"`, each log record that was not exported, the same way; the journal still has its line ([When the collector is down](#when-the-collector-is-down)) |

Every attribute's values come from a fixed list in the code, so the
number of series cannot grow with what clients send:

| Attribute | Values |
|---|---|
| `op` | An operation of the protocol, such as `create`, `start` or `member.set`; `unknown` for any other |
| `result` | `ok`, or the refusal's code: `not_found`, `conflict`, `exists`, `cycle`, `invalid`, `acceptance`, `forbidden`, `unavailable`, `version`, `auth`, `busy`, `stale`. For a handshake also `failed`: it ended without a refusal to name, as when a frame could not be read. For the keys file: `ok` or `failed` |
| `result_class` | `ok`; `error` for `unavailable`, which is the server's failure; `refused` for every other refusal, which is the request's |
| `project` | The project's UUID, only for a project a connection was admitted to. At most as many as the `projects` limit, and `other` past that |
| `reason` of a dropped connection | `membership`, `disabled`, `archived`, `key` (the access that let it in ended), `idle` (`idle_timeout`), `limit` (a socket past twice `conns`), `access` (the access ended while the handshake ran) |
| `limit` | `conns`, `conns_per_principal`, `handshakes_per_principal`, `handshake_sockets` (twice `conns`), `write_rate`, `control_write_rate`, `open_stores`, `telemetry_requests`, `projects`, `principals`, `keys_per_principal`, `members_per_project`, `prices`, `plans`, `hours_per_day`, `usage_per_day`, `locks`, `lock_waiters`, `lock_waits_per_principal` |
| `migrated` | `true` when opening the store applied a migration |
| `event` of a claim | `take`, `renew`, `release`, `expire` |
| `event` of a lock | `acquire`, `wait`, `grant`, `renew`, `release`, `expire`, `leave` |
| `reason` of a claim | `membership`, `disabled` or `archived` for a release that ended with the holder's access; `none` for every other change |
| `signal` | `metrics`, `traces`, `logs` |
| `direction` | `forward`, `backward` |

- **Nothing a client chooses is an attribute:** no principal, session,
  machine or issue id, and no project the server does not serve.
- **`project` is never beside `op`.** One instrument has the operation
  and its result; separate, coarser ones have the project.
- **`telemetry: per_project: false`** removes `project` from every
  instrument, for a server with many projects. The numbers are then the
  server's as a whole.
- **Histograms share one list of buckets:** 5, 10, 25, 50, 100, 250 and
  500 ms, then 1, 2.5, 5 and 10 s.
- **Limits not counted by name.** A request past one of the other limits
  ([Limits](#limits)), such as `labels_per_issue`, is refused as
  `invalid` like any other invalid request, and shows only under
  `result="invalid"`.
- **The Go runtime's metrics** come with these, under the names of
  OpenTelemetry's semantic conventions: `go.memory.used`,
  `go.memory.allocated`, `go.memory.allocations`, `go.memory.gc.goal`,
  `go.goroutine.count`, `go.processor.limit`, `go.config.gogc`, and
  `go.memory.limit` when `GOMEMLIMIT` is set. At `/metrics` they are
  `go_memory_used_bytes`, `go_goroutine_count` and so on. Garbage
  collection pauses and scheduler latency are not among them.

### Traces

With an endpoint for traces in its environment, ariesd records where
each request's time goes as spans and exports them over OTLP/HTTP.
Without one it starts no span: a request costs nothing for them.

**A trace follows a request from the agent's harness into Aries.**
`aries` sends the W3C trace context it was started with, or the one a
tool call brought ([agent guide](agents.md#trace-context)), and
ariesd's span for the request is a child of the caller's span. A
request that brings none, or one that is not valid, starts a trace of
its own; it is never refused for it. Dolt has no tracing, so a trace
ends at ariesd's span around the work it asked of Dolt.

| Span | Kind | Covers | Attributes |
|---|---|---|---|
| A request's, named for its op: `create`, `show` and so on, and `unknown` for an op the server does not know | server | One request, from its frame being read to its answer being ready: the stretch `aries.request.duration` measures | `op`, `result`, `project`, `protocol`, `principal`, `session` |
| `handshake` | server | A connection's handshake, from its first frame to its welcome, with the opening of the project's store when this connection caused it. The note of its session in the registry is beneath it and may end after it: it is written beside the wait for the first request. The root of a trace of its own, since a hello brings no trace context | `result`, `project`, `protocol`, `principal`, `session` |
| `store.write.attempt` | internal | One attempt of a write: the wait for the writer, the SQL transaction and its commit. A write that loses to a concurrent transaction is attempted again, in a new span, so a retried write shows as several | `project`, `attempt`, `outcome` |
| `store.write.wait` | internal | Within an attempt: the wait for the project's one writer connection, and the start of its transaction | `project`, `outcome` |
| `store.open` | internal | With `control_db`: opening a project's store, as `aries.store.open.duration` measures it | `project`, `migrated` |
| `store.migrate` | internal | Within the opening of a store, a project's or the control database's: the check of its schema, and the migrations applied | `project`, `outcome` |
| `reaper` | internal | One 30-second tick, with the four below and the store's writes beneath it, so a slow tick shows which part was slow | none |
| `clock.check` | internal | The look at the server's clock | none |
| `control.reload` | internal | With `control_db`: the reread of the control database's index | none |
| `keys_file.write` | internal | With `control_db`: the check of the keys file, and its write when it was out of date | `result`, `changed` |
| `access.sweep` | internal | With `control_db`: ending the claims of holders who lost their access, in every open store, or in one as it opens | `project`, when it sweeps one store |

- **The last four are also roots or children elsewhere.** Each is the
  root of a trace when it runs outside a tick, as the keys file's write
  does when ariesd starts; the keys file's write after an `aries admin`
  change is beneath that request's span.
- **A span of the control database's store names no project.**
- **A span is marked failed only for the server's own failures:** a
  request answered `unavailable`; a handshake that ended `failed` or
  `unavailable`; a Dolt commit, a migration, a wait for the writer, an
  opening, a reread or a keys file write that returned an error. Its
  status description is that code and nothing more. A refusal is the
  request's doing: its code is the span's `result`, and the span is not
  failed. An attempt that lost to another transaction has the outcome
  `retry`, and is not failed either.

| Attribute | Holds |
|---|---|
| `op` | The operation, as on the metrics: `unknown` for any the server does not know |
| `result` | `ok`, or the refusal's code; for a handshake also `failed`; for the keys file `ok` or `failed` |
| `project` | The project's UUID; `other` for anything that is not one |
| `protocol` | The protocol version the connection speaks |
| `principal`, `session` | Who, as `telemetry: principals:` allows (People, below). Only a request's and a handshake's span say who |
| `attempt` | A write attempt's number, from 1 |
| `outcome` | `ok`; `retry`, for an attempt that lost to a concurrent transaction; `error` for any other error, which for an attempt is most often the request's own refusal |
| `migrated` | `true` when opening the store applied a migration, `false` when not |
| `changed` | `true` when the keys file was written, `false` when it already held the right keys |

**A span carries ids, codes and small numbers, and nothing else.** No
issue's title or body, no memory's text, no comment, no handoff, no key,
no DSN, no token and no error's message is ever on a span, and a span
has no events and no links. The caller's `tracestate` is not put on
ariesd's spans either: of the caller's trace context they take the
trace id, the parent's span id and the sampled flag. This is enforced
where spans are made, not left to each caller: a name, a key or a value
that is not on its list is dropped or recorded as `unknown` or `other`,
and tests send requests with marked text in every field and read what
is exported.

**People.** A metric never says who did something. A span may, and
`telemetry: principals:` in the config file decides how:

| `principals` | A request's and a handshake's span carry |
|---|---|
| `pseudonym` (default) | `principal`: a stand-in, `p_` and 16 hex digits, the same in every span for the same principal. `session`: one for the session id, `s_` and 16 hex digits |
| `name` | `principal`: the principal's name. `session`: the session id, left out when it is not 1 to 255 printable ASCII characters with no space |
| `omit` | Neither |

- **A stand-in is the start of an HMAC-SHA-256** of the principal's
  name under a key of 256 random bits. ariesd makes the key the first
  time it exports traces with stand-ins, and keeps it in
  `telemetry: principals_key:`, by default
  `/var/lib/aries/telemetry/principals.key`, mode 0600. It reads the
  file at each start. The backend never sees the key or a name.
- **Without the key, spans say nothing about who, and ariesd says
  so.** A key that cannot be made or read never stops ariesd, and it
  never makes one up in memory or sends the name in its place: spans
  carry no principal and no session until the key is there. That holds
  for the default path and for one you set. The log has it at error
  level, with the cause and each fix on a line of its own; the status
  report has `TRACES SAY NOTHING ABOUT WHO` with the cause; `whois`
  answers that the server has no key, and why. ariesd looks for the
  key again every 30 seconds and logs one line when stand-ins begin.
- **A key file that is there and cannot be trusted is said to be exactly
  that.** One that others can read (any mode but 0600 or stricter), or
  that does not hold a key, is not used: someone may have read or
  changed it. The log and the status say `others can read it`, or that
  it `does not hold a key`. Replace it by deleting it, which makes every
  earlier stand-in unlinkable; or, if nobody can have read it, `chmod
  600` it.
- **The status report says how people appear:** `traces: people are
  named by stand-ins`, `by name`, `nothing says who (telemetry:
  principals: omit)`, or the line in capitals above. `--json` has it as
  `traces.people`: `pseudonym`, `name`, `omit` or `unavailable`, with
  `cause`, `key_file` and `damaged`.
- **A server admin can resolve one:** `aries admin principal whois
  p_0123456789abcdef` prints the principal's name
  ([`aries admin principal`](cli.md#agents-and-maintenance)). The server tries its key against
  each of its principals; nobody else may ask, and a session's stand-in
  cannot be resolved. A server of one project has no `aries admin`, so no
  `whois`.
- **Deleting the key file makes every earlier stand-in unlinkable**, to
  you as well: stop ariesd, delete the file, start it. It makes a new
  key, the same person is a new stand-in from then on, and `whois`
  answers that an old stand-in is nobody's. Do it when a retention rule
  asks that old traces no longer lead to a person. The file is in no
  backup Aries makes; if you back it up, old stand-ins stay
  resolvable for as long as a copy exists, and whoever holds a copy can
  test a name against a stand-in.
- **A stand-in hides a name from the backend. It does not make a person
  anonymous.** What they did, when, and in which project is all still
  there, under one stand-in, and on a small team that is often enough
  to tell who it is.
- **The setting does not change ariesd's own log**, which names
  principals and sessions as before ([Logs](#logs)). Whoever can read
  the journal can read the databases; if you ship the journal to a
  third party, filter it in the collector.

**Sampling** is the OpenTelemetry SDK's, set by `OTEL_TRACES_SAMPLER`:

| `OTEL_TRACES_SAMPLER` | Records |
|---|---|
| `parentbased_always_on` (default) | What the caller decided, when the request brought a trace context; every trace that starts in ariesd: a request with no trace context, a handshake, the reaper's tick, a Dolt commit |
| `parentbased_always_off` | Only requests whose caller sampled them |
| `parentbased_traceidratio` | What the caller decided; of the traces that start in ariesd, the share `OTEL_TRACES_SAMPLER_ARG` gives, from `0` to `1` |
| `always_on`, `always_off` | Everything, or nothing, whatever the caller decided |
| `traceidratio` | The share `OTEL_TRACES_SAMPLER_ARG` gives, whatever the caller decided |

Any other sampler is refused at start. A span that is not sampled is
neither recorded nor exported; the request's log line carries its ids
all the same.

**How many spans** to expect:

| Work | Spans |
|---|---|
| A read | 1 |
| A write | 3: the request, one attempt and its wait. 2 more for each retry |
| A handshake | 3; with `control_db`, 12 when it is the one that opens the project's store |
| A Dolt commit | 1, at most one a second for each database being written to |
| The reaper's tick, every 30 seconds | 4 on a server of one project; with `control_db`, 5 and 4 for each open store. 2 more for each store every 10 minutes, when it prunes |

A tracker's traffic is small, which is why the default records
everything: ten requests a second, a third of them writes, is about 17
spans a second, and one more for the Dolt commit. An idle server of 100 open projects sends about 400
spans every 30 seconds for its reaper alone, so on a server with many
open projects, or one where the count matters to the backend's bill,
set `parentbased_traceidratio` and a share: requests whose caller
sampled them are still recorded.

**Log lines carry the ids,** so that a line and a trace can be joined
([Logs](#logs)). With traces on, every line logged for a request has
`trace_id` and `span_id` of ariesd's own span for it, whether or not
the trace is sampled; a handshake's and the reaper's lines have those
of the span they were logged under. With traces off, a request's lines
have the caller's ids, when the request brought a valid trace context,
and none otherwise. A log record exported over OTLP has the same ids as
the record's own trace and span id.

### Logs over OTLP

The journal is the logs' home, and stderr is never turned off. With
`OTEL_LOGS_EXPORTER=otlp` beside an endpoint, ariesd also sends each
line it logs to the collector, as an OTLP log record, at whatever level
`log_level` sets:

```ini
Environment=OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
Environment=OTEL_LOGS_EXPORTER=otlp
```

It is off unless asked for, unlike metrics and traces, because
OpenTelemetry's log exporter for Go is not yet declared stable upstream
(version 0.23), and most servers already ship their journal.

A record leaves the machine, so it carries less than the line does, by
the rule of [Traces](#traces):

| Of the line | The record has |
|---|---|
| `time`, `level` | Its timestamp, severity number and severity text |
| `msg` | Its body. A message is fixed text in ariesd's code, never built from what a client sent |
| `event` | Its event name, and the attribute `event` |
| `trace_id`, `span_id` | Its own trace and span id |
| `project`, `op`, `code`, `req`, `issue`, `fingerprint` | The attribute, when the value has that id's shape: a UUID, an op ariesd knows (any other is `unknown`), a code, 16 hex digits, an issue's id, a key's fingerprint |
| Numbers, booleans, times, durations and the fixed words (`dur_ms`, `audit`, `seq`, `at`, `reason`, `role`, `connections` and the like) | The attribute |
| `principal`, `session`, `by`, `subject`, `admin`, `created_by`, `added_by` | The attribute as `telemetry: principals:` allows, exactly as a span's (People, above): a stand-in by default, the name with `name`, nothing with `omit`, and nothing, never the name, while the key of the stand-ins is unavailable. A session is treated as a principal is |
| `err`, `detail`, `cause`, `fix`, `effect`, `target`, `machine`, `harness`, `name`, `database`, `path`, `url` and every other field | **Nothing.** An error's text can quote what it refused or name a person, so it stays in the journal, under every setting |

- **To read the rest, go to the journal.** A record's `req`, its trace
  id, or its time and `event` find the line, which has the `err`.
- **The audit stream is exported with the rest,** with `audit=true`,
  `seq`, `op`, the `project`, the `fingerprint` and the `role`. Who made
  the change and whom it changed are stand-ins by default, which a
  server admin resolves with `aries admin principal whois`; an audit
  trail that must name people outside the machine needs `principals:
  name`, or the journal.
- **The same key makes the stand-ins** as for spans, so one person is
  one stand-in in a trace and in a log record. Exporting logs makes the
  key, as exporting traces does, and the same directory must exist
  ([Send metrics to a collector on the machine](#send-metrics-to-a-collector-on-the-machine)).
- **What it costs.** A line that is exported takes about 3 µs longer
  to log, on the goroutine that logged it, and nothing when logs are
  not exported (measured on an Apple M4 Max). At `log_level: info` a
  busy server logs a few lines a second; at `debug` it logs one for
  each request, and exports each. Records wait in a queue of
  `OTEL_BLRP_MAX_QUEUE_SIZE`, 2048 by default, and one more is dropped
  and counted, so memory is bounded.
- **ariesd's own line about a failed log export is exported too,**
  when exports work again. It is sampled, once and then every 10
  minutes, so it cannot feed itself.

### When the collector is down

Telemetry never slows or fails a request. Recording a measurement and
ending a span change memory only; exports run on their own goroutines.

- A collector that is down, slow or refusing costs one warning in the log
  for each signal (`telemetry: a metrics export failed`, `telemetry: a
  trace export failed`, `telemetry: a log export failed`; the event
  `telemetry.export_failed` with its `signal`), then one every 10
  minutes while it lasts, with the number left out, and a line when
  exports succeed again.
- **Metrics lose nothing.** ariesd does not retry within an interval:
  the next export carries the same cumulative totals.
  `aries.telemetry.dropped{signal="metrics"}` counts each failed
  export.
- **Spans are lost.** A span that could not be exported is gone. The
  exporter retries a collector that answers 429, 502, 503 or 504, or
  does not answer in time, for as long as `OTEL_BSP_EXPORT_TIMEOUT`
  allows; a connection that is refused fails at once. Meanwhile finished
  spans wait in a queue of `OTEL_BSP_MAX_QUEUE_SIZE`, and one more is
  dropped, so memory is bounded: a full queue of 2048, the default,
  measured about 1.5 MB.
  `aries.telemetry.dropped{signal="traces"}` counts every span lost,
  at the queue or with its export, exactly.
- **Exported log records are lost, and their lines are not.** A record
  that could not be exported is gone from the export; the journal has
  the line. The queue and the retries are as for spans, under the
  `OTEL_BLRP_*` variables, and
  `aries.telemetry.dropped{signal="logs"}` counts every record lost,
  exactly. Logging never waits for the collector.
- Stopping ariesd exports what is waiting and makes one last export of
  metrics, and gives all of it at most 5 seconds together. Spans and
  log records it could not send in that time are counted as dropped, in
  a count that itself may not reach the collector. The lines ariesd
  logs while it stops go to the journal alone.

### A collector, alert rules and a dashboard

[deploy/observability](../deploy/observability) holds four files to
start from. **They are examples that are tested, not a product:** tests
hold every metric, label and value they name to what ariesd has, and
the [Docker test server](deploy/docker.md#telemetry) runs them against a
real ariesd, Dolt, OpenTelemetry Collector and Prometheus. Nothing in
them names a vendor.

| File | Is |
|---|---|
| `collector.yaml` | An OpenTelemetry Collector's configuration: it receives what ariesd pushes on `127.0.0.1:4318` (metrics, traces and, when asked, log records), scrapes ariesd's `/metrics` and Dolt's, and sends all of it on |
| `alerts.yaml` | Alert rules in Prometheus's rule format, which most monitoring systems read or translate |
| `prometheus.yaml` | A Prometheus configuration that scrapes the two endpoints and loads the rules, for a server that runs Prometheus and no collector |
| `dashboard.json` | One dashboard, in Grafana's format: requests, latency, writes, connections, claims, the server's own health, and Dolt's memory and collector |

**Wire a collector.** It runs on the server itself: ariesd's listener
and Dolt's metrics answer on loopback only and have no authentication,
and ariesd pushes to loopback with no credential.

1. Install the OpenTelemetry Collector as its own documentation says.
   The core distribution has every component the file uses; it was
   tested at 0.162.0.
2. Turn on what it reads, if it is not on already: `metrics:` in Dolt's
   config ([step 2](#2-run-dolt-under-its-own-account)),
   `telemetry: listen: 127.0.0.1:9464` in ariesd's
   ([The listener](#the-listener-metrics-to-scrape-and-health)) and
   `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318` in its unit
   ([above](#send-metrics-to-a-collector-on-the-machine)).
3. Use `collector.yaml` as the collector's configuration, with its
   `debug` exporter, which only prints, replaced by your backend's. For
   one that takes OTLP over HTTP, as most do:

   ```yaml
   exporters:
     otlp_http/backend:
       endpoint: https://otlp.example.com
       headers:
         Authorization: Bearer ${env:BACKEND_TOKEN}
   ```

   Name `otlp_http/backend` in place of `debug` in each pipeline. The
   token is a secret: give it to the collector from a file of mode 0600
   that its unit reads (`EnvironmentFile=`), never in the configuration
   file or on a command line.
4. **Keep one path for ariesd's metrics.** The file receives them
   twice, pushed and scraped, because the test server checks both. On a
   real server take `prometheus/ariesd` out of the `metrics` pipeline,
   or set `OTEL_METRICS_EXPORTER=none` for ariesd and keep the scrape.
   Dolt's metrics can only be scraped.
5. **Logs, if you want them there.** The file has a `logs` pipeline,
   which carries nothing until ariesd is asked to export its log
   (`OTEL_LOGS_EXPORTER=otlp`, [Logs over OTLP](#logs-over-otlp)). The
   journal stays the log's home and has every field; a record has the
   line's ids, codes and numbers and none of its text. To send
   [the audit stream](#the-audit-stream) somewhere and nothing else of
   the log, give it a logs pipeline of its own with the file's
   `filter/audit`, which drops every record that is not the stream's:

   ```yaml
   processors:
     filter/audit:
       error_mode: ignore
       logs:
         log_record:
           - attributes["audit"] != true
   ```

   The records name who made a change and whom it changed by stand-ins,
   unless `telemetry: principals: name` says otherwise. **To ship the
   journal itself to a third party,** which has the names and the
   errors' text, filter it where it is read, on `audit=true` or on
   `event`: ariesd does not send the journal anywhere.

Pushed metrics carry their OpenTelemetry names (`aries.request.duration`)
and scraped ones the names on `/metrics`
(`aries_request_duration_seconds`); [What is measured](#what-is-measured)
lists both. The rules and the dashboard use the second, which is also
what most backends store the first as.

**Load the rules.** With Prometheus, name the file under `rule_files:`,
as `prometheus.yaml` does; other systems import the same format. The
rules select by the scrape jobs `ariesd` and `dolt`, so keep those
names or change both. Their thresholds are starting points to set
against your server's own history.

| Alert | Fires when |
|---|---|
| `AriesdDown`, `DoltDown` | The endpoint has not answered a scrape for 2 minutes |
| `AriesReaperStalled` | The reaper has not finished a tick for 2 minutes: expired claims are not ended, and systemd's watchdog is about to restart ariesd |
| `AriesKeysFileWriteFailing` | A write of the keys file failed: sshd reads an older list of keys ([Keys from the store](#keys-from-the-store)) |
| `AriesClockBehind`, `AriesClockStepped` | The clock is behind Aries's own history, or was set ([The server's clock](#the-servers-clock)) |
| `AriesProjectUnavailable`, `AriesRequestsFailing` | Handshakes or requests are answered `unavailable`: the server's failure, most often a database that cannot be reached |
| `AriesWriteLatencyHigh`, `AriesWritesQueueing` | One write in a hundred takes over a second; more than one in five is retried |
| `AriesWritesRateLimited`, `AriesConnectionCapReached`, `AriesStoreCapReached` | Requests are refused at `write_rate`, at a connection limit, or at `open_stores` ([Limits](#limits)) |
| `AriesTelemetryMetricsExportFailing`, `AriesTelemetrySpansDropped`, `AriesTelemetryLogRecordsDropped` | ariesd counts failed pushes of metrics, spans it lost, or log records it lost from the export, whose lines the journal still has ([When the collector is down](#when-the-collector-is-down)) |
| `DoltCollectorPastTheCliff`, `DoltCollectorBusy` | With a memory limit set, Dolt's garbage collector ran more than 14 times for each 100 writes, or 3 or more: the thresholds of [Dolt's memory](#dolts-memory) and of the status report |
| `DoltMemoryOverItsLimit`, `DoltMachineMemoryHigh`, `DoltDiskFilling` | Dolt's resident memory is over its `GOMEMLIMIT`; the machine's memory is over 90% in use, or its disk over 85%, as Dolt reads them |

- **No rule asks `/readyz`,** because no metric says what it answers.
  Probe `http://127.0.0.1:9464/readyz` from the machine with whatever
  probes HTTP for you. `AriesdDown`, `AriesReaperStalled`,
  `AriesKeysFileWriteFailing` and `DoltDown` cover the same ground
  from the metrics.
- **No rule matches a log line's `event`.** These are rules over
  metrics, and nothing in the examples turns a log record into one.
  Match [`event`](#events) where your logs are read. Three of the events
  worth an alert have a rule here from the metric that counts the same
  thing: `keys_file.write_failed`, `project.open_failed` at a handshake
  (`AriesProjectUnavailable`) and `telemetry.export_failed`.
- **No metric gives the wait for a project's writer alone.** A write's
  latency includes it, and a trace shows it, in each attempt's
  `store.write.wait` span.
- **A counter has no series until it first counts,** and Prometheus sees
  no increase in a series' first sample. The rules on counters that
  should stay at nothing therefore also fire on a series that is new
  and already above zero.
- **For several ariesd servers in one Prometheus,** add `by (instance)`
  to the rules' sums.

**The dashboard** is imported into Grafana as it is and asks which
Prometheus data source to read. No test loads it into Grafana: tests
check its queries, as they do the rules', and the test server runs each
query against a real Prometheus.

**What was run.** All of it in the Docker test server, on Docker
Desktop on a Mac (arm64), on 12 October 2026: the Collector at 0.162.0
and Prometheus at 3.15.0, with every rule loaded and evaluated, every
query of the dashboard run, and ariesd's metrics, traces and log
records received, the audit stream among them. The `filter/audit`
example and the backend's exporter were only checked by the Collector's
`validate`: no record was sent through either. Not run: the Collector as a systemd unit on
a server, beside the [AppArmor profiles](#confine-ariesd-with-apparmor),
which confine ariesd and not a collector; a hosted backend; and any
alert against days of real traffic.

### ariesd status

```sh
sudo -u aries ariesd status
sudo -u aries ariesd status --json
```

It prints [the status report](#watching-a-server) that `aries admin status`
gives a server admin, with every project, for a script or an agent on
the server that has no admin key. `--json` prints exactly one JSON
document: the report, or `{"error": …}` and exit code 1.

- It asks the running daemon over its unix socket, so it works only for
  **the user ariesd runs as**: the config file and the socket's
  directory are that user's alone, and on Linux the daemon also checks
  that whoever connects runs as its own user. Anyone else gets a refusal
  with a `fix:`. So does root where the
  [AppArmor profiles](#confine-ariesd-with-apparmor) are loaded: run
  it with `sudo -u aries -H`, as above. It needs no `aa-exec`.
- It must be the same build as the daemon, since it speaks the daemon's
  protocol: one of another protocol version is refused with `version`.
- It connects under the reserved principal `ariesd-status`, which no
  key can sign in as. The connection is in no project, is not listed by
  `aries who`, and is answered nothing but the report.
- On a server of one project it lists that project by its UUID, since
  only a control database gives projects names, and counts no projects
  or principals.
- It answers in about 2 seconds when Dolt does not answer at all, with
  each open project's sessions as `unknown`
  ([When Dolt does not answer](#when-dolt-does-not-answer)), whichever
  stores are open: `aries admin status` is as prompt only from a
  repository whose project's store is open, and is refused from any
  other. It waits 30
  seconds for the daemon and no longer: `the daemon did not answer`
  then means ariesd itself is stuck, which its log explains.

## Upgrades

**From v0.4.x or earlier, there is no upgrade in place.** v1.0.0 has
new names for every binary, file, variable and path, a new release
key, and a new database, and its server refuses every earlier client.
A v0.4.x `ariesd upgrade` cannot find or verify it. Set up a new server
and remove the former one, as [Upgrading to 1.0](upgrade-to-1.0.md)
says. What follows is for upgrades between releases from v1.0.0 on.

**The protocol.** Each release's notes say whether it changes the
protocol. One that does still accepts the version before it (design
principle 9), so the server is upgraded first and the clients after, at
their own pace:

1. **Upgrade the server,** as below. The health check of the old
   binary that runs the upgrade is answered by the new ariesd whatever
   protocol the old binary speaks.
2. **Everyone upgrades `aries`:** `aries upgrade`, on each machine. It
   reads the release from GitHub, or a mirror, and asks the server
   nothing, so it works while the server refuses that `aries`.
3. **Everyone restarts their agents.** The MCP server in an agent's
   harness is an `aries mcp` process, started from the old binary, and
   it stays the old one until the harness starts it again.

What each side sees when its versions are outside the other's range,
with exit code 3:

| Who | Is told |
|---|---|
| An `aries` older than the server accepts | The server's message, naming both versions, and `fix: upgrade this client: run `aries upgrade`, or install the latest release (docs/install.md), then restart any agent that runs `aries mcp``. The old `aries` prints the server's words, so it says this whatever its version |
| An agent on such an `aries mcp` | Every tool call fails with code `version` and that message, and the fix `tell the user aries cannot connect, quoting the server:` followed by the line above. The session's start says the same in one line. The agent cannot fix it: the person upgrades `aries` and restarts the agent |
| An `aries` newer than the server | A message that the server is older, and `fix: ask the server admin to run `ariesd upgrade`, or install the aries release that matches the server` |

On the server, as the `aries` user:

```sh
cd /var/lib/aries
sudo -u aries -H ariesd upgrade --check
sudo -u aries -H ariesd upgrade
```

`ariesd upgrade` does this, in order:

1. Downloads the release and checks its signature and checksum.
2. Checks the clock against the server's history, as `serve` does at
   start. If the restarted daemon would refuse to start for it
   (`clock_behind: stop`), the upgrade stops here with that refusal and
   changes nothing: otherwise the daemon's refusal would fail the health
   check of step 5 and be taken for a bad release. `upgrade --check`
   prints the same finding on a line starting `clock:`.
3. Commits the database and tags it `aries-<old version>`, a point to go
   back to. With `control_db`, it does so in the control database and in
   every project's database, and stops, changing nothing, if one cannot
   be reached.
4. Swaps the binary.
5. With `systemd_unit:` set, or `--restart`, restarts the unit through the
   sudo rule from [step 5](#5-run-ariesd-under-systemd). It then checks
   that the new version answers on the socket within 30 seconds. If it does
   not, upgrade puts the old binary back and restarts it. The check is
   made by the binary that ran the upgrade, the old one, and ariesd
   answers it whatever protocol that binary speaks, so an upgrade across
   a protocol change is not mistaken for a failed one.

   sudo compares the whole command with its rule, so the rule must spell
   the unit as `systemd_unit` does: `ariesd.service` in both, as steps
   4 and 5 write them. `--restart` with no `systemd_unit` asks for
   `systemctl restart ariesd`, which that rule does not cover: sudo
   answers `a password is required`, nothing is restarted, and upgrade
   reports the daemon as not healthy (seen with `--rollback --restart`).

With `control_db`, ariesd opens the database of every active project
once before it listens, which applies the new version's migrations to
each (a few milliseconds for a database already current), and the check
answers for all of them: it does not depend on the config file's
`project` being active. If a project's database cannot be opened, the
check is refused and names the project, so upgrade puts the old binary
back; the log says why. An archived project's database is migrated when
the project is activated, and not at start: ariesd opens it then only
as it is, to end a claim left in it
([Managing access](#managing-access)).

The same happens at every start. A project whose database cannot be
opened does not stop the others from being served: it is logged at
`error`, its connections are refused with `unavailable`, and each new
connection tries it again. Fix its database, or archive the project.
As on a server of one project, the old binary refuses a database the new
one has already migrated, since migrations only go forward. So after an
upgrade that failed partway, the restored version may not serve the
databases the new one reached; the tag of step 3 marks each as it was.

**Keys and the upgrade.** sshd runs nothing of Aries's to find a key
([Keys from the store](#keys-from-the-store)), so an upgrade has no
copy of the binary to refresh for it. The new ariesd writes the keys
file again as it starts.

Where the [AppArmor profiles](#confine-ariesd-with-apparmor) are
installed, run each of these through `aa-exec -p ariesd-admin --`.

It refuses to run as root, except with `--check`. `--to vX.Y.Z` installs a given release, but
never an older one, because migrations only go forward. `--rollback`
restores the binary the last upgrade replaced. Take a
[dump](#backups-and-restore) first if you want a copy outside Dolt.

**Where the release is read from.** GitHub, unless the server is told
otherwise, in one of two ways. Both take what `aries upgrade` and
`install.sh` take ([Where releases are read
from](install.md#where-releases-are-read-from)): the `https://` URL of a
static tree, or the absolute path of a directory that holds one.

| Set | Where | Note |
|---|---|---|
| `releases:` | The config file | The usual way on a server |
| `ARIES_RELEASES` | The environment of `ariesd upgrade` | Wins over `releases:`. `sudo` does not pass your shell's variables on, so write `sudo -u aries -H env ARIES_RELEASES=… ariesd upgrade` |

```yaml
releases: https://releases.example.com/aries
```

A value that is neither is refused before anything is asked of anyone,
and so is an `http://` URL to anything but `127.0.0.1` or `[::1]`. The
daemon never reads the setting: only `upgrade` does, and `upgrade
--check` is how to try it.

**A server with no way out.** Put the release on the server and name
the directory. Carry in `ariesd_<version>_linux_<arch>.tar.gz`,
`checksums.txt` and `checksums.txt.sig`, as the `aries` user can read
them:

```sh
sudo install -d -o aries -g aries /var/lib/aries-releases/v0.6.0
sudo install -o aries -g aries -m 0644 ariesd_0.6.0_linux_amd64.tar.gz checksums.txt checksums.txt.sig \
  /var/lib/aries-releases/v0.6.0/
sudo -u aries -H env ARIES_RELEASES=/var/lib/aries-releases ariesd upgrade --to v0.6.0
```

`--to` needs no `index.json`; an upgrade to the latest release reads
one, which `internal/tools/releaseindex` writes
([RELEASING.md](../RELEASING.md#publishing-to-a-static-host)). The
directory is read as it is and nothing in it is trusted: the signature
on `checksums.txt` is checked against the release key built into the
running ariesd, and the archive against `checksums.txt`, exactly as
for a download. A link in the directory that leads out of it is not
followed.

Under the [AppArmor profiles](#confine-ariesd-with-apparmor), an
`https://` mirror needs nothing new: the operator's profile already
reaches any host. A directory does, since that profile reads nothing
outside what it lists: add `/var/lib/aries-releases/{,**} r,` to
`/etc/apparmor.d/local/ariesd-admin` and load the profiles again.
That rule has not been run on a confined server.

A mirror, like GitHub, can hold a release back: one that goes on
serving an old `index.json` keeps `upgrade --check` saying `up to
date`. It cannot make the server install anything the release key did
not sign, nor a release older than the one running.

Each person runs `aries upgrade` on their own machine. It never runs by
itself ([Install](install.md#upgrade-and-roll-back)).

Keep Dolt at the release Aries names, and change it only with an Aries
release that does.

## Rotating the host key

Every `.aries.yaml` pins the server's ED25519 host key, and `aries`
refuses a server that presents another. To change the key without
refusing every client at once, list the new key beside the old before
sshd presents it, and remove the old one after. `aries` connects when the
server presents any key the file pins (`server.host_keys`, [Connect a
repository](cli.md#the-host-key-is-pinned)). Nothing changes on
ariesd's side: the host key is sshd's, not part of Aries's protocol.

1. **Make the new key** on the server, beside the one in use, which
   sshd keeps presenting:

   ```sh
   ssh-keygen -q -t ed25519 -N '' -C '' -f /etc/ssh/ssh_host_ed25519_key.next
   ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.next.pub
   ```

   The second command prints the new fingerprint.

2. **Pin both** in every repository whose `.aries.yaml` names this
   server, one for each project, and commit the change:

   ```yaml
   server:
     host: aries.example.com
     host_keys:
       - SHA256:…   # the key sshd presents now
       - SHA256:…   # the new key, from step 1
   ```

   `host_keys` replaces `host_key`; a file with both is refused. An
   `aries` older than the list refuses the whole file, so everyone runs
   `aries upgrade` first.

3. **Wait for clients to pick it up.** Everyone pulls the commit, and
   each machine runs one `aries` command that reaches the server, which
   is when its outbox, the writes it keeps while the server is away,
   becomes the new key's too ([When the server's host key
   changes](cli.md#when-the-server-cannot-be-reached)). Nothing on the
   server says which clients have done so; how long to wait is yours to
   choose.

4. **Switch sshd to the new key**, keeping the old file for now:

   ```sh
   cd /etc/ssh
   mv ssh_host_ed25519_key ssh_host_ed25519_key.old
   mv ssh_host_ed25519_key.pub ssh_host_ed25519_key.old.pub
   mv ssh_host_ed25519_key.next ssh_host_ed25519_key
   mv ssh_host_ed25519_key.next.pub ssh_host_ed25519_key.pub
   sshd -t && systemctl try-reload-or-restart ssh
   ssh-keyscan -t ed25519 localhost | ssh-keygen -lf -
   ```

   The last command prints the new fingerprint. A checkout that pins
   both keys connects as before; one that still pins the old key alone
   is refused, `host key mismatch`, naming the key the server presented,
   until it pulls step 2's commit.

5. **Remove the old key** from every `.aries.yaml`, back to
   `host_key: SHA256:…` with the new fingerprint, and commit. Once no
   file pins it, delete `ssh_host_ed25519_key.old*` and take a new
   backup of the host key ([Backups and restore](#backups-and-restore)).

`aries admin project add` prints the new project's `.aries.yaml` with
the keys the admin's own file pins: `host_key` for one, `host_keys` for
several. A project added during a rotation therefore pins both, and
needs step 5 too.

A machine that ran no command while both keys were pinned treats the
server as a new one once the old key is removed, as for a server
rebuilt with a new key: what it had queued under the old key waits
there, never sent by itself, and `aries outbox` lists it for its owner to
move or drop.

## Backups and restore

Back up three things:

| What | Holds | How |
|---|---|---|
| The database | Every issue, comment and event | A daily `dolt dump`, below. With `control_db`: one dump for the control database and one for each project's, each from its own directory under `/var/lib/dolt/data` |
| `/etc/aries/ariesd.yaml` | The project UUID and Dolt's password | With your other secrets |
| `~aries/.ssh/authorized_keys` and `/etc/ssh/ssh_host_ed25519_key*` | Who may connect, and the host key every `.aries.yaml` pins. With `control_db`, the control database's dump also holds principals, their keys and their memberships | With your other secrets |

A dump writes the database's current rows, the event log included, as SQL.
It runs while ariesd serves. This cron entry keeps a week of dumps,
one file per weekday. Save it as `/etc/cron.d/aries-backup`:

```text
15 3 * * * root cd /var/lib/dolt/data/aries && sudo -u dolt -H dolt dump -f -r sql -fn /var/backups/aries/aries-$(date -u +\%a).sql
```

```sh
install -d -o dolt -g dolt -m 0700 /var/backups/aries
```

Copy the files off the machine every day: to object storage with
versioning, or with your backup tool. A dump does not hold Dolt's own
commit history. Disk snapshots do, so take both if you can.

**To restore**, stop ariesd: a dump recreates the `aries` database it
came from, dropping each table first.

```sh
systemctl stop ariesd
cd /var/lib/dolt/data
sudo -u dolt -H dolt sql -q "DROP DATABASE aries"
sudo -u dolt -H dolt sql < /var/backups/aries/aries-Mon.sql
systemctl start ariesd
```

After a restore ariesd compares the clock with the restored
history, like at every start. A dump from this server's own past is
behind the clock and starts as usual. One written by a machine whose
clock ran ahead is refused, with the flag that starts it once
([When the clock is behind the server's
history](#when-the-clock-is-behind-the-servers-history)).

On a new machine, follow steps 1 to 6 first. Then put back the config
file's `project`, the host keys and `authorized_keys`, and restore the
dump. If the host key changed, everyone must update `host_key`. `aries`
refuses the new key until they do.

## Storage growth

The event log is never pruned. It is the history, and Dolt keeps every
version of it. An event keeps a text over 8 KiB as its first 512 bytes,
its length and its SHA-256, so an edit loop over long fields grows the log
by about a kilobyte a write, not by the text. The write rate limit bounds
how fast one principal can grow it.

Token usage is one row per harness request in `token_usage`, about 200
bytes each, also never pruned; a `usage` call adds one summary event, not
one per row. `usage_per_day` bounds how fast one principal can grow it.
Prices are at most `prices` rows and plans at most `plans` rows, and
only admins add them. Hours entries are one row each in `hours`, about
100 bytes plus the note, never pruned; `hours_per_day` bounds how many
one principal may log for a day, and an undo deletes the row, its event
keeping the history.

An issue's paths are at most `paths_per_issue` rows in `issue_paths`. A
call that adds paths records one `issue.paths` event listing at most 20;
one that only sees paths already recorded refreshes their time and
records nothing. A request carries at most 200 paths for an issue, and a
`renew` paths for at most 50 issues, whatever `paths_per_issue` is;
past either it is refused with `invalid`.

Watch the size of `/var/lib/dolt/data`. If it grows, run `dolt gc` in a
quiet hour: `cd /var/lib/dolt/data/aries && sudo -u dolt -H dolt gc`.
It grows faster than the rows suggest: in
[the measurements](https://github.com/ariesworx/aries/blob/fbbbf4655665845db81a72d2bbd1845472fd1c5a/spike/multidb/RESULTS.md) Dolt's files took 28 to
56 KiB for each write, 6.5 GiB for a project of 30,000 small issues, and
its garbage collection made them 7 to 47 times smaller. It is also how
to get Dolt's memory back ([Dolt's memory](#dolts-memory)).

## Dolt's memory

Dolt, not ariesd, is what the machine's memory goes to. These figures
are from [one set of measurements](https://github.com/ariesworx/aries/blob/fbbbf4655665845db81a72d2bbd1845472fd1c5a/spike/multidb/RESULTS.md): Linux
on arm64, in a container on a laptop, with synthetic issues of one shape
written as fast as a writer can. Take the shape from them, and check the
sizes on your own server.

- **It grows as Dolt writes, not with what it holds.** An empty Dolt
  took about 80 MiB. With no restart, one project took 300 MiB after
  300 issues, 1.1 GiB after 3,000, 1.4 GiB after 10,000 and 2.1 GiB
  after 30,000. Each further project of 300 issues that was written to
  added 95 to 180 MiB, up to 5.1 GiB for fifty. A project that is only
  opened and read costs far less: about 15 MiB each, for twenty of them
  after a restart.
- **A restart gives most of it back.** Restarted on the same data, Dolt
  took 14 to 42% of what it had before, and a fifth to a half once
  every project was open, read and written to again. `systemctl restart
  dolt` restarts ariesd with it, since ariesd's unit requires
  Dolt's, so connections drop: pick a quiet hour.
- **`dolt gc` and then a restart give back nearly all of it:** 110 to
  200 MiB at rest in every case measured, up to the project of 30,000
  issues, which a restart alone left at 895 MiB. The collection took
  from under a second to 13 seconds a project. It was measured as
  `CALL DOLT_GC()` in each database, as root, with ariesd's
  connections closed; stop ariesd first if you want to match that.
- **So size the machine for what is written between restarts.** 2 GB
  holds one project of a few thousand issues. Give it 4 GB for a
  project past about 10,000 issues or for ten or more projects in
  regular use, or restart Dolt on a schedule, weekly say, and after a
  bulk import.
- **Read Dolt's own figure.** The `Dolt` line of
  [`aries admin status`](#dolts-figures) gives it as `MiB resident`, with
  how long Dolt has been up; `ps -o rss= -C dolt` and
  `process_resident_memory_bytes` in Dolt's metrics are the same number.
  A container's or a
  unit's total also counts the page cache of Dolt's files, which the
  kernel drops when it must: one measured Dolt of 2.1 GiB sat in a
  container showing 5.7 GiB.

**A Go memory limit** is optional. `Environment=GOMEMLIMIT=1500MiB` in
`dolt.service` makes Dolt collect its garbage sooner. It is a target,
not a cap: Dolt went past it in every run where it was too low, and
nothing was refused or killed.

- **Set it from the running server:** no lower than 0.6 of the memory
  Dolt reaches without a limit by the time you would restart it (its
  resident memory, or `go_memstats_next_gc_bytes` from its metrics), and
  0.75 for room. At 0.6 Dolt used up to twice the CPU for the same
  writes and kept to about 60% of the memory.
- **Too low is expensive.** At 0.5, Dolt's CPU for the same writes went
  up 1.5 to 25 times, and at 0.2, 100 times: 7 to 8 of the machine's
  16 cores, the collector running without pause, with writes still
  served, more slowly. The step from costing nothing to costing nine times was
  between 0.59 and 0.52.
- **It moves.** What Dolt holds grows as it writes, so a limit with room
  after a restart is too low later. Watch Dolt's CPU, and its collector
  against ariesd's writes.
- **Not measured:** a limit together with systemd's `MemoryMax`, which
  is the way to a hard cap, and a limit on a server that mostly reads.

**The line to read** is `Dolt's collector:` in
[`aries admin status`](#dolts-figures), which sets the cycles of Dolt's
garbage collector against ariesd's writes over the last ten minutes:

```text
Dolt 2.4.2, up 12d 3h, 9 connections, 891 MiB resident, memory limit 850 MiB
Dolt's collector: 1762 cycles for 12000 writes in 10m, 15 for each 100 writes: OVER 14, AS WITH A MEMORY LIMIT TOO LOW in the measurements: Dolt is spending its CPU collecting. Raise GOMEMLIMIT or restart Dolt (docs/server.md, Dolt's memory)
```

| For each 100 writes | In the measurements | Do |
|---|---|---|
| Under 3 | Every run without a limit, and every run whose limit cost at most twice the CPU | Nothing |
| 3 to 14 | No run fell here: the step from one to the other was narrow | Watch it and Dolt's CPU. If it climbs, act as below before it is past 14 |
| Over 14 | Every run past the cliff: 9 to about 100 times the CPU for the same writes, with the writes still served, more slowly | Raise the limit, or restart Dolt, or run `dolt gc` and then restart it |

- **Raise the limit** (`Environment=GOMEMLIMIT=…` in `dolt.service`,
  then `systemctl daemon-reload` and `systemctl restart dolt`) when the
  machine has the memory: to no less than 0.6 of what Dolt reaches
  without a limit, as above. If you never measured that, raise it by
  half, and read the line again once it shows ten minutes of writes;
  the limits past the cliff were half of that figure or less. The
  restart it takes also empties Dolt, so the line improves either way;
  the higher limit is what keeps it from coming back as soon.
- **Restart Dolt** (`systemctl restart dolt`) when it does not: Dolt
  then holds a fifth to a half of what it did, and the same limit has
  room again for a while.
- **`dolt gc` and then a restart** when a restart alone does not bring
  `MiB resident` far enough below the limit
  ([Storage growth](#storage-growth) has the command).
- **Resident memory against the limit is a weaker sign.** Dolt was up to
  4% over its limit in runs that cost little, and from 4.8% over in runs
  that cost nine times the CPU or more, and one run 8% over cost only
  1.5 times. Read the collector line.
- **What the thresholds rest on:** one machine (Linux on arm64, in a
  container on a laptop), one shape of load (small synthetic issues,
  written as fast as one writer can, into 5, 10 and 20 projects), with
  Dolt 2.4.2. No run mixed reads with writes, and none wrote at the few
  writes a second of a real server. So the line judges only a window
  with at least 500 writes, since an idle Dolt collects about every two
  minutes whatever is written: on a quiet server it says `too few
  writes to judge the memory limit by`, and Dolt's CPU
  (`process_cpu_seconds_total` in its metrics, or `top`) is then what
  to watch.
- **What the line showed on this guide's machine**
  ([What was run, and where](#what-was-run-and-where)), with fourteen
  small projects and a burst of writes at about twenty a second: with no
  limit, 4 cycles for 342 writes, and with a limit of 2 GiB, 7 for 562,
  which is 1.2 for each 100 both times, at 4.2 ms of Dolt's CPU a write;
  with a limit of 120 MiB, under half of the 265 MiB Dolt held at rest,
  3441 cycles for 562 writes, 612 for each 100, at 55 ms of CPU a write,
  and a median write of 20 ms where it had been 5 to 8 ms. Under that
  limit Dolt also made 500 cycles in 15 seconds with nothing written,
  which is why the line judges a window of few writes when its cycles
  are past what 500 writes would excuse.
- **Without a limit** the line gives the figures and no verdict: there
  is no limit to be too low, and every run without one made fewer than
  3 for each 100 writes.

## ariesd commands

| Command | Does |
|---|---|
| `serve` | Run the daemon. Flags: `--config`, `--dsn`, `--socket`, `--project`, `--prefix`, `--log-level`, `--log-format`, `--dev`, `--allow-unsafe-dolt`, and `--accept-clock-behind TIME`, which starts once although the clock is behind a history no later than TIME ([The server's clock](#the-servers-clock)). Exits 75 when it refuses to start for the clock, and 1 for any other failure |
| `stdio --principal NAME [--key FINGERPRINT]` | The sshd forced command: bridge one SSH session to the daemon. `--key` is written by ariesd, into each line of its keys file, never by hand: it names the key that signed in, so ariesd refuses the session unless the control database holds that key for NAME, and closes it when the key is removed |
| `control init --name NAME [--authorized-keys FILE]` | Once, after setting `control_db`: adopt the server's database as the first project and import principals and keys from `authorized_keys` ([Serving many projects](#serving-many-projects)). Safe to run again |
| `import-bd [--project UUID] [--principal NAME] [--dry-run] [--json] FILE` | Import a bd export (`-` for stdin); see [Moving from bd](migrate-from-bd.md). `--project` picks a project on a server of many |
| `export-bd [--project UUID] [-o FILE]` | Write the store in bd's JSONL format |
| `upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart]` | Replace the binary with a verified release ([Upgrades](#upgrades)) |
| `status [--json] [--config FILE] [--socket PATH]` | Print the running daemon's [status report](#ariesd-status), as the user ariesd runs as |
| `version` | Print the version |

## Troubleshooting

| What you see | Cause | Fix |
|---|---|---|
| `aries: host key mismatch` | The server's key is not the pinned one | Check the new key with the admin, out of band, before you change `host_key` |
| `aries: … accepted no key: aries offered SHA256:…, the one key ARIES_KEY_FILE names` | A key is configured, so `aries` offered it alone, and the server does not know it | Have an admin add that key's `.pub`, or unset the variable to offer your default identities and ssh-agent's keys |
| `aries: … accepted none of the N keys aries offered: …` | No key is configured, and the server knows none of your default identities or ssh-agent's keys. The refusal lists each by fingerprint and file | Send the admin the `.pub` of the key you mean, or name your key file with `ARIES_KEY_FILE` |
| The same, ending `… more keys that ssh-agent holds were not offered` | `aries` offers five keys at most, since sshd hangs up at its sixth refusal, and ssh-agent holds more | Name your key file with `ARIES_KEY_FILE`: it is then the only key offered |
| The same, ending `skipped ~/.ssh/id_ed25519: it is passphrase-protected, and ssh-agent does not hold its key` | A default identity needs a passphrase, which `aries` never asks for, or is on a hardware token (`_sk`), and ssh-agent does not hold it | `ssh-add` the key |
| `aries: no SSH key available` | No key is configured, `~/.ssh` has no usable default identity and ssh-agent holds none | Set `ARIES_KEY_FILE` to your key file's path, keep the key under a default name, or `ssh-add` it |
| `aries: ARIES_KEY_FILE does not hold the path of a key file` | The variable holds something that is plainly a key, or is too long to be a path. `aries` does not repeat it | Put the key in a file that only you can read and set the variable to the file's path ([CI](deploy/team.md)) |
| A refusal that ends `ARIES_KEY is set, and aries does not read it` | Sign-in failed, and the environment holds a variable of the old name, whose value `aries` never looks at | Set `ARIES_KEY_FILE` to the key file's path, and unset `ARIES_KEY` |
| `aries: --key-file is only for your own files` | `aries setup --key-file` without `--global`: a project's files are committed | Add `--global`, or set `ARIES_KEY_FILE` in the environment the agent starts in ([agent guide](agents.md#give-an-agent-its-own-identity)) |
| `aries: … accepted no key`, though the key line is right | The `aries` account is locked (`!` in `/etc/shadow`), or its shell is `nologin` | `usermod -p '*' aries`; `usermod -s /bin/sh aries` |
| `aries: ariesd is not running on the server` | The daemon is stopped, or failed at start | `journalctl -u ariesd` shows why; `systemctl start ariesd` |
| `aries: timed out after 15s waiting for … to open an ssh session`, `to start ariesd` or `to answer the hello` | The server accepted the key and then did not answer within the 15 seconds `aries` gives a connection: sshd, or something on the path to it, went quiet (the first two), or `ariesd stdio` or the daemon did not answer (the last) | Run it again. If it persists: `journalctl -u ssh` for the first two, and `ariesd status` and `journalctl -u ariesd` for the last |
| `aries: HOST:PORT closed the connection before the ssh handshake (…)`, often for everyone behind one address at once; from an older `aries`, `ssh handshake with HOST:PORT failed: ssh: handshake failed: EOF` | The server accepted the TCP connection and hung up before the handshake. Usually sshd turned the address away for a while: on OpenSSH 9.8 and later, Ubuntu 26.04's among them, after failed logins or connections that ended before logging in from it, such as a removed key or a pin of an old host key retried ([step 6](#6-let-the-first-principal-in)). Also sshd past `MaxStartups`, or a firewall or proxy in front of it | Wait a minute and try once, not in a loop: each attempt can lengthen the penalty. Check that `aries whoami` signs in with the key the server knows. A penalty ends within 10 minutes. On the server, `journalctl -u ssh -g 'penalty|MaxStartups'` names the address and the cause; stop the client that fails, and list shared addresses in `PerSourcePenaltyExemptList`. Writes made meanwhile are queued, and `aries mcp` waits 30 seconds before it dials again |
| `aries: open ssh session: the server closed the connection once it had accepted the key` | sshd took the key and hung up before a session was open: it was restarting, or something between the two machines cut the connection | Run it again. If it persists, `journalctl -u ssh` on the server |
| `project "…" is not served by this server` | `.aries.yaml` and the config file name different projects; with `control_db`, the project is unknown or archived | Make the two `project` values match |
| `… is not a member of project "…"` | With `control_db`: the principal has no row in the control database, or is in other projects only | An admin runs `aries admin member add NAME` in the project's repository (`principal add NAME` for someone new) |
| `… is disabled on this server` | With `control_db`: the principal was disabled, whatever `admins:` says | An admin with reach over it runs `aries admin principal enable NAME` |
| `aries: … accepted no key`, for a key added with `aries admin` | sshd is not reading ariesd's keys file, or the file has no line for the key | On the server: `sshd -T -C user=aries \| grep -i authorizedkeysfile` must name `.ssh/aries_keys`; `grep -c NAME /var/lib/aries/.ssh/aries_keys` shows whether the line is there; `journalctl -u ssh` says when sshd would not read the file for its owner or mode |
| `aries admin` warns that ariesd `could not write the keys file sshd reads`, or `aries admin status` says `THE LAST WRITE FAILED` | ariesd cannot replace its keys file. The change itself is recorded | `journalctl -u ariesd` has `the keys file is out of date` with the cause and a fix: usually the unit's `ReadWritePaths` ([Keys from the store](#keys-from-the-store), step 1), or the owner or mode of `.ssh`. ariesd tries again every 30 seconds |
| ariesd does not start: `write the keys file …` | With `control_db`: it cannot write its keys file | The error's `fix:` names the change; see [Keys from the store](#keys-from-the-store), step 1 |
| The log has `telemetry: spans say nothing about who: the key for principals' stand-ins is unavailable`, or the status report says `TRACES SAY NOTHING ABOUT WHO` | ariesd exports traces, or logs, and cannot make or read the key it names people with. It serves as before; its spans and exported log records carry no principal and no session. The line with `cause=` says which: `it cannot be made: no such file or directory` (no directory), `read-only file system` (the unit's `ReadWritePaths`), `permission denied` (the directory's owner, or the AppArmor profile) | The lines with `fix=` beside it: make the directory and add the drop-in ([Send metrics to a collector on the machine](#send-metrics-to-a-collector-on-the-machine)). ariesd looks again every 30 seconds; a new drop-in needs a restart. Or set `telemetry: principals: omit` to leave people out on purpose |
| The same, with `cause="its file is mode 0644: others can read it"`, or `its file does not hold a key`; the status report says the key `CANNOT BE TRUSTED` | The key's file is there and is not safe to use: someone else may have read it, or it was cut short or changed | If someone may have read it, delete it: ariesd makes a new one within 30 seconds, and stand-ins made with the old key can no longer be resolved. If nobody can have, `chmod 600` it ([Traces](#traces), People) |
| `aries admin principal whois` answers `this server has no key for stand-ins: …` | What follows the colon says why: the key is unavailable, as in the two rows above; or the server exports no traces, or names people by `name` or `omit`, and has no key file from before | For an unavailable key, the rows above. Nothing resolves a stand-in made under a key that is gone |
| `the file there was not generated by ariesd` | `keys_file` names a file that holds something else, such as `authorized_keys` | Set `keys_file` to a file of its own, or move the other file away |
| A key removed with `aries admin key remove` still signs in | Its line is still in `authorized_keys`, as the command warned | Delete the line ([Keys from the store](#keys-from-the-store), step 5) |
| `… may not add a key to …: nobody adds a key to their own account` | `aries admin key add` for one's own principal | Ask another admin with reach; for a server admin, another server admin or a line in `authorized_keys` |
| `key … is the key this connection signed in with` | `aries admin key remove` of your own key, on a connection that key signed in | Sign in with another of your keys (`ARIES_KEY_FILE=PATH`) and remove it from there, or ask an admin |
| `… is the last server admin who is not disabled` | `aries admin principal disable` of the only enabled server admin | Enable another first, or name another under `admins:` and restart |
| `the key that signed in is no longer one the server holds for …` | The key was removed, and sshd read a keys file from before that: one ariesd could not rewrite, or one restored from a backup; or an `authorized_keys` line was given `--key` by hand, which only ariesd writes | Use another key, or have the key added again; check `aries admin status` for a failed write; remove `--key` from a hand-written line |
| `this server holds one project, so it has no projects, principals or members to manage` | `aries admin project`, `principal`, `member` or `key` on a server with no `control_db` | [Opt in](#opt-in), or add people as lines of `authorized_keys` |
| `the server has no database names for new projects` | `aries admin project add` on a server with no `project_dbs` | Grant a block of names and set `project_dbs` ([Opt in](#opt-in), step 4) |
| `ariesd's Dolt account was never granted database …` | `project add` took a name in `project_dbs` that was never granted | As Dolt's root, run the `GRANT` statement in the fix, then run the same `aries admin project add NAME` again |
| `every database name for new projects is in use` | Every name in `project_dbs` has a project, archived ones included | Grant more names and raise `count` |
| `… may not …: it is for …` | An `aries admin` command by someone who may not run it ([Managing access](#managing-access)) | Ask the admin the fix names |
| `ariesd upgrade` restores or installs the binary, then `sudo -n systemctl restart …: fork/exec /usr/bin/sudo: permission denied` | On Ubuntu 26.04, AppArmor profiles from before 14 October 2026: `sudo` is sudo-rs there, at a path the old operator's profile does not name | Install this release's profiles ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)), then `sudo systemctl restart ariesd` |
| `ariesd could not open the database of an active project` from `ariesd upgrade` | A project's database is gone, not granted, or failed to migrate | `journalctl -u ariesd` names it and why; fix it or archive the project, then restart |
| `the control database … holds no active project` at start | `control_db` is set and `control init` has not run | `sudo -u aries -H ariesd control init --name NAME` |
| ariesd does not start, and `systemctl status ariesd` shows `status=75/TEMPFAIL` under `not starting: the server's clock is behind its own history` | The clock is behind the latest event of the database named, by more than `clock_behind_tolerance`: the clock was reset or set back, the machine booted without its time, or the database was written by a clock that ran ahead | `timedatectl`; correct the clock and `systemctl start ariesd`. If the history is what is wrong, start once with the `--accept-clock-behind` the refusal prints, or set `clock_behind: warn` ([The server's clock](#the-servers-clock)). systemd does not retry this exit |
| After a boot, ariesd and every timer wait for good; `systemctl list-jobs` shows `time-sync.target start waiting` | `systemd-time-wait-sync` is enabled with its shipped `TimeoutStartSec=infinity`, and no time server answers | Add the drop-in of [Start ariesd after the clock is set](#start-ariesd-after-the-clock-is-set), which bounds the wait at 90 seconds; `systemctl stop systemd-time-wait-sync` lets this boot go on |
| `clock behind history` in the log, or `CLOCK BEHIND HISTORY` in `aries admin status` | The clock went back under a running ariesd, or it started with `clock_behind: warn` or `--accept-clock-behind` | Correct the clock. Until it has caught up, timestamps are ahead of it and leases run at half speed; with `clock_behind: stop`, ariesd will not start again while this lasts |
| `clock stepped` in the log, or a `clock:` line in `aries admin status` | The clock was set while ariesd ran, or the machine was suspended and resumed | Nothing for the data: leases were extended by a jump forward, and time did not go back for a step back. Find what set the clock, and whether the time service is synchronized |
| `ariesd upgrade` stops with `nothing was changed: ariesd would not start again after this upgrade` | The clock is behind the server's history, so the restarted daemon would refuse to start | As for exit status 75 above; then run the upgrade again |
| `usage record …: at … is more than 5m0s past the server's clock` | The machine that recorded the usage runs ahead of the server, or the server runs behind | Check both clocks; `clock_client_tolerance` widens the bound. `aries usage --hook` drops such a record and says how many |
| ariesd refuses the Dolt account | The account is `root` or has too many rights, or `secure_file_priv` is empty | Run the SQL its fix names, or redo [step 4](#4-create-the-database-and-the-config-file) |
| `aries` exits with code 3 | Client and server speak incompatible protocols | Upgrade the server, then the client |
| `busy` | A [limit](#limits) | Wait as the fix says, or raise the limit |
| `permission denied` on a file or socket the `aries` user owns, after you moved a path; or from `control init`, `import-bd`, `export-bd` or `upgrade` | With the [AppArmor profiles](#confine-ariesd-with-apparmor): the path is not the one the profiles name, or the command ran without `aa-exec` | `journalctl -k -g 'apparmor="DENIED"'` names the profile and the path. Change the path in `/etc/apparmor.d/tunables/ariesd` and run `apparmor_parser -r /etc/apparmor.d/ariesd`; run an operator's command through `aa-exec -p ariesd-admin --` |
| ariesd does not start: `Failed to prepare AppArmor profile change to ariesd-serve` | The unit asks for a profile that is not loaded | `apparmor_parser -r /etc/apparmor.d/ariesd`, or remove the unit's `apparmor.conf` ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)) |
| ariesd does not start: `dial tcp 127.0.0.1:3306: socket: permission denied` | The AppArmor profiles are loaded and the unit does not ask for `ariesd-serve`, so the daemon has the bridge's profile | Add the unit's `AppArmorProfile=ariesd-serve` line, `systemctl daemon-reload`, and restart |
| One failed start of ariesd in the journal of every boot, `connect: connection refused` | Dolt was not listening yet: `dolt.service` has no `ExecStartPost` that waits for its port | Nothing has to be done, since systemd starts ariesd again five seconds later. To remove the failed start, add the line to Dolt's unit ([step 5](#5-run-ariesd-under-systemd)) |
| ariesd answers `aries`, yet `systemctl status ariesd` says `activating`, and every ten minutes `start operation timed out` restarts it; its log has `could not tell systemd the daemon is ready` | The unit has `Type=notify` and the daemon cannot write to systemd's socket: the AppArmor profiles loaded are from before the unit had that line | Install this release's profiles and `apparmor_parser -r /etc/apparmor.d/ariesd` ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)); `journalctl -k -g 'apparmor="DENIED"'` names `/run/systemd/notify` |
| `Watchdog timeout (limit 2min)!` in the journal, then goroutine stacks and a restart | ariesd did not tell systemd it was alive for `WatchdogSec`: it hung, or one tick of its reaper took that long, as it can when Dolt stops answering | Read the stacks, which say where it was; check Dolt. If the machine is merely slow, raise `WatchdogSec` |
| `ariesd status`: `config: open /etc/aries/ariesd.yaml: permission denied` | It was run as a user who cannot read the config file, or as root with the AppArmor profiles loaded | `sudo -u aries -H ariesd status` |
| `telemetry: a metrics export failed` with `socket: permission denied` or `failed to load system roots` | With the AppArmor profiles: the collector's endpoint is a name that needs DNS, or is HTTPS, and the daemon's profile allows neither as shipped | Add the three lines of [Confine ariesd with AppArmor](#confine-ariesd-with-apparmor) to `local/ariesd-serve`, or send to a collector on the machine |
| `Dolt: unknown` in `aries admin status`, and `could not read Dolt's metrics` in the log | ariesd cannot read Dolt's own metrics: Dolt is stopped, its config has no `metrics:` block, or `telemetry: dolt_metrics_url:` names another port or path. Nothing else is affected | The log line has the URL and the cause. `curl -s http://127.0.0.1:11228/metrics \| head -3` on the server shows whether Dolt answers; then [Dolt's figures](#dolts-figures) |
| ariesd does not start: `telemetry dolt_metrics_url … is not a loopback address`, `is not an http:// URL` or `has no path` | The setting names something other than Dolt's endpoint on this machine | Set it to `http://127.0.0.1:11228/metrics`, or remove it |
| `Dolt's collector: … OVER 14, AS WITH A MEMORY LIMIT TOO LOW` in `aries admin status` | Dolt's garbage collector runs far more often than its writes explain, as it did in the measurements under a `GOMEMLIMIT` that was too low | Raise the limit, or restart Dolt, or `dolt gc` and restart it ([Dolt's memory](#dolts-memory)) |
| `SESSIONS` is `unknown` in `aries admin status` or `ariesd status`, with `sessions: unknown for … (database did not answer in 2s)` | Dolt did not answer a count within 2 seconds: it hangs, is stopped (`SIGSTOP`), swaps, or is collecting without pause under a memory limit too low. The rest of the report is ariesd's own memory and stands | On the server: `systemctl status dolt`, `journalctl -u dolt`, and the report's `Dolt` lines ([Dolt's memory](#dolts-memory)). With `(the read failed; see ariesd's log)` instead, the log's `count the sessions seen, for status` has the error |
| `aries`: `the server's database does not answer, so project "…" was not opened`, from `aries admin status` too | Dolt did not answer a ping within 2 seconds, and the project's store is closed: ariesd does not open a store on a Dolt that hangs, and a connection is to a project ([When Dolt does not answer](#when-dolt-does-not-answer)). The log has `dolt.check_failed` with the ping's error | Run `sudo -u aries -H ariesd status` on the server, or `aries admin status` from the repository of a project in use; then see to Dolt: `systemctl status dolt`, `journalctl -u dolt` |
| `aries`: `the server timed out on …` after a minute, while `aries admin status` answers | Dolt does not answer. The status report does not wait for it; every other request does, for the request timeout | `aries admin status`, or `ariesd status` on the server, for what ariesd knows; then see to Dolt |
| `ariesd status`: `status: the daemon did not answer` | ariesd welcomed the connection and then sent no report for 30 seconds, or closed it: the daemon itself is stuck or stopping, since the report does not wait on Dolt | `journalctl -u ariesd` for what it waits on; `systemctl restart ariesd` if that does not end |
| `/readyz` answers `not ready: the last check of Dolt failed` | Dolt hangs or cannot be reached | `systemctl status dolt`, `journalctl -u dolt`; ariesd's log has the ping's error |
