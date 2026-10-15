# Troubleshooting

Find the cause before you change anything. Everything in "Read" runs
freely; a fix is a change, shown first. The guide's table,
docs/server.md#troubleshooting, maps what each party sees to its cause
and fix: read it at the server's release and quote the row you act on.

## Read, in this order

1. **What the person saw**, word for word: `aries` prints the refusal, a
   `fix:` line and, for a failure on the server, a `req:` line with the
   request's id. Exit code 3 is a protocol mismatch
   (docs/cli.md#exit-codes).
2. **The status report.** From the workstation, `aries -C REPO admin
   status`; on the server, `sudo -n -u aries -H ariesd status`,
   which needs no admin key and answers in about 2 seconds even when
   the database does not (docs/server.md#watching-a-server,
   docs/server.md#ariesd-status). Look at: the version; stores and
   connections against their limits; the keys file line and `THE LAST
   WRITE FAILED`; `clock:` and `CLOCK BEHIND HISTORY`; the database's lines;
   each project's state, `SESSIONS` and latency.
3. **The units**: `systemctl status ariesd --no-pager`, and
   `systemctl show ariesd -p Result -p ExecMainStatus` (75 is the
   clock refusal, docs/server.md#the-servers-clock).
4. **The logs**: `journalctl -u ariesd -n 100 --no-pager`; for one
   request, `journalctl -u ariesd --no-pager | grep REQ`; by event,
   `journalctl -u ariesd -n 200 --no-pager | grep 'event=EVENT'`
   (docs/server.md#events). sshd: `journalctl -u ssh -n 100 --no-pager`,
   and on 26.04 `journalctl -u ssh -g penalty`. AppArmor:
   `sudo -n journalctl -k -g 'apparmor="DENIED"' --no-pager`.
5. **Health**, where the listener is on:
   `curl -s http://127.0.0.1:9464/readyz`.

## Common causes, by where they show

| Seen | First read | Guide |
|---|---|---|
| "ariesd is not running on the server" | `systemctl status ariesd`, `journalctl -u ariesd` | docs/server.md#troubleshooting |
| `accepted no key`, for a key added with `aries admin` | `sudo -n sshd -T -C user=aries \| grep -i authorizedkeysfile`; `sudo -n ssh-keygen -lf /var/lib/aries/.ssh/aries_keys` | docs/server.md#keys-from-the-store |
| `host key mismatch` | Stop. A changed host key is checked with the admin out of band, never accepted to make the error go | docs/cli.md#the-host-key-is-pinned |
| `… is not a member of project` or `is disabled` | `aries -C REPO admin member list --project P`, `aries -C REPO admin principal list` | docs/server.md#managing-access |
| `busy` | The limit named; `aries -C REPO admin status` | docs/server.md#limits |
| `closed the connection before the ssh handshake` (older clients: `handshake failed: EOF`) for everyone behind one address | `journalctl -u ssh -g penalty` | docs/server.md#6-let-the-first-principal-in |
| ariesd does not start, status 75 | `timedatectl` | docs/server.md#when-the-clock-is-behind-the-servers-history |
| `permission denied` under AppArmor | The kernel's denial names the profile and path | docs/server.md#confine-ariesd-with-apparmor |
| Anything about the database | The status report's lines about it; then references/database.md | docs/server.md#when-dolt-does-not-answer |

## Fixing

- Fix what the row names, and nothing next to it. Show the change and
  wait for a yes; restarts drop connections, so say so.
- `systemctl restart ariesd` is a change. `ariesd`'s refusal at
  start carries a `fix:`; follow it.
- If no row fits, stop. Gather the version, the report, the log lines
  around the time and the person's exact message into the record, and
  tell the operator it may be a bug to report.
