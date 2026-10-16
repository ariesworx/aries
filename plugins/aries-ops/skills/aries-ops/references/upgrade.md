# Upgrading the server

`ariesd upgrade` downloads a release, checks its signature and
checksum, checks the clock against the server's history, swaps the
binary, restarts the unit through its sudo rule and checks the new
daemon answers, putting the old one back if it does not
(docs/server.md#upgrades). It keeps no copy of the database: the dump
before it is the way back from a migration. It runs as the `aries` user, never as root
(except `--check`). An upgrade is destructive in SKILL.md's sense: the
operator names the version in their reply.

## 1. Look (read-only)

- The running version and the target: `sudo -n -u aries -H ariesd
  status`, and the target release's notes, which say whether it changes
  the protocol or the database.
- Confined or not: `test -f /etc/systemd/system/ariesd.service.d/apparmor.conf`.
  If it is, every `ariesd` command below runs through `aa-exec -p
  ariesd-admin --` (docs/server.md#confine-ariesd-with-apparmor).
- `systemd_unit:` is set (the installer sets it):
  `sudo -n grep -vi dsn /etc/aries/ariesd.yaml`.
- The check, which changes nothing:

  ```sh
  ssh -o BatchMode=yes HOST -- 'cd /var/lib/aries && sudo -n -u aries -H ariesd upgrade --check'
  ```

  Its `clock:` line must not say the restarted daemon would refuse to
  start (docs/server.md#when-the-clock-is-behind-the-servers-history).

## 2. Before

- **Across a protocol change**, tell everyone first: once the server is
  upgraded, every older `aries` may be refused until it is upgraded, and
  every agent restarted (docs/server.md#upgrades, the table of what each
  side sees).
- **Take a dump, and keep it apart**, as docs/server.md#upgrades says:
  `sudo -n systemctl start aries-backup.service`, then copy the day's
  file to `/var/backups/aries/before-upgrade.dump` (references/backup.md).
  A release may migrate the database when the new daemon starts, and
  the old binary refuses a migrated schema: this dump is the only way
  back from that.
- **Postgres itself** is upgraded apart from Aries
  (references/database.md, "Postgres's own upgrades").

## 3. Upgrade

Name the release; never an older one, which `ariesd upgrade` refuses
since migrations only go forward:

```sh
ssh -o BatchMode=yes HOST -- 'cd /var/lib/aries && sudo -n -u aries -H ariesd upgrade --to vX.Y.Z'
```

From a mirror or a directory on a server with no way out, follow
docs/server.md#upgrades ("Where the release is read from", "A server
with no way out"), with `env ARIES_RELEASES=…` after `sudo -u aries
-H`.

## 4. Verify

`sudo -n -u aries -H ariesd status` (the new version, every project
`active`), `systemctl is-active ariesd`, `journalctl -u ariesd -n 20
--no-pager`, and `aries -C REPO admin status` from the workstation, which
also tests the operator's `aries` against the new server.

## 5. The clients

Each person runs `aries upgrade` on their own machine, and restarts their
agents, whose `aries mcp` stays the old binary until the harness starts it
again (docs/install.md#upgrade-and-roll-back). You may run it on the
operator's workstation with their yes.

## If it fails

- **The health check failed:** upgrade put the old binary back. Read
  `journalctl -u ariesd -n 50 --no-pager` and the guide's
  troubleshooting table (references/troubleshoot.md). Do not retry
  blindly.
- **To go back by hand:** `ariesd upgrade --rollback --restart`,
  through `aa-exec` where confined, restores the binary the last upgrade
  replaced. The old binary refuses a schema the new one migrated: going
  back all the way is the restore of the dump taken before
  (docs/server.md#restore), a destructive step the operator names,
  then the rollback.
- **`sudo … a password is required`** from the restart: the sudo rule
  must name `ariesd.service` exactly as `systemd_unit` does
  (docs/server.md#5-run-ariesd-under-systemd).
