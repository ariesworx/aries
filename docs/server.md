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
step, then covers principals, settings, limits, upgrades, backups and
troubleshooting.
For complete deployments with costs, see the
[deployment examples](deploy/README.md).

## What a server needs

- **Linux**, amd64 or arm64, with systemd and OpenSSH. `starfixd serve`
  refuses to run on other systems, because only Linux lets it check which
  user connects to its socket.
- **Ubuntu 24.04 LTS is the supported platform.** The steps below are
  written for it and were tested on it. Other distributions may work and
  are untested.
- **Systems that run SELinux are not supported yet**: RHEL, Rocky Linux,
  AlmaLinux, Fedora. Nothing here has been run on one, and this guide
  sets no SELinux label and ships no policy module. That support will
  follow once starfix has a test harness that boots such a machine.
  Ubuntu's mandatory access control is AppArmor; an AppArmor profile for
  starfixd is planned and does not exist yet.
- **Dolt**, at the release starfix is tested with: `Dolt` in
  [internal/version](../internal/version/version.go), 2.4.2 for starfix
  v0.2.0. Each release's notes name it.
- **Little hardware.** Idle, Dolt used about 90 MB of memory and starfixd
  about 13 MB. Under the stage 0 load test Dolt reached 1.4 GB and about
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
tar -xzf dolt-linux-amd64.tar.gz
install -m 0755 dolt-linux-amd64/bin/dolt /usr/local/bin/dolt
dolt version
```

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
```

The log ends with `Server ready. Accepting connections.`

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
  printf 'dsn: starfix:%s@tcp(127.0.0.1:3306)/starfix\nproject: %s\nsystemd_unit: starfixd.service\nadmins: [alice]\n' "$dbpw" "$(cat /proc/sys/kernel/random/uuid)" |
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

The step runs once: a second run finds `root` protected and stops.

### 5. Run starfixd under systemd

Save this as `/etc/systemd/system/starfixd.service`:

```ini
[Unit]
Description=starfix server
After=network.target dolt.service
Requires=dolt.service

[Service]
User=starfix
Group=starfix
ExecStart=/opt/starfix/bin/starfixd serve
Restart=on-failure
RestartSec=5
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
`starfix` user reaches the daemon's socket. Then let `starfixd upgrade`
restart the unit, and start it:

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
```

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
everyone who connects.

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

## Principals

A principal is a name for a person or an agent identity, such as `alice`.
It comes from the key that authenticated, never from the client: sshd
runs a forced command that names it, and nothing the client sends can
change that.

- **Name rules:** lowercase, starting with a letter, then letters, digits,
  `.`, `_` or `-`, up to 64 characters. `starfixd` (the claim reaper and
  the daemon's own changes), `import` (the bd importer and `control init`)
  and `starfixd-upgrade` (the health check of `starfixd upgrade`) are
  reserved.
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
[Docker test server](deploy/docker.md). It has not been run under
systemd, or on a cloud provider's machine; that section says what was
checked and what was not.

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
   3 keys in the store are also lines of /var/lib/starfix/.ssh/authorized_keys: sshd falls back to that file, so `sfx admin key remove` does not keep such a key out until its line is deleted too
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
database holds to a file, and sshd reads that file beside
`authorized_keys`. An admin adds and removes keys with `sfx admin`, from
anywhere, and nobody edits a file on the server. The file is
`~starfix/.ssh/starfix_keys`, in the format of `authorized_keys`: a
first line that says starfixd generates it, then one line for each key
of each active principal, the line you would have written by hand with
the key's fingerprint added:

```text
restrict,command="/usr/local/bin/starfixd stdio --principal alice --key SHA256:…" ssh-ed25519 AAAA…
```

sshd asks starfixd nothing. It reads two files, and nothing of starfix
runs before sshd has authenticated a key. `authorized_keys`, which you
maintain by hand, stays the way in for the first admin and the way to
replace a server admin's own key.

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
  and is refused by starfixd.
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
       AuthorizedKeysFile .ssh/authorized_keys .ssh/starfix_keys
       AuthorizedKeysCommand none
   ```

   ```sh
   sshd -t
   systemctl try-reload-or-restart ssh
   ```

   sshd takes the first line that matches the key, in the order the
   files are named, so a key in both files is let in by its line of
   `authorized_keys`.

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

   - **A key whose line is still in `authorized_keys` cannot be revoked
     with `sfx admin key remove`.** sshd finds the line in that file
     and lets the key in, by a forced command that names no
     fingerprint, so starfixd cannot refuse the key or close the
     connections it made. Its principal can still be disabled, and its
     memberships removed, which starfixd enforces whatever let the key
     in.
   - **starfixd tells you which keys those are.** It runs as the
     account whose `authorized_keys` it is, so it reads the file: it
     never writes to it, and never prints or logs a line of it. `sfx
     admin key list NAME` marks each key that is also in
     `authorized_keys`, and `sfx admin key remove` of such a key warns
     that sshd still accepts it there and that its line must be deleted
     on the server. A line starfixd cannot read as a key is skipped,
     and only the first mebibyte of the file is read.
   - **It also counts who still comes in that way.** A connection whose
     forced command named no fingerprint came in by `authorized_keys`.
     `sfx admin status` counts those for each project (BY HAND), and
     `sfx admin key list NAME` for NAME. A line of `authorized_keys`
     whose key was never put in the control database shows up only
     there.
   - **Keep one line for a server admin.** It is how a server admin's
     own key is replaced, since nobody changes the keys of their own
     account with `sfx admin`.

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
without the daemon.

**When a write fails after the start,** the change an admin made is
already recorded, and stays. The admin's command succeeds and prints a
warning that sshd may still be reading the old file; starfixd logs
`the keys file is out of date` at `error` with the cause and a fix, and
tries again every 30 seconds; and `sfx admin status` says `THE LAST
WRITE FAILED` on its `keys file` line until a write succeeds. Meanwhile
a key just added does not sign in, and a key just removed is refused by
starfixd rather than by sshd.

**On an image whose sshd already has a global `AuthorizedKeysCommand`**
for its provider's login service (Google Cloud's OS Login, AWS's EC2
Instance Connect, Azure's Entra ID login), nothing above needs to
change: `AuthorizedKeysFile` in the `Match` block is read whatever that
command is. Left alone, sshd would still run the provider's command for
the `starfix` account, on every sign-in, and accept a key it printed,
with no forced command. `AuthorizedKeysCommand none` in the `Match`
block is what stops that: the account's keys come from the two files and
nowhere else.

What was checked, on 12 October 2026, with OpenSSH 9.6 on Ubuntu 24.04
in the [Docker test server](deploy/docker.md), which has no systemd:

| Checked | Result |
|---|---|
| starfixd installed as [step 3](#3-install-starfixd) installs it: owned by `starfix` in `/opt/starfix/bin`, linked from `/usr/local/bin`, and no copy that root owns | Keys from the control database sign in. sshd runs no program of starfix's before authentication, so who owns the binary does not matter to it |
| A global `AuthorizedKeysCommand`, outside any `Match` block, set to a root-owned script that prints nothing, with `AuthorizedKeysCommandUser nobody` | Keys from both files sign in, with no override. sshd ran the script twice for each sign-in by `sfx`. With `AuthorizedKeysCommand none` in the `Match` block it did not run it |
| starfixd killed, sshd left running | sshd accepts a key from either file, and the client gets `starfixd is not running on the server` |
| A key in both files | sshd lets it in by its line of `authorized_keys` |

Not checked:

- **Under systemd.** Step 1's `ReadWritePaths`, and what `ProtectHome`
  covers, are from systemd's documentation; the unit was not run with
  them. If the path is wrong, starfixd does not start and its error
  names `ReadWritePaths`.
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
| `sfx admin principal disable NAME`, `enable NAME` | Server admins; a project admin, for a principal within their reach |
| `sfx admin member add NAME`, `remove NAME` | Server admins; the project's admins |
| `sfx admin member list` | Server admins; the project's admins |
| `sfx admin principal add NAME --key FILE`, `key add NAME FILE`, `key remove FINGERPRINT` | Server admins; a project admin, for a principal within their reach. Never for one's own account |
| `sfx admin key list NAME` | Server admins; a project admin with reach over NAME; NAME themselves |
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
  on the server.
- **Nobody changes the keys of their own account**, a server admin
  included: an agent holding a person's key could otherwise add one of
  its own. Another admin with reach does it. A server admin's keys are
  changed by another server admin, or by a line in `authorized_keys` on
  the server.
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
  released. Taking the admin role away releases nothing. A project
  whose store is closed at that moment has nothing running to release
  them: its claims end when the store next opens, before its first
  request, recorded under `starfixd` with the same reason.
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
  request in flight took meanwhile. The server's last active project cannot be archived, since
  `sfx admin` needs a project to connect to. `activate` opens the
  database, which applies any migration it missed while archived, and
  serves it again.
- **Everything is on the record** in the control database's event log,
  under the principal who ran the command.

One case in the above is slower than "at once". A project whose store is
closed has nothing running in it: when its member is removed, or when it
is archived by a starfixd that restarted before it released the claims,
the claims stay in its database until the store next opens, which for an
archived project is when it is activated. Nobody can use them meanwhile,
and they end before the first request is answered.

### Watching a server

`sfx admin status` reports what starfixd knows of itself and of its
projects now. A server admin sees the server and every project; a project
admin sees the active projects they administer and nothing of the
server; anyone else is refused. It reads memory, plus one count on an
index for each open project listed, opens no store and writes nothing.

```text
starfixd v0.5.0, protocol 2 to 6, up 3d 4h
stores 2 of 64, connections 14 of 1024
keys file /var/lib/starfix/.ssh/starfix_keys: 31 keys, written 2026-10-11 09:12 UTC
starfixd's memory 42 MiB in use, 78 MiB from the system; 112 goroutines
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
| starfixd's memory | The Go heap in use and all the memory starfixd has from the operating system (`HeapAlloc` and `Sys`). Not Dolt's |
| projects, principals | The control database's rows, by state |
| STORE | Whether the project's store is open. The figures after it exist only while it is, and start from zero each time it opens |
| CONNS, BY HAND | Connections past their handshake, and those among them that named no key of the store, which sshd let in by the hand-maintained `authorized_keys` |
| SESSIONS | Sessions seen in the last five minutes, as `sfx who` lists them |
| WRITES/MIN | Transactions that changed something and committed in the last 60 seconds. A refused request is not one; a lease renewal is |
| P50, P99 | The median and 99th percentile of the latest 256 writes' latency, however old they are: from the request reaching the project's store to its commit, the wait for the one writer included |
| LAST USED | When the project last answered a request, or was opened |

`-n` and `--after NAME` page the projects as the admin lists do, 200 at a
time. `--json` gives the same figures, with latency in microseconds.

**Dolt's memory is not in it.** Dolt is another process under another
user, and starfixd has no portable way to read its memory, so read it
from the machine: `systemctl status dolt`, `ps -o rss= -C dolt` or your
monitoring. It follows what has been written since Dolt started, in
every project; closing a store gives none of it back, and a restart of
Dolt gives back most ([Dolt's memory](#dolts-memory)). Nor is a database's size
on disk: Dolt offers its least-privileged account no table or function
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
| `log_level` | `debug`, `info`, `warn` or `error` ([Logs](#logs)) | `info` | `--log-level`, `STARFIXD_LOG_LEVEL` |
| `log_format` | `text` (key=value) or `json`, for a log shipper | `text` | `--log-format`, `STARFIXD_LOG_FORMAT` |
| `latest` | The newest starfix release, set by hand. Clients' `prime` notes when this server is older | none | |
| `account` | The project's account: what an issue's time and tokens report against when neither it nor an ancestor sets one ([Accounts, time and tokens](concepts.md#accounts-time-and-tokens)). A code name: lowercase letters, digits and inner hyphens | `internal` | |
| `limits` | [Limits](#limits) | defaults | |

- **A password never goes on the command line.** `--dsn` with a password is
  refused, because it would show in the process list. A config file that
  holds one must be mode 0600.
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
| `conns` | 1024 | Connections past the handshake; twice this caps sockets still in it |
| `conns_per_principal` | 32 | One principal's connections |
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
| `keys_per_principal` | 20 | With `control_db`: public keys of one principal |
| `members_per_project` | 1000 | With `control_db`: principals with a role in one project |
| `control_write_rate`, `control_write_burst` | 1, 30 | With `control_db`: each principal's token bucket for `sfx admin` changes to projects, principals, members and keys: changes a second, and how many at once. It is the principal's on the whole server, apart from its write bucket in each project; the lists are not counted |

On a server of [many projects](#serving-many-projects), every limit above
that is per principal is per principal in each project: `conns_per_principal`,
the write bucket, `refusal_logs` and the limits on rows a principal owns.
`conns` stays the whole server's, and the `sfx admin` bucket is each
principal's on the whole server. Past `principals`, `keys_per_principal`
or `members_per_project`, `control init` skips the line and says which
limit it met, and `sfx admin` refuses the row with `invalid` and names
the limit; past `projects` it refuses a new project the same way. A key
lookup takes none of a principal's allowances and no connection slot:
sshd asks before anyone is authenticated.

A request past a per-request cap is refused with `invalid`. A write past
the write rate, or an `sfx admin` change past its own, is refused with
`busy`, and its fix says how long to wait.
A `usage` call past `usage_per_day` is refused with `busy`, and stores
none of its records. A
connection past a connection cap is also refused with `busy`, and its fix
asks you to close other starfix sessions or wait for them to end.

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
  at most an hour past the server's clock, and a turn's or session's span
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

## Logs

starfixd logs to stderr, which systemd sends to the journal
(`journalctl -u starfixd`).

| Level | Logs |
|---|---|
| `debug` | Connections and each successful request. With `control_db`, each names its `project` |
| `info` | Startup, refusals, handshake failures and expired claims. Refusals are capped at `refusal_logs` a minute per principal. With `control_db`: each project store opened and closed, each project added, each write of the keys file, the connections closed and each claim released when an `sfx admin` change ends someone's access, and the `project` of every line about one |
| `warn` | Connections refused at the connection cap, socket peers refused, how many refusal lines the cap left out, and failures to accept a connection that may pass, such as running out of file descriptors, which starfixd retries after a pause of up to a second |
| `error` | Internal errors. With `control_db`: an active project whose database could not be opened at start, and a write of the keys file that failed (`the keys file is out of date`), every 30 seconds until one succeeds |

Any other failure to accept connections stops starfixd: it prints the
error and exits 1, and the unit's `Restart=on-failure` starts it again.

## Upgrades

Upgrade the server first, then the clients. A newer server accepts older
clients. A newer client that needs a newer protocol is refused by an older
server at the handshake, with exit code 3 and a fix that says to upgrade
the server. This release speaks protocol 6 (`sfx admin` for projects,
principals, members and keys) and accepts clients that speak 2 to 6, so
v0.2.x clients (protocol 2), v0.3.x clients (protocol 3) and v0.4.x
clients (protocol 4) keep working, without what came after them, and
v0.1.x clients, which speak 1, must upgrade.

On the server, as the `starfix` user:

```sh
cd /var/lib/starfix
sudo -u starfix -H starfixd upgrade --check
sudo -u starfix -H starfixd upgrade
```

`starfixd upgrade` does this, in order:

1. Downloads the release and checks its signature and checksum.
2. Commits the database and tags it `starfix-<old version>`, a point to go
   back to. With `control_db`, it does so in the control database and in
   every project's database, and stops, changing nothing, if one cannot
   be reached.
3. Swaps the binary.
4. With `systemd_unit:` set, or `--restart`, restarts the unit through the
   sudo rule from [step 5](#5-run-starfixd-under-systemd). It then checks
   that the new version answers on the socket within 30 seconds. If it does
   not, upgrade puts the old binary back and restarts it.

With `control_db`, starfixd opens the database of every active project
once before it listens, which applies the new version's migrations to
each (a few milliseconds for a database already current), and the check
answers for all of them: it does not depend on the config file's
`project` being active. If a project's database cannot be opened, the
check is refused and names the project, so upgrade puts the old binary
back; the log says why. An archived project's database is migrated when
the project is activated.

The same happens at every start. A project whose database cannot be
opened does not stop the others from being served: it is logged at
`error`, its connections are refused with `unavailable`, and each new
connection tries it again. Fix its database, or archive the project.
As on a server of one project, the old binary refuses a database the new
one has already migrated, since migrations only go forward. So after an
upgrade that failed partway, the restored version may not serve the
databases the new one reached; the tag of step 2 marks each as it was.

**Keys and the upgrade.** sshd runs nothing of starfix's to find a key
([Keys from the store](#keys-from-the-store)), so an upgrade has no
copy of the binary to refresh for it. The new starfixd writes the keys
file again as it starts.

It refuses to run as root, except with `--check`. `--to vX.Y.Z` installs a given release, but
never an older one, because migrations only go forward. `--rollback`
restores the binary the last upgrade replaced. Take a
[dump](#backups-and-restore) first if you want a copy outside Dolt.

Each person runs `sfx upgrade` on their own machine. It never runs by
itself ([Install](install.md#upgrade-and-roll-back)).

Keep Dolt at the release starfix names, and change it only with a starfix
release that does.

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
- **Read Dolt's own figure.** `ps -o rss= -C dolt`, or
  `process_resident_memory_bytes` if you turn on Dolt's metrics (a
  `metrics:` block with `host: 127.0.0.1` and a `port` in its config;
  it has no authentication, so keep it on loopback). A container's or a
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
  against starfixd's writes (`go_gc_duration_seconds_count` from Dolt's
  metrics; writes from `sfx admin status`): fewer than 3 collections for
  each 100 writes was normal, and more than 14 meant the limit was too
  low. Then restart Dolt or raise the limit.
- **Not measured:** a limit together with systemd's `MemoryMax`, which
  is the way to a hard cap, and a limit on a server that mostly reads.

## starfixd commands

| Command | Does |
|---|---|
| `serve` | Run the daemon. Flags: `--config`, `--dsn`, `--socket`, `--project`, `--prefix`, `--log-level`, `--log-format`, `--dev`, `--allow-unsafe-dolt` |
| `stdio --principal NAME [--key FINGERPRINT]` | The sshd forced command: bridge one SSH session to the daemon. `--key` is written by starfixd, into each line of its keys file, never by hand: it names the key that signed in, so starfixd refuses the session unless the control database holds that key for NAME, and closes it when the key is removed |
| `control init --name NAME [--authorized-keys FILE]` | Once, after setting `control_db`: adopt the server's database as the first project and import principals and keys from `authorized_keys` ([Serving many projects](#serving-many-projects)). Safe to run again |
| `import-bd [--project UUID] [--principal NAME] [--dry-run] [--json] FILE` | Import a bd export (`-` for stdin); see [Moving from bd](migrate-from-bd.md). `--project` picks a project on a server of many |
| `export-bd [--project UUID] [-o FILE]` | Write the store in bd's JSONL format |
| `upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart]` | Replace the binary with a verified release ([Upgrades](#upgrades)) |
| `version` | Print the version and protocol range |

## Troubleshooting

| What you see | Cause | Fix |
|---|---|---|
| `sfx: host key mismatch` | The server's key is not the pinned one | Check the new key with the admin, out of band, before you change `host_key` |
| `sfx: … refused your SSH key`, though the key line is right | The `starfix` account is locked (`!` in `/etc/shadow`), or its shell is `nologin` | `usermod -p '*' starfix`; `usermod -s /bin/sh starfix` |
| `sfx: starfixd is not running on the server` | The daemon is stopped, or failed at start | `journalctl -u starfixd` shows why; `systemctl start starfixd` |
| `project "…" is not served by this server` | `.starfix.yaml` and the config file name different projects; with `control_db`, the project is unknown or archived | Make the two `project` values match |
| `… is not a member of project "…"` | With `control_db`: the principal has no row in the control database, or is in other projects only | An admin runs `sfx admin member add NAME` in the project's repository (`principal add NAME` for someone new) |
| `… is disabled on this server` | With `control_db`: the principal was disabled, whatever `admins:` says | An admin with reach over it runs `sfx admin principal enable NAME` |
| `sfx: … refused your SSH key`, for a key added with `sfx admin` | sshd is not reading starfixd's keys file, or the file has no line for the key | On the server: `sshd -T -C user=starfix \| grep -i authorizedkeysfile` must name `.ssh/starfix_keys`; `grep -c NAME /var/lib/starfix/.ssh/starfix_keys` shows whether the line is there; `journalctl -u ssh` says when sshd would not read the file for its owner or mode |
| `sfx admin` warns that starfixd `could not write the keys file sshd reads`, or `sfx admin status` says `THE LAST WRITE FAILED` | starfixd cannot replace its keys file. The change itself is recorded | `journalctl -u starfixd` has `the keys file is out of date` with the cause and a fix: usually the unit's `ReadWritePaths` ([Keys from the store](#keys-from-the-store), step 1), or the owner or mode of `.ssh`. starfixd tries again every 30 seconds |
| starfixd does not start: `write the keys file …` | With `control_db`: it cannot write its keys file | The error's `fix:` names the change; see [Keys from the store](#keys-from-the-store), step 1 |
| `the file there was not generated by starfixd` | `keys_file` names a file that holds something else, such as `authorized_keys` | Set `keys_file` to a file of its own, or move the other file away |
| A key removed with `sfx admin key remove` still signs in | Its line is still in `authorized_keys`, as the command warned | Delete the line ([Keys from the store](#keys-from-the-store), step 5) |
| `… may not add a key to …: nobody changes the keys of their own account` | `sfx admin key add` or `remove` for one's own principal | Ask another admin with reach; for a server admin, another server admin or a line in `authorized_keys` |
| `… is the last server admin who is not disabled` | `sfx admin principal disable` of the only enabled server admin | Enable another first, or name another under `admins:` and restart |
| `the key that signed in is no longer one the server holds for …` | The key was removed, and sshd read a keys file from before that: one starfixd could not rewrite, or one restored from a backup; or an `authorized_keys` line was given `--key` by hand, which only starfixd writes | Use another key, or have the key added again; check `sfx admin status` for a failed write; remove `--key` from a hand-written line |
| `this server holds one project, so it has no projects, principals or members to manage` | `sfx admin project`, `principal`, `member` or `key` on a server with no `control_db` | [Opt in](#opt-in), or add people as lines of `authorized_keys` |
| `the server has no database names for new projects` | `sfx admin project add` on a server with no `project_dbs` | Grant a block of names and set `project_dbs` ([Opt in](#opt-in), step 4) |
| `starfixd's Dolt account was never granted database …` | `project add` took a name in `project_dbs` that was never granted | As Dolt's root, run the `GRANT` statement in the fix, then run the same `sfx admin project add NAME` again |
| `every database name for new projects is in use` | Every name in `project_dbs` has a project, archived ones included | Grant more names and raise `count` |
| `… may not …: it is for …` | An `sfx admin` command by someone who may not run it ([Managing access](#managing-access)) | Ask the admin the fix names |
| `starfixd could not open the database of an active project` from `starfixd upgrade` | A project's database is gone, not granted, or failed to migrate | `journalctl -u starfixd` names it and why; fix it or archive the project, then restart |
| `the control database … holds no active project` at start | `control_db` is set and `control init` has not run | `sudo -u starfix -H starfixd control init --name NAME` |
| starfixd refuses the Dolt account | The account is `root` or has too many rights, or `secure_file_priv` is empty | Run the SQL its fix names, or redo [step 4](#4-create-the-database-and-the-config-file) |
| `sfx` exits with code 3 | Client and server speak incompatible protocols | Upgrade the server, then the client |
| `busy` | A [limit](#limits) | Wait as the fix says, or raise the limit |
