# Running a server

A starfix server is one Linux machine running three programs:

| Program | Role |
|---|---|
| sshd | The front door. It checks each person's or agent's key and runs `starfixd stdio` for them |
| `starfixd serve` | The daemon. It owns the data and applies every change in order |
| Dolt | The database, listening on loopback for starfixd alone |

A server holds one project, as the steps below set it up. It can be
switched to hold many, each in its own database ([Serving many
projects](#serving-many-projects)). This page sets a server up step by
step, then covers principals, settings, limits, logs, metrics and health
([Observability](#observability)), upgrades, backups and troubleshooting.
For complete deployments with costs, see the
[deployment examples](deploy/README.md).

## What a server needs

- **Linux**, amd64 or arm64, with systemd and OpenSSH. `starfixd serve`
  refuses to run on other systems, because only Linux lets it check which
  user connects to its socket.
- **Ubuntu 24.04 LTS is the supported platform.** The steps below are
  written for it and were run on it, under systemd, in a virtual machine
  on arm64 ([What was run, and where](#what-was-run-and-where)). amd64
  and cloud providers' images were not. Other distributions may work and
  are untested.
- **Systems that run SELinux are not supported yet**: RHEL, Rocky Linux,
  AlmaLinux, Fedora. Nothing here has been run on one, and this guide
  sets no SELinux label and ships no policy module. That support will
  follow once starfix has a test harness that boots such a machine.
  Ubuntu's mandatory access control is AppArmor. Stock Ubuntu 24.04
  confines neither sshd nor starfixd with it; profiles for starfixd ship
  with starfix, as an optional step
  ([Confine starfixd with AppArmor](#confine-starfixd-with-apparmor)).
- **A synchronized clock.** Leases and the order of history depend on
  it; starfixd detects a clock that jumps or goes back and, by default,
  refuses to start when the clock is behind its own history
  ([The server's clock](#the-servers-clock)).
- **Dolt**, at the release starfix is tested with: `Dolt` in
  [internal/version](../internal/version/version.go), 2.4.2 for starfix
  v0.2.0. Each release's notes name it.
- **Little hardware.** Idle, Dolt used about 90 MB of memory and starfixd
  about 20 MB, or 22 MB with metrics pushed and scraped (resident
  memory on arm64, a minute after its start; about 14 MB of either is
  the program itself, mapped from its file). Each connection adds about
  5 MiB more: 2.5 MiB for its bridge (`starfixd stdio`), 2 MiB for
  sshd's two processes, half a MiB in the kernel and under 0.1 MiB in
  starfixd, so 64 connections take about 320 MiB and 1,024 about
  5 GiB. That is with transparent huge pages in `madvise` mode, Ubuntu's
  default; with them `always` (`cat
  /sys/kernel/mm/transparent_hugepage/enabled`) the bridge takes about
  6 MiB, and a connection about 9 (measured on Linux, arm64, OpenSSH 9.6,
  14 October 2026; [the profile](design/profile-starfixd.md#2-a-bridge-that-is-not-the-whole-of-starfixd)
  has how). Under the stage 0 load test Dolt reached 1.4 GB and about
  250 writes a second on 4 vCPUs
  ([results](../spike/dolt/RESULTS.md)), roughly ten times what a busy
  team of agents needs. 2 GB of memory suits a team with one project of
  a few thousand issues; [Dolt's memory](#dolts-memory) says how it grows
  and how to get it back. The data starts at under 1 MB.

## Set up a server

Run these steps as root (`sudo -i`). Each step ends with a command that
shows it worked, and none puts a password on a command line.

### 1. Install Dolt

Use `arm64` in place of `amd64` on an Arm machine:

```sh
curl -fsSLO https://github.com/dolthub/dolt/releases/download/v2.4.2/dolt-linux-amd64.tar.gz
sha256sum dolt-linux-amd64.tar.gz
tar -xzf dolt-linux-amd64.tar.gz
install -m 0755 dolt-linux-amd64/bin/dolt /usr/local/bin/dolt
dolt version
```

Before you unpack it, compare the checksum with the one starfix's own
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
sudo -u dolt -H dolt config --global --add user.name "starfix server"
sudo -u dolt -H dolt config --global --add user.email starfix@localhost
sudo -u dolt -H dolt config --global --add metrics.disabled true
```

- `secure_file_priv` names a directory that does not exist, so no account
  can read or write files through Dolt, whatever its grants. starfixd
  refuses to start when it is empty ([why](security-model.md#the-dolt-account)).
- `cfg_dir` keeps Dolt's accounts outside the data directory.
- **`metrics:` turns on Dolt's own metrics**, in Prometheus's text
  format, at `http://127.0.0.1:11228/metrics`. starfixd reads them for
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
  [starfixd's own listener](#the-listener-metrics-to-scrape-and-health).
  Leave the block out to run without it; the status report then says
  nothing of Dolt.
- Run `dolt` commands from a directory the `dolt` user can read, such as
  `/var/lib/dolt`, or it fails to load its config.

Then run it under systemd. Save this as `/etc/systemd/system/dolt.service`:

```ini
[Unit]
Description=Dolt SQL server for starfix
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
connection, and whatever is ordered after it, starfixd first, starts
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

### 3. Install starfixd

starfixd runs as its own user, `starfix`. sshd runs every key's forced
command as this user:

```sh
useradd --system --create-home --home-dir /var/lib/starfix --shell /bin/sh starfix
usermod -p '*' starfix
install -d -o starfix -g starfix -m 0755 /opt/starfix/bin
cd /var/lib/starfix
curl -fsSL https://raw.githubusercontent.com/ariesworx/starfix/main/install.sh | sudo -u starfix -H sh -s -- --server --dir /opt/starfix/bin --require-signature
ln -sfn /opt/starfix/bin/starfixd /usr/local/bin/starfixd
starfixd version
```

- **The shell must be real.** sshd runs a forced command through the
  account's shell, so `/usr/sbin/nologin` would refuse every connection.
- **The password is `*`, not locked.** No password matches `*`. A locked
  account (`passwd -l`, a `!` in `/etc/shadow`) makes sshd refuse even
  keys when PAM is off.
- **starfix owns the binary's directory**, so `starfixd upgrade` can
  replace it without root. `/usr/local/bin/starfixd` gives the forced
  command a path that never changes.
- The installer checks the release's signature and checksum
  ([Install](install.md)).

### 4. Create the database and the config file

This creates starfix's database and an account with rights on it alone,
gives Dolt's `root` a random password, and writes the config file. Both
passwords travel on stdin:

```sh
cd /var/lib/dolt
(
  set -e
  umask 077
  dbpw=$(openssl rand -hex 24)
  rootpw=$(openssl rand -hex 24)
  printf "CREATE DATABASE starfix;\nCREATE USER 'starfix'@'localhost' IDENTIFIED BY '%s';\nGRANT ALL ON starfix.* TO 'starfix'@'localhost';\nALTER USER 'root'@'localhost' IDENTIFIED BY '%s';\n" "$dbpw" "$rootpw" |
    sudo -u dolt -H env DOLT_CLI_PASSWORD= dolt --host 127.0.0.1 --port 3306 --no-tls -u root sql
  install -d -o root -g starfix -m 0750 /etc/starfix
  printf 'dsn: starfix:%s@tcp(127.0.0.1:3306)/starfix\nproject: %s\nsystemd_unit: starfixd.service\nadmins: [alice]\ntelemetry:\n  dolt_metrics_url: http://127.0.0.1:11228/metrics\n' "$dbpw" "$(cat /proc/sys/kernel/random/uuid)" |
    install -o starfix -g starfix -m 0600 /dev/stdin /etc/starfix/starfixd.yaml
)
grep project /etc/starfix/starfixd.yaml
```

- **The account can do anything to its one database and nothing else**:
  no global privilege, no `FILE`, no `GRANT OPTION` (`ALL` does not include
  it). starfixd checks this every time it opens the store.
- **Nobody needs `root`'s password**, so the step forgets it. Dolt's own
  user can still administer the server from the data directory:
  `cd /var/lib/dolt/data/starfix && sudo -u dolt -H dolt sql`. You may
  drop `root` instead (`DROP USER 'root'@'localhost';`); Dolt does not
  bring it back on restart.
- **`project`** is the project's UUID. Every repository's `.starfix.yaml`
  names it.
- **`admins`** lists the first principal, `alice`, who you let in at step
  6. [Settings](#settings) lists every other key.
- **`telemetry: dolt_metrics_url:`** is where starfixd reads Dolt's own
  metrics, which step 2 turned on, for
  [the status report](#watching-a-server). Leave the two lines out if
  you left `metrics:` out of Dolt's config.

The step runs once: a second run finds `root` protected and stops.

### 5. Run starfixd under systemd

Save this as `/etc/systemd/system/starfixd.service`:

```ini
[Unit]
Description=starfix server
After=network.target dolt.service time-sync.target
Wants=time-sync.target
Requires=dolt.service

[Service]
Type=notify
NotifyAccess=main
TimeoutStartSec=600
WatchdogSec=120
User=starfix
Group=starfix
ExecStart=/opt/starfix/bin/starfixd serve
Restart=on-failure
RestartSec=5
RestartPreventExitStatus=75
RuntimeDirectory=starfix
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

`RuntimeDirectory` makes `/run/starfix` with mode 0700, so only the
`starfix` user reaches the daemon's socket.

| Line | Does |
|---|---|
| `Type=notify` | systemd counts the unit as started when starfixd says it is ready, which it does once it listens on its socket: after the database is open and, on a server of many projects, every project migrated. Until then `systemctl start` waits, and units ordered after this one do not start |
| `NotifyAccess=main` | Only starfixd's own process may send those notifications |
| `TimeoutStartSec=600` | How long systemd waits for that. Its default is 90 seconds, after which it fails the start and `Restart=on-failure` tries again; an upgrade that migrates many projects can take longer than that, so the unit gives it 10 minutes |
| `WatchdogSec=120` | starfixd must say it is alive at least every 120 seconds, or systemd kills it and `Restart=on-failure` starts it again. starfixd says so every 60, half the interval, from the goroutine that runs its 30-second reaper, so a daemon whose reaper hangs is restarted. One tick of the reaper that takes longer than `WatchdogSec` is a hang too, and a Dolt that has stopped answering can cause one: each of the reaper's writes waits up to a minute |
| `Wants=time-sync.target` and `time-sync.target` in `After=` | Order starfixd after the clock is synchronized, on a machine where something provides that target. Stock Ubuntu 24.04 reaches it at once, so there the lines wait for nothing ([Start starfixd after the clock is set](#start-starfixd-after-the-clock-is-set)) |
| `RestartPreventExitStatus=75` | Leaves starfixd stopped when it refuses to start because the clock is behind its own history, which no restart can correct ([The server's clock](#the-servers-clock)). It exits with that status 0.13 seconds after systemd starts it, before it would tell systemd it is ready, so the start fails then and not when `TimeoutStartSec` runs out. Every other failure is still restarted |

Without the first four lines starfixd runs as before: it sends a
notification only when systemd asks for one.

The lines were run on the machine of
[What was run, and where](#what-was-run-and-where); what the two for
the clock showed is in [The server's clock](#the-servers-clock):

- **The unit is active only once starfixd serves.** systemd logs
  `Started starfixd.service` within a millisecond of starfixd's own
  `starfixd serving`, and until then the unit is `activating`.
- **A daemon that hangs is restarted.** With the daemon frozen
  (`SIGSTOP`), systemd logged `Watchdog timeout (limit 2min)!` 120
  seconds after the last sign of life, killed it with `SIGABRT` and
  started it again five seconds later. Go answers `SIGABRT` by printing
  every goroutine's stack, so the journal then holds what the daemon was
  doing. A healthy daemon was left alone.
- **`systemctl stop` is clean**: starfixd tells systemd it is stopping,
  closes its stores and exits with status 0.
- **If you use the [AppArmor profiles](#confine-starfixd-with-apparmor),
  install this release's with the unit.** The daemon's profile from
  before these lines existed does not let it write to systemd's socket:
  the daemon then serves, logs `could not tell systemd the daemon is
  ready`, and the unit stays `activating` until `TimeoutStartSec` runs
  out, when systemd stops it and starts it again
  ([Troubleshooting](#troubleshooting)).

Then let `starfixd upgrade` restart the unit, and start it:

```sh
echo 'starfix ALL=(root) NOPASSWD: /usr/bin/systemctl restart starfixd.service' > /etc/sudoers.d/starfix
chmod 0440 /etc/sudoers.d/starfix
visudo -cf /etc/sudoers.d/starfix
systemctl daemon-reload
systemctl enable --now starfixd
journalctl -u starfixd -n 3
```

The log shows `starfixd serving` with the socket, the project and the
admins. If it shows a refusal instead, its `fix:` says what to change.

**starfixd starts once Dolt listens,** at boot and whenever Dolt's unit
restarts. `After=dolt.service` orders it after Dolt's unit, and that
unit is started only when Dolt accepts connections (its `ExecStartPost`,
[step 2](#2-run-dolt-under-its-own-account)). `Requires=dolt.service`
has systemd stop starfixd with Dolt, on `systemctl restart dolt` or
after a crash, and start both again in that order.

**A server set up from this guide before 13 October 2026 has no such
line,** and there starfixd's first start fails at every boot and at
every restart of Dolt: starfixd finds no database (`dial tcp
127.0.0.1:3306: connect: connection refused`) and exits, and
`Restart=on-failure` starts it again five seconds later, which works.
The journal holds one failed start each time (`Failed to start
starfixd.service`), and `sfx` answers `starfixd is not running on the
server` for those seconds. `Type=notify` does not remove that race,
since it is Dolt's unit that is counted as started too early. To remove
it, add the `ExecStartPost` line of step 2 to `dolt.service` and run
`systemctl daemon-reload`; it takes effect when Dolt next starts.

The line is in Dolt's unit, and not an `ExecStartPre` in starfixd's,
because the [AppArmor profile](#confine-starfixd-with-apparmor) the
daemon's unit may ask for confines every command of that unit, and it
allows the daemon to run nothing.

### 6. Let the first principal in

Each key gets one line in `~starfix/.ssh/authorized_keys`. The line's
forced command names the principal, so sshd, not the client, decides who
connects:

```sh
install -d -o starfix -g starfix -m 0700 /var/lib/starfix/.ssh
echo 'restrict,command="/usr/local/bin/starfixd stdio --principal alice" ssh-ed25519 AAAA… alice@example.com' >> /var/lib/starfix/.ssh/authorized_keys
chown starfix:starfix /var/lib/starfix/.ssh/authorized_keys
chmod 0600 /var/lib/starfix/.ssh/authorized_keys
```

`restrict` turns off forwarding, terminals and the rest. Never add a
`ForceCommand` for the account: it would override each key's `command=`,
and so its principal.

Then harden sshd for the account. Save this as
`/etc/ssh/sshd_config.d/00-starfix.conf`:

```text
MaxStartups 10:30:60
ClientAliveInterval 60
Match User starfix
    AuthenticationMethods publickey
    DisableForwarding yes
    PermitTTY no
    AuthorizedKeysCommand none
    TrustedUserCAKeys none
    AuthorizedPrincipalsFile none
```

The last three lines make the account's keys come from its own key
files and nowhere else. Without them, whatever the machine's sshd trusts
for every account applies to `starfix` too: a cloud image's own key
command (Google Cloud's OS Login, AWS's EC2 Instance Connect, Azure's
Entra ID login), or a certificate authority your company's images trust.
A key such a command prints, or a certificate from such an authority
that names `starfix`, would be let in with no forced command, which is a
shell as the account that can read starfixd's config file. Checked on 12
October 2026 with OpenSSH 9.6 on Ubuntu 24.04: with a certificate
authority trusted outside the `Match` block, a certificate for the
principal `starfix` ran a command as the account; with these lines it is
refused.

```sh
install -d -m 0755 /run/sshd
sshd -t
systemctl try-reload-or-restart ssh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

- `sshd -t` checks the config. It needs `/run/sshd`, which systems that
  start sshd on the first connection have not made yet.
- The unit is `sshd` rather than `ssh` on some distributions.
- `DisableForwarding` means no tunnel to Dolt, even from a key line that
  lacks `restrict`.
- `MaxStartups` drops floods of unauthenticated connections early.
- `ClientAliveInterval` keeps an agent's long session alive through
  tunnels that drop idle connections.
- A `Match` block in a drop-in file applies to that file only (OpenSSH 9.6
  checked).
- On a machine whose admins all sign in with keys, also add
  `PasswordAuthentication no` above the `Match` line.

The last command prints the host key fingerprint, `SHA256:…`. Give it to
everyone who connects. To change the key later without refusing every
client at once, see [Rotating the host key](#rotating-the-host-key).

**On a public port**, also rate-limit new connections at the firewall
(`ufw limit 22/tcp`). Skip that when everyone arrives through a few shared
addresses (Google Cloud IAP, a VPN or a NAT), because it would throttle
the whole team. To keep sshd off the internet altogether, put it behind a
VPN such as WireGuard; starfix works the same either way.

OpenSSH 9.8 and later also turn away, for a while, an address that fails
to log in or crashes a session (`PerSourcePenalties`, on by default).
Behind shared addresses, one bad client could then lock out the team, so
list those ranges in `PerSourcePenaltyExemptList`, above the `Match` line
(it is a global setting).

### 7. Connect a repository

Commit a `.starfix.yaml` to the repository, with the project UUID from step
4 and the fingerprint from step 6:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  host: starfix.example.com
  host_key: SHA256:…
```

Then, on alice's machine, inside the repository:

```console
$ sfx create "Fix the login redirect" -p 1 -t bug
sf-aukz3rip
$ sfx ready
sf-aukz3rip  P1  open  Fix the login redirect
```

[Using sfx](cli.md#connect-a-repository) explains every field of the file,
and the [agent guide](agents.md) sets agents up in the repository.

### What was run, and where

Steps 1 to 7, then [Serving many projects](#serving-many-projects) with
its keys file and `sfx admin`, were run in order as written on 12
October 2026, on one machine:

| | |
|---|---|
| Machine | A virtual machine on an Apple silicon Mac (Lima 2.2.1), arm64, 2 CPUs and 4 GiB, from Ubuntu's cloud image for 24.04 |
| System | Ubuntu 24.04.5, kernel 6.8.0-142, systemd 255, OpenSSH 9.6p1, sudo 1.9.15p5, AppArmor 4.0.1, Dolt 2.4.2 |
| starfixd | Built from the source tree for linux/arm64 with the release's flags, and put where step 3's installer puts a release: `/opt/starfix/bin/starfixd`, owned by `starfix`, mode 0755. The installer itself was not run |
| Clients | `sfx` inside the same machine, against `localhost`, as three Unix users with a key each |

What that showed:

- **The units work as written.** `systemd-analyze verify` accepts both.
  `systemd-analyze security` scores starfixd's 7.2 and Dolt's 7.1
  ("medium"; lower is tighter): they run as their own users with a
  read-only file system, and nothing limits their system calls,
  capabilities or namespaces.
- **sshd's `Match` block works on stock Ubuntu.** The forced command
  runs in place of whatever command the client asks for, a terminal is
  refused
  (`PTY allocation request failed`), and a tunnel to Dolt is refused
  (`open failed: administratively prohibited`).
- **The first start after a boot fails**, and the next one works
  ([step 5](#5-run-starfixd-under-systemd)). That was with Dolt's unit
  as it then was, without its `ExecStartPost`.
- **Dolt's unit with `ExecStartPost` is started only once Dolt
  listens.** This was run on 13 October 2026, and not on the machine
  above: in a container (Docker Desktop on an Apple silicon Mac, arm64)
  of Ubuntu 24.04 with systemd 255.4 as its first process and Dolt
  2.4.2 set up by steps 1 and 2, with a stand-in for starfixd: a unit
  with starfixd's `After=dolt.service` and `Requires=dolt.service` that
  tries the port once. Started from stopped five times each way,
  without the line every attempt was refused, and with it every attempt
  connected; the start took 0.28 seconds in place of 0.03. A connection
  made at once after `systemctl restart dolt` succeeded three times of
  three with the line. With a process in Dolt's place that never
  listens, the start failed after 63 seconds and systemd tried again.
  `systemd-analyze verify` accepts the unit. Not run: starfixd itself
  ordered after the unit, a real boot, and the machine above.
- **The unit's `Type=notify`, start timeout and watchdog work**
  ([step 5](#5-run-starfixd-under-systemd)), on the same machine with
  the AppArmor profiles enforced, across a cold boot. So do the
  loopback listener, `/readyz` when Dolt stops answering, metrics
  pushed to a receiver on the machine, and `starfixd status`
  ([Observability](#observability)).
- **Traces work as written** ([Traces](#traces)), on the same machine
  on the same day with the profiles enforced. With the guide's one
  variable and nothing else starfixd starts and serves, posts spans to
  a receiver on the machine that hold no stand-in and no name, and says
  why at error level, in its status report and in `whois`: `it cannot
  be made: no such file or directory`. With the directory made and no
  restart, half a minute later: `read-only file system`. With the
  drop-in and a restart, under a profile without the rule for the file:
  `permission denied`, with the denial in the kernel's log. With the
  rule loaded and no restart, half a minute later: `made the key`,
  mode 0600, `spans name people by stand-ins from now on`, and spans
  with a stand-in. `sfx admin principal whois` then resolves it for a
  server admin. With the file made readable by others and a restart:
  no stand-in again, `CANNOT BE TRUSTED` in the status; after `chmod
  600` and no restart, the same stand-in as before. An earlier build of
  the same day, which still refused to start without the key, showed
  that a restart keeps the key, that deleting the file gives the same
  person another stand-in and leaves the old one resolving to nobody,
  that `principals: name` and `OTEL_TRACES_EXPORTER=none` need no key,
  and that neither another user, nor the `starfix` user under the
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
  `ps`'s, and the connections were starfixd's own, counted with `ss`.
  The collector's line gave the same cycles and writes as Dolt's counter
  and starfixd's own metric did over a burst. With the port refused by
  a firewall rule for 50 seconds the report said `Dolt: unknown` with
  the last read, `/readyz` stayed ready and `sfx` worked, with one
  warning in the log and one line when the reads came back; a URL with
  the wrong port, one that is not loopback and one with `https://` did
  what [Dolt's figures](#dolts-figures) says. [Dolt's
  memory](#dolts-memory) has what the line showed under a memory limit
  too low and one with room. The 76 checks passed afterwards, once
  their script was brought up to date with this release in three
  places: each user's
  key is named by `STARFIX_KEY_FILE` in place of a `key:` line outside
  the repository, `member add` for someone with a key takes
  `--accept-keys`, and the refusal to add a key to one's own account
  has new words.
- **`starfixd upgrade --rollback` works for the `starfix` user**: it
  renames the binaries in `/opt/starfix/bin`, restarts the unit through
  the sudo rule and finds the daemon healthy. `upgrade --check` reaches
  GitHub and compares versions.
- **The daily dump** of [Backups and restore](#backups-and-restore)
  writes its file.

Not run: amd64; a cloud provider's image or network; a real
OpenTelemetry Collector on this machine, or a hosted backend, in place
of which a few lines of Python received the pushes (an OpenTelemetry
Collector, 0.162.0, did receive and decode the spans of a starfixd run
by hand on a developer's machine); `install.sh` on the server; an upgrade to a newer release, which a build from source refuses
(`a development build … never upgrades itself`), so the download, the
signature check and the swap to a new binary were not run here; a
restore from a dump. For the clock: a machine with a real-time clock of
its own, working or dead (this guest's comes from the hypervisor at
every boot and was always right); a boot with the clock wrong; a
virtual machine paused and resumed, or restored from a snapshot; a
provider's time service; `chrony`.

## Principals

A principal is a name for a person or an agent identity, such as `alice`.
It comes from the key that authenticated, never from the client: sshd
runs a forced command that names it, and nothing the client sends can
change that.

- **Name rules:** lowercase, starting with a letter, then letters, digits,
  `.`, `_` or `-`, up to 64 characters. `starfixd` (the claim reaper and
  the daemon's own changes), `import` (the bd importer and `control init`),
  `starfixd-upgrade` (the health check of `starfixd upgrade`) and
  `starfixd-status` ([`starfixd status`](#starfixd-status)) are reserved.
- **Agents on someone's machine** use that person's key, so their work is
  theirs. Give an agent that runs on its own, in CI or in a cloud sandbox,
  its own principal and key (for example `ci-bot`). Its work then shows
  under its own name, and its key can be revoked alone. A key belongs to
  one principal.

Where the key and its principal are recorded depends on the server:

| | A server of one project | A server of [many projects](#serving-many-projects) |
|---|---|---|
| A key is | A line of `~starfix/.ssh/authorized_keys` whose forced command names the principal ([step 6](#6-let-the-first-principal-in)) | A row of the control database, which starfixd writes to a file sshd reads ([Keys from the store](#keys-from-the-store)). A line of `authorized_keys` still works too |
| Add someone | Append a line, on the server. A principal may have several, one per key | `sfx admin principal add NAME --key FILE`, from anywhere |
| Add a key | Append a line | `sfx admin key add NAME FILE` |
| Remove a key | Delete its line. That stops new connections; to end open sessions too, stop the bridges: `pkill -f -- 'stdio --principal bob$'` | `sfx admin key remove FINGERPRINT`, which also closes the connections that key signed in |
| Remove someone | Delete their lines, then `pkill` as above | `sfx admin principal disable NAME`, or `member remove NAME` for one project: their connections close and their claims end at once |

On a server of many projects what a principal may reach is its
memberships, managed with `sfx admin` ([Managing access](#managing-access)).
A key says who someone is and nothing else.

## Admins

Admins are principals named under `admins:` in the config file, or in
`STARFIXD_ADMINS` (comma-separated). `serve` reads the list when it starts.
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
share the one Dolt, the one starfixd and the machine. A second database,
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
  with `sfx admin`, over the same SSH connection as every other command
  ([Managing access](#managing-access)). A new project needs no shell on
  the server: its database comes from a block of names you grant once.
  Nor does a new person: an admin adds them and their public key, and
  starfixd writes the key to a file sshd reads
  ([Keys from the store](#keys-from-the-store)).
- **Access that ends, ends at once.** When a membership is removed, a
  principal is disabled, a project is archived or a key is removed,
  starfixd closes the connections it had let in, once the change is
  committed. The first three also end the claims the lost access held,
  so the issues are ready for someone else.
- **`sfx admin status`** shows an admin the server and each project as
  they are now: open stores, connections, sessions, writes and their
  latency, and starfixd's own memory ([Watching a server](#watching-a-server)).
- **`import-bd` and `export-bd`** act on the config file's `project`, or
  on another with `--project UUID`.

Everything above is built and tested against a real Dolt and starfix's
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

1. Create the control database and grant it to starfixd's account, as
   Dolt's own user, the way step 4 says to administer Dolt:

   ```sh
   cd /var/lib/dolt/data/starfix
   sudo -u dolt -H dolt sql -q "CREATE DATABASE IF NOT EXISTS starfix_control; GRANT ALL ON starfix_control.* TO 'starfix'@'localhost';"
   ```

   The grant names the database exactly. Dolt ignores a grant on a name
   pattern.

2. Name it in `/etc/starfix/starfixd.yaml`, leaving `dsn`, `project`,
   `prefix` and `account` as they are. They now describe the project to
   adopt:

   ```yaml
   control_db: starfix_control
   ```

3. Adopt the existing database as the first project, and import the
   principals and keys of `authorized_keys`:

   ```sh
   sudo -u starfix -H starfixd control init --name acme
   ```

   ```text
   project acme (6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f) adopted from database starfix: active
   principals: 2 added, 0 already there; keys: 3 added, 0 already there; members: 1 added, 0 already there; 0 lines skipped
   3 keys in the store are also lines of /var/lib/starfix/.ssh/authorized_keys: sshd reads that file too, so `sfx admin key remove` does not keep such a key out until its line is deleted too
   ```

   The last line counts the keys that are now in both places. It is a
   reminder, and `control init` never edits `authorized_keys`: see
   [Keys from the store](#keys-from-the-store), step 5.

4. To let admins create further projects, grant starfixd's account a
   block of database names now, and name the block in the config file.
   Dolt honors a grant on a database that does not exist yet, and only on
   an exact name, so the names are listed one by one:

   ```sh
   cd /var/lib/dolt/data/starfix
   for i in $(seq 1 100); do
     printf "GRANT ALL ON starfix_p%03d.* TO 'starfix'@'localhost';\n" "$i"
   done | sudo -u dolt -H dolt sql
   ```

   ```yaml
   project_dbs:
     prefix: starfix_p
     count: 100
   ```

   The names are the prefix and a number of three digits, here
   `starfix_p001` to `starfix_p100`; `count` is at most 999, and the
   prefix a lowercase letter, then lowercase letters, digits and
   underscores. `sfx admin project add` takes the lowest name no project
   has. To add more later, grant them the same way (`seq 101 200`)
   and raise `count`. Without `project_dbs` the server serves the
   projects it has and refuses to create one.

5. Let starfixd write its keys file and have sshd read it, which is
   also what lets you add people from anywhere, with no line in
   `authorized_keys`: [Keys from the store](#keys-from-the-store), steps
   1 and 2. starfixd does not start on a server of many projects until
   it can write that file.

6. Restart starfixd: `systemctl restart starfixd`.

`control init` records the database in `dsn` as a project, with the
config file's `project` UUID, `prefix` and `account` and the name you
give, and changes nothing in that database. Then, for each line of
`/var/lib/starfix/.ssh/authorized_keys` (or `--authorized-keys FILE`)
whose forced command is `starfixd stdio --principal NAME`, it adds the
principal if it is new, adds the key, and makes the principal a member
of the project. A principal named under `admins:` gets its row and keys
and no membership, since a server admin needs none. A line it cannot use
is skipped and reported on stderr by number and reason, never by its
text.

- **Run it again whenever `authorized_keys` gains a line.** It adds what
  is new and leaves the rest: it does not demote an admin, re-activate an
  archived project, or remove anyone. A running starfixd reads the
  control database again every 30 seconds, so the new member can connect
  within the minute.
- **Run again, it does not undo what an admin did.** The file is not
  edited when an admin removes someone with `sfx admin`, so its lines
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
  removed with `sfx admin key remove`, the memberships of the adopted
  project removed with `sfx admin member remove`, and the principals
  disabled with `sfx admin principal disable`. Each restoration is an
  event under `import`, and is reported. Use it when the file is what
  you mean, for example after restoring a control database from an old
  backup. It is an operator's command like `control init` itself: where
  the AppArmor profiles are installed, run it through `aa-exec -p
  starfixd-admin` ([Confine starfixd with AppArmor](#confine-starfixd-with-apparmor)). It reads the control
  database and `authorized_keys`, and writes no file.
- **Each line is read as `starfixd stdio` would run it.** The principal
  is the one sshd would sign the key in as: the last `--principal` of
  the forced command. A command that `stdio` would refuse, such as one
  with a stray argument, is skipped.
- **It does not remove.** Deleting a line from `authorized_keys` leaves
  the principal's row, its membership and the key in the control
  database. Where sshd reads starfixd's keys file
  ([Keys from the store](#keys-from-the-store)) the key therefore still
  signs in, until `sfx admin key remove` takes it out; where sshd does
  not, deleting the line stops the key, as before.
- **Its changes are on the record** in the control database's event log,
  under the reserved principal `import`.
- **`serve` refuses to start** while the control database holds no active
  project, and says to run `control init`.
- **To go back,** remove `control_db` and restart. The project's database
  was never changed, and the control database is left unused.

### Keys from the store

On a server of many projects starfixd writes the keys its control
database holds to a file, and sshd reads that file before
`authorized_keys`. An admin adds and removes keys with `sfx admin`, from
anywhere, and nobody edits a file on the server. The file is
`~starfix/.ssh/starfix_keys`, in the format of `authorized_keys`: a
first line that says starfixd generates it, then one line for each key
of each active principal, the line you would have written by hand with
the key's fingerprint added:

```text
restrict,command="/usr/local/bin/starfixd stdio --principal alice --key SHA256:…" ssh-ed25519 AAAA…
```

sshd asks starfixd nothing. It reads two files, starfixd's first, and
nothing of starfix runs before sshd has authenticated a key.
`authorized_keys`, which you maintain by hand, stays the way in for a
key the control database does not hold: the first admin's before
`control init`, and a server admin's own new key.

- **When it is written.** At start, before starfixd listens; after every
  `sfx admin` change that alters which keys sign in (a key added or
  removed, a principal added with keys, disabled or enabled), before
  the admin is answered; and every 30 seconds if it no longer holds
  what the control database does, which is how a `starfixd control
  init` run beside the daemon reaches it, and how an edit to the file
  is undone. A key added with `sfx admin` signs in at once, with no
  restart of anything. When nothing changed, nothing is written.
- **How it is written.** A new file beside it, mode 0600, synced, then
  renamed over it, so sshd never reads half a file and a crash leaves
  the old one or the new one. The lines are in a fixed order, so the
  same keys give the same bytes.
- **The file is a cache, and the control database decides.** Each line
  names its key's fingerprint, and starfixd refuses a connection whose
  key the database no longer holds for that principal, and one whose
  principal is disabled. So a key that was removed, in a file that
  could not be rewritten or was restored from a backup, gets past sshd
  and is refused by starfixd. sshd reads this file first, so that
  refusal stands even when the key is also a line of `authorized_keys`:
  an out-of-date file can refuse a key the other file would have let
  in, and never lets in one the database has dropped.
- **A server of one project writes no file.**

1. **Let starfixd write to the account's `.ssh`.** The unit of
   [step 5](#5-run-starfixd-under-systemd) has `ProtectSystem=strict`,
   which makes the file system read-only for starfixd, apart from
   `/run/starfix` and its private `/tmp`. Its `ProtectHome=yes` does not matter here: it
   covers `/home`, `/root` and `/run/user`, and the account's home is
   `/var/lib/starfix`. Add the one directory:

   ```sh
   mkdir -p /etc/systemd/system/starfixd.service.d
   printf '[Service]\nReadWritePaths=/var/lib/starfix/.ssh\n' > /etc/systemd/system/starfixd.service.d/keys.conf
   systemctl daemon-reload
   ```

   The directory is the one [step 6](#6-let-the-first-principal-in)
   made: owned by `starfix`, mode 0700. starfixd refuses to write into
   a directory that group or others may write to, as sshd refuses to
   read keys from one. It also refuses a path that is a symbolic link
   or anything but a regular file, and a file it did not generate, so
   pointing `keys_file` at `authorized_keys` by mistake cannot replace
   it.

2. **Tell sshd to read the file,** for the `starfix` account only. Add
   one line to the `Match` block of
   `/etc/ssh/sshd_config.d/00-starfix.conf`
   ([step 6](#6-let-the-first-principal-in)):

   ```text
   Match User starfix
       AuthenticationMethods publickey
       DisableForwarding yes
       PermitTTY no
       AuthorizedKeysFile .ssh/starfix_keys .ssh/authorized_keys
       AuthorizedKeysCommand none
       TrustedUserCAKeys none
       AuthorizedPrincipalsFile none
   ```

   ```sh
   sshd -t
   systemctl try-reload-or-restart ssh
   ```

   sshd takes the first line that matches the key, in the order the
   files are named, so the order matters: starfixd's file comes first.
   A key in both files is then let in by starfixd's line, which names
   its fingerprint and the principal the control database gives it, so
   `sfx admin key remove` closes its connections and starfixd can
   refuse it. With the files the other way round, such a key would come
   in by its line of `authorized_keys`, which names no fingerprint, and
   removing it would neither close its connections nor show.

   With `AuthorizedKeysCommand none` in the `Match` block, sshd on
   stock Ubuntu 24.04 (OpenSSH 9.6, no `AuthorizedKeysCommand` of its
   own) logs `error: No user for AuthorizedKeysCommand specified,
   skipping` each time it is offered a key that is in neither file. It
   is logged as an error and is none: sshd is saying that it ran no
   command, which is what the line is for.

3. **Restart starfixd and check the file:**

   ```sh
   systemctl restart starfixd
   ls -l /var/lib/starfix/.ssh/starfix_keys
   journalctl -u starfixd -n 5
   ```

   The file is mode 0600 and owned by `starfix`, and the log has
   `keys file written`. If starfixd cannot write it at start, it does
   not start, and the error's `fix:` says what to change.

4. **Add someone**, from any repository connected to the server, and
   have them run a command: `sfx admin principal add bob --key
   bob.pub`, then `sfx ready` as bob. `sfx admin status` shows the file,
   how many keys it holds and when it was written.

5. **Move the keys.** `control init` has already copied every key of
   `authorized_keys` into the control database
   ([Opt in](#opt-in)), so each is now in both files. Remove the lines
   from `authorized_keys` that you want `sfx admin` to govern, and keep
   the first admin's:

   - **A key whose line is still in `authorized_keys` is not kept out
     by `sfx admin key remove`.** While the control database holds the
     key, sshd lets it in by starfixd's line, so the removal closes the
     connections it had signed in. But the removal also takes that line
     out of starfixd's file, and sshd then finds the key's line of
     `authorized_keys` and lets it in again, by a forced command that
     names no fingerprint: starfixd cannot refuse that key, or close a
     connection it makes from then on. The key is revoked when its line
     of `authorized_keys` is deleted too. Meanwhile its principal can
     be disabled, and its memberships removed, which starfixd enforces
     whatever let the key in.
   - **starfixd tells you which keys those are.** It runs as the
     account whose `authorized_keys` it is, so it reads the file: it
     never writes to it, and never prints or logs a line of it. `sfx
     admin key list NAME` marks each key that is also in
     `authorized_keys`, and `sfx admin key remove` of such a key warns
     that its connections were closed, that sshd will still accept it
     by that line, and that the line must be deleted on the server. A
     line starfixd cannot read as a key is skipped, and only the first
     mebibyte of the file is read.
   - **It also counts who comes in that way.** A connection whose
     forced command named no fingerprint came in by `authorized_keys`:
     its key is not in the control database, or was removed from it.
     `sfx admin status` counts those for each project (BY HAND), and
     `sfx admin key list NAME` for NAME. `sfx admin key remove` does not
     close them.
   - **The control database's line wins for a key in both.** The key
     signs in as the principal the control database gives it, whatever
     principal its line of `authorized_keys` names, and a disabled
     principal's key has no line in starfixd's file, so it reaches its
     line of `authorized_keys` and is refused by starfixd as disabled.
     `authorized_keys` cannot override the control database; it only
     lets in a key the database does not hold.
   - **Keep one line for a server admin.** It is how a server admin's
     own key is replaced, since nobody adds a key to their own account
     with `sfx admin`. A key in `authorized_keys` keeps the
     principal its line names: `sfx admin key add` and `principal add
     --key` refuse it for anyone else, as they refuse a key another
     principal has, so nobody can take a server admin's key by adding
     it to the control database under another name. And a line you add
     for a key the control database already holds under another name
     wins: starfixd leaves that key out of its file, so that sshd
     reaches your line, says so in its log, and counts it in `sfx admin
     status` as a key held back. It takes effect at the next key change
     or within 30 seconds; until then that key is refused. starfixd
     reads the first mebibyte of the file, and takes the first line for
     a key, as sshd does.

**Settings.** `keys_file` is where starfixd writes, and
`authorized_keys` the hand-maintained file it reads; a relative path is
under the home directory of the user starfixd runs as, taken from the
account database and not from `$HOME`. `bridge` is the path of starfixd
in each line's forced command, `/usr/local/bin/starfixd` by default: an
absolute path of letters, digits and `._+-`, since it is written into a
command sshd runs. If you move `keys_file`, change sshd's
`AuthorizedKeysFile` and the unit's `ReadWritePaths` with it
([Settings](#settings)).

**When starfixd is stopped,** sshd still reads both files and accepts
their keys, and every command then fails with `starfixd is not running
on the server` until it is started. Neither file is a way to work
without the daemon, so the order of the two changes nothing then.
starfixd writes its file before it listens, and does not start if it
cannot, so a file left out of date by a stopped daemon is replaced
before anyone is answered.

**When a write fails after the start,** the change an admin made is
already recorded, and stays. The admin's command succeeds and prints a
warning that sshd may still be reading the old file; starfixd logs
`the keys file is out of date` at `error` with the cause and a fix, and
tries again every 30 seconds; and `sfx admin status` says `THE LAST
WRITE FAILED` on its `keys file` line until a write succeeds. Meanwhile
a key just added does not sign in, and a key just removed is refused by
starfixd rather than by sshd. That holds for a removed key that is also
a line of `authorized_keys`: sshd finds the old line of starfixd's file
first, so the key is refused until the file is written, and let in by
`authorized_keys` after. If the file cannot be written for long and
someone is shut out who should not be, fix what the log names, or move
the file aside on the server: sshd then reads `authorized_keys` alone,
and starfixd writes a new file when it can.

**On an image whose sshd already has a global `AuthorizedKeysCommand`**
for its provider's login service (Google Cloud's OS Login, AWS's EC2
Instance Connect, Azure's Entra ID login), nothing above needs to
change: `AuthorizedKeysFile` in the `Match` block is read whatever that
command is. Left alone, sshd would still run the provider's command for
the `starfix` account, on every sign-in, and accept a key it printed,
with no forced command. `AuthorizedKeysCommand none` in the `Match`
block is what stops that, and `TrustedUserCAKeys none` does the same for
a certificate authority the machine trusts for every account
([step 6](#6-let-the-first-principal-in)): the account's keys come from
the two files and nowhere else.

What was checked, on 12 October 2026, with OpenSSH 9.6 on Ubuntu 24.04
in the [Docker test server](deploy/docker.md), which has no systemd:

| Checked | Result |
|---|---|
| starfixd installed as [step 3](#3-install-starfixd) installs it: owned by `starfix` in `/opt/starfix/bin`, linked from `/usr/local/bin`, and no copy that root owns | Keys from the control database sign in. sshd runs no program of starfix's before authentication, so who owns the binary does not matter to it |
| A global `AuthorizedKeysCommand`, outside any `Match` block, set to a root-owned script that prints nothing, with `AuthorizedKeysCommandUser nobody` | Keys from both files sign in, with no override. sshd ran the script twice for each sign-in by `sfx`. With `AuthorizedKeysCommand none` in the `Match` block it did not run it |
| starfixd killed, sshd left running | sshd accepts a key from either file, and the client gets `starfixd is not running on the server` |
| A key in both files, with `authorized_keys` named first, as this guide had it until that day | sshd let it in by its line of `authorized_keys`, which names no fingerprint |
| A key in both files, with starfixd's file named first, as above | sshd lets it in by starfixd's line: starfixd counts no connection of that key as let in by `authorized_keys`. With the key removed from the control database, the same key signs in by `authorized_keys`, and its connection is counted so |

Checked under systemd 255, on 12 October 2026, on the Ubuntu 24.04
virtual machine of [What was run, and where](#what-was-run-and-where):

| Checked | Result |
|---|---|
| The unit of step 5 as written, on a server of many projects, without step 1's `ReadWritePaths` | starfixd does not start: `write the keys file …: read-only file system`, and its `fix:` names `ReadWritePaths=/var/lib/starfix/.ssh` |
| The same with step 1's drop-in | starfixd starts and writes the file, mode 0600, owned by `starfix`. `ProtectHome=yes` does not get in the way |
| Step 2 on stock Ubuntu's sshd, which listens through `ssh.socket` | `sshd -t` passes and `systemctl try-reload-or-restart ssh` applies it. A key the control database alone holds signs in with no restart of anything; removing it, or disabling its principal, locks it out |
| A global setting in a later drop-in file (`PasswordAuthentication no` in Ubuntu's `60-cloudimg-settings.conf`) | Still applies to every account: the `Match` block of `00-starfix.conf` ends with its file |

Not checked:

- **A provider's own machine.** The stand-in above shows that a global
  command that prints nothing does not get in the way. It does not show
  what OS Login, EC2 Instance Connect or the Entra ID extension change
  besides that one setting, or what their commands print for an account
  named `starfix`.
- **SELinux**, which is not supported yet
  ([What a server needs](#what-a-server-needs)). From general knowledge
  of it, unverified: sshd reads keys only from files labeled for it,
  and a home directory outside `/home`, such as `/var/lib/starfix`,
  does not get that label on its `.ssh` by default. That is as true of
  `authorized_keys` as of the keys file.

### Managing access

Admins manage a server of many projects with `sfx admin`, from any
repository connected to it ([commands](cli.md#agents-and-maintenance)).
A command acts on that repository's project unless `--project` names
another, by UUID or by name. Nothing here is an MCP tool: an agent
cannot run it, and one whose principal is refused is told to ask a
person.

| Command | Who may |
|---|---|
| `sfx admin project add NAME`, `archive`, `activate` | Server admins |
| `sfx admin project list` | Anyone the server lets in: a server admin sees every project and its database, anyone else the active projects they belong to |
| `sfx admin principal add NAME` | Server admins; a project admin, into a project they administer |
| `sfx admin principal add NAME --no-project`, `principal list` | Server admins |
| `sfx admin key list --added-by NAME`, `principal list --created-by NAME`, `member list --added-by NAME` | Server admins, for all of it; a project admin, for the keys and principals of their projects' members and for their projects' memberships |
| `sfx admin principal disable NAME`, `enable NAME` | Server admins; a project admin, for a principal within their reach |
| `sfx admin member add NAME`, `remove NAME` | Server admins; the project's admins |
| `sfx admin member list` | Server admins; the project's admins |
| `sfx admin principal add NAME --key FILE`, `key add NAME FILE`, `key remove FINGERPRINT` | Server admins; a project admin, for a principal within their reach. Never for one's own account |
| `sfx admin key list NAME` | Server admins; an admin of an active project NAME belongs to, with or without reach over NAME; NAME themselves |
| `sfx admin status` | Server admins, for the server and every project; a project admin, for the active projects they administer ([Watching a server](#watching-a-server)) |

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
  starfixd does not judge. So name only admins who can sign in: with
  `admins: [dana, spare]` and no key for `spare`, dana can be disabled
  and nobody is then left to sign in as a server admin.
- **Naming an admin trusts whoever set that name up.** A project admin
  may create a principal of any free name, with a key, and may add keys
  to the members of their own projects. If you later put such a name
  under `admins:`, those keys are a server admin's. starfixd warns at
  start, in its log, of each configured admin whose row, or one of whose
  keys, was put there by someone who is neither a server admin nor
  `control init`, and names the key. Check it with its owner, or remove
  it, before you rely on the name.
- **Disabling an account does not undo what it did.** The keys it added
  to others, the principals it created and the memberships it added all
  keep working. `principal disable` says how many of each there are and
  names the three commands that list them: `sfx admin key list
  --added-by NAME`, `sfx admin principal list --created-by NAME` and
  `sfx admin member list --added-by NAME`. A server admin sees all of
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
  with rights over it added a key that is not yours: `sfx whoami` shows
  the key you are signed in with, `sfx admin key list YOU` shows every
  key of yours with who added it and when, and `sfx admin key remove
  FINGERPRINT` removes one. Two limits keep you from locking yourself
  out by accident. The key your connection signed in with is refused,
  by fingerprint: sign in with another of your keys to remove it, or
  ask an admin. And when your connection came in by a line of
  `authorized_keys`, which names no key, starfixd cannot tell which key
  you used: it removes the key and warns that it could not check. That
  connection's key is not one the control database can take away in any
  case. Removing the last key the control database holds for you is
  allowed, a server admin's included: you keep the way in you are
  using. Someone else's key still needs reach.
- **Keys.** `key add NAME FILE` takes a public key file, such as
  `~/.ssh/id_ed25519.pub`, or `-` for standard input. `sfx` reads the
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
  `starfixd` with the same reason. An archive does not wait: it opens a
  closed store to end the claims, which are then recorded under
  `starfixd` and not counted in what the command says it released.
- **A project admin** is a member whose role in a project is `admin`
  (`sfx admin member add NAME --role admin`).
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
  On a terminal `sfx` then asks, and adds the member on `y`. Without a
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
- **A new person** takes one command once sshd reads starfixd's keys file
  ([Keys from the store](#keys-from-the-store)): `sfx admin principal add
  NAME --key FILE`, with the `.pub` file they sent you. `--key` may be
  given more than once. Someone the server already knows joins another
  project with `member add NAME`.
- **A new project.** `sfx admin project add NAME` records the project,
  creates its database under the next free name of `project_dbs`, applies
  the schema and marks it active, then prints the `.starfix.yaml` for its
  repository. If a step fails, run the same command again: it finishes
  the same project. If the name was never granted, the refusal gives the
  `GRANT` statement to run as Dolt's root, and the command then resumes.
  The project is served at once to server admins; add its members with
  `sfx admin member add NAME --project NAME`.
- **Archiving** keeps the project's database, members and name, and
  serves it to no one: its connections are closed, its claims end, and
  its store closes with them, after one more sweep for a claim that a
  request in flight took meanwhile. A starfixd that stopped before it
  had released them ends them when it starts, before it listens. The
  server's last active project cannot be archived, since
  `sfx admin` needs a project to connect to. `activate` opens the
  database, which applies any migration it missed while archived, and
  serves it again.
- **Everything is on the record** in the control database's event log,
  under the principal who ran the command.

One case in the above is slower than "at once". A project whose store is
closed has nothing running in it: when its member is removed or disabled,
the claims stay in its database until the store next opens. Nobody can
use them meanwhile, and they end before the first request is answered.

An archived project's store never opens for a connection, so starfixd
does not leave its claims to that. The archive opens a closed store to
end them, and at every start, before it listens, starfixd asks each
archived project's database whether it holds a claim, and opens the ones
that do, ends every claim, recorded under `starfixd` for `project
archived`, and closes them. A project with nothing to end costs that one
read, a fraction of a millisecond, and nothing is written. It does not
migrate: a database that an older starfixd last migrated is left as it
is, with a line in the log at `info`, and a claim in it ends when the
project is activated, as before. One that cannot be opened is logged at
`error` and does not stop the server or the other projects. Nothing
runs for archived projects on the 30-second tick: a running starfixd
leaves a claim in one only if the release itself failed, which it logs,
and the next start or the activation ends it.

### Watching a server

`sfx admin status` reports what starfixd knows of itself and of its
projects now. A server admin sees the server and every project; a project
admin sees the active projects they administer and nothing of the
server; anyone else is refused. It reads memory, plus one count on an
index for each open project listed, opens no store and writes nothing.
It does not wait for a database that does not answer
([When Dolt does not answer](#when-dolt-does-not-answer)).

```text
starfixd v0.5.0, protocol 5 to 6, up 3d 4h
stores 2 of 64, connections 14 of 1024
keys file /var/lib/starfix/.ssh/starfix_keys: 31 keys, written 2026-10-11 09:12 UTC
starfixd's memory 42 MiB in use, 78 MiB from the system; 112 goroutines
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
| keys file | The file starfixd writes for sshd ([Keys from the store](#keys-from-the-store)): its path, how many keys starfixd last put in it, and when it was last written. `THE LAST WRITE FAILED` means sshd may be reading an older list than the control database holds; starfixd's log says why |
| `clock:` | Shown once the clock has been set while starfixd ran: how many times, in which direction, and the last step's size and time. Each is a `clock stepped` line in the log ([The server's clock](#the-servers-clock)) |
| `CLOCK BEHIND HISTORY` | The clock is behind the latest event of the database named, by how much and since when. starfixd's time runs on from the history at half speed until the clock has caught up. With `clock_behind: stop` it adds that starfixd will not start again until the clock is corrected: check `timedatectl` on the server before any restart |
| starfixd's memory | The Go heap in use and all the memory starfixd has from the operating system (`HeapAlloc` and `Sys`). Not Dolt's |
| `Dolt`, `Dolt's collector:` | With `telemetry: dolt_metrics_url:` set: what starfixd last read from Dolt's own metrics, and how often Dolt's garbage collector ran against starfixd's writes ([Dolt's figures](#dolts-figures)). `Dolt: unknown` means the last read failed. Without the setting neither line is shown |
| projects, principals | The control database's rows, by state |
| STORE | Whether the project's store is open. The figures after it exist only while it is, and start from zero each time it opens |
| CONNS, BY HAND | Connections past their handshake, and those among them that named no key of the store, which sshd let in by the hand-maintained `authorized_keys` |
| SESSIONS | Sessions seen in the last five minutes, as `sfx who` lists them. `unknown` when the database did not give the count ([below](#when-dolt-does-not-answer)) |
| WRITES/MIN | Transactions that changed something and committed in the last 60 seconds. A refused request is not one; a lease renewal is |
| P50, P99 | The median and 99th percentile of the latest 256 writes' latency, however old they are: from the request reaching the project's store to its commit, the wait for the one writer included |
| LAST USED | When the project last answered a request, or was opened |

`-n` and `--after NAME` page the projects as the admin lists do, 200 at a
time. `--json` gives the same figures, with latency in microseconds.

On the server itself, [`starfixd status`](#starfixd-status) prints the
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
sessions: unknown for 2 projects (database did not answer in 2s); the rest is what starfixd holds in memory. Check Dolt on the server (docs/server.md, Troubleshooting)
```

- **Everything else is still there**, since starfixd holds it in memory:
  the server's lines, the stores and connections, the clock, the
  writes and their latency as they last were, and
  [Dolt's line](#dolts-figures), which says `unknown` by itself once a
  read of Dolt's metrics has failed.
- **`--json`** leaves `sessions` out for such a project and gives
  `sessions_unknown`, the cause as text: `database did not answer in 2s`,
  or `the read failed; see starfixd's log` for any other failure, which
  the log has as `count the sessions seen, for status`. A project whose
  store is closed has neither.
- **One project's database that does not answer** costs that project's
  figure and no other's.
- **`sfx admin status` answers in about 2 seconds too, from a repository
  whose project's store is open.** `sfx` connects anew for each command,
  and a new connection is welcomed without waiting on Dolt: its session
  is noted for `sfx who` beside the wait for its first request. The
  report counts its asker's session, so it waits for that note, but
  within the same 2 seconds as its own reads: when the note does not
  come, the sessions are `unknown` and the rest is there.
- **From a repository whose project's store is closed, it is refused
  in about 2 seconds:** `the server's database does not answer, so
  project "…" was not opened`, with `starfixd status` on the server as
  what to do. A connection is to a project, opening the project's store
  waits on Dolt for up to a minute, and the report itself opens no
  store; so before a store is opened for a connection, Dolt is given 2
  seconds to answer a ping. Run `sfx admin status` from the repository
  of a project in use, or `starfixd status` on the server.
- **Every other command waits for Dolt**, for the request timeout of a
  minute and no longer, and then ends with `the server timed out on …`.
  It waits for its session to be noted first, within that same minute,
  so nothing reads or writes a project for a session `sfx who` cannot
  list. A session whose note failed is noted by its next request that
  Dolt answers.
- **`starfixd status` on the server** answers in about 2 seconds
  whichever stores are open, and needs no admin key.

#### Dolt's figures

Dolt is another process under another user, so what the report says of
it is what Dolt says of itself: starfixd reads Dolt's own metrics
endpoint ([step 2](#2-run-dolt-under-its-own-account)) every 15 seconds
and shows the last answer. A server admin sees the two lines; a project
admin does not, since they are the server's and not a project's.

| Figure | From Dolt's | Why it is there |
|---|---|---|
| Version | `dss_dolt_version` | Which Dolt is running, which after an upgrade of Dolt is not always the one on the disk |
| `up` | `process_start_time_seconds` | When Dolt last restarted. Its memory grows from then ([Dolt's memory](#dolts-memory)), and so does what a restart would give back |
| connections | `dss_concurrent_connections` | Clients connected to Dolt now: starfixd's own, a few for each open store, and anyone else's, such as a `dolt sql` session or a dump |
| `MiB resident` | `process_resident_memory_bytes` | The memory of Dolt's process, which is what the machine must hold. It is the figure `ps` gives, not a container's or a unit's total, which adds the page cache of Dolt's files |
| `memory limit` | `go_gc_gomemlimit_bytes` | The Go memory limit Dolt runs under (`GOMEMLIMIT`), or `no memory limit` |
| `Dolt's collector:` | `go_gc_duration_seconds_count`, and starfixd's own count of writes | The cycles of Dolt's garbage collector, the writes starfixd's project stores committed, and the first for each 100 of the second, over the last ten minutes or since either started. It is what tells whether a memory limit has room ([Dolt's memory](#dolts-memory)) |

- **`Dolt: unknown`** means the latest read failed: nothing answers
  there, the
  port or the path is wrong, the answer took more than 5 seconds, was
  more than 1 MiB or was not Dolt's metrics, or it was a redirect, which
  starfixd does not follow. The line gives the last read that worked,
  and starfixd's log has `could not read Dolt's metrics` with the cause:
  once when it starts, then every 10 minutes while it lasts, and
  `reading Dolt's metrics again` when it ends. Nothing else changes:
  starfixd serves as before, and `/readyz` still answers from its own
  query of Dolt, not from this.
- **A figure Dolt did not give is `unknown` by itself**, as a newer Dolt
  that renamed a metric would make it.
- **The collector line needs two reads** of the same Dolt process, so it
  is missing for the first 15 seconds after starfixd or Dolt starts, and
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
- **starfixd does not export Dolt's metrics again.** Have your collector
  scrape `http://127.0.0.1:11228/metrics` beside starfixd's
  ([Observability](#observability)).

**On a server set up before this**, turn Dolt's endpoint on and tell
starfixd where it is. Restarting Dolt restarts starfixd with it and
drops every connection for a few seconds, so pick a quiet moment:

```sh
printf 'metrics:\n  host: 127.0.0.1\n  port: 11228\n  labels: {}\n' >> /etc/dolt/config.yaml
systemctl restart dolt
curl -s --retry 5 --retry-connrefused http://127.0.0.1:11228/metrics | grep -c '^dss_'
printf 'telemetry:\n  dolt_metrics_url: http://127.0.0.1:11228/metrics\n' >> /etc/starfix/starfixd.yaml
systemctl restart starfixd
sleep 20
sudo -u starfix -H starfixd status | grep '^Dolt'
```

- If `/etc/starfix/starfixd.yaml` already has a `telemetry:` block, add
  the `dolt_metrics_url:` line under it with an editor instead of the
  second `printf`: a key given twice stops starfixd at start.
- The `curl` line prints 14 at Dolt 2.4.2, and the last line prints the
  two lines above, the second only once starfixd has read twice.
- A URL that is not `http://` on a loopback address stops starfixd at
  start with a `fix:`. The endpoint has no authentication, and starfixd
  will not read it from another machine.

A database's size on disk is not in the report: Dolt offers its
least-privileged account no table or function
for it, and the files belong to Dolt's user. Read it with `du -sh
/var/lib/dolt/data/starfix/*` as root or `dolt`
([Storage growth](#storage-growth)).

Each project's store is opened by its first connection and closed after
`store_idle` without one ([Limits](#limits)). Closing gives back the
store's connections to Dolt. It does not give back Dolt's memory
([Dolt's memory](#dolts-memory)), so size the machine by what all its
projects write, not by how many are open. A store that reopens ends the claims
that expired while it was closed, and those of anyone who lost their
access meanwhile, before it serves its first request.

## Prices

`sfx cost`, `show` and `digest` price tokens at each model's list rates,
which an admin records with `sfx admin prices set`. Rates are US dollars
per million tokens, from an effective date (midnight UTC) or time; the
model is spelled exactly as the harness reports it, which `sfx cost --by
model` shows. For example, with invented rates for an invented model:

```sh
sfx admin prices set example-large --from 2026-01-01 \
  --input 4 --output 20 --cache-write 5 --cache-write-1h 8 --cache-read 0.4
sfx admin prices
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
whatever its tokens would cost at list price. `sfx cost` shows each
group's share of the plans' fees as its **amortized cost**, beside the
list-price equivalent, once an admin records the plans with `sfx admin
plans set`. For example, with an invented plan:

```sh
sfx admin plans set team --from 2026-09 --fee 25 --seats 3 \
  --principal alice --principal bob
sfx admin plans
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

starfixd relies on the server's clock and does not trust it to be right
([design §14, decision 37](design/starfix.md#14-decisions)). A lease
runs out by it, every event is dated by it, and windows such as the
daily cap on usage records are counted back from it. A client's own
clock decides nothing here, with one bounded exception: the time a
usage record claims, which may be at most `clock_client_tolerance` (5
minutes) past the server's.

What starfixd does about a clock that is wrong or moves:

| The clock | starfixd |
|---|---|
| Is set back while starfixd runs | Its time does not go back. It runs on from where it was at half speed until the clock has caught up, which takes twice as long as the clock was set back: an hour takes two. Events stay in order and leases still run out, at half speed meanwhile, so a lease lasts longer by at most how far the clock was set back. It logs `clock stepped` and `clock behind history`, and keeps serving |
| Jumps forward while starfixd runs (it was set, or the machine was suspended and resumed) | Every lease that was live is extended by the jump, so no claim ends because the clock moved; each database records one `clock.step` event. It logs `clock stepped` and, for each database, `clock step settled` |
| Is behind the server's own history when starfixd starts | By default starfixd **refuses to start** ([below](#when-the-clock-is-behind-the-servers-history)) |
| Was ahead and has been corrected | That is the same thing, seen later: the events written while it ran ahead are now in its future, so the clock is behind its history |

A database follows a jump forward only once it has settled it: with
its next write, which is the reaper's next tick at the latest, or, for a
project whose store was closed for idleness, when the project is next
used. Until then its time runs on as if the clock had not moved. A lease
that runs out meanwhile has had all of its time and ends as it would
have without the jump; every lease still live is extended. The
`clock.step` event's time is the database's time when it settled, and
its `claims` the leases it extended.

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

A database starfixd opens for the first time since it started (a
project whose database could not be opened at start, or one added or
restored since) follows every jump made since the start in the same
way, with one `clock.step` event for all of them: whatever claims it
holds were made before starfixd started, so each of those jumps took
time from their leases. A database with no history has nothing to
settle and records nothing. If something other than this starfixd wrote
to the database after a jump, as a restore or `starfixd import-bd` may
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
line is the start of the process: a jump made while starfixd was down is
an outage of that length, and one made at any time after it started is
a step, for every database. The same holds for a virtual machine paused in a way that leaves its
monotonic clock running: starfixd sees time pass, not a step.

### Keep the clock synchronized

- **Run a time service, and check it.** Ubuntu 24.04 ships
  `systemd-timesyncd`, enabled, synchronizing to `ntp.ubuntu.com`.
  `timedatectl` must say `System clock synchronized: yes`, and
  `timedatectl timesync-status` names the source and the offset.
- **Use several sources, authenticated where they offer it.** Ordinary
  NTP is not authenticated: whoever can answer for the time source can
  set the server's clock. Network Time Security (NTS) authenticates the
  source. `systemd-timesyncd` asks one server at a time and does not
  speak NTS (general knowledge, not checked here). `chrony`, version 4.5
  in Ubuntu 24.04's main archive and built with NTS, combines several
  sources and can use NTS ones; its default configuration there lists
  pools without NTS, and installing it removes `systemd-timesyncd`.
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
  15 minutes and `sfx mcp` renews it every minute; a person's own
  commands take 8 hours. Steps that stay under `clock_step_tolerance`
  are not compensated, so they can cost a lease at most 5 seconds, and
  a second an hour after that, against a minute between renewals and a
  lease that is rewritten when half of it is left: two orders of
  magnitude of room. Raising the step tolerance
  toward the renewal interval gives that room away.

### Start starfixd after the clock is set

The unit in [step 5](#5-run-starfixd-under-systemd) has
`Wants=time-sync.target` and `After=time-sync.target`. What that waits
for depends on the machine. Each of these was run on the virtual
machine of [What was run, and where](#what-was-run-and-where), with a
cold boot each time:

| Machine | `time-sync.target` reached | starfixd serving | Boot |
|---|---|---|---|
| Stock Ubuntu 24.04 | 1.7 s after the kernel started, 37 microseconds after `time-set.target`, before the network was up or any time server heard from | 9.7 s (its first start loses the race with Dolt, as before) | 4.6 s |
| `systemd-time-wait-sync` enabled, a time server reachable | 33.4 s, as `systemd-timesyncd` logged `Initial clock synchronization` | 33.5 s | 33.7 s |
| The same, no time server answering | 91.5 s, when the wait unit's 90 second timeout failed it (`Failed with result 'timeout'`) | 91.6 s | 1 min 32 s |

- **On stock Ubuntu 24.04 the two lines wait for nothing.**
  `time-sync.target` follows `time-set.target`, which
  `systemd-timesyncd` reaches as it starts. The unit that waits for a
  synchronized clock, `systemd-time-wait-sync.service`, ships disabled.
  The lines are in the unit so that it waits once something on the
  machine provides the target.
- **Waiting for synchronization is optional, and needs a timeout.**

  ```sh
  mkdir -p /etc/systemd/system/systemd-time-wait-sync.service.d
  printf '[Service]\nTimeoutStartSec=90\n' > /etc/systemd/system/systemd-time-wait-sync.service.d/timeout.conf
  systemctl daemon-reload
  systemctl enable systemd-time-wait-sync.service
  ```

  **Never enable it without the drop-in.** As shipped it has
  `TimeoutStartSec=infinity`. With no time server answering, starfixd's
  start was then still `waiting` after 130 seconds, and would have
  stayed so; so would everything else ordered after the target, which
  on Ubuntu is every timer and `cloud-final.service`. With the drop-in
  the wait fails after 90 seconds, the target is reached all the same,
  and starfixd starts with the clock as it is.
- **What it costs, and when it is worth it.** With a time server
  reachable it added 24 seconds to this machine's boot, since
  `systemd-timesyncd`'s first try comes before the network is up and
  its next is 30 seconds later; with none it adds the 90 seconds. It
  delays the timers too. It is worth that on a machine whose clock at
  boot cannot be trusted: one with no battery-backed real-time clock,
  or one that has come up with a wrong date before. On a virtual
  machine whose hypervisor supplies the time at boot, as this one's
  does (the kernel logged `rtc-efi: setting system clock` 0.05 s into
  every boot, and it was right each time), it buys little.
  `chrony` ships `chrony-wait.service` for the same purpose, with a
  timeout of 180 seconds (not run).

Why the order matters at all: a daemon that starts with the clock
**ahead** writes events in the future, and once the clock is corrected
it is behind its own history, which the next start refuses. A clock that
is behind at start is caught by the check below, whatever the unit
waited for.

What each boot does, with the defaults:

| Boot | Result |
|---|---|
| Normal | The clock is ahead of the last event written before the shutdown. starfixd starts. Run: three cold boots and one after the hypervisor's process died, which is a power loss to the guest |
| The real-time clock is a few seconds slow | The clock is behind the history by less than the time the reboot took, or by seconds: within `clock_behind_tolerance`. starfixd starts; if the time service then steps the clock forward by more than 5 seconds, leases are extended by the step. Not run: this machine's clock at boot was always right |
| The machine has no working real-time clock (a dead battery) | `systemd-timesyncd` advances the clock to the time it last saved, then synchronizes if it can. Imitated, not booted: with the clock set back a day and `systemd-timesyncd` started, it logged `System clock time unset or jumped backwards, restored from recorded timestamp`, the clock read the saved time, and starfixd started. It saves that time every 60 to 70 seconds, synchronized or not (measured, with the poll interval at 4 minutes), and when it stops, so after a power loss the clock comes up at most about 70 seconds behind the history. That is why the tolerance is 2 minutes and not 1 |
| A virtual machine restored from an older snapshot | The disk's history is as old as the snapshot and the clock is the present: not behind. starfixd starts. Not run |
| A database restored from a backup written by a machine whose clock ran ahead | The clock is behind the history. starfixd refuses to start and says how to start once anyway. Run, as the clock set forward under a running daemon and then put back |
| The clock is hours or years behind (a reset with no saved time, a wrong date set by hand) | starfixd refuses to start. Run with the clock set back one and two hours |

### When the clock is behind the server's history

At start, starfixd compares the clock with the latest event of every
database it will write to: the control database's and each active
project's, which it opens once at start anyway, so the check costs no
extra query. A database with no events has nothing to be behind. If the
clock is behind any by more than `clock_behind_tolerance`:

- **`clock_behind: stop`**, the default: starfixd does not start. It
  prints the refusal and exits with status 75, a seventh of a second
  after systemd started it, long before `TimeoutStartSec`:

  ```text
  # systemctl status starfixd
  × starfixd.service - starfix server
       Active: failed (Result: exit-code) since Mon 2026-10-12 17:26:10 UTC; 3ms ago
      Process: 5116 ExecStart=/opt/starfix/bin/starfixd serve (code=exited, status=75)
     Main PID: 5116 (code=exited, status=75)

  starfixd[5116]: starfixd: not starting: the server's clock is behind its own history (clock_behind: stop)
  starfixd[5116]: starfixd:   the clock reads   2026-10-12T17:26:10Z
  starfixd[5116]: starfixd:   the latest event  2026-10-12T17:35:32Z, 9m21s later
  starfixd[5116]: starfixd:   is in project 5d6b7c84-0ce6-4b09-abcf-e0a10c8bcc78 (database starfix), and 1 more database is ahead of the clock too
  starfixd[5116]: starfixd: fix: if the clock is wrong, correct it (`timedatectl` must say synchronized: yes), then `systemctl start starfixd`
  starfixd[5116]: starfixd: fix: if the clock is right and the history is ahead, start once with: starfixd serve --accept-clock-behind 2026-10-12T17:35:34Z
  starfixd[5116]: starfixd: fix: or set clock_behind: warn in the config file, to start with a warning (docs/server.md, The server's clock)
  systemd[1]: starfixd.service: Main process exited, code=exited, status=75/TEMPFAIL
  systemd[1]: starfixd.service: Failed with result 'exit-code'.
  systemd[1]: Failed to start starfixd.service - starfix server.
  ```

  The unit's `RestartPreventExitStatus=75` keeps systemd from starting
  it again, since a restart cannot correct a clock: 20 seconds later the
  journal held that one start and the unit was still `failed`, with
  `NRestarts=0`. Without the line the same refusal was retried every
  five seconds, 7 starts in 32 seconds, each adding its lines to the
  journal. systemd names status 75 `TEMPFAIL`, which is what it is:
  nothing is wrong with starfixd or its data. Every other failure to
  start still exits 1 and is retried, as before. `sfx` answers
  `starfixd is not running on the server` meanwhile.
- **Correct the clock** and `systemctl start starfixd`.
- **Or, when the history is what is wrong, start once anyway,** with
  the flag the refusal printed, in a drop-in that you then remove:

  ```sh
  mkdir -p /etc/systemd/system/starfixd.service.d
  printf '[Service]\nExecStart=\nExecStart=/opt/starfix/bin/starfixd serve --accept-clock-behind 2026-10-12T17:35:34Z\n' > /etc/systemd/system/starfixd.service.d/accept-clock.conf
  systemctl daemon-reload
  systemctl start starfixd
  rm /etc/systemd/system/starfixd.service.d/accept-clock.conf
  systemctl daemon-reload
  ```

  The daemon keeps the unit's confinement, its
  [AppArmor profile](#confine-starfixd-with-apparmor) included, since
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
starfixd's time runs on from the latest event, not from the clock, so no
event is dated before its predecessor and no window reopens. What it
costs is that timestamps are ahead of the clock until the clock catches
up, and leases run at half speed meanwhile.

**A clock set back under a running daemon never stops it,** under
either setting: a daemon that exits when its clock moves would give
whoever can set the clock, or answer for its time source, a way to take
the server down. It is reported instead, and with `clock_behind: stop`
the report says `THIS SERVER WILL NOT START AGAIN until the clock is
corrected`. `sfx admin status`, `starfixd status` and
`starfixd upgrade --check` show the same, so look there before a
planned restart. Run on the same machine with the clock set back seven
minutes: an event written two seconds after another was dated one
second after it, `starfix_clock_behind_seconds` fell from 390 to 0 by
30 a minute, the `error` line came again at ten minutes, `clock caught
up with history` at fourteen, and through sixteen minutes, eight of the
watchdog's intervals, the unit stayed `active` with no restart.

**`/readyz` stays ready through it,** for the same reason: the daemon
is serving correctly, and whatever watches readiness may take an
unready daemon out of service, which is the outage a moved clock must
not buy. A monitor sees it in the metrics instead
([Observability](#observability)): alert when
`starfix_clock_behind_seconds` is above zero for longer than a boot
takes, and on any increase of `starfix_clock_steps_total`. The gauge
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
framework reported the machine in error a minute later). How starfixd
fares across a pause depends on whether the guest's monotonic clock
stops with it: if it does, the daemon sees a jump forward and extends
its leases, as above; if it keeps running, the daemon sees time pass
and ends the claims whose leases ran out, as after an outage of that
length.

## Settings

Settings come from flags, then `STARFIXD_*` environment variables, then
the config file (`/etc/starfix/starfixd.yaml`, or `--config FILE`, or
`STARFIXD_CONFIG`), then defaults.

| Key | Holds | Default | Flag and variable |
|---|---|---|---|
| `dsn` | Dolt's address and account, in [go-sql-driver/mysql](https://github.com/go-sql-driver/mysql#dsn-data-source-name) form | required | `--dsn`, `STARFIXD_DSN` |
| `project` | The project's UUID, in lowercase. A client naming another project is refused. With `control_db`: the project `control init` adopts, and the one `import-bd`, `export-bd` and the health check of `upgrade` use by default | required | `--project`, `STARFIXD_PROJECT` |
| `control_db` | The name of the control database, on the same Dolt server and account as `dsn`. Setting it makes this a server of [many projects](#serving-many-projects). A lowercase letter, then lowercase letters, digits and underscores; not the database in `dsn` | none: one project | `STARFIXD_CONTROL_DB` |
| `project_dbs` | With `control_db`: the block of database names granted for new projects, as `prefix` and `count` ([Opt in](#opt-in), step 4). The names are `prefix` and three digits, `001` to `count` (at most 999) | none: no project can be added | |
| `keys_file` | With `control_db`: the file starfixd writes the control database's keys to, for sshd ([Keys from the store](#keys-from-the-store)). A relative path is under the home directory of the user starfixd runs as | `.ssh/starfix_keys` | `STARFIXD_KEYS_FILE` |
| `authorized_keys` | With `control_db`: the hand-maintained file sshd reads beside it. starfixd only reads it, to say which keys are in both. Never the same file as `keys_file` | `.ssh/authorized_keys` | `STARFIXD_AUTHORIZED_KEYS` |
| `bridge` | With `control_db`: the path of starfixd in the forced command of each line of `keys_file`. Absolute, of letters, digits and `._+-` | `/usr/local/bin/starfixd` | `STARFIXD_BRIDGE` |
| `socket` | The daemon's unix socket | `/run/starfix/starfixd.sock` | `--socket`, `STARFIXD_SOCKET` |
| `prefix` | The prefix of new issue ids | `sf` | `--prefix` |
| `admins` | [Admins](#admins) | none | `STARFIXD_ADMINS` |
| `systemd_unit` | The unit `starfixd upgrade` restarts and health-checks | none | |
| `releases` | Where `starfixd upgrade` reads releases from: the `https://` URL of a mirror, or the absolute path of a directory ([Upgrades](#upgrades)) | GitHub | `STARFIX_RELEASES`, the variable `sfx` and `install.sh` read |
| `log_level` | `debug`, `info`, `warn` or `error` ([Logs](#logs)) | `info` | `--log-level`, `STARFIXD_LOG_LEVEL` |
| `log_format` | `text` (key=value) or `json`, for a log shipper | `text` | `--log-format`, `STARFIXD_LOG_FORMAT` |
| `latest` | The newest starfix release, set by hand. Clients' `prime` notes when this server is older | none | |
| `account` | The project's account: what an issue's time and tokens report against when neither it nor an ancestor sets one ([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)). A code name: lowercase letters, digits and inner hyphens | `internal` | |
| `clock_behind` | What `serve` does when it finds the clock behind the server's own history by more than `clock_behind_tolerance`: `stop` refuses to start, `warn` starts with a warning that repeats ([The server's clock](#the-servers-clock)). Any other value is refused | `stop` | `STARFIXD_CLOCK_BEHIND` |
| `clock_behind_tolerance` | How far behind its history the clock may be at start before `clock_behind` applies. 1s to 24h. Raise it only for a machine known to boot with a clock further off than this and no time service to correct it; lower it if starfixd always starts after synchronization | `2m` | |
| `clock_step_tolerance` | How far elapsed wall-clock time and elapsed monotonic time may differ between two readings before starfixd calls it a step of the clock. 1s to 10m. Lower it, to 1s at the least, to be told of smaller corrections; raise it only if a time service on the machine is known to step by more in normal running | `5s` | |
| `clock_client_tolerance` | How far past the server's clock a time a client supplies may be: a usage record's. 1s to 24h. Raise it if people's machines are known to run ahead and their usage records are being dropped | `5m` | |
| `limits` | [Limits](#limits) | defaults | |
| `telemetry.listen` | The loopback address, `HOST:PORT`, of the listener for `/metrics`, `/healthz` and `/readyz` ([Observability](#observability)). An address that is not loopback is refused | none: no listener | `STARFIXD_TELEMETRY_LISTEN` |
| `telemetry.per_project` | Whether metrics name the project. `false` leaves it off every instrument | `true` | |
| `telemetry.principals` | How a person appears on a span and on a log record exported over OTLP ([Traces](#traces), People): `pseudonym`, a stable stand-in; `name`; or `omit`, nothing. It does not change the log on stderr, nor the audit stream there. Any other value is refused | `pseudonym` | |
| `telemetry.audit_log` | Whether each access change in the control database's log is also written as a log line with `audit=true` ([The audit stream](#the-audit-stream)). `false` writes none; the event log is the record either way | `true` | |
| `telemetry.principals_key` | The file that holds the key the stand-ins are made with. starfixd makes it, mode 0600, when it exports traces with stand-ins. A relative path is under the home directory of the user starfixd runs as. A file that cannot be read or made does not stop starfixd, whether the path is this default or one you set: its spans then say nothing about who, and its log and status say why ([Traces](#traces), People) | `telemetry/principals.key` | |
| `telemetry.dolt_metrics_url` | The URL of Dolt's own metrics endpoint, which starfixd reads every 15 seconds for the status report ([Dolt's figures](#dolts-figures)): `http://`, a loopback address or a name whose every address is loopback, a port and a path, such as `http://127.0.0.1:11228/metrics`. Anything else is refused at start | none: nothing is read, and the report says nothing of Dolt | `STARFIXD_TELEMETRY_DOLT_METRICS_URL` |

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
| `notices_per_minute` | 10 | Mentions, assignments and handoffs one principal can send another a minute; the rest are not delivered. Lost claims always are |
| `inbox_keep` | `30d` | How long a read inbox item is kept |
| `idem_window` | `7d` | How long an idempotency key gives back its request's result: a write a client sends again with the same key within it, as an outbox does after an outage, applies once and gets the first answer. A key used longer ago is refused with `stale` (`conflict` to a client of protocol 5), saying its request was applied then, and nothing is written; the same key with other arguments is `conflict`. It is how long a write can wait on a person's machine and still be delivered without them. From `1h` to `365d`. Keys are kept on the events they stamped whatever this is, so it bounds no table: a key is never applied twice, however late it comes back |
| `conns` | 1024 | Connections past the handshake, refused with `busy` beyond it. Twice this caps the sockets the daemon holds in all; at that cap a new connection is let in and the one that has waited longest without a hello is closed |
| `conns_per_principal` | 64 | One principal's connections: one person's sessions on several machines, and a few unattended runs on their key. Agents that run unattended in numbers should have principals of their own, and meet `conns` instead. Each connection costs the server about 5 MiB, most of it the bridge and sshd outside starfixd ([What a server needs](#what-a-server-needs)): 64 connections about 320 MiB, and `conns`' 1,024 about 5 GiB (550 MiB and 8.6 GiB with transparent huge pages `always`). Past it, every new connection of the principal is refused with `busy`, `alice is at its connection limit (64)`, until one of its sessions closes: an `sfx` command fails, saying to close some sessions or have this limit raised; an `sfx mcp` session that is refused cannot reach the server at all, tells its agent so, and tries again at most every 5 seconds. Its claims lapse if it stays refused for their 15-minute lease, and its writes are not queued |
| `handshakes_per_principal` | 16 | One principal's connections that sshd has let in and that have not yet sent their hello, on the whole server. A connection has 10 seconds to send it. Past the cap the next is refused with `busy` at once, so one key holder cannot use up the sockets everyone else connects through. The upgrade's health probe is not counted |
| `idle_timeout` | `10m` | A connection that sends nothing this long is closed with a note, unless it watches its inbox (`sfx mcp`, `sfx watch` and `sfx tui` do) |
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
| `open_stores` | 64 | With `control_db`: project stores open at once. Each holds one connection to Dolt when idle and up to nine under load. At the cap, a connection to another project closes the store that has been without connections longest; when every open store has one, it is refused with `busy` |
| `store_idle` | `10m` | With `control_db`: how long a project's store stays open with no connection. Closing it gives back its connections to Dolt, not Dolt's memory |
| `projects` | 500 | With `control_db`: projects on the server, archived ones included |
| `principals` | 5000 | With `control_db`: principals in the control database, disabled ones included |
| `principals_per_creator` | 500 | With `control_db`: principals one principal who is no server admin may create, disabled ones included. No command deletes a principal and names are the whole server's, so this keeps one project admin from using up `principals`. Past it `principal add` is refused with `invalid`, and its fix says to ask a server admin, who has no such cap, or to raise the limit |
| `keys_per_principal` | 20 | With `control_db`: public keys of one principal |
| `members_per_project` | 1000 | With `control_db`: principals with a role in one project |
| `telemetry_requests` | 8 | With `telemetry: listen:`: requests the listener serves at once. One more is answered `503`; twice as many connections may be open, and one more is closed |
| `control_write_rate`, `control_write_burst` | 1, 30 | With `control_db`: each principal's token bucket for `sfx admin` changes to projects, principals, members and keys: changes a second, and how many at once. It is the principal's on the whole server, apart from its write bucket in each project; the lists are not counted |

On a server of [many projects](#serving-many-projects), every limit above
that is per principal is per principal in each project: `conns_per_principal`,
the write bucket, `refusal_logs` and the limits on rows a principal owns.
`conns` stays the whole server's, and the `sfx admin` bucket is each
principal's on the whole server. So is `handshakes_per_principal`, since
a connection has named no project before its hello. Past `principals`,
`keys_per_principal` or `members_per_project`, `control init` skips the
line and says which limit it met, and `sfx admin` refuses the row with
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
the write rate, or an `sfx admin` change past its own, is refused with
`busy`, and its fix says how long to wait.
A `usage` call past `usage_per_day` is refused with `busy`, and stores
none of its records. A
connection past a connection cap is also refused with `busy`, and its fix
asks you to close other starfix sessions or wait for them to end; past
`conns_per_principal` it also names the setting to raise.

Fixed caps that no setting changes:

- **Text:** titles 500 bytes, names 255, bodies and comments 64 KiB.
- **Pages:** no reply may pass the 4 MiB frame. `comments` and `history`
  return the newest page (up to 100 entries, 500 with a limit, about 1 MiB
  of text) and a cursor to the page before, which `sfx` follows.
- **Lists:** the `sfx admin` lists of projects, principals and members
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
  names at most 20 people per issue, summing the rest as `(other)`; `sfx
  log` lists at most 500 entries. Entries are bounded per day, but the
  events of logging and undoing them are not: a principal who logs and
  undoes over and over grows the event log at the write rate, as any
  write does.
- **Similar issues:** lookups read closed titles from a cache refreshed on
  close and reopen, or after a minute.

## Confine starfixd with AppArmor

This step is optional. It was tested on Ubuntu 24.04, on arm64, in a
virtual machine, on 12 October 2026
([What was run, and where](#what-was-run-and-where)); amd64 was not
tested.

Ubuntu 24.04 runs AppArmor, and out of the box it confines neither sshd
nor a program it has no profile for: on the machine above, `aa-status`
listed no profile for sshd, sshd and its children ran `unconfined`, and
so did starfixd. starfix ships three profiles for starfixd
([deploy/apparmor](../deploy/apparmor/README.md)). They matter most for
the bridge, `starfixd stdio`: sshd starts it for every connection,
outside the systemd unit and so with none of the unit's protections,
as a user that owns its own binary, its config file and its
`authorized_keys`.

| Profile | Runs | May |
|---|---|---|
| `starfixd` | `starfixd stdio`, `starfixd status`, and anything else run from the binary's path with no profile asked for | Read the config file and connect to the daemon's socket. No network, no other file, no program |
| `starfixd-serve` | `starfixd serve`, because the unit asks for it | Read the config file, listen on the socket, reach Dolt over TCP, for SQL and for [its metrics](#dolts-figures), write the keys file and read `authorized_keys`; listen on the loopback port for metrics and health, post metrics and traces to a collector over TCP, make and read the key of the stand-ins its traces name people by, and write to systemd's notify socket ([Observability](#observability)). No program |
| `starfixd-admin` | `control init`, `import-bd`, `export-bd` and `upgrade`, when the operator asks for it | Open the store, read and write files in the top of `/var/lib/starfix`, read `authorized_keys`, reach the network, replace the binary in `/opt/starfix/bin`, and run `sudo` |

AppArmor picks a profile by the program's path and never by its
arguments, so one path can carry one profile by itself. It carries the
smallest, since sshd has no way to ask for another. None of the three
may write the config file or `authorized_keys`, and only the operator's
may write the binary.

**Install them**, as root:

```sh
cd "$(mktemp -d)"
curl -fsSL -o profile https://raw.githubusercontent.com/ariesworx/starfix/main/deploy/apparmor/starfixd
curl -fsSL -o tunables https://raw.githubusercontent.com/ariesworx/starfix/main/deploy/apparmor/tunables/starfixd
install -m 0644 profile /etc/apparmor.d/starfixd
install -m 0644 tunables /etc/apparmor.d/tunables/starfixd
apparmor_parser -r /etc/apparmor.d/starfixd
mkdir -p /etc/systemd/system/starfixd.service.d
printf '[Service]\nAppArmorProfile=starfixd-serve\n' > /etc/systemd/system/starfixd.service.d/apparmor.conf
systemctl daemon-reload
systemctl restart starfixd
cat /proc/"$(systemctl show starfixd -p MainPID --value)"/attr/current
```

The last command prints `starfixd-serve (enforce)`. The two files are
short and are not part of a signed release, so read them before you
install them. AppArmor loads everything in `/etc/apparmor.d` at boot.
To watch before you enforce, load them with `apparmor_parser -r -C`
instead: AppArmor then logs what it would have refused and refuses
nothing.

**Run the operator's commands through `aa-exec`** from then on, as the
`starfix` user as before:

```sh
cd /var/lib/starfix
sudo -u starfix -H aa-exec -p starfixd-admin -- starfixd upgrade --check
sudo -u starfix -H aa-exec -p starfixd-admin -- starfixd upgrade
sudo -u starfix -H aa-exec -p starfixd-admin -- starfixd control init --name acme
sudo -u starfix -H aa-exec -p starfixd-admin -- starfixd import-bd bd.jsonl
sudo -u starfix -H aa-exec -p starfixd-admin -- starfixd export-bd -o back.jsonl
```

- Without `aa-exec` such a command runs under the bridge's profile and
  is refused with `permission denied`: `control init` on
  `authorized_keys`, anything that opens the store on `dial tcp
  127.0.0.1:3306: socket`, and `upgrade` on its lookup of the release
  host (`api.github.com`, unless `releases:` names another).
- `import-bd` and `export-bd -o` take files in the top of
  `/var/lib/starfix`, not below it or elsewhere. A file your shell
  opens is not limited: `starfixd import-bd - < FILE`, `starfixd
  export-bd > FILE`, or a pipe.
- `aa-exec` is in Ubuntu's `apparmor` package, which is installed.
- **`starfixd status` needs no `aa-exec`**: `sudo -u starfix -H starfixd
  status` runs under the bridge's profile, which allows the two things
  it does. root cannot run it directly here, because that profile gives
  root no way past the config file's mode; the refusal says to run it
  as `starfix`.

**If you moved a path** (the binary, the config file, the socket, the
home directory, `keys_file`, `authorized_keys` or
`telemetry: principals_key`), change it in
`/etc/apparmor.d/tunables/starfixd`, which names each one once, and
load the profiles again with `apparmor_parser -r
/etc/apparmor.d/starfixd`. Rules of your own go in
`/etc/apparmor.d/local/starfixd`, `local/starfixd-serve` and
`local/starfixd-admin`. A `dsn` that names Dolt's host by a name that
needs DNS, where the guide's has an address, needs UDP allowed in
`local/starfixd-serve`; the profile's comments give the two lines.

**Metrics and traces sent straight to a hosted backend need three lines** in
`/etc/apparmor.d/local/starfixd-serve`, then `apparmor_parser -r
/etc/apparmor.d/starfixd` and a restart. The daemon otherwise speaks to
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
journalctl -k -g 'apparmor="DENIED"'
```

Each line names the profile, the operation and the path or the kind of
socket. In correct use there are none.

**To remove them:**

```sh
apparmor_parser -R /etc/apparmor.d/starfixd
rm /etc/apparmor.d/starfixd /etc/apparmor.d/tunables/starfixd
rm /etc/systemd/system/starfixd.service.d/apparmor.conf
systemctl daemon-reload
systemctl restart starfixd
```

Unload the profiles and remove the unit's line together. With the line
and no profile, starfixd does not start (`Failed to prepare AppArmor
profile change to starfixd-serve`). With the profiles and no line, the
daemon gets the bridge's profile and cannot reach Dolt.

What they do not do:

- **They do not keep a daemon that has been taken over away from the
  keys file.** `starfixd-serve` must write the file sshd reads keys
  from, and sshd accepts any line in it. The bridge and the operator's
  commands cannot write it.
- **They do not name Dolt's address.** `starfixd-serve` may open TCP
  connections anywhere: rules that name an address or a port were not
  tested, and the profile does not rely on them. The unit can hold the
  daemon to this machine instead: `IPAddressDeny=any` and
  `IPAddressAllow=localhost` in a drop-in, as Dolt's unit has, were run
  on the machine above with the guide's Dolt on 127.0.0.1, and starfixd
  still read Dolt's metrics there. Leave them
  out if `dsn` names another machine.
- **They do not hold the listener to loopback.** starfixd does that
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
starfixd reading them every 15 seconds, which needed no rule added:
to AppArmor it is one more TCP connection. What
was not: an upgrade to a newer release (its `--check` and `--rollback`
were run), and amd64.

## Logs

starfixd logs to stderr, which systemd sends to the journal
(`journalctl -u starfixd`), as text (`log_format: text`, the default)
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
- **`req` is the request's id:** 16 hex digits, on every line starfixd
  logs for one request, and in the refusal the client is sent. When a
  request fails on the server (`unavailable`), `sfx` prints it as a
  third line, `req:`, and `sfx --json` and an agent's tool error carry
  it, so a report of the failure finds its lines:
  `journalctl -u starfixd | grep 3f2a9c0e51b7d684`.
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
| `agents`, `inbox`, `claims`, `extended` | How many registry rows and inbox items a prune removed; how many leases a clock step extended, and by how much |
| `path`, `keys` | The keys file, or the stand-in key's file, and how many keys were written |
| `log`, `clock`, `history`, `behind`, `since`, `was_behind_since`, `direction`, `step`, `steps`, `from`, `to`, `forward_since_start`, `backward_since_start` | [The server's clock](#the-servers-clock): which history, the two times, how far behind and since when, and a step's direction, size and count |
| `url`, `failures`, `after_failures`, `every` | [Dolt's figures](#dolts-figures): the URL read, how many reads have failed, and how often it is read |
| `version`, `socket`, `control_db`, `projects`, `admins`, `addr` | The start: the build, the socket, the control database, how many active projects, the configured admins, and the listener's address |
| `push`, `push_interval`, `push_headers`, `pull`, `per_project`, `traces`, `trace_headers`, `sampler`, `logs`, `log_headers`, `principals`, `service` | What telemetry is on ([Observability](#observability)). Header counts, never their names or values |
| `signal`, `failed`, `failures_not_logged` | An export that failed or recovered: `metrics`, `traces` or `logs`, and how many failures |

### Events

| Event | Level | Logged when |
|---|---|---|
| `daemon.serving` | info | starfixd listens on its socket |
| `admin.unvouched` | warn | At start: a principal named under `admins:` has a row or a key that someone who is no server admin made. Check the key is theirs |
| `admin.check_failed` | error | At start: that check could not read the control store |
| `systemd.notify_failed` | warn | A notification could not be sent to systemd: ready, or the watchdog's |
| `log.suppressed` | warn | A principal's refusal lines went past `refusal_logs` a minute; `lines` is how many were left out |
| `conn.accept_failed` | warn | Accepting a connection failed in a way that may pass, such as running out of file descriptors; starfixd retries after a pause of up to a second |
| `conn.cap_refused` | warn | A connection that had not said hello was closed at twice `conns` |
| `conn.peer_refused` | warn | A socket peer that is not the user starfixd runs as was refused |
| `conn.opened` | debug | A connection passed its handshake |
| `conn.idle_closed` | debug | A connection idled past `idle_timeout` |
| `conn.read_failed`, `conn.write_failed` | info | A connection ended on a read or a write that failed |
| `conn.access_ended` | info | A connection was closed because its own access ended, during its handshake or by its own request |
| `conn.dropped` | info | An `sfx admin` change closed the connections of whoever lost access; `connections` is how many |
| `conn.key_lookup_ignored` | debug | A `starfixd` from before the keys file asked about a key; it is not answered |
| `handshake.failed` | info | A handshake was refused or failed; `err` says why |
| `agent.harness_ignored` | info | A client named a harness starfixd does not know |
| `agent.register_failed` | error | A session could not be noted for `sfx who` |
| `request.done` | debug | A request was answered |
| `request.refused` | info | A request was refused, with its `code`. Capped at `refusal_logs` a minute per principal |
| `request.failed` | error | A request failed on the server, and its client was answered `unavailable`. The line with the client's `req` has the cause |
| `request.part_failed` | error | A request was answered, without a part that could not be read: an issue's similar issues, its memories, a new member's keys |
| `project.added` | info | `sfx admin project add` made a project |
| `project.opened`, `project.closed` | info | A project's store was opened, or closed (`reason`: `idle`, `to make room` or `archived`) |
| `project.open_failed` | error | A project's database could not be opened: at start, at a handshake, or for an `sfx admin` change. Alert on it |
| `project.not_opened` | info | At start: an archived project's database awaits this starfixd's migrations and was left alone |
| `project.close_failed` | error | Closing a project's store failed |
| `store.schema_too_new` | error | A database was migrated by a newer starfixd |
| `store.commit_failed` | error | The Dolt commit of pending writes failed; the next tries again |
| `store.unsafe_account` | warn | `--allow-unsafe-dolt` let an unsafe Dolt account through |
| `control.reload_failed` | error | The reaper could not reread the control store |
| `control.index_stale` | error | A control write failed and the reread after it failed too: what starfixd holds in memory of keys and members may be stale until a restart |
| `claim.expired` | info | A claim's lease ran out and the reaper ended it |
| `claim.released` | info | A claim ended because its holder lost access; `reason` says how |
| `reaper.failed`, `reaper.prune_failed` | error | The reaper could not end expired claims, or prune, in a project |
| `reaper.pruned` | info | The reaper removed old registry rows or read inbox items |
| `access.sweep_failed` | error | The claims of holders without access could not be ended in a project |
| `keys_file.written` | info | The keys file was written |
| `keys_file.write_failed` | error | The keys file could not be written: sshd reads the old one. Every 30 seconds until a write succeeds. Alert on it |
| `keys_file.key_held_back` | warn | A key of the store is left out of the keys file because `authorized_keys` gives it to another principal |
| `keys_file.key_left_out` | error | A key's row in the store does not check out, and is left out of the keys file |
| `clock.behind` | error | The clock is behind the latest event of the database named, at start and every ten minutes while it lasts, with the fix ([The server's clock](#the-servers-clock)) |
| `clock.caught_up` | info | A clock that was behind has passed its history |
| `clock.stepped` | warn | The clock was set, or the machine suspended: the direction, the size and what starfixd did about it |
| `clock.step_settled` | warn | For each database, a forward step was added to every live lease |
| `clock.settle_failed` | error | A step could not be settled in the control store |
| `dolt.check_failed` | error | A check of Dolt failed: the one for `/readyz`, or the one made before a connection's project store is opened, which is then not opened (`project`, `database`) |
| `dolt.driver` | warn | The database driver found a connection to Dolt dead (`closing bad idle connection`); the pool dials again. Many of them mean Dolt restarted or is refusing connections |
| `dolt_metrics.reading` | info | At start, with `dolt_metrics_url` set |
| `dolt_metrics.read_failed` | warn | Dolt's metrics could not be read: once, then every 10 minutes while it lasts ([Dolt's figures](#dolts-figures)) |
| `dolt_metrics.read_recovered` | info | They can be read again |
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

Any failure to accept connections that will not pass stops starfixd: it
prints the error and exits 1, and the unit's `Restart=on-failure`
starts it again. An error that stops a start is printed as plain lines,
the cause and then each `fix:`, not as a log line.

### The audit stream

With `control_db`, every change to who has access to what is an event
in the control database's log: a project added, archived or activated;
a principal added, disabled or enabled; a key added or removed; a
membership set or removed. starfixd also writes each as a log line with
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
- **The event log is the record; the lines are a copy.** starfixd reads
  the lines from the log, after the change has committed, so a line can
  say nothing the log does not, and there is a line for a change
  whoever made it: `starfixd control init` run beside the daemon shows
  at the reaper's next tick, within 30 seconds.
- **A line appears once, and only while starfixd runs.** The stream
  starts at the newest event there is when starfixd starts. What was
  written while it was stopped (by `starfixd control init`) is in the
  event log and not in the journal, and the journal is not a substitute
  for backing the control database up.
- **A line is no request's.** It carries the event's `seq` and no `req`.
- **`telemetry: audit_log: false`** writes no audit line. The event log
  is unchanged.
- **To send the audit stream somewhere and nothing else**, filter on
  `audit=true` where the journal is read; with [logs over
  OTLP](#logs-over-otlp), on the record's `audit` attribute.

## Observability

starfixd can report on itself in open formats, so that whatever you
already monitor with can read it. **It is off by default:** with nothing
configured, starfixd exports nothing and opens no port.

| What | How it is turned on |
|---|---|
| Metrics pushed over OTLP/HTTP | `OTEL_EXPORTER_OTLP_ENDPOINT` in starfixd's environment |
| Traces pushed over OTLP/HTTP ([Traces](#traces)) | The same variable. They say who did what, by a stand-in, once a directory is made for a key |
| Log lines with a stable schema, and the audit stream of access changes ([Logs](#logs)) | Always there, on stderr. `telemetry: audit_log: false` leaves the audit stream out |
| The same log lines pushed over OTLP/HTTP ([Logs over OTLP](#logs-over-otlp)) | `OTEL_LOGS_EXPORTER=otlp` beside an endpoint. Off unless asked for |
| Metrics for a scraper, in Prometheus's text format | `telemetry: listen:` in the config file |
| `/healthz` and `/readyz` | The same listener |
| Readiness and a watchdog under systemd | `Type=notify` and `WatchdogSec` in the unit ([step 5](#5-run-starfixd-under-systemd)), which the guide's unit has |
| `starfixd status` | Always there, for the user starfixd runs as |
| Dolt's own figures in the status report | `metrics:` in Dolt's config and `telemetry: dolt_metrics_url:` in starfixd's ([Dolt's figures](#dolts-figures)); the guide's setup has both |
| Dolt's own metrics, for a scraper | The same `metrics:` block: `http://127.0.0.1:11228/metrics`. starfixd does not export them again |

Push and pull are independent: turn on either, both or neither. The
instruments are the same either way.

A collector's configuration, alert rules and a dashboard to start from
are in [deploy/observability](../deploy/observability):
[A collector, alert rules and a dashboard](#a-collector-alert-rules-and-a-dashboard).

### Send metrics to a collector on the machine

This is the setup to start with. An
[OpenTelemetry Collector](https://opentelemetry.io/docs/collector/), or
your vendor's or cloud's own agent, runs on the server, receives OTLP on
loopback and forwards to your backend. starfixd then holds no credential
and knows no vendor, and the collector does the retrying.

Add to the unit, then `systemctl daemon-reload` and restart starfixd:

```ini
[Service]
Environment=OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
```

The one variable sends both metrics and [traces](#traces), and that is
all it takes. The log says what is on:

```text
level=INFO msg="telemetry on" event=telemetry.on push=http://127.0.0.1:4318/v1/metrics push_interval=1m0s push_headers=0 pull=false per_project=true traces=http://127.0.0.1:4318/v1/traces logs=off trace_headers=0 sampler=parentbased_always_on principals=pseudonym service=starfixd
```

**Until you make one directory, the traces say nothing about who.**
Traces name people by a stand-in, made with a key that starfixd keeps
in a file, and the guide's unit gives it nowhere to write one. starfixd
starts and serves all the same, sends spans with no principal and no
session on them, exactly as `telemetry: principals: omit` does, and
says so at error level:

```text
level=ERROR msg="telemetry: spans say nothing about who: the key for principals' stand-ins is unavailable" event=telemetry.key_unavailable
level=ERROR msg="the key's file" event=telemetry.key_unavailable path=/var/lib/starfix/telemetry/principals.key
level=ERROR msg=cause event=telemetry.key_unavailable cause="it cannot be made: no such file or directory"
level=ERROR msg=fix event=telemetry.key_unavailable fix="make its directory, owned by the user starfixd runs as: install -d -m 0700 -o starfix -g starfix /var/lib/starfix/telemetry"
level=ERROR msg=fix event=telemetry.key_unavailable fix="under systemd, let starfixd write there with a drop-in: ReadWritePaths=/var/lib/starfix/telemetry (docs/server.md, Traces)"
```

To have the stand-ins, as root:

```sh
install -d -m 0700 -o starfix -g starfix /var/lib/starfix/telemetry
mkdir -p /etc/systemd/system/starfixd.service.d
printf '[Service]\nReadWritePaths=/var/lib/starfix/telemetry\n' > /etc/systemd/system/starfixd.service.d/telemetry.conf
systemctl daemon-reload
systemctl restart starfixd
```

```text
level=INFO msg="telemetry: made the key that principals' stand-ins are derived from" event=telemetry.key_made path=/var/lib/starfix/telemetry/principals.key
```

- **The restart is for the drop-in**, which systemd applies when the
  unit starts. Where starfixd may already write to the directory's
  parent, making the directory is enough: it looks for the key again
  every 30 seconds, and logs `spans name people by stand-ins from now
  on` when it has it.
- **`sfx admin status` and `starfixd status` say which it is**
  ([Traces](#traces), People), so it is not only in the log.
- **To leave people out on purpose**, set `telemetry: principals: omit`:
  no key is wanted, and nothing is logged as wrong.
- **To send metrics and no traces**, add
  `Environment=OTEL_TRACES_EXPORTER=none`. No key is then made.

### Send metrics straight to a hosted backend

Where running a collector is not wanted, starfixd can post to a backend
that takes OTLP over HTTP: metrics and traces alike, and the traces
say who by a stand-in once the key's directory above is made. That
usually needs a token, which is a secret:

```sh
install -m 0600 -o root -g root /dev/null /etc/starfix/telemetry.env
```

```sh
# /etc/starfix/telemetry.env
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example.com
OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer%20REPLACE_WITH_THE_TOKEN
```

```ini
[Service]
EnvironmentFile=/etc/starfix/telemetry.env
```

- **With the [AppArmor profiles](#confine-starfixd-with-apparmor)**
  this setup needs three lines added for the daemon, which that section
  gives: DNS, and the machine's certificate authorities. A collector on
  the machine needs none.
- **The token is read from the environment only.** There is no flag and
  no config key for it, so it cannot reach the process list or a config
  file that is not 0600. systemd reads the file as root before it drops
  to the `starfix` user.
- **starfixd never logs it.** The start-up line gives the number of
  headers, not their names or values, and the endpoint without any user,
  password or query. An export error is logged with every header's value
  removed, even when the backend's answer repeats it.
- A space in a header's value is written `%20`: the variable is a
  comma-separated list of `name=value` pairs, percent-encoded.

### The variables starfixd reads

| Variable | Does |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector's base URL, `http://` or `https://`. starfixd posts metrics to `/v1/metrics` and traces to `/v1/traces` under it, and logs to `/v1/logs` when `OTEL_LOGS_EXPORTER` asks for them. Unset: nothing is pushed |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | The full URL for metrics, for traces or for logs, used as it is; each wins over the one above for its signal, and any may be set alone |
| `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_METRICS_HEADERS`, `OTEL_EXPORTER_OTLP_TRACES_HEADERS`, `OTEL_EXPORTER_OTLP_LOGS_HEADERS` | Headers sent with each export, as `name=value,name=value`; a signal's own win for a name in both. A secret |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL`, `OTEL_EXPORTER_OTLP_LOGS_PROTOCOL` | Must be unset or `http/protobuf`. starfixd has no gRPC exporter, and refuses `grpc` at start |
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
| `OTEL_SERVICE_NAME` | The resource's `service.name`. Default `starfixd` |
| `OTEL_RESOURCE_ATTRIBUTES` | More resource attributes, as `name=value,name=value`, at most 64. It may set `service.instance.id`; `service.version` is always the build's |
| `OTEL_SDK_DISABLED` | `true` turns all of it off: no push, no span, no exported log record, no key and no `/metrics`. The listener, if set, still answers health, and stderr still has every log line |

A variable starfixd cannot honor stops it at start, with a `fix:`. The
OpenTelemetry SDK itself also reads the variables for TLS files
(`OTEL_EXPORTER_OTLP_CERTIFICATE` and the client key and certificate),
`OTEL_EXPORTER_OTLP_COMPRESSION` and the limits on one span
(`OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT` and the like, which starfixd's spans
are far below) from starfixd's environment; starfixd does not check
those.

Every export, of metrics, of spans and of log records, carries the resource
`service.name`, `service.version` (the starfixd version) and
`service.instance.id` (a random UUID, new at each start, so one run can
be told from the next).

### The listener: metrics to scrape, and health

```yaml
# /etc/starfix/starfixd.yaml
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
  answers at once, and `/readyz` says `starfixd is starting`.
- **A port that cannot be bound stops starfixd at start**, with a `fix:`.
- **It is bounded.** At most `telemetry_requests` requests are served at
  once ([Limits](#limits)); one more is answered `503` with
  `Retry-After: 1`. Twice that many connections may be open, and one more
  is closed. A request's headers must arrive within 5 seconds and its
  answer be read within 30.

`/readyz` is ready when all of these hold, and otherwise names the first
that does not:

| Line | Means |
|---|---|
| `starfixd is starting` | The store is not open yet, or the socket not yet listening |
| `starfixd is not listening on its socket` | The daemon is stopping, or could no longer accept connections |
| `the last check of Dolt failed` | Dolt did not answer a ping within 2 seconds: it hangs, or cannot be reached. The ping is on a pooled connection to the control database, or to the one project's; it is made at most every 5 seconds, however often `/readyz` is asked, and its answer stands until the next. starfixd's log has the error. A Dolt whose unit stops or restarts takes starfixd down with it ([step 5](#5-run-starfixd-under-systemd)), so then nothing answers on the listener at all until both are back |
| `the last write of the keys file failed` | With `control_db`: sshd may be reading an older list of keys ([Keys from the store](#keys-from-the-store)) |

`/healthz` says only that the process answers. A daemon that is healthy
and not ready is usually waiting for Dolt.

To have Prometheus scrape it:

```yaml
scrape_configs:
  - job_name: starfixd
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
| `starfix.request.duration` | `starfix_request_duration_seconds` | histogram, seconds | `op`, `result` |
| `starfix.project.requests` | `starfix_project_requests_total` | counter | `project`, `result_class` |
| `starfix.connections` | `starfix_connections` | up-down counter (a gauge on `/metrics`) | `project` |
| `starfix.handshakes` | `starfix_handshakes_total` | counter | `result` |
| `starfix.connections.dropped` | `starfix_connections_dropped_total` | counter | `reason` |
| `starfix.limit.refusals` | `starfix_limit_refusals_total` | counter | `limit` |
| `starfix.store.write.duration` | `starfix_store_write_duration_seconds` | histogram, seconds | `project` |
| `starfix.store.write.retries` | `starfix_store_write_retries_total` | counter | `project` |
| `starfix.store.commit.duration` | `starfix_store_commit_duration_seconds` | histogram, seconds | none |
| `starfix.stores.open` | `starfix_stores_open` | gauge | none |
| `starfix.store.open.duration` | `starfix_store_open_duration_seconds` | histogram, seconds | `migrated` |
| `starfix.claims` | `starfix_claims_total` | counter | `event`, `reason` |
| `starfix.control.writes` | `starfix_control_writes_total` | counter | `op`, `result` |
| `starfix.keys_file.writes` | `starfix_keys_file_writes_total` | counter | `result` |
| `starfix.keys_file.last_write.time` | `starfix_keys_file_last_write_time_seconds` | gauge, Unix seconds | none |
| `starfix.reaper.duration` | `starfix_reaper_duration_seconds` | histogram, seconds | none |
| `starfix.reaper.last_run.time` | `starfix_reaper_last_run_time_seconds` | gauge, Unix seconds | none |
| `starfix.clock.behind` | `starfix_clock_behind_seconds` | gauge, seconds | none |
| `starfix.clock.steps` | `starfix_clock_steps_total` | counter | `direction` |
| `starfix.telemetry.dropped` | `starfix_telemetry_dropped_total` | counter | `signal` |

What each counts:

| Instrument | Counts |
|---|---|
| `request.duration` | Each request on a connection, from its frame being read to its answer being ready, refusals and rate-limited requests included |
| `project.requests` | The same requests, by project and by class |
| `connections` | Connections past the handshake |
| `handshakes` | Each handshake, by how it ended; those of `starfixd upgrade`'s health check and `starfixd status` among them. A connection refused before its handshake is not one: a socket past twice `conns`, or a peer that is not the daemon's user |
| `connections.dropped` | Connections starfixd closed for a reason of its own, not those a client closed |
| `limit.refusals` | Refusals at a limit that starfixd can name |
| `store.write.duration` | A write that changed a row and committed: the same measurement as P50 and P99 in the status report, the wait for the project's one writer and any retry included |
| `store.write.retries` | Each attempt of a write that lost to a concurrent transaction and was tried again |
| `store.commit.duration` | Each Dolt commit of pending writes, about one a second while a project is written to, failed ones included |
| `stores.open` | Project stores open, opening or closing; always 1 on a server of one project |
| `store.open.duration` | With `control_db`: opening a project's store, migrations included: once for each active project before the daemon listens, which is where an upgrade's migrations run, then whenever a connection or an archive opens one that was closed |
| `claims` | Changes of a claim, as the event log records them, and renewals |
| `control.writes` | With `control_db`: each `sfx admin` change, refused ones included |
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
| `result` | `ok`, or the refusal's code: `not_found`, `conflict`, `exists`, `cycle`, `invalid`, `acceptance`, `forbidden`, `unavailable`, `version`, `auth`, `busy`, `stale` (sent to a client of protocol 6 alone; a client of 5 is sent `conflict`). For a handshake also `failed`: it ended without a refusal to name, as when a frame could not be read. For the keys file: `ok` or `failed` |
| `result_class` | `ok`; `error` for `unavailable`, which is the server's failure; `refused` for every other refusal, which is the request's |
| `project` | The project's UUID, only for a project a connection was admitted to. At most as many as the `projects` limit, and `other` past that |
| `reason` of a dropped connection | `membership`, `disabled`, `archived`, `key` (the access that let it in ended), `idle` (`idle_timeout`), `limit` (a socket past twice `conns`), `access` (the access ended while the handshake ran) |
| `limit` | `conns`, `conns_per_principal`, `handshakes_per_principal`, `handshake_sockets` (twice `conns`), `write_rate`, `control_write_rate`, `open_stores`, `telemetry_requests`, `projects`, `principals`, `keys_per_principal`, `members_per_project`, `prices`, `plans`, `hours_per_day`, `usage_per_day` |
| `migrated` | `true` when opening the store applied a migration |
| `event` of a claim | `take`, `renew`, `release`, `expire` |
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

With an endpoint for traces in its environment, starfixd records where
each request's time goes as spans and exports them over OTLP/HTTP.
Without one it starts no span: a request costs nothing for them.

**A trace follows a request from the agent's harness into starfix.**
`sfx` sends the W3C trace context it was started with, or the one a
tool call brought ([agent guide](agents.md#trace-context)), and
starfixd's span for the request is a child of the caller's span. A
request that brings none, or one that is not valid, starts a trace of
its own; it is never refused for it. Dolt has no tracing, so a trace
ends at starfixd's span around the work it asked of Dolt.

| Span | Kind | Covers | Attributes |
|---|---|---|---|
| A request's, named for its op: `create`, `show` and so on, and `unknown` for an op the server does not know | server | One request, from its frame being read to its answer being ready: the stretch `starfix.request.duration` measures | `op`, `result`, `project`, `protocol`, `principal`, `session` |
| `handshake` | server | A connection's handshake, from its first frame to its welcome, with the opening of the project's store when this connection caused it. The note of its session in the registry is beneath it and may end after it: it is written beside the wait for the first request. The root of a trace of its own, since a hello brings no trace context | `result`, `project`, `protocol`, `principal`, `session` |
| `store.write.attempt` | internal | One attempt of a write: the wait for the writer, the SQL transaction and its commit. A write that loses to a concurrent transaction is attempted again, in a new span, so a retried write shows as several | `project`, `attempt`, `outcome` |
| `store.write.wait` | internal | Within an attempt: the wait for the project's one writer connection, and the start of its transaction | `project`, `outcome` |
| `store.commit` | internal | A Dolt commit of pending writes, about one a second while a database is written to. The root of a trace of its own: no request waits for it | `project`, `outcome` |
| `store.open` | internal | With `control_db`: opening a project's store, as `starfix.store.open.duration` measures it | `project`, `migrated` |
| `store.migrate` | internal | Within the opening of a store, a project's or the control database's: the check of its schema, and the migrations applied | `project`, `outcome` |
| `reaper` | internal | One 30-second tick, with the four below and the store's writes beneath it, so a slow tick shows which part was slow | none |
| `clock.check` | internal | The look at the server's clock | none |
| `control.reload` | internal | With `control_db`: the reread of the control database's index | none |
| `keys_file.write` | internal | With `control_db`: the check of the keys file, and its write when it was out of date | `result`, `changed` |
| `access.sweep` | internal | With `control_db`: ending the claims of holders who lost their access, in every open store, or in one as it opens | `project`, when it sweeps one store |

- **The last four are also roots or children elsewhere.** Each is the
  root of a trace when it runs outside a tick, as the keys file's write
  does when starfixd starts; the keys file's write after an `sfx admin`
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
starfixd's spans either: of the caller's trace context they take the
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
  name under a key of 256 random bits. starfixd makes the key the first
  time it exports traces with stand-ins, and keeps it in
  `telemetry: principals_key:`, by default
  `/var/lib/starfix/telemetry/principals.key`, mode 0600. It reads the
  file at each start. The backend never sees the key or a name.
- **Without the key, spans say nothing about who, and starfixd says
  so.** A key that cannot be made or read never stops starfixd, and it
  never makes one up in memory or sends the name in its place: spans
  carry no principal and no session until the key is there. That holds
  for the default path and for one you set. The log has it at error
  level, with the cause and each fix on a line of its own; the status
  report has `TRACES SAY NOTHING ABOUT WHO` with the cause; `whois`
  answers that the server has no key, and why. starfixd looks for the
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
- **A server admin can resolve one:** `sfx admin principal whois
  p_0123456789abcdef` prints the principal's name
  ([`sfx admin principal`](cli.md#agents-and-maintenance)). The server tries its key against
  each of its principals; nobody else may ask, and a session's stand-in
  cannot be resolved. A server of one project has no `sfx admin`, so no
  `whois`.
- **Deleting the key file makes every earlier stand-in unlinkable**, to
  you as well: stop starfixd, delete the file, start it. It makes a new
  key, the same person is a new stand-in from then on, and `whois`
  answers that an old stand-in is nobody's. Do it when a retention rule
  asks that old traces no longer lead to a person. The file is in no
  backup starfix makes; if you back it up, old stand-ins stay
  resolvable for as long as a copy exists, and whoever holds a copy can
  test a name against a stand-in.
- **A stand-in hides a name from the backend. It does not make a person
  anonymous.** What they did, when, and in which project is all still
  there, under one stand-in, and on a small team that is often enough
  to tell who it is.
- **The setting does not change starfixd's own log**, which names
  principals and sessions as before ([Logs](#logs)). Whoever can read
  the journal can read the databases; if you ship the journal to a
  third party, filter it in the collector.

**Sampling** is the OpenTelemetry SDK's, set by `OTEL_TRACES_SAMPLER`:

| `OTEL_TRACES_SAMPLER` | Records |
|---|---|
| `parentbased_always_on` (default) | What the caller decided, when the request brought a trace context; every trace that starts in starfixd: a request with no trace context, a handshake, the reaper's tick, a Dolt commit |
| `parentbased_always_off` | Only requests whose caller sampled them |
| `parentbased_traceidratio` | What the caller decided; of the traces that start in starfixd, the share `OTEL_TRACES_SAMPLER_ARG` gives, from `0` to `1` |
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
`trace_id` and `span_id` of starfixd's own span for it, whether or not
the trace is sampled; a handshake's and the reaper's lines have those
of the span they were logged under. With traces off, a request's lines
have the caller's ids, when the request brought a valid trace context,
and none otherwise. A log record exported over OTLP has the same ids as
the record's own trace and span id.

### Logs over OTLP

The journal is the logs' home, and stderr is never turned off. With
`OTEL_LOGS_EXPORTER=otlp` beside an endpoint, starfixd also sends each
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
| `msg` | Its body. A message is fixed text in starfixd's code, never built from what a client sent |
| `event` | Its event name, and the attribute `event` |
| `trace_id`, `span_id` | Its own trace and span id |
| `project`, `op`, `code`, `req`, `issue`, `fingerprint` | The attribute, when the value has that id's shape: a UUID, an op starfixd knows (any other is `unknown`), a code, 16 hex digits, an issue's id, a key's fingerprint |
| Numbers, booleans, times, durations and the fixed words (`dur_ms`, `audit`, `seq`, `at`, `reason`, `role`, `connections` and the like) | The attribute |
| `principal`, `session`, `by`, `subject`, `admin`, `created_by`, `added_by` | The attribute as `telemetry: principals:` allows, exactly as a span's (People, above): a stand-in by default, the name with `name`, nothing with `omit`, and nothing, never the name, while the key of the stand-ins is unavailable. A session is treated as a principal is |
| `err`, `detail`, `cause`, `fix`, `effect`, `target`, `machine`, `harness`, `name`, `database`, `path`, `url` and every other field | **Nothing.** An error's text can quote what it refused or name a person, so it stays in the journal, under every setting |

- **To read the rest, go to the journal.** A record's `req`, its trace
  id, or its time and `event` find the line, which has the `err`.
- **The audit stream is exported with the rest,** with `audit=true`,
  `seq`, `op`, the `project`, the `fingerprint` and the `role`. Who made
  the change and whom it changed are stand-ins by default, which a
  server admin resolves with `sfx admin principal whois`; an audit
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
- **starfixd's own line about a failed log export is exported too,**
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
- **Metrics lose nothing.** starfixd does not retry within an interval:
  the next export carries the same cumulative totals.
  `starfix.telemetry.dropped{signal="metrics"}` counts each failed
  export.
- **Spans are lost.** A span that could not be exported is gone. The
  exporter retries a collector that answers 429, 502, 503 or 504, or
  does not answer in time, for as long as `OTEL_BSP_EXPORT_TIMEOUT`
  allows; a connection that is refused fails at once. Meanwhile finished
  spans wait in a queue of `OTEL_BSP_MAX_QUEUE_SIZE`, and one more is
  dropped, so memory is bounded: a full queue of 2048, the default,
  measured about 1.5 MB.
  `starfix.telemetry.dropped{signal="traces"}` counts every span lost,
  at the queue or with its export, exactly.
- **Exported log records are lost, and their lines are not.** A record
  that could not be exported is gone from the export; the journal has
  the line. The queue and the retries are as for spans, under the
  `OTEL_BLRP_*` variables, and
  `starfix.telemetry.dropped{signal="logs"}` counts every record lost,
  exactly. Logging never waits for the collector.
- Stopping starfixd exports what is waiting and makes one last export of
  metrics, and gives all of it at most 5 seconds together. Spans and
  log records it could not send in that time are counted as dropped, in
  a count that itself may not reach the collector. The lines starfixd
  logs while it stops go to the journal alone.

### A collector, alert rules and a dashboard

[deploy/observability](../deploy/observability) holds four files to
start from. **They are examples that are tested, not a product:** tests
hold every metric, label and value they name to what starfixd has, and
the [Docker test server](deploy/docker.md#telemetry) runs them against a
real starfixd, Dolt, OpenTelemetry Collector and Prometheus. Nothing in
them names a vendor.

| File | Is |
|---|---|
| `collector.yaml` | An OpenTelemetry Collector's configuration: it receives what starfixd pushes on `127.0.0.1:4318` (metrics, traces and, when asked, log records), scrapes starfixd's `/metrics` and Dolt's, and sends all of it on |
| `alerts.yaml` | Alert rules in Prometheus's rule format, which most monitoring systems read or translate |
| `prometheus.yaml` | A Prometheus configuration that scrapes the two endpoints and loads the rules, for a server that runs Prometheus and no collector |
| `dashboard.json` | One dashboard, in Grafana's format: requests, latency, writes, connections, claims, the server's own health, and Dolt's memory and collector |

**Wire a collector.** It runs on the server itself: starfixd's listener
and Dolt's metrics answer on loopback only and have no authentication,
and starfixd pushes to loopback with no credential.

1. Install the OpenTelemetry Collector as its own documentation says.
   The core distribution has every component the file uses; it was
   tested at 0.162.0.
2. Turn on what it reads, if it is not on already: `metrics:` in Dolt's
   config ([step 2](#2-run-dolt-under-its-own-account)),
   `telemetry: listen: 127.0.0.1:9464` in starfixd's
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
4. **Keep one path for starfixd's metrics.** The file receives them
   twice, pushed and scraped, because the test server checks both. On a
   real server take `prometheus/starfixd` out of the `metrics` pipeline,
   or set `OTEL_METRICS_EXPORTER=none` for starfixd and keep the scrape.
   Dolt's metrics can only be scraped.
5. **Logs, if you want them there.** The file has a `logs` pipeline,
   which carries nothing until starfixd is asked to export its log
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
   `event`: starfixd does not send the journal anywhere.

Pushed metrics carry their OpenTelemetry names (`starfix.request.duration`)
and scraped ones the names on `/metrics`
(`starfix_request_duration_seconds`); [What is measured](#what-is-measured)
lists both. The rules and the dashboard use the second, which is also
what most backends store the first as.

**Load the rules.** With Prometheus, name the file under `rule_files:`,
as `prometheus.yaml` does; other systems import the same format. The
rules select by the scrape jobs `starfixd` and `dolt`, so keep those
names or change both. Their thresholds are starting points to set
against your server's own history.

| Alert | Fires when |
|---|---|
| `StarfixdDown`, `DoltDown` | The endpoint has not answered a scrape for 2 minutes |
| `StarfixReaperStalled` | The reaper has not finished a tick for 2 minutes: expired claims are not ended, and systemd's watchdog is about to restart starfixd |
| `StarfixKeysFileWriteFailing` | A write of the keys file failed: sshd reads an older list of keys ([Keys from the store](#keys-from-the-store)) |
| `StarfixClockBehind`, `StarfixClockStepped` | The clock is behind starfix's own history, or was set ([The server's clock](#the-servers-clock)) |
| `StarfixProjectUnavailable`, `StarfixRequestsFailing` | Handshakes or requests are answered `unavailable`: the server's failure, most often a database that cannot be reached |
| `StarfixWriteLatencyHigh`, `StarfixWritesQueueing` | One write in a hundred takes over a second; more than one in five is retried |
| `StarfixWritesRateLimited`, `StarfixConnectionCapReached`, `StarfixStoreCapReached` | Requests are refused at `write_rate`, at a connection limit, or at `open_stores` ([Limits](#limits)) |
| `StarfixTelemetryMetricsExportFailing`, `StarfixTelemetrySpansDropped`, `StarfixTelemetryLogRecordsDropped` | starfixd counts failed pushes of metrics, spans it lost, or log records it lost from the export, whose lines the journal still has ([When the collector is down](#when-the-collector-is-down)) |
| `DoltCollectorPastTheCliff`, `DoltCollectorBusy` | With a memory limit set, Dolt's garbage collector ran more than 14 times for each 100 writes, or 3 or more: the thresholds of [Dolt's memory](#dolts-memory) and of the status report |
| `DoltMemoryOverItsLimit`, `DoltMachineMemoryHigh`, `DoltDiskFilling` | Dolt's resident memory is over its `GOMEMLIMIT`; the machine's memory is over 90% in use, or its disk over 85%, as Dolt reads them |

- **No rule asks `/readyz`,** because no metric says what it answers.
  Probe `http://127.0.0.1:9464/readyz` from the machine with whatever
  probes HTTP for you. `StarfixdDown`, `StarfixReaperStalled`,
  `StarfixKeysFileWriteFailing` and `DoltDown` cover the same ground
  from the metrics.
- **No rule matches a log line's `event`.** These are rules over
  metrics, and nothing in the examples turns a log record into one.
  Match [`event`](#events) where your logs are read. Three of the events
  worth an alert have a rule here from the metric that counts the same
  thing: `keys_file.write_failed`, `project.open_failed` at a handshake
  (`StarfixProjectUnavailable`) and `telemetry.export_failed`.
- **No metric gives the wait for a project's writer alone.** A write's
  latency includes it, and a trace shows it, in each attempt's
  `store.write.wait` span.
- **A counter has no series until it first counts,** and Prometheus sees
  no increase in a series' first sample. The rules on counters that
  should stay at nothing therefore also fire on a series that is new
  and already above zero.
- **For several starfixd servers in one Prometheus,** add `by (instance)`
  to the rules' sums.

**The dashboard** is imported into Grafana as it is and asks which
Prometheus data source to read. No test loads it into Grafana: tests
check its queries, as they do the rules', and the test server runs each
query against a real Prometheus.

**What was run.** All of it in the Docker test server, on Docker
Desktop on a Mac (arm64), on 12 October 2026: the Collector at 0.162.0
and Prometheus at 3.15.0, with every rule loaded and evaluated, every
query of the dashboard run, and starfixd's metrics, traces and log
records received, the audit stream among them. The `filter/audit`
example and the backend's exporter were only checked by the Collector's
`validate`: no record was sent through either. Not run: the Collector as a systemd unit on
a server, beside the [AppArmor profiles](#confine-starfixd-with-apparmor),
which confine starfixd and not a collector; a hosted backend; and any
alert against days of real traffic.

### starfixd status

```sh
sudo -u starfix starfixd status
sudo -u starfix starfixd status --json
```

It prints [the status report](#watching-a-server) that `sfx admin status`
gives a server admin, with every project, for a script or an agent on
the server that has no admin key. `--json` prints exactly one JSON
document: the report, or `{"error": …}` and exit code 1.

- It asks the running daemon over its unix socket, so it works only for
  **the user starfixd runs as**: the config file and the socket's
  directory are that user's alone, and on Linux the daemon also checks
  that whoever connects runs as its own user. Anyone else gets a refusal
  with a `fix:`. So does root where the
  [AppArmor profiles](#confine-starfixd-with-apparmor) are loaded: run
  it with `sudo -u starfix -H`, as above. It needs no `aa-exec`.
- It must be the same build as the daemon, since it speaks the daemon's
  protocol: one of another protocol version is refused with `version`.
- It connects under the reserved principal `starfixd-status`, which no
  key can sign in as. The connection is in no project, is not listed by
  `sfx who`, and is answered nothing but the report.
- On a server of one project it lists that project by its UUID, since
  only a control database gives projects names, and counts no projects
  or principals.
- It answers in about 2 seconds when Dolt does not answer at all, with
  each open project's sessions as `unknown`
  ([When Dolt does not answer](#when-dolt-does-not-answer)), whichever
  stores are open: `sfx admin status` is as prompt only from a
  repository whose project's store is open, and is refused from any
  other. It waits 30
  seconds for the daemon and no longer: `the daemon did not answer`
  then means starfixd itself is stuck, which its log explains.

## Upgrades

This release speaks protocols 5 and 6, and accepts both. Protocol 6
adds idempotency keys to the writes an outbox may queue, and the
refusal `stale` ([When the server cannot be reached](cli.md#when-the-server-cannot-be-reached));
an `sfx` of protocol 5 is served as it was, and is told `conflict`
where a client of 6 is told `stale`. This `sfx` against a server of
protocol 5 speaks 5: refused at its first hello, it says hello again in
5 on the same connection, and then queues, while the server is away,
only the writes that server applies once. **It refuses every earlier
release's `sfx`**: v0.2.x (protocol 2), v0.3.x (protocol 3) and v0.4.x
(protocol 4). That is a break made once, on purpose,
while nobody runs starfix in production and it changes quickly
([design §14, decision 25](design/starfix.md#14-decisions)). From here
on the rule holds again: a release that changes the protocol still
accepts the one before it, so the server is upgraded first and the
clients after, at their own pace.

Crossing this one boundary, both sides must be upgraded, and until both
are, the one upgraded first cannot talk to the other:

1. **Tell everyone first.** Once the server is upgraded, every `sfx`
   older than this release is refused until it is upgraded.
2. **Upgrade the server,** as below. `starfixd upgrade` run by v0.4.0
   works across the boundary: its health check is answered by the new
   starfixd whatever protocol the old binary speaks.
3. **Everyone upgrades `sfx`:** `sfx upgrade`, on each machine. It reads
   the release from GitHub and asks the server nothing, so it works
   while the server refuses that `sfx`. Someone without it runs the
   install script again ([Installing](install.md)).
4. **Everyone restarts their agents.** The MCP server in an agent's
   harness is an `sfx mcp` process, started from the old binary, and it
   stays the old one until the harness starts it again.

What each side sees meanwhile, with exit code 3:

| Who | Is told |
|---|---|
| An older `sfx` against this server | `sfx: starfix speaks protocol 4; the server (v0.5.0) needs protocols 5 to 6` and `fix: upgrade this client: run `sfx upgrade`, or install the latest release (docs/install.md), then restart any agent that runs `sfx mcp``. The old `sfx` prints the server's words, so it says this whatever its version |
| An agent on an older `sfx mcp` | Every tool call fails with code `version` and that message, and the fix `tell the user starfix cannot connect, quoting the server:` followed by the line above. The session's start says the same in one line. The agent cannot fix it: the person upgrades `sfx` and restarts the agent |
| This `sfx` against an older server | `sfx: starfix speaks protocol 6; the server (v0.4.0) is older and accepts protocols 2 to 4` and `fix: ask the server admin to run `starfixd upgrade`, or install the starfix release that matches the server` |

Upgrading the clients first works too, in the sense that nothing is
lost: each new `sfx` is refused, as in the last row, until the server is
upgraded. Server first is shorter, because step 3 is then one command
that the refusal itself names.

On the server, as the `starfix` user:

```sh
cd /var/lib/starfix
sudo -u starfix -H starfixd upgrade --check
sudo -u starfix -H starfixd upgrade
```

`starfixd upgrade` does this, in order:

1. Downloads the release and checks its signature and checksum.
2. Checks the clock against the server's history, as `serve` does at
   start. If the restarted daemon would refuse to start for it
   (`clock_behind: stop`), the upgrade stops here with that refusal and
   changes nothing: otherwise the daemon's refusal would fail the health
   check of step 5 and be taken for a bad release. `upgrade --check`
   prints the same finding on a line starting `clock:`.
3. Commits the database and tags it `starfix-<old version>`, a point to go
   back to. With `control_db`, it does so in the control database and in
   every project's database, and stops, changing nothing, if one cannot
   be reached.
4. Swaps the binary.
5. With `systemd_unit:` set, or `--restart`, restarts the unit through the
   sudo rule from [step 5](#5-run-starfixd-under-systemd). It then checks
   that the new version answers on the socket within 30 seconds. If it does
   not, upgrade puts the old binary back and restarts it. The check is
   made by the binary that ran the upgrade, the old one, and starfixd
   answers it whatever protocol that binary speaks, so an upgrade across
   a protocol change is not mistaken for a failed one.

   sudo compares the whole command with its rule, so the rule must spell
   the unit as `systemd_unit` does: `starfixd.service` in both, as steps
   4 and 5 write them. `--restart` with no `systemd_unit` asks for
   `systemctl restart starfixd`, which that rule does not cover: sudo
   answers `a password is required`, nothing is restarted, and upgrade
   reports the daemon as not healthy (seen with `--rollback --restart`).

With `control_db`, starfixd opens the database of every active project
once before it listens, which applies the new version's migrations to
each (a few milliseconds for a database already current), and the check
answers for all of them: it does not depend on the config file's
`project` being active. If a project's database cannot be opened, the
check is refused and names the project, so upgrade puts the old binary
back; the log says why. An archived project's database is migrated when
the project is activated, and not at start: starfixd opens it then only
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

**Upgrading to the first release that checks the clock.** A server
whose clock once ran ahead may hold events dated after the present. It
runs today, and after this upgrade it will not start
([When the clock is behind the server's
history](#when-the-clock-is-behind-the-servers-history)). The release
before has no step 2, so its `starfixd upgrade` swaps the binary, the
new daemon refuses to start, the health check fails and the old binary
is put back: nothing is lost, and the upgrade reports the new version
unhealthy, with the refusal in `journalctl -u starfixd`. To find out
beforehand, compare the clock with each database's latest event, as
Dolt's root:

```sh
date -u +%Y-%m-%dT%H:%M:%SZ
cd /var/lib/dolt/data && sudo -u dolt -H dolt sql -q "SELECT MAX(at) FROM starfix.events"
```

If an event is later than the clock by more than two minutes, either
wait until the clock has passed it, or set `clock_behind: warn` for the
upgrade, or start the new version once with `--accept-clock-behind`.
Add `RestartPreventExitStatus=75`, `Wants=time-sync.target` and
`time-sync.target` in `After=` to the unit
([step 5](#5-run-starfixd-under-systemd)) before upgrading, and run
`systemctl daemon-reload`.

**Keys and the upgrade.** sshd runs nothing of starfix's to find a key
([Keys from the store](#keys-from-the-store)), so an upgrade has no
copy of the binary to refresh for it. The new starfixd writes the keys
file again as it starts.

Where the [AppArmor profiles](#confine-starfixd-with-apparmor) are
installed, run each of these through `aa-exec -p starfixd-admin --`.

It refuses to run as root, except with `--check`. `--to vX.Y.Z` installs a given release, but
never an older one, because migrations only go forward. `--rollback`
restores the binary the last upgrade replaced. Take a
[dump](#backups-and-restore) first if you want a copy outside Dolt.

**Where the release is read from.** GitHub, unless the server is told
otherwise, in one of two ways. Both take what `sfx upgrade` and
`install.sh` take ([Where releases are read
from](install.md#where-releases-are-read-from)): the `https://` URL of a
static tree, or the absolute path of a directory that holds one.

| Set | Where | Note |
|---|---|---|
| `releases:` | The config file | The usual way on a server |
| `STARFIX_RELEASES` | The environment of `starfixd upgrade` | Wins over `releases:`. `sudo` does not pass your shell's variables on, so write `sudo -u starfix -H env STARFIX_RELEASES=… starfixd upgrade` |

```yaml
releases: https://releases.example.com/starfix
```

A value that is neither is refused before anything is asked of anyone,
and so is an `http://` URL to anything but `127.0.0.1` or `[::1]`. The
daemon never reads the setting: only `upgrade` does, and `upgrade
--check` is how to try it.

**A server with no way out.** Put the release on the server and name
the directory. Carry in `starfixd_<version>_linux_<arch>.tar.gz`,
`checksums.txt` and `checksums.txt.sig`, as the `starfix` user can read
them:

```sh
sudo install -d -o starfix -g starfix /var/lib/starfix-releases/v0.6.0
sudo install -o starfix -g starfix -m 0644 starfixd_0.6.0_linux_amd64.tar.gz checksums.txt checksums.txt.sig \
  /var/lib/starfix-releases/v0.6.0/
sudo -u starfix -H env STARFIX_RELEASES=/var/lib/starfix-releases starfixd upgrade --to v0.6.0
```

`--to` needs no `index.json`; an upgrade to the latest release reads
one, which `internal/tools/releaseindex` writes
([RELEASING.md](../RELEASING.md#publishing-to-a-static-host)). The
directory is read as it is and nothing in it is trusted: the signature
on `checksums.txt` is checked against the release key built into the
running starfixd, and the archive against `checksums.txt`, exactly as
for a download. A link in the directory that leads out of it is not
followed.

Under the [AppArmor profiles](#confine-starfixd-with-apparmor), an
`https://` mirror needs nothing new: the operator's profile already
reaches any host. A directory does, since that profile reads nothing
outside what it lists: add `/var/lib/starfix-releases/{,**} r,` to
`/etc/apparmor.d/local/starfixd-admin` and load the profiles again.
That rule has not been run on a confined server.

A mirror, like GitHub, can hold a release back: one that goes on
serving an old `index.json` keeps `upgrade --check` saying `up to
date`. It cannot make the server install anything the release key did
not sign, nor a release older than the one running.

Each person runs `sfx upgrade` on their own machine. It never runs by
itself ([Install](install.md#upgrade-and-roll-back)).

Keep Dolt at the release starfix names, and change it only with a starfix
release that does.

## Rotating the host key

Every `.starfix.yaml` pins the server's ED25519 host key, and `sfx`
refuses a server that presents another. To change the key without
refusing every client at once, list the new key beside the old before
sshd presents it, and remove the old one after. `sfx` connects when the
server presents any key the file pins (`server.host_keys`, [Connect a
repository](cli.md#the-host-key-is-pinned)). Nothing changes on
starfixd's side: the host key is sshd's, not part of starfix's protocol.

1. **Make the new key** on the server, beside the one in use, which
   sshd keeps presenting:

   ```sh
   ssh-keygen -q -t ed25519 -N '' -C '' -f /etc/ssh/ssh_host_ed25519_key.next
   ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.next.pub
   ```

   The second command prints the new fingerprint.

2. **Pin both** in every repository whose `.starfix.yaml` names this
   server, one for each project, and commit the change:

   ```yaml
   server:
     host: starfix.example.com
     host_keys:
       - SHA256:…   # the key sshd presents now
       - SHA256:…   # the new key, from step 1
   ```

   `host_keys` replaces `host_key`; a file with both is refused. An
   `sfx` older than the list refuses the whole file, so everyone runs
   `sfx upgrade` first.

3. **Wait for clients to pick it up.** Everyone pulls the commit, and
   each machine runs one `sfx` command that reaches the server, which
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

5. **Remove the old key** from every `.starfix.yaml`, back to
   `host_key: SHA256:…` with the new fingerprint, and commit. Once no
   file pins it, delete `ssh_host_ed25519_key.old*` and take a new
   backup of the host key ([Backups and restore](#backups-and-restore)).

`sfx admin project add` prints the new project's `.starfix.yaml` with
the keys the admin's own file pins: `host_key` for one, `host_keys` for
several. A project added during a rotation therefore pins both, and
needs step 5 too.

A machine that ran no command while both keys were pinned treats the
server as a new one once the old key is removed, as for a server
rebuilt with a new key: what it had queued under the old key waits
there, never sent by itself, and `sfx outbox` lists it for its owner to
move or drop.

## Backups and restore

Back up three things:

| What | Holds | How |
|---|---|---|
| The database | Every issue, comment and event | A daily `dolt dump`, below. With `control_db`: one dump for the control database and one for each project's, each from its own directory under `/var/lib/dolt/data` |
| `/etc/starfix/starfixd.yaml` | The project UUID and Dolt's password | With your other secrets |
| `~starfix/.ssh/authorized_keys` and `/etc/ssh/ssh_host_ed25519_key*` | Who may connect, and the host key every `.starfix.yaml` pins. With `control_db`, the control database's dump also holds principals, their keys and their memberships | With your other secrets |

A dump writes the database's current rows, the event log included, as SQL.
It runs while starfixd serves. This cron entry keeps a week of dumps,
one file per weekday. Save it as `/etc/cron.d/starfix-backup`:

```text
15 3 * * * root cd /var/lib/dolt/data/starfix && sudo -u dolt -H dolt dump -f -r sql -fn /var/backups/starfix/starfix-$(date -u +\%a).sql
```

```sh
install -d -o dolt -g dolt -m 0700 /var/backups/starfix
```

Copy the files off the machine every day: to object storage with
versioning, or with your backup tool. A dump does not hold Dolt's own
commit history. Disk snapshots do, so take both if you can.

**To restore**, stop starfixd: a dump recreates the `starfix` database it
came from, dropping each table first.

```sh
systemctl stop starfixd
cd /var/lib/dolt/data
sudo -u dolt -H dolt sql -q "DROP DATABASE starfix"
sudo -u dolt -H dolt sql < /var/backups/starfix/starfix-Mon.sql
systemctl start starfixd
```

After a restore starfixd compares the clock with the restored
history, like at every start. A dump from this server's own past is
behind the clock and starts as usual. One written by a machine whose
clock ran ahead is refused, with the flag that starts it once
([When the clock is behind the server's
history](#when-the-clock-is-behind-the-servers-history)).

On a new machine, follow steps 1 to 6 first. Then put back the config
file's `project`, the host keys and `authorized_keys`, and restore the
dump. If the host key changed, everyone must update `host_key`. `sfx`
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
quiet hour: `cd /var/lib/dolt/data/starfix && sudo -u dolt -H dolt gc`.
It grows faster than the rows suggest: in
[the measurements](../spike/multidb/RESULTS.md) Dolt's files took 28 to
56 KiB for each write, 6.5 GiB for a project of 30,000 small issues, and
its garbage collection made them 7 to 47 times smaller. It is also how
to get Dolt's memory back ([Dolt's memory](#dolts-memory)).

## Dolt's memory

Dolt, not starfixd, is what the machine's memory goes to. These figures
are from [one set of measurements](../spike/multidb/RESULTS.md): Linux
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
  dolt` restarts starfixd with it, since starfixd's unit requires
  Dolt's, so connections drop: pick a quiet hour.
- **`dolt gc` and then a restart give back nearly all of it:** 110 to
  200 MiB at rest in every case measured, up to the project of 30,000
  issues, which a restart alone left at 895 MiB. The collection took
  from under a second to 13 seconds a project. It was measured as
  `CALL DOLT_GC()` in each database, as root, with starfixd's
  connections closed; stop starfixd first if you want to match that.
- **So size the machine for what is written between restarts.** 2 GB
  holds one project of a few thousand issues. Give it 4 GB for a
  project past about 10,000 issues or for ten or more projects in
  regular use, or restart Dolt on a schedule, weekly say, and after a
  bulk import.
- **Read Dolt's own figure.** The `Dolt` line of
  [`sfx admin status`](#dolts-figures) gives it as `MiB resident`, with
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
  against starfixd's writes.
- **Not measured:** a limit together with systemd's `MemoryMax`, which
  is the way to a hard cap, and a limit on a server that mostly reads.

**The line to read** is `Dolt's collector:` in
[`sfx admin status`](#dolts-figures), which sets the cycles of Dolt's
garbage collector against starfixd's writes over the last ten minutes:

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

## starfixd commands

| Command | Does |
|---|---|
| `serve` | Run the daemon. Flags: `--config`, `--dsn`, `--socket`, `--project`, `--prefix`, `--log-level`, `--log-format`, `--dev`, `--allow-unsafe-dolt`, and `--accept-clock-behind TIME`, which starts once although the clock is behind a history no later than TIME ([The server's clock](#the-servers-clock)). Exits 75 when it refuses to start for the clock, and 1 for any other failure |
| `stdio --principal NAME [--key FINGERPRINT]` | The sshd forced command: bridge one SSH session to the daemon. `--key` is written by starfixd, into each line of its keys file, never by hand: it names the key that signed in, so starfixd refuses the session unless the control database holds that key for NAME, and closes it when the key is removed |
| `control init --name NAME [--authorized-keys FILE]` | Once, after setting `control_db`: adopt the server's database as the first project and import principals and keys from `authorized_keys` ([Serving many projects](#serving-many-projects)). Safe to run again |
| `import-bd [--project UUID] [--principal NAME] [--dry-run] [--json] FILE` | Import a bd export (`-` for stdin); see [Moving from bd](migrate-from-bd.md). `--project` picks a project on a server of many |
| `export-bd [--project UUID] [-o FILE]` | Write the store in bd's JSONL format |
| `upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart]` | Replace the binary with a verified release ([Upgrades](#upgrades)) |
| `status [--json] [--config FILE] [--socket PATH]` | Print the running daemon's [status report](#starfixd-status), as the user starfixd runs as |
| `version` | Print the version and protocol range |

## Troubleshooting

| What you see | Cause | Fix |
|---|---|---|
| `sfx: host key mismatch` | The server's key is not the pinned one | Check the new key with the admin, out of band, before you change `host_key` |
| `sfx: … accepted no key: sfx offered SHA256:…, the one key STARFIX_KEY_FILE names` | A key is configured, so `sfx` offered it alone, and the server does not know it | Have an admin add that key's `.pub`, or unset the variable to offer your default identities and ssh-agent's keys |
| `sfx: … accepted none of the N keys sfx offered: …` | No key is configured, and the server knows none of your default identities or ssh-agent's keys. The refusal lists each by fingerprint and file | Send the admin the `.pub` of the key you mean, or name your key file with `STARFIX_KEY_FILE` |
| The same, ending `… more keys that ssh-agent holds were not offered` | `sfx` offers five keys at most, since sshd hangs up at its sixth refusal, and ssh-agent holds more | Name your key file with `STARFIX_KEY_FILE`: it is then the only key offered |
| The same, ending `skipped ~/.ssh/id_ed25519: it is passphrase-protected, and ssh-agent does not hold its key` | A default identity needs a passphrase, which `sfx` never asks for, or is on a hardware token (`_sk`), and ssh-agent does not hold it | `ssh-add` the key |
| `sfx: no SSH key available` | No key is configured, `~/.ssh` has no usable default identity and ssh-agent holds none | Set `STARFIX_KEY_FILE` to your key file's path, keep the key under a default name, or `ssh-add` it |
| `sfx: STARFIX_KEY_FILE does not hold the path of a key file` | The variable holds something that is plainly a key, or is too long to be a path. `sfx` does not repeat it | Put the key in a file that only you can read and set the variable to the file's path ([CI](deploy/team.md)) |
| A refusal that ends `STARFIX_KEY is set, and sfx does not read it` | Sign-in failed, and the environment holds a variable of the old name, whose value `sfx` never looks at | Set `STARFIX_KEY_FILE` to the key file's path, and unset `STARFIX_KEY` |
| `sfx: --key-file is only for your own files` | `sfx setup --key-file` without `--global`: a project's files are committed | Add `--global`, or set `STARFIX_KEY_FILE` in the environment the agent starts in ([agent guide](agents.md#give-an-agent-its-own-identity)) |
| `sfx: … accepted no key`, though the key line is right | The `starfix` account is locked (`!` in `/etc/shadow`), or its shell is `nologin` | `usermod -p '*' starfix`; `usermod -s /bin/sh starfix` |
| `sfx: starfixd is not running on the server` | The daemon is stopped, or failed at start | `journalctl -u starfixd` shows why; `systemctl start starfixd` |
| `sfx: timed out after 15s waiting for … to open an ssh session`, `to start starfixd` or `to answer the hello` | The server accepted the key and then did not answer within the 15 seconds `sfx` gives a connection: sshd, or something on the path to it, went quiet (the first two), or `starfixd stdio` or the daemon did not answer (the last) | Run it again. If it persists: `journalctl -u ssh` for the first two, and `starfixd status` and `journalctl -u starfixd` for the last |
| `sfx: open ssh session: the server closed the connection once it had accepted the key` | sshd took the key and hung up before a session was open: it was restarting, or something between the two machines cut the connection | Run it again. If it persists, `journalctl -u ssh` on the server |
| `project "…" is not served by this server` | `.starfix.yaml` and the config file name different projects; with `control_db`, the project is unknown or archived | Make the two `project` values match |
| `… is not a member of project "…"` | With `control_db`: the principal has no row in the control database, or is in other projects only | An admin runs `sfx admin member add NAME` in the project's repository (`principal add NAME` for someone new) |
| `… is disabled on this server` | With `control_db`: the principal was disabled, whatever `admins:` says | An admin with reach over it runs `sfx admin principal enable NAME` |
| `sfx: … accepted no key`, for a key added with `sfx admin` | sshd is not reading starfixd's keys file, or the file has no line for the key | On the server: `sshd -T -C user=starfix \| grep -i authorizedkeysfile` must name `.ssh/starfix_keys`; `grep -c NAME /var/lib/starfix/.ssh/starfix_keys` shows whether the line is there; `journalctl -u ssh` says when sshd would not read the file for its owner or mode |
| `sfx admin` warns that starfixd `could not write the keys file sshd reads`, or `sfx admin status` says `THE LAST WRITE FAILED` | starfixd cannot replace its keys file. The change itself is recorded | `journalctl -u starfixd` has `the keys file is out of date` with the cause and a fix: usually the unit's `ReadWritePaths` ([Keys from the store](#keys-from-the-store), step 1), or the owner or mode of `.ssh`. starfixd tries again every 30 seconds |
| starfixd does not start: `write the keys file …` | With `control_db`: it cannot write its keys file | The error's `fix:` names the change; see [Keys from the store](#keys-from-the-store), step 1 |
| The log has `telemetry: spans say nothing about who: the key for principals' stand-ins is unavailable`, or the status report says `TRACES SAY NOTHING ABOUT WHO` | starfixd exports traces, or logs, and cannot make or read the key it names people with. It serves as before; its spans and exported log records carry no principal and no session. The line with `cause=` says which: `it cannot be made: no such file or directory` (no directory), `read-only file system` (the unit's `ReadWritePaths`), `permission denied` (the directory's owner, or the AppArmor profile) | The lines with `fix=` beside it: make the directory and add the drop-in ([Send metrics to a collector on the machine](#send-metrics-to-a-collector-on-the-machine)). starfixd looks again every 30 seconds; a new drop-in needs a restart. Or set `telemetry: principals: omit` to leave people out on purpose |
| The same, with `cause="its file is mode 0644: others can read it"`, or `its file does not hold a key`; the status report says the key `CANNOT BE TRUSTED` | The key's file is there and is not safe to use: someone else may have read it, or it was cut short or changed | If someone may have read it, delete it: starfixd makes a new one within 30 seconds, and stand-ins made with the old key can no longer be resolved. If nobody can have, `chmod 600` it ([Traces](#traces), People) |
| `sfx admin principal whois` answers `this server has no key for stand-ins: …` | What follows the colon says why: the key is unavailable, as in the two rows above; or the server exports no traces, or names people by `name` or `omit`, and has no key file from before | For an unavailable key, the rows above. Nothing resolves a stand-in made under a key that is gone |
| `the file there was not generated by starfixd` | `keys_file` names a file that holds something else, such as `authorized_keys` | Set `keys_file` to a file of its own, or move the other file away |
| A key removed with `sfx admin key remove` still signs in | Its line is still in `authorized_keys`, as the command warned | Delete the line ([Keys from the store](#keys-from-the-store), step 5) |
| `… may not add a key to …: nobody adds a key to their own account` | `sfx admin key add` for one's own principal | Ask another admin with reach; for a server admin, another server admin or a line in `authorized_keys` |
| `key … is the key this connection signed in with` | `sfx admin key remove` of your own key, on a connection that key signed in | Sign in with another of your keys (`STARFIX_KEY_FILE=PATH`) and remove it from there, or ask an admin |
| `… is the last server admin who is not disabled` | `sfx admin principal disable` of the only enabled server admin | Enable another first, or name another under `admins:` and restart |
| `the key that signed in is no longer one the server holds for …` | The key was removed, and sshd read a keys file from before that: one starfixd could not rewrite, or one restored from a backup; or an `authorized_keys` line was given `--key` by hand, which only starfixd writes | Use another key, or have the key added again; check `sfx admin status` for a failed write; remove `--key` from a hand-written line |
| `this server holds one project, so it has no projects, principals or members to manage` | `sfx admin project`, `principal`, `member` or `key` on a server with no `control_db` | [Opt in](#opt-in), or add people as lines of `authorized_keys` |
| `the server has no database names for new projects` | `sfx admin project add` on a server with no `project_dbs` | Grant a block of names and set `project_dbs` ([Opt in](#opt-in), step 4) |
| `starfixd's Dolt account was never granted database …` | `project add` took a name in `project_dbs` that was never granted | As Dolt's root, run the `GRANT` statement in the fix, then run the same `sfx admin project add NAME` again |
| `every database name for new projects is in use` | Every name in `project_dbs` has a project, archived ones included | Grant more names and raise `count` |
| `… may not …: it is for …` | An `sfx admin` command by someone who may not run it ([Managing access](#managing-access)) | Ask the admin the fix names |
| `starfixd could not open the database of an active project` from `starfixd upgrade` | A project's database is gone, not granted, or failed to migrate | `journalctl -u starfixd` names it and why; fix it or archive the project, then restart |
| `the control database … holds no active project` at start | `control_db` is set and `control init` has not run | `sudo -u starfix -H starfixd control init --name NAME` |
| starfixd does not start, and `systemctl status starfixd` shows `status=75/TEMPFAIL` under `not starting: the server's clock is behind its own history` | The clock is behind the latest event of the database named, by more than `clock_behind_tolerance`: the clock was reset or set back, the machine booted without its time, or the database was written by a clock that ran ahead | `timedatectl`; correct the clock and `systemctl start starfixd`. If the history is what is wrong, start once with the `--accept-clock-behind` the refusal prints, or set `clock_behind: warn` ([The server's clock](#the-servers-clock)). systemd does not retry this exit |
| After a boot, starfixd and every timer wait for good; `systemctl list-jobs` shows `time-sync.target start waiting` | `systemd-time-wait-sync` is enabled with its shipped `TimeoutStartSec=infinity`, and no time server answers | Add the drop-in of [Start starfixd after the clock is set](#start-starfixd-after-the-clock-is-set), which bounds the wait at 90 seconds; `systemctl stop systemd-time-wait-sync` lets this boot go on |
| `clock behind history` in the log, or `CLOCK BEHIND HISTORY` in `sfx admin status` | The clock went back under a running starfixd, or it started with `clock_behind: warn` or `--accept-clock-behind` | Correct the clock. Until it has caught up, timestamps are ahead of it and leases run at half speed; with `clock_behind: stop`, starfixd will not start again while this lasts |
| `clock stepped` in the log, or a `clock:` line in `sfx admin status` | The clock was set while starfixd ran, or the machine was suspended and resumed | Nothing for the data: leases were extended by a jump forward, and time did not go back for a step back. Find what set the clock, and whether the time service is synchronized |
| `starfixd upgrade` stops with `nothing was changed: starfixd would not start again after this upgrade` | The clock is behind the server's history, so the restarted daemon would refuse to start | As for exit status 75 above; then run the upgrade again |
| `usage record …: at … is more than 5m0s past the server's clock` | The machine that recorded the usage runs ahead of the server, or the server runs behind | Check both clocks; `clock_client_tolerance` widens the bound. `sfx usage --hook` drops such a record and says how many |
| starfixd refuses the Dolt account | The account is `root` or has too many rights, or `secure_file_priv` is empty | Run the SQL its fix names, or redo [step 4](#4-create-the-database-and-the-config-file) |
| `sfx` exits with code 3 | Client and server speak incompatible protocols | Upgrade the server, then the client |
| `busy` | A [limit](#limits) | Wait as the fix says, or raise the limit |
| `permission denied` on a file or socket the `starfix` user owns, after you moved a path; or from `control init`, `import-bd`, `export-bd` or `upgrade` | With the [AppArmor profiles](#confine-starfixd-with-apparmor): the path is not the one the profiles name, or the command ran without `aa-exec` | `journalctl -k -g 'apparmor="DENIED"'` names the profile and the path. Change the path in `/etc/apparmor.d/tunables/starfixd` and run `apparmor_parser -r /etc/apparmor.d/starfixd`; run an operator's command through `aa-exec -p starfixd-admin --` |
| starfixd does not start: `Failed to prepare AppArmor profile change to starfixd-serve` | The unit asks for a profile that is not loaded | `apparmor_parser -r /etc/apparmor.d/starfixd`, or remove the unit's `apparmor.conf` ([Confine starfixd with AppArmor](#confine-starfixd-with-apparmor)) |
| starfixd does not start: `dial tcp 127.0.0.1:3306: socket: permission denied` | The AppArmor profiles are loaded and the unit does not ask for `starfixd-serve`, so the daemon has the bridge's profile | Add the unit's `AppArmorProfile=starfixd-serve` line, `systemctl daemon-reload`, and restart |
| One failed start of starfixd in the journal of every boot, `connect: connection refused` | Dolt was not listening yet: `dolt.service` has no `ExecStartPost` that waits for its port | Nothing has to be done, since systemd starts starfixd again five seconds later. To remove the failed start, add the line to Dolt's unit ([step 5](#5-run-starfixd-under-systemd)) |
| starfixd answers `sfx`, yet `systemctl status starfixd` says `activating`, and every ten minutes `start operation timed out` restarts it; its log has `could not tell systemd the daemon is ready` | The unit has `Type=notify` and the daemon cannot write to systemd's socket: the AppArmor profiles loaded are from before the unit had that line | Install this release's profiles and `apparmor_parser -r /etc/apparmor.d/starfixd` ([Confine starfixd with AppArmor](#confine-starfixd-with-apparmor)); `journalctl -k -g 'apparmor="DENIED"'` names `/run/systemd/notify` |
| `Watchdog timeout (limit 2min)!` in the journal, then goroutine stacks and a restart | starfixd did not tell systemd it was alive for `WatchdogSec`: it hung, or one tick of its reaper took that long, as it can when Dolt stops answering | Read the stacks, which say where it was; check Dolt. If the machine is merely slow, raise `WatchdogSec` |
| `starfixd status`: `config: open /etc/starfix/starfixd.yaml: permission denied` | It was run as a user who cannot read the config file, or as root with the AppArmor profiles loaded | `sudo -u starfix -H starfixd status` |
| `telemetry: a metrics export failed` with `socket: permission denied` or `failed to load system roots` | With the AppArmor profiles: the collector's endpoint is a name that needs DNS, or is HTTPS, and the daemon's profile allows neither as shipped | Add the three lines of [Confine starfixd with AppArmor](#confine-starfixd-with-apparmor) to `local/starfixd-serve`, or send to a collector on the machine |
| `Dolt: unknown` in `sfx admin status`, and `could not read Dolt's metrics` in the log | starfixd cannot read Dolt's own metrics: Dolt is stopped, its config has no `metrics:` block, or `telemetry: dolt_metrics_url:` names another port or path. Nothing else is affected | The log line has the URL and the cause. `curl -s http://127.0.0.1:11228/metrics \| head -3` on the server shows whether Dolt answers; then [Dolt's figures](#dolts-figures) |
| starfixd does not start: `telemetry dolt_metrics_url … is not a loopback address`, `is not an http:// URL` or `has no path` | The setting names something other than Dolt's endpoint on this machine | Set it to `http://127.0.0.1:11228/metrics`, or remove it |
| `Dolt's collector: … OVER 14, AS WITH A MEMORY LIMIT TOO LOW` in `sfx admin status` | Dolt's garbage collector runs far more often than its writes explain, as it did in the measurements under a `GOMEMLIMIT` that was too low | Raise the limit, or restart Dolt, or `dolt gc` and restart it ([Dolt's memory](#dolts-memory)) |
| `SESSIONS` is `unknown` in `sfx admin status` or `starfixd status`, with `sessions: unknown for … (database did not answer in 2s)` | Dolt did not answer a count within 2 seconds: it hangs, is stopped (`SIGSTOP`), swaps, or is collecting without pause under a memory limit too low. The rest of the report is starfixd's own memory and stands | On the server: `systemctl status dolt`, `journalctl -u dolt`, and the report's `Dolt` lines ([Dolt's memory](#dolts-memory)). With `(the read failed; see starfixd's log)` instead, the log's `count the sessions seen, for status` has the error |
| `sfx`: `the server's database does not answer, so project "…" was not opened`, from `sfx admin status` too | Dolt did not answer a ping within 2 seconds, and the project's store is closed: starfixd does not open a store on a Dolt that hangs, and a connection is to a project ([When Dolt does not answer](#when-dolt-does-not-answer)). The log has `dolt.check_failed` with the ping's error | Run `sudo -u starfix -H starfixd status` on the server, or `sfx admin status` from the repository of a project in use; then see to Dolt: `systemctl status dolt`, `journalctl -u dolt` |
| `sfx`: `the server timed out on …` after a minute, while `sfx admin status` answers | Dolt does not answer. The status report does not wait for it; every other request does, for the request timeout | `sfx admin status`, or `starfixd status` on the server, for what starfixd knows; then see to Dolt |
| `starfixd status`: `status: the daemon did not answer` | starfixd welcomed the connection and then sent no report for 30 seconds, or closed it: the daemon itself is stuck or stopping, since the report does not wait on Dolt | `journalctl -u starfixd` for what it waits on; `systemctl restart starfixd` if that does not end |
| `/readyz` answers `not ready: the last check of Dolt failed` | Dolt hangs or cannot be reached | `systemctl status dolt`, `journalctl -u dolt`; starfixd's log has the ping's error |
