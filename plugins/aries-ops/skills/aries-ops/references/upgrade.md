# Upgrading the server

`starfixd upgrade` downloads a release, checks its signature and
checksum, checks the clock against the server's history, tags every
database, swaps the binary, restarts the unit through its sudo rule and
checks the new daemon answers, putting the old one back if it does not
(docs/server.md#upgrades). It runs as the `starfix` user, never as root
(except `--check`). An upgrade is destructive in SKILL.md's sense: the
operator names the version in their reply.

## 1. Look (read-only)

- The running version and the target: `sudo -n -u starfix -H starfixd
  status`, and the target release's notes, which say whether it changes
  the protocol or the database.
- Confined or not: `test -f /etc/systemd/system/starfixd.service.d/apparmor.conf`.
  If it is, every `starfixd` command below runs through `aa-exec -p
  starfixd-admin --` (docs/server.md#confine-starfixd-with-apparmor).
- `systemd_unit:` is set (the installer sets it):
  `sudo -n grep -vi dsn /etc/starfix/starfixd.yaml`.
- The check, which changes nothing:

  ```sh
  ssh -o BatchMode=yes HOST -- 'cd /var/lib/starfix && sudo -n -u starfix -H starfixd upgrade --check'
  ```

  Its `clock:` line must not say the restarted daemon would refuse to
  start (docs/server.md#when-the-clock-is-behind-the-servers-history).

## 2. Before

- **Across a protocol change**, tell everyone first: once the server is
  upgraded, every older `sfx` may be refused until it is upgraded, and
  every agent restarted (docs/server.md#upgrades, the table of what each
  side sees).
- **Take a dump** (references/backup.md), so there is a copy outside
  the database. The upgrade's own tags are inside it.
- **The database** changes only with a starfix release that says how,
  in its notes (references/database.md). The move to Postgres is such a
  release: follow its notes, and stop and ask where this skill has no
  steps for it.

## 3. Upgrade

Name the release; never an older one, which `starfixd upgrade` refuses
since migrations only go forward:

```sh
ssh -o BatchMode=yes HOST -- 'cd /var/lib/starfix && sudo -n -u starfix -H starfixd upgrade --to vX.Y.Z'
```

From a mirror or a directory on a server with no way out, follow
docs/server.md#upgrades ("Where the release is read from", "A server
with no way out"), with `env STARFIX_RELEASES=…` after `sudo -u starfix
-H`.

## 4. Verify

`sudo -n -u starfix -H starfixd status` (the new version, every project
`active`), `systemctl is-active starfixd`, `journalctl -u starfixd -n 20
--no-pager`, and `sfx -C REPO admin status` from the workstation, which
also tests the operator's `sfx` against the new server.

## 5. The clients

Each person runs `sfx upgrade` on their own machine, and restarts their
agents, whose `sfx mcp` stays the old binary until the harness starts it
again (docs/install.md#upgrade-and-roll-back). You may run it on the
operator's workstation with their yes.

## If it fails

- **The health check failed:** upgrade put the old binary back. Read
  `journalctl -u starfixd -n 50 --no-pager` and the guide's
  troubleshooting table (references/troubleshoot.md). Do not retry
  blindly.
- **To go back by hand:** `starfixd upgrade --rollback --restart`,
  through `aa-exec` where confined, restores the binary the last upgrade
  replaced. The old binary refuses a database the new one migrated; the
  tag `starfix-<old version>` marks each database as it was. Restoring a
  database to that tag is not a step the guide gives: stop and ask the
  operator, and point them at docs/server.md#upgrades.
- **`sudo … a password is required`** from the restart: the sudo rule
  must name `starfixd.service` exactly as `systemd_unit` does
  (docs/server.md#5-run-starfixd-under-systemd).
