# The record

At the end of an install or a change, write the operator a short
summary in a Markdown file on their workstation, at the path they chose
at the start (ask if they did not). Never on the server, and never with
a secret in it. Append to an existing record rather than replace it:
show the new section, and write it with their yes.

It records what the guides call for checking
(docs/server.md#what-was-run-and-where shows the kind of evidence the
guide keeps).

## What goes in

```markdown
## 2026-10-15 14:20 UTC: install of starfix.example.com

- Operator: alice. Workstation: the operator's. Server: `HOST` as the operator's SSH config names it.
- Host key: SHA256:… (verified out of band against the provider's console).
- System: Ubuntu 24.04.x, arm64, 2 vCPUs, 4 GiB.
- Versions: starfixd vX.Y.Z, sfx vX.Y.Z (server and workstation), the database's release.
- Done, each with its guide section:
  - install-server.sh --dry-run, then the run: N changes (docs/server.md#with-the-installer).
  - …
- Checked: the status report, `sfx whoami` as alice, the keys file 600 starfix.
- Left to do:
  - Daily dumps off the machine (docs/server.md#backups-and-restore).
  - …
- Not done, and why: …
```

## Never in it

- The config file's contents, a DSN, a password, a token, a private key
  or a passphrase; nor a path to a private key file.
- Public keys whole: give fingerprints.
- Anything the operator did not see during the session.

If a step failed or was skipped, say so first, with the refusal's
message and its fix.
