# Upgrading to 1.0

From v0.4.0 to v1.0.0, for a developer who uses `aries` and for an
operator who runs a server. [The release notes](releases/v1.0.0.md) say
what changed and why.

> **Draft.** v1.0.0 is not released, and this guide is kept current as
> its work lands. A part marked **(planned)** describes work not yet on
> `main`, with the decision that puts it in the release. Names may still
> change in the release's review of public names (decision 81).

v1.0.0 is not an upgrade in place. The project has a new name, every
binary, file, variable and path is named to match, releases are signed
by a new key, and the server keeps its data in a new database. So:

- each person **installs `aries` again**, and removes the former client;
- each operator **sets up a new server**, and removes the former one;
- each repository gets **a new config file and a new agent setup**.

Nothing reads a former name. Where this guide must show one, it writes
a placeholder:

| Placeholder | Stands for |
|---|---|
| `[former name]` | The project's former name, in lower case, as it appears in v0.4.0's paths |
| `[FORMER]` | The same name in capitals, as it begins v0.4.0's environment variables |
| `[former client binary]` | v0.4.0's client, the binary that is now `aries` |
| `[former daemon binary]` | v0.4.0's server daemon, the binary that is now `ariesd` |

## Contents

- [For a developer](#for-a-developer)
- [For an operator](#for-an-operator)
- [What to remove](#what-to-remove)
- [Carrying issues across (optional)](#carrying-issues-across-optional)

## For a developer

Do these in order on each machine where you or your agents work.

### 1. Take the former agent setup out, with the former client

The new `aries setup` knows only Aries's own entries, so it cannot
remove those the former client wrote. While the former client is still
installed, in each repository you set up, run its `setup --all
--remove`:

```sh
[former client binary] setup --all --remove
```

Add `--global` if you set agents up for every repository, and run
`[former client binary] setup claude-desktop --remove` in each
repository you registered with Claude Desktop. Commit what it changed.
If the former client is already gone, remove the entries by hand
([Agent setup files](#agent-setup-files)).

### 2. Remove the former client

Delete the former client binary, and the `.prev` copy an upgrade left
beside it, from `~/.local/bin` or wherever you installed it, and its
other files ([On a developer's machine](#on-a-developers-machine)).

### 3. Install `aries`

```sh
curl -fsSL https://raw.githubusercontent.com/ariesworx/aries/main/install.sh | sh
```

On Windows, download the zip from the
[releases page](https://github.com/ariesworx/aries/releases).
[Installing Aries](install.md) has the options.

**Why `aries upgrade` cannot do this.** A v0.4.0 client looks for
releases under the project's former repository path, by the former
asset names, and accepts only a release signed by the former key.
v1.0.0's assets are named `aries_<version>_<os>_<arch>` and are signed
by a new key, so the former client finds nothing it would install. The
install script trusts the new key; check a download by hand as
[Verify a download by hand](install.md#verify-a-download-by-hand) says.

### 4. Sign in

How you sign in depends on the transport your server uses. On the new
server you are a new principal either way.

**Over HTTPS (planned, decision 86), the default.** Your admin invites
you by the address your company's identity provider knows you by. Then,
in the repository:

```sh
aries login
```

It opens the browser at your provider; where there is no browser,
`aries login --url` prints a URL to open on any machine. The sign-in
lasts a day by default, and the token it leaves is bound to a key on
this machine and useless anywhere else. `aries logout` ends it. There
is no SSH key to send.

**Over SSH,** where your operator turned SSH on: `aries` signs in with
the key `ssh` would use, a default file in `~/.ssh` and then
ssh-agent's keys. To use another, set `ARIES_KEY_FILE` to the key
file's path. A `key:` line in the config file may name only a file
inside the repository; `key: ~/.ssh/…` is refused, and `aries` names
the variable to use instead. Send your admin the `.pub` of the key you
will use ([Your key is your
identity](cli.md#your-key-is-your-identity)). **(planned, decision
87)** Your admin may give the key an end date.

### 5. Rename your environment variables

Every variable `aries` reads begins `ARIES_`. Rename any you set, in
your shell's profile, an IDE's settings or a CI job:

| v0.4.0 | v1.0.0 |
|---|---|
| `[FORMER]_SESSION` | `ARIES_SESSION` |
| `[FORMER]_HARNESS` | `ARIES_HARNESS`, which `aries setup` writes into each agent's MCP entry |
| (none) | `ARIES_KEY_FILE`, the path of the key file to sign in with |
| (none) | `ARIES_RELEASES`, `ARIES_DATA_DIR`, `ARIES_NO_QUEUE` ([Environment variables](cli.md#environment-variables)) |

A CI secret that held a private key for the client to read is never
read: write the key to a file in the job and set `ARIES_KEY_FILE`
([Team deployment](deploy/team.md)). **(planned, decision 87)** Over
HTTPS, a CI job instead signs in as a service principal with its
platform's own token, `aries login --workload github-actions` (or
`google`, `azure`), and holds no key.

### 6. Connect the repository again

Delete the former project config file from the repository's root and
commit `.aries.yaml` in its place. The admin of the new server gives it
to you: `aries admin project add` prints one for a new project. It
names a new project (a new UUID) and the new server: **(planned,
decision 86)** its HTTPS address, and its host and host key when SSH is
on:

```yaml
project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f
server:
  url: https://aries.example.com   # planned: HTTPS, the default
  host: aries.example.com          # SSH, when the server has it on
  host_key: SHA256:…
```

`server.ca` (planned) holds a site's own certificate authority, for a
machine whose system store lacks it. Where both transports are set,
`aries` uses HTTPS, and `ARIES_TRANSPORT=ssh` in your environment
chooses SSH, for a server reached through IAP.

Then check the connection:

```sh
aries whoami
aries ready
```

`aries whoami` prints your principal and which key signed in
([Connect a repository](cli.md#connect-a-repository)).

**Commit trailers.** A commit is tied to its issue by an `Aries:`
trailer. Trailers under the former name in old commits are not read.

### 7. Set your agents up again

In each repository, write the new setup and commit it:

```sh
aries setup --all --write
```

or one agent at a time, such as `aries setup claude-code --write`.
Then take each agent's step again, since the MCP server has a new name:
approve the project's MCP server in Claude Code, trust it in Codex,
Gemini CLI or VS Code, turn Aries on in Cursor
([Set up an agent](agents.md#set-up-an-agent)). For Claude Desktop, run
`aries setup claude-desktop --write` in the repository and reopen the
app. **(planned, decision 87)** Or install the Aries extension for
Claude Desktop, which carries `aries` and signs you in.

**(planned, decision 87)** On an internet-facing server, Claude's
custom connectors (claude.ai, Claude Desktop, mobile) and Claude Code
can instead add Aries as a remote MCP server by its URL, one per
project, with no binary on the machine:

```sh
claude mcp add --transport http aries https://aries.example.com/mcp/p/PROJECT
```

A connector reaches the server from Anthropic's cloud, so it cannot
reach a private or air-gapped server; use `aries mcp` or the extension
there. Long work belongs in an agent with `aries mcp`, which renews its
claims; a connector's claim lapses a lease after its last tool call.

Restart every agent: an agent's MCP server is the binary the harness
started, and it stays the former one until the harness starts it again.

### 8. Learn what is new

Commands that did not exist in v0.4.0: `aries whoami`, `aries outbox`,
`aries cache`, `aries resolve`, `aries away`, `aries gate`,
`aries lock`, and `aries admin`
([Using `aries`](cli.md#commands)). A write made while the server
cannot be reached is now queued and sent later
([When the server cannot be reached](cli.md#when-the-server-cannot-be-reached)).

## For an operator

### 1. Decide what to keep

v1.0.0 does not read a v0.4.0 server's data: there is no migration from
Dolt, because no server ran on it in production (decision 79). Choose
one:

- **Start empty.** The usual case.
- **Carry the issues across** through bd's JSONL format, before you
  remove the former server
  ([Carrying issues across](#carrying-issues-across-optional)).

Tell everyone first: once the former server is gone, every former
client fails, and once the new one is up, every former client is
refused by it.

### 2. Set up a new server

On a fresh Ubuntu 24.04 LTS or 26.04 LTS machine, `install-server.sh`
does the whole setup, for many projects
([With the installer](server.md#with-the-installer)):

```sh
curl -fsSLO https://raw.githubusercontent.com/ariesworx/aries/main/install-server.sh
sudo sh install-server.sh --admin alice --admin-key alice.pub --host aries.example.com --dry-run
```

Drop `--dry-run` to set it up. **(planned, decision 87)** Choose a
profile with `--profile`:

| Profile | For |
|---|---|
| `internet` (the default) | A team on any network, with Claude's connectors: HTTPS, with ACME or your certificate, and any identity provider |
| `private` | A company network or VPN: HTTPS with your company CA's certificate, and an internal or a cloud provider |
| `airgapped` | A site with no route out: HTTPS with the site CA's certificate, an identity provider inside the network, releases from a directory |
| `ssh-only` | Today's server: SSH alone, HTTPS off, keys and no sign-in |

**SSH is off unless you turn it on (planned, decision 86).** Add
`--enable-ssh` to any of the first three profiles for teams that sign in
with keys or reach the server through IAP; then set
`transports: [https, ssh]` and reload `ariesd`. Keys added while SSH is
off take effect when it is turned on.

Or follow
[Running a server](server.md#set-up-a-server) step by step, or have
Claude Code do it with the `aries-ops` plugin
([Operate a server with Claude Code](server.md#operate-a-server-with-claude-code)).

**The database.** **(planned, decision 79)** v1.0.0 keeps its data in
Postgres 16 or newer, from the PGDG apt repository, with each project a
schema in one database. Until that work lands, the installer and the
guide set up Dolt, and this section will be rewritten with them.

A fresh machine is simpler than reusing the former server's. To reuse
it, remove the former server first ([On the former
server](#on-the-former-server)). Its SSH host key does not change, so a
`.aries.yaml` may pin the same fingerprint as before.

**(planned, decision 86)** An HTTPS server needs a DNS name and a
certificate (ACME, or your CA's files), and the registration of Aries
at your identity provider, with one redirect,
`https://aries.example.com/auth/callback`. An Entra ID provider takes
its cloud's authority from the settings, with no default; Microsoft's
US Government and classified clouds are configurable and not tested.

### 3. Add projects and people

`aries admin` manages projects, principals, keys and members from any
machine ([Serving many projects](server.md#serving-many-projects)):

```sh
aries admin project add …
aries admin principal add …
aries admin member set …
```

Send each project's `.aries.yaml` to its people (developer step 6).
**(planned, decisions 85 and 87)** With sign-in configured, invite
people by the address their identity provider knows them by; a
sign-in binds only an identity an admin invited. Add CI jobs and
unattended agents as service principals, with trust rules matched on
their platform's token.

### 4. Bring over what you configured

Settings carry over by meaning, not by file: write them again in
`/etc/aries/ariesd.yaml` ([Settings](server.md#settings),
[Limits](server.md#limits)). Every daemon variable is now `ARIESD_*`:
`ARIESD_CONFIG`, `ARIESD_DSN`, `ARIESD_SOCKET`, `ARIESD_PROJECT`,
`ARIESD_ADMINS`, `ARIESD_LOG_LEVEL` and `ARIESD_LOG_FORMAT` replace the
`[FORMER]D_` ones of the same suffix. Prices and plans are set again
with `aries admin prices` and `aries admin plans`.

If you ran a build from `main` between v0.4.0 and the rename, with
telemetry on, its metrics were named under the former name: take
dashboards and alert rules from `deploy/observability` again.

### 5. Remove the former server

Once nobody uses it ([On the former server](#on-the-former-server)).

## What to remove

### On a developer's machine

| What | Where |
|---|---|
| The former client binary, and its `.prev` copy | `~/.local/bin`, or where you installed it; on Windows, where you put the `.exe` |
| Its token usage offsets | A directory named `[former name]` under your cache directory: `$XDG_CACHE_HOME` or `~/.cache` on Linux, `~/Library/Caches` on macOS, `%LocalAppData%` on Windows |
| An outbox and cache, if you ran a build from `main` after v0.4.0 | A directory named `[former name]` under your data directory: `$XDG_DATA_HOME` or `~/.local/share` on Linux, `~/Library/Application Support` on macOS, `%LocalAppData%` on Windows. Send or note what `[former client binary] outbox` lists first |
| Environment variables | Any beginning `[FORMER]_`, in your shell's profile, an IDE's settings, a CI job's variables or its secrets |
| Global agent setup | `~/.claude`, `~/.codex`, `~/.gemini`, `~/.junie` and Cursor's user files, if you used `--global` (step 1 does it) |
| Claude Desktop's entries | Each entry named `[former name]-<directory>` in `claude_desktop_config.json` (step 1 does it) |

### In each repository

| What | Where |
|---|---|
| The former project config file | The repository's root: `.[former name].yaml` |
| The former agent setup | Below |

#### Agent setup files

Step 1 removes these. By hand, remove the former entries and blocks,
and leave everything else in the file:

| Agent | Former parts |
|---|---|
| Claude Code | The MCP server entry in `.mcp.json`; the pointer block in `CLAUDE.md`, between the comments `<!-- [former name]:begin -->` and `<!-- [former name]:end -->`; the session-start and usage hooks in `.claude/settings.json` |
| Codex | The MCP table in `.codex/config.toml`; the pointer block in `AGENTS.md`; the hook in `.codex/hooks.json` |
| Gemini CLI | The MCP entry and hook in `.gemini/settings.json`; the pointer block in `GEMINI.md` |
| Cursor | The entry in `.cursor/mcp.json`; `.cursor/rules/[former name].mdc`; the hook in `.cursor/hooks.json` |
| VS Code | The entry in `.vscode/mcp.json`; the pointer block in `.github/copilot-instructions.md`; `.github/hooks/[former name].json` |
| Junie | The entry in `.junie/mcp/mcp.json`; the pointer block in `AGENTS.md`; the hook in `~/.junie/config.json` |
| JetBrains AI Assistant | `.aiassistant/rules/[former name].md`, and the MCP server added in the IDE's settings |

### On the former server

Stop it first, then remove each part. Paths are those v0.4.0's guide
used; a server set up differently has its own.

| What | v0.4.0's place |
|---|---|
| The daemon's unit | `/etc/systemd/system/[former daemon binary].service`: `systemctl disable --now`, delete, `systemctl daemon-reload` |
| **sshd's drop-in** | `/etc/ssh/sshd_config.d/00-[former name].conf`, with its `Match User` block. Remove it and reload sshd |
| **The account's keys** | `/var/lib/[former name]/.ssh/authorized_keys`, each line a forced command for a principal |
| **The sudo rule** | `/etc/sudoers.d/[former name]`, which let the account restart the unit |
| The account and its group | `[former name]`, with `/var/lib/[former name]` as its home |
| The daemon binary | `/opt/[former name]/bin/`, and the link `/usr/local/bin/[former daemon binary]` |
| Its config | `/etc/[former name]/`, with `[former daemon binary].yaml` and the DSN's password |
| Its socket | `/run/[former name]/` |
| The daily dump | `/etc/cron.d/[former name]-backup`, and the dumps in `/var/backups/[former name]/` once you no longer need them |
| Variables | Any beginning `[FORMER]D_`, in the unit or an environment file |
| AppArmor profiles, if a build from `main` installed them | Under `/etc/apparmor.d/`, named for the former daemon, with their tunables and `local/` files: unload with `apparmor_parser -R`, then delete |
| Dolt, when nothing else uses it | `dolt.service`, `/usr/local/bin/dolt`, `/etc/dolt`, `/var/lib/dolt` (the databases, among them the former project's) and the `dolt` account |

The parts in bold still grant access while they exist: an
`authorized_keys` line, read through sshd's `Match` block, runs the
former daemon for its principal. Remove them before you leave the
machine running. Each person's `.ssh/known_hosts` is not touched by
Aries: `aries` pins the host key from `.aries.yaml`.

**(planned, decision 79)** On a v1.0.0 server that you later retire,
the database is Postgres; [Running a server](server.md) will say what
to remove.

## Carrying issues across (optional)

v0.4.0's daemon writes its store in bd's JSONL format, and v1.0.0's
reads that format, so issues can cross through a file. This path has
not been run from v0.4.0 to v1.0.0.

1. On the former server, before you remove it, as its account, run the
   former daemon's `export-bd -o issues.jsonl`, as v0.4.0's
   [Moving from bd](https://github.com/ariesworx/aries/blob/v0.4.0/docs/migrate-from-bd.md#going-back)
   guide said.
2. On the new server, import the file into the project, as
   [Moving from bd](migrate-from-bd.md) says (`ariesd import-bd`).

What crosses: issues with their IDs, fields, labels, dependencies,
parents and comments; a handoff becomes a plain comment. What does not:
memories, an issue's account, claims, the inbox, token usage, logged
hours, prices, plans and the event history. To keep the project's
memories, note the ones you need and `aries remember` them again.
