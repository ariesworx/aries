# Running a server

An Aries server is one Linux machine running two programs, and a third
when SSH is on:

| Program | Role |
|---|---|
| `ariesd serve` | The daemon. It owns the data and applies every change in order, and its [HTTPS listener](#the-https-listener) is the front door: it checks each person's or agent's key, and serves them |
| sshd | With SSH on ([SSH on and off](#ssh-on-and-off)): a second front door. It checks each key and runs `ariesd stdio` for it |
| Postgres | The database: on the same machine, where ariesd reaches it by its unix socket, or on another machine or a managed service ([Managed services](#managed-services)) |

A server holds one project, as the steps below set it up. It can be
switched to hold many, each in a schema of its own in the one database
([Serving many projects](#serving-many-projects)). This page sets a
server up step by step, then covers principals, settings, limits, logs,
metrics and health ([Observability](#observability)), Postgres
([sizing](#sizing-postgres), [managed services](#managed-services),
[standby and failover](#standby-and-failover)), upgrades, backups and
troubleshooting. For complete deployments with costs, see the
[deployment examples](deploy/README.md).

## What a server needs

- **Linux**, amd64 or arm64, with systemd and OpenSSH. `ariesd serve`
  refuses to run on other systems, because only Linux lets it check which
  user connects to its socket.
- **Ubuntu 24.04 LTS and 26.04 LTS are the supported platforms.** The
  steps below are written for both and were run on both
  ([What was run, and where](#what-was-run-and-where)). amd64 was run on
  GitHub's runners, and cloud providers' images were not. Other distributions, and Ubuntu's releases
  between the two, may work and are untested. One text serves both; where
  26.04 behaves differently the guide says so: its `sudo` is sudo-rs, its
  sshd turns away for a while an address that fails to log in
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
- **Postgres 16 or newer.** ariesd refuses an older one. Aries is tested
  with Postgres 18, at the release in `Postgres` in
  [internal/version](../internal/version/version.go), and each release's
  notes name it. The steps install Postgres 18 from the PostgreSQL
  project's own apt repository; a Postgres 16 or newer that the machine
  already runs, another machine's, or a managed service's serves as
  well ([Managed services](#managed-services)).
- **Little hardware.** Idle, ariesd used about 20 MB of memory, or 22 MB
  with metrics pushed and scraped (resident memory on arm64, a minute
  after its start; about 14 MB of either is the program itself, mapped
  from its file). Each connection adds about 5 MiB more: 2.5 MiB for its
  bridge (`ariesd stdio`), 2 MiB for sshd's two processes, half a MiB
  in the kernel and under 0.1 MiB in ariesd, so 64 connections take about
  320 MiB and 1,024 about 5 GiB. That is with transparent huge pages in
  `madvise` mode, Ubuntu's default; with them `always` (`cat
  /sys/kernel/mm/transparent_hugepage/enabled`) the bridge takes about
  6 MiB, and a connection about 9 (measured on Linux, arm64, OpenSSH 9.6,
  14 October 2026; [the profile](design/profile-ariesd.md#2-a-bridge-that-is-not-the-whole-of-ariesd)
  has how). Postgres took about 50 MiB idle, and about 2.5 MiB more for
  each project in use up to 20 and 0.6 MiB for each after, through
  ariesd's one pool of connections ([Sizing Postgres](#sizing-postgres)
  has the measurements). 2 GB of memory suits a team with a few projects
  of a few thousand issues each.

## Set up a server

Run these steps as root (`sudo -i`). Each step ends with a command that
shows it worked, and none puts a password on a command line. On a fresh
Ubuntu 24.04 LTS or 26.04 LTS machine, the installer does them for you
([below](#with-the-installer)); the steps stay the reference for what
it does and why.

### With the installer

`install-server.sh`, beside `install.sh` at the top of the repository,
does the steps of this section, and those of [Opt in](#opt-in),
[The HTTPS listener](#the-https-listener) and, with SSH on,
[Keys from the store](#keys-from-the-store), on a fresh Ubuntu 24.04 LTS
or 26.04 LTS machine. It sets up a server of [many projects](#serving-many-projects)
from the start: admins add people and projects with `aries admin`, from
anywhere. By default it serves HTTPS alone, with a certificate from
Let's Encrypt, and no SSH ([Profiles](#profiles)). Copy the first
admin's public key to the machine, read the script, and run it as root,
first with `--plan` and `--dry-run`:

```sh
curl -fsSLO https://raw.githubusercontent.com/ariesworx/aries/main/install-server.sh
sudo sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com --plan
sudo sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com --dry-run
sudo sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com
```

It prints each step as it goes, each thing it finds already done, and
each command it runs; it ends with the `.aries.yaml` to commit
([step 7](#7-connect-a-repository)): the server's URL and, with SSH on,
the host key sshd presents now. [Rotating the host key](#rotating-the-host-key)
says how to change that key later without refusing every client.

| Option | Does |
|---|---|
| `--admin NAME` | The first server admin: named under `admins:`, and given its key in the control store |
| `--admin-key FILE` | That admin's public key, a `.pub` file. It signs in over HTTPS ([How aries signs in over HTTPS](#how-aries-signs-in-over-https)), and over SSH when SSH is on |
| `--host HOST` | The server's DNS name or address, for the URL and `.aries.yaml`, and the name ACME gets a certificate for. Default `hostname -f` |
| `--profile P` | `internet`, the default, `private`, `airgapped` or `ssh-only` ([Profiles](#profiles)) |
| `--enable-ssh` | Serve SSH beside HTTPS ([SSH on and off](#ssh-on-and-off)) |
| `--disable-ssh` | Turn SSH off on a server that served it: remove every SSH path an earlier run set up ([SSH on and off](#ssh-on-and-off)). Set `transports:` without `ssh`, and restart ariesd, first |
| `--url URL` | The `https://` URL clients use, `server.url` and `public_urls:`. Default `https://HOST`, with the port when clients reach the listener on another than 443 |
| `--listen ADDR` | The listener's `HOST:PORT`. Default `:443`, or `127.0.0.1:8443` behind a proxy |
| `--tls MODE` | `acme`, `files` or `proxy`, in place of the profile's ([Certificates](#certificates)) |
| `--cert FILE`, `--key FILE` | With `files`: the certificate chain and its key, in PEM. Copied to `/etc/aries/tls/cert.pem` and `/etc/aries/tls/key.pem`, the key mode 0600 and never shown |
| `--acme-email ADDR`, `--acme-directory URL` | With `acme`: an address the ACME server may write to, and an internal ACME server's directory URL in place of Let's Encrypt's |
| `--trusted ADDRS` | With `proxy`: the proxy's addresses or networks, comma-separated, which alone may connect. Default `127.0.0.1` |
| `--port PORT` | With SSH on: sshd's port, for `.aries.yaml`, if not 22 |
| `--plan` | Print what the options serve and which files the run would write, and stop: before it checks the machine, so it runs anywhere |
| `--project NAME` | The name the first project is adopted under (`control init --name`). Default `first` |
| `--version vX.Y.Z` | The release of `ariesd` and `aries` to install. Default the latest |
| `--releases SOURCE` | Where releases are read from ([Install](install.md#where-releases-are-read-from)), also written to the config file as `releases:` for `ariesd upgrade`. Default `$ARIES_RELEASES`, or GitHub |
| `--dsn URL` | Use the Postgres this `postgres://` URL names, on another machine or a managed service, in place of installing one: [Managed services](#managed-services). Its role and database are made beforehand. No password in it: the installer refuses one |
| `--db-password-file FILE` | With `--dsn`: a file holding the role's password alone on one line. The installer copies it to `/etc/aries/postgres/pgpass`, mode 0600, which the DSN it writes names |
| `--apparmor` | Also confine ariesd ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)). Off unless given |
| `--dry-run` | Print every action, and change nothing |

Its steps, and the sections of this guide each one follows:

| Step | | Follows |
|---|---|---|
| 1 | Check the machine | [What a server needs](#what-a-server-needs): Ubuntu 24.04 or 26.04, amd64 or arm64, systemd, OpenSSH |
| 2 | Install Postgres | [1. Install Postgres](#1-install-postgres), with the key of the apt repository checked against its fingerprint for you. A Postgres 16 or newer that already runs on the machine is used as it is. With `--dsn`, only Postgres's client programs, for the checks and the [backups](#backups-and-restore) |
| 3 | Create the role and the database | [2. Create the role and the database](#2-create-the-role-and-the-database). With `--dsn`, it prints the statements for the database's administrator to run ([Managed services](#managed-services)) |
| 4 | Install ariesd | [3. Install ariesd](#3-install-ariesd), by `install.sh`, which checks the signature and the checksum; and `aries` in `/usr/local/bin` |
| 5 | Write the config file | [4. Write the config file](#4-write-the-config-file), [Opt in](#opt-in) step 1: the control schema, and the profile's `transports:` and `https:` ([The HTTPS listener](#the-https-listener)). It then signs in to the database as ariesd will, and checks its version and `max_connections`. On a rerun it leaves the file as it is, and says what to set when the options serve other transports |
| 6 | Set up TLS | [Certificates](#certificates): the files of `--cert` and `--key`, or ACME's cache; behind a proxy, nothing ([Behind a proxy](#behind-a-proxy)). Skipped with `ssh-only` |
| 7 | Run ariesd under systemd | [5. Run ariesd under systemd](#5-run-ariesd-under-systemd); the drop-ins of [Certificates](#certificates) for a port below 1024 and for ACME; with SSH, [Keys from the store](#keys-from-the-store) step 1. It lets the HTTPS port through `ufw` when `ufw` is active, and says so |
| 8 | Let the first admin in | With SSH, [6. Let the first principal in](#6-let-the-first-principal-in); without, the line in `/etc/aries/admin.keys` ([SSH on and off](#ssh-on-and-off)) |
| 9 | Have sshd read the keys file | With SSH, [Keys from the store](#keys-from-the-store) step 2, and step 6's drop-in; skipped without ([SSH on and off](#ssh-on-and-off)) |
| 10 | Confine ariesd with AppArmor | [Confine ariesd with AppArmor](#confine-ariesd-with-apparmor), with `--apparmor` |
| 11 | Adopt the database as the first project | [Opt in](#opt-in) step 2, with the first admin's key from step 8's file; with `--disable-ssh`, also the keys of the `aries` account's `authorized_keys` |
| 12 | Remove what served SSH | With `--disable-ssh`, [SSH on and off](#ssh-on-and-off): sshd's drop-in, `authorized_keys`, the keys file and its drop-in, and the bridge's AppArmor profile; skipped without |
| 13 | Start ariesd | [5. Run ariesd under systemd](#5-run-ariesd-under-systemd), and with SSH [Keys from the store](#keys-from-the-store) step 3 |
| 14 | Connect a repository | [7. Connect a repository](#7-connect-a-repository) |

- **Run it again at any time.** Each step checks before it acts, so a
  second run changes nothing and ends `Nothing to change`, and a run
  that was interrupted is finished by running it again. `control init`
  runs each time, as [Opt in](#opt-in) allows, and says that it added
  nothing.
- **It changes no file it did not write.** Where a file it writes
  already exists with other text, it shows the difference and stops,
  and you choose: make it the guide's, or move it aside and run again.
  Of the files it did not create, it only appends the admin's line to
  `authorized_keys`, and prints the line; `--disable-ssh` removes that
  file once its keys are in the control store. It changes no role or database
  it did not make: a role `aries` with more rights than step 2 gives, or
  a database `aries` that another role owns or whose collation is not
  `C`, stops it with the fix.
- **A default install exposes no SSH path.** Without `--enable-ssh` or
  `ssh-only` it writes nothing sshd reads for the `aries` account, and
  ariesd refuses the bridge; `--plan` lists what each profile writes,
  and a test holds each profile to it. `--disable-ssh` takes a server
  that had SSH on back to that, and `--plan` lists what it removes.
- **sshd is checked before it is reloaded.** With SSH on, the drop-in is
  checked with `sshd -t`; if that fails, the new file is removed again
  and sshd is not reloaded.
- **No secret is on a command line or in its output.** With Postgres on
  the machine there is no password at all: ariesd's user signs in over
  the unix socket as the role of its own name. With `--dsn`, the only
  password is the one `--db-password-file` holds: the installer refuses
  a `--dsn` that holds one, reads the file, writes its line to a file of
  mode 0600 that the `aries` user owns, and never prints it or a
  difference from what is there. The SQL travels on stdin, as the
  `postgres` user.
- **The files it writes are this guide's,** byte for byte: ariesd's unit
  and its drop-ins, the sudo rule and sshd's drop-in; and the SQL it
  runs is step 2's. A test (`scripts/installserver_test.go`) fails when
  they differ from the blocks below, or when its steps and this table
  do. The files whose text it makes are the apt source of step 1, which
  names this release of Ubuntu, and the config file, from the options;
  it copies the certificate and the key of `--cert` and `--key`, and
  never shows the key or a difference from what is there.
- **Where its parts come from.** `install.sh` and the AppArmor profiles
  are read from beside it when it runs from a checkout of Aries, and
  otherwise from `raw.githubusercontent.com`, as the steps download
  them. It forwards `ARIES_RELEASES` (or `--releases`) to `install.sh`.
  Postgres comes from `apt.postgresql.org`, whose key is checked against
  the fingerprint the installer holds, which a test keeps equal to CI's.
- **What it leaves to you:** a DNS name that leads to the machine, for
  ACME; the proxy, behind one ([Behind a proxy](#behind-a-proxy)); the
  firewall beyond the HTTPS port it opens in an active `ufw`; with SSH,
  `ufw limit 22/tcp` (step 6), `PasswordAuthentication no` and, on
  26.04, `PerSourcePenaltyExemptList`,
  [telemetry](#observability), [backups](#backups-and-restore),
  [tuning Postgres](#sizing-postgres), and upgrades (`ariesd upgrade`).
  With SSH, keep the admin's line in `authorized_keys`: it is the way in
  when nothing else is ([Keys from the store](#keys-from-the-store),
  step 5). Without, `/etc/aries/admin.keys` is what `control init`
  takes the admin's key from again, should it be removed from the store.
- **It refuses** another system, a machine without systemd, a user other
  than root (`--dry-run` included, since it reads root's files), and a
  machine set up some other way: more than one Postgres cluster, one
  older than 16, accounts with other homes or shells, a config file it
  did not write or whose `dsn` is not the one it would write. Each
  refusal says what to do.

What was run, on 15 October 2026, with `ariesd` and `aries` built from
the source tree for linux/arm64 and installed by `install.sh` from a
release tree in a directory, signed with a test key, since the
installer is newer than any release: to repeat it, run the installer
from a checkout whose `install.sh` holds the test key in place of the
release key.

- **In Docker** (Docker Desktop on an Apple silicon Mac), Ubuntu 24.04.5
  with systemd 255 as its first process, and Ubuntu 26.04.1 with
  systemd 259, each a fresh container with what a cloud image has
  (openssh-server, curl, openssl, sudo, gnupg): a dry run that changed
  nothing; a run, which checked the repository's key, installed PGDG's
  `postgresql-18` (18.6), made the role and the database, and did the
  rest (24 changes, on 26.04); then a rerun and a dry run that found
  nothing to change. On 24.04 the base image's `policy-rc.d` kept the package from
  starting its cluster, and the installer started it. That run stopped
  at step 3 on a fault of the installer's, since fixed, and the next run
  finished it, finding and adding the `REVOKE` of step 2 that a second
  fault had left out. Then, from the Mac, through the container's sshd:
  the first admin connected, created an issue, added a project and a
  member with a key only the control store holds; the member was
  refused the second project, and once the key was removed, refused at
  sshd. `systemd-analyze verify` accepted the unit, and `systemd-analyze
  security` scored it 7.2, as before. A restart of Postgres under
  systemd left ariesd running, with the same process and no restart;
  after a restart of the container, the three units were active, with
  no failed start.
- **With `--dsn`**, on Ubuntu 26.04.1 the same way, against the official
  `postgres:18` image in another container on the same network, its
  role and database made with a password as
  [Managed services](#managed-services) says: the installer installed
  only `postgresql-client-18`, printed the administrator's statements
  of [Managed services](#managed-services), wrote the password file, mode 0600 and owned by
  `aries`, and a DSN that names it, and signed in with it; a rerun
  changed nothing; a run with another `--dsn` was refused; the password
  was in neither the installer's output nor the journal. The same
  `aries` checks passed.
- **Before Postgres**, on 14 October 2026, the installer of that time
  was run the same way in Docker, and in virtual machines of 24.04 and
  26.04 with `--apparmor`, across cold boots
  ([What was run, and where](#what-was-run-and-where)).

Not run: amd64; a real release downloaded from GitHub, and
`raw.githubusercontent.com` as the source of `install.sh` and the
profiles (each run read them from a checkout); a virtual machine or a
cloud provider's image with this installer, and so AppArmor enforced
and a reboot; `--dsn` against a managed service.

### 1. Install Postgres

From the PostgreSQL project's apt repository (PGDG), which publishes
every supported release of Postgres for every Ubuntu LTS. Check the
repository's key before apt trusts it:

```sh
apt-get install -y curl gnupg
curl -fsSL -o /tmp/pgdg.asc https://www.postgresql.org/media/keys/ACCC4CF8.asc
gpg --show-keys --with-colons /tmp/pgdg.asc | awk -F: '$1 == "fpr" { print $10; exit }'
```

The last command must print `B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8`,
the fingerprint the PostgreSQL project publishes, which Aries's CI and
the installer check too. Then:

```sh
install -D -m 0644 /tmp/pgdg.asc /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
. /etc/os-release
echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $VERSION_CODENAME-pgdg main" > /etc/apt/sources.list.d/pgdg.list
apt-get update
apt-get install -y postgresql-18
pg_lsclusters
```

`pg_lsclusters` shows the cluster the package made and started,
`18 main 5432 online`. Its configuration is in `/etc/postgresql/18/main`
and its data in `/var/lib/postgresql/18/main`.

- **It listens on loopback and on its unix socket**,
  `/var/run/postgresql`, and nowhere else (`listen_addresses` is
  `localhost`). Leave it so: ariesd uses the socket.
- **A local user signs in as the role of its own name**, over the
  socket and with no password (`peer` in `pg_hba.conf`, as the package
  writes it). That is how ariesd signs in, as the `aries` user, and how
  root administers Postgres, as the `postgres` user: `sudo -u postgres
  psql`.
- **`signed-by`** has apt trust that key for that repository alone.
- **A Postgres 16 or newer that the machine already runs** serves too,
  as it is, and Ubuntu's own packages of one would (not run here). Skip
  this step, and run step 2 on it.
- **Upgrades of Postgres** within 18 come with `apt-get upgrade`, and
  restart the cluster; ariesd connects again by itself
  ([step 5](#5-run-ariesd-under-systemd)). A new major version is
  `pg_upgradecluster`, after a [backup](#backups-and-restore).

### 2. Create the role and the database

ariesd's role may log in and nothing more, and owns one database, made
with the collation `C`:

```sh
cd /
sudo -u postgres psql -v ON_ERROR_STOP=1 <<'EOF'
CREATE ROLE aries LOGIN;
CREATE DATABASE aries OWNER aries TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LOCALE 'C';
REVOKE ALL ON DATABASE aries FROM PUBLIC;
EOF
sudo -u postgres psql -c '\l aries'
```

- **The role is not a superuser and has no other right**: no
  `CREATEROLE`, `CREATEDB`, `REPLICATION` or `BYPASSRLS`, and no
  membership of the roles that reach the server's files. ariesd checks
  this every time it opens the store, and refuses a role with any of
  them ([The database role](security-model.md#the-database-role)).
- **It owns the database**, and so may make schemas in it: the control
  schema, which `ariesd control init` makes, and a schema for each
  project, which `aries admin project add` makes. It has no right in any
  other database.
- **The collation is `C`,** so lists order ids and titles byte by byte,
  as the tests do, whatever the machine's locale. A database made with
  another collation works, and may order lists differently.
- **`REVOKE … FROM PUBLIC`** keeps every other role from connecting to
  it. Superusers still may.
- **There is no password.** The role signs in by the unix socket as the
  user of its name, which step 3 makes. With Postgres on another machine,
  or a managed service, the role has one: [Managed
  services](#managed-services).

The last command lists the database, owned by `aries`, with `C` in its
`Collate` and `Ctype` columns.

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

### 4. Write the config file

This writes the config file, and signs in to the database as ariesd
will:

```sh
(
  set -e
  umask 077
  install -d -o root -g aries -m 0750 /etc/aries
  printf 'dsn: postgres://aries@/aries?host=/var/run/postgresql\nproject: %s\nsystemd_unit: ariesd.service\nadmins: [alice]\ntransports: [ssh]\n' "$(cat /proc/sys/kernel/random/uuid)" |
    install -o aries -g aries -m 0600 /dev/stdin /etc/aries/ariesd.yaml
)
grep project /etc/aries/ariesd.yaml
cd /var/lib/aries
sudo -u aries -H psql -d 'postgres://aries@/aries?host=/var/run/postgresql' -c 'SELECT current_user, version()'
```

- **`dsn`** is a `postgres://` URL: the role, no host, the database,
  and `host=` naming the directory of Postgres's unix socket. It holds
  no password and needs none. A `search_path=` would name the schema
  that holds this project; with none it is `public`, which the owner of
  the database owns. [Settings](#settings) has the rest of the form.
- **`project`** is the project's UUID. Every repository's `.aries.yaml`
  names it.
- **`admins`** lists the first principal, `alice`, who you let in at step
  6. [Settings](#settings) lists every other key.
- **`transports: [ssh]`** serves SSH alone, which these steps set up.
  Without it a server serves HTTPS alone, the default, and needs
  `https:` ([The HTTPS listener](#the-https-listener)); the
  [installer](#with-the-installer) sets that up. With HTTPS on, the
  keys of step 6's file sign in over HTTPS too
  ([How aries signs in over HTTPS](#how-aries-signs-in-over-https)).
- **The file is mode 0600**, owned by `aries`, although it holds no
  secret here: a `dsn` may hold a password, or name a file that does
  ([Managed services](#managed-services)).

The last command answers with `aries` and the server's version: ariesd's
user can sign in. `role "aries" does not exist` means step 2 has not
run; `Peer authentication failed` means the command did not run as the
`aries` user.

### 5. Run ariesd under systemd

Save this as `/etc/systemd/system/ariesd.service`:

```ini
[Unit]
Description=Aries server
After=network.target postgresql.service time-sync.target
Wants=postgresql.service time-sync.target

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
| `WatchdogSec=120` | ariesd must say it is alive at least every 120 seconds, or systemd kills it and `Restart=on-failure` starts it again. ariesd says so every 60, half the interval, from the goroutine that runs its 30-second reaper, so a daemon whose reaper hangs is restarted. One tick of the reaper that takes longer than `WatchdogSec` is a hang too, and a database that has stopped answering can cause one: each of the reaper's writes waits up to a minute |
| `postgresql.service` in `After=` and `Wants=` | Start Postgres with ariesd, and ariesd after it. On Ubuntu that unit is reached once each cluster's own unit (`postgresql@18-main.service`) has started, and that one only once the cluster accepts connections. Where Postgres is on another machine there is no such unit, and the two do nothing |
| `Wants=time-sync.target` and `time-sync.target` in `After=` | Order ariesd after the clock is synchronized, on a machine where something provides that target. Stock Ubuntu, 24.04 or 26.04, reaches it at once, so there the lines wait for nothing ([Start ariesd after the clock is set](#start-ariesd-after-the-clock-is-set)) |
| `RestartPreventExitStatus=75` | Leaves ariesd stopped when it refuses to start because the clock is behind its own history, which no restart can correct ([The server's clock](#the-servers-clock)). It exits with that status 0.13 seconds after systemd starts it, before it would tell systemd it is ready, so the start fails then and not when `TimeoutStartSec` runs out. Every other failure is still restarted |

Without the first four lines ariesd runs as before: it sends a
notification only when systemd asks for one.

What the lines showed on the machines of
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
  ([Troubleshooting](#troubleshooting)). Profiles from before Postgres
  do not let it reach Postgres's socket.

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

**ariesd outlives a restart of Postgres,** and the unit does not stop
it with Postgres (it has no `Requires=`). While Postgres is down,
requests fail with `unavailable` and [`/readyz`](#the-listener-metrics-to-scrape-and-health)
says why; when it is back, ariesd's pool connects again, and its next
write takes back the [writers' locks](#one-writer-for-each-project), with
no restart of ariesd. Run on Ubuntu 24.04 and 26.04 booted by systemd
in containers, on 15 October 2026: `systemctl restart
postgresql@18-main` under a running ariesd left it running, with the
same process and no restart, and a write made at once after succeeded.
In the [Docker test server](deploy/docker.md) the same held, and
ariesd's log had one `agent.register_failed` for a pooled connection
the restart had ended. Since 16 October 2026 the pool looks at each
connection before it hands it out, without a round trip, and makes
another in place of one Postgres has ended, so a restart between
requests logs nothing; a request in flight at the moment of the restart
still fails ([Troubleshooting](#troubleshooting)).

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
- `DisableForwarding` means no tunnel to Postgres, or anywhere else,
  even from a key line that lacks `restrict`.
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
ar-aukz3rip
$ aries ready
ar-aukz3rip  P1  open  Fix the login redirect
```

[Using `aries`](cli.md#connect-a-repository) explains every field of the file,
and the [agent guide](agents.md) sets agents up in the repository.

### What was run, and where

**On GitHub's runners**, on 16 October 2026, from `main` at c4b311d: the
[`p5-machine` workflow](../.github/workflows/p5-machine.yml), run
38978641331, on `ubuntu-24.04` (amd64) and `ubuntu-24.04-arm` (arm64).
Each job is a fresh virtual machine with root, systemd and AppArmor, on
Ubuntu 24.04 with Postgres from PGDG, installed by the installer; Ubuntu
26.04 was not run there.

| | |
|---|---|
| Machines | GitHub's `ubuntu-24.04` and `ubuntu-24.04-arm` runners |
| ariesd | Built from the source tree at c4b311d for each machine, with the release's flags, and put where the installer looks first |
| Clients | `aries` on the same machine, over HTTPS to `ariesd`, with a certificate from a throwaway CA |

- **The installer**, `install-server.sh --profile private` as root, on
  both. A second run changed nothing.
- **AppArmor**: the profiles `ariesd-serve` and `ariesd-admin` loaded in
  enforce mode, and `ariesd` ran under systemd labeled `ariesd-serve
  (enforce)`. The job's check of the journal found no line that says an
  ariesd profile denied anything.
- **aries over HTTPS**, before and after a restart of `ariesd` under its
  profile.

Not run there: a cloud provider's own image, which the runners are not,
and a managed Postgres.

**On Postgres**, on 15 October 2026 (slice P4 of decision 79):

| | |
|---|---|
| Machines | Containers booted by systemd, on Docker Desktop on an Apple silicon Mac (arm64): Ubuntu 24.04.5 (systemd 255, OpenSSH 9.6p1, sudo 1.9.15p5) and Ubuntu 26.04.1 (systemd 259, OpenSSH 10.2p1, the original sudo as `sudo.ws`), with Postgres 18.6 from PGDG |
| ariesd | Built from the source tree for linux/arm64, and installed by the installer from a release tree signed with a test key |
| Clients | `aries` on the Mac, through the port Docker forwards to sshd |

- **The installer**, on both, with Postgres on the machine and with
  `--dsn` ([With the installer](#with-the-installer)). It writes
  steps 4 to 6's files as they are written here, byte for byte, and runs
  step 2's statements, so those steps ran as written.
- **ariesd's unit**: `systemd-analyze verify` accepted it; Postgres
  restarted under it ([step 5](#5-run-ariesd-under-systemd)); the
  container restarted, with every unit active after.
- **[Backups and restore](#backups-and-restore)**, as written, on 24.04:
  the timer and the service, a dump, a restore of the whole database
  and of one project.
- **The [Docker test server](deploy/docker.md#run-the-smoke-test)**, whose
  Postgres is another container's: its smoke test, plain (155 checks),
  with the telemetry harness (212) and with the identity provider (189).

Not run on Postgres: a virtual machine, so neither a real boot nor
AppArmor enforced (Docker Desktop's kernel has no AppArmor; the profiles
were only parsed, [Confine ariesd with AppArmor](#confine-ariesd-with-apparmor));
the clock's checks below; amd64; a cloud image; a managed service.

**Before Postgres**, the steps were run in order as they were then
written, with the database Aries used until decision 79, on virtual
machines on an Apple silicon Mac (Lima 2.2.1, arm64, 2 CPUs and 4 GiB,
from Ubuntu's cloud images), and the installer on both:

| | |
|---|---|
| Ubuntu 24.04, 12 to 14 October 2026 | Ubuntu 24.04.5, kernel 6.8.0-142, systemd 255, OpenSSH 9.6p1, sudo 1.9.15p5, AppArmor 4.0.1 |
| Ubuntu 26.04, 14 October 2026 | Ubuntu 26.04.1, kernel 7.0.0-34, systemd 259, OpenSSH 10.2p1, sudo-rs 0.2.13 as `sudo` (the original, 1.9.17p2, installed as `sudo.ws`), Rust coreutils 0.10, chrony with NTS, AppArmor 5.0.2 |
| ariesd | Built from the source tree for linux/arm64 with the release's flags, and put where step 3's installer puts a release, or installed by the installer from a release tree signed with a test key |
| Clients | `aries` inside the machine, against `localhost`, as three Unix users with a key each; and `aries` on the Mac, through the port Lima forwards to sshd |

What they showed does not depend on the database, and holds for these
steps, which changed only in what they say of it:

- **ariesd's unit works as written.** `systemd-analyze verify` accepts
  it, and `systemd-analyze security` scores it 7.2 ("medium"; lower is
  tighter): it runs as its own user with a read-only file system, and
  nothing limits its system calls, capabilities or namespaces. Its
  `Type=notify`, start timeout and watchdog worked as
  [step 5](#5-run-ariesd-under-systemd) says, with the AppArmor
  profiles enforced, across a cold boot.
- **sshd's `Match` block works on stock Ubuntu.** The forced command
  runs in place of whatever command the client asks for, a terminal is
  refused (`PTY allocation request failed`), a tunnel is refused
  (`open failed: administratively prohibited`), and a certificate from
  an authority trusted for every account is refused for `aries`
  ([step 6](#6-let-the-first-principal-in)). sshd listens through
  `ssh.socket` on both; systemd 259 also makes it answer on `AF_VSOCK`
  in a virtual machine (`sshd-vsock.socket`), with the same
  configuration.
- **Every command the steps and the installer use works under sudo-rs
  and Rust coreutils as under sudo and GNU coreutils**: `sudo -u USER -H`
  with `env` and standard input, `visudo -c -f` (sudo-rs's prints
  `parsed OK` even with `-q`), the sudo rule for `systemctl restart`
  from the `aries` user, and refusing anything else; `install` with
  `-o`, `-g`, `-m` and `-d`, and from `/dev/stdin` and `/dev/null`;
  `stat -c`, `mktemp -d`, `cut -s`, `tail -c`, `readlink`, `ln -sfn`.
  Ubuntu keeps GNU's `mv` (and `cp` and `rm`).
- **The loopback listener, metrics pushed to a receiver on the machine,
  and `ariesd status`** work ([Observability](#observability)).
- **Traces work as written** ([Traces](#traces)), with the profiles
  enforced. With the guide's one variable and nothing else ariesd
  starts and serves, posts spans to a receiver on the machine that hold
  no stand-in and no name, and says why at error level, in its status
  report and in `whois`: `it cannot be made: no such file or
  directory`. With the directory made and no restart, half a minute
  later: `read-only file system`. With the drop-in and a restart, under
  a profile without the rule for the file: `permission denied`, with the
  denial in the kernel's log. With the rule loaded and no restart, half
  a minute later: `made the key`, mode 0600, `spans name people by
  stand-ins from now on`, and spans with a stand-in. `aries admin
  principal whois` then resolves it for a server admin. With the file
  made readable by others and a restart: no stand-in again, `CANNOT BE
  TRUSTED` in the status; after `chmod 600` and no restart, the same
  stand-in as before. A restart keeps the key, deleting the file gives
  the same person another stand-in and leaves the old one resolving to
  nobody, `principals: name` and `OTEL_TRACES_EXPORTER=none` need no
  key, and neither another user, nor the `aries` user under the bridge's
  or the operator's profile, can read the key.
- **The clock's unit lines and settings work** ([The server's
  clock](#the-servers-clock)), with the profiles enforced and no denial
  in the kernel's log: the refusal to start and exit status 75 under
  `Type=notify`, with and without `RestartPreventExitStatus=75`; each
  way on from it; the clock set back and forward under a running daemon
  with claims held; boots with and without `systemd-time-wait-sync`, and
  with no time server answering; `upgrade --check`'s `clock:` line; and,
  on 26.04, `time-sync.target` and `chrony-wait.service`
  ([Start ariesd after the clock is set](#start-ariesd-after-the-clock-is-set)).
- **`ariesd upgrade --rollback` works for the `aries` user**: it
  renames the binaries in `/opt/aries/bin`, restarts the unit through
  the sudo rule and finds the daemon healthy, under the operator's
  profile, with sudo-rs and with `sudo.ws` as `sudo` on 26.04 once the
  profile names both ([Confine ariesd with
  AppArmor](#confine-ariesd-with-apparmor)). `upgrade --check` reaches
  GitHub and compares versions.

Not run, before or on Postgres: amd64; a cloud provider's image or
network; a real OpenTelemetry Collector on a machine of the guide, or a
hosted backend, in place of which a few lines of Python received the
pushes (an OpenTelemetry Collector, 0.162.0, receives and decodes the
spans of the [Docker test server](deploy/docker.md#telemetry)); an
upgrade to a newer release, which a build from source refuses (`a
development build … never upgrades itself`), so the download, the
signature check and the swap to a new binary were not run. For the
clock: a machine with a real-time clock of its own, working or dead (a
guest's comes from the hypervisor at every boot and was always right);
a boot with the clock wrong; a virtual machine paused and resumed, or
restored from a snapshot; a provider's time service.

## The HTTPS listener

HTTPS is the transport a fresh server serves; SSH is a second one, off
until an operator turns it on ([SSH on and off](#ssh-on-and-off);
design: [transport.md](design/transport.md)). The listener is part of
`ariesd serve`, beside its unix socket. It serves these paths, and with `login:` providers those of
[Sign-in](#sign-in), and answers 404 for every other:

| Path | Serves |
|---|---|
| `GET /.well-known/aries` | What a client may know before it signs in: the server's version, the transports on, how one signs in (`key`, and `signin` with providers) and the providers' names. No principal and no secret |
| `GET /v1/connect` | A WebSocket that carries the same frames `aries` sends over SSH. Nothing on it reaches the protocol until a sign-in's token with its key's proof, or a key the server knows, has proved itself |

The loopback listener's paths (`/metrics`, `/healthz`, `/readyz`), the
status socket and the unix socket the SSH bridge connects to stay where
they were; none is served over HTTPS. Every response says to use HTTPS
alone (`Strict-Transport-Security`), HTTP/2 is offered for everything
but the WebSocket, which is HTTP/1.1's, and a request from a browser's
page (one that says its `Origin`) is refused.

```yaml
transports: [https]                 # the default; [https, ssh] or [ssh]
https:
  listen: ":443"
  public_urls: [https://aries.example.com]
  tls:
    mode: acme                      # acme, files or proxy
    min_version: "1.2"              # or "1.3"
    acme:
      domains: [aries.example.com]
      email: ops@example.com
      cache: /var/lib/aries/acme
      directory: ""                 # empty: Let's Encrypt
    files:
      cert: /etc/aries/tls/cert.pem
      key: /etc/aries/tls/key.pem
    proxy:
      trusted: [127.0.0.1]
  refuse_key_proof_v1: false        # true refuses v0.5.0's aries's key proof
```

- **`listen`** is `HOST:PORT`; `:443` listens on every address. A port
  below 1024 needs the capability below.
- **`public_urls`** are the URLs clients have in `server.url`. ariesd
  checks what a client proves against them, never against the request's
  `Host` or an `X-Forwarded-*` header: a proxy may rewrite those (it was
  seen to, [spike/https](../spike/https/RESULTS.md)).
- **Only the `tls:` block of the mode is read.** `ariesd serve` refuses
  to start with HTTPS on and `https:` unset, naming both fixes: set it,
  or `transports: [ssh]`.
- **`refuse_key_proof_v1`** refuses the key proof of `aries` of
  protocol 8 (v0.5.0), which binds nothing of the TLS session (below).
  Off by default, so that those clients keep working until `ProtoMin`
  moves to 9, which v0.7.0 did: a v0.7.0 server refuses protocol 8's
  clients at the handshake whatever this says, so the setting matters
  only to a v0.6.0 server. A client it refuses is told, with code
  `version`, to run `aries upgrade`.

### How aries signs in over HTTPS

A connection over HTTPS is let in one of two ways. With a sign-in kept
on the machine (`aries login`, [Sign-in](#sign-in)), `aries` presents
its token on the upgrade, `Authorization: DPoP TOKEN`, with a proof by
the token's key (`DPoP: PROOF`) that names `/v1/connect` at the server's
URL as `.aries.yaml` has it; the server checks both before it upgrades,
against `public_urls`, never `Host`, and refuses with a 401 that `aries`
prints as it prints any refusal. Without one, it proves it holds one of
the SSH keys the server already knows (decision 89): the keys
`aries admin key add` records, and on a server of one project the lines
of `authorized_keys` that force `ariesd stdio --principal NAME`. After
the upgrade `aries` offers the keys it would offer sshd (the one key
`ARIES_KEY_FILE` or `key:` names, or else the default identities and
ssh-agent's), the server challenges the first it knows with 32 random
bytes, and the key signs them with the server's URL; the server checks
the signature, and that the URL is one of `public_urls`. A proof cannot
be replayed on another connection, since the challenge is that
connection's, nor relayed to another server, since its URL is signed.
Where ariesd terminates TLS (`tls: files` or `acme`), the proof is also
bound to the TLS session (RFC 9266 `tls-exporter`), so that whoever
stands between with a certificate the client trusts cannot relay it to
ariesd: a proof made on another TLS session is refused with
`the key proof does not verify for this TLS session`, and a fix that
names a proxy that inspects TLS, the usual cause, and says to connect
where nothing inspects it, to have the server's name exempted, or to
use SSH. Behind a proxy (`tls: proxy`) nothing is bound, since ariesd
never sees the client's session. v0.5.0's `aries`, of protocol 8, binds
nothing either, and is served until `refuse_key_proof_v1` is set
([security-model.md](security-model.md#the-https-listener)).
The connection is then the key's principal's, with the key's
fingerprint as a connection through the keys file has it: removing the
key, disabling the principal or ending its membership closes it, as it
does an SSH connection. `aries whoami` says which key signed in.

A key the server does not know is refused as sshd refuses one, with
the keys offered; past `auth_failures_per_minute`, an address is
refused `busy` until its bucket refills ([Limits](#limits)). A key of
a disabled principal, once proved, is refused as that principal's
(`forbidden`, with the fix to enable it) and is not counted against
the address, so that a disabled person's agents, still trying, do not
shut out everyone else behind the same address.

### Sign-in

With providers under `login:`, a person signs in at the company's
identity provider with `aries login` ([cli.md](cli.md#sign-in)), and
the server issues a token of its own for the day (design:
[transport.md §6](design/transport.md#6-tokens)). Sign-in needs a
server of many projects, whose [identities](#identities-and-invitations)
say who signs in, and HTTPS; `ariesd serve` refuses providers without
either.

```yaml
login:
  token_lifetime: 24h             # a sign-in's life; at most max_token_lifetime
  max_token_lifetime: 7d          # at most 7d
  fresh_auth: 10m                 # the provider's auth_time must be this recent; sent as max_age
  max_token_age: 10m              # the provider's ID token's iat no older
  skew: 30s                       # between the provider's clock and the server's; at most 5m
  jwks_refetch: 1m                # the least time between two reads of a provider's keys
  prune_after: 1h                 # how long an ended sign-in stays listed
  invite_ttl: 7d
  providers:
    - name: corp                  # what invitations and aries login --provider name
      kind: oidc                  # Keycloak, AD FS, Okta, Google Workspace, any OpenID Connect provider
      issuer: https://sso.example.com/realms/example
      client_id: aries
      client_secret_file: /etc/aries/corp-secret   # 0600; the secret is never in this file
      ca_file: /etc/aries/corp-ca.pem              # the internal CA of the provider's certificate, in place of the system's
      domains: [example.com]      # the token's address must be at one of these, and verified
      require: {hd: example.com}  # claims that must have these values
    - name: corp-offline
      kind: oidc
      issuer: https://sso.example.com/realms/example
      client_id: aries
      client_secret_file: /etc/aries/corp-secret
      discovery: static           # nothing discovered: every endpoint written here
      authorization_endpoint: https://sso.example.com/realms/example/protocol/openid-connect/auth
      token_endpoint: https://sso.example.com/realms/example/protocol/openid-connect/token
      jwks_file: /etc/aries/corp-jwks.json         # the provider's key set; no key is fetched (or jwks_url:)
    - name: entra
      kind: entra
      authority: https://login.microsoftonline.com   # the tenant's cloud's sign-in host: required, no default
      tenant: 00000000-0000-4000-8000-000000000000   # the tenant's id; common and organizations are refused
      token_version: 2            # what the app registration issues
      client_id: 00000000-0000-4000-8000-000000000000
      client_secret_file: /etc/aries/entra-secret
```

- **Register the server at the provider** as a confidential web
  application, with one redirect: the first of `public_urls` and
  `/auth/callback`, such as `https://aries.example.com/auth/callback`.
  The server exchanges the provider's code itself (decision 85), with
  the client secret; `aries` never talks to the provider.
- **The provider's keys**: with `discovery: fetch`, the default, from
  the issuer's own discovery document, read at the first sign-in (so
  that a provider that cannot be reached at start delays nothing), and
  fetched again for a key id not known, at most once a `jwks_refetch`.
  With `discovery: static` and `jwks_file`, from the file, and **nothing
  is fetched to check a token**: the offline mode for a site with no
  route out. The code's exchange still calls `token_endpoint`, inside
  the network. Replace the file before the provider signs with a new key
  (Keycloak and AD FS publish the next key ahead); ariesd reads it again
  at the first token of a key it does not hold, at most once a
  `jwks_refetch`, and until then refuses such a token `unavailable`,
  naming the file. Keycloak's key set is at
  `ISSUER/protocol/openid-connect/certs`.
- **Every endpoint is https**, or http to `localhost`, a name under
  `.localhost` or a loopback address, for a provider on the machine in
  testing. A provider's redirect is never followed, and each request is
  bounded to 10 seconds and 64 KiB. Requests to a provider go through
  `HTTPS_PROXY` when ariesd's environment sets one (a drop-in's
  `Environment=`), except for `NO_PROXY`'s hosts.
- **What a token is held to**: RS256 or ES256 alone, never `none`, an
  HMAC or a `crit` header; the issuer exactly, the client id among its
  audiences (and `azp` when there are several), its times within `skew`
  on the server's clock, the nonce of this sign-in, and `auth_time`
  within `fresh_auth`: the server sends `prompt=login` (`prompt_login:
  false` for Google, which has none) so that the person signs in again at
  the provider each time, and a token of an older sign-in is refused.
  Entra's is held to `tid` (the tenant), `ver` (the version), and its
  subject is `oid`; its address counts as verified only with `xms_edov`.
  With `domains`, an address the provider does not say is verified is
  refused, since it may be anyone's. An `iss` on the provider's answer
  (RFC 9207), which most providers do not send, must be the provider's
  issuer; another is refused before its code is spent.
- **Who signs in.** The token's issuer and subject name an identity
  bound before, whatever address the token says; or else its address,
  which the provider says is verified, binds the open invitation of that
  address at that provider ([Identities and
  invitations](#identities-and-invitations)), once. Nobody else signs
  in. Since a bound identity is matched by issuer and subject alone,
  `ariesd serve` refuses two providers with one issuer, which would each
  sign in the other's people past its own `require` and `domains`.
- **The token** is 256 random bits and more, of which only the SHA-256
  is kept, bound to a P-256 key `aries` makes at each sign-in (DPoP, RFC
  9449): presented without a proof by that key it is refused, so a token
  copied off the machine is useless. It lasts `token_lifetime` and
  nothing renews it (decision 58): the next day, `aries login` again. A
  sign-in's scopes are `cli` and `mcp`, or `mcp` alone; an op is checked
  against them at dispatch, and the session `cli` needs `cli` ([Scopes](#scopes)).
- **When a sign-in ends**: at its end, at `aries logout`, when an admin
  ends it (`aries admin signin end ID`), when the identity it was made
  by is removed, or when its principal is disabled. Its connections
  close at once, each told why, and `aries` says to sign in again; the
  connection that asked, when it is one of them, closes once it is
  answered. A disabled principal's sign-ins stay ended when it is
  enabled again: its keys work again, its old tokens do not. Ending a
  membership closes a sign-in's connections to that project as it
  closes any. Should a revocation's write fail after it took effect,
  ariesd closes the connections of any sign-in no longer live on its
  next reaper tick (every 30 seconds).
- **Replay of a proof.** A DPoP proof is accepted once, within a minute
  of the server's time either way; ariesd remembers each for two minutes
  (`dpop_proofs_per_server`). Anyone can sign a proof with a key of
  their own, so a proof is remembered only once its request has earned
  it: a live code with its PKCE verifier at the token endpoint, a live
  token and a proof by that token's key at connect. A flood of proofs
  with neither fills nothing. The memory is the daemon's: several
  daemons in front of one database would need the proofs seen shared, or
  DPoP nonces (transport.md §13), and 1.0 runs one.
- **Refusals**, each with a fix: no invitation for the address
  (`forbidden`, naming `aries admin invite add`); a lapsed invitation;
  an address the provider does not say is verified; an address bound to
  another account at the provider (`auth`); a sign-in at the provider
  that is not fresh (`auth`); a token unknown, ended or presented without
  its key's proof (`auth`, 401, naming `aries login`); the providers'
  keys not to be had (`unavailable`).
- **`aries admin status`** shows each provider, and each workload
  provider ([Service principals](#service-principals)): where its
  endpoints and keys come from, the key ids and when they were read, and
  the last failure; and how many sign-ins are live.

The server's paths for sign-in, with `login:` providers:

| Path | Serves |
|---|---|
| `GET /.well-known/oauth-authorization-server` | The authorization server's metadata (RFC 8414): its endpoints, PKCE S256, DPoP ES256, `iss` in the response |
| `GET /oauth/authorize` | Where `aries login` opens the browser: checks `aries`'s request (its client, its loopback redirect, PKCE, state), and sends the browser on to the provider with the server's own PKCE, state, nonce and `prompt=login` |
| `GET /auth/callback` | Where the provider sends the browser back; the server exchanges the code and sends the browser to `aries`'s loopback address with a code of its own, good for a minute and once, and as `iss` the one of `public_urls` that `aries` named |
| `POST /oauth/token` | Where `aries` spends that code, with its PKCE verifier and a DPoP proof, for the token; and, with `workload:` providers, where a workload's token is exchanged ([Service principals](#service-principals)) |
| `POST /oauth/revoke` | Ends the sign-in a token is of, and closes its connections (RFC 7009); `aries logout` calls it |

With `workload:` providers and no `login:` providers, the server serves
the metadata, `/oauth/token` and `/oauth/revoke` alone.

### Service principals

A CI job, an unattended agent or a cloud workload signs in as a
principal of kind `service` with the token its platform already gives
it, and holds no stored key or secret: `aries login --workload KIND`
([cli.md](cli.md#sign-in), [CI gates](ci.md)). A trust rule in the
control store says which tokens sign in as which service (design:
[identity.md §7](design/identity.md#7-service-principals),
[transport.md §7](design/transport.md#7-service-principals-over-https)).
It needs a server of many projects and HTTPS; `ariesd serve` refuses
`workload:` providers without either.

```yaml
workload:
  audience: https://aries.example.com   # what a job asks its platform for; default the first of https: public_urls
  token_lifetime: 1h              # a sign-in's life when a rule names none
  max_token_lifetime: 6h          # the most any rule gives; at most 1d
  fresh: 2m                       # a platform's token is exchanged within this of its iat; 30s to 10m
  providers:
    - name: github-actions        # what trust rules name
      kind: github-actions
      issuer: https://token.actions.githubusercontent.com   # GHES, or an enterprise's customized issuer: its own
    - name: gcp
      kind: google
      issuer: https://accounts.google.com
    - name: azure
      kind: azure
      issuer: https://login.microsoftonline.com/00000000-0000-4000-8000-000000000000/v2.0
      audience: api://aries.example.com   # the Application ID URI of the app registration that stands for the server
    - name: gha-offline
      kind: github-actions
      issuer: https://ghes.example.com/_services/token
      discovery: static           # nothing discovered (or jwks_url:)
      jwks_file: /etc/aries/ghes-jwks.json
```

A provider's `discovery`, `jwks_url`, `jwks_file` and `ca_file` work as
a `login:` provider's do, and it shares `login:`'s `skew` and
`jwks_refetch`. `ariesd serve` refuses two providers with one issuer,
since a token goes to the provider its issuer names.

- **A service and its rules.** A server admin adds the principal and
  a rule; a trust rule names a service, a provider, and claims the
  platform's token must carry, each with exactly that value:

  ```sh
  aries admin principal add ci-example --kind service --owner alice
  aries admin trust add ci-example-ci --principal ci-example --provider github-actions \
      --claim repository_owner_id=111111111 --claim repository_id=123456789 [--token-lifetime 1h]
  aries admin trust list [SERVICE] | show NAME | set NAME [--claim NAME=VALUE…] [--token-lifetime D] | disable NAME | enable NAME | remove NAME
  ```

  A rule must pin the platform's immutable ids, which `trust add`
  checks by the provider's kind: `repository_owner_id` and
  `repository_id` for `github-actions`; `sub` (the account's numeric
  id) and `email` for `google`; `tid` and `oid` for `azure`. A value is
  matched exactly, as a string: no pattern, no empty value, never `sub`
  alone. More claims narrow it (`ref`, `event_name`, `environment`,
  `job_workflow_ref`; Google's nested
  `google.compute_engine.project_id`, written with dots). 1 to 16
  claims. `set --claim` replaces every claim.
- **Who signs in.** A token signs in as the one service whose active
  rules at its provider it satisfies, by the rule with the most claims;
  rules of two principals that both match it are refused, naming both,
  since the server does not choose. No rule: refused `forbidden`,
  naming the claims of the job's own token that a rule would pin, with
  the `aries admin trust add` to run. A disabled service, or a person,
  signs in by none.
- **The exchange** is OAuth 2.0 Token Exchange (RFC 8693) at
  `/oauth/token`, with a DPoP proof by a key `aries` makes for the job.
  The token is held to its provider's issuer and keys, to the server's
  audience, and to an `iat` within `fresh`. Where the platform lets the
  job choose its token's audience (GitHub Actions, Google), `aries` asks
  for `AUDIENCE/n/THUMBPRINT`, the thumbprint of its DPoP key's, and the
  server takes no other, so a token copied from a job is good for
  nothing without the job's key. Every exchanged token is remembered,
  by a hash of its claims, for as long as it could be taken (`fresh` and
  the skew), and taken once (`workload_tokens_per_server`); one refused
  for the server's sake (`busy`, `unavailable`) may be sent again.
  Azure's, which no key binds, is also refused unless issued, past the
  skew, after the server started, since a restart forgot what was spent.
- **The sign-in** is of scope `ci` ([Scopes](#scopes)): a member's ops
  and a ci gate's report from the session `ci`, never an admin op, a
  waive or a person's gate. It lasts the rule's lifetime, at most
  `max_token_lifetime`, is bound to the job's key, has no refresh token,
  and ends at `aries logout`, at `aries admin signin end`, when the
  service is disabled, and when its rule's claims change, or the rule
  is disabled or removed: its connections close at once. A rule gives at
  most `grants_per_trust_rule` live sign-ins, and a service
  `grants_per_principal`. `aries admin signin list SERVICE` names each
  one's rule.
- **Who may.** Adding, changing and removing rules is a server admin's,
  never for their own principal; a service's owner lists and shows its
  rules. A service never becomes a person while it has rules.
- **The record.** `trust.add`, `trust.set` and `trust.remove` are events
  of the control store, with the rule, its claims and who changed it,
  and lines of the audit stream (`audit.trust.*`, the rule as `target`
  and its service as `subject`). Each sign-in's `signin.grant` names
  the rule and the claims that say which run it was: for GitHub
  `run_id`, `run_attempt`, `workflow_ref`, `actor_id`, `event_name` and
  `ref`; for Google the project and the instance; for Azure the
  resource id. A workload's sign-in is not announced in an inbox.
- **Azure** needs an app registration that stands for the server: its
  Application ID URI is the provider's `audience`, for which the
  workload's managed identity asks its token. The issuer is the
  tenant's v2.0 one, or for an app that issues version 1 tokens
  `https://sts.windows.net/TENANT/`, with `discovery: static` and
  `jwks_url` set to the tenant's keys. Azure hands out a cached token
  until it nears its end, so two jobs on one host within `fresh` may not
  both sign in; this has not been run against Azure.
- **A rule follows its provider's name.** Pointing a provider at another
  issuer, such as another GitHub Enterprise Server, points its rules
  there too, and a repository's ids are unique only within one
  instance: give a new issuer a provider of a new name. A `login:`
  provider and a `workload:` provider may not share a name.
- **Not tested against the real platforms.** GitHub Actions, Google
  Cloud and Azure are from their documentation; the tests run against a
  stand-in issuer. Kubernetes service account tokens are not a kind of
  1.0.

### Scopes

| Scope | Allows |
|---|---|
| `cli` | Every op the principal's role allows, the session `cli` among them: admin ops, deciding a gate, locks |
| `mcp` | The ops behind the MCP tools: never an admin op, never deciding a gate, never a lock, never the session `cli` |
| `ci` | A member's ops from a CI job, and reporting a ci gate's check from the session `ci`; never a waive or an admin op. Issued by a workload's token exchange ([Service principals](#service-principals)) |

`aries login` asks for `cli mcp`. A connection that proved a key is held
to its principal's role alone, as over SSH.

### Certificates

| `mode` | The certificate | Needs |
|---|---|---|
| `acme` | From an ACME server: Let's Encrypt, or an internal one's `directory` (step-ca, say), with which ACME needs no internet. ariesd answers the TLS-ALPN-01 challenge on the listener itself, so nothing else listens; the ACME server reaches port 443 | A DNS name that leads to the machine; the `cache` directory, mode 0700, which ariesd makes and which holds the account's key and the certificates; under systemd, `ReadWritePaths` for it (below) |
| `files` | The operator's: a certificate chain, leaf first, and its key, in PEM, from the company's or the site's CA. ariesd reads them again at the first handshake after either changes, with no restart; a pair that does not read is logged once (`tls.cert_reload_failed`) and the certificate served is kept | The key file mode 0600 (ariesd refuses one others may read), owned by the user ariesd runs as |
| `proxy` | None at ariesd: plain HTTP behind a proxy or load balancer that terminates TLS ([Behind a proxy](#behind-a-proxy)) | `trusted`: the proxy's addresses, or networks in CIDR form; one of IPv4 written in IPv6's form (`::ffff:10.0.0.0/104`) is taken as IPv4's, as a peer's address is |

TLS is 1.2 or 1.3 with Go's own cipher suites, which are not
configurable; `min_version: "1.3"` refuses 1.2. Clients check the
certificate against their system's roots and, where those lack the
site's CA, against the CA `server.ca` names in `.aries.yaml`
([Connect a repository](cli.md#connect-a-repository)); pinning a
certificate is not offered, since renewals would break it. `aries admin
status` shows when a certificate from files ends.

Under systemd the unit of [step 5](#5-run-ariesd-under-systemd) needs,
for a port below 1024, the capability to bind it, and nothing else:

```sh
printf '[Service]\nAmbientCapabilities=CAP_NET_BIND_SERVICE\nCapabilityBoundingSet=CAP_NET_BIND_SERVICE\n' > /etc/systemd/system/ariesd.service.d/https.conf
```

and for `acme`, its cache, which `ProtectSystem=strict` otherwise keeps
read-only:

```sh
install -d -o aries -g aries -m 0700 /var/lib/aries/acme
printf '[Service]\nReadWritePaths=/var/lib/aries/acme\n' > /etc/systemd/system/ariesd.service.d/acme.conf
systemctl daemon-reload
```

`RestrictAddressFamilies` already allows `AF_INET` and `AF_INET6`. The
[AppArmor profiles](#confine-ariesd-with-apparmor) allow the listener,
the capability, `/etc/aries/tls` and the ACME cache.

### Behind a proxy

With `mode: proxy`, ariesd serves plain HTTP, and only to `trusted`:
a connection from any other address is closed unanswered. What must
then be true, and what ariesd cannot check:

- **The proxy is the only way in.** Bind `listen` to loopback or a
  private address the proxy alone reaches (the installer binds
  `127.0.0.1:8443`), so that nothing else can claim to be the proxy.
- **It passes WebSocket upgrades** on `/v1/connect`, with HTTP/1.1 to
  ariesd. nginx needs, in that `location`, `proxy_http_version 1.1;`,
  `proxy_set_header Upgrade $http_upgrade;` and `proxy_set_header
  Connection "upgrade";`. A proxy that refuses the upgrade shows in
  `aries` as `answered the WebSocket upgrade with 400`, with a fix that
  names this section.
- **It keeps an idle connection at least 60 seconds.** ariesd pings
  every WebSocket every 25 seconds, so a proxy that closes connections
  idle for 60 keeps them; `idle_timeout` counts frames, not pings.
- **`public_urls` names what clients use,** the proxy's URL. A proxy
  that serves Aries under a path strips it.
- **The key proof is not bound to the TLS session** behind a proxy:
  ariesd never sees the client's session, and nginx exports no channel
  binding, so the proxy's certificate is what stands between a client
  and a relay ([security-model.md](security-model.md#the-https-listener)).
  Use `mode: proxy` behind one: a proxy that re-encrypts to an ariesd
  with `mode: files` is, to ariesd, a second TLS session, and
  `aries` from v0.6.0 on is refused there.
- **The limits per address are the proxy's.** Every connection comes
  from it, so `https_conns_per_addr`, `auth_failures_per_minute` and
  `signins_pending_per_addr` do not apply behind one, and ariesd does
  not read the client's address from `X-Forwarded-For` or any other
  header, which a client can forge. Without the proxy's own, one outsider
  can hold `https_conns_per_server` connections and fill
  `signins_pending_per_server` with sign-ins. The block below sets
  them: `limit_conn` for the connections (64 to an address, as
  `https_conns_per_addr`), and `limit_req` on `/v1/connect` (10 a second
  held, a burst of 200: each `aries` command is one request, and an
  office or a CI farm shares an address) and on `/oauth/authorize` and
  `/oauth/token` (1 a second, a burst of 30: each sign-in started is
  held in ariesd's memory), each answered 429 past it. These are looser
  than ariesd's `auth_failures_per_minute`, because nginx counts every
  request, not only the refused ones. nginx counts an IPv6 address alone, where ariesd counts a /64
  (`https_addr_prefix_v6`); a proxy with IPv6 clients keys the zones on
  the /64 with a `map` of its own.

A whole nginx server block that does all of this is below. The Docker
test server runs it: its proxy profile puts that file in front of
ariesd, and `smoke.sh` drives Aries through it
([nginx.conf](../deploy/docker/nginx.conf)). It is the same file with
the server's name changed to `aries.example.com`; its certificate paths
are the test CA's, to be replaced with the company's.

```nginx
# nginx in front of the test server with the proxy profile
# (deploy/docker/compose.yaml; docs/deploy/docker.md, Behind a proxy).
# It terminates TLS with the test CA's certificate, and passes each
# request to ariesd's loopback listener, which the entrypoint starts in
# proxy mode with 127.0.0.1 trusted (docs/server.md, Behind a proxy).
# It is a test fixture, never production.

# Paths nginx writes are under /tmp, the one writable tmpfs of the
# read-only container.
pid /tmp/nginx.pid;
error_log /dev/stderr warn;

worker_processes 1;

events {
    worker_connections 256;
}

http {
    access_log /dev/stdout;
    client_body_temp_path /tmp/nginx-client-body;
    proxy_temp_path /tmp/nginx-proxy;

    # The limits per client address are nginx's: every connection ariesd
    # sees is nginx's, so its own do not apply (docs/server.md, Behind a
    # proxy). These stand in for https_conns_per_addr and the other two.
    limit_conn_zone $binary_remote_addr zone=aries_conns:10m;
    # Every aries command is one request to /v1/connect, and an office or a
    # CI farm shares an address, so its limit is generous: 10 a second
    # held, a burst of 200. A request for any other path has an empty
    # key, which is not limited.
    map $uri $aries_connect_addr {
        default "";
        /v1/connect $binary_remote_addr;
    }
    limit_req_zone $aries_connect_addr zone=aries_connect:10m rate=600r/m;
    # A sign-in is a person at a browser, and each one started is held in
    # ariesd's memory for minutes: 1 a second, a burst of 30.
    map $uri $aries_signin_addr {
        default "";
        /oauth/authorize $binary_remote_addr;
        /oauth/token $binary_remote_addr;
    }
    limit_req_zone $aries_signin_addr zone=aries_signin:10m rate=60r/m;
    limit_conn_status 429;
    limit_req_status 429;

    server {
        listen 8443 ssl;
        server_name aries.example.com;

        ssl_certificate /etc/nginx/tls/cert.pem;
        ssl_certificate_key /etc/nginx/tls/key.pem;
        ssl_protocols TLSv1.2 TLSv1.3;

        limit_conn aries_conns 64;
        limit_req zone=aries_connect burst=200 nodelay;
        limit_req zone=aries_signin burst=30 nodelay;

        location / {
            proxy_pass http://127.0.0.1:8080;
            proxy_http_version 1.1;
            # WebSocket upgrades on /v1/connect
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection "upgrade";
            proxy_set_header Host $host;
            # ariesd pings each WebSocket every 25 seconds; this is above the
            # 60 seconds docs/server.md asks a proxy to keep an idle one.
            proxy_read_timeout 75s;
            proxy_send_timeout 75s;
        }
    }
}
```

Client certificates do not pass a proxy, so smart cards checked by
ariesd itself (1.x) need TLS at ariesd.

### Profiles

`install-server.sh --profile` chooses one of these, and its `--plan`
prints what it would serve and write without touching the machine. Each
is a set of defaults, not a different build.

| Profile | Transports | TLS | For |
|---|---|---|---|
| `internet`, the default | `[https]` | `acme`, on `:443`, for `--host`'s DNS name | A team on any network: the server's port 443 must reach it, and the ACME server must reach port 443 |
| `private` | `[https]` | `files` (`--cert`, `--key`, the company CA's), or `proxy` (`--tls proxy`, `--trusted`) | A company network or VPN |
| `airgapped` | `[https]` | `files`, the site CA's; or `acme` with an internal ACME server (`--acme-directory`), never Let's Encrypt | A site with no route out |
| `ssh-only` | `[ssh]` | none | The servers of before HTTPS, IAP, a site that allows only SSH |

`--enable-ssh` adds SSH to any of the first three. Nothing in
`internet` reaches outside but the ACME server; nothing in `private` or
`airgapped` does, unless its `--acme-directory` is outside.

### SSH on and off

`transports:` says which transports `ariesd serve` serves, and must name
at least one.

| | SSH off (the default) | SSH on |
|---|---|---|
| The installer | Writes no sshd drop-in, no `.ssh` for the `aries` account, no forced command and no `ariesd-stdio` AppArmor profile; the first admin's key goes to `/etc/aries/admin.keys`, which no sshd reads, for `control init`. `--disable-ssh` removes what a run with SSH on wrote | `--enable-ssh`, or `--profile ssh-only`: does [Keys from the store](#keys-from-the-store) and step 6, and checks `sshd -t` |
| ariesd | Writes no keys file, and removes one it wrote (`keys_file.removed`); refuses a bridge from `ariesd stdio` with `unavailable`, `SSH is off on this server`, and a fix naming `transports: [https, ssh]` and `--enable-ssh`. `ariesd status` and the upgrade's health check use the socket as before | Writes the keys file; serves the bridge |
| `aries admin key …` | Works, with a notice: the key signs in over HTTPS, and sshd takes it once SSH is on | As before |
| `aries admin status`, `ariesd status` | `transports: https on :443 for https://aries.example.com (files, certificate ends 2027-01-13), 3 connections; ssh off` | `…; ssh on, 2 connections` |

**Turning SSH on**: run the installer again with the options it was
first run with and `--enable-ssh`, which checks before it acts and sets
up sshd; then set `transports: [https, ssh]` in `/etc/aries/ariesd.yaml`
and `systemctl restart ariesd`, which writes the keys file. The
installer leaves the config file as it is, and says what to set.
**Turning it off**: set `transports: [https]` in
`/etc/aries/ariesd.yaml` and `systemctl restart ariesd`, which removes
the keys file and refuses bridges from then. Then run the installer
again with the options it was first run with and `--disable-ssh` in
place of `--enable-ssh`, first with `--plan` and `--dry-run`; it
refuses to run while the config file still serves SSH, since ariesd
could then no longer write its keys file. Its step 11 takes the keys of
the `aries` account's `authorized_keys` into the control store, as a
run with SSH on does, so that each still signs in over HTTPS; then
`install-server.sh --disable-ssh` removes, each only where it is:

- `/etc/ssh/sshd_config.d/00-aries.conf`, if it is this guide's; then
  it checks `sshd -t` and reloads sshd
- `/var/lib/aries/.ssh/authorized_keys`, and with it the forced command
  of each of its lines
- `/var/lib/aries/.ssh/aries_keys`, if ariesd wrote it
- `/var/lib/aries/.ssh/`, when nothing else is in it
- `/etc/systemd/system/ariesd.service.d/keys.conf`, if it is this
  guide's; then it restarts ariesd
- `/etc/apparmor.d/ariesd-stdio`, unloaded first: with SSH off nothing
  runs as sshd's forced command

So sshd lets no key in for the account, and nothing on the machine
forces the bridge: no SSH path is left. It changes nothing else of
sshd's, which other accounts may use. A file it would remove that holds
other text than this guide's stops it, with the difference shown.

`aries` configured for SSH against a server with SSH off is refused by
sshd, which has no key for the account; when `.aries.yaml` also has
`server.url`, the refusal says to use it (`ARIES_TRANSPORT`,
[Connect a repository](cli.md#connect-a-repository)).

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
  you to name the target in your reply, and the dump this guide has for
  them is taken first. It stops and asks when what it finds
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
Code reads. Its database steps, in `references/database.md`, follow
this guide's sections on Postgres: the role and the database, sizing,
managed services, backups and restore. What was run: its read-only and
admin commands against the [Docker test server](deploy/docker.md), on
15 October 2026, before it had database steps; not on a virtual machine
or a cloud provider's. Its database steps were not run through Claude;
the guide's commands they follow were
([Backups and restore](#backups-and-restore)).

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
| A key is | A line of `~aries/.ssh/authorized_keys` whose forced command names the principal ([step 6](#6-let-the-first-principal-in)) | A row of the control store, which ariesd writes to a file sshd reads ([Keys from the store](#keys-from-the-store)). A line of `authorized_keys` still works too |
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

A server can hold many projects, each in a schema of its own in the one
database, with its own writer, so a busy project does not queue
another's writes. They share the one Postgres, the one ariesd and the
machine. Another schema beside them, the control store, records the
projects, the principals and their keys, and which principal is a member
of which project. You opt in: a server with no `control_schema` setting
is the server of one project it was.

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
  the server: ariesd makes its schema.
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

Everything above is built and tested against a real Postgres and Aries's
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

1. Name the control store's schema in `/etc/aries/ariesd.yaml`,
   leaving `dsn`, `project`, `prefix` and `account` as they are. They now
   describe the project to adopt:

   ```yaml
   control_schema: aries_control
   ```

   Nothing is made yet. ariesd's role owns the database
   ([step 2](#2-create-the-role-and-the-database)), so `control init`
   makes the schema in the next step.

2. Make the control store, adopt the existing project as the first
   one, and import the principals and keys of `authorized_keys`:

   ```sh
   sudo -u aries -H ariesd control init --name acme
   ```

   ```text
   project acme (6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f) adopted from schema public: active
   principals: 2 added, 0 already there; keys: 3 added, 0 already there; members: 1 added, 0 already there; 0 lines skipped
   3 keys in the store are also lines of /var/lib/aries/.ssh/authorized_keys: sshd reads that file too, so `aries admin key remove` does not keep such a key out until its line is deleted too
   ```

   The last line counts the keys that are now in both places. It is a
   reminder, and `control init` never edits `authorized_keys`: see
   [Keys from the store](#keys-from-the-store), step 5. It is printed
   only while something still reads the file: sshd, with SSH on, or
   ariesd's key proof over HTTPS, when the file is ariesd's
   `authorized_keys`. With SSH off, `/etc/aries/admin.keys`, which no
   one else reads, gets no such line.

3. Nothing more is needed for admins to create further projects. Each
   project is a schema of the DSN's database, which ariesd's role owns
   and so may make schemas in, and ariesd names each one itself:
   `project_` and the project's name, with each hyphen an underscore
   (`aries admin project add web-app` makes `project_web_app`). A
   project's name is then at most 55 characters. A schema of that name
   already in the database is never taken over: the add is refused,
   and nothing is written, until the name is changed or the schema
   dropped. `control_schema` may not start with `project_`. Where the
   role does not own the database, as on some managed services, see
   [Managed services](#managed-services).

4. Let ariesd write its keys file and have sshd read it, which is
   also what lets you add people from anywhere, with no line in
   `authorized_keys`: [Keys from the store](#keys-from-the-store), steps
   1 and 2. ariesd does not start on a server of many projects until
   it can write that file.

5. Restart ariesd: `systemctl restart ariesd`.

`control init` makes the control store's schema if it is not there,
then records the schema `dsn` names (`public`, unless its `search_path`
names another) as a project, with the config file's `project` UUID,
`prefix` and `account` and the name you give, and changes nothing in
that schema. Then, for each line of
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
  control store again every 30 seconds, so the new member can connect
  within the minute.
- **Run again, it does not undo what an admin did.** The file is not
  edited when an admin removes someone with `aries admin`, so its lines
  still name them. `control init` reads the control store's event log
  before it adds anything: a key whose newest event is its removal stays
  out of the control store, a principal whose newest event on the
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
  you mean, for example after restoring a control store from an old
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
- **Its changes are on the record** in the control store's event log,
  under the reserved principal `import`.
- **`serve` refuses to start** while the control store holds no active
  project, and says to run `control init`.
- **To go back,** remove `control_schema` and restart. The project's schema
  was never changed, and the control store is left unused.

### Keys from the store

On a server of many projects ariesd writes the keys its control
database holds to a file, and sshd reads that file before
`authorized_keys`. An admin adds and removes keys with `aries admin`, from
anywhere, and nobody edits a file on the server. The file is
`~aries/.ssh/aries_keys`, in the format of `authorized_keys`: a
first line that says ariesd generates it, then one line for each key
of each active principal that has not ended, the line you would have
written by hand with the key's fingerprint added, and for a key with an
end ([Keys that end](#keys-that-end)) that end as `expiry-time`:

```text
restrict,command="/usr/local/bin/ariesd stdio --principal alice --key SHA256:…" ssh-ed25519 AAAA…
restrict,expiry-time="20261116170000Z",command="/usr/local/bin/ariesd stdio --principal bob --key SHA256:…" ssh-ed25519 AAAA…
```

sshd asks ariesd nothing. It reads two files, ariesd's first, and
nothing of Aries runs before sshd has authenticated a key.
`authorized_keys`, which you maintain by hand, stays the way in for a
key the control store does not hold: the first admin's before
`control init`, and a server admin's own new key. Over HTTPS, ariesd's
key proof takes a line of it that forces the bridge, as sshd would,
unless the line has `from=`, `expiry-time`, `cert-authority` or
`verify-required`: the proof checks none of those conditions, so such
a key signs in over SSH alone. For a key that should end, add it to the
store with `aries admin key add --expires`, which both transports
enforce.

- **When it is written.** At start, before ariesd listens; after every
  `aries admin` change that alters which keys sign in (a key added or
  removed, a principal added with keys, disabled or enabled), before
  the admin is answered; and every 30 seconds if it no longer holds
  what the control store does, which is how an `ariesd control
  init` run beside the daemon reaches it, and how an edit to the file
  is undone. A key added with `aries admin` signs in at once, with no
  restart of anything. When nothing changed, nothing is written.
- **How it is written.** A new file beside it, mode 0600, synced, then
  renamed over it, so sshd never reads half a file and a crash leaves
  the old one or the new one. The lines are in a fixed order, so the
  same keys give the same bytes.
- **The file is a cache, and the control store decides.** Each line
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
   its fingerprint and the principal the control store gives it, so
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
   `authorized_keys` into the control store
   ([Opt in](#opt-in)), so each is now in both files. Remove the lines
   from `authorized_keys` that you want `aries admin` to govern, and keep
   the first admin's:

   - **A key whose line is still in `authorized_keys` is not kept out
     by `aries admin key remove`.** While the control store holds the
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
     its key is not in the control store, or was removed from it.
     `aries admin status` counts those for each project (BY HAND), and
     `aries admin key list NAME` for NAME. `aries admin key remove` does not
     close them.
   - **The control store's line wins for a key in both.** The key
     signs in as the principal the control store gives it, whatever
     principal its line of `authorized_keys` names, and a disabled
     principal's key has no line in ariesd's file, so it reaches its
     line of `authorized_keys` and is refused by ariesd as disabled.
     `authorized_keys` cannot override the control store; it only
     lets in a key the database does not hold.
   - **Keep one line for a server admin.** It is how a server admin's
     own key is replaced, since nobody adds a key to their own account
     with `aries admin`. A key in `authorized_keys` keeps the
     principal its line names: `aries admin key add` and `principal add
     --key` refuse it for anyone else, as they refuse a key another
     principal has, so nobody can take a server admin's key by adding
     it to the control store under another name. And a line you add
     for a key the control store already holds under another name
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
| ariesd installed as [step 3](#3-install-ariesd) installs it: owned by `aries` in `/opt/aries/bin`, linked from `/usr/local/bin`, and no copy that root owns | Keys from the control store sign in. sshd runs no program of Aries's before authentication, so who owns the binary does not matter to it |
| A global `AuthorizedKeysCommand`, outside any `Match` block, set to a root-owned script that prints nothing, with `AuthorizedKeysCommandUser nobody` | Keys from both files sign in, with no override. sshd ran the script twice for each sign-in by `aries`. With `AuthorizedKeysCommand none` in the `Match` block it did not run it |
| ariesd killed, sshd left running | sshd accepts a key from either file, and the client gets `ariesd is not running on the server` |
| A key in both files, with `authorized_keys` named first, as this guide had it until that day | sshd let it in by its line of `authorized_keys`, which names no fingerprint |
| A key in both files, with ariesd's file named first, as above | sshd lets it in by ariesd's line: ariesd counts no connection of that key as let in by `authorized_keys`. With the key removed from the control store, the same key signs in by `authorized_keys`, and its connection is counted so |

Checked under systemd 255, on 12 October 2026, on the Ubuntu 24.04
virtual machine of [What was run, and where](#what-was-run-and-where):

| Checked | Result |
|---|---|
| The unit of step 5 as written, on a server of many projects, without step 1's `ReadWritePaths` | ariesd does not start: `write the keys file …: read-only file system`, and its `fix:` names `ReadWritePaths=/var/lib/aries/.ssh` |
| The same with step 1's drop-in | ariesd starts and writes the file, mode 0600, owned by `aries`. `ProtectHome=yes` does not get in the way |
| Step 2 on stock Ubuntu's sshd, which listens through `ssh.socket` | `sshd -t` passes and `systemctl try-reload-or-restart ssh` applies it. A key the control store alone holds signs in with no restart of anything; removing it, or disabling its principal, locks it out |
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

#### Keys that end

A key may be given an end, for a contractor's key or a stopgap CI key
(design decision 87):

```sh
aries admin key add bob bob.pub --expires 30d
aries admin key add ci-deploy deploy.pub --expires 2026-12-31T17:00:00Z
```

A duration (`12h`, `30d`, `2w`) is counted on the server's clock, not
the machine's that runs `aries`; a time is RFC 3339 with its zone. The
end is kept to the second, and may be at most ten years away: give a
key meant to last longer no end. `aries admin key add` and
`aries admin key list` print each key's end, in UTC; a key with none
lasts until it is removed, as before. To give a key another end, remove
it and add it again: adding a key the principal has with another end is
refused, `exists`, naming the removal.

From its end the key signs in over neither transport (decisions 59 and
89):

- **sshd** refuses it by its line's `expiry-time`, which ariesd writes
  in UTC, fourteen digits marked `Z`. Without the `Z` sshd reads the
  digits in the server's own zone, and on a server set west of UTC
  would let an ended key in for hours (measured on OpenSSH 9.6p1 and
  10.2p1). sshd checks once, when a connection authenticates, on the
  wall clock.
- **ariesd** refuses it at the handshake, and in the proof over HTTPS,
  on [its own clock](#the-servers-clock), which is never behind the
  wall clock, with `auth` and the time it ended. Over HTTPS the key is
  challenged first, and told whose it was and when it ended only once
  its proof verifies: its public key is no secret, and a proof by any
  other key is refused as any proof that does not verify. So ariesd refuses at
  or before the moment sshd would, and a clock set back, which revives
  the line at sshd, does not revive the key.
- **The connections it signed in close at its end**, over SSH and over
  HTTPS, since sshd never looks at an open connection again. ariesd
  sets a timer for the earliest end of any key, and looks at least
  every 30 seconds besides; at the end it writes the keys file without
  the key's line, closes its connections, and logs `key.expired` once
  for the key and `conn.dropped` with `reason="key expired"`
  ([Events](#events)). Claims are left as they are: a lease the client
  no longer renews ends as leases do.
- **An ended key is listed** by `aries admin key list` for an hour
  after its end, so that its refusals can be traced, and is then
  deleted on the reaper's tick. That is pruning, not history: the
  control store's event log has the `key.add` that set the end, and
  no event for the deletion.

With SSH off the key's end holds for the proof over HTTPS alike; the
keys file is not written.

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
| `aries admin project list` | Anyone the server lets in: a server admin sees every project and its schema, anyone else the active projects they belong to |
| `aries admin principal add NAME` | Server admins; a project admin, into a project they administer |
| `aries admin principal add NAME --no-project`, `principal list` | Server admins |
| `aries admin key list --added-by NAME`, `principal list --created-by NAME`, `member list --added-by NAME` | Server admins, for all of it; a project admin, for the keys and principals of their projects' members and for their projects' memberships |
| `aries admin principal disable NAME`, `enable NAME` | Server admins; a project admin, for a principal within their reach |
| `aries admin member add NAME`, `remove NAME` | Server admins; the project's admins |
| `aries admin member list` | Server admins; the project's admins |
| `aries admin principal add NAME --key FILE`, `key add NAME FILE`, `key remove FINGERPRINT` | Server admins; a project admin, for a principal within their reach. Never for one's own account |
| `aries admin key list NAME` | Server admins; an admin of an active project NAME belongs to, with or without reach over NAME; NAME themselves |
| `aries admin principal add NAME --kind`, `--owner`, `--display-name` | As `principal add` |
| `aries admin principal set NAME`, `invite add`, `invite list`, `invite remove ID`, `identity remove ID`, `principal add NAME --invite` | Server admins. Never an invitation onto one's own principal ([Identities and invitations](#identities-and-invitations)) |
| `aries admin identity list NAME`, `principal show NAME` | Server admins; NAME themselves |
| `aries admin trust add`, `set`, `disable`, `enable`, `remove` | Server admins. Never a rule of one's own principal ([Service principals](#service-principals)) |
| `aries admin trust list SERVICE`, `trust show NAME` | Server admins; the service's owner. `trust list` with no service: server admins |
| `aries admin status` | Server admins, for the server and every project; a project admin, for the active projects they administer ([Watching a server](#watching-a-server)) |

- **A server admin** is a principal named under `admins:`. They are let
  into every active project and may do everything above. Nothing over
  the protocol makes one.
- **Disabled wins.** A principal that is disabled is refused everywhere
  and has no admin rights, in the control store or in any project,
  even when `admins:` names it. An admin named there who has no row in
  the control store is not disabled. Only a server admin disables or
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
  connection's key is not one the control store can take away in any
  case. Removing the last key the control store holds for you is
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
  ([Keys from the store](#keys-from-the-store), step 5). `key add
  --expires` gives a key an end ([Keys that end](#keys-that-end)).
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
  makes its schema, `project_` and the name with hyphens as underscores
  ([Opt in](#opt-in), step 3), migrates it and marks it active, then
  prints the `.aries.yaml` for its repository. If a step fails, run the
  same command again: it finishes the same project. If another role
  made the schema meanwhile, the refusal gives the statement to run as
  its owner, and the command then resumes. The project is served at
  once to server admins; add its members with
  `aries admin member add NAME --project NAME`.
- **No project is removed**, and ariesd never drops a schema. A project
  you no longer want is archived.
- **Archiving** keeps the project's schema, members and name, and
  serves it to no one: its connections are closed, its claims end, and
  its store closes with them, after one more sweep for a claim that a
  request in flight took meanwhile, which gives up the schema's
  writer's lock (see [One writer for each project](#one-writer-for-each-project)).
  An ariesd that stopped before it had released them ends them when it
  starts, before it listens. The server's last active project cannot be
  archived, since `aries admin` needs a project to connect to.
  `activate` opens the schema, which applies any migration it missed
  while archived, and serves it again.
- **Everything is on the record** in the control store's event log,
  under the principal who ran the command.

One case in the above is slower than "at once". A project whose store is
closed has nothing running in it: when its member is removed or disabled,
the claims stay in its schema until the store next opens. Nobody can
use them meanwhile, and they end before the first request is answered.

An archived project's store never opens for a connection, so ariesd
does not leave its claims to that. The archive opens a closed store to
end them, and at every start, before it listens, ariesd asks each
archived project's schema whether it holds a claim, and opens the ones
that do, ends every claim, recorded under `ariesd` for `project
archived`, and closes them. A project with nothing to end costs that one
read, a fraction of a millisecond, and nothing is written. It does not
migrate: a schema that an older ariesd last migrated is left as it
is, with a line in the log at `info`, and a claim in it ends when the
project is activated, as before. One that cannot be opened is logged at
`error` and does not stop the server or the other projects. Nothing
runs for archived projects on the 30-second tick: a running ariesd
leaves a claim in one only if the release itself failed, which it logs,
and the next start or the activation ends it.

### Identities and invitations

A person signs in at the company's identity provider ([Sign-in](#sign-in);
[docs/design/identity.md](design/identity.md)). Which principal a
sign-in is, is one of that principal's identities: the provider, and
the issuer and subject the provider's token names. Nobody becomes a
principal by signing in (decision 57): an admin adds the principal, and
invites an address for it at a provider; the person's first sign-in
whose verified address matches binds the invitation (an
`identity.bind` event), and from then on only the issuer and subject
are matched.

```console
$ aries admin principal add bob --display-name "Bob Example" --invite bob@example.com --provider google
principal bob added, a member of project acme
invitation 7 for bob: bob@example.com at google, open until 2026-10-23 09:00 UTC
$ aries admin principal add ci-bot --kind service --owner alice
$ aries admin invite list --pending
$ aries admin identity list bob
$ aries admin invite remove 7
```

- **Only a server admin** invites, removes an invitation or an
  identity, or sets a principal's kind, owner or display name. **Nobody
  invites an identity onto their own principal**, a server admin
  included: an agent holding a person's key could otherwise point their
  principal at an address it controls. Another server admin does it.
  A principal may list its own identities (`aries admin identity list
  YOU`) and show itself (`principal show YOU`).
- **A person or a service.** A principal is a `person` unless added or
  set as a `service`. A service answers to an owner, a person other
  than itself, and has no identities: it signs in by a key, and later
  by a trust rule. A person has no owner. A person with identities, or
  who owns a service, cannot become a service until they are removed
  or the services given another owner. Every principal from before this
  release is a person.
- **The display name** is what people see beside the name, at most 128
  characters of one line; `principal set NAME --display-name ""`
  clears it. It is not unique.
- **An invitation** is an address at a provider, as the provider will
  name it in its token, such as `bob@example.com`. The address is
  compared without the case of ASCII letters. One address has one open
  invitation at a provider: inviting it again for the same principal
  renews it, from now, and inviting it for another is refused, naming
  the invitation to remove first. Two providers are never joined by
  address. The provider is a name of the server's `login: providers`;
  with any configured, another name is refused, naming them. A server
  with none takes any name, since nothing signs in there yet.
- **An invitation lapses** unused after `login.invite_ttl`, 7 days by
  default ([Settings](#settings)), since until it is used it is a match
  by address. A lapsed one shows as `lapsed`, matches nothing, and stays
  until it is removed or renewed. It counts against its principal's
  `identities_per_principal` and not against `invites_per_server`
  ([Limits](#limits)).
- **Removing an identity** removes the match: no sign-in is that
  principal by it from then on, and the sign-ins made with it end, their
  connections closed: a server admin who removes the identity behind
  their own sign-in is answered, and then that connection closes too.
  `invite remove` removes an invitation no sign-in
  has bound, and refuses a bound identity, naming `identity remove`.
- **A person's sign-ins** are listed by `aries admin signin list NAME`
  (a server admin, or the person), and one is ended by `aries admin
  signin end ID` (a server admin, or its holder): live and ended ones,
  with the provider, the machine and the end, never a token.
- **On the record, and personal data kept out.** Each change is an
  event in the control store's log (`principal.set`, `identity.invite`,
  `identity.remove`, `identity.bind`, `signin.grant`, `signin.end`) and
  a line of [the audit stream](#the-audit-stream).
  Neither holds an invited address, a provider's subject or a display
  name's value: the log is kept for good.
- **`aries admin status`** counts the identities, bound, pending and
  lapsed.

### One writer for each project

Every project is a schema of the one database in `dsn`, and ariesd
reaches all of them, and the control store, through one pool of
connections, at most `db_conns_per_server` ([Limits](#limits)). A store
holds no connection of its own: each write and each read takes one from
the pool and gives it back, so a project nobody is using costs Postgres
nothing. One project's reads hold at most `db_conns_per_project` of
them at once, and its writes one more, one write at a time. Beside the
pool ariesd holds one more connection, named `ariesd writer locks` in
`pg_stat_activity`. Postgres's `max_connections` must leave room for
all of these, and for anything else that connects.

That last connection makes ariesd the only writer of each schema it
serves, however many processes share the database. While ariesd has a
project's store open, and the control store always, it holds a Postgres
advisory lock on a key made from the schema's name, and every write
checks that it still does. Postgres gives the locks up when that
connection ends, so a daemon that dies frees its projects for another.

- **A second ariesd on the same control store** refuses to start:
  `the control store … is served by another ariesd on this database`.
  Stop the first, or give the second a `control_schema` of its own.
- **A second ariesd whose control store names a project the first
  serves** refuses that project's connections, as `unavailable`, and
  serves its others.
- **An operator's command that writes a project the daemon serves**,
  `ariesd import-bd`, is refused with what to do: stop ariesd, import,
  and start it again. `import-bd --dry-run`, `export-bd`, and the clock
  check of `ariesd upgrade` only read, and run beside the daemon.
  `ariesd control init` writes the control store beside the daemon, as
  before, and takes no lock.
- **When the lock's connection is lost**, the next write connects
  again and takes back every lock. A write in flight finishes before
  another process that took the project over writes anything; a
  project another process took meanwhile refuses this ariesd's writes,
  and its reads go on, until its store is closed.
- **Archiving** a project closes its store and gives its lock up; the
  schema stays as it is.

### Watching a server

`aries admin status` reports what ariesd knows of itself and of its
projects now. A server admin sees the server and every project; a project
admin sees the active projects they administer and nothing of the
server; anyone else is refused. It reads memory, a count on an index
for each open project listed, Postgres's own figures and each project's
schema's size; it opens no store and writes nothing. It does not wait
for a database that does not answer
([When Postgres does not answer](#when-postgres-does-not-answer)).

```text
ariesd v0.5.0, up 3d 4h
stores 2 of 64, connections 14 of 1024
keys file /var/lib/aries/.ssh/aries_keys: 31 keys, written 2026-10-11 09:12 UTC
ariesd's memory 42 MiB in use, 78 MiB from the system; 112 goroutines
postgres 18.6: connections 14 of 100, 9 in this database; ariesd's pool 2 in use of 5 held, at most 32, waited for 0 times
postgres's database 41.0 MiB; since its statistics were reset, 52340 commits, 12 rollbacks, 0 deadlocks, 0 conflicts, 0 KiB in temporary files
projects: 2 active, 1 archived; principals: 12 active, 1 disabled; identities: 1 lapsed, 3 pending

PROJECT  STATE     STORE   SIZE      CONNS  BY HAND  SESSIONS  WRITES/MIN  P50    P99    LAST USED
acme     active    open    21.3 MiB  9      2        14        212         4.2ms  31ms   2026-10-11 09:30 UTC
web      active    open    9.3 MiB   5      0        6         40          3.9ms  18ms   2026-10-11 09:29 UTC
old      archived  closed  600 KiB   0      -        -         -           -      -      -
```

| Figure | What it is |
|---|---|
| stores, connections | How many are open, beside the limit that caps each (`open_stores_per_server`, `conns_per_server`) |
| keys file | The file ariesd writes for sshd ([Keys from the store](#keys-from-the-store)): its path, how many keys ariesd last put in it, and when it was last written. `THE LAST WRITE FAILED` means sshd may be reading an older list than the control store holds; ariesd's log says why |
| `clock:` | Shown once the clock has been set while ariesd ran: how many times, in which direction, and the last step's size and time. Each is a `clock stepped` line in the log ([The server's clock](#the-servers-clock)) |
| `CLOCK BEHIND HISTORY` | The clock is behind the latest event of the database named, by how much and since when. ariesd's time runs on from the history at half speed until the clock has caught up. With `clock_behind: stop` it adds that ariesd will not start again until the clock is corrected: check `timedatectl` on the server before any restart |
| ariesd's memory | The Go heap in use and all the memory ariesd has from the operating system (`HeapAlloc` and `Sys`). Not Postgres's |
| `postgres` | What Postgres says of itself, read with ariesd's own role, which needs no `pg_monitor`: its version; the client connections on the whole server against its `max_connections`, and those in ariesd's database; ariesd's [pool](#one-writer-for-each-project), its connections in use and held against `db_conns_per_server`, and how often a request waited for one; the database's size; and, since Postgres last reset its statistics, its commits, rollbacks, deadlocks, conflicts (queries a standby canceled) and what its queries wrote to temporary files. `postgres: unknown` says why Postgres did not answer, with the pool's figures alone |
| projects, principals, identities | The control store's rows, by state; identities, shown once there are any, as `bound`, and invitations `pending` or `lapsed` ([Identities and invitations](#identities-and-invitations)) |
| STORE | Whether the project's store is open. The figures after SIZE exist only while it is, and start from zero each time it opens |
| SIZE | The project's schema on disk, its tables and indexes, whether its store is open or not; 0 KiB for one made and never migrated. Shown by a server that reads it |
| CONNS, BY HAND | Connections past their handshake, and those among them that named no key of the store, which sshd let in by the hand-maintained `authorized_keys` |
| SESSIONS | Sessions seen in the last five minutes, as `aries who` lists them. `unknown` when the database did not give the count ([below](#when-postgres-does-not-answer)) |
| WRITES/MIN | Transactions that changed something and committed in the last 60 seconds. A refused request is not one; a lease renewal is |
| P50, P99 | The median and 99th percentile of the latest 256 writes' latency, however old they are: from the request reaching the project's store to its commit, the wait for the one writer included |
| LAST USED | When the project last answered a request, or was opened |

`-n` and `--after NAME` page the projects as the admin lists do, 200 at a
time. `--json` gives the same figures, with latency in microseconds.

On the server itself, [`ariesd status`](#ariesd-status) prints the
same report with no admin key, and the same measurements are exported as
metrics ([Observability](#observability)).

#### When Postgres does not answer

The report is there to be read when the database is in trouble, so it
does not wait for the database. Its reads of Postgres, the count of each
open project's sessions and Postgres's own figures, are made at once and
given 2 seconds in all, the bound `/readyz` gives its own check of
Postgres: each takes milliseconds when Postgres answers at all. What did
not come back is `unknown`, never 0, and a line says why:

```text
postgres: unknown (database did not answer in 2s); ariesd's pool 1 in use of 1 held, at most 32, waited for 0 times. Check Postgres on the server (docs/server.md, Troubleshooting)
…
PROJECT  STATE   STORE  CONNS  BY HAND  SESSIONS  WRITES/MIN  P50    P99   LAST USED
acme     active  open   9      2        unknown   0           4.2ms  31ms  2026-10-11 09:30 UTC
web      active  open   5      0        unknown   0           3.9ms  18ms  2026-10-11 09:29 UTC
sessions: unknown for 2 projects (database did not answer in 2s); the rest is what ariesd holds in memory. Check Postgres on the server (docs/server.md, Troubleshooting)
```

- **Everything else is still there**, since ariesd holds it in memory:
  the server's lines, the stores and connections, the clock, the
  writes and their latency as they last were, and the pool's figures.
  The SIZE column is left out when no project's size came back.
- **`--json`** leaves `sessions` out for such a project and gives
  `sessions_unknown`, the cause as text: `database did not answer in 2s`,
  or `the read failed; see ariesd's log` for any other failure, which
  the log has as `count the sessions seen, for status`. A project whose
  store is closed has neither. `postgres` then has `unknown` and the
  pool's figures alone.
- **`aries admin status` answers in about 2 seconds too, from a repository
  whose project's store is open.** `aries` connects anew for each command,
  and a new connection is welcomed without waiting on Postgres: its
  session is noted for `aries who` beside the wait for its first request.
  The report counts its asker's session, so it waits for that note, but
  within the same 2 seconds as its own reads: when the note does not
  come, the sessions are `unknown` and the rest is there. Run in the
  [Docker test server](deploy/docker.md#with-the-telemetry-harness)
  with Postgres's container frozen (15 October 2026).
- **From a repository whose project's store is closed, it is refused
  in about 2 seconds:** `the server's database does not answer, so
  project "…" was not opened`, with `ariesd status` on the server as
  what to do. A connection is to a project, opening the project's store
  waits on Postgres for up to a minute, and the report itself opens no
  store; so before a store is opened for a connection, Postgres is given
  2 seconds to answer a ping. Run `aries admin status` from the
  repository of a project in use, or `ariesd status` on the server.
- **Every other command waits for Postgres**, for the request timeout
  of a minute and no longer, and then ends with `the server timed out on
  …`. It waits for its session to be noted first, within that same
  minute, so nothing reads or writes a project for a session `aries who`
  cannot list. A session whose note failed is noted by its next request
  that Postgres answers.
- **`ariesd status` on the server** answers in about 2 seconds
  whichever stores are open, and needs no admin key.

Each project's store is opened by its first connection and closed after
`store_idle` without one ([Limits](#limits)). An open store holds no
connection to Postgres of its own: it takes one from the pool for each
read or write and gives it back. Closing it gives up its writer's lock
and what ariesd holds of it in memory. Postgres keeps, in each pooled
connection, its caches of every schema that connection has worked in,
until the pool retires the connection, idle for 30 minutes or an hour
old ([Sizing Postgres](#sizing-postgres)). A store that reopens ends the
claims that expired while it was closed, and those of anyone who lost
their access meanwhile, before it serves its first request.

## A project's workflow

A project's [workflow](concepts.md#the-workflow) is a document a project
admin applies with `aries admin workflow set FILE`, from their own
terminal: the acceptance items the server adds to every issue it
creates, by type; the checks it requires before an issue closes, whose
passing report closes it; the close-out lines it shows when work ends (they are shown, not
tracked); and the review and approval lines a later release enforces. It is a row of the project's `settings` table,
named `workflow`, whose `rev` is the workflow's, written by the
project's writer with a `workflow.set` event (target
`setting:workflow`) that holds the document before and after; every
`create` reads it in its own write. On a server of many projects a
project admin or a server admin sets it, and the principals it names
must be members of the project; on a server of one project, an admin of
its `admins:`, and any principal name is taken. Its size is bounded by
`workflow_bytes`, `workflow_items_per_type`, `workflow_checks_per_type`
and `workflow_closeout_per_type` ([Limits](#limits)). Its lines are
people's text: none goes on a metric, a span or a log line.

What an issue requires is read from its own events, never from the
workflow in force: the rev and profile its `issue.create`, or a change
of type, records, and that rev's `workflow.set` event, which holds the
document. A report of a required check that has no gate makes one, with
a `gate.add` event before the report's, and the closing pass and a
revert's reopen are `issue.close` (reason `landed`) and `issue.reopen`
(reason `check failed`) by the reporting principal, in the report's
write. A server CI cannot reach never hears a check pass, so an issue
that requires one waits until a person the workflow's `ci` names
reports it from their own terminal: `aries land` after a pull of the
default branch ([A server CI cannot reach](ci.md#a-server-ci-cannot-reach)),
or `aries gate pass --check landed --issue ID --sha SHA`, each from the
session `cli`, which an agent's is not. An admin's `close --force`
closes it too, recording the checks it overrode.

A profile's `review` and `approve` make human gates in the write that
creates the issue (a `gate.add` event after the `issue.create`, by the
creating principal; Aries's own gate for the issue). An approval gate
holds the issue back with a `blocks` edge, as `aries gate add` does; a
review gate has the issue as its subject and holds nothing, so `finish`
and `close` are refused with `acceptance` until a person passes or an
admin waives it, and a forced close records `reviews_overridden`. They
count against `open_gates_per_project` and `approvers_per_gate`, and a
create the limit refuses is refused whole. Neither adds an op, an
argument or a protocol version.

```sh
aries admin workflow diff .aries/workflow.yaml
aries admin workflow set .aries/workflow.yaml --rev 3
aries workflow check .aries/workflow.yaml   # exits 1 when the server's differs
```

## Prices

`aries cost`, `show` and `digest` price tokens at each model's list rates,
which an admin records with `aries admin price set`. Rates are US dollars
per million tokens, from an effective date (midnight UTC) or time; the
model is spelled exactly as the harness reports it, which `aries cost
--group-by model` shows. For example, with invented rates for an invented model:

```sh
aries admin price set example-large --from 2026-01-01 \
  --input 4 --output 20 --cache-write 5 --cache-write-1h 8 --cache-read 0.4
aries admin price list
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

**Prices by prompt length.** Some models charge more for a request whose
prompt is long: Claude Haiku 5.5 pays $0.50 input, $0.625 five-minute
write, $1 one-hour write, $0.05 cache read and $2.50 output per million
tokens when the prompt is over 100,000 tokens, against $0.10, $0.125,
$0.20, $0.01 and $0.50 up to it ([Anthropic's pricing
page](https://platform.claude.com/docs/en/about-claude/pricing), "Long
context pricing"). A price takes that as a **tier**: `--over TOKENS` and a
second set of the five rates, all required together (0 for what is not
charged), with the base rates given as before. For example, with invented
rates:

```sh
aries admin price set example-small --from 2026-01-01 \
  --input 0.1 --output 0.5 --cache-write 0.125 --cache-write-1h 0.2 --cache-read 0.01 \
  --over 100000 --over-input 0.5 --over-output 2.5 --over-cache-write 0.625 \
  --over-cache-write-1h 1 --over-cache-read 0.05
```

A usage record is priced at the tier when its **prompt**, its input plus
cache-write plus cache-read tokens (a count the harness did not report
counts as none), is *over* `--over`; exactly `--over` tokens is not. Each
record is priced on its own, as the provider bills a request, and an issue's
part of a record shared by time is priced at the tier its whole record falls
in. Only a record of one request (granularity `request`, which is what
`aries usage --hook` sends for Claude Code, one per response) has a prompt
length: a record that sums a turn or a session is priced at the base rates,
so a harness that reports only totals is priced low for such a model.
Setting a price without `--over` clears a tier it had. The tier is part of
the price row (migration 3), of the `price.set` event, and of `aries admin
price list`, which shows it under its price. `price.set` takes `tier` from
protocol 10; a server of protocol 9 refuses it. The 1.1x of `inference_geo`
(US-only inference) is not priced.

## Plans

A flat-rate plan (a subscription paid per seat a month) costs the same
whatever its tokens would cost at list price. `aries cost` shows each
group's share of the plans' fees as its **amortized cost**, beside the
list-price equivalent, once an admin records the plans with `aries admin
plans set`. For example, with an invented plan:

```sh
aries admin plan set team --from 2026-09 --fee 25 --seats 3 \
  --principal alice --principal bob
aries admin plan list
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
database of the server: the control store's and every project's. A
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
| Stock Ubuntu 24.04 | 1.7 s after the kernel started, 37 microseconds after `time-set.target`, before the network was up or any time server heard from | 9.7 s (its first start then lost a race with the database of that time, which the unit's ordering after Postgres does not have) | 4.6 s |
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
database it will write to: the control store's and each active
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
| `dsn` | The database: a `postgres://` URL, as Postgres's own clients read it, of the role, the host and port, and the database, `postgres://ROLE@HOST:PORT/DATABASE?PARAMS`. With no host, `host=` names the directory of a unix socket, as [step 4](#4-write-the-config-file)'s does. `search_path=` names the schema of the one project, `public` when it names none. Other parameters are Postgres's: `sslmode=` and `sslrootcert=` for TLS, and `passfile=` for a file that holds the role's password ([Managed services](#managed-services)). A password in the URL itself works, in a config file of mode 0600, and never on the command line. ariesd refuses to start, naming `chmod 600`, when the password file (`passfile=`, else `PGPASSFILE`, else `~/.pgpass`) or the key of a client certificate (`sslkey=`, else `PGSSLKEY`, else `~/.postgresql/postgresql.key` beside its certificate) is open to group or others, as libpq does | required | `--dsn`, `ARIESD_DSN` |
| `project` | The project's UUID, in lowercase. A client naming another project is refused. With `control_schema`: the project `control init` adopts, and the one `import-bd`, `export-bd` and the health check of `upgrade` use by default | required | `--project`, `ARIESD_PROJECT` |
| `control_schema` | The schema of the control store, in the database `dsn` names and as its role. Setting it makes this a server of [many projects](#serving-many-projects), each in a schema ariesd names `project_NAME` ([Opt in](#opt-in), step 3). A lowercase letter, then lowercase letters, digits and underscores; not the schema `dsn` names, and not starting `project_` | none: one project | `ARIESD_CONTROL_SCHEMA` |
| `transports` | What `serve` serves: `[https]`, `[https, ssh]` or `[ssh]` ([SSH on and off](#ssh-on-and-off)). With SSH off, ariesd writes no keys file and refuses the bridge | `[https]` | |
| `https` | The HTTPS listener: `listen`, `public_urls` and `tls` ([The HTTPS listener](#the-https-listener)). Required when `transports` names `https` | none | |
| `https.refuse_key_proof_v1` | Refuse the key proof of `aries` of protocol 8 (v0.5.0), which is not bound to the TLS session ([How aries signs in over HTTPS](#how-aries-signs-in-over-https)). Set it once every client is v0.6.0 or later; it is off until `ProtoMin` moves to 9 | `false` | |
| `keys_file` | With `control_schema` and SSH on: the file ariesd writes the control store's keys to, for sshd ([Keys from the store](#keys-from-the-store)). A relative path is under the home directory of the user ariesd runs as | `.ssh/aries_keys` | `ARIESD_KEYS_FILE` |
| `authorized_keys` | With `control_schema`: the hand-maintained file sshd reads beside it. ariesd only reads it, to say which keys are in both. Never the same file as `keys_file` | `.ssh/authorized_keys` | `ARIESD_AUTHORIZED_KEYS` |
| `bridge` | With `control_schema`: the path of ariesd in the forced command of each line of `keys_file`. Absolute, of letters, digits and `._+-` | `/usr/local/bin/ariesd` | `ARIESD_BRIDGE` |
| `socket` | The daemon's unix socket | `/run/aries/ariesd.sock` | `--socket`, `ARIESD_SOCKET` |
| `prefix` | The prefix of new issue ids | `ar` | `--prefix` |
| `admins` | [Admins](#admins) | none | `ARIESD_ADMINS` |
| `systemd_unit` | The unit `ariesd upgrade` restarts and health-checks | none | |
| `releases` | Where `ariesd upgrade` reads releases from: the `https://` URL of a mirror, or the absolute path of a directory ([Upgrades](#upgrades)) | GitHub | `ARIES_RELEASES`, the variable `aries` and `install.sh` read |
| `log_level` | `debug`, `info`, `warn` or `error` ([Logs](#logs)) | `info` | `--log-level`, `ARIESD_LOG_LEVEL` |
| `log_format` | `text` (key=value) or `json`, for a log shipper | `text` | `--log-format`, `ARIESD_LOG_FORMAT` |
| `account` | The project's account: what an issue's time and tokens report against when neither it nor an ancestor sets one ([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)). A code name: lowercase letters, digits and inner hyphens | `internal` | |
| `clock_behind` | What `serve` does when it finds the clock behind the server's own history by more than `clock_behind_tolerance`: `stop` refuses to start, `warn` starts with a warning that repeats ([The server's clock](#the-servers-clock)). Any other value is refused | `stop` | `ARIESD_CLOCK_BEHIND` |
| `clock_behind_tolerance` | How far behind its history the clock may be at start before `clock_behind` applies. 1s to 24h. Raise it only for a machine known to boot with a clock further off than this and no time service to correct it; lower it if ariesd always starts after synchronization | `2m` | |
| `clock_step_tolerance` | How far elapsed wall-clock time and elapsed monotonic time may differ between two readings before ariesd calls it a step of the clock. 1s to 10m. Lower it, to 1s at the least, to be told of smaller corrections; raise it only if a time service on the machine is known to step by more in normal running | `5s` | |
| `clock_client_tolerance` | How far past the server's clock a time a client supplies may be: a usage record's. 1s to 24h. Raise it if people's machines are known to run ahead and their usage records are being dropped | `5m` | |
| `limits` | [Limits](#limits) | defaults | |
| `conflict_age` | How long a conflict is open before `digest` lists it, under old conflicts. It bounds nothing, so it is not under `limits:` | `1d` | |
| `due_soon` | How far ahead of its due date `digest` and `prime` list an issue as due soon ([Dates](concepts.md#dates)). Up to a year. It bounds nothing, so it is not under `limits:` | `1d` | |
| `login.invite_ttl` | With `control_schema`: how long an [invitation](#identities-and-invitations) stays open unused, from `1h` to `90d`. The rest of `login:`, the providers people sign in at, comes with sign-in itself | `7d` | |
| `workload` | With `control_schema` and HTTPS: the platforms whose tokens [service principals](#service-principals) sign in with (`providers`), the audience a job asks for, a sign-in's `token_lifetime` and `max_token_lifetime`, and `fresh`, how recent a platform's token must be | none: no service principal signs in by a token | |
| `telemetry.listen` | The loopback address, `HOST:PORT`, of the listener for `/metrics`, `/healthz` and `/readyz` ([Observability](#observability)). An address that is not loopback is refused | none: no listener | `ARIESD_TELEMETRY_LISTEN` |
| `telemetry.max_requests` | With `telemetry.listen`: requests the listener serves at once. One more is answered `503`; twice as many connections may be open, and one more is closed | 8 | |
| `telemetry.per_project` | Whether metrics name the project. `false` leaves it off every instrument | `true` | |
| `telemetry.principals` | How a person appears on a span and on a log record exported over OTLP ([Traces](#traces), People): `pseudonym`, a stable stand-in; `name`; or `omit`, nothing. It does not change the log on stderr, nor the audit stream there. Any other value is refused | `pseudonym` | |
| `telemetry.audit_log` | Whether each access change in the control store's log is also written as a log line with `audit=true` ([The audit stream](#the-audit-stream)). `false` writes none; the event log is the record either way | `true` | |
| `telemetry.principals_key` | The file that holds the key the stand-ins are made with. ariesd makes it, mode 0600, when it exports traces with stand-ins. A relative path is under the home directory of the user ariesd runs as. A file that cannot be read or made does not stop ariesd, whether the path is this default or one you set: its spans then say nothing about who, and its log and status say why ([Traces](#traces), People) | `telemetry/principals.key` | |

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
- **The database role is checked** by every command that opens the
  store: `serve`, `control init`, `import-bd`, `export-bd` and `upgrade`.
  [The database role](security-model.md#the-database-role) lists what
  they refuse. So is the server: Postgres 16 or newer.
- **`--dev`** is for a developer's own machine. It lets `serve` run off
  Linux. With `--dev`, the commands that open the store also accept
  `--allow-unsafe-account`, which lets a role with too many rights, such
  as a superuser, through with a warning.
  No config file or variable can set either.

## Limits

`limits:` in the config file bounds what one request or one principal can
make the daemon do. Leave a field out for its default:

```yaml
limits:
  writes_per_second: 20
  write_burst: 200
```

| Setting | Default | Bounds |
|---|---|---|
| `labels_per_issue` | 50 | Labels on one issue, and so in one `create` |
| `acceptance_items_per_issue` | 200 | Items in acceptance text, and item numbers in one `accept` or `finish` |
| `deps_per_issue` | 200 | Edges out of one issue |
| `sessions_per_principal` | 256 | A principal's rows in the `who` registry; a new session past it drops the least recently seen |
| `agent_keep` | `7d` | How long an unseen registry row is kept. A principal's latest row is always kept, so it stays mentionable |
| `unread_per_principal` | 1000 | A principal's unread inbox items; past it, the oldest are marked read (still under `inbox --all`) |
| `notices_per_minute` | 10 | Mentions, assignments and handoffs one principal can send another a minute; the rest are not delivered. Lost claims, locks granted or lost, and sign-ins always are |
| `inbox_keep` | `30d` | How long a read inbox item is kept |
| `idem_window` | `7d` | How long an idempotency key gives back its request's result: a write a client sends again with the same key within it, as an outbox does after an outage, applies once and gets the first answer. A key used longer ago is refused with `stale`, saying its request was applied then, and nothing is written; the same key with other arguments is `conflict`. It is how long a write can wait on a person's machine and still be delivered without them. From `1h` to `365d`. Keys are kept on the events they stamped whatever this is, so it bounds no table: a key is never applied twice, however late it comes back |
| `conns_per_server` | 1024 | Connections past the handshake, refused with `busy` beyond it. Twice this caps the sockets the daemon holds in all; at that cap a new connection is let in and the one that has waited longest without a hello is closed |
| `conns_per_principal` | 64 | One principal's connections: one person's sessions on several machines, and a few unattended runs on their key. Agents that run unattended in numbers should have principals of their own, and meet `conns_per_server` instead. Each connection costs the server about 5 MiB, most of it the bridge and sshd outside ariesd ([What a server needs](#what-a-server-needs)): 64 connections about 320 MiB, and `conns_per_server`' 1,024 about 5 GiB (550 MiB and 8.6 GiB with transparent huge pages `always`). Past it, every new connection of the principal is refused with `busy`, `alice is at its connection limit (64)`, until one of its sessions closes: an `aries` command fails, saying to close some sessions or have this limit raised; an `aries mcp` session that is refused cannot reach the server at all, tells its agent so, and tries again at most every 5 seconds. Its claims lapse if it stays refused for their 15-minute lease, and its writes are not queued |
| `handshakes_per_principal` | 16 | One principal's connections that sshd has let in and that have not yet sent their hello, on the whole server. A connection has 10 seconds to send it. Past the cap the next is refused with `busy` at once, so one key holder cannot use up the sockets everyone else connects through. The upgrade's health probe is not counted |
| `idle_timeout` | `10m` | A connection that sends nothing this long is closed with a note, unless it watches its inbox (`aries mcp`, `aries watch` and `aries tui` do) |
| `db_conns_per_server` | 32 | Connections to Postgres in ariesd's one pool, which every project's store and the control store share ([One writer for each project](#one-writer-for-each-project)); ariesd holds one more for the writers' locks. A request that finds every one in use waits for one, which the status report and `aries.db.pool.waits` count. Postgres's `max_connections` must leave room for them |
| `db_conns_per_project` | 8 | Connections one project's reads hold at once, so that a busy project leaves the pool to the others; its writes hold one more. At most `db_conns_per_server` |
| `writes_per_second`, `write_burst` | 10, 100 | Each principal's write token bucket: writes a second, and how many at once. Reads are not counted |
| `refusal_logs_per_minute` | 20 | Refusal log lines per principal a minute; those of HTTPS and sign-in refusals per address and reason, so behind a proxy one kind of refusal cannot hide another |
| `records_per_usage_call` | 500 | Token usage records in one `usage` call |
| `usage_per_day` | 50000 | Token usage records one principal may add in 24 hours, counted by when the server stored them. Per principal, because a client can name any number of sessions |
| `paths_per_issue` | 200 | Paths one issue keeps, declared and from commits, and the commit paths one `renew`, `finish` or `handoff` records. Too many declared paths are refused with `invalid`; past the cap, commit paths keep the request's first (most recent) and the issue's most recently recorded, and the request still succeeds |
| `memory_body_bytes` | 4096 | Bytes in one memory's body (at most 65535) |
| `memory_key_bytes` | 128 | Bytes in a memory's key (at most 255) |
| `tags_per_memory` | 20 | Tags on one memory |
| `memory_tag_bytes` | 64 | Bytes in one memory tag (at most 255) |
| `memories_per_scope` | 1000 | Memories one principal may author in one scope, forgotten ones included. A new key at the cap first deletes that principal's oldest forgotten memories in the scope; only when live memories fill it is the key refused with `invalid`, and its fix says to forget some or raise the limit; replacing an existing memory is not refused. `import-bd` is exempt |
| `prices_per_project` | 1000 | Rows of the prices table (at most 10000, since every cost report, show and digest reads them all): a model's rates from one date are one row. A new price past it is refused with `invalid`, and its fix says to replace an existing one or raise the limit; replacing a price is not refused |
| `plans_per_project` | 1000 | Rows of the plans table (at most 10000, since every cost report reads them all): a plan's terms from one month are one row. A new row past it is refused with `invalid`, and its fix says to replace an existing one or raise the limit; replacing a row is not refused |
| `principals_per_plan` | 100 | Principals one plan row names (at most 1000). `plans_per_project` times `principals_per_plan` is at most 100000, the principals a cost report may read, so raising one past its default means lowering the other; the server refuses to start otherwise |
| `hours_per_day` | 50 | Hours entries one principal may have on one day. Past it a new entry is refused with `invalid`, and its fix says to undo some and log their sum, or raise the limit |
| `hours_note_bytes` | 500 | Bytes in an hours entry's note (at most 65535) |
| `conflicts_per_issue` | 20 | Open conflicts on one issue: fields of replayed updates parked against others' changes ([Conflicts](cli.md#conflicts)). An update that would park past it is refused with `invalid`, and nothing of it applies; its fix says to resolve the open ones (`aries conflicts`, `aries resolve`) and `aries outbox retry` it, or to raise the limit. A resolved conflict no longer counts |
| `conflicts_per_principal` | 100 | Open conflicts one principal's replayed updates have parked, on every issue; past it, refused as `conflicts_per_issue` is |
| `approvers_per_gate` | 10 | Principals one [gate](concepts.md#gates) names (at most 100). More are refused with `invalid`. A gate that names none tells at most this many of the project's admins that it waits for them, the project's own before the server's |
| `open_gates_per_project` | 1000 | Open gates in a project. One more is refused with `busy`, and its fix says to pass or waive an open one (`aries gate list`) or raise the limit. The gates on one issue are its dependencies, so `deps_per_issue` bounds them |
| `checks_per_issue` | 20 | [CI gates](ci.md) whose subject is one issue, which is its checks, one gate each (at most 100). One more is refused with `invalid` |
| `gate_evidence_bytes` | 1024 | Bytes of a CI report's evidence, its commit, link and reason together (at most 4096). More is refused with `invalid`, as is a link with a user or password, or evidence that looks like a secret |
| `locks_per_project` | 200 | [Locks](concepts.md#locks) in use in a project: names someone holds or waits for (at most 10000). A lock nobody holds or waits for is gone and does not count. One more is refused with `busy`, and its fix says to see `aries lock list` or raise the limit |
| `holders_per_lock` | 64 | The capacity a lock may be given, holders at once (at most 1000). More is refused with `invalid` |
| `waiters_per_lock` | 100 | Places in one lock's queue (at most 1000). One more is refused with `busy` |
| `lock_waits_per_principal` | 20 | Places one principal has in all of a project's queues, from every session (at most 1000). One more is refused with `busy`, and its fix says to leave a queue (`aries lock leave NAME`) or raise the limit. A principal's holds need no cap of their own: `locks_per_project` times `holders_per_lock` bounds them |
| `workflow_bytes` | 16384 | Bytes of the project's [workflow](concepts.md#the-workflow) as the server keeps it (at most 65536); every `create` reads it. A larger one is refused with `invalid` at `aries admin workflow set` |
| `workflow_items_per_type` | 20 | Default acceptance items in one profile of the workflow (at most 100). More are refused with `invalid` at set. The items an issue ends with, its own and the workflow's, are capped by `acceptance_items_per_issue`: a `create` past it is refused with `invalid`, naming both counts |
| `workflow_checks_per_type` | 5 | Required checks in one profile of the workflow (at most `checks_per_issue`, since each is a CI gate on the issue). More are refused with `invalid` at set |
| `workflow_closeout_per_type` | 20 | Close-out lines in one profile of the workflow (at most 100). More are refused with `invalid` at set |
| `away_max` | `7d` | The longest `aries away` may extend a person's claims, in every session, ahead of an absence; asking for longer is refused with `invalid`, its fix naming the cap. A claim so extended holds its issue against everyone else until then, so this bounds how long work can be held by someone who is not doing it. From `1h` to `30d` |
| `open_stores_per_server` | 64 | With `control_schema`: project stores open at once. An open store holds no connection to Postgres of its own, only its writer's lock, and takes connections from the pool while it answers ([One writer for each project](#one-writer-for-each-project)). At the cap, a connection to another project closes the store that has been without connections longest; when every open store has one, it is refused with `busy` |
| `store_idle` | `10m` | With `control_schema`: how long a project's store stays open with no connection. Closing it gives up its writer's lock and what ariesd holds of it in memory |
| `projects_per_server` | 500 | With `control_schema`: projects on the server, archived ones included |
| `principals_per_server` | 5000 | With `control_schema`: principals in the control store, disabled ones included |
| `principals_per_creator` | 500 | With `control_schema`: principals one principal who is no server admin may create, disabled ones included. No command deletes a principal and names are the whole server's, so this keeps one project admin from using up `principals`. Past it `principal add` is refused with `invalid`, and its fix says to ask a server admin, who has no such cap, or to raise the limit |
| `keys_per_principal` | 20 | With `control_schema`: public keys of one principal |
| `members_per_project` | 1000 | With `control_schema`: principals with a role in one project |
| `identities_per_principal` | 8 | With `control_schema`: [identities](#identities-and-invitations) of one principal, bound, open or lapsed: the accounts at providers a person signs in with. One more is refused with `invalid`, and its fix names `aries admin identity list NAME` and `identity remove` |
| `invites_per_server` | 1000 | With `control_schema`: open [invitations](#identities-and-invitations) on the server, those no sign-in has bound and that have not lapsed; a lapsed one does not count. One more is refused with `invalid`, and its fix names `aries admin invite list` and `invite remove` |
| `grants_per_principal` | 64 | With `login:` or `workload:` providers: one principal's live [sign-ins](#sign-in), one for each machine signed in; an ended one does not count. One more is refused `busy`, and its fix names `aries logout` on a machine no longer used and `aries admin signin end` |
| `trust_rules_per_server` | 1000 | With `control_schema`: the [trust rules](#service-principals) on the server. One more is refused with `invalid`, and its fix names `aries admin trust list` and `trust remove` |
| `trust_rules_per_principal` | 16 | With `control_schema`: the trust rules of one service principal. One more is refused as above |
| `grants_per_trust_rule` | 64 | With `workload:` providers: the live sign-ins one trust rule has given, one for each job of a matrix; an ended one does not count. One more is refused `busy`, and its fix names `aries logout` in a step that always runs |
| `control_writes_per_second`, `control_write_burst` | 1, 30 | With `control_schema`: each principal's token bucket for `aries admin` changes to projects, principals, members, keys, invitations and identities: changes a second, and how many at once. It is the principal's on the whole server, apart from its write bucket in each project; the lists are not counted |
| `https_conns_per_server` | 4096 | With HTTPS on: open TCP connections to the [HTTPS listener](#the-https-listener). One more is closed unanswered |
| `https_conns_per_addr` | 64 | With HTTPS on: open connections from one client address; one more is closed unanswered. Behind a proxy that terminates TLS every connection is the proxy's, so this is the proxy's to keep, and does not apply |
| `https_addr_prefix_v6` | 64 | With HTTPS on: how many leading bits of an IPv6 address name its client for `https_conns_per_addr`, `auth_failures_per_minute` and `signins_pending_per_addr`, so that the 2^64 addresses of one /64 share one share of each. An IPv4 address is counted alone, an IPv4-mapped one as IPv4. From 1 to 128; lower it (56, 48) where clients are given more than a /64, and raise it to 128 to count every address alone |
| `https_header_bytes` | 16384 | With HTTPS on: bytes of a request's headers; more is answered 431 |
| `https_header_timeout` | `10s` | With HTTPS on: how long a request's headers may take to arrive, the slow client's bound; and how long a connection kept alive may wait idle for its next request |
| `https_body_bytes` | 65536 | With HTTPS on: bytes of a request's body |
| `https_body_timeout` | `10s` | With HTTPS on: how long a request's body may take to arrive once its headers are in, the bound of a client that names a body and sends none, or sends it a byte at a time. The connection is closed at the end of it, or at once when ariesd stops. A WebSocket's upgrade is not held to it |
| `upgrade_timeout` | `10s` | With HTTPS on: how long a WebSocket has from its upgrade to the end of its key proof; the hello then has 10 seconds, as over SSH |
| `ws_message_bytes` | 4194304 | With HTTPS on: bytes of one WebSocket message, which carries one frame (at most 4 MiB, a frame's own bound). A larger one closes the connection. Until its key is proved, a connection's messages are held to 64 KiB, the key proof's own bound, whatever this says |
| `ws_write_timeout` | `30s` | With HTTPS on: how long one frame the server writes to a WebSocket, once its key is proved, may take. A client that signs in, then stops reading, makes the write wait; when it has waited this long the connection is closed and its slot (`conns_per_principal`, `https_conns_per_server`) freed. Until the key is proved, `upgrade_timeout` bounds the connection |
| `auth_failures_per_minute`, `auth_failure_burst` | 30, 60 | With HTTPS on: each client address's token bucket of refused key proofs (a key the server does not know, a proof that does not verify; not a disabled principal's key, proved), refused sign-in tokens at `/v1/connect`, and, with `login:` providers, refused requests to `/oauth/token` (a DPoP proof, a code or a PKCE verifier refused), and sign-ins refused past `signins_pending_per_addr`: refusals a minute, and how many at once. With none left an upgrade, a token request or a sign-in begun from the address is refused `busy`, 429, saying how long to wait. Behind a proxy it does not apply, as `https_conns_per_addr` does not |
| `signins_pending_per_server` | 10000 | With `login:` providers: [sign-ins](#sign-in) in the daemon's memory started and not finished, sent to the provider (10 minutes each at most) or with a code `aries` has not spent (a minute). Past it the oldest is let go of, and counted, so that sign-ins never meant to finish cannot refuse a person's; one let go of tells `aries login` to run again |
| `signins_pending_per_addr` | 16 | With `login:` providers: of those, the ones started from one client address. One more is refused, back at `aries`, `temporarily_unavailable`, and counts against the address's `auth_failures_per_minute`; an address with none left is refused sign-ins too, saying how long to wait. Behind a proxy it does not apply, as `https_conns_per_addr` does not |
| `dpop_proofs_per_server` | 100000 | With `login:` or `workload:` providers: the DPoP proofs the daemon remembers, each for two minutes, so that none is used twice. Only a proof whose request has earned it is remembered: at `/oauth/token`, one that comes with a live code and its PKCE verifier; at `/v1/connect`, one by the key of a live sign-in's token. Past it a proof is refused `busy` until the oldest age out |
| `workload_tokens_per_server` | 10000 | With `workload:` providers: the workload tokens the daemon remembers as exchanged, each for `workload: fresh` and five minutes more, so that none signs in twice. Only a token that verified is remembered. Past it an exchange is refused `busy` until the oldest age out |

On a server of [many projects](#serving-many-projects), every limit above
that is per principal is per principal in each project: `conns_per_principal`,
the write bucket, `refusal_logs_per_minute` and the limits on rows a principal owns.
`conns_per_server` stays the whole server's, and the `aries admin` bucket is each
principal's on the whole server. So is `handshakes_per_principal`, since
a connection has named no project before its hello. Past `principals_per_server`,
`keys_per_principal` or `members_per_project`, `control init` skips the
line and says which limit it met, and `aries admin` refuses the row with
`invalid` and names the limit; past `projects_per_server` it refuses a new project
the same way. `principals_per_creator` applies to a principal who is no
server admin, and never to `control init`.

Before the hello, a server of many projects also refuses at once a
principal that is disabled, or that the control store does not know
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
([What was run, and where](#what-was-run-and-where)). On amd64 and arm64
GitHub's runners it was run with the profiles in enforce mode on 16
October 2026, with no denial in the journal.

Ubuntu runs AppArmor, and out of the box it confines neither sshd
nor a program it has no profile for: on the machines above, `aa-status`
listed no profile for sshd, sshd and its children ran `unconfined`, and
so did ariesd. Aries ships three profiles for ariesd
([deploy/apparmor](../deploy/apparmor/README.md)), in two files: the
daemon's and the operator's in `ariesd`, and the bridge's in
`ariesd-stdio`, which only a server with SSH on installs. A fourth,
`ariesd-default` in a file of its own, is what a server with SSH off
installs in the bridge's place. With SSH on
they matter most for the bridge, `ariesd stdio`: sshd starts it for every connection,
outside the systemd unit and so with none of the unit's protections,
as a user that owns its own binary, its config file and its
`authorized_keys`.

| Profile | Runs | May |
|---|---|---|
| `ariesd-stdio`, with SSH on | `ariesd stdio`, `ariesd status`, and anything else run from the binary's path with no profile asked for | Read the config file and connect to the daemon's socket. No network, no other file, no program |
| `ariesd-default`, with SSH off | `ariesd status`, `control init`, `import-bd`, `export-bd`, `upgrade`, and anything else run from the binary's path with no profile asked for | The operator's rules, below, so a command run without `aa-exec` is confined as one run with it |
| `ariesd-serve` | `ariesd serve`, because the unit asks for it | Read the config file, listen on the socket, reach Postgres by its unix socket or over TCP and read the database's client files in `/etc/aries/postgres`, write the keys file and read `authorized_keys`; resolve names and read the machine's certificate authorities, for a managed Postgres or a hosted backend; listen on the loopback port for metrics and health, post metrics and traces to a collector, make and read the key of the stand-ins its traces name people by, and write to systemd's notify socket ([Observability](#observability)). No program |
| `ariesd-admin` | `control init`, `import-bd`, `export-bd` and `upgrade`, when the operator asks for it with `aa-exec` | Open the store as the daemon does, read and write files in the top of `/var/lib/aries`, read `authorized_keys`, reach the network, replace the binary in `/opt/aries/bin`, and run `sudo` |

AppArmor picks a profile by the program's path and never by its
arguments, so one path can carry one profile by itself. With SSH on it
carries the smallest, since sshd has no way to ask for another. None of
the profiles may write the config file or `authorized_keys`, and only
the operator's may write the binary. With SSH off nothing runs as
sshd's forced command, so the bridge's profile is not installed and the
path carries `ariesd-default` instead, which has the operator's rules
(they are one file, `abstractions/ariesd-admin`, that it and
`ariesd-admin` both include): `ariesd status`, and anything else run
from the path with no profile asked for, is confined, and the
operator's commands need no `aa-exec`
([SSH on and off](#ssh-on-and-off)). The two are never loaded
together; `install-server.sh` swaps them when SSH is turned on or off.

**Install them**, as root:

```sh
cd "$(mktemp -d)"
curl -fsSL -o profile https://raw.githubusercontent.com/ariesworx/aries/main/deploy/apparmor/ariesd
curl -fsSL -o tunables https://raw.githubusercontent.com/ariesworx/aries/main/deploy/apparmor/tunables/ariesd
curl -fsSL -o admin https://raw.githubusercontent.com/ariesworx/aries/main/deploy/apparmor/abstractions/ariesd-admin
install -m 0644 profile /etc/apparmor.d/ariesd
install -m 0644 tunables /etc/apparmor.d/tunables/ariesd
install -d -m 0755 /etc/apparmor.d/abstractions
install -m 0644 admin /etc/apparmor.d/abstractions/ariesd-admin
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

**Only with SSH on**, install the bridge's profile too:

```sh
curl -fsSL -o stdio https://raw.githubusercontent.com/ariesworx/aries/main/deploy/apparmor/ariesd-stdio
install -m 0644 stdio /etc/apparmor.d/ariesd-stdio
apparmor_parser -r /etc/apparmor.d/ariesd-stdio
```

The installer's `--apparmor` installs it with SSH on, and not without;
`--disable-ssh` unloads and removes it. If `ariesd-default` is loaded,
unload it first (below): both attach to the binary's path.

**Only with SSH off**, install the default profile instead, which holds
the binary's path with the operator's rules:

```sh
curl -fsSL -o default https://raw.githubusercontent.com/ariesworx/aries/main/deploy/apparmor/ariesd-default
install -m 0644 default /etc/apparmor.d/ariesd-default
apparmor_parser -r /etc/apparmor.d/ariesd-default
```

Turning SSH on or off later swaps the two: `apparmor_parser -R` and
`rm` the one, then install the other. The installer's `--apparmor` does
it. Profiles from before 16 October
2026 held the bridge's in `/etc/apparmor.d/ariesd`, named `ariesd`:
install this release's files as above, and move rules of your own for
the bridge from `local/ariesd` to `local/ariesd-stdio`. The installer
shows how the installed file differs, and leaves it to you. A profile
`ariesd` the old file loaded stays loaded until the next boot, and
attached to the binary's path as before: `apparmor_parser -R` on the
old file, before you replace it, unloads it.

**On Ubuntu 26.04 a profile changed to is stacked.** There
`kernel.apparmor_restrict_unprivileged_unconfined` is 1 (on 24.04 it
is 0), so when a process that is not root changes to a profile, as
systemd does for the unit's `User=aries` and `aa-exec` does for the
`aries` user, the kernel stacks the profile on `unconfined`:
`ariesd-serve//&unconfined`, and `ariesd-admin//&unconfined`. The
profile still decides, since a stack allows only what each of its
profiles allows: under the operator's, `export-bd -o` into `.ssh` was
still refused and logged. The bridge, which its path
attaches, is a plain `ariesd-stdio`. What the stack changed, each in this
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

- With SSH off, `aa-exec` is not needed: the path carries
  `ariesd-default`, which has the same rules, so such a command is
  confined as it is, and `aa-exec -p ariesd-admin` gives the same result.
- With SSH on, without `aa-exec` such a command runs under the
  bridge's profile and is refused with `permission denied`: `control init` on
  `authorized_keys`, anything that opens the store on Postgres's
  socket (`/var/run/postgresql/.s.PGSQL.5432`), and `upgrade` on its
  lookup of the release host (`api.github.com`, unless `releases:` names
  another).
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
  status` runs under the bridge's profile with SSH on, which allows the
  two things it does, and under `ariesd-default` with SSH off. With SSH on root
  cannot run it directly, because that profile gives root no way past
  the config file's mode; the refusal says to run it as `aries`.

**If you moved a path** (the binary, the config file, the socket, the
home directory, `keys_file`, `authorized_keys`,
`telemetry: principals_key`, Postgres's socket or the database's client
files), change it in
`/etc/apparmor.d/tunables/ariesd`, which names each one once, and
load the profiles again with `apparmor_parser -r
/etc/apparmor.d/ariesd`, with SSH on `apparmor_parser -r
/etc/apparmor.d/ariesd-stdio`, and with SSH off `apparmor_parser -r
/etc/apparmor.d/ariesd-default`. Rules of your own go in
`/etc/apparmor.d/local/ariesd-serve`, `local/ariesd-admin` and
`local/ariesd-stdio`.

**A Postgres on another machine, a managed service, and a hosted
backend need nothing added.** The daemon's profile allows DNS
(`network inet dgram`) and the machine's certificate authorities
(`abstractions/ssl_certs`), which a DSN with a host name and
`sslmode=verify-full` needs, and so does a collector or a backend named
through DNS or behind TLS (design decision 85). Profiles from before
Postgres left both to three lines in `local/ariesd-serve`, which may
stay: they allow nothing more. A certificate authority of the database's
own, which `sslrootcert=` names, belongs in `/etc/aries/postgres`, which
the daemon's and the operator's profiles read
([Managed services](#managed-services)).

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
[ ! -e /etc/apparmor.d/ariesd-stdio ] || apparmor_parser -R /etc/apparmor.d/ariesd-stdio
[ ! -e /etc/apparmor.d/ariesd-default ] || apparmor_parser -R /etc/apparmor.d/ariesd-default
rm -f /etc/apparmor.d/ariesd /etc/apparmor.d/ariesd-stdio /etc/apparmor.d/ariesd-default /etc/apparmor.d/tunables/ariesd /etc/apparmor.d/abstractions/ariesd-admin
rm /etc/systemd/system/ariesd.service.d/apparmor.conf
systemctl daemon-reload
systemctl restart ariesd
```

Unload the profiles and remove the unit's line together. With the line
and no profile, ariesd does not start (`Failed to prepare AppArmor
profile change to ariesd-serve`). With the profiles and no line, the
daemon gets the bridge's profile, with SSH on, and cannot reach
Postgres.

What they do not do:

- **They do not keep a daemon that has been taken over away from the
  keys file.** `ariesd-serve` must write the file sshd reads keys
  from, and sshd accepts any line in it. The bridge and the operator's
  commands cannot write it.
- **They do not name Postgres's address.** `ariesd-serve` may open TCP
  connections anywhere: rules that name an address or a port were not
  tested, and the profile does not rely on them. With Postgres on the
  machine, which the daemon reaches by its unix socket, the unit can
  hold the daemon to this machine instead: `IPAddressDeny=any` and
  `IPAddressAllow=localhost` in a drop-in (run on the machine above
  before Postgres, against a database on 127.0.0.1). Leave them out if
  `dsn` names another machine, or telemetry goes to one.
- **They do not hold the listener to loopback.** ariesd does that
  itself, refusing any other address at start; to AppArmor the listener
  is one more TCP socket.
- **They confine neither sshd nor Postgres.** Ubuntu ships no profile
  for either, and both of sshd's keys files worked with the bridge
  confined. Postgres runs as its own user, `postgres`, under its own
  unit.

What was tested, with the profiles enforced: a sequence taken from the
[Docker smoke test](deploy/docker.md#run-the-smoke-test) (issues, a
second project, keys from the control store, a key removed and a
principal disabled, restarts) and the operator's commands above, 76
checks, before and after a reboot, with no line in the kernel log; then
attempts to do what the profiles forbid, each refused and logged. The
76 checks were run again, and passed with no line in the kernel log,
with the unit's `Type=notify` and watchdog, the listener and metrics
pushed to a receiver on the machine; each of the three lines that
DNS and the certificate authorities then needed was shown to be needed,
and to be enough, against a receiver named through DNS and one behind
TLS on the same machine. What was not: an upgrade to a newer release
(its `--check` and `--rollback` were run), and amd64.

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

**The runs above were made before Postgres**, with the database Aries
used until decision 79, which the daemon reached over TCP on loopback.
The rules for Postgres (its unix socket, the database's client files,
and DNS and the certificate authorities, now in the profile itself)
were checked with `apparmor_parser -Q -K` under 24.04's parser (4.0.1)
and 26.04's (5.0.2), as CI checks them, on 15 October 2026, and were
not run under an enforcing kernel: Docker Desktop's has no AppArmor.
Until they are, load them with `apparmor_parser -r -C` first, use
`ariesd`, `aries` and the operator's commands, and look for denials as
below before you enforce them. The installer's `--apparmor` loads them
enforced: after it, look for denials the same way.

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
| `project` | A project's UUID. With `control_schema`, on every line about one project |
| `principal`, `session` | Whose connection or request the line is about; on a claim's line, the holder; on an audit line, who made the change |
| `machine`, `harness` | What the client called its machine and its agent harness |
| `op`, `code`, `dur_ms` | A request's operation, a refusal's code, and how long the request took in milliseconds |
| `req`, `trace_id`, `span_id` | The request's id and its span's ids, above |
| `audit`, `seq`, `at`, `target`, `subject`, `fingerprint`, `role` | The [audit stream](#the-audit-stream)'s. `subject` and `fingerprint` are also on other lines about a principal or a key that is not the line's `principal` |
| `by`, `admin`, `created_by`, `added_by` | A principal other than the line's own: whose change caused it, a configured server admin, and who made that admin's row or added its key |
| `err`, `detail`, `cause`, `fix`, `effect` | An error's text; what the database driver, the SDK or the HTTP server said; why something is wrong, what to do about it, and what follows meanwhile |
| `issue`, `epoch`, `reason` | A claim's issue and number, and a few fixed words for why (`membership removed`, `principal disabled`, `project archived`, `key removed`, `idle`, `to make room`, `archived`, `connection limit`) |
| `name`, `schema`, `account`, `problems`, `resumed` | A project's name and schema; the database role and what is unsafe about it; whether a `project add` resumed one begun before |
| `after`, `oldest`, `connections`, `own`, `lines`, `per_minute` | Connections: how long one idled or how long before a retry, whether the one closed was the oldest waiting, how many were dropped and whether the admin's own was one, and how many refusal lines the cap left out of how many allowed |
| `agents`, `inbox`, `claims`, `locks`, `extended` | How many registry rows and inbox items a prune removed; how many claims' leases, and locks' holds and places, a clock step extended, and by how much |
| `gate`, `kind`, `check`, `run` | A [gate](concepts.md#gates)'s id and kind (`human` or `ci`), a ci gate's check and the number of the CI run whose report changed it. A check's name is text a person chose, so it stays in the journal and is never exported ([Logs over OTLP](#logs-over-otlp)); `issue` is a ci gate's subject |
| `lock`, `waiting` | A [lock](concepts.md#locks)'s name, and whether what ended was a place in its queue rather than a hold. A lock's name is text a person chose, so it stays in the journal and is never exported ([Logs over OTLP](#logs-over-otlp)) |
| `path`, `keys` | The keys file, or the stand-in key's file, and how many keys were written |
| `mode`, `urls` | The HTTPS listener's TLS mode (`acme`, `files` or `proxy`) and the server's URLs |
| `transports` | At start: the transports served, `https`, `ssh` or both |
| `provider`, `grant`, `flow`, `signins` | [Sign-in](#sign-in): a provider's name in the settings, a sign-in's id (random, no secret), how it came (`loopback`, `url`, `workload`), and how many ended sign-ins a prune deleted. No token, code, address or subject is ever on a line |
| `trust_rule` | A [service principal's](#service-principals) sign-in: the trust rule it matched, a name an admin chose. It stays in the journal, and is not exported |
| `log`, `clock`, `history`, `behind`, `since`, `was_behind_since`, `direction`, `step`, `steps`, `from`, `to`, `forward_since_start`, `backward_since_start` | [The server's clock](#the-servers-clock): which history, the two times, how far behind and since when, and a step's direction, size and count |
| `version`, `socket`, `control_schema`, `projects`, `admins`, `addr` | The start: the build, the socket, the control store, how many active projects, the configured admins, and the listener's address |
| `push`, `push_interval`, `push_headers`, `pull`, `per_project`, `traces`, `trace_headers`, `sampler`, `logs`, `log_headers`, `principals`, `service` | What telemetry is on ([Observability](#observability)). Header counts, never their names or values |
| `signal`, `failed`, `failures_not_logged` | An export that failed or recovered: `metrics`, `traces` or `logs`, and how many failures |

### Events

| Event | Level | Logged when |
|---|---|---|
| `daemon.serving` | info | ariesd listens on its socket |
| `admin.unvouched` | warn | At start: a principal named under `admins:` has a row or a key that someone who is no server admin made. Check the key is theirs |
| `admin.check_failed` | error | At start: that check could not read the control store |
| `systemd.notify_failed` | warn | A notification could not be sent to systemd: ready, or the watchdog's |
| `log.suppressed` | warn | A principal's refusal lines went past `refusal_logs_per_minute`; `lines` is how many were left out |
| `conn.accept_failed` | warn | Accepting a connection failed in a way that may pass, such as running out of file descriptors; ariesd retries after a pause of up to a second |
| `conn.cap_refused` | warn | A connection that had not said hello was closed at twice `conns_per_server` |
| `conn.peer_refused` | warn | A socket peer that is not the user ariesd runs as was refused |
| `conn.opened` | debug | A connection passed its handshake |
| `conn.idle_closed` | debug | A connection idled past `idle_timeout` |
| `conn.read_failed`, `conn.write_failed` | info | A connection ended on a read or a write that failed |
| `conn.access_ended` | info | A connection was closed because its own access ended, during its handshake or by its own request |
| `conn.dropped` | info | An `aries admin` change closed the connections of whoever lost access, or a key reached its end (`reason="key expired"`, by `ariesd`); `connections` is how many |
| `conn.key_lookup_ignored` | debug | An `ariesd` from before the keys file asked about a key; it is not answered |
| `handshake.failed` | info | A handshake was refused or failed; `err` says why |
| `agent.harness_ignored` | info | A client named a harness ariesd does not know |
| `agent.register_failed` | error | A session could not be noted for `aries who` |
| `request.done` | debug | A request was answered |
| `request.refused` | info | A request was refused, with its `code`. Capped at `refusal_logs_per_minute` per principal |
| `request.failed` | error | A request failed on the server, and its client was answered `unavailable`. The line with the client's `req` has the cause |
| `request.part_failed` | error | A request was answered, without a part that could not be read: an issue's similar issues, its memories, a new member's keys |
| `request.commit_unknown` | error | A write's commit was sent to Postgres and its answer never came back (the daemon was stopping, the request ran out of time, or the connection to Postgres broke), so the write may stand. The request is not answered: its connection is closed, as a dropped one would be, so that the client never takes a write that may stand for a refusal, and resends it with its idempotency key |
| `project.added` | info | `aries admin project add` made a project |
| `project.opened`, `project.closed` | info | A project's store was opened, or closed (`reason`: `idle`, `to make room` or `archived`) |
| `project.open_failed` | error | A project's database could not be opened: at start, at a handshake, or for an `aries admin` change. Alert on it |
| `project.not_opened` | info | At start: an archived project's database awaits this ariesd's migrations and was left alone |
| `project.close_failed` | error | Closing a project's store failed |
| `store.schema_too_new` | error | A database was migrated by a newer ariesd |
| `store.unsafe_account` | warn | `--allow-unsafe-account` let a database role with too many rights through, on a developer's machine |
| `control.reload_failed` | error | The reaper could not reread the control store |
| `control.index_stale` | error | A control write failed and the reread after it failed too: what ariesd holds in memory of keys and members may be stale until a restart |
| `claim.expired` | info | A claim's lease ran out and the reaper ended it |
| `claim.released` | info | A claim ended because its holder lost access; `reason` says how |
| `lock.expired` | info | A lock's hold, or a place in its queue (`waiting=true`), ran out of lease and the reaper ended it, granting the next waiter |
| `lock.released` | info | A lock's hold or place ended because its holder lost access; `reason` says how |
| `gate.added` | info | A [gate](concepts.md#gates) was added; `by` added it |
| `gate.passed` | info | A gate was passed: by a person it names or an admin, or by a CI job's report of its check (`run`) |
| `gate.failed` | info | A CI job reported a ci gate's check failed, leaving the gate open or reopening it |
| `gate.waived` | info | An admin waived a gate |
| `gate.rearmed` | info | A passed or waived gate was reopened |
| `reaper.failed`, `reaper.prune_failed` | error | The reaper could not end expired claims, or prune, in a project |
| `reaper.pruned` | info | The reaper removed old registry rows or read inbox items, or keys that ended over an hour before (`keys`) |
| `access.sweep_failed` | error | The claims of holders without access could not be ended in a project |
| `keys_file.written` | info | The keys file was written |
| `keys_file.write_failed` | error | The keys file could not be written: sshd reads the old one. Every 30 seconds until a write succeeds. Alert on it |
| `keys_file.key_held_back` | warn | A key of the store is left out of the keys file because `authorized_keys` gives it to another principal |
| `keys_file.key_left_out` | error | A key's row in the store does not check out, and is left out of the keys file |
| `keys_file.removed` | info | SSH is off, so the keys file ariesd wrote was removed |
| `key.expired` | info | A key of the store reached its end (`aries admin key add --expires`): its line left the keys file, and the connections it signed in, `connections`, over SSH or HTTPS, were closed. Once per key |
| `https.listening` | info | The HTTPS listener serves: its address, TLS mode and URLs |
| `https.listener_stopped` | error | The HTTPS listener stopped on an error; nothing is served over HTTPS until a restart |
| `https.refused` | info | A connection to `/v1/connect` was refused before its hello (`reason`: a key the server does not know, a key past its end, a proof that does not verify, a disabled principal's key, a browser's origin, the limit on failures), sampled per address |
| `signin.granted` | info | A person signed in (`principal`, `provider`, `grant`, `flow`), or a service principal by a workload's token (`flow` `workload`, and `trust_rule`) |
| `signin.refused` | info | A step of a sign-in was refused (`reason`, `provider`): before the provider, sampled per address; after it, by the refusal's code |
| `signin.ended` | info | A sign-in ended before its end (`grant`, `reason`: `logout`, `revoked`, `identity_removed`, `trust_rule_changed`, `trust_rule_removed`), with the `connections` it closed; or the reaper closed the `connections` of sign-ins no longer live, which no request had closed (`reason`: `swept`, no `grant`). A disabled principal's sign-ins end with its connections, which `conn.dropped` counts |
| `signin.announce_failed` | error | A sign-in could not be announced in the inbox of one of its principal's projects (`principal`, `grant`, and `project` when it was one project's store), which could not be opened or written. The sign-in stands, and the audit stream has it |
| `signin.provider_failed` | warn | A provider's discovery document, keys or token endpoint could not be reached or read (`provider`, `err`), sampled per provider. Sign-ins at it fail until it answers; `aries admin status` shows the failure |
| `tls.cert_reloaded` | info | The certificate files changed and were read again |
| `tls.cert_reload_failed` | error | The certificate files changed and could not be read; the listener keeps the certificate it had. Alert on it |
| `clock.behind` | error | The clock is behind the latest event of the database named, at start and every ten minutes while it lasts, with the fix ([The server's clock](#the-servers-clock)) |
| `clock.caught_up` | info | A clock that was behind has passed its history |
| `clock.stepped` | warn | The clock was set, or the machine suspended: the direction, the size and what ariesd did about it |
| `clock.step_settled` | warn | For each database, a forward step was added to every live lease |
| `clock.settle_failed` | error | A step could not be settled in the control store |
| `database.check_failed` | error | A check of the database failed: the one for `/readyz`, or the one made before a connection's project store is opened, which is then not opened (`project`, `schema`) |
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
| `audit.project.add`, `audit.project.archive`, `audit.project.activate`, `audit.principal.add`, `audit.principal.disable`, `audit.principal.enable`, `audit.principal.set`, `audit.key.add`, `audit.key.remove`, `audit.member.set`, `audit.member.remove`, `audit.identity.invite`, `audit.identity.remove`, `audit.identity.bind`, `audit.signin.grant`, `audit.signin.end`, `audit.trust.add`, `audit.trust.set`, `audit.trust.remove` | info | [The audit stream](#the-audit-stream): one line for each access change |
| `audit.read_failed` | error | The audit stream could not read the control store's log. Its lines follow when a read succeeds; the event log has lost nothing |

Any failure to accept connections that will not pass stops ariesd: it
prints the error and exits 1, and the unit's `Restart=on-failure`
starts it again. An error that stops a start is printed as plain lines,
the cause and then each `fix:`, not as a log line.

### The audit stream

With `control_schema`, every change to who has access to what is an event
in the control store's log: a project added, archived or activated;
a principal added, disabled, enabled or set (its kind, owner or display
name); a key added or removed; a membership set or removed; an identity
invited or removed. ariesd also writes each as a log line with
`audit=true`, for a SIEM that reads the journal:

```text
level=INFO msg=audit event=audit.key.add audit=true seq=41 at=2026-10-12T09:30:00.123456Z principal=dana session=s-d op=key.add target=SHA256:fw8YS1jLRFPv61bKxSwx+1PRkYi+gXIIutriFmX8Bic fingerprint=SHA256:fw8YS1jLRFPv61bKxSwx+1PRkYi+gXIIutriFmX8Bic subject=carol
level=INFO msg=audit event=audit.member.set audit=true seq=42 at=2026-10-12T09:30:02.200100Z principal=dana session=s-d op=member.set target=0190a8a0-0000-7000-8000-00000000000a/carol project=0190a8a0-0000-7000-8000-00000000000a subject=carol role=member
```

| Field | Holds |
|---|---|
| `seq`, `at` | The event's number in the control store's log, and its time there. `seq` is what to deduplicate on, and what finds the event |
| `principal`, `session` | Who made the change |
| `op` | What it was: `project.add`, `key.remove` and so on |
| `target` | What it changed, as the event log names it: a project's UUID, a principal's name (an identity's events too), a key's fingerprint, or `PROJECT/PRINCIPAL` for a membership |
| `project`, `subject`, `fingerprint`, `role` | The target's parts, where it has them: the project, the principal it changed, the key's fingerprint, and the role a membership was set to |

- **Fingerprints, never keys.** No line holds a key's text or its
  comment.
- **No address and no display name.** An identity's events name its
  principal, its provider and its id, never the address invited or the
  subject a sign-in binds, and `principal.set` names the fields it
  set, never a display name's value: the log is kept for good, and
  they are personal.
- **The event log is the record; the lines are a copy.** ariesd reads
  the lines from the log, after the change has committed, so a line can
  say nothing the log does not, and there is a line for a change
  whoever made it: `ariesd control init` run beside the daemon shows
  at the reaper's next tick, within 30 seconds.
- **A line appears once, and only while ariesd runs.** The stream
  starts at the newest event there is when ariesd starts. What was
  written while it was stopped (by `ariesd control init`) is in the
  event log and not in the journal, and the journal is not a substitute
  for backing the control store up.
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
| Postgres's own figures in the status report | Always there, read with ariesd's own role ([Watching a server](#watching-a-server)) |
| Postgres's own metrics, for a scraper | Not ariesd's: run Postgres's own exporter, such as `postgres_exporter`, beside it. ariesd's pool is in its own metrics (`aries.db.pool.*`) |

Push and pull are independent: turn on either, both or neither. The
instruments are the same either way.

A collector's configuration, alert rules and a dashboard to start from
are in [deploy/observability](../deploy/observability):
[A collector, alert rules and a dashboard](#a-collector-alert-rules-and-a-dashboard).

### Names that are kept

From v0.5.0 on, and through every release after it, what ariesd reports
keeps its names, so that an alert, a dashboard or a log query written against
one release works against the next (decision 88):

- **Metrics:** each instrument's name, and its Prometheus name;
  each attribute's key, and every value the guide lists for it, such as
  the `limit` values, which name settings.
- **Traces:** each span's name, and each key a span carries and its
  values.
- **Logs:** each event's name, and each field's name, on stderr and
  over OTLP alike.

A name is never renamed or removed from v0.5.0 on, and a value never comes
to mean something else. What may change is only additive: a new
instrument, span, event or field, a new attribute on an instrument, or
a new value of an attribute, which a query that matches values it knows
is unaffected by. The examples in
[deploy/observability](../deploy/observability) follow the instruments
rather than bind them, and the text of `ariesd status` and of a log
line's message is for people and may change: scripts read `--json`, and
the fields.

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
- **It is bounded.** At most `telemetry.max_requests` requests are served at
  once ([Settings](#settings)); one more is answered `503` with
  `Retry-After: 1`. Twice that many connections may be open, and one more
  is closed. A request's headers must arrive within 5 seconds and its
  answer be read within 30.

`/readyz` is ready when all of these hold, and otherwise names the first
that does not:

| Line | Means |
|---|---|
| `ariesd is starting` | The store is not open yet, or the socket not yet listening |
| `ariesd is not listening on its socket` | The daemon is stopping, or could no longer accept connections |
| `the last check of the database failed` | Postgres did not answer a ping within 2 seconds: it hangs, is stopped or restarting, or cannot be reached. The ping is on a pooled connection; it is made at most every 5 seconds, however often `/readyz` is asked, and its answer stands until the next. ariesd's log has `database.check_failed` with the error. ariesd outlives a restart of Postgres ([step 5](#5-run-ariesd-under-systemd)), so this line clears by itself once Postgres answers |
| `the last write of the keys file failed` | With `control_schema`: sshd may be reading an older list of keys ([Keys from the store](#keys-from-the-store)) |

`/healthz` says only that the process answers. A daemon that is healthy
and not ready is usually waiting for Postgres.

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
| `aries.gates` | `aries_gates_total` | counter | `kind`, `event` |
| `aries.usage.tokens` | `aries_usage_tokens_total` | counter | `project`, `kind`, `model` |
| `aries.issue.cycle.duration` | `aries_issue_cycle_duration_seconds` | histogram, seconds | `project`, `type` |
| `aries.issues` | `aries_issues` | gauge | `project`, `status` |
| `aries.issues.closed` | `aries_issues_closed_total` | counter | `project`, `type`, `reason` |
| `aries.issues.reopened` | `aries_issues_reopened_total` | counter | `project`, `type`, `reason` |
| `aries.issue.queue.duration` | `aries_issue_queue_duration_seconds` | histogram, seconds | `project`, `type` |
| `aries.issue.landing.duration` | `aries_issue_landing_duration_seconds` | histogram, seconds | `project`, `type` |
| `aries.gate.wait.duration` | `aries_gate_wait_duration_seconds` | histogram, seconds | `project`, `kind` |
| `aries.claims.held` | `aries_claims_held` | gauge | `project` |
| `aries.issues.ready` | `aries_issues_ready` | gauge | `project` |
| `aries.control.writes` | `aries_control_writes_total` | counter | `op`, `result` |
| `aries.keys_file.writes` | `aries_keys_file_writes_total` | counter | `result` |
| `aries.keys_file.last_write.time` | `aries_keys_file_last_write_time_seconds` | gauge, Unix seconds | none |
| `aries.reaper.duration` | `aries_reaper_duration_seconds` | histogram, seconds | none |
| `aries.reaper.last_run.time` | `aries_reaper_last_run_time_seconds` | gauge, Unix seconds | none |
| `aries.clock.behind` | `aries_clock_behind_seconds` | gauge, seconds | none |
| `aries.clock.steps` | `aries_clock_steps_total` | counter | `direction` |
| `aries.telemetry.dropped` | `aries_telemetry_dropped_total` | counter | `signal` |
| `aries.db.pool.conns` | `aries_db_pool_conns` | gauge | none |
| `aries.db.pool.in_use` | `aries_db_pool_in_use` | gauge | none |
| `aries.db.pool.waits` | `aries_db_pool_waits_total` | counter | none |
| `aries.https.connections` | `aries_https_connections` | up-down counter (a gauge on `/metrics`) | none |
| `aries.https.connections.refused` | `aries_https_connections_refused_total` | counter | `reason` |
| `aries.https.upgrades` | `aries_https_upgrades_total` | counter | `result`, `reason` |
| `aries.https.key_proofs` | `aries_https_key_proofs_total` | counter | `result` |
| `aries.https.tokens` | `aries_https_tokens_total` | counter | `result` |
| `aries.https.tls_handshakes.failed` | `aries_https_tls_handshakes_failed_total` | counter | none |
| `aries.https.cert.reloads` | `aries_https_cert_reloads_total` | counter | `result` |

What each counts:

| Instrument | Counts |
|---|---|
| `request.duration` | Each request on a connection, from its frame being read to its answer being ready, refusals and rate-limited requests included |
| `project.requests` | The same requests, by project and by class |
| `connections` | Connections past the handshake |
| `handshakes` | Each handshake, by how it ended; those of `ariesd upgrade`'s health check and `ariesd status` among them. A connection refused before its handshake is not one: a socket past twice `conns_per_server`, or a peer that is not the daemon's user |
| `connections.dropped` | Connections ariesd closed for a reason of its own, not those a client closed |
| `limit.refusals` | Refusals at a limit that ariesd can name |
| `store.write.duration` | A write that changed a row and committed: the same measurement as P50 and P99 in the status report, the wait for the project's one writer and any retry included |
| `store.write.retries` | Each attempt of a write that lost to a concurrent transaction and was tried again |
| `stores.open` | Project stores open, opening or closing; always 1 on a server of one project |
| `store.open.duration` | With `control_schema`: opening a project's store, migrations included: once for each active project before the daemon listens, which is where an upgrade's migrations run, then whenever a connection or an archive opens one that was closed |
| `claims` | Changes of a claim, as the event log records them, and renewals |
| `locks` | Changes of a [lock](concepts.md#locks)'s holds and queue, as the event log records them, and renewals of a hold or a place. A lock's name is no attribute: it is a person's text |
| `gates` | Changes of a [gate](concepts.md#gates), as the event log records them: added, passed (by a person or a CI job's report), failed (a CI job's report), waived, rearmed. A report or a decision that changed nothing, such as a pass of a passed gate or a report of an older run, is not counted |
| `usage.tokens` | The tokens of each usage record the server stored, once: a batch sent again, or replayed under its idempotency key, adds nothing. Harnesses report usage through `aries usage --hook` (Claude Code today). Tokens only: cost is computed on read from prices that change, so it is no counter and stays with `aries cost`. The `model` is the name of a model with a price in the project's table, and `(other)` for every other, however many a client reports. A model priced later is counted by name from then on |
| `issue.cycle.duration` | Recorded once when an issue closes: the time from its first start (its first `claim.take` event) since it was last opened, to the close, on the server's clock, by the issue's `type` (`bug`, `feature`, `task`, `epic`, `chore` or `gate`). A close of an issue that was never started records nothing. An issue that is reopened and closed again records again, from its next start after the reopen, so a start before the reopen does not count. Every way an issue closes is one: a close, a finish, a gate's pass, a CI report. The buckets run from 1 minute to 14 days, not to 10 seconds like the others |
| `issue.queue.duration` | Recorded at an issue's first start (its first `claim.take` event, ever): the time from its creation, or its `defer_until` if that is later, to the start, on the server's clock, by the issue's `type`. A later take, and a start after a reopen, record nothing. The time the issue became ready, when its last blocker closed, is not used: ready is derived and no event records it, so an issue that waited on a blocker counts that wait too. The buckets run from 1 minute to 14 days |
| `issue.landing.duration` | Recorded when the closing pass of a required check closes an issue (the check `landed`, say): the time from its last handoff with state `done` to the close, on the server's clock, by the issue's `type`. It is the wait for review and merge. An issue closed any other way records nothing, and neither does one with no such handoff. Same buckets |
| `gate.wait.duration` | Recorded when a gate is passed or waived: the time from its add, or its last rearm, to the decision, on the server's clock, by the gate's `kind` (`human` or `ci`). A CI failure does not restart it; a rearm does. Same buckets |
| `issues` | Issues by `status` (`open`, `in_progress`, `blocked`, `deferred` or `closed`), counted when the metrics are collected: one grouped count of the project's `issues` table for each project whose store is open, which a scrape never opens, so a project with no connection lately has no series. It is counted rather than kept as a counter the writes adjust, because the table is the truth, so a count can drift from nothing and needs no rebuilding at open; the cost is one indexed scan of the status column for each open project at each scrape, which at the limits of a project is a few milliseconds. A count that fails or takes over 3 seconds leaves that project out of that collection |
| `issues.closed` | Each close that committed, once, by the issue's `type` and a `reason` from a fixed list: `landed` (the closing pass of the check `landed`), `forced` (a close that recorded `acceptance_overridden`, `checks_overridden` or `reviews_overridden`) and `other` (every other close). A close's own reason text is never an attribute, so a close that says "landed" in words is `other`. A gate's own issue, which a report or a rearm closes and reopens, is not counted |
| `issues.reopened` | Each reopen that committed, once, by `type` and a `reason`: `check_failed` (a revert: a failing report of the check after the closing pass) and `other` (every other reopen, a person's or a gate's rearm). Revert rate is `check_failed` reopens over `landed` closes; the work dashboard shows both with their counts |
| `lock.wait.duration` | Each grant to a place in a lock's queue: how long the place waited, on the server's time. The buckets end at 10 seconds, so most waits for a deploy or a merge fall past the last |
| `control.writes` | With `control_schema`: each `aries admin` change, refused ones included |
| `keys_file.writes` | With `control_schema`: each time the keys file was written, or could not be. A file that already held the right keys is not a write |
| `keys_file.last_write.time` | When the keys file took its current content |
| `reaper.duration`, `reaper.last_run.time` | Each 30-second tick that ends expired claims, closes idle stores and rereads the control store, and when the last one ended. A time that stops advancing means the reaper is stuck |
| `clock.behind` | How far the server's clock is behind the history its time is held against, as of the reaper's last tick; 0 when it is not behind. Above 0 the daemon serves, with its time running on from the history at half speed, and with `clock_behind: stop` it will not start again: alert on it ([The server's clock](#the-servers-clock)) |
| `clock.steps` | Each time the daemon found the clock set, or the machine suspended and resumed, by more than `clock_step_tolerance`, counted at the reaper's next tick |
| `telemetry.dropped` | For `signal="metrics"`, each OTLP export that failed; nothing is lost for good, since totals are cumulative and the next export carries them. For `signal="traces"`, each span that was lost: one the full queue had no room for, or one of an export that failed. For `signal="logs"`, each log record that was not exported, the same way; the journal still has its line ([When the collector is down](#when-the-collector-is-down)) |
| `db.pool.conns`, `db.pool.in_use` | The connections to Postgres ariesd's one pool holds, which every project's store shares, and those a request is using now; at most `db_conns_per_server`. The connection that holds the writers' locks is not counted |
| `db.pool.waits` | Each time a request waited for a connection of the pool because every one was in use. A rate that stays above zero is a pool too small for the load: raise `db_conns_per_server`, and Postgres's `max_connections` with it |
| `https.connections` | With HTTPS on: the TCP connections the [HTTPS listener](#the-https-listener) holds, at most `https_conns_per_server`: those in their TLS handshake, plain requests and WebSockets. A WebSocket past its hello is also one of `connections` |
| `https.connections.refused` | Each connection the listener closed unanswered: `untrusted`, from an address `trusted` does not name behind a proxy; `limit`, past `https_conns_per_server` or `https_conns_per_addr`, which `limit.refusals` names |
| `https.upgrades` | Each request to `/v1/connect`: upgraded to a WebSocket, or refused before, and why. An address past `auth_failures_per_minute` is also counted in `limit.refusals` |
| `https.key_proofs` | Each WebSocket's [key proof](#the-https-listener): accepted, refused and why, or `failed` when the client left, or did not finish within `upgrade_timeout`. A refused proof is counted before the refusal is written. What follows an accepted one is a handshake, counted in `handshakes` |
| `https.tokens` | Each [sign-in](#sign-in)'s token presented to connect, with its DPoP proof: accepted, or refused and why; a refused one is also an upgrade refused with reason `token`. Counted before the refusal or the upgrade is answered. A connection that brought a token proves no key |
| `https.tls_handshakes.failed` | With TLS at ariesd: each connection closed before its TLS handshake completed, whatever the cause: a client that does not trust the certificate, a scanner, plain HTTP sent to the port, a load balancer's TCP check. A steady rate is the internet's noise; a jump with complaints from clients is a certificate they do not trust |
| `https.cert.reloads` | With `tls: mode: files`: each time the certificate's files changed and were read again, or could not be, once for each change. ACME's renewals are not counted |

Every attribute's values come from a fixed list in the code, so the
number of series cannot grow with what clients send:

| Attribute | Values |
|---|---|
| `op` | An operation of the protocol, such as `create`, `start` or `member.set`; `unknown` for any other |
| `result` | `ok`, or the refusal's code: `not_found`, `conflict`, `exists`, `cycle`, `invalid`, `acceptance`, `forbidden`, `unavailable`, `version`, `auth`, `busy`, `stale`. For a handshake also `failed`: it ended without a refusal to name, as when a frame could not be read. For the keys file and the certificate's reloads: `ok` or `failed` |
| `result` of an upgrade | `ok`, or `refused` |
| `reason` of an upgrade | `none` for one upgraded; for one refused, `origin` (a browser's page, 403), `auth_failures` (the address past `auth_failures_per_minute`, 429), `token` (a sign-in's token or its DPoP proof refused, 401, or 503 for `dpop_busy`) or `bad_request` (not a WebSocket upgrade, 400, or one that could not be completed) |
| `result` of a key proof | `ok`; `invalid` (no key offer, or a malformed proof), `url` (a proof for a URL `public_urls` does not name), `unknown_key` (no key offered is known), `key_ended` (a key offered is the server's and past its end, proved), `bad_proof` (the signature does not verify), `disabled` (a disabled principal's key, proved); `failed` (the client left, or said nothing within `upgrade_timeout`) |
| `result` of a token | `ok`; the token: `token_scheme` (not presented as DPoP), `token_missing`, `token_unknown` (never issued, or its sign-in ended and was pruned), `token_ended`, `token_bearer` (bound to no key), `token_no_login` (the server has no `login:`); its DPoP proof: `dpop_missing`, `dpop_malformed`, `dpop_signature` (not signed by the key it carries), `dpop_request` (for another method or URL), `dpop_token` (for another token), `dpop_time` (too far from the server's time), `dpop_replayed`, `dpop_busy` (past `dpop_proofs_per_server`), `dpop_key` (signed by another key than the token's) |
| `reason` of a connection the HTTPS listener refused | `untrusted`, `limit` |
| `result_class` | `ok`; `error` for `unavailable`, which is the server's failure; `refused` for every other refusal, which is the request's |
| `project` | The project's UUID, only for a project a connection was admitted to. At most as many as the `projects_per_server` limit, and `other` past that |
| `reason` of a dropped connection | `membership`, `disabled`, `archived`, `key` (the access that let it in ended), `expired` (the key that signed it in reached its end), `idle` (`idle_timeout`), `limit` (a socket past twice `conns_per_server`), `access` (the access ended while the handshake ran), `signin` (its sign-in ended: at its end, a logout, or an admin's word) |
| `limit` | `conns_per_server`, `conns_per_principal`, `handshakes_per_principal`, `handshake_sockets` (twice `conns_per_server`), `writes_per_second`, `control_writes_per_second`, `open_stores_per_server`, `telemetry.max_requests`, `projects_per_server`, `principals_per_server`, `keys_per_principal`, `members_per_project`, `identities_per_principal`, `invites_per_server`, `prices_per_project`, `plans_per_project`, `hours_per_day`, `usage_per_day`, `locks_per_project`, `waiters_per_lock`, `lock_waits_per_principal`; with HTTPS on, `https_conns_per_server`, `https_conns_per_addr`, `auth_failures_per_minute`; with `login:` providers, `grants_per_principal`, `signins_pending_per_server`, `signins_pending_per_addr`, `dpop_proofs_per_server`; with `workload:` providers, `trust_rules_per_server`, `trust_rules_per_principal`, `grants_per_trust_rule`, `workload_tokens_per_server` |
| `migrated` | `true` when opening the store applied a migration |
| `event` of a claim | `take`, `renew`, `release`, `expire` |
| `event` of a lock | `acquire`, `wait`, `grant`, `renew`, `release`, `expire`, `leave` |
| `kind` of a gate | `human`, `ci` |
| `event` of a gate | `added`, `passed`, `failed`, `waived`, `rearmed` |
| `kind` of a token | `input`, `output`, `cache_write`, `cache_write_1h`, `cache_read` (`cache_write` is the part written for five minutes and `cache_write_1h` the part written for an hour, which are priced apart) |
| `model` | The name of a model the project's price table has a price for, at most 64 of them; `(other)` for any other model, a client's text |
| `reason` of a claim | `membership`, `disabled` or `archived` for a release that ended with the holder's access; `none` for every other change |
| `signal` | `metrics`, `traces`, `logs` |
| `direction` | `forward`, `backward` |

- **Nothing a client chooses is an attribute:** no principal, session,
  machine or issue id, no project the server does not serve, and over
  HTTPS no address, URL, header or key.
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
its own; it is never refused for it. Postgres is not traced: a trace
ends at ariesd's span around the work it asked of Postgres.

| Span | Kind | Covers | Attributes |
|---|---|---|---|
| A request's, named for its op: `create`, `show` and so on, and `unknown` for an op the server does not know | server | One request, from its frame being read to its answer being ready: the stretch `aries.request.duration` measures | `op`, `result`, `project`, `protocol`, `principal`, `session` |
| `handshake` | server | A connection's handshake, from its first frame to its welcome, with the opening of the project's store when this connection caused it. The note of its session in the registry is beneath it and may end after it: it is written beside the wait for the first request. The root of a trace of its own, since a hello brings no trace context | `result`, `project`, `protocol`, `principal`, `session` |
| `store.write.attempt` | internal | One attempt of a write: the wait for the writer, the SQL transaction and its commit. A write that loses to a concurrent transaction is attempted again, in a new span, so a retried write shows as several | `project`, `attempt`, `outcome` |
| `store.write.wait` | internal | Within an attempt: the wait for the project's one writer connection, and the start of its transaction | `project`, `outcome` |
| `store.open` | internal | With `control_schema`: opening a project's store, as `aries.store.open.duration` measures it | `project`, `migrated` |
| `store.migrate` | internal | Within the opening of a store, a project's or the control store's: the check of its schema, and the migrations applied | `project`, `outcome` |
| `reaper` | internal | One 30-second tick, with the four below and the store's writes beneath it, so a slow tick shows which part was slow | none |
| `clock.check` | internal | The look at the server's clock | none |
| `control.reload` | internal | With `control_schema`: the reread of the control store's index | none |
| `keys_file.write` | internal | With `control_schema`: the check of the keys file, and its write when it was out of date | `result`, `changed` |
| `access.sweep` | internal | With `control_schema`: ending the claims of holders who lost their access, in every open store, or in one as it opens | `project`, when it sweeps one store |

- **The last four are also roots or children elsewhere.** Each is the
  root of a trace when it runs outside a tick, as the keys file's write
  does when ariesd starts; the keys file's write after an `aries admin`
  change is beneath that request's span.
- **A span of the control store's store names no project.**
- **A span is marked failed only for the server's own failures:** a
  request answered `unavailable`; a handshake that ended `failed` or
  `unavailable`; a migration, a wait for the writer, an
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
| `parentbased_always_on` (default) | What the caller decided, when the request brought a trace context; every trace that starts in ariesd: a request with no trace context, a handshake, the reaper's tick |
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
| A handshake | 3; with `control_schema`, 12 when it is the one that opens the project's store |
| The reaper's tick, every 30 seconds | 4 on a server of one project; with `control_schema`, 5 and 4 for each open store. 2 more for each store every 10 minutes, when it prunes |

A tracker's traffic is small, which is why the default records
everything: ten requests a second, a third of them writes, is about 17
spans a second. An idle server of 100 open projects sends about 400
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
| `err`, `detail`, `cause`, `fix`, `effect`, `target`, `machine`, `harness`, `name`, `schema`, `path`, `url` and every other field | **Nothing.** An error's text can quote what it refused or name a person, so it stays in the journal, under every setting |

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
real ariesd, Postgres, OpenTelemetry Collector and Prometheus. Nothing in
them names a vendor.

| File | Is |
|---|---|
| `collector.yaml` | An OpenTelemetry Collector's configuration: it receives what ariesd pushes on `127.0.0.1:4318` (metrics, traces and, when asked, log records), scrapes ariesd's `/metrics`, and sends all of it on |
| `alerts.yaml` | Alert rules in Prometheus's rule format, which most monitoring systems read or translate |
| `prometheus.yaml` | A Prometheus configuration that scrapes ariesd and loads the rules, for a server that runs Prometheus and no collector |
| `dashboard.json` | The server's dashboard, in Grafana's format: headline numbers, then requests, latency, writes, connections, claims, the server's own health, ariesd's memory, and its pool of connections to Postgres |
| `dashboard-requests.json` | Requests by op: traffic, latency, refusals and failures by code, by project, handshakes, and write latency by project |
| `dashboard-work.json` | The work: issues in progress, open and blocked by project, closes a day, the mean and 50th and 90th percentile of start to close by type over the dashboard's window, and tokens by model, which read `(other)` until the server has prices |
| `dashboard-traces-logs.json` | Traces and logs, for a Grafana with Tempo and Loki: recent and slow traces, log events by name, warnings and errors, and the audit stream |
| `grafana-dashboards.yaml` | Grafana's provisioning of the three, as the Docker test server's `observe` profile loads them |

**Wire a collector.** It runs on the server itself: ariesd's listener
answers on loopback only and has no authentication, and ariesd pushes to
loopback with no credential.

1. Install the OpenTelemetry Collector as its own documentation says.
   The core distribution has every component the file uses; it was
   tested at 0.162.0.
2. Turn on what it reads, if it is not on already:
   `telemetry: listen: 127.0.0.1:9464` in ariesd's config
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
   Postgres's own metrics come from its own exporter, if you run one:
   add a scrape of it beside ariesd's.
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
rules select by the scrape job `ariesd`, so keep that name or change
both. Their thresholds are starting points to set
against your server's own history.

| Alert | Fires when |
|---|---|
| `AriesdDown` | ariesd's endpoint has not answered a scrape for 2 minutes |
| `AriesReaperStalled` | The reaper has not finished a tick for 2 minutes: expired claims are not ended, and systemd's watchdog is about to restart ariesd |
| `AriesKeysFileWriteFailing` | A write of the keys file failed: sshd reads an older list of keys ([Keys from the store](#keys-from-the-store)) |
| `AriesTLSCertReloadFailing` | The HTTPS listener's certificate files changed and could not be read: it keeps serving the certificate it had, which still ends on its date ([The HTTPS listener](#the-https-listener)) |
| `AriesClockBehind`, `AriesClockStepped` | The clock is behind Aries's own history, or was set ([The server's clock](#the-servers-clock)) |
| `AriesProjectUnavailable`, `AriesRequestsFailing` | Handshakes or requests are answered `unavailable`: the server's failure, most often a database that cannot be reached |
| `AriesWriteLatencyHigh`, `AriesWritesQueueing` | One write in a hundred takes over a second; more than one in five is retried |
| `AriesWritesRateLimited`, `AriesConnectionCapReached`, `AriesStoreCapReached` | Requests are refused at `writes_per_second`, at a connection limit (`https_conns_per_server` among them), or at `open_stores_per_server` ([Limits](#limits)) |
| `AriesTelemetryMetricsExportFailing`, `AriesTelemetrySpansDropped`, `AriesTelemetryLogRecordsDropped` | ariesd counts failed pushes of metrics, spans it lost, or log records it lost from the export, whose lines the journal still has ([When the collector is down](#when-the-collector-is-down)) |
| `AriesDatabasePoolExhausted` | Requests waited for a connection of ariesd's pool, every one in use, for 10 minutes: raise `db_conns_per_server`, with Postgres's `max_connections` above it ([Sizing Postgres](#sizing-postgres)) |

- **No rule asks `/readyz`,** because no metric says what it answers.
  Probe `http://127.0.0.1:9464/readyz` from the machine with whatever
  probes HTTP for you. `AriesdDown`, `AriesReaperStalled` and
  `AriesKeysFileWriteFailing` cover the same ground from the metrics.
- **No rule watches Postgres itself**: its connections, deadlocks, disk
  and replication are its own exporter's, and in `aries admin status`.
- **No rule matches a log line's `event`.** These are rules over
  metrics, and nothing in the examples turns a log record into one.
  Match [`event`](#events) where your logs are read. Four of the events
  worth an alert have a rule here from the metric that counts the same
  thing: `keys_file.write_failed`, `tls.cert_reload_failed`
  (`AriesTLSCertReloadFailing`), `project.open_failed` at a handshake
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

**The dashboards** are imported into Grafana as they are and ask which
data source to read: Prometheus for the first two, Tempo and Loki for
the third. No test loads them into Grafana: tests
check their Prometheus queries, as they do the rules', and the test server runs each
query against a real Prometheus.

**What was run.** All of it in the Docker test server, on Docker
Desktop on a Mac (arm64), on 12 October 2026: the Collector at 0.162.0
and Prometheus at 3.15.0, with every rule loaded and evaluated, every
query of the dashboard run, and ariesd's metrics, traces and log
records received, the audit stream among them; and again on Postgres,
on 15 October 2026, with the same results, 16 rules loaded and every
query of the dashboard run. The `filter/audit`
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
  with a `fix:`. So does root where the bridge's
  [AppArmor profile](#confine-ariesd-with-apparmor) is loaded, with SSH
  on: run it with `sudo -u aries -H`, as above. It needs no `aa-exec`.
- It must be the same build as the daemon, since it speaks the daemon's
  protocol: one of another protocol version is refused with `version`.
- It connects under the reserved principal `ariesd-status`, which no
  key can sign in as. The connection is in no project, is not listed by
  `aries who`, and is answered nothing but the report.
- On a server of one project it lists that project by its UUID, since
  only a control store gives projects names, and counts no projects
  or principals.
- It answers in about 2 seconds when Postgres does not answer at all, with
  each open project's sessions as `unknown`
  ([When Postgres does not answer](#when-postgres-does-not-answer)), whichever
  stores are open: `aries admin status` is as prompt only from a
  repository whose project's store is open, and is refused from any
  other. It waits 30
  seconds for the daemon and no longer: `the daemon did not answer`
  then means ariesd itself is stuck, which its log explains.

## Upgrades

**From v0.4.x or earlier, there is no upgrade in place.** v0.5.0 has
new names for every binary, file, variable and path, a new release
key, and a new database, and its server refuses every earlier client.
A v0.4.x `ariesd upgrade` cannot find or verify it. Set up a new server
and remove the former one, as [Upgrading to 0.5](upgrade-to-0.5.md)
says. What follows is for upgrades between releases from v0.5.0 on.

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

**Take a dump first.** A release may migrate the database when the new
ariesd starts, and migrations only go forward: the old binary refuses a
schema the new one migrated. A migration runs in one transaction with
the row that records it, so one that fails leaves its schema as it was;
but a migration that succeeds and a release you then want to leave can
only be undone from a backup. So before each upgrade, run the daily dump
once more, and keep its file apart from the rotation
([Backups and restore](#backups-and-restore)):

```sh
systemctl start aries-backup.service
cp /var/backups/aries/aries-"$(date -u +%a)".dump /var/backups/aries/before-upgrade.dump
```

Then, on the server, as the `aries` user:

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
   check of step 4 and be taken for a bad release. `upgrade --check`
   prints the same finding on a line starting `clock:`.
3. Swaps the binary.
4. With `systemd_unit:` set, or `--restart`, restarts the unit through the
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

With `control_schema`, ariesd opens the schema of every active project
once before it listens, which applies the new version's migrations to
each (a few milliseconds for a schema already current), and the check
answers for all of them: it does not depend on the config file's
`project` being active. If a project's schema cannot be opened, the
check is refused and names the project, so upgrade puts the old binary
back; the log says why. An archived project's schema is migrated when
the project is activated, and not at start: ariesd opens it then only
as it is, to end a claim left in it
([Managing access](#managing-access)).

The same happens at every start. A project whose schema cannot be
opened does not stop the others from being served: it is logged at
`error`, its connections are refused with `unavailable`, and each new
connection tries it again. Fix its schema, or archive the project.
As on a server of one project, the old binary refuses a schema the new
one has already migrated, since migrations only go forward. So after an
upgrade that failed partway, the restored version may not serve the
schemas the new one reached. To go back all the way, stop ariesd,
restore the dump you took before the upgrade
([Restore](#restore)), and run `ariesd upgrade --rollback --restart`.

**Keys and the upgrade.** sshd runs nothing of Aries's to find a key
([Keys from the store](#keys-from-the-store)), so an upgrade has no
copy of the binary to refresh for it. The new ariesd writes the keys
file again as it starts.

Where the [AppArmor profiles](#confine-ariesd-with-apparmor) are
installed, run each of these through `aa-exec -p ariesd-admin --`.

It refuses to run as root, except with `--check`. `--to vX.Y.Z` installs a given release, but
never an older one, because migrations only go forward. `--rollback`
restores the binary the last upgrade replaced; it does not touch the
database, so a release that migrated it needs the dump as well.

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

**Postgres's own upgrades** are its packages': `apt-get upgrade` brings
the newest release of its major version and restarts the cluster, which
ariesd outlives ([step 5](#5-run-ariesd-under-systemd)). A new major
version is `pg_upgradecluster`, with ariesd stopped and after a dump; any
release of 16 or newer serves. Each Aries release's notes name the
Postgres it was tested with.

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
| The database | Every project's issues, comments and events, and, with `control_schema`, the control store's projects, principals, keys and memberships | A daily dump of the whole database, below, carried off the machine |
| `/etc/aries/ariesd.yaml`, and `/etc/aries/postgres` where it exists | The project UUID; with a Postgres on another machine, the role's password | With your other secrets |
| `~aries/.ssh/authorized_keys` and `/etc/ssh/ssh_host_ed25519_key*` | Who may connect by a line of their own, and the host key every `.aries.yaml` pins | With your other secrets |

### The daily dump

`pg_dump` writes the database's rows as they stand at one moment: it
reads every schema in one transaction, so the control store and every
project agree with each other. It runs while ariesd serves, and holds
no lock that stops a write; a migration, which changes tables, waits
for it. Its custom format is compressed, and `pg_restore` takes from it
the whole database or one schema.

Save this as `/etc/systemd/system/aries-backup.service`:

```ini
[Unit]
Description=Dump the Aries database
After=postgresql.service

[Service]
Type=oneshot
User=postgres
Group=postgres
UMask=0077
ExecStart=/bin/sh -c 'exec pg_dump --format=custom --file="/var/backups/aries/aries-$$(date -u +%%a).dump" aries'
```

and this as `/etc/systemd/system/aries-backup.timer`:

```ini
[Unit]
Description=Dump the Aries database every day

[Timer]
OnCalendar=*-*-* 03:15:00 UTC
Persistent=true

[Install]
WantedBy=timers.target
```

```sh
install -d -o postgres -g postgres -m 0700 /var/backups/aries
systemctl daemon-reload
systemctl enable --now aries-backup.timer
systemctl start aries-backup.service
ls -l /var/backups/aries
```

- **It keeps a week**, one file a weekday, each written over seven days
  later. `$$` and `%%` are systemd's way of writing `$` and `%`.
- **It runs as `postgres`**, the superuser, by the unix socket with no
  password, and reads every schema whoever owns it. The files are
  `postgres`'s alone, mode 0600.
- **`Persistent=true`** runs a dump the machine slept or was off
  through, when it is next up.
- **Copy the files off the machine every day**: to object storage with
  versioning and a retention the machine cannot shorten, or with your
  backup tool. A dump on the server's own disk goes with it.
- **One project alone**, to hand to another server or keep apart:
  `sudo -u postgres pg_dump --format=custom --schema=project_web
  --file=web.dump aries`. A project's schema holds its issues and their
  history, not who may reach it: that is the control store's.
- **A cron line does the same**, if you prefer cron: `15 3 * * *
  postgres pg_dump --format=custom
  --file=/var/backups/aries/aries-$(date -u +\%a).dump aries` in
  `/etc/cron.d/aries-backup`.
- **With Postgres on another machine**, dump from the server with the
  client programs the installer installs, as ariesd's role, which owns
  everything in its database: `cd /var/lib/aries && sudo -u aries -H
  pg_dump --format=custom --file=aries.dump "$DSN"`, where `DSN` is the
  config file's. A managed service also keeps its own snapshots
  ([Managed services](#managed-services)).

### Restore

A restore replaces the database with the dump's rows and loses what
was written since. Stop ariesd first, so that nothing writes meanwhile.

**The whole database:**

```sh
systemctl stop ariesd
cd /
sudo -u postgres dropdb aries
sudo -u postgres pg_restore --create --dbname postgres /var/backups/aries/aries-Mon.dump
systemctl start ariesd
sudo -u aries -H ariesd status
```

`--create` makes the database again as it was dumped: its owner, its
encoding and collation `C`, and the `REVOKE` of step 2. Every schema in
it comes back owned by `aries`.

**One project**, from the whole dump or from one of its own; the other
projects lose nothing:

```sh
systemctl stop ariesd
cd /
sudo -u postgres psql -d aries -c 'DROP SCHEMA project_web CASCADE' -c 'CREATE SCHEMA project_web AUTHORIZATION aries'
sudo -u postgres pg_restore --dbname aries --schema project_web /var/backups/aries/aries-Mon.dump
systemctl start ariesd
```

`pg_restore --schema` restores what is in the schema and not the schema
itself, which the second statement makes again, owned by `aries`.
Without it, every statement of the restore fails on `schema
"project_web" does not exist`.

- **After a restore ariesd compares the clock with the restored
  history**, like at every start. A dump from this server's own past is
  behind the clock and starts as usual. One written by a machine whose
  clock ran ahead is refused, with the flag that starts it once
  ([When the clock is behind the server's
  history](#when-the-clock-is-behind-the-servers-history)).
- **After restoring the control store,** access is as it was then: a
  key removed or a person offboarded since is back. Check `aries admin
  principal list` and redo what was undone.
- **On a new machine**, run the installer, or steps 1 to 6, then put
  back the config file's `project`, the host keys and `authorized_keys`,
  and restore the whole database as above. If the host key changed,
  everyone must update `host_key`: `aries` refuses the new key until
  they do.

What was run, on 15 October 2026: these commands as written, on Ubuntu
24.04 booted by systemd in a container, set up by the installer, with
two projects. `systemd-analyze verify` accepted the service and the
timer; the timer was scheduled; the service wrote the day's dump, mode
0600. A restore of the whole database brought back what was written
before the dump and nothing after, with the database's owner,
collation `C` and `REVOKE` as they were, and `ariesd status` and `aries
admin status` reported no fault. A restore of `web` alone from the
whole dump brought `web` back to the dump and left `first` as it was.
The same, from dumps of the whole database and of one schema, was run
in the [Docker test server](deploy/docker.md). Not run: a restore on a
new machine, and one on a confined server.

### Point-in-time recovery

A daily dump can lose up to a day of writes. For less, Postgres archives
its write-ahead log continuously and restores to any moment: a base
backup, every WAL segment since, and recovery to a time you name
(`archive_mode`, `archive_command` or `archive_library`, and
`recovery_target_time`, in Postgres's own documentation, "Continuous
Archiving and Point-in-Time Recovery"). pgBackRest, Barman and WAL-G
run it for you, with retention and checks. A managed service does it
itself, within a window it calls point-in-time restore.

- **It restores the whole cluster**, every database in it, to the same
  moment. Where ariesd's database shares the cluster with others,
  recover into a cluster of its own and `pg_dump` ariesd's database
  from there.
- **Keep the dumps too.** They are what restores one project alone, and
  they are readable by any Postgres of the same major version or newer.
- Not run for this guide, with any tool.

## Storage growth

The event log is never pruned. It is the history. An event keeps a text
over 8 KiB as its first 512 bytes, its length and its SHA-256, so an
edit loop over long fields grows the log by about a kilobyte a write,
not by the text. The write rate limit bounds how fast one principal can
grow it.

Token usage is one row per harness request in `token_usage`, about 200
bytes each, also never pruned; a `usage` call adds one summary event, not
one per row. `usage_per_day` bounds how fast one principal can grow it.
Prices are at most `prices_per_project` rows and plans at most `plans_per_project` rows, and
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

**Watch the database's size.** `aries admin status` gives the
database's and each project's schema's (`postgres's database` and
`SIZE`); on the machine, `sudo -u postgres psql -c '\l+ aries'` and
`df -h /var/lib/postgresql`. A new project's schema took 792 KiB in the
Docker test server. Postgres reuses the space of rows it no longer
needs by itself (autovacuum, on by default), and Aries deletes little,
so a schema grows with what is written to it and shrinks only by
`VACUUM FULL`, which locks its tables: run it, if ever, with ariesd
stopped.

## Postgres

### Sizing Postgres

- **Connections.** ariesd holds at most `db_conns_per_server` (32)
  connections in its pool, and one more for its writers' locks
  ([One writer for each project](#one-writer-for-each-project)). An
  operator's command that opens the store (`control init`, `import-bd`,
  `export-bd`, the clock check of `ariesd upgrade`) holds a few of its
  own while it runs, and so does each `pg_dump` and `psql`. Postgres
  allows 100 by default (`max_connections`), 3 of them kept for
  superusers, which leaves room. The installer warns when there is not
  room for ariesd's 33 and ten more. A Postgres that two ariesd
  processes share, each with its control schema of its own, needs room
  for both. `aries admin status` shows the server's connections against
  `max_connections`, and how often a request waited for one of the
  pool's; the alert `AriesDatabasePoolExhausted` fires on that wait.
  Raising `max_connections` costs memory for each connection that is
  used, so raise `db_conns_per_server` only when requests wait.
- **Memory.** Measured on a development machine (Apple M4 Max, macOS,
  Postgres 18.6, 15 October 2026; design §14, the As built note of
  slice P2), Postgres's processes together took about 50 MiB idle, about
  2.5 MiB more for each project in use up to 20, and 0.6 MiB more for
  each after: 108 MiB at 20 projects and 228 MiB at 200. The most it
  took was 949 MiB, with every connection of the pool having worked in
  each of 200 projects: a connection keeps its caches of every schema it
  has touched, until the pool retires it, idle for 30 minutes or an hour
  old. Not measured on Linux.
- **Settings.** The package's defaults serve a team: `shared_buffers`
  of 128 MB, `max_connections` of 100, `fsync` and `synchronous_commit`
  on. On a machine of its own, Postgres's documentation suggests
  `shared_buffers` at a quarter of the memory; Aries has measured no
  other setting. Keep `fsync` on: a write ariesd acknowledged is then on
  disk.
- **Disk.** A new project's schema is under a megabyte, and grows with
  what is written ([Storage growth](#storage-growth)). Put
  `/var/lib/postgresql` on a disk of its own where you can, so that the
  system's disk can be rebuilt without it.

### Managed services

A Postgres 16 or newer that a cloud runs, such as Amazon RDS or Aurora,
Google Cloud SQL or AlloyDB, or Azure Database for PostgreSQL, serves
as well as one on the machine: ariesd uses nothing but Postgres's
protocol and SQL. Give the installer its URL (`--dsn`), or follow the
steps with the changes below. None of these services was run for this
guide; the steps were run against a Postgres container that ariesd
reached over the network with a password.

1. **The service's administrator makes the role and the database.**
   The administrator a service gives is not a superuser: it has
   `CREATEROLE` and `CREATEDB`, and since Postgres 16 it must hold a
   role to make a database that role owns. In `psql`, as that
   administrator:

   ```sql
   CREATE ROLE aries LOGIN;
   \password aries
   GRANT aries TO CURRENT_USER;
   CREATE DATABASE aries OWNER aries TEMPLATE template0 ENCODING 'UTF8' LOCALE_PROVIDER libc LOCALE 'C';
   REVOKE ALL ON DATABASE aries FROM PUBLIC;
   REVOKE aries FROM CURRENT_USER;
   ```

   `\password` asks for the password and sends it hashed, so it is in no
   statement, history or log. Without the `GRANT`, `CREATE DATABASE`
   is refused with `must be able to SET ROLE "aries"` (run on Postgres 18
   as an administrator with `CREATEROLE` and `CREATEDB`). A service
   that refuses `TEMPLATE template0` or `LOCALE 'C'` makes the database
   with its own defaults; ariesd works on it, and orders lists by its
   collation.
2. **The password, in a file.** Put the password, alone on one line,
   in a file only root can read, and give it to the installer with
   `--db-password-file`. It writes `/etc/aries/postgres/pgpass`, mode
   0600, owned by `aries`, in the form Postgres reads (`*:*:*:*:` and the
   password, with `\` and `:` escaped), and adds `passfile=` to the DSN.
   ariesd reads the file when it opens its database, at start: after the
   password changes, change the file and restart ariesd. It refuses to
   start while the file is open to group or others: `chmod 600` it. A service's
   sign-in by a token that expires (an IAM database authentication) is
   not supported.
3. **TLS to the database.** Ask for it, and check the server's name:
   `sslmode=verify-full`, with `sslrootcert=` naming the certificate
   authority the service publishes, saved in `/etc/aries/postgres`, or
   `sslrootcert=system` where the machine's own authorities vouch for
   it. The AppArmor profiles read both
   ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)).

   ```yaml
   dsn: postgres://aries@db.example.com:5432/aries?sslmode=verify-full&sslrootcert=/etc/aries/postgres/root.crt&passfile=/etc/aries/postgres/pgpass
   ```

4. **No `CREATE` on the database: a schema made beforehand.** Where the
   role may not have a database of its own, the database's owner makes
   the control store's schema and the first project's, owned by the
   role:

   ```sql
   GRANT aries TO CURRENT_USER;
   CREATE SCHEMA aries_control AUTHORIZATION aries;
   CREATE SCHEMA project_first AUTHORIZATION aries;
   REVOKE aries FROM CURRENT_USER;
   GRANT CONNECT ON DATABASE app TO aries;
   ```

   The DSN then names the first with `search_path=project_first`, and
   `control init --name first` uses both schemas as they are.
   `aries admin project add` is refused, with the `GRANT CREATE ON
   DATABASE` that would let it make the new project's schema: ariesd
   never takes over a schema it did not make
   ([Opt in](#opt-in), step 3), so such a server serves its first
   project until the role may make schemas. Run on Postgres 18 with an administrator that had
   `CREATEROLE` and `CREATEDB`, on 15 October 2026: `control init`
   made the control store's tables in `aries_control` and adopted
   `project_first`, and the role's own `CREATE SCHEMA` was refused with
   `permission denied for database app`. `aries admin project add` was
   not run on it.
5. **Connect directly, or through a pooler in session mode.** ariesd's
   writers' locks are held by one connection for as long as it lives,
   and each pooled connection keeps the `search_path` ariesd sets on
   it; a pooler in transaction mode (PgBouncer's, or a service's proxy
   set so) hands them to other clients. Not run.
6. **Connections** are capped by the instance's size on most services:
   leave room for ariesd's 33 ([Sizing Postgres](#sizing-postgres)).
7. **Backups** are the service's snapshots and point-in-time restore,
   and the daily dump from the server ([The daily dump](#the-daily-dump)).

### Standby and failover

Postgres keeps a standby in step by streaming replication: the standby
replays the primary's write-ahead log, and holds the whole cluster,
every schema, role and grant, byte for byte. Promoting it after the
primary is lost is a step that core Postgres leaves to something
outside it.

- **Who promotes.** Patroni (with etcd or Consul) or pg_auto_failover
  on machines of your own; a managed service's high availability
  (RDS Multi-AZ, Cloud SQL's, AlloyDB's, Azure's zone-redundant) as a
  setting. Each also fences the old primary, so that two never take
  writes.
- **What a failover may lose.** Replication is asynchronous by default,
  so a promotion can lose the last writes ariesd was told were
  committed. With `synchronous_standby_names` set and
  `synchronous_commit = on` (or `remote_apply`), a commit returns only
  once the standby has it, and a promotion loses nothing acknowledged,
  at the cost of the standby's round trip on every write. A write lost
  this way had already been answered, and no client sends it again.
- **Point ariesd at whatever follows the primary**: the service's
  endpoint, a virtual address Patroni moves, or every host, with the
  primary chosen by its role:
  `postgres://aries@db1.example.com,db2.example.com/aries?target_session_attrs=read-write`.
  When the primary goes, ariesd's connections end and its requests fail
  with `unavailable` until the new primary answers; then its pool
  connects again, its next write takes back the writers' locks, and it
  serves, with no restart. That is what a restart of Postgres under a
  running ariesd showed ([step 5](#5-run-ariesd-under-systemd)); a
  failover itself was not run.
- **ariesd's writer's lock fences ariesd, not the database.** Its locks
  are held in the primary's memory and are not replicated, so after a
  promotion they are free, and the first ariesd that writes takes them.
  While one ariesd holds a project's lock no other writes that project,
  however many share the database
  ([One writer for each project](#one-writer-for-each-project)).
  Keeping an old primary from taking writes is Patroni's or the
  service's.
- **A second ariesd, ready to take over**, on another machine, needs
  the same config file and the same SSH host key, since every
  `.aries.yaml` pins it ([Rotating the host key](#rotating-the-host-key)),
  and starts only when the first has stopped: the control store's lock
  refuses it while the first holds it. Moving clients to it (a DNS name,
  an address that moves) is yours. Not run.
- **A standby is not a backup.** A mistake replicates at once. Keep
  the daily dumps.

## ariesd commands

| Command | Does |
|---|---|
| `serve` | Run the daemon. Flags: `--config`, `--dsn`, `--socket`, `--project`, `--prefix`, `--log-level`, `--log-format`, `--dev`, `--allow-unsafe-account`, and `--accept-clock-behind TIME`, which starts once although the clock is behind a history no later than TIME ([The server's clock](#the-servers-clock)). Exits 75 when it refuses to start for the clock, and 1 for any other failure |
| `stdio --principal NAME [--key FINGERPRINT]` | The sshd forced command: bridge one SSH session to the daemon. `--key` is written by ariesd, into each line of its keys file, never by hand: it names the key that signed in, so ariesd refuses the session unless the control store holds that key for NAME, and closes it when the key is removed |
| `control init --name NAME [--authorized-keys FILE]` | Once, after setting `control_schema`: make the control store, adopt the schema `dsn` names as the first project and import principals and keys from `authorized_keys` ([Serving many projects](#serving-many-projects)). Safe to run again |
| `import-bd [--project UUID] [--principal NAME] [--dry-run] [--json] FILE` | Import a bd export (`-` for stdin); see [Moving from bd](migrate-from-bd.md). `--project` picks a project on a server of many |
| `export-bd [--project UUID] [-o FILE]` | Write the store in bd's JSONL format |
| `upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart]` | Replace the binary with a verified release ([Upgrades](#upgrades)) |
| `status [--json] [--config FILE] [--socket PATH]` | Print the running daemon's [status report](#ariesd-status), as the user ariesd runs as |
| `version` | Print the version |

## Troubleshooting

| What you see | Cause | Fix |
|---|---|---|
| `ariesd serve` refuses to start: `transports: names https, and https: is not set` | A fresh config serves HTTPS, and says nothing of the listener | Set `https:` ([The HTTPS listener](#the-https-listener)), or `transports: [ssh]` for a server of SSH alone |
| `aries: the TLS certificate of HOST:443 does not verify` | The server's certificate is from a CA the machine's system store lacks (a company's or a site's), or something stands between | Ask the operator for the CA's certificate and put it in `server.ca`. If the certificate should verify as it is, do not connect. Nothing is queued |
| `aries: HOST:443 answered the WebSocket upgrade with 400 Bad Request` | A proxy on the way does not pass WebSocket upgrades | Configure the proxy ([Behind a proxy](#behind-a-proxy)), or use SSH where the server has it (`ARIES_TRANSPORT=ssh`) |
| `aries: HOST:443 answered 502 Bad Gateway` | ariesd does not run behind the proxy, or does not answer it | Start ariesd; check `journalctl -u ariesd` and the proxy's log. Writes are queued meanwhile |
| `aries: … SSH is off on this server` | The server serves HTTPS alone, and this checkout reaches it by SSH | Set `server.url` in `.aries.yaml` (and unset `ARIES_TRANSPORT`), or have the operator turn SSH on ([SSH on and off](#ssh-on-and-off)) |
| `tls.cert_reload_failed` in the log | The certificate's files changed and do not make a pair: a key that is not the certificate's, or one others may read | Put the matching pair in place, the key mode 0600; ariesd keeps serving the old certificate meanwhile, and reads the files again when they change |
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
| `project "…" is not served by this server` | `.aries.yaml` and the config file name different projects; with `control_schema`, the project is unknown or archived | Make the two `project` values match |
| `… is not a member of project "…"` | With `control_schema`: the principal has no row in the control store, or is in other projects only | An admin runs `aries admin member add NAME` in the project's repository (`principal add NAME` for someone new) |
| `… is disabled on this server` | With `control_schema`: the principal was disabled, whatever `admins:` says | An admin with reach over it runs `aries admin principal enable NAME` |
| `aries: … accepted no key`, for a key added with `aries admin` | sshd is not reading ariesd's keys file, or the file has no line for the key | On the server: `sshd -T -C user=aries \| grep -i authorizedkeysfile` must name `.ssh/aries_keys`; `grep -c NAME /var/lib/aries/.ssh/aries_keys` shows whether the line is there; `journalctl -u ssh` says when sshd would not read the file for its owner or mode |
| `aries admin` warns that ariesd `could not write the keys file sshd reads`, or `aries admin status` says `THE LAST WRITE FAILED` | ariesd cannot replace its keys file. The change itself is recorded | `journalctl -u ariesd` has `the keys file is out of date` with the cause and a fix: usually the unit's `ReadWritePaths` ([Keys from the store](#keys-from-the-store), step 1), or the owner or mode of `.ssh`. ariesd tries again every 30 seconds |
| ariesd does not start: `write the keys file …` | With `control_schema`: it cannot write its keys file | The error's `fix:` names the change; see [Keys from the store](#keys-from-the-store), step 1 |
| The log has `telemetry: spans say nothing about who: the key for principals' stand-ins is unavailable`, or the status report says `TRACES SAY NOTHING ABOUT WHO` | ariesd exports traces, or logs, and cannot make or read the key it names people with. It serves as before; its spans and exported log records carry no principal and no session. The line with `cause=` says which: `it cannot be made: no such file or directory` (no directory), `read-only file system` (the unit's `ReadWritePaths`), `permission denied` (the directory's owner, or the AppArmor profile) | The lines with `fix=` beside it: make the directory and add the drop-in ([Send metrics to a collector on the machine](#send-metrics-to-a-collector-on-the-machine)). ariesd looks again every 30 seconds; a new drop-in needs a restart. Or set `telemetry: principals: omit` to leave people out on purpose |
| The same, with `cause="its file is mode 0644: others can read it"`, or `its file does not hold a key`; the status report says the key `CANNOT BE TRUSTED` | The key's file is there and is not safe to use: someone else may have read it, or it was cut short or changed | If someone may have read it, delete it: ariesd makes a new one within 30 seconds, and stand-ins made with the old key can no longer be resolved. If nobody can have, `chmod 600` it ([Traces](#traces), People) |
| `aries admin principal whois` answers `this server has no key for stand-ins: …` | What follows the colon says why: the key is unavailable, as in the two rows above; or the server exports no traces, or names people by `name` or `omit`, and has no key file from before | For an unavailable key, the rows above. Nothing resolves a stand-in made under a key that is gone |
| `the file there was not generated by ariesd` | `keys_file` names a file that holds something else, such as `authorized_keys` | Set `keys_file` to a file of its own, or move the other file away |
| A key removed with `aries admin key remove` still signs in | Its line is still in `authorized_keys`, as the command warned | Delete the line ([Keys from the store](#keys-from-the-store), step 5) |
| `… may not add a key to …: nobody adds a key to their own account` | `aries admin key add` for one's own principal | Ask another admin with reach; for a server admin, another server admin or a line in `authorized_keys` |
| `key … is the key this connection signed in with` | `aries admin key remove` of your own key, on a connection that key signed in | Sign in with another of your keys (`ARIES_KEY_FILE=PATH`) and remove it from there, or ask an admin |
| `… is the last server admin who is not disabled` | `aries admin principal disable` of the only enabled server admin | Enable another first, or name another under `admins:` and restart |
| `the key that signed in is no longer one the server holds for …` | The key was removed, and sshd read a keys file from before that: one ariesd could not rewrite, or one restored from a backup; or an `authorized_keys` line was given `--key` by hand, which only ariesd writes | Use another key, or have the key added again; check `aries admin status` for a failed write; remove `--key` from a hand-written line |
| `the key that signed in ended at …` | The key was added with `--expires`, and its end has passed ([Keys that end](#keys-that-end)) | Ask an admin for a key that has not ended: `aries admin key add NAME FILE.pub`, with `--expires` for another end |
| `this server holds one project, so it has no projects, principals or members to manage` | `aries admin project`, `principal`, `member` or `key` on a server with no `control_schema` | [Opt in](#opt-in), or add people as lines of `authorized_keys` |
| `schema project_… , which project … would have, is already in the database` | `aries admin project add` named a project whose schema ([Opt in](#opt-in), step 3) something else made: another ariesd's project, an earlier one, or a person | Choose another name; or, if nothing uses that schema, drop or rename it as its owner and run the same command |
| `another role made schema …` | A project being created found its schema made by another role, which ariesd's may not use | As the schema's owner or a superuser, run the statement in the fix, then run the same `aries admin project add NAME` again |
| `project … is served by another process on the same database` | Another ariesd, or an operator's command, holds the project's writer's lock ([One writer for each project](#one-writer-for-each-project)) | Stop the other process; `pg_stat_activity` shows its `ariesd writer locks` connection |
| `the control store … is served by another ariesd on this database` | ariesd does not start: another holds the control store's writer's lock | Stop the other ariesd, or give this one a `control_schema` of its own |
| `… may not …: it is for …` | An `aries admin` command by someone who may not run it ([Managing access](#managing-access)) | Ask the admin the fix names |
| `ariesd upgrade` restores or installs the binary, then `sudo -n systemctl restart …: fork/exec /usr/bin/sudo: permission denied` | On Ubuntu 26.04, AppArmor profiles from before 14 October 2026: `sudo` is sudo-rs there, at a path the old operator's profile does not name | Install this release's profiles ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)), then `sudo systemctl restart ariesd` |
| `ariesd could not open the database of an active project` from `ariesd upgrade` | A project's database is gone, not granted, or failed to migrate | `journalctl -u ariesd` names it and why; fix it or archive the project, then restart |
| `the control store … holds no active project` at start | `control_schema` is set and `control init` has not run | `sudo -u aries -H ariesd control init --name NAME` |
| ariesd does not start, and `systemctl status ariesd` shows `status=75/TEMPFAIL` under `not starting: the server's clock is behind its own history` | The clock is behind the latest event of the database named, by more than `clock_behind_tolerance`: the clock was reset or set back, the machine booted without its time, or the database was written by a clock that ran ahead | `timedatectl`; correct the clock and `systemctl start ariesd`. If the history is what is wrong, start once with the `--accept-clock-behind` the refusal prints, or set `clock_behind: warn` ([The server's clock](#the-servers-clock)). systemd does not retry this exit |
| After a boot, ariesd and every timer wait for good; `systemctl list-jobs` shows `time-sync.target start waiting` | `systemd-time-wait-sync` is enabled with its shipped `TimeoutStartSec=infinity`, and no time server answers | Add the drop-in of [Start ariesd after the clock is set](#start-ariesd-after-the-clock-is-set), which bounds the wait at 90 seconds; `systemctl stop systemd-time-wait-sync` lets this boot go on |
| `clock behind history` in the log, or `CLOCK BEHIND HISTORY` in `aries admin status` | The clock went back under a running ariesd, or it started with `clock_behind: warn` or `--accept-clock-behind` | Correct the clock. Until it has caught up, timestamps are ahead of it and leases run at half speed; with `clock_behind: stop`, ariesd will not start again while this lasts |
| `clock stepped` in the log, or a `clock:` line in `aries admin status` | The clock was set while ariesd ran, or the machine was suspended and resumed | Nothing for the data: leases were extended by a jump forward, and time did not go back for a step back. Find what set the clock, and whether the time service is synchronized |
| `ariesd upgrade` stops with `nothing was changed: ariesd would not start again after this upgrade` | The clock is behind the server's history, so the restarted daemon would refuse to start | As for exit status 75 above; then run the upgrade again |
| `usage record …: at … is more than 5m0s past the server's clock` | The machine that recorded the usage runs ahead of the server, or the server runs behind | Check both clocks; `clock_client_tolerance` widens the bound. `aries usage --hook` drops such a record and says how many |
| ariesd refuses the database role: `… is unsafe` | The role is a superuser, or has `CREATEROLE`, `CREATEDB`, `REPLICATION` or `BYPASSRLS`, or is a member of a role that reaches the server's files | Run, as `postgres`, the SQL its fix names, or make the role as [step 2](#2-create-the-role-and-the-database) does |
| `the database server is Postgres …, older than 16` | ariesd opens Postgres 16 or newer | Upgrade Postgres (`pg_upgradecluster`, after a [dump](#backups-and-restore)), or point `dsn` at a newer one |
| `role "aries" does not exist`, or `Peer authentication failed for user "aries"` | Step 2 has not run on this Postgres; or ariesd, or the command, runs as a Unix user other than `aries`, whom the unix socket signs in as the role of that name | Run [step 2](#2-create-the-role-and-the-database); run operators' commands as `sudo -u aries -H ariesd …` |
| `password authentication failed for user "aries"` | With a Postgres on another machine: the password in `/etc/aries/postgres/pgpass` is not the role's, the DSN names no `passfile=`, or the password changed and ariesd still has the old one | Correct the file (it is `*:*:*:*:PASSWORD`, with `\` and `:` escaped) and restart ariesd ([Managed services](#managed-services)) |
| `dsn: passfile /etc/aries/postgres/pgpass holds the database password but is mode 0644, open to group or others` | The password file, or the key of a client certificate (`sslkey`), can be read by other users | `chmod 600` the file it names, owned by `aries`, and start ariesd again |
| `sorry, too many clients already`, or `remaining connection slots are reserved` | Postgres is at its `max_connections` | Find who holds them: `sudo -u postgres psql -c 'SELECT usename, application_name, count(*) FROM pg_stat_activity GROUP BY 1, 2'`; raise `max_connections` or lower what others hold ([Sizing Postgres](#sizing-postgres)) |
| `aries admin project add` is refused with a fix that says `GRANT CREATE ON DATABASE …` | ariesd's role may not make schemas in its database: it does not own it, as where schemas were made beforehand ([Managed services](#managed-services), step 4) | Run the statement as the database's owner, then the same `aries admin project add` again |
| `aries` exits with code 3 | Client and server speak incompatible protocols | Upgrade the server, then the client |
| `busy` | A [limit](#limits) | Wait as the fix says, or raise the limit |
| `permission denied` on a file or socket the `aries` user owns, after you moved a path; or from `control init`, `import-bd`, `export-bd` or `upgrade` | With the [AppArmor profiles](#confine-ariesd-with-apparmor): the path is not the one the profiles name, or the command ran without `aa-exec` | `journalctl -k -g 'apparmor="DENIED"'` names the profile and the path. Change the path in `/etc/apparmor.d/tunables/ariesd` and run `apparmor_parser -r /etc/apparmor.d/ariesd`; run an operator's command through `aa-exec -p ariesd-admin --` |
| ariesd does not start: `Failed to prepare AppArmor profile change to ariesd-serve` | The unit asks for a profile that is not loaded | `apparmor_parser -r /etc/apparmor.d/ariesd`, or remove the unit's `apparmor.conf` ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)) |
| ariesd does not start: `connect: permission denied` on `/var/run/postgresql/.s.PGSQL.5432`, or a refused connection to another machine's Postgres | The AppArmor profiles are loaded and the unit does not ask for `ariesd-serve`, so the daemon has the bridge's profile (with SSH on); or the profiles are from before Postgres | Add the unit's `AppArmorProfile=ariesd-serve` line, `systemctl daemon-reload`, and restart; install this release's profiles |
| ariesd does not start at boot: `connect: no such file or directory` on `/var/run/postgresql/.s.PGSQL.5432` | Postgres was not running: its cluster failed, or the unit lacks `postgresql.service` in `After=` and `Wants=` | `systemctl status postgresql@18-main`, `journalctl -u postgresql@18-main`; the unit of [step 5](#5-run-ariesd-under-systemd). systemd starts ariesd again five seconds later |
| ariesd answers `aries`, yet `systemctl status ariesd` says `activating`, and every ten minutes `start operation timed out` restarts it; its log has `could not tell systemd the daemon is ready` | The unit has `Type=notify` and the daemon cannot write to systemd's socket: the AppArmor profiles loaded are from before the unit had that line | Install this release's profiles and `apparmor_parser -r /etc/apparmor.d/ariesd` ([Confine ariesd with AppArmor](#confine-ariesd-with-apparmor)); `journalctl -k -g 'apparmor="DENIED"'` names `/run/systemd/notify` |
| `Watchdog timeout (limit 2min)!` in the journal, then goroutine stacks and a restart | ariesd did not tell systemd it was alive for `WatchdogSec`: it hung, or one tick of its reaper took that long, as it can when Postgres stops answering | Read the stacks, which say where it was; check Postgres. If the machine is merely slow, raise `WatchdogSec` |
| `ariesd status`: `config: open /etc/aries/ariesd.yaml: permission denied` | It was run as a user who cannot read the config file, or as root with the AppArmor profiles loaded | `sudo -u aries -H ariesd status` |
| `telemetry: a metrics export failed` with `socket: permission denied` or `failed to load system roots` | With the AppArmor profiles: the collector's endpoint is a name that needs DNS, or is HTTPS, and the daemon's profile allows neither as shipped | Add the three lines of [Confine ariesd with AppArmor](#confine-ariesd-with-apparmor) to `local/ariesd-serve`, or send to a collector on the machine |
| `SESSIONS` is `unknown` in `aries admin status` or `ariesd status`, with `sessions: unknown for … (database did not answer in 2s)`, or `postgres: unknown` | Postgres did not answer within 2 seconds: it hangs, is stopped, restarting or frozen, swaps, or a lock holds its catalog. The rest of the report is ariesd's own memory and stands | On the server: `systemctl status postgresql@18-main`, `journalctl -u postgresql@18-main`, and what waits: `sudo -u postgres psql -c 'SELECT pid, state, wait_event, query_start, left(query, 60) FROM pg_stat_activity WHERE datname = '\''aries'\'''`. With `(the read failed; see ariesd's log)` instead, the log's `count the sessions seen, for status` has the error |
| `aries`: `the server's database does not answer, so project "…" was not opened`, from `aries admin status` too | Postgres did not answer a ping within 2 seconds, and the project's store is closed: ariesd does not open a store on a Postgres that hangs, and a connection is to a project ([When Postgres does not answer](#when-postgres-does-not-answer)). The log has `database.check_failed` with the ping's error | Run `sudo -u aries -H ariesd status` on the server, or `aries admin status` from the repository of a project in use; then see to Postgres: `systemctl status postgresql@18-main` |
| `aries`: `the server timed out on …` after a minute, while `aries admin status` answers | Postgres does not answer. The status report does not wait for it; every other request does, for the request timeout | `aries admin status`, or `ariesd status` on the server, for what ariesd knows; then see to Postgres |
| `ariesd status`: `status: the daemon did not answer` | ariesd welcomed the connection and then sent no report for 30 seconds, or closed it: the daemon itself is stuck or stopping, since the report does not wait on Postgres | `journalctl -u ariesd` for what it waits on; `systemctl restart ariesd` if that does not end |
| `/readyz` answers `not ready: the last check of the database failed` | Postgres hangs, is restarting, or cannot be reached | `systemctl status postgresql@18-main`, `journalctl -u postgresql@18-main`; ariesd's log has `database.check_failed` with the ping's error. It clears by itself once Postgres answers |
| One `agent.register_failed` in the log, with `terminating connection due to administrator command (SQLSTATE 57P01)`, after Postgres restarted | The restart ended a connection in the middle of a session's note. The pool hands out no connection Postgres ended while it was idle, so this is only for what was in flight at the moment of the restart. The request itself is answered; the session is noted at its next one | Nothing |
