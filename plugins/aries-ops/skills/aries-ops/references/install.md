# First install

The installer, `install-server.sh`, does the guide's setup on a fresh
Ubuntu 24.04 LTS or 26.04 LTS machine, for a server of many projects:
Postgres with ariesd's role and database, ariesd under systemd serving
HTTPS by the profile chosen, sshd and the forced command only with SSH
on, optionally AppArmor, and the first project and admin
(docs/server.md#with-the-installer; its table maps each of its thirteen
steps to the section it follows). By default it serves HTTPS alone,
with a certificate from Let's Encrypt, and no SSH
(docs/server.md#profiles). You drive it; you do not redo its steps by
hand. Read references/remote.md first.

## 1. Preflight (read-only)

| Check | Command, on the server unless said | Expect | Guide |
|---|---|---|---|
| Reachable, host key verified | `ssh -o BatchMode=yes HOST true`, on the workstation | exit 0 | docs/cli.md#the-host-key-is-pinned |
| Root without a prompt | `sudo -n true` | exit 0, or hand root steps to the operator | references/remote.md |
| The system | `cat /etc/os-release`, `uname -m`, `test -d /run/systemd/system` | Ubuntu 24.04 or 26.04, `x86_64` or `aarch64`, systemd | docs/server.md#what-a-server-needs |
| Resources | `nproc`, `free -m`, `df -h / /var/lib` | 2 GB of memory or more, room for the data | docs/server.md#sizing-postgres |
| No Postgres in the way | `pg_lsclusters` (absent on a fresh machine is fine) | None, or one cluster of 16 or newer, which the installer uses as it is | docs/server.md#1-install-postgres |
| The clock | `timedatectl` | `System clock synchronized: yes` | docs/server.md#keep-the-clock-synchronized |
| DNS | `getent hosts aries.example.com` on the workstation; `hostname -f` on the server | The name clients will use resolves to this machine; with ACME, the ACME server must reach its port 443 too | docs/server.md#certificates |
| Ports | `ss -ltn` | Nothing on the HTTPS listener's port (443 by default), and nothing else of Aries's yet; sshd on your own port, for your path in | docs/server.md#the-https-listener |
| Not set up some other way | `test -e /etc/aries/ariesd.yaml; echo $?`, `command -v ariesd` | Nothing, on a fresh machine | docs/server.md#with-the-installer ("It refuses") |
| AppArmor, if wanted | `test -d /sys/kernel/security/apparmor && command -v apparmor_parser aa-exec` | Both | docs/server.md#confine-ariesd-with-apparmor |

If the machine already has aries: an installer-made server reruns
safely and ends `Nothing to change`; anything else, stop and ask.

## 2. The inputs

Agree each with the operator, and write them into the record:

| Option | Is |
|---|---|
| `--admin NAME` | The first server admin's principal, such as `alice` |
| `--admin-key FILE` | That admin's public key, a `.pub` file, on the workstation. It signs in over HTTPS, and over SSH with SSH on |
| `--host HOST` | The server's DNS name, for its URL and the certificate |
| `--profile P` | `internet` (the default), `private`, `airgapped` or `ssh-only` (docs/server.md#profiles) |
| `--enable-ssh` | Only if the team reaches the server over SSH, IAP included (docs/server.md#ssh-on-and-off) |
| `--disable-ssh` | Only to turn SSH off on a server that served it, once `transports:` no longer names `ssh` and ariesd was restarted (docs/server.md#ssh-on-and-off) |
| `--url URL`, `--listen ADDR` | Only where clients use another URL than `https://HOST`, or the listener another port than 443 |
| `--tls MODE` | `acme`, `files` or `proxy`, in place of the profile's (docs/server.md#certificates) |
| `--cert FILE`, `--key FILE` | With `files`: the certificate and its key, files on the server the operator put there. The key is a secret: you never read, copy or print it; the installer copies it to a 0600 file and never shows it |
| `--acme-email ADDR`, `--acme-directory URL` | With `acme`: an address the ACME server may write to; an internal ACME server, on a site with no route out |
| `--trusted ADDRS` | With `proxy`: the proxy's addresses (docs/server.md#behind-a-proxy) |
| `--port PORT` | With SSH on: sshd's port, if not 22 |
| `--project NAME` | The first project's code name; default `first` |
| `--version vX.Y.Z` | The release; pin it rather than take the latest |
| `--releases SOURCE` | A mirror, only if the server reads releases from one (docs/install.md#where-releases-are-read-from) |
| `--dsn URL` | Only for a Postgres on another machine or a managed service, whose role and database are made first (references/database.md, docs/server.md#managed-services). No password in it: the installer refuses one |
| `--db-password-file FILE` | With `--dsn`: a file on the server the operator wrote, holding the role's password alone (references/database.md). You never read or copy it |
| `--apparmor` | Confine ariesd (docs/server.md#confine-ariesd-with-apparmor) |

Check the key is public before it leaves the workstation:
`ssh-keygen -lf alice.pub` and `grep -c 'PRIVATE KEY' alice.pub`, which
must print 0. Never open the file beside it.

## 3. Copy, then read

1. Copy the key: `scp -o BatchMode=yes alice.pub HOST:`.
2. Fetch the installer on the server, as the guide does:
   `curl -fsSLO https://raw.githubusercontent.com/ariesworx/aries/main/install-server.sh`,
   then `sha256sum install-server.sh`. Offer the operator the script to
   read; the guide asks that it be read before it runs as root.

## 4. Plan and dry run

First the plan, which reads the options alone and changes nothing:

```sh
ssh -o BatchMode=yes HOST -- sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com --plan
```

Show it: the transports, the URL, the TLS mode, and the files it would
write. Without SSH on it names no sshd drop-in and no `authorized_keys`;
if the operator wanted SSH, stop and agree `--enable-ssh`. Then the dry
run:

```sh
ssh -o BatchMode=yes HOST -- sudo -n sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com --version vX.Y.Z --dry-run
```

Show the output. Check: steps 1 to 13 as the guide's table lists them;
no refusal; for each file it would write, either "would write" or "already
the guide's"; no password anywhere (with `--dsn`, "with the password of
--db-password-file" in its place). A difference
in a file it writes, or a refusal, means stop and ask: the fix line says
what to do.

## 5. Run

The same command without `--dry-run`, once the operator approves it.
It prints each step and each command; it ends with the `.aries.yaml`
for the first project. Then run the dry run again: it must say
`Nothing to change`.

## 6. What the installer leaves to you

Each is a change: show it, and wait for a yes (docs/server.md#with-the-installer,
"What it leaves to you").

- **The firewall.** The installer lets the HTTPS port through an active
  ufw, and says so. With SSH on, on a public port: `sudo -n ufw limit
  22/tcp`, and `sudo -n ufw --force enable` if ufw is off
  (docs/deploy/team.md#build-it). Check first that the operator's own
  path to sshd stays allowed.
- **Behind a proxy**, the proxy is the operator's: it passes WebSocket
  upgrades and keeps idle connections 60 seconds
  (docs/server.md#behind-a-proxy).
- **`PasswordAuthentication no`.** Check first: `sudo -n sshd -T | grep
  -i passwordauthentication`; Ubuntu's cloud images already say `no`.
  The guide puts the line above the `Match` line of
  `/etc/ssh/sshd_config.d/00-aries.conf`, but that file is the
  installer's and a later rerun stops on it. Stop and ask the operator
  which they want.
- **On 26.04 behind shared addresses** (IAP, a VPN, a NAT):
  `PerSourcePenaltyExemptList` for their ranges, with the same caution
  (docs/server.md#6-let-the-first-principal-in).
- **Any sshd change:** `sudo -n sshd -t` first; reload with `sudo -n
  systemctl try-reload-or-restart ssh`; keep the current session open
  and check a new `ssh` works before you close it.
- **Backups** (references/backup.md, the daily dump), **telemetry**
  (references/telemetry.md), and **upgrades** (references/upgrade.md).
- **With SSH, keep the admin's line in `authorized_keys`.** It is the
  way in when nothing else is (docs/server.md#keys-from-the-store, step
  5). Without SSH, `/etc/aries/admin.keys` holds it for `control init`.

## 7. Verify

| Check | Command | Expect |
|---|---|---|
| The daemon active | `systemctl is-active ariesd` | `active` |
| The daemon's report | `sudo -n -u aries -H ariesd status` | The version, `transports: https on …; ssh off` (or `ssh on`), a `postgres` line with its version, the first project `active` |
| Postgres | `systemctl is-active postgresql@18-main` (Postgres on the machine) | `active` (references/database.md, "Look") |
| The listener, from the workstation | `curl -fsS https://aries.example.com/.well-known/aries` | JSON with the version and `"transports":["https"]` (or with `ssh`) |
| No SSH path, with SSH off | `sudo -n test ! -e /var/lib/aries/.ssh/aries_keys && echo none` | `none` (docs/server.md#ssh-on-and-off) |
| The keys file, with SSH on | `sudo -n stat -c '%a %U' /var/lib/aries/.ssh/aries_keys` | `600 aries` |
| sshd reads it, with SSH on | `sudo -n sshd -T -C user=aries \| grep -i authorizedkeysfile` | `.ssh/aries_keys .ssh/authorized_keys` |
| Confined, with `--apparmor` | `sudo -n cat /proc/"$(systemctl show ariesd -p MainPID --value)"/attr/current` | `ariesd-serve (enforce)`, or `ariesd-serve//&unconfined (enforce)` on 26.04 |
| The host key, with SSH on | `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` | The fingerprint the operator verified, and the one in `.aries.yaml` |

Then, on the workstation, with the operator's approval to write the file
into their repository: save the printed `.aries.yaml` at the
repository's root, and run `aries -C REPO whoami` (expect the admin's
name), `aries -C REPO admin status` and `aries -C REPO admin project list`
(docs/server.md#7-connect-a-repository). The file holds no secret and is
meant to be committed.

Further projects and people: references/access.md. Developers and
agents: references/onboarding.md. Then write the record
(references/record.md).

## Without the installer

On another distribution, or a machine set up otherwise, the installer
refuses. The guide's steps by hand (docs/server.md#set-up-a-server) are
the reference, untested beyond Ubuntu: say so, and do them only if the
operator asks, one section at a time, each as a script on stdin, shown
whole. For the database's steps, read references/database.md first.
