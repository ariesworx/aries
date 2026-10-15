# Running commands on the server

How to reach the server from the operator's workstation, and what never
to run. The server's users, paths and units are those of
docs/server.md#set-up-a-server; the installer
(docs/server.md#with-the-installer) makes the same.

## The forms

Show the whole command, including `ssh HOST --`, when you ask to run it.

| Runs | Form |
|---|---|
| As the operator, on the server | `ssh -o BatchMode=yes HOST -- COMMAND` |
| As root | `ssh -o BatchMode=yes HOST -- sudo -n COMMAND` |
| As the `starfix` user | `ssh -o BatchMode=yes HOST -- sudo -n -u starfix -H COMMAND`, from `/var/lib/starfix` |
| An operator's `starfixd` command where the AppArmor profiles are installed | `ssh -o BatchMode=yes HOST -- sudo -n -u starfix -H aa-exec -p starfixd-admin -- starfixd upgrade --check` (docs/server.md#confine-starfixd-with-apparmor) |
| A script of several lines | Write it to a file in a scratch directory on the workstation, show it whole, then `ssh -o BatchMode=yes HOST -- sudo -n sh -s < FILE` |
| `sfx`, against the server | On the workstation, in a repository connected to it: `sfx -C REPO admin status` |

- **`BatchMode=yes`** makes ssh fail rather than prompt: you cannot
  answer a prompt, and a prompt for a passphrase or a password is the
  operator's to answer. If it fails for want of a key in the agent, say
  so: the operator loads their key themselves. Never run `ssh-add`.
- **`sudo -n`** fails rather than asks for a password. When it answers
  `a password is required`, the root steps are the operator's to run:
  give them the exact command for a terminal of their own (`ssh -t HOST
  sudo …`), and check its result yourself with a read-only command once
  they say it ran. Never ask for the password.
- **Never** pass `-A` or `ForwardAgent=yes`, `StrictHostKeyChecking=no`
  or `UserKnownHostsFile=/dev/null`. On a new machine the operator
  checks the host key's fingerprint against the provider's console or
  serial output, out of band, before the first connection
  (docs/deploy/enterprise.md#build-it, step 5).
- **Quote for two shells.** The command passes through the
  workstation's shell and then the server's. Prefer a script on stdin
  to nested quotes.
- **Write a file on the server** from a local file the operator has
  seen: `ssh -o BatchMode=yes HOST -- sudo -n install -m MODE -o OWNER
  -g GROUP /dev/stdin PATH < FILE`. First check what is there (`sudo -n
  cmp FILE PATH`, or `stat`); if the path holds other text, show the
  difference and stop, as the installer does.
- **Copy a public key or the installer** with `scp -o BatchMode=yes FILE
  HOST:` and check it there (`ssh-keygen -lf`, `sha256sum`).
- **Long output:** `journalctl -u starfixd -n 50 --no-pager`, and `grep`
  for what you need, rather than whole logs.

## Never printed

These hold secrets. Never `cat`, `less`, `head`, `tail`, `grep` without
`-c`, `sed -n` or copy them, nor `systemctl show -p Environment`:

| File | Holds |
|---|---|
| `/etc/starfix/starfixd.yaml` | The database password, in `dsn:`. To read its other settings: `sudo -n grep -vi dsn /etc/starfix/starfixd.yaml` |
| `/etc/starfix/telemetry.env`, and any `EnvironmentFile` of the unit | A backend's token (docs/server.md#send-metrics-straight-to-a-hosted-backend) |
| `/var/lib/starfix/telemetry/principals.key` | The key of the stand-ins in traces (docs/server.md#traces) |
| `/etc/ssh/ssh_host_*_key` (no `.pub`) | sshd's private host keys. Fingerprints: `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` |
| The database's own account files (`/var/lib/dolt/cfg` today) | Its accounts |
| Any private key, on either machine | Never read. Check a file is a public key with `ssh-keygen -lf FILE.pub` and `grep -c 'PRIVATE KEY' FILE.pub` (must print 0) |

To learn about such a file, ask about its shape: `stat -c '%a %U:%G %s'
FILE`, `grep -c PATTERN FILE`, `sudo -n test -s FILE`.

## Read-only checks

Run these freely; add `sudo -n` where they need root.

| What | Command |
|---|---|
| The system | `cat /etc/os-release`, `uname -m`, `nproc`, `free -m`, `df -h / /var/lib` |
| The clock | `timedatectl` (docs/server.md#keep-the-clock-synchronized) |
| The units | `systemctl status starfixd --no-pager`, `journalctl -u starfixd -n 50 --no-pager` |
| The versions | `starfixd version`, `sfx version` |
| The status report, on the server | `sudo -n -u starfix -H starfixd status` (docs/server.md#starfixd-status) |
| The status report, from the workstation | `sfx -C REPO admin status` (docs/server.md#watching-a-server) |
| Who the operator is | `sfx -C REPO whoami` |
| The config, without the password | `sudo -n grep -vi dsn /etc/starfix/starfixd.yaml` and `sudo -n stat -c '%a %U:%G' /etc/starfix/starfixd.yaml` (must be `600 starfix:starfix`) |
| sshd's view of the account | `sudo -n sshd -T -C user=starfix \| grep -i authorizedkeys` |
| The keys file | `sudo -n ls -l /var/lib/starfix/.ssh/` and `sudo -n ssh-keygen -lf /var/lib/starfix/.ssh/starfix_keys` |
| The host key | `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub` |
| AppArmor | `sudo -n aa-status --enabled; echo $?`, and `test -f /etc/systemd/system/starfixd.service.d/apparmor.conf` |
| Listening ports | `ss -ltn` |
